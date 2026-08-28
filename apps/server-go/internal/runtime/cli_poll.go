// cli_poll —— agent CLI 的 poll 命令面(#89)。引擎已固化到
// internal/polls(#121,HTTP/CLI/清扫器三路共用);本文件只做参数整理、
// 输出格式化与本地类型(pollPayload 等,cli_read2 读路径共用)的桥接。
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/polls"
)

type cliPollError struct{ msg string }

func (e *cliPollError) Error() string { return e.msg }

type cliPollCreated struct {
	MessageID string
	Sequence  int
	Poll      pollPayload
}

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

// ── 引擎桥接(pollPayload 与 polls.Payload 字段一一对应,读路径共用本地类型)──

func payloadFromEngine(p polls.Payload) pollPayload {
	out := pollPayload{Question: p.Question, Mode: p.Mode, ExpiresAt: p.ExpiresAt, ClosedAt: p.ClosedAt, ClosedReason: p.ClosedReason}
	for _, o := range p.Options {
		out.Options = append(out.Options, cliPollOption{ID: o.ID, Text: o.Text})
	}
	return out
}

func eventFromEngine(e polls.UpdatedEvent) cliPollUpdatedEvent {
	out := cliPollUpdatedEvent{
		ConversationID: e.ConversationID, CompanyID: e.CompanyID, MessageID: e.MessageID,
		Poll: payloadFromEngine(e.Poll), ActorID: e.ActorID,
	}
	for _, t := range e.Tallies {
		out.Tallies = append(out.Tallies, cliPollTally{OptionID: t.OptionID, Count: t.Count, VoterIDs: t.VoterIDs})
	}
	return out
}

func (s *Service) cliCreatePoll(ctx context.Context, conversationID, companyID, authorID, question, mode string, options []string, expiresInMinutes *float64) (cliPollCreated, error) {
	created, perr := polls.Create(ctx, s.DB, polls.CreateArgs{
		ConversationID: conversationID, CompanyID: companyID, AuthorID: authorID,
		Question: question, Mode: mode, Options: options, ExpiresInMinutes: expiresInMinutes,
	})
	if perr != nil {
		return cliPollCreated{}, &cliPollError{perr.Msg}
	}
	return cliPollCreated{MessageID: created.MessageID, Sequence: created.Sequence, Poll: payloadFromEngine(created.Poll)}, nil
}

func (s *Service) cliCastVote(ctx context.Context, messageID, companyID, voterParticipantID, voterKind string, optionIDs []string) (cliPollUpdatedEvent, error) {
	event, perr := polls.CastVote(ctx, s.DB, polls.CastVoteArgs{
		MessageID: messageID, CompanyID: companyID, VoterParticipant: voterParticipantID,
		VoterKind: voterKind, OptionIDs: optionIDs,
	})
	if perr != nil {
		return cliPollUpdatedEvent{}, &cliPollError{perr.Msg}
	}
	return eventFromEngine(event), nil
}

func (s *Service) cliClosePoll(ctx context.Context, messageID, companyID string, actorID *string, reason string) (*cliPollUpdatedEvent, error) {
	event, perr := polls.ClosePoll(ctx, s.DB, polls.CloseArgs{
		MessageID: messageID, CompanyID: companyID, ActorID: actorID, Reason: reason,
	})
	if perr != nil {
		return nil, &cliPollError{perr.Msg}
	}
	if event == nil {
		return nil, nil
	}
	mapped := eventFromEngine(*event)
	return &mapped, nil
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
