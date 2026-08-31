// polls —— 投票共享引擎(#121 自 polls.ts 固化):kind='poll' 消息行 +
// poll_votes 投票行(主键防同选项双投;事务 DELETE+INSERT 支持改票)。
// HTTP 面(人类)/runtime CLI(agent)/过期清扫器三路共用,保证 WS 广播、
// 校验与 tally 形状跨行为一致。载荷键序对齐 TS PollPayload。
package polls

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
)

const (
	MaxOptions     = 10
	MinOptions     = 2
	MaxQuestionLen = 280
	MaxOptionLen   = 120

	chPolls = "cumora:polls"
)

// PollError:引擎层可预期错误,携带 HTTP 状态(对齐 TS PollError.status)。
type PollError struct {
	Status int
	Msg    string
}

func (e *PollError) Error() string { return e.Msg }

func errf(status int, format string, args ...any) *PollError {
	return &PollError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

type Option struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type Payload struct {
	Question     string   `json:"question"`
	Mode         string   `json:"mode"`
	Options      []Option `json:"options"`
	ExpiresAt    *string  `json:"expiresAt"`
	ClosedAt     *string  `json:"closedAt"`
	ClosedReason *string  `json:"closedReason"`
}

// Tally:每选项计数+投票者 id(排序稳定,对齐 SQL array_agg ORDER BY)。
type Tally struct {
	OptionID string   `json:"optionId"`
	Count    int      `json:"count"`
	VoterIDs []string `json:"voterIds"`
}

// UpdatedEvent:CH_POLLS 扇出载荷(渲染端免 refetch 原地补丁)。
type UpdatedEvent struct {
	Type           string  `json:"type"`
	ConversationID string  `json:"conversationId"`
	CompanyID      string  `json:"companyId"`
	MessageID      string  `json:"messageId"`
	Poll           Payload `json:"poll"`
	Tallies        []Tally `json:"tallies"`
	ActorID        *string `json:"actorId"`
}

type Created struct {
	MessageID string
	Sequence  int
	Poll      Payload
}

func randHex8() string {
	b := make([]byte, 4)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

type CreateArgs struct {
	ConversationID string
	CompanyID      string
	AuthorID       string
	Question       string
	Mode           string // 'single' | 'multi'
	Options        []string
	// 分钟数;nil/0/负 ⇒ 不过期(对齐 TS null/0/undefined 语义)。
	ExpiresInMinutes *float64
}

func Create(ctx context.Context, db *sql.DB, a CreateArgs) (Created, *PollError) {
	question := strings.TrimSpace(a.Question)
	if question == "" {
		return Created{}, errf(400, "question is required")
	}
	if len(question) > MaxQuestionLen {
		return Created{}, errf(400, "question too long (max %d chars)", MaxQuestionLen)
	}
	if a.Mode != "single" && a.Mode != "multi" {
		return Created{}, errf(400, `mode must be "single" or "multi"`)
	}

	var cleaned []Option
	seen := map[string]bool{}
	for _, raw := range a.Options {
		text := strings.TrimSpace(raw)
		if text == "" {
			continue
		}
		if len(text) > MaxOptionLen {
			return Created{}, errf(400, "option too long (max %d chars)", MaxOptionLen)
		}
		key := strings.ToLower(text)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, Option{ID: "opt-" + randHex8(), Text: text})
		if len(cleaned) >= MaxOptions {
			break
		}
	}
	if len(cleaned) < MinOptions {
		return Created{}, errf(400, "need at least %d distinct options", MinOptions)
	}

	var payload Payload
	if a.ExpiresInMinutes != nil && *a.ExpiresInMinutes > 0 {
		// TS Math.floor(分钟)*60_000。
		ms := time.Duration(math.Floor(*a.ExpiresInMinutes)) * time.Minute
		at := time.Now().UTC().Add(ms).Format("2006-01-02T15:04:05.000Z")
		payload.ExpiresAt = &at
	}
	payload.Question = question
	payload.Mode = a.Mode
	payload.Options = cleaned

	// 成员资格边界复检:HTTP 层已查,agent 路径直达引擎,双检不信任调用方。
	var membersJSON string
	if err := db.QueryRowContext(ctx,
		`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2`,
		a.ConversationID, a.CompanyID).Scan(&membersJSON); err != nil {
		return Created{}, errf(404, "conversation not found")
	}
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	isMember := false
	for _, m := range members {
		if m == a.AuthorID {
			isMember = true
			break
		}
	}
	if !isMember {
		return Created{}, errf(403, "not a member of this conversation")
	}

	var sequence int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1`, a.ConversationID).Scan(&sequence); err != nil {
		sequence = 1
	}
	messageID := "m-" + randUUID()
	// body 复述问题:不解包 poll 载荷的消费者(通知/搜索/纯文本日志)仍有可读面。
	body := "📊 " + question
	payloadJSON, _ := json.Marshal(payload)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, poll, company_id)
		VALUES ($1,$2,$3,'poll',$4,$5,$6::jsonb,$7)`,
		messageID, a.ConversationID, a.AuthorID, body, sequence, string(payloadJSON), a.CompanyID); err != nil {
		return Created{}, errf(500, "insert failed")
	}
	_, _ = db.ExecContext(ctx, `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, a.ConversationID)

	// 与文本消息同形入总线(mailbox scheduler 唤醒成员 agent);必须携带
	// 结构化载荷,否则渲染端 PollBubble 拿不到 poll 字段;空 pollTallies
	// 预置防 total=0 的 NaN 除。
	events.MessageNew(ctx, a.CompanyID, a.ConversationID, map[string]any{
		"id": messageID, "conversationId": a.ConversationID, "authorId": a.AuthorID,
		"kind": "poll", "body": body, "sequence": sequence, "at": nowISO(),
		"poll": payload, "pollTallies": []any{},
	})

	return Created{MessageID: messageID, Sequence: sequence, Poll: payload}, nil
}

type CastVoteArgs struct {
	MessageID        string
	CompanyID        string
	VoterParticipant string
	VoterKind        string // 'human' | 'agent'
	OptionIDs        []string
}

func (a *CastVoteArgs) normalize() []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range a.OptionIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// CastVote 替换式投票:空 optionIds ⇒ 撤回;single 模式 >1 拒绝。
func CastVote(ctx context.Context, db *sql.DB, a CastVoteArgs) (UpdatedEvent, *PollError) {
	requested := a.normalize()
	// 事务豁免(#213,#235 复审仍留):引擎面 500 不在 #214 收敛范围
	// (errf 静态文案经 pollHttpError→WriteError 原样透传,无 dev/prod
	// 分流),"tx failed"/"vote replace failed"/"vote insert failed"/
	// "commit failed" 四段文案仍 client-visible,且 WithTx 单一错误通道
	// 无法区分 BeginTx 与 Commit——需阶段化错误 API(另议)才能收编。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return UpdatedEvent{}, errf(500, "tx failed")
	}
	defer tx.Rollback()

	var pollJSON sql.NullString
	var conversationID string
	if err := tx.QueryRowContext(ctx, `
		SELECT poll, conversation_id FROM messages
		 WHERE id = $1 AND company_id = $2 AND kind = 'poll' FOR UPDATE`,
		a.MessageID, a.CompanyID).Scan(&pollJSON, &conversationID); err != nil || !pollJSON.Valid {
		return UpdatedEvent{}, errf(404, "poll not found")
	}
	var poll Payload
	if err := json.Unmarshal([]byte(pollJSON.String), &poll); err != nil {
		return UpdatedEvent{}, errf(404, "poll not found")
	}
	if poll.ClosedAt != nil {
		return UpdatedEvent{}, errf(409, "poll is closed")
	}

	var membersJSON string
	_ = tx.QueryRowContext(ctx,
		`SELECT members::text FROM conversations WHERE id = $1`, conversationID).Scan(&membersJSON)
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	isMember := false
	for _, m := range members {
		if m == a.VoterParticipant {
			isMember = true
			break
		}
	}
	if !isMember {
		return UpdatedEvent{}, errf(403, "not a member of this conversation")
	}

	if poll.Mode == "single" && len(requested) > 1 {
		return UpdatedEvent{}, errf(400, "single-choice poll accepts at most one option")
	}
	valid := map[string]bool{}
	for _, o := range poll.Options {
		valid[o.ID] = true
	}
	for _, id := range requested {
		if !valid[id] {
			return UpdatedEvent{}, errf(400, "unknown option: %s", id)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM poll_votes WHERE message_id = $1 AND voter_participant_id = $2`,
		a.MessageID, a.VoterParticipant); err != nil {
		return UpdatedEvent{}, errf(500, "vote replace failed")
	}
	for _, id := range requested {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO poll_votes (message_id, voter_participant_id, voter_kind, option_id, company_id)
			VALUES ($1,$2,$3,$4,$5)`,
			a.MessageID, a.VoterParticipant, a.VoterKind, id, a.CompanyID); err != nil {
			return UpdatedEvent{}, errf(500, "vote insert failed")
		}
	}
	if err := tx.Commit(); err != nil {
		return UpdatedEvent{}, errf(500, "commit failed")
	}

	event, perr := BuildUpdatedEvent(ctx, db, a.MessageID, &a.VoterParticipant)
	if perr != nil {
		return UpdatedEvent{}, perr
	}
	publishUpdated(ctx, event)
	return event, nil
}

type CloseArgs struct {
	MessageID string
	CompanyID string
	ActorID   *string // manual 必填=作者;expired 清扫传 nil
	Reason    string  // 'manual' | 'expired'
}

// ClosePoll 关闭投票。已关闭 ⇒ 幂等返回 nil(不重播)。manual 非作者 ⇒ 403。
func ClosePoll(ctx context.Context, db *sql.DB, a CloseArgs) (*UpdatedEvent, *PollError) {
	// 事务豁免(#213,#235 复审仍留):已关闭幂等路径 mid-body early-Commit
	// 后短路返回 nil(提交只读事务后不再执行 UPDATE),WithTx 单一提交点
	// 无法表达;begin/commit 500 文案区分同 CastVote(引擎面未经 #214)。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, errf(500, "tx failed")
	}
	defer tx.Rollback()

	var pollJSON sql.NullString
	var authorID string
	if err := tx.QueryRowContext(ctx, `
		SELECT poll, author_id FROM messages
		 WHERE id = $1 AND company_id = $2 AND kind = 'poll' FOR UPDATE`,
		a.MessageID, a.CompanyID).Scan(&pollJSON, &authorID); err != nil || !pollJSON.Valid {
		return nil, errf(404, "poll not found")
	}
	var poll Payload
	if err := json.Unmarshal([]byte(pollJSON.String), &poll); err != nil {
		return nil, errf(404, "poll not found")
	}
	if poll.ClosedAt != nil {
		// 幂等:跳过广播(听者已见过早前关闭事件)。
		if err := tx.Commit(); err != nil {
			return nil, errf(500, "commit failed")
		}
		return nil, nil
	}
	if a.Reason == "manual" && (a.ActorID == nil || *a.ActorID != authorID) {
		return nil, errf(403, "only the poll author can close this poll")
	}
	closedAt := nowISO()
	reason := a.Reason
	poll.ClosedAt = &closedAt
	poll.ClosedReason = &reason
	payloadJSON, _ := json.Marshal(poll)
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET poll = $2::jsonb WHERE id = $1`, a.MessageID, string(payloadJSON)); err != nil {
		return nil, errf(500, "close update failed")
	}
	if err := tx.Commit(); err != nil {
		return nil, errf(500, "commit failed")
	}

	event, perr := BuildUpdatedEvent(ctx, db, a.MessageID, a.ActorID)
	if perr != nil {
		return nil, perr
	}
	publishUpdated(ctx, event)
	return &event, nil
}

