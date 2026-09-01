// doctor —— "为什么坏"面(#281):配置与依赖健康的只读体检。
//
// 与 status 的分工:doctor 检查的是「要让这套部署活起来,机器上该有的
// 东西在不在」(pg/redis/pgvector/unit 注册态/env 键/端口/引擎);status
// 检查的是「现在跑着的东西状态如何」。doctor 全程零写入。
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackd"
)

// Status —— 三态 + info。fail 使 doctor 退出码非零;warn 不阻断。
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Info Status = "info"
)

// Check —— 一行体检结果。Group 用于人读输出的分区。
type Check struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report —— doctor 全量结果。AnyFail 驱动退出码。
type Report struct {
	Checks  []Check `json:"checks"`
	AnyFail bool    `json:"anyFail"`
}

// Config —— doctor 输入。路径/地址全部可注入:默认值属本部署布局,
// 但不以硬编码形态出现在逻辑里(ADR 0005 "自用优先,不写死机器事实")。
type Config struct {
	EnvFile        string          // 主 .env(服务三件套的 EnvironmentFile)
	DaemonEnvFile  string          // daemon.env(引擎凭据)
	Units          []string        // 生产三件套 unit 名
	StackAddrs     []AddrExpect    // 栈应监听的地址
	Engines        []string        // 引擎名集合(claude/codex/grok/cursor)
	EngineExtraDir func() []string // 引擎发现的额外目录(nvm glob 等)
	// 单 unit 形态感知(#298):stackd 的 unit 名。LoadState=loaded
	// (install 写文件起恒真、uninstall 删文件止)= 单 unit 形态;空串 =
	// 不感知(纯三 unit 面,留给测试与旧形态)。
	StackdUnit string
	// stackd 状态文件:单 unit 形态下校验子进程面(与 status 的 stackd
	// 段同源同契约)。
	StateFile string
	// stack.toml 感知(#284):校验红进 [config] 段;受管形态切 pg/redis
	// 的 socket 检查面。ConfigFound=false 且 ConfigErr="" = 未创建(合法)。
	ConfigFile  string
	ConfigFound bool
	ConfigErr   string
	// 受管形态(stackconfig.ModeInternal)时的探针目标;external 留空。
	PGMode           string
	InternalDSN      string
	RedisMode        string
	InternalRedisURL string
}

// AddrExpect —— 端口期望:Must(栈服务,关闭=fail)/Desktop(桌面 App
// 在跑才有,warn)/Info(仅报告)。
type AddrExpect struct {
	Name string
	Addr string
	Kind string // "must" | "desktop" | "info"
}

// envRequired —— .env 里缺席即红的键(GitHub OAuth 是登录链硬依赖,
// #272/#271 的教训:缺键=静默断链,doctor 的职责就是把它变显性)。
// 受管 pg 形态下 DATABASE_URL 由 stackd 派生注入,不再是 env 红线键。
var envRequired = []string{
	"CUMORA_SECRETS_KEY",
	"GITHUB_CLIENT_ID",
	"GITHUB_CLIENT_SECRET",
	"CUMORA_AUTH_RETURN_ALLOWLIST",
	"YJS_SIDECAR_TOKEN",
}

// envRequiredExternal —— external pg 形态追加 DATABASE_URL(外部库必须
// 指路;受管形态它由 socket 派生,缺席不是病)。
var envRequiredExternal = append([]string{"DATABASE_URL"}, envRequired...)

// envWarn —— 缺席仅黄的键(功能降级,不阻断部署)。
var envWarn = map[string]string{
	"OPENAI_API_KEY": "Cerebellum remote 无凭据",
}

var daemonEnvWarn = map[string]string{
	"ANTHROPIC_AUTH_TOKEN": "codex 引擎(Claude Relay)无凭据",
}

