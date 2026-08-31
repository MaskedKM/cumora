// /runtime/cli 对话元数据命令组(#89):topic / topic-set / rename,及
// conversation.updated 事件发布助手(原 cli_private.go 拆出,函数体零改动)。
package agent

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
)

/* ───────── topic / topic-set / rename ───────── */

func (s *Service) cliCmdTopicRead(ctx context.Context, parsed cliParsed) cliResult {
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: topic <conversation_id>")
	}
	convoID := parsed.positional[0]
	var topic sql.NullString
	var title string
	err := s.DB.QueryRowContext(ctx,
		`SELECT topic, title FROM conversations WHERE id = $1`, convoID).Scan(&topic, &title)
	if err == sql.ErrNoRows {
		return cliErr("unknown conversation " + convoID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if !topic.Valid || topic.String == "" {
		return cliOK(fmt.Sprintf("(no topic set on %q)", title))
	}
	return cliOK(topic.String)
}

func (s *Service) publishConvoUpdated(conversationID, companyID string, patch map[string]any) {
	payload, err := jsonMarshalOrdered(map[string]any{
		"type":           "conversation.updated",
		"conversationId": conversationID,
		"companyId":      companyID,
		"patch":          patch,
	})
	if err == nil {
		_ = s.publishRaw(events.ChConvoUpdated, payload)
	}
}

func (s *Service) publishRaw(channel string, payload []byte) error {
	if s.RDB == nil {
		return nil
	}
	return s.RDB.Publish(context.Background(), channel, payload).Err()
}

func jsonMarshalOrdered(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := newJSONEncoderNoEscape(&buf).Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

func (s *Service) cliCmdTopicSet(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr(`usage: topic-set <conversation_id> "<text>"  (empty body clears the topic)`)
	}
	convoID := parsed.positional[0]
	raw := strings.TrimSpace(cliUnescapeChat(strings.Join(parsed.positional[1:], " ")))
	var topic any
	if len(raw) > 0 {
		topic = utf16Slice(raw, 200)
	}
	var members cliStrArr
	var companyID string
	err = s.DB.QueryRowContext(ctx,
		`SELECT members, company_id FROM conversations WHERE id = $1`, convoID,
	).Scan(&members, &companyID)
	if err == sql.ErrNoRows {
		return cliErr("unknown conversation " + convoID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if !containsString(members, me) {
		return cliErr(me + " is not a member of " + convoID)
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE conversations SET topic = $2, updated_at = NOW() WHERE id = $1`, convoID, topic); err != nil {
		return cliErrThrow(err)
	}
	s.publishConvoUpdated(convoID, companyID, map[string]any{"topic": topic})
	effect := CliSideEffect{
		"event":          "conversation.topic_updated",
		"command":        "topic-set",
		"conversationId": convoID,
		"actorId":        me,
		"companyId":      companyID,
		"topic":          topic,
		"visibleToUser":  true,
	}
	if topic != nil {
		return cliOK(fmt.Sprintf("topic set: %q", topic.(string)), effect)
	}
	return cliOK("(topic cleared)", effect)
}

func (s *Service) cliCmdRename(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr(`usage: rename <conversation_id> "<new title>"`)
	}
	convoID := parsed.positional[0]
	title := utf16Slice(strings.TrimSpace(cliUnescapeChat(strings.Join(parsed.positional[1:], " "))), 80)
	if title == "" {
		return cliErr("rename requires a non-empty title")
	}
	var members cliStrArr
	var kind, companyID, currentTitle string
	err = s.DB.QueryRowContext(ctx,
		`SELECT members, kind, company_id, title FROM conversations WHERE id = $1`, convoID,
	).Scan(&members, &kind, &companyID, &currentTitle)
	if err == sql.ErrNoRows {
		return cliErr("unknown conversation " + convoID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if kind != "group" {
		return cliErr(fmt.Sprintf("only group chats can be renamed (%s is a %s)", convoID, kind))
	}
	if !containsString(members, me) {
		return cliErr(me + " is not a member of " + convoID)
	}
	// 乐观并发:--if-equals 声明调用方相信的当前标题;不符即拒绝重读。
	if ifEqualsRaw, ok := parsed.flagStr("if-equals"); ok {
		ifEquals := utf16Slice(strings.TrimSpace(cliUnescapeChat(ifEqualsRaw)), 80)
		if currentTitle != ifEquals {
			return cliErr(fmt.Sprintf("stale: current title is %q, you passed --if-equals %q. Re-read with `cumora conversations` and decide if you still want to rename.", currentTitle, ifEquals))
		}
	}
	// 幂等 no-op:标题已是目标值即返回成功,不发事件不广播 —— 压掉 N 个
	// agent 同瞬间改同名的噪声;真正的变化才写穿。
	if currentTitle == title {
		return cliOK(fmt.Sprintf("(no-op — title was already %q)", title))
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE conversations SET title = $2, updated_at = NOW() WHERE id = $1`, convoID, title); err != nil {
		return cliErrThrow(err)
	}
	s.publishConvoUpdated(convoID, companyID, map[string]any{"title": title})
	return cliOK(fmt.Sprintf("renamed to %q (%s)", title, convoID), CliSideEffect{
		"event":          "conversation.renamed",
		"command":        "rename",
		"conversationId": convoID,
		"actorId":        me,
		"companyId":      companyID,
		"title":          title,
		"visibleToUser":  true,
	})
}
