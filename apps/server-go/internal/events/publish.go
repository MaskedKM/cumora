// events —— Redis 事件广播(#53):Go 侧消息/typing 事件的 publish 面。
// 通道名、事件名与载荷结构体(#221 起)全部来自契约生成物 ws.gen.go
// (packages/contract/ws-events.json → npm run contract:gen 再生);本文件
// 只做组装与发布,不再内联任何事件 type/通道字面量 —— 手写漂移由
// scripts/contract-check.sh 的守卫 grep 拦截。
package events

import (
	"context"
	"encoding/json"
	"log/slog"
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

// MessageNew 广播一条新消息(载荷=契约 MessageNewEvent)。companyId 为空
// 时省键——TS 是 `companyId: x ?? undefined`,JSON 序列化即无此键;
// 空串键在消费端虽为假值等价,但线上形状保持一致。
func MessageNew(ctx context.Context, companyID, convID string, msg map[string]any) {
	_ = publishJSON(ctx, ChMessageNew, MessageNewEvent{
		Type:           EventMessageNew,
		CompanyID:      companyID,
		ConversationID: convID,
		Message:        msg,
	})
}

// Typing 广播输入态(done=false 开始 / true 停止)。
func Typing(ctx context.Context, companyID, convID, agentID string, done bool) {
	_ = publishJSON(ctx, ChTyping, TypingEvent{
		Type:           EventTyping,
		CompanyID:      companyID,
		ConversationID: convID,
		AgentID:        agentID,
		Done:           done,
	})
}

// PublishRaw 供域包广播自定义通道(如 CH_BOARDS)。
func PublishRaw(ctx context.Context, channel string, payload []byte) error {
	return active.Publish(ctx, channel, payload)
}

// DocChanged 广播文档生命周期事件(载荷=契约 DocIndexEvent)。
func DocChanged(ctx context.Context, companyID, documentID, kind, actorID string) {
	_ = publishJSON(ctx, ChDocs, DocIndexEvent{
		Type:       EventDocIndex,
		Kind:       kind,
		CompanyID:  companyID,
		DocumentID: documentID,
		ActorID:    actorID,
	})
}

// DocMention 广播文档 @mention 事件(载荷=契约 DocMentionEvent)。
func DocMention(ctx context.Context, companyID, documentID, documentTitle, mentionerID, mentionerName string, mentionedIDs []string) {
	_ = publishJSON(ctx, ChDocMention, DocMentionEvent{
		Type:          EventDocMention,
		CompanyID:     companyID,
		DocumentID:    documentID,
		DocumentTitle: documentTitle,
		MentionerID:   mentionerID,
		MentionerName: mentionerName,
		MentionedIDs:  mentionedIDs,
	})
}

func publishJSON(ctx context.Context, channel string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := active.Publish(ctx, channel, b); err != nil {
		slog.Warn("redis publish failed", "channel", channel, "err", err)
	}
	return nil
}
