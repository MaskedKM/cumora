// sched 包 —— 唤醒调度与议程面(#140 收官刀,自 runtime 拆出):
// scheduler(msg.new/polls 订阅扇出 + 预算/turn-rate/steer 速率)、agenda
// (议程构建与小脑分类)、idle(安静 agent 合成唤醒)、triage(小脑 prompt
// 纯函数心脏)。S 嵌入 agent.Service;presence 的 busy 探经钩子注入
// (runtime.New 接线),防反向依赖。
package sched

import (
	"context"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
)

// S:调度域接收器——嵌入 agent.Service(DB/RDB/Bus/Relay)。
type S struct {
	*agent.Service

	busyProbe func(agentID string) bool
}

// New:构造(runtime.New 唯一调用方,随后 SetBusyProbe 接线)。
func New(core *agent.Service) *S { return &S{Service: core} }

// SetBusyProbe:注入 presence 的 IsAgentBusy(未接线 = fail-open 不忙,
// 与无 Redis 时的 IsAgentBusy 同语义)。
func (s *S) SetBusyProbe(fn func(agentID string) bool) { s.busyProbe = fn }

func (s *S) busy(agentID string) bool {
	if s.busyProbe == nil {
		return false
	}
	return s.busyProbe(agentID)
}

// ctxBG:fire-and-forget 后台写与 worker 循环共用的父上下文。默认
// Background;SetBaseContext 由 cmd/server 在 boot 期、任何 Start* 之前
// 注入可取消的 ctx(#215:优雅停机对后台任务生效——此前恒 Background,
// 停机对这里的消费者全部无效)。goroutine 启动即建立 happens-before,
// 注入后不再改写。
var ctxBG = context.Background()

// SetBaseContext:见 ctxBG 注释。须在 worker 启动前调用(boot 期单线程)。
func SetBaseContext(ctx context.Context) { ctxBG = ctx }
