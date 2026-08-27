// runtime 包 polls 面 —— polls.ts 的共享核心(createPoll/castVote/
// closePoll)+ cli.ts 的 cmdPoll(create/vote/close/show)。poll 存在
// messages.poll(jsonb),票在 poll_votes;主键 (message_id, voter,
// option_id) 防重复投同一项,DELETE+INSERT 事务内换票。
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

const (
	pollMaxOptions    = 10
	pollMinOptions    = 2
	pollMaxQuestion   = 280
	pollMaxOptionText = 120
)

// cliPollError:PollError 等价(消息原样透传给 CLI)。
type cliPollError struct{ msg string }

func (e *cliPollError) Error() string { return e.msg }

// cliPollCreated:createPoll 的返回。
type cliPollCreated struct {
	MessageID string
	Sequence  int
	Poll      pollPayload
}

// cliPollTally / cliPollUpdatedEvent:PollUpdatedEvent 形状。
type cliPollTally struct {
	OptionID string
	Count    int
	VoterIDs []string
}

type cliPollUpdatedEvent struct {
	ConversationID string
	CompanyID      string
	MessageID      string
	Poll           pollPayload
	Tallies        []cliPollTally
	ActorID        *string
}

func (s *Service) cliCreatePoll(ctx context.Context, conversationID, companyID, authorID, question, mode string, options []string, expiresInMinutes *float64) (cliPollCreated, error) {
	fail := func(format string, a ...any) (cliPollCreated, error) {
		return cliPollCreated{}, &cliPollError{fmt.Sprintf(format, a...)}
	}
	question = strings.TrimSpace(question)
	if question == "" {
		return fail("question is required")
	}
	if len(question) > pollMaxQuestion {
		return fail("question too long (max %d chars)", pollMaxQuestion)
	}
	if mode != "single" && mode != "multi" {
		return fail(`mode must be "single" or "multi"`)
	}
	cleaned := []cliPollOption{}
	seen := map[string]bool{}
	for _, raw := range options {
		text := strings.TrimSpace(fmt.Sprintf("%v", raw))
		if text == "" {
			continue
		}
		if len(text) > pollMaxOptionText {
			return fail("option too long (max %d chars)", pollMaxOptionText)
		}
		key := strings.ToLower(text)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, cliPollOption{ID: "opt-" + jsUUID()[:8], Text: text})
		if len(cleaned) >= pollMaxOptions {
			break
		}
	}
	if len(cleaned) < pollMinOptions {
		return fail("need at least %d distinct options", pollMinOptions)
	}
	var expiresAt *string
	if expiresInMinutes != nil && *expiresInMinutes > 0 {
		ms := int64(*expiresInMinutes) * 60_000
		expires := httpx.ISOms(time.Now().Add(time.Duration(ms) * time.Millisecond))
		expiresAt = &expires
	}
	payload := pollPayload{Question: question, Mode: mode, Options: cleaned, ExpiresAt: expiresAt, ClosedAt: nil, ClosedReason: nil}

	var members cliStrArr
	err := s.DB.QueryRowContext(ctx,
		`SELECT members FROM conversations WHERE id = $1 AND company_id = $2`,
		conversationID, companyID).Scan(&members)
	if err != nil {
		return fail("conversation not found")
	}
	if !containsString(members, authorID) {
		return fail("not a member of this conversation")
	}

	var sequence int
	err = s.DB.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1 AS seq`, conversationID).Scan(&sequence)
	if err != nil {
		sequence = 1
	}
	messageID := "m-" + jsUUID()
	body := "📊 " + question
	payloadJSON, _ := json.Marshal(payload)
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, poll, company_id)
		VALUES ($1,$2,$3,'poll',$4,$5,$6::jsonb,$7)`,
		messageID, conversationID, authorID, body, sequence, string(payloadJSON), companyID); err != nil {
		return cliPollCreated{}, err
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, conversationID)

	// 与文本消息同通道广播;携带结构化 payload + 空 tallies,渲染端才能
	// 直接出 PollBubble。
	pollCopy := payload
	eventsPublishMessageNew(ctx, &companyID, conversationID, map[string]any{
		"id":             messageID,
		"conversationId": conversationID,
		"authorId":       authorID,
		"kind":           "poll",
		"body":           body,
		"sequence":       sequence,
		"at":             isoNowMs(),
		"poll":           pollCopy,
		"pollTallies":    []any{},
	})
	return cliPollCreated{MessageID: messageID, Sequence: sequence, Poll: payload}, nil
}