// Run —— 执行体检。永不 panic、永不写外部状态;单探针失败变成一行
// fail,不中断其余检查。
func Run(d probe.Deps, cfg Config) Report {
	var r Report
	add := func(group, name string, st Status, format string, a ...any) {
		r.Checks = append(r.Checks, Check{Group: group, Name: name, Status: st, Detail: fmt.Sprintf(format, a...)})
		if st == Fail {
			r.AnyFail = true
		}
	}

	// stack.toml(#284):坏文件 = 红(校验不静默);未创建 = 合法缺省。
	switch {
	case cfg.ConfigErr != "":
		add("config", "stack.toml", Fail, "校验失败: %s", cfg.ConfigErr)
	case cfg.ConfigFound:
		add("config", "stack.toml", OK, "%s", cfg.ConfigFile)
	default:
		add("config", "stack.toml", OK, "未创建(内置缺省运行;import-env 可生成)")
	}

	// env 文件读取与键面(env 键同时是 pg/redis 探针的 DSN 来源)。
	env := map[string]string{}
	if data, err := d.ReadFile(cfg.EnvFile); err == nil {
		env = probe.ParseEnvFile(data)
		add("env", filepath.Base(cfg.EnvFile), OK, "%d 键", len(env))
	} else {
		add("env", filepath.Base(cfg.EnvFile), Fail, "读不到: %v", err)
	}
	required := envRequired
	if cfg.PGMode != stackconfig.ModeInternal {
		required = envRequiredExternal
	}
	for _, k := range required {
		if v, ok := env[k]; ok && v != "" {
			add("env", k, OK, "")
		} else {
			add("env", k, Fail, "缺失或为空")
		}
	}
	for _, k := range sortedKeys(envWarn) {
		if v, ok := env[k]; ok && v != "" {
			add("env", k, OK, "")
		} else {
			add("env", k, Warn, "%s", envWarn[k])
		}
	}

	daemonEnv := map[string]string{}
	if cfg.DaemonEnvFile != "" {
		if data, err := d.ReadFile(cfg.DaemonEnvFile); err == nil {
			daemonEnv = probe.ParseEnvFile(data)
			add("env", filepath.Base(cfg.DaemonEnvFile), OK, "%d 键", len(daemonEnv))
		} else {
			add("env", filepath.Base(cfg.DaemonEnvFile), Warn, "读不到: %v", err)
		}
	}
	for _, k := range sortedKeys(daemonEnvWarn) {
		if v, ok := daemonEnv[k]; ok && v != "" {
			add("env", k, OK, "")
		} else {
			add("env", k, Warn, "%s", daemonEnvWarn[k])
		}
	}

	// 依赖服务:pg(含 pgvector)、redis。受管形态(#284)探 stack.toml
	// 派生的 socket 面;external 语义照旧:DSN 优先 env 文件,退 OS env,
	// 再退 localhost 缺省(与 server-go config 同缺省)。两探针各自起
	// 独立超时:共享预算时慢失败的 pg 会把 redis 拖成误导性的 deadline 红。
	dsn := cfg.InternalDSN
	if cfg.PGMode != stackconfig.ModeInternal {
		dsn = env["DATABASE_URL"]
		if dsn == "" {
			dsn = os.Getenv("DATABASE_URL")
		}
	}
	pgCtx, pgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pgCancel()
	if info, err := d.PG(pgCtx, dsn); err != nil {
		add("postgres", "可达", Fail, "%v", err)
	} else {
		add("postgres", "可达", OK, "%s", info.Version)
		if info.PgvectorAvailable {
			add("postgres", "pgvector", OK, "")
		} else {
			add("postgres", "pgvector", Fail, "pg_available_extensions 无 vector")
		}
	}
	redisURL := cfg.InternalRedisURL
	if cfg.RedisMode != stackconfig.ModeInternal {
		redisURL = env["REDIS_URL"]
		if redisURL == "" {
			redisURL = os.Getenv("REDIS_URL")
		}
	}
	redisCtx, redisCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer redisCancel()
	if err := d.Redis(redisCtx, redisURL); err != nil {
		add("redis", "可达", Fail, "%v", err)
	} else {
		add("redis", "可达", OK, "")
	}

	// 形态判定(#298,与 deploy-release 的 LoadState=loaded 同语义):
	// stackd 的 unit 文件在 = 单 unit 形态。此时旧三 unit 本就该
	// inactive —— 再拿它们当体检面就是假红(切换演练实测);改查
	// stackd unit 本体 + 状态文件里的子进程面。三 unit 形态照旧。
	stackdForm := false
	var stackdState probe.UnitState
	var stackdProbeErr error
	if cfg.StackdUnit != "" {
		st, err := d.Systemd(cfg.StackdUnit)
		switch {
		case err != nil:
			stackdProbeErr = err
		case st.Load == "loaded":
			stackdForm, stackdState = true, st
		}
	}
	switch {
	case stackdForm:
		add("form", "形态", Info, "stackd 单 unit(%s)", cfg.StackdUnit)
		if stackdState.Active == "active" {
			add("units", cfg.StackdUnit, OK, "%s/%s", stackdState.Active, stackdState.Sub)
		} else {
			add("units", cfg.StackdUnit, Fail, "ActiveState=%s SubState=%s", stackdState.Active, stackdState.Sub)
		}
		// 子进程面:状态文件不可读/解析失败 = warn 而非 fail —— 启动
		// 首写前的短暂窗口属正常,且端口 must 门(5181/5182)仍在下面
		// 跑,实际门不空转;子进程 Running=false / 熔断 = fail。
		if cfg.StateFile == "" {
			add("stackd", "状态文件", Warn, "未配置 StateFile,子进程面不可查")
		} else if data, err := d.ReadFile(cfg.StateFile); err != nil {
			add("stackd", "状态文件", Warn, "读不到: %v", err)
		} else {
			var snap stackd.Snapshot
			if err := json.Unmarshal(data, &snap); err != nil {
				add("stackd", "状态文件", Warn, "解析失败: %v", err)
			} else if len(snap.Children) == 0 {
				add("stackd", "子进程面", Fail, "状态文件无子进程记录")
			} else {
				for _, ch := range snap.Children {
					if ch.Running {
						add("stackd", ch.Name, OK, "pid=%d restarts=%d", ch.PID, ch.Restarts)
					} else {
						detail := fmt.Sprintf("Running=false restarts=%d", ch.Restarts)
						if ch.LastErr != "" {
							detail += " lastErr=" + ch.LastErr
						}
						if ch.CircuitOpen {
							detail += " [circuit-open]"
						}
						add("stackd", ch.Name, Fail, "%s", detail)
					}
				}
			}
		}
	default:
		// 三 unit 面。形态行按落因分说:探针失败(dbus 断/容器无
		// session bus)≠ 未装 —— 误称"未装"会误导排障方向(评审 P2);
		// 探针失败本身不置 fail(units 行会带同一原始错误红,门不空转)。
		switch {
		case cfg.StackdUnit == "":
			// 不感知形态(测试注入/旧调用方):无形态行。
		case stackdProbeErr != nil:
			add("form", "形态", Warn, "stackd unit 探测失败,按三 unit 面体检: %v", stackdProbeErr)
		default:
			add("form", "形态", Info, "三 unit 形态(%s 未装)", cfg.StackdUnit)
		}
		// unit 注册态与活性:masked/not-found 单列(loads 分离,防假绿)。
		for _, u := range cfg.Units {
			st, err := d.Systemd(u)
			switch {
			case err != nil:
				add("units", u, Fail, "%v", err)
			case st.Load == "masked":
				add("units", u, Fail, "LoadState=masked(被屏蔽,enable 也不会跑)")
			case st.Load == "not-found":
				add("units", u, Fail, "unit 未安装(install-units.sh 未跑?)")
			case st.Active == "active":
				add("units", u, OK, "%s/%s", st.Active, st.Sub)
			default:
				add("units", u, Fail, "ActiveState=%s SubState=%s", st.Active, st.Sub)
			}
		}
	}

	// 端口:栈服务 must;桌面回环 desktop(App 没开就没有);info 仅记录。
	for _, a := range cfg.StackAddrs {
		err := d.Dial(a.Addr)
		switch a.Kind {
		case "desktop":
			if err == nil {
				add("ports", a.Name, OK, "监听中")
			} else {
				add("ports", a.Name, Warn, "未监听(桌面 App 未运行时属正常)")
			}
		case "info":
			if err == nil {
				add("ports", a.Name, Info, "监听中")
			} else {
				add("ports", a.Name, Info, "未监听")
			}
		default: // must
			if err == nil {
				add("ports", a.Name, OK, "监听中")
			} else {
				add("ports", a.Name, Fail, "未监听: %v", err)
			}
		}
	}

	// 引擎发现:PATH + nvm 等额外目录。缺席=warn(BYOA 引擎按需存在)。
	extra := []string{}
	if cfg.EngineExtraDir != nil {
		extra = cfg.EngineExtraDir()
	}
	for _, e := range cfg.Engines {
		if p, err := d.LookPath(e, extra); err == nil {
			add("engines", e, OK, "%s", p)
		} else {
			add("engines", e, Warn, "PATH 与额外目录均未发现(fresh-boot PATH 钉扎坑的显性化)")
		}
	}
	return r
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
