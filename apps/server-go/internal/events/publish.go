// events —— Redis 事件广播(#53):Go 侧消息/typing 事件的 publish 面。
// 通道与载荷对齐 server/src/redis.ts 的 MessageNewEvent/TypingEvent。
package events

import (
	"context"
	"encoding/json"
	"log/slog"
)

const (
	ChMessageNew = "cumora:msg.new"
	ChTyping     = "cumora:typing"
)

// Publisher 抽象 pg 池上的 Redis 客户端;骨架期由 noop 兜底,
// #60(运行时服务面)引入真实 Redis 连接后注入。
type Publisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

// NoopPublisher:无 Redis 时的静默兜底(单机模式事件不走广播面)。
type NoopPublisher struct{}

func (NoopPublisher) Publish(_ context.Context, _ string, _ []byte) error { return nil }

var active Publisher = NoopPublisher{}

func SetPublisher(p Publisher) { active = p }

// MessageNew 广播一条新消息(载荷对齐 MessageNewEvent)。
func MessageNew(ctx context.Context, companyID, convID string, msg map[string]any) {
	payload := map[string]any{
		"type":           "message.new",
		"companyId":      companyID,
		"conversationId": convID,
		"message":        msg,
	}
	_ = publishJSON(ctx, ChMessageNew, payload)
}

// Typing 广播输入态(done=false 开始 / true 停止)。
func Typing(ctx context.Context, companyID, convID, agentID string, done bool) {
	payload := map[string]any{
		"type":           "typing",
		"companyId":      companyID,
		"conversationId": convID,
		"agentId":        agentID,
		"done":           done,
	}
	_ = publishJSON(ctx, ChTyping, payload)
}

func publishJSON(ctx context.Context, channel string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := active.Publish(ctx, channel, b); err != nil {
		slog.Warn("redis publish failed", "channel", channel, "err", err)
	}
	return nil
}
