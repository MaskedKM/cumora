// httpx —— 域路由的公共助手(#51)。契约事实源:packages/contract/openapi.yaml;
// 镜像断言(tests/integration/mirror-*.test.ts)是行为基准。
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
)

// WriteJSON 统一 JSON 响应与错误形状({error: string},与 TS baseline 对齐)。
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// WriteInternalError:TS errorHandler 非 HttpError 分支的合一(#141,
// 122 处裸 err.Error() 500 收编;#214 再收编 76 处裸
// WriteError(w,500,静态文案)——dev 文案从静态串变 err.Error(),
// production 变通用文案,状态码不变)——恒 slog 排障;对外复刻 TS 的
// dev/prod 分流:NODE_ENV≠production 保留 err.message(TS 设计特性:不翻
// 日志即可排障),production 收敛通用文案关泄漏。三处不进本助手:devtools
// 头像 502 与 apple 400 是 TS baseline 无条件透传;waitlist approve 的受控
// 域错走原 WriteError。
//
// 已知有意分歧(#184 评审 P1):TS 另有约 10 个端点是 handler 内显式
// catch 500 无条件透传 msg(不经 errorHandler,production 也透传),
// 对应 Go 站点已随本助手收编 —— 仅 NODE_ENV=production 下可见差异
// (TS 动态文案 vs 通用文案),方向是收敛泄漏、状态码不变;本部署
// NODE_ENV=development 无行为差。
func WriteInternalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Warn("api 500", "method", r.Method, "path", r.URL.Path, "err", err)
	msg := err.Error()
	if config.IsProduction() {
		msg = "internal server error"
	}
	WriteError(w, http.StatusInternalServerError, msg)
}

// RedisPing:livez 的 Redis 硬依赖探活注入(#211)。闭包形态,httpx
// 不直接依赖 go-redis(测试替身零成本);nil = 未注入(嵌入/测试场景),
// livez 退回无依赖活探。
type RedisPing func(ctx context.Context) error

// MountHealth 挂 /api/health(带 pg 探活)与 /api/livez(进程活 + Redis)。
// 形状对齐 TS baseline:{ok, ts(ms)};池耗尽时 1s 超时兜底返回确定性 503。
// #187 批次 8:两 handler 导出为 Livez/Health(core tag 经 ServerInterface
// 委托到此;根 mux 特定 pattern 优先于 /api/ 子树,绕过 auth 的既有
// 语义不变 —— 单一函数体,双注册)。#211:livez 从"无依赖活探"扩为
// Redis 硬依赖显性面(注入 rping,语义见 Livez)。
func MountHealth(mux *http.ServeMux, pool interface {
	PingContext(ctx context.Context) error
}, rping RedisPing) {
	mux.HandleFunc("GET /api/livez", func(w http.ResponseWriter, r *http.Request) {
		Livez(rping, w, r)
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		Health(pool, w, r)
	})
}

// Livez:进程活 + Redis 硬依赖(#211)。Redis 不可达、或启动时已降级
// NoopPublisher(事件面静默吞掉)时返回 503 —— 此前 Noop 降级只在启动
// 日志 Warn 一行,HTTP 面保持假绿,是 8-31 事故三病之一;协同面
// (docrelay,#216)在 Redis 挂时本就快败,livez 变红只是把同一事实抬到
// 探活面。503 形状:{ok:false, error, ts};200 保持 {ok:true, ts}。
func Livez(rping RedisPing, w http.ResponseWriter, _ *http.Request) {
	if rping != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := rping(ctx); err != nil {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error(), "ts": time.Now().UnixMilli()})
			return
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().UnixMilli()})
}

// Health:pg 探活;池耗尽时 1s 超时兜底返回确定性 503。
func Health(pool interface {
	PingContext(ctx context.Context) error
}, w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pool.PingContext(ctx); err != nil {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().UnixMilli()})
}

// ISOms 对齐 JS Date.toISOString():恒为 UTC + 毫秒三位小数
// (RFC3339Nano 会按需省略尾零,pg 的 µs 精度也会透出)。日历域(#57)
// 起统一采用;其余域在被触及时迁移。
func ISOms(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
