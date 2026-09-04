// runtime 包 service —— #140 拆包后的 HTTP 壳:真身(agent 面 +
// client 数据面)在 internal/agent,唤醒调度/议程在 internal/sched(经
// Sched 字段与壳代理可达);本包自留 /runtime/* 路由、扫描、presence,
// 并承担域子包与调度域的接线。
package runtime

import (
	"context"
	"database/sql"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
	"github.com/MaskedKM/cumora/apps/server-go/internal/sched"
	"github.com/redis/go-redis/v9"
)

// Service:一个进程一份。方法面在 agent.Service(嵌入提升,既有调用点
// 零改动);调度/扫描/presence/路由方法定义在本包。
type Service struct {
	*agent.Service

	// Sched:唤醒调度/议程域(#140 收官刀自本包拆出,构造时接线)。
	Sched *sched.S
}

func New(db *sql.DB, rdb redis.UniversalClient) *Service {
	core := agent.New(db, rdb)
	schedS := sched.New(core)
	svc := &Service{Service: core, Sched: schedS}
	// busy 探针:调度域的 steer 判定经此读 presence 租约(防反向依赖)。
	schedS.SetBusyProbe(svc.IsAgentBusy)
	// 唤醒钩子:agent 面(cli boards manual 唤醒)→ 调度域 WakeOne。
	core.SetWakeHook(func(agentID, reason string, conversationID *string) {
		schedS.WakeOne(agentID, reason, conversationID, nil, nil)
	})
	wireDomainDispatch(core)
	return svc
}

// SetRelay:main 在 relay 构造后注入(避免构造环)。
func (s *Service) SetRelay(r *docrelay.Relay) { s.Service.SetRelay(r) }

// redisOrNil:子功能取 Redis 客户端(nil = 降级路径)。
func (s *Service) redis() redis.UniversalClient { return s.RDB }

// StartScheduler / StartIdleScheduler / StartCalendarReminderScheduler /
// StartDbGcWorker / WakeMentionedAgents:main.go 与域挂载的启动/回调入口
// (调度域实装的壳代理)。
func (s *Service) StartScheduler()     { s.Sched.StartScheduler() }
func (s *Service) StartIdleScheduler() { s.Sched.StartIdleScheduler() }

// StartCalendarReminderScheduler:日历提醒调度器(#209)——calendar.reminder
// 的发布方(toast 恒发 + email 按 Resend 配置),详见 sched 包。
func (s *Service) StartCalendarReminderScheduler() { s.Sched.StartCalendarReminderScheduler() }

// StartDbGcWorker:DB 行保留 GC(同族审计 P1-2;#70 退役时丢失的 TS
// startDbGcWorker 移植)——五张高量表按保留窗小批扫删,详见 sched 包。
func (s *Service) StartDbGcWorker() { s.Sched.StartDbGcWorker() }

// StartWorkspaceScanWorker:#337 文件已知态 60min 兜底扫描(挂载 watcher
// 丢事件/inotify 上限时接管;WORKSPACE_SCAN_INTERVAL_MS=0 禁用)。
func (s *Service) StartWorkspaceScanWorker() { s.Sched.StartWorkspaceScanWorker() }

// StartCalendarDispatchScheduler:#263 例行事务 dispatch 半边(到点
// agent_task 事件投递 + 唤醒;幂等由 dispatch 槽位唯一键承担)。
func (s *Service) StartCalendarDispatchScheduler() { s.Sched.StartCalendarDispatchScheduler() }

// StartRunSweeperWorker:#324(#262-M2)失败未决扫描兜底重排(三闸防
// 双跑,详见 sched/run_sweeper.go)。
func (s *Service) StartRunSweeperWorker() { s.Sched.StartRunSweeperWorker() }
func (s *Service) WakeMentionedAgents(companyID string, mentions []string, actorID string) {
	s.Sched.WakeMentionedAgents(companyID, mentions, actorID)
}

// ctxBG:fire-and-forget 后台写与 worker 循环共用的父上下文。默认
// Background;SetBaseContext 由 cmd/server 在 boot 期、任何 Start* 之前
// 注入可取消的 ctx(#215:优雅停机对后台任务生效——此前恒 Background,
// 停机对这里的消费者全部无效)。goroutine 启动即建立 happens-before,
// 注入后不再改写。
var ctxBG = context.Background()

// SetBaseContext:见 ctxBG 注释。须在 worker 启动前调用(boot 期单线程)。
func SetBaseContext(ctx context.Context) { ctxBG = ctx }
