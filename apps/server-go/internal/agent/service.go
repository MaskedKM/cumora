// agent 包 service —— #60 运行时服务面的依赖容器。
// 对齐 已退役 TS server 的 agents/runtime/(server.ts + inproc-client.ts + wake-bus.ts)
// 的服务端:HTTP 面 + 数据面 + 唤醒总线。
package agent

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
	"github.com/MaskedKM/cumora/apps/server-go/internal/wakebus"
	"github.com/redis/go-redis/v9"
)

// Service:一个进程一份。DB 为 pg 池;Redis 客户端可能为 nil(单机无
// Redis 降级——wake-bus/typing/busy 等 Redis 面会按各自策略降级);
// Bus 在无 Redis 时不可用(wake-stream 直接 503,不做半开)。
type Service struct {
	DB  *sql.DB
	RDB redis.UniversalClient
	Bus *wakebus.Bus
	// Relay:Yjs sidecar 客户端(doc read/agent-edit 走它)。nil 时 doc
	// 命令按 sidecar 不可用报错。
	Relay *docrelay.Relay

	wakeAgentHook wakeAgentFn

	// domainDispatch:域子包命令分发钩子(#140 刀法)——kanban/email/…
	// 居 agent/<domain> 子包,本包不得 import 它们(防环);runtime 接线
	// 时注入 sub → 域方法的闭包表,RunCli 的 default 分支回落于此。
	domainDispatch func(sub string, ctx context.Context, p cliParsed) (cliResult, bool)
}

func New(db *sql.DB, rdb redis.UniversalClient) *Service {
	var bus *wakebus.Bus
	if rdb != nil {
		bus = wakebus.New(rdb)
	}
	return &Service{DB: db, RDB: rdb, Bus: bus}
}

// SetRelay:main 在 relay 构造后注入(避免构造环)。
func (s *Service) SetRelay(r *docrelay.Relay) { s.Relay = r }

// wakeAgentFn:唤醒投递钩子——唤醒机制(预算/steer/bus 投递)住在
// runtime 调度包,agent 面只经此投递(cli boards 的 manual 唤醒)。
// 未接线(单测/独立构造)时静默丢弃并记日志。
type wakeAgentFn func(agentID, reason string, conversationID *string)

func (s *Service) SetWakeHook(fn wakeAgentFn) { s.wakeAgentHook = fn }

// SetDomainDispatcher:注入域子包命令分发(runtime.New 接线)。
func (s *Service) SetDomainDispatcher(fn func(sub string, ctx context.Context, p cliParsed) (cliResult, bool)) {
	s.domainDispatch = fn
}

// WakeAgent:Boards 面的 legacy 形态(#82)——经钩子投递到 runtime 的
// wakeOne(无 steer、无附加选项);钩子未接线则丢弃并记日志。
func (s *Service) WakeAgent(agentID, reason string, conversationID *string) {
	if s.wakeAgentHook == nil {
		slog.Warn("[agent] wake hook not wired — dropping wake", "agent", agentID, "reason", reason)
		return
	}
	s.wakeAgentHook(agentID, reason, conversationID)
}

// rdbOrNil:子功能取 Redis 客户端(nil = 降级路径)。
func (s *Service) redis() redis.UniversalClient { return s.RDB }

// ctxBG:fire-and-forget 后台写共用的父上下文。
var ctxBG = context.Background()
