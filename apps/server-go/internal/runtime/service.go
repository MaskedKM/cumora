// runtime 包 service —— #60 运行时服务面的依赖容器。
// 对齐 server/src/agents/runtime/(server.ts + inproc-client.ts + wake-bus.ts)
// 的服务端:HTTP 面 + 数据面 + 唤醒总线。
package runtime

import (
	"context"
	"database/sql"

	"github.com/MaskedKM/cumora/apps/server-go/internal/wakebus"
	"github.com/redis/go-redis/v9"
	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
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

// rdbOrNil:子功能取 Redis 客户端(nil = 降级路径)。
func (s *Service) redis() redis.UniversalClient { return s.RDB }

// ctxBG:fire-and-forget 后台写共用的父上下文。
var ctxBG = context.Background()
