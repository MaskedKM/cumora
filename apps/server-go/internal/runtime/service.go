// runtime 包 service —— #140 拆包后的 HTTP/调度壳:真身(agent 面 +
// client 数据面)在 internal/agent,本包经嵌入承接全部既有方法,并保留
// 唤醒调度、agenda、扫描、presence、/runtime/* 路由面。
package runtime

import (
	"context"
	"database/sql"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
	"github.com/redis/go-redis/v9"
)

// Service:一个进程一份。方法面在 agent.Service(嵌入提升,既有调用点
// 零改动);调度/扫描/presence/路由方法定义在本包。
type Service struct {
	*agent.Service
}

func New(db *sql.DB, rdb redis.UniversalClient) *Service {
	core := agent.New(db, rdb)
	svc := &Service{Service: core}
	// 唤醒钩子:agent 面(cli boards manual 唤醒)→ 本包 wakeOne
	// (预算/steer/bus 投递住在这里)。
	core.SetWakeHook(func(agentID, reason string, conversationID *string) {
		svc.wakeOne(agentID, reason, conversationID, nil, nil)
	})
	return svc
}

// SetRelay:main 在 relay 构造后注入(避免构造环)。
func (s *Service) SetRelay(r *docrelay.Relay) { s.Service.SetRelay(r) }

// redisOrNil:子功能取 Redis 客户端(nil = 降级路径)。
func (s *Service) redis() redis.UniversalClient { return s.RDB }

// ctxBG:fire-and-forget 后台写共用的父上下文。
var ctxBG = context.Background()
