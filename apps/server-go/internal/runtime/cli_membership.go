// /runtime/cli 群成员组(#89):leave / invite / kick + 成员变更系统消息
// (membership.ts postMembershipSystemMessage 等价)。
package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// cliJSONKV:按键的书写序拼 JSON 对象(Go map 会按字母序重排;TS
// JSON.stringify 按字面量序,系统消息 body 的键序要逐字节对齐)。
func cliJSONKV(pairs ...[2]string) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.Write(cliJSONString(p[0]))
		sb.WriteByte(':')
		sb.Write(cliJSONString(p[1]))
	}
	sb.WriteByte('}')
	return sb.String()
}

func cliJSONString(s string) []byte {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, "\\u%04x", r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return []byte(sb.String())
}

// cliPostMembershipSystemMessage:插入 kind='system' 的成员变更行并广播。
// 相对 members 数组变更的调用次序是语义的一部分:
//   - joined:在 members 更新之后调(新成员已在数组里,调度器会唤醒他);
//   - left / kicked:在移除之前调(邮箱按当前 members 过滤,先移除他就
//     看不见解释"为什么这个会话安静了"的那行系统消息)。
func (s *Service) cliPostMembershipSystemMessage(ctx context.Context, conversationID string, companyID *string,
	actorID, kind, participantID string) (messageID string, sequence int64, err error) {
	messageID = "m-" + uuidHex()
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
	eventsPublishMessageNew(ctx, companyID, conversationID, map[string]any{
		"id":             messageID,
		"conversationId": conversationID,
		"authorId":       actorID,
		"kind":           "system",
		"body":           body,
		"sequence":       sequence,
		"at":             isoNowMs(),
	})
	return messageID, sequence, nil
}

/* ───────── leave ───────── */

type cliConvoInfo struct {
	kind      string
	title     string
	members   cliStrArr
	companyID *string
}

func (s *Service) cliLoadConvoInfo(ctx context.Context, convoID string) (*cliConvoInfo, error) {
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

func (s *Service) cliCmdLeave(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: leave <conversation_id>")
	}
	convoID := parsed.positional[0]
	c, err := s.cliLoadConvoInfo(ctx, convoID)
	if err != nil {
		return cliErrThrow(err)
	}
	if c == nil {
		return cliErr("unknown conversation " + convoID)
	}
	if c.kind == "direct" {
		return cliErr("cannot leave a direct conversation — use `cumora ack` to mute it from your inbox instead")
	}
	if !containsString(c.members, me) {
		return cliErr(me + " is not a member of " + convoID)
	}
	// 先发系统消息再改 members:离场 agent 的邮箱按 c.members 过滤,这行
	// "告别消息"仍会在他下次唤醒时出现——他由此干净地感知自己的离场。
	sysMsg, _, err := s.cliPostMembershipSystemMessage(ctx, convoID, c.companyID, me, "left", me)
	if err != nil {
		return cliErrThrow(err)
	}
	next := c.members.without(me)
	if err := s.cliUpdateMembers(ctx, convoID, next); err != nil {
		return cliErrThrow(err)
	}
	effect := cliSideEffect{
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
	return cliOK(fmt.Sprintf("left %q (%s); %d member(s) remain", c.title, convoID, len(next)), effect)
}

func (s *Service) cliUpdateMembers(ctx context.Context, convoID string, members cliStrArr) error {
	b, err := jsonMarshalStrings(members)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		`UPDATE conversations SET members = $2::jsonb, updated_at = NOW() WHERE id = $1`,
		convoID, b)
	return err
}

/* ───────── invite ───────── */

func (s *Service) cliCmdInvite(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) < 2 || parsed.positional[0] == "" || parsed.positional[1] == "" {
		return cliErr("usage: invite <conversation_id> <member_id>")
	}
	convoID, target := parsed.positional[0], parsed.positional[1]
	if target == me {
		return cliErr(me + " is already the one inviting")
	}
	c, err := s.cliLoadConvoInfo(ctx, convoID)
	if err != nil {
		return cliErrThrow(err)
	}
	if c == nil {
		return cliErr("unknown conversation " + convoID)
	}
	if c.kind == "direct" {
		return cliErr("cannot invite into a direct conversation — use `cumora pull-group` to start a fresh thread")
	}
	if !containsString(c.members, me) {
		return cliErr(me + " is not a member of " + convoID + " — can't invite into a group you're not in")
	}
	if containsString(c.members, target) {
		return cliOK(target + " is already a member of " + convoID)
	}
	// 受邀人必须存在于本租户。
	if c.companyID != nil {
		var id string
		err := s.DB.QueryRowContext(ctx,
			`SELECT id FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`,
			target, *c.companyID).Scan(&id)
		if err == sql.ErrNoRows {
			return cliErr(target + " is not a participant in this workspace")
		}
		if err != nil {
			return cliErrThrow(err)
		}
	}
	next := append(append(cliStrArr{}, c.members...), target)
	if err := s.cliUpdateMembers(ctx, convoID, next); err != nil {
		return cliErrThrow(err)
	}
	sysMsg, _, err := s.cliPostMembershipSystemMessage(ctx, convoID, c.companyID, me, "joined", target)
	if err != nil {
		return cliErrThrow(err)
	}
	effect := cliSideEffect{
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
	return cliOK(fmt.Sprintf("invited %s into %q (%s); %d member(s) total", target, c.title, convoID, len(next)), effect)
}

/* ───────── kick ───────── */

func (s *Service) cliCmdKick(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) < 2 || parsed.positional[0] == "" || parsed.positional[1] == "" {
		return cliErr("usage: kick <conversation_id> <member_id>")
	}
	convoID, target := parsed.positional[0], parsed.positional[1]
	if target == me {
		return cliErr("use `cumora leave <convo_id>` to leave a group yourself")
	}
	c, err := s.cliLoadConvoInfo(ctx, convoID)
	if err != nil {
		return cliErrThrow(err)
	}
	if c == nil {
		return cliErr("unknown conversation " + convoID)
	}
	if c.kind == "direct" {
		return cliErr("cannot kick from a direct conversation")
	}
	if !containsString(c.members, me) {
		return cliErr(me + " is not a member of " + convoID + " — can't kick from a group you're not in")
	}
	if !containsString(c.members, target) {
		return cliErr(target + " is not a member of " + convoID)
	}
	next := c.members.without(target)
	// kick 不许顺手清空群:只剩自己时要显式 --confirm-empty。
	if len(next) == 1 && !parsed.flagTruey("confirm-empty") {
		return cliErr("kicking " + target + " would leave only " + me + " in this group; pass --confirm-empty if that's intended")
	}
	// 先发系统消息再移除:邮箱按当前 members 过滤,先移除被踢者就永远
	// 看不见解释"为什么这个会话安静了"的那行——先发让他带着这行醒来。
	sysMsg, _, err := s.cliPostMembershipSystemMessage(ctx, convoID, c.companyID, me, "kicked", target)
	if err != nil {
		return cliErrThrow(err)
	}
	if err := s.cliUpdateMembers(ctx, convoID, next); err != nil {
		return cliErrThrow(err)
	}
	effect := cliSideEffect{
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
	return cliOK(fmt.Sprintf("kicked %s from %q (%s); %d member(s) remain", target, c.title, convoID, len(next)), effect)
}
