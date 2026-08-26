// events/redispub —— 真 Redis Publisher(#55 引入;#60 推广到全部通道)。
// 文档协同的跨进程链路(Go relay ← sidecar 扇出)必须有真订阅端,
// publish 端顺水推舟换成 go-redis;此前 NoopPublisher 兜底的领域
// (消息/typing/看板)在 REDIS_URL 可达时自动升级为真广播。
package events

import (
	"context"

	"github.com/redis/go-redis/v9"
)

type RedisPublisher struct {
	RDB *redis.Client
}

func (p RedisPublisher) Publish(ctx context.Context, channel string, payload []byte) error {
	return p.RDB.Publish(ctx, channel, payload).Err()
}
