// /runtime/cli 群成员组(#89):leave / invite / kick + 成员变更系统消息
// (membership.ts postMembershipSystemMessage 等价)。
package membership

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	agent "github.com/MaskedKM/cumora/apps/server-go/internal/agent"
)

// Domain:域子包接收器——嵌入 agent.Service(内核),方法体与拆包前逐字
// 对齐(#140 刀法)。
type Domain struct {
	*agent.Service
}

// cliJSONKV:按键的书写序拼 JSON 对象(Go map 会按字母序重排;TS
// JSON.stringify 按字面量序,系统消息 body 的键序要逐字节对齐)。
func cliJSONKV(pairs ...[2]string) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.Write(agent.JSONString(p[0]))
		sb.WriteByte(':')
		sb.Write(agent.JSONString(p[1]))
	}
	sb.WriteByte('}')
	return sb.String()
}

// cliPostMembershipSystemMessage:插入 kind='system' 的成员变更行并广播。
// 相对 members 数组变更的调用次序是语义的一部分:
//   - joined:在 members 更新之后调(新成员已在数组里,调度器会唤醒他);
//   - left / kicked:在移除之前调(邮箱按当前 members 过滤,先移除他就
//     看不见解释"为什么这个会话安静了"的那行系统消息)。
func (s *Domain) cliPostMembershipSystemMessage(ctx context.Context, conversationID string, companyID *string,
	actorID, kind, participantID string) (messageID string, sequence int64, err error) {
	messageID = "m-" + agent.UUIDHex()
	sequence, err = s.NextConversationSequence(ctx, conversationID)
	if err != nil {
		return "", 0, err
	}
	body := cliJSONKV(
		[2]string{"kind", kind},
		[2]string{"participantId", participantID},
		[2]string{"actorId", actorID},
	)
	companyArg := any(nil)
	if companyID != nil {
		companyArg = *companyID
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		 VALUES ($1,$2,$3,'system',$4,$5,$6)`,
		messageID, conversationID, actorID, body, sequence, companyArg); err != nil {
		return "", 0, err
	}
	agent.EventsPublishMessageNew(ctx, companyID, conversationID, map[string]any{
		"id":             messageID,
		"conversationId": conversationID,
		"authorId":       actorID,
		"kind":           "system",
		"body":           body,
		"sequence":       sequence,
		"at":             agent.ISONowMs(),
	})
	return messageID, sequence, nil
}

/* ───────── leave ───────── */

type cliConvoInfo struct {
	kind      string
	title     string
	members   agent.StrArr
	companyID *string
}

func (s *Domain) cliLoadConvoInfo(ctx context.Context, convoID string) (*cliConvoInfo, error) {
	var c cliConvoInfo
	err := s.DB.QueryRowContext(ctx,
		`SELECT kind, title, members, company_id FROM conversations WHERE id = $1`, convoID,
	).Scan(&c.kind, &c.title, &c.members, &c.companyID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Domain) CmdLeave(ctx context.Context, parsed agent.Parsed) agent.Result {
	me, err := agent.ResolveAs(parsed)
	if err != nil {
		return agent.ErrThrow(err)
	}
	if len(parsed.Positional()) == 0 || parsed.Positional()[0] == "" {
		return agent.Err("usage: leave <conversation_id>")
	}
	convoID := parsed.Positional()[0]
	c, err := s.cliLoadConvoInfo(ctx, convoID)
	if err != nil {
		return agent.ErrThrow(err)
	}
	if c == nil {
		return agent.Err("unknown conversation " + convoID)
	}
	if c.kind == "direct" {
		return agent.Err("cannot leave a direct conversation — use `cumora ack` to mute it from your inbox instead")
	}
	if !agent.ContainsString(c.members, me) {
		return agent.Err(me + " is not a member of " + convoID)
	}
	// 先发系统消息再改 members:离场 agent 的邮箱按 c.members 过滤,这行
	// "告别消息"仍会在他下次唤醒时出现——他由此干净地感知自己的离场。
	sysMsg, _, err := s.cliPostMembershipSystemMessage(ctx, convoID, c.companyID, me, "left", me)
	if err != nil {
		return agent.ErrThrow(err)
	}
	next := c.members.Without(me)
	if err := s.cliUpdateMembers(ctx, convoID, next); err != nil {
		return agent.ErrThrow(err)
	}
	effect := agent.CliSideEffect{
		"event":           "conversation.membership_changed",
		"command":         "leave",
		"action":          "left",
		"conversationId":  convoID,
		"actorId":         me,
		"participantId":   me,
		"systemMessageId": sysMsg,
		"memberCount":     len(next),
		"visibleToUser":   true,
	}
	if c.companyID != nil {
		effect["companyId"] = *c.companyID
	}
	return agent.OK(fmt.Sprintf("left %q (%s); %d member(s) remain", c.title, convoID, len(next)), effect)
}

func (s *Domain) cliUpdateMembers(ctx context.Context, convoID string, members agent.StrArr) error {
	b, err := agent.MarshalStrings(members)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		`UPDATE conversations SET members = $2::jsonb, updated_at = NOW() WHERE id = $1`,
		convoID, b)
	return err
}

/* ───────── invite ───────── */

func (s *Domain) CmdInvite(ctx context.Context, parsed agent.Parsed) agent.Result {
	me, err := agent.ResolveAs(parsed)
	if err != nil {
		return agent.ErrThrow(err)
	}
	if len(parsed.Positional()) < 2 || parsed.Positional()[0] == "" || parsed.Positional()[1] == "" {
		return agent.Err("usage: invite <conversation_id> <member_id>")
	}
	convoID, target := parsed.Positional()[0], parsed.Positional()[1]
	if target == me {
		return agent.Err(me + " is already the one inviting")
	}
	c, err := s.cliLoadConvoInfo(ctx, convoID)
	if err != nil {
		return agent.ErrThrow(err)
	}
	if c == nil {
		return agent.Err("unknown conversation " + convoID)
	}
	if c.kind == "direct" {
		return agent.Err("cannot invite into a direct conversation — use `cumora pull-group` to start a fresh thread")
	}
	if !agent.ContainsString(c.members, me) {
		return agent.Err(me + " is not a member of " + convoID + " — can't invite into a group you're not in")
	}
	if agent.ContainsString(c.members, target) {
		return agent.OK(target + " is already a member of " + convoID)
	}
	// 受邀人必须存在于本租户。
	if c.companyID != nil {
		var id string
		err := s.DB.QueryRowContext(ctx,
			`SELECT id FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`,
			target, *c.companyID).Scan(&id)
		if err == sql.ErrNoRows {
			return agent.Err(target + " is not a participant in this workspace")
		}
		if err != nil {
			return agent.ErrThrow(err)
		}
	}
	next := append(append(agent.StrArr{}, c.members...), target)
	if err := s.cliUpdateMembers(ctx, convoID, next); err != nil {
		return agent.ErrThrow(err)
	}
	sysMsg, _, err := s.cliPostMembershipSystemMessage(ctx, convoID, c.companyID, me, "joined", target)
	if err != nil {
		return agent.ErrThrow(err)
	}
	effect := agent.CliSideEffect{
		"event":           "conversation.membership_changed",
		"command":         "invite",
		"action":          "joined",
		"conversationId":  convoID,
		"actorId":         me,
		"participantId":   target,
		"systemMessageId": sysMsg,
		"memberCount":     len(next),
		"visibleToUser":   true,
	}
	if c.companyID != nil {
		effect["companyId"] = *c.companyID
	}
	return agent.OK(fmt.Sprintf("invited %s into %q (%s); %d member(s) total", target, c.title, convoID, len(next)), effect)
}

/* ───────── kick ───────── */

func (s *Domain) CmdKick(ctx context.Context, parsed agent.Parsed) agent.Result {
	me, err := agent.ResolveAs(parsed)
	if err != nil {
		return agent.ErrThrow(err)
	}
	if len(parsed.Positional()) < 2 || parsed.Positional()[0] == "" || parsed.Positional()[1] == "" {
		return agent.Err("usage: kick <conversation_id> <member_id>")
	}
	convoID, target := parsed.Positional()[0], parsed.Positional()[1]
	if target == me {
		return agent.Err("use `cumora leave <convo_id>` to leave a group yourself")
	}
	c, err := s.cliLoadConvoInfo(ctx, convoID)
	if err != nil {
		return agent.ErrThrow(err)
	}
	if c == nil {
		return agent.Err("unknown conversation " + convoID)
	}
	if c.kind == "direct" {
		return agent.Err("cannot kick from a direct conversation")
	}
	if !agent.ContainsString(c.members, me) {
		return agent.Err(me + " is not a member of " + convoID + " — can't kick from a group you're not in")
	}
	if !agent.ContainsString(c.members, target) {
		return agent.Err(target + " is not a member of " + convoID)
	}
	next := c.members.Without(target)
	// kick 不许顺手清空群:只剩自己时要显式 --confirm-empty。
	if len(next) == 1 && !parsed.FlagTruey("confirm-empty") {
		return agent.Err("kicking " + target + " would leave only " + me + " in this group; pass --confirm-empty if that's intended")
	}
	// 先发系统消息再移除:邮箱按当前 members 过滤,先移除被踢者就永远
	// 看不见解释"为什么这个会话安静了"的那行——先发让他带着这行醒来。
	sysMsg, _, err := s.cliPostMembershipSystemMessage(ctx, convoID, c.companyID, me, "kicked", target)
	if err != nil {
		return agent.ErrThrow(err)
	}
	if err := s.cliUpdateMembers(ctx, convoID, next); err != nil {
		return agent.ErrThrow(err)
	}
	effect := agent.CliSideEffect{
		"event":           "conversation.membership_changed",
		"command":         "kick",
		"action":          "kicked",
		"conversationId":  convoID,
		"actorId":         me,
		"participantId":   target,
		"systemMessageId": sysMsg,
		"memberCount":     len(next),
		"visibleToUser":   true,
	}
	if c.companyID != nil {
		effect["companyId"] = *c.companyID
	}
	return agent.OK(fmt.Sprintf("kicked %s from %q (%s); %d member(s) remain", target, c.title, convoID, len(next)), effect)
}
