// cumora-server —— Go 单二进制服务端骨架(#51 · ADR 0004)。
//
// 本票只立骨架:启动/配置/pg 连接/goose 迁移基线/healthz/日志/优雅停机。
// 域实现按 #52+ 逐票落地,验收以验收镜像(CUMORA_MIRROR_BASE 指向本进程)
// 双跑为准。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/computers"
	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/costing"
	"github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/admin"
	domagents "github.com/MaskedKM/cumora/apps/server-go/internal/domains/agents"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/boards"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/calendar"
	domcomputers "github.com/MaskedKM/cumora/apps/server-go/internal/domains/computers"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/conversations"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/core"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/devtools"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/documents"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/projects"
	shipping "github.com/MaskedKM/cumora/apps/server-go/internal/domains/shipping"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/uploads"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/workspaces"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	pollsengine "github.com/MaskedKM/cumora/apps/server-go/internal/polls"
	"github.com/MaskedKM/cumora/apps/server-go/internal/push"
	"github.com/MaskedKM/cumora/apps/server-go/internal/runtime"
	"github.com/MaskedKM/cumora/apps/server-go/internal/sched"
	"github.com/MaskedKM/cumora/apps/server-go/internal/webapp"
	"github.com/MaskedKM/cumora/apps/server-go/internal/wsx"
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg := config.Load()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// 生产安全默认门(#212):危险配置拒启不带病运行;非生产侧的
	// 回退提示也在内(开发回退密钥 / 生产缺 CUMORA_SECRETS_KEY)。
	// #217 豁免:os.Getenv 在此仅作为 reader 注入 config 的守卫(键名与
	// 判定语义都在 config 包内),便于 prodguard_test 用表驱动替身。
	if violations := config.ProdEnvViolations(os.Getenv); len(violations) > 0 {
		for _, v := range violations {
			slog.Error("unsafe production config — refusing to start", "violation", v)
		}
		os.Exit(1)
	}
	for _, warn := range config.EnvFallbackWarnings(os.Getenv) {
		slog.Warn(warn)
	}

	pool, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		slog.Error("pg connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := db.Migrate(pool, cfg.MigrationsDir); err != nil {
		slog.Error("go_migrations apply failed", "err", err)
		os.Exit(1)
	}
	slog.Info("schema at baseline", "migrations", cfg.MigrationsDir)

	mux := http.NewServeMux()

	// Redis(#55 引入):文档协同的跨进程扇出链路(Go relay ← sidecar)
	// 必须有真订阅端;publish 面从 Noop 升级为真广播。不可达时降级
	// Noop(HTTP 域照常,协同 401/超时,与 TS 未配 token 的姿态一致)。
	ctxBoot, cancelBoot := context.WithCancel(context.Background())
	defer cancelBoot()
	// #215:后台 worker 父 ctx 注入——cancelBoot 时 sched/runtime 的
	// tick 循环与调度订阅真正停下来(此前两包的 ctxBG 恒 Background,
	// 优雅停机对它们无效)。须在任何 Start* 之前。
	sched.SetBaseContext(ctxBoot)
	runtime.SetBaseContext(ctxBoot)
	var rdb *redis.Client
	if ropts, err := redis.ParseURL(cfg.RedisURL); err != nil {
		slog.Warn("REDIS_URL unparsable — events degrade to noop, doc collab unavailable", "err", err)
	} else {
		rdb = redis.NewClient(ropts)
	}
	// boot 探活带界重试(#211):机器重启时用户单元可能先于系统 redis
	// 就绪,一次 ping 失败就永久 Noop 的竞态窗口由此收窄(5×2s;Redis
	// 正常时首次即过,零额外延迟)。仍失败才降级 —— 之后 livez 持续
	// 503 显性示警,而非假绿。
	if rdb != nil {
		redisUp := rdb.Ping(ctxBoot).Err() == nil
		for i := 0; !redisUp && i < 4; i++ {
			time.Sleep(2 * time.Second)
			redisUp = rdb.Ping(ctxBoot).Err() == nil
		}
		if !redisUp {
			slog.Warn("redis unreachable — events degrade to noop, doc collab unavailable; /api/livez will report 503 until restart", "url", cfg.RedisURL)
			rdb = nil
		}
	}
	eventsLive := false
	if rdb != nil {
		events.SetPublisher(events.RedisPublisher{RDB: rdb})
		eventsLive = true
		defer rdb.Close()
	} else {
		events.SetPublisher(events.NoopPublisher{})
	}

	// livez 扩 Redis 硬依赖(#211,8-31 事故三病之三):boot 降级 Noop
	// 或运行中 Redis 不可达时 /api/livez 返回 503 —— 不再让"事件静默
	// 吞掉"披着假绿。与 #216 docrelay 降级姿态一致:协同面本就快败,
	// livez 变红只是把同一事实抬到探活面。注意降级判定是 boot 时一次
	// 性的:Redis 中途恢复后 livez 转绿 ≠ 事件面恢复(Noop 不会自愈,
	// 需 restart cumora-go)—— 探活闭包把 eventsLive 挡在 ping 之前,
	// 降级实例保持红,运维一眼可知要重启。core tag 的冗余注册同源注入。
	livezPing := func(ctx context.Context) error {
		if !eventsLive {
			return errors.New("redis unreachable at boot — events degraded to noop; fix redis/REDIS_URL then restart cumora-go (restart alone cannot recover a bad REDIS_URL)")
		}
		return rdb.Ping(ctx).Err()
	}
	httpx.MountHealth(mux, pool, livezPing)

	relay := docrelay.New(cfg.YjsSidecarURL, cfg.YjsSidecarToken, cfg.YjsSidecarTimeout, cfg.InstanceID)
	relay.Boot(ctxBoot, rdb)

	// /runtime 面(#60):BYOA daemon 服务端(wake-stream SSE + 数据面)。
	// 自带 agent-runtime JWT 鉴权,不嵌 /api。Redis 不可达时 wake-stream
	// 直接 503(不做半开会话),其余数据面路由照常。
	runtimeSvc := runtime.New(pool, rdb)
	runtimeSvc.SetRelay(relay)
	// 在场清账(TS resetHumanPresenceOnBoot):先于任何 WS 连接把上次
	// 运行残留的 'avail' 人类降 'resting'——半开连接的 close 不会来。
	runtimeSvc.ResetHumanPresenceOnBoot(ctxBoot)

	// WS 网关(/ws,自带 ws-ticket 鉴权,不走 /api/ 中间件链)。#198/#202
	// 起补齐 TS ws.ts 全语义:hello 握手帧 + 聊天推送桥(Redis→按租户
	// 过滤转发)+ 人在场 0→1/1→0 翻转。
	wsx.Mount(mux, pool, relay, rdb, cfg.InstanceID, runtimeSvc)

	runtimeSvc.Mount(mux)

	// 静态托管(#69 切换日):SPA(生产 dist/)与 /uploads 本地目录——
	// 全量承载前端所需,语义对齐 TS index.ts 静态段。
	webapp.Mount(mux)

	// 认知辅后台任务组(#62):mailbox scheduler(msg.new/polls 订阅 →
	// 唤醒/steer)、背景扫描、idle 调度、llm_calls_rollup 刷新——各自
	// env 门控对齐 TS index.ts(ENABLE_*/INTERVAL),tick 失败自隔离。
	runtimeSvc.StartScheduler()
	runtimeSvc.StartScanner()
	runtimeSvc.StartIdleScheduler()
	// 日历提醒调度器(#209):calendar.reminder 补发布方——扫 calendar_events
	// 提醒窗口,落 calendar_reminders(唯一键去重)后 Redis 广播(toast)+
	// Resend email(未配置静默跳过)。TS startCalendarScheduler 的提醒半边。
	runtimeSvc.StartCalendarReminderScheduler()
	costing.StartLlmRollupRefresher(ctxBoot, runtimeSvc.DB)

	// 投票过期清扫器(#121):POLL_SWEEP_INTERVAL_MS(默认 60s;0=禁用,
	// 须透传——0→fallback 会吞掉 kill-switch,#62 教训,故用 EnvIntRaw)。
	pollSweepInterval := int64(60_000)
	if v, ok := config.EnvIntRaw("POLL_SWEEP_INTERVAL_MS"); ok {
		pollSweepInterval = v
	}
	if pollSweepInterval > 0 {
		go func() {
			tickSweep := time.NewTicker(time.Duration(pollSweepInterval) * time.Millisecond)
			defer tickSweep.Stop()
			for {
				select {
				case <-ctxBoot.Done():
					return
				case <-tickSweep.C:
					if n := pollsengine.Sweep(ctxBoot, pool); n > 0 {
						slog.Info("[polls] sweeper closed expired polls", "count", n)
					}
				}
			}
		}()
		slog.Info("[polls] expiration sweeper running", "interval_ms", pollSweepInterval)
	} else {
		slog.Info("[polls] expiration sweeper disabled (POLL_SWEEP_INTERVAL_MS=0)")
	}

	// 邮件任务组(#58):出站重试 + 附件 GC(受管 goroutine,ctx 随停机)
	email.StartRetryWorker(ctxBoot, pool, config.EnvInt("EMAIL_RETRY_INTERVAL_MS", 60_000))
	email.StartGcWorker(ctxBoot, pool, config.EnvInt("EMAIL_GC_INTERVAL_MS", 24*60*60_000))

	// 认证中间件(有令牌即解析注入,不拒绝——requireAuth 语义在各 handler)
	authMiddleware := httpx.Authn(pool)
	coreRouter := http.NewServeMux()
	core.Mount(coreRouter, pool, rdb, livezPing)
	conversations.Mount(coreRouter, pool)
	boards.Mount(coreRouter, pool, runtimeSvc.WakeMentionedAgents)
	workspaces.Mount(coreRouter, pool)
	documents.Mount(coreRouter, pool)
	email.Mount(coreRouter, pool)
	email.MountInbound(mux, pool)
	calendar.Mount(coreRouter, pool)
	push.Mount(coreRouter, pool)
	domcomputers.Mount(coreRouter, pool)
	// 长尾路由(#77):uploads 面 + 开发者/观察面(devtools 文件读、run
	// 事件、纯 agent 房偷看;头像钩子经 domagents 注入 runtime 面)。
	uploads.Mount(coreRouter, pool)
	projects.Mount(coreRouter, pool)
	domagents.Mount(coreRouter, pool,
		func(agentID, tenant string) { _, _ = runtimeSvc.GenerateAgentAvatar(ctxBoot, agentID, tenant) },
		runtimeSvc.GenerateAgentAvatar)
	devtools.Mount(coreRouter, pool)
	// admin 面(#112):settings 读写+Cerebellum 密钥遮蔽+/me 门探+引擎并集;
	// users/waitlist/stats/observability-llm 子面留待完整化票。
	admin.Mount(coreRouter, pool)
	computers.StartSweepWorker(ctxBoot, pool)

	// shipping 全子面(#125,#117-f):feature 契约机/验证方格/发布/
	// 回读/回归/摩擦,16 路由。
	shipping.Mount(coreRouter, pool)
	// /api/* 统一入口:panic 兜底(#214,最外层——域 handler panic 不再
	// 走 net/http 默认路径变成连接重置,而是 500 JSON)→ 写期限兜底
	// (#136,非流式面)→ 认证中间件 → core 域;域未挂载的路径落到
	// JSON 404 兜底(baseline 形状 {error:'not found'},#53 起域渐挂
	// 期间的平价)。
	coreRouter.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, "not found")
	})
	mux.Handle("/api/", httpx.Recover()(httpx.WriteDeadline(5*time.Minute)(authMiddleware(coreRouter))))
	// 后续域(#53 会话起)同样:各自 Mount 后经 authMiddleware 串接。

	srv := newHTTPServer(cfg.ListenAddr, mux)

	go func() {
		slog.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	slog.Info("shutting down")
	// #215:先停后台 worker(订阅/tick/回写),与 HTTP 排空并行收尾;
	// defer 的 cancelBoot 仍在,双保险。
	cancelBoot()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown incomplete", "err", err)
	}
	slog.Info("bye")
}

// newHTTPServer:#136 超时兜底。ReadTimeout 取宽值只为框住停滞的请求体
// (大上传慢链路不误伤);WriteTimeout 必须保持 0——SSE 唤醒流与 WS
// 升级共享本 Server,全局写期限会掐死长响应,非流式 /api 面由
// httpx.WriteDeadline 单独设。IdleTimeout 只回收 keep-alive 空闲连接,
// 对活跃请求无影响;WS hijack 后的连接不受任何 Server 超时管理。
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}
