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
	// 文档域(#55):doc.changed 走 CH_DOCS;协同 update/awareness 由
	// docrelay 直接订阅(sidecar 扇出通道);mention 走 CH_DOC_MENTION。
	ChDocs       = "cumora:docs"
	ChDocUpdate  = "cumora:doc.update"
	ChDocAware   = "cumora:doc.awareness"
	ChDocMention = "cumora:doc.mention"
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

// PublishRaw 供域包广播自定义通道(如 CH_BOARDS)。
func PublishRaw(ctx context.Context, channel string, payload []byte) error {
	return active.Publish(ctx, channel, payload)
}

// DocChanged 广播文档生命周期事件(载荷对齐 publishDocumentChanged)。
func DocChanged(ctx context.Context, companyID, documentID, kind, actorID string) {
	payload := map[string]any{
		"type":       "doc.changed",
		"kind":       kind,
		"companyId":  companyID,
		"documentId": documentID,
		"actorId":    actorID,
	}
	_ = publishJSON(ctx, ChDocs, payload)
}

// DocMention 广播文档 @mention 事件(载荷对齐 DocMentionEvent)。
func DocMention(ctx context.Context, companyID, documentID, documentTitle, mentionerID, mentionerName string, mentionedIDs []string) {
	payload := map[string]any{
		"type":          "doc.mention",
		"companyId":     companyID,
		"documentId":    documentID,
		"documentTitle": documentTitle,
		"mentionerId":   mentionerID,
		"mentionerName": mentionerName,
		"mentionedIds":  mentionedIDs,
	}
	_ = publishJSON(ctx, ChDocMention, payload)
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
