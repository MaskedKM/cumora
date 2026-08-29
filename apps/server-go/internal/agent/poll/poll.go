// cli_poll —— agent CLI 的 poll 命令面(#89)。引擎已固化到
// internal/polls(#121,HTTP/CLI/清扫器三路共用);本文件只做参数整理、
// 输出格式化与本地类型(agent.PollPayload 等,cli_read2 读路径共用)的桥接。
package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/polls"

	agent "github.com/MaskedKM/cumora/apps/server-go/internal/agent"
)

// Domain:域子包接收器——嵌入 agent.Service(内核),方法体与拆包前逐字
// 对齐(#140 刀法)。
type Domain struct {
	*agent.Service
}

type cliPollError struct{ msg string }

func (e *cliPollError) Error() string { return e.msg }

type cliPollCreated struct {
	MessageID string
	Sequence  int
	Poll      agent.PollPayload
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
	Poll           agent.PollPayload
	Tallies        []cliPollTally
	ActorID        *string
}

// ── 引擎桥接(agent.PollPayload 与 polls.Payload 字段一一对应,读路径共用本地类型)──

func payloadFromEngine(p polls.Payload) agent.PollPayload {
	out := agent.PollPayload{Question: p.Question, Mode: p.Mode, ExpiresAt: p.ExpiresAt, ClosedAt: p.ClosedAt, ClosedReason: p.ClosedReason}
	for _, o := range p.Options {
		out.Options = append(out.Options, agent.PollOption{ID: o.ID, Text: o.Text})
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

func (s *Domain) cliCreatePoll(ctx context.Context, conversationID, companyID, authorID, question, mode string, options []string, expiresInMinutes *float64) (cliPollCreated, error) {
	created, perr := polls.Create(ctx, s.DB, polls.CreateArgs{
		ConversationID: conversationID, CompanyID: companyID, AuthorID: authorID,
		Question: question, Mode: mode, Options: options, ExpiresInMinutes: expiresInMinutes,
	})
	if perr != nil {
		return cliPollCreated{}, &cliPollError{perr.Msg}
	}
	return cliPollCreated{MessageID: created.MessageID, Sequence: created.Sequence, Poll: payloadFromEngine(created.Poll)}, nil
}

func (s *Domain) cliCastVote(ctx context.Context, messageID, companyID, voterParticipantID, voterKind string, optionIDs []string) (cliPollUpdatedEvent, error) {
	event, perr := polls.CastVote(ctx, s.DB, polls.CastVoteArgs{
		MessageID: messageID, CompanyID: companyID, VoterParticipant: voterParticipantID,
		VoterKind: voterKind, OptionIDs: optionIDs,
	})
	if perr != nil {
		return cliPollUpdatedEvent{}, &cliPollError{perr.Msg}
	}
	return eventFromEngine(event), nil
}

func (s *Domain) cliClosePoll(ctx context.Context, messageID, companyID string, actorID *string, reason string) (*cliPollUpdatedEvent, error) {
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

func (s *Domain) CmdPoll(ctx context.Context, parsed agent.Parsed) agent.Result {
	sub := ""
	if len(parsed.Positional()) > 0 {
		sub = parsed.Positional()[0]
	}
	if sub == "" {
		return agent.Err(strings.Join([]string{
			"usage:",
			`  poll create <convo_id> "<question>" "<opt1>" "<opt2>" [<opt3>...] [--mode single|multi] [--expires-in <minutes>]`,
			"  poll vote <message_id> <option_id>[,<option_id>...]    # multi-choice: comma-separated. Pass --clear to retract",
			"  poll close <message_id>                                # only the author can close",
			"  poll show <message_id>                                 # current tallies + your vote",
		}, "\n"))
	}
	me, err := agent.ResolveAs(parsed)
	if err != nil {
		return agent.Err(err.Error())
	}
	companyID, err := s.AgentCompany(ctx, me)
	if err != nil {
		return agent.Err(err.Error())
	}
	if companyID == "" {
		return agent.Err(fmt.Sprintf("unknown agent %s (no company)", me))
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
		return agent.Err(fmt.Sprintf("unknown poll subcommand: %s", sub))
	}
}

func pollErrText(err error) string {
	var pe *cliPollError
	if errors.As(err, &pe) {
		return pe.msg
	}
	return err.Error()
}

func (s *Domain) cliPollCreate(ctx context.Context, parsed agent.Parsed, me, companyID string) agent.Result {
	convoID, question := "", ""
	if len(parsed.Positional()) > 1 {
		convoID = parsed.Positional()[1]
	}
	if len(parsed.Positional()) > 2 {
		question = parsed.Positional()[2]
	}
	options := []string{}
	if len(parsed.Positional()) > 3 {
		for _, o := range parsed.Positional()[3:] {
			options = append(options, agent.UnescapeChat(o))
		}
	}
	if convoID == "" || question == "" || len(options) < 2 {
		return agent.Err(`usage: poll create <convo_id> "<question>" "<opt1>" "<opt2>" [<opt3>...] [--mode single|multi] [--expires-in <minutes>]`)
	}
	mode := "single"
	if mv, ok := parsed.FlagStr("mode"); ok && mv == "multi" {
		mode = "multi"
	}
	var expiresInMinutes *float64
	if raw, ok := parsed.FlagStr("expires-in"); ok && raw != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			// TS Math.floor(minutes) * 60_000 —— 取整后再换算毫秒。
			floored := float64(agent.JSFloor(f))
			expiresInMinutes = &floored
		} else {
			return agent.Err("--expires-in must be a number of minutes")
		}
	}
	created, err := s.cliCreatePoll(ctx, convoID, companyID, me, agent.UnescapeChat(question), mode, options, expiresInMinutes)
	if err != nil {
		return agent.Err(fmt.Sprintf("poll create failed: %s", pollErrText(err)))
	}
	opts := make([]string, 0, len(created.Poll.Options))
	for _, o := range created.Poll.Options {
		opts = append(opts, fmt.Sprintf("  %s → %s", o.ID, o.Text))
	}
	head := fmt.Sprintf("poll posted · %s (seq %d)\nmode: %s", created.MessageID, created.Sequence, created.Poll.Mode)
	if created.Poll.ExpiresAt != nil {
		head += fmt.Sprintf("\nexpires: %s", *created.Poll.ExpiresAt)
	}
	return agent.OK(fmt.Sprintf("%s\noptions:\n%s", head, strings.Join(opts, "\n")), agent.CliSideEffect{
		"event":          "message.posted",
		"command":        "poll",
		"conversationId": convoID,
		"messageId":      created.MessageID,
		"authorId":       me,
		"companyId":      companyID,
		"visibleToUser":  true,
	})
}

func (s *Domain) cliPollVote(ctx context.Context, parsed agent.Parsed, me, companyID string) agent.Result {
	messageID := ""
	if len(parsed.Positional()) > 1 {
		messageID = parsed.Positional()[1]
	}
	if messageID == "" {
		return agent.Err("usage: poll vote <message_id> <option_id>[,<option_id>...] [--clear]")
	}
	optsRaw := ""
	if len(parsed.Positional()) > 2 {
		optsRaw = parsed.Positional()[2]
	}
	clear := parsed.FlagTruey("clear")
	optionIDs := []string{}
	if !clear {
		for _, part := range strings.Split(optsRaw, ",") {
			if t := strings.TrimSpace(part); t != "" {
				optionIDs = append(optionIDs, t)
			}
		}
	}
	if !clear && len(optionIDs) == 0 {
		return agent.Err("provide at least one option id, or pass --clear to retract")
	}
	event, err := s.cliCastVote(ctx, messageID, companyID, me, "agent", optionIDs)
	if err != nil {
		return agent.Err(fmt.Sprintf("poll vote failed: %s", pollErrText(err)))
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
		return agent.OK(fmt.Sprintf("vote retracted on %s\n%s", messageID, tally))
	}
	return agent.OK(fmt.Sprintf("vote cast on %s → %s\n%s", messageID, strings.Join(optionIDs, ", "), tally))
}

func (s *Domain) cliPollCloseCmd(ctx context.Context, parsed agent.Parsed, me, companyID string) agent.Result {
	messageID := ""
	if len(parsed.Positional()) > 1 {
		messageID = parsed.Positional()[1]
	}
	if messageID == "" {
		return agent.Err("usage: poll close <message_id>")
	}
	event, err := s.cliClosePoll(ctx, messageID, companyID, &me, "manual")
	if err != nil {
		return agent.Err(fmt.Sprintf("poll close failed: %s", pollErrText(err)))
	}
	if event == nil {
		return agent.OK(fmt.Sprintf("poll %s was already closed", messageID))
	}
	return agent.OK(fmt.Sprintf("poll %s closed", messageID))
}

func (s *Domain) cliPollShow(ctx context.Context, parsed agent.Parsed, me, companyID string) agent.Result {
	messageID := ""
	if len(parsed.Positional()) > 1 {
		messageID = parsed.Positional()[1]
	}
	if messageID == "" {
		return agent.Err("usage: poll show <message_id>")
	}
	var pollJSON []byte
	var authorID string
	err := s.DB.QueryRowContext(ctx, `
		SELECT poll, author_id FROM messages
		 WHERE id = $1 AND company_id = $2 AND kind = 'poll' LIMIT 1`,
		messageID, companyID).Scan(&pollJSON, &authorID)
	if err != nil {
		return agent.Err(fmt.Sprintf("poll %s not found", messageID))
	}
	var poll agent.PollPayload
	if pollJSON == nil || json.Unmarshal(pollJSON, &poll) != nil {
		return agent.Err(fmt.Sprintf("poll %s not found", messageID))
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT option_id, COUNT(*)::int AS cnt,
		       to_json(array_agg(voter_participant_id ORDER BY voter_participant_id)) AS voter_ids
		  FROM poll_votes WHERE message_id = $1 GROUP BY option_id`, messageID)
	if err != nil {
		return agent.ErrCode(fmt.Sprintf("error: %v", err), 2)
	}
	defer rows.Close()
	tallyMap := map[string]cliPollTally{}
	for rows.Next() {
		var t cliPollTally
		var voters agent.StrArr
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
		if agent.ContainsString(t.VoterIDs, me) {
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
	return agent.OK(head + "\n" + strings.Join(lines, "\n"))
}