// BuildUpdatedEvent 组装 CH_POLLS 扇出载荷(不发布)——show 类只读路径复用。
func BuildUpdatedEvent(ctx context.Context, db *sql.DB, messageID string, actorID *string) (UpdatedEvent, *PollError) {
	var conversationID, companyID string
	var pollJSON sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT conversation_id, company_id, poll FROM messages WHERE id = $1`, messageID).
		Scan(&conversationID, &companyID, &pollJSON); err != nil || !pollJSON.Valid {
		return UpdatedEvent{}, errf(500, "poll vanished mid-update")
	}
	var poll Payload
	if err := json.Unmarshal([]byte(pollJSON.String), &poll); err != nil {
		return UpdatedEvent{}, errf(500, "poll vanished mid-update")
	}

	rows, err := db.QueryContext(ctx, `
		SELECT option_id, COUNT(*)::int,
		       to_json(array_agg(voter_participant_id ORDER BY voter_participant_id)) AS voter_ids
		  FROM poll_votes WHERE message_id = $1 GROUP BY option_id`, messageID)
	if err != nil {
		return UpdatedEvent{}, errf(500, "tally query failed")
	}
	defer rows.Close()
	tallies := []Tally{}
	for rows.Next() {
		var t Tally
		var voterIDs []byte
		if err := rows.Scan(&t.OptionID, &t.Count, &voterIDs); err == nil {
			_ = json.Unmarshal(voterIDs, &t.VoterIDs)
			if t.VoterIDs == nil {
				t.VoterIDs = []string{}
			}
			tallies = append(tallies, t)
		}
	}

	return UpdatedEvent{
		Type: "poll.updated", ConversationID: conversationID, CompanyID: companyID,
		MessageID: messageID, Poll: poll, Tallies: tallies, ActorID: actorID,
	}, nil
}

func publishUpdated(ctx context.Context, e UpdatedEvent) {
	payload, _ := json.Marshal(e)
	_ = events.PublishRaw(ctx, chPolls, payload)
}

// Sweep 过期清扫:关闭所有 expiresAt 已过且未关闭的投票,返回关闭数。
// 单次执行;节拍由调用方(boot 的 ticker)安排。LIMIT 200 对齐 TS。
func Sweep(ctx context.Context, db *sql.DB) int {
	rows, err := db.QueryContext(ctx, `
		SELECT id, company_id FROM messages
		 WHERE kind = 'poll' AND poll IS NOT NULL
		   AND (poll->>'closedAt') IS NULL
		   AND (poll->>'expiresAt') IS NOT NULL
		   AND (poll->>'expiresAt')::timestamptz <= NOW()
		 LIMIT 200`)
	if err != nil {
		return 0
	}
	type target struct{ id, companyID string }
	var all []target
	for rows.Next() {
		var t target
		if rows.Scan(&t.id, &t.companyID) == nil {
			all = append(all, t)
		}
	}
	rows.Close()
	closed := 0
	for _, t := range all {
		if event, perr := ClosePoll(ctx, db, CloseArgs{
			MessageID: t.id, CompanyID: t.companyID, ActorID: nil, Reason: "expired",
		}); perr == nil && event != nil { // TS: if (event) closed += 1 —— 幂等路径不计
			closed++
		}
	}
	return closed
}

func randUUID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
