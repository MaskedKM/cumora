// runtime 包 service —— #140 拆包后的 HTTP 壳:真身(agent 面 +
// client 数据面)在 internal/agent,唤醒调度/议程在 internal/sched(经
// Sched 字段与壳代理可达);本包自留 /runtime/* 路由、扫描、presence,
// 并承担域子包与调度域的接线。
package runtime

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"

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

// StartScheduler / StartIdleScheduler / WakeMentionedAgents:main.go 与
// 域挂载的启动/回调入口(调度域实装的壳代理)。
func (s *Service) StartScheduler()     { s.Sched.StartScheduler() }
func (s *Service) StartIdleScheduler() { s.Sched.StartIdleScheduler() }
func (s *Service) WakeMentionedAgents(companyID string, mentions []string, actorID string) {
	s.Sched.WakeMentionedAgents(companyID, mentions, actorID)
}

// ctxBG:fire-and-forget 后台写共用的父上下文。
var ctxBG = context.Background()

// envIntRaw / getenv:调度/议程面(#140 拆出)同名助手的本包副本
// (scanner 的间隔/门控 env 仍在此读)。
func envIntRaw(name string) (int64, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func getenv(name string) string { return os.Getenv(name) }