func (s *Service) cliCastVote(ctx context.Context, messageID, companyID, voterParticipantID, voterKind string, optionIDs []string) (cliPollUpdatedEvent, error) {
	fail := func(format string, a ...any) (cliPollUpdatedEvent, error) {
		return cliPollUpdatedEvent{}, &cliPollError{fmt.Sprintf(format, a...)}
	}
	requested := []string{}
	reqSeen := map[string]bool{}
	for _, o := range optionIDs {
		if o == "" || reqSeen[o] {
			continue
		}
		reqSeen[o] = true
		requested = append(requested, o)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return cliPollUpdatedEvent{}, err
	}
	defer tx.Rollback()
	var pollJSON []byte
	var convoID, rowCompany string
	err = tx.QueryRowContext(ctx, `
		SELECT poll, conversation_id, company_id
		  FROM messages
		 WHERE id = $1 AND company_id = $2 AND kind = 'poll'
		 FOR UPDATE`, messageID, companyID).Scan(&pollJSON, &convoID, &rowCompany)
	if err != nil || pollJSON == nil {
		return fail("poll not found")
	}
	var poll pollPayload
	if err := json.Unmarshal(pollJSON, &poll); err != nil {
		return fail("poll not found")
	}
	if poll.ClosedAt != nil {
		return fail("poll is closed")
	}
	var members cliStrArr
	if tx.QueryRowContext(ctx, `SELECT members FROM conversations WHERE id = $1`, convoID).Scan(&members) != nil {
		members = nil
	}
	if !containsString(members, voterParticipantID) {
		return fail("not a member of this conversation")
	}
	if poll.Mode == "single" && len(requested) > 1 {
		return fail("single-choice poll accepts at most one option")
	}
	validIDs := map[string]bool{}
	for _, o := range poll.Options {
		validIDs[o.ID] = true
	}
	for _, optID := range requested {
		if !validIDs[optID] {
			return fail("unknown option: %s", optID)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM poll_votes WHERE message_id = $1 AND voter_participant_id = $2`,
		messageID, voterParticipantID); err != nil {
		return cliPollUpdatedEvent{}, err
	}
	for _, optID := range requested {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO poll_votes (message_id, voter_participant_id, voter_kind, option_id, company_id)
			VALUES ($1,$2,$3,$4,$5)`, messageID, voterParticipantID, voterKind, optID, companyID); err != nil {
			return cliPollUpdatedEvent{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return cliPollUpdatedEvent{}, err
	}
	event, err := s.cliBuildPollUpdatedEvent(ctx, messageID, &voterParticipantID)
	if err != nil {
		return cliPollUpdatedEvent{}, err
	}
	s.cliPublishPollUpdated(event)
	return event, nil
}

func (s *Service) cliClosePoll(ctx context.Context, messageID, companyID string, actorID *string, reason string) (*cliPollUpdatedEvent, error) {
	fail := func(format string, a ...any) (*cliPollUpdatedEvent, error) {
		return nil, &cliPollError{fmt.Sprintf(format, a...)}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pollJSON []byte
	var authorID string
	err = tx.QueryRowContext(ctx, `
		SELECT poll, author_id FROM messages
		 WHERE id = $1 AND company_id = $2 AND kind = 'poll'
		 FOR UPDATE`, messageID, companyID).Scan(&pollJSON, &authorID)
	if err != nil || pollJSON == nil {
		return fail("poll not found")
	}
	var poll pollPayload
	if err := json.Unmarshal(pollJSON, &poll); err != nil {
		return fail("poll not found")
	}
	if poll.ClosedAt != nil {
		// 幂等关闭:不再广播。
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if reason == "manual" && (actorID == nil || *actorID != authorID) {
		return fail("only the poll author can close this poll")
	}
	closedAt := isoNowMs()
	poll.ClosedAt = &closedAt
	poll.ClosedReason = &reason
	closedJSON, _ := json.Marshal(poll)
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET poll = $2::jsonb WHERE id = $1`,
		messageID, string(closedJSON)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	event, err := s.cliBuildPollUpdatedEvent(ctx, messageID, actorID)
	if err != nil {
		return nil, err
	}
	s.cliPublishPollUpdated(event)
	return &event, nil
}

func (s *Service) cliBuildPollUpdatedEvent(ctx context.Context, messageID string, actorID *string) (cliPollUpdatedEvent, error) {
	var convoID, rowCompany string
	var pollJSON []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT conversation_id, company_id, poll FROM messages WHERE id = $1`, messageID).
		Scan(&convoID, &rowCompany, &pollJSON)
	if err != nil || pollJSON == nil {
		return cliPollUpdatedEvent{}, &cliPollError{"poll vanished mid-update"}
	}
	var poll pollPayload
	if err := json.Unmarshal(pollJSON, &poll); err != nil {
		return cliPollUpdatedEvent{}, &cliPollError{"poll vanished mid-update"}
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT option_id, COUNT(*)::int AS cnt,
		       to_json(array_agg(voter_participant_id ORDER BY voter_participant_id)) AS voter_ids
		  FROM poll_votes WHERE message_id = $1 GROUP BY option_id`, messageID)
	if err != nil {
		return cliPollUpdatedEvent{}, err
	}
	defer rows.Close()
	tallies := []cliPollTally{}
	for rows.Next() {
		var t cliPollTally
		var voters cliStrArr
		if rows.Scan(&t.OptionID, &t.Count, &voters) != nil {
			continue
		}
		t.VoterIDs = voters
		tallies = append(tallies, t)
	}
	return cliPollUpdatedEvent{
		ConversationID: convoID,
		CompanyID:      rowCompany,
		MessageID:      messageID,
		Poll:           poll,
		Tallies:        tallies,
		ActorID:        actorID,
	}, rows.Err()
}

func (s *Service) cliPublishPollUpdated(e cliPollUpdatedEvent) {
	actor := any(nil)
	if e.ActorID != nil {
		actor = *e.ActorID
	}
	payload := map[string]any{
		"type":           "poll.updated",
		"conversationId": e.ConversationID,
		"companyId":      e.CompanyID,
		"messageId":      e.MessageID,
		"poll":           e.Poll,
		"tallies":        e.Tallies,
		"actorId":        actor,
	}
	if actor == nil {
		delete(payload, "actorId")
	}
	_ = s.publishRaw("cumora:polls", mustJSON(payload))
}

/* ───────────── CLI 命令面 ───────────── */

func (s *Service) cliCmdPoll(ctx context.Context, parsed cliParsed) cliResult {
	sub := ""
	if len(parsed.positional) > 0 {
		sub = parsed.positional[0]
	}
	if sub == "" {
		return cliErr(strings.Join([]string{
			"usage:",
			`  poll create <convo_id> "<question>" "<opt1>" "<opt2>" [<opt3>...] [--mode single|multi] [--expires-in <minutes>]`,
			"  poll vote <message_id> <option_id>[,<option_id>...]    # multi-choice: comma-separated. Pass --clear to retract",
			"  poll close <message_id>                                # only the author can close",
			"  poll show <message_id>                                 # current tallies + your vote",
		}, "\n"))
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErr(err.Error())
	}
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErr(err.Error())
	}
	if companyID == "" {
		return cliErr(fmt.Sprintf("unknown agent %s (no company)", me))
	}
	switch sub {
	case "create":
		return s.cliPollCreate(ctx, parsed, me, companyID)
	case "vote":
		return s.cliPollVote(ctx, parsed, me, companyID)
	case "close":
		return s.cliPollCloseCmd(ctx, parsed, me, companyID)
	case "show":
		return s.cliPollShow(ctx, parsed, me, companyID)
	default:
		return cliErr(fmt.Sprintf("unknown poll subcommand: %s", sub))
	}
}

func pollErrText(err error) string {
	var pe *cliPollError
	if errors.As(err, &pe) {
		return pe.msg
	}
	return err.Error()
}

func (s *Service) cliPollCreate(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	convoID, question := "", ""
	if len(parsed.positional) > 1 {
		convoID = parsed.positional[1]
	}
	if len(parsed.positional) > 2 {
		question = parsed.positional[2]
	}
	options := []string{}
	if len(parsed.positional) > 3 {
		for _, o := range parsed.positional[3:] {
			options = append(options, cliUnescapeChat(o))
		}
	}
	if convoID == "" || question == "" || len(options) < 2 {
		return cliErr(`usage: poll create <convo_id> "<question>" "<opt1>" "<opt2>" [<opt3>...] [--mode single|multi] [--expires-in <minutes>]`)
	}
	mode := "single"
	if mv, ok := parsed.flagStr("mode"); ok && mv == "multi" {
		mode = "multi"
	}
	var expiresInMinutes *float64
	if raw, ok := parsed.flagStr("expires-in"); ok && raw != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			// TS Math.floor(minutes) * 60_000 —— 取整后再换算毫秒。
			floored := float64(floorJS(f))
			expiresInMinutes = &floored
		} else {
			return cliErr("--expires-in must be a number of minutes")
		}
	}
	created, err := s.cliCreatePoll(ctx, convoID, companyID, me, cliUnescapeChat(question), mode, options, expiresInMinutes)
	if err != nil {
		return cliErr(fmt.Sprintf("poll create failed: %s", pollErrText(err)))
	}
	opts := make([]string, 0, len(created.Poll.Options))
	for _, o := range created.Poll.Options {
		opts = append(opts, fmt.Sprintf("  %s → %s", o.ID, o.Text))
	}
	head := fmt.Sprintf("poll posted · %s (seq %d)\nmode: %s", created.MessageID, created.Sequence, created.Poll.Mode)
	if created.Poll.ExpiresAt != nil {
		head += fmt.Sprintf("\nexpires: %s", *created.Poll.ExpiresAt)
	}
	return cliOK(fmt.Sprintf("%s\noptions:\n%s", head, strings.Join(opts, "\n")), cliSideEffect{
		"event":          "message.posted",
		"command":        "poll",
		"conversationId": convoID,
		"messageId":      created.MessageID,
		"authorId":       me,
		"companyId":      companyID,
		"visibleToUser":  true,
	})
}

func (s *Service) cliPollVote(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	messageID := ""
	if len(parsed.positional) > 1 {
		messageID = parsed.positional[1]
	}
	if messageID == "" {
		return cliErr("usage: poll vote <message_id> <option_id>[,<option_id>...] [--clear]")
	}
	optsRaw := ""
	if len(parsed.positional) > 2 {
		optsRaw = parsed.positional[2]
	}
	clear := parsed.flagTruey("clear")
	optionIDs := []string{}
	if !clear {
		for _, part := range strings.Split(optsRaw, ",") {
			if t := strings.TrimSpace(part); t != "" {
				optionIDs = append(optionIDs, t)
			}
		}
	}
	if !clear && len(optionIDs) == 0 {
		return cliErr("provide at least one option id, or pass --clear to retract")
	}
	event, err := s.cliCastVote(ctx, messageID, companyID, me, "agent", optionIDs)
	if err != nil {
		return cliErr(fmt.Sprintf("poll vote failed: %s", pollErrText(err)))
	}
	tally := "(no votes yet)"
	if len(event.Tallies) > 0 {
		lines := make([]string, 0, len(event.Tallies))
		for _, t := range event.Tallies {
			lines = append(lines, fmt.Sprintf("  %s · %d (%s)", t.OptionID, t.Count, strings.Join(t.VoterIDs, ", ")))
		}
		tally = strings.Join(lines, "\n")
	}
	if clear {
		return cliOK(fmt.Sprintf("vote retracted on %s\n%s", messageID, tally))
	}
	return cliOK(fmt.Sprintf("vote cast on %s → %s\n%s", messageID, strings.Join(optionIDs, ", "), tally))
}

func (s *Service) cliPollCloseCmd(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	messageID := ""
	if len(parsed.positional) > 1 {
		messageID = parsed.positional[1]
	}
	if messageID == "" {
		return cliErr("usage: poll close <message_id>")
	}
	event, err := s.cliClosePoll(ctx, messageID, companyID, &me, "manual")
	if err != nil {
		return cliErr(fmt.Sprintf("poll close failed: %s", pollErrText(err)))
	}
	if event == nil {
		return cliOK(fmt.Sprintf("poll %s was already closed", messageID))
	}
	return cliOK(fmt.Sprintf("poll %s closed", messageID))
}

func (s *Service) cliPollShow(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	messageID := ""
	if len(parsed.positional) > 1 {
		messageID = parsed.positional[1]
	}
	if messageID == "" {
		return cliErr("usage: poll show <message_id>")
	}
	var pollJSON []byte
	var authorID string
	err := s.DB.QueryRowContext(ctx, `
		SELECT poll, author_id FROM messages
		 WHERE id = $1 AND company_id = $2 AND kind = 'poll' LIMIT 1`,
		messageID, companyID).Scan(&pollJSON, &authorID)
	if err != nil {
		return cliErr(fmt.Sprintf("poll %s not found", messageID))
	}
	var poll pollPayload
	if pollJSON == nil || json.Unmarshal(pollJSON, &poll) != nil {
		return cliErr(fmt.Sprintf("poll %s not found", messageID))
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT option_id, COUNT(*)::int AS cnt,
		       to_json(array_agg(voter_participant_id ORDER BY voter_participant_id)) AS voter_ids
		  FROM poll_votes WHERE message_id = $1 GROUP BY option_id`, messageID)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	defer rows.Close()
	tallyMap := map[string]cliPollTally{}
	for rows.Next() {
		var t cliPollTally
		var voters cliStrArr
		if rows.Scan(&t.OptionID, &t.Count, &voters) != nil {
			continue
		}
		t.VoterIDs = voters
		tallyMap[t.OptionID] = t
	}
	lines := make([]string, 0, len(poll.Options))
	for _, o := range poll.Options {
		t, ok := tallyMap[o.ID]
		if !ok {
			t = cliPollTally{VoterIDs: []string{}}
		}
		mine := ""
		if containsString(t.VoterIDs, me) {
			mine = " ← you"
		}
		lines = append(lines, fmt.Sprintf("  %s (%d) · %s%s", o.ID, t.Count, o.Text, mine))
	}
	state := "open"
	if poll.ClosedAt != nil {
		state = fmt.Sprintf("closed at %s", *poll.ClosedAt)
	} else if poll.ExpiresAt != nil {
		state = fmt.Sprintf("expires %s", *poll.ExpiresAt)
	}
	head := strings.Join([]string{
		fmt.Sprintf("poll %s · by %s · mode=%s", messageID, authorID, poll.Mode),
		state,
		poll.Question,
	}, "\n")
	return cliOK(head + "\n" + strings.Join(lines, "\n"))
}
