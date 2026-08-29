// runtime 包 agenda —— agenda.ts + agenda-triage-core.ts + cerebellum-route.ts
// 的心跳议程面:收集 agent 可行动面(看板卡/日历槽/停摆会话)、组分类器
// prompt(纯函数)、确定性回退、停摆 nudge 认领、brief 渲染、按 agent
// 解析 cerebellum 路由。
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/costing"
	"github.com/MaskedKM/cumora/apps/server-go/internal/obs"

	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
)

func getenv(name string) string           { return os.Getenv(name) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

/* ───────── 类型(对齐 agenda-triage-core.ts) ───────── */

type AgendaCard struct {
	ID          string   `json:"id"`
	BoardID     string   `json:"board_id"`
	BoardTitle  string   `json:"board_title"`
	ColumnID    string   `json:"column_id"`
	ColumnTitle string   `json:"column_title"`
	Title       string   `json:"title"`
	Description *string  `json:"description"`
	AssigneeID  *string  `json:"assignee_id"`
	Mentions    []string `json:"mentions"`
	UpdatedAt   string   `json:"updated_at"`
}

type AgendaEvent struct {
	ID                   string  `json:"id"`
	Title                string  `json:"title"`
	Description          *string `json:"description"`
	StartAt              string  `json:"start_at"`
	AgentPrompt          *string `json:"agent_prompt"`
	TargetConversationID *string `json:"target_conversation_id"`
}

type StalledConvo struct {
	ConversationID   string  `json:"conversationId"`
	Kind             string  `json:"kind"`
	Title            *string `json:"title"`
	LastMessageID    string  `json:"lastMessageId"`
	LastAuthorID     string  `json:"lastAuthorId"`
	LastAuthorName   string  `json:"lastAuthorName"`
	LastAuthorIsSelf bool    `json:"lastAuthorIsSelf"`
	LastBody         string  `json:"lastBody"`
	MinutesSilent    int     `json:"minutesSilent"`
	RecentTail       string  `json:"recentTail"`
}

type AgentAgenda struct {
	Cards  []AgendaCard   `json:"cards"`
	Events []AgendaEvent  `json:"events"`
	Stalls []StalledConvo `json:"stalls"`
}

// AgendaVerdict:分类器判定。reason 字面 'classifier error' 是哨兵——
// 调用方应视作"triage 失败走安全路"而非"无事可做",一次瞬断的 LLM
// 故障不得让所有带议程的 agent 沉默。
type AgendaVerdict struct {
	Actionable bool   `json:"actionable"`
	Focus      string `json:"focus"`
	Reason     string `json:"reason"`
}

const AgendaClassifierError = "classifier error"

// AgendaClassifierRequest:空议程即决(不调模型)或 {instructions,input}。
type AgendaClassifierRequest struct {
	Verdict      *AgendaVerdict
	Instructions string
	Input        string
}

/* ───────── 纯函数(agenda-triage-core.ts) ───────── */

// doneColumnRe:列标题命中即"卡已完成,别烦 agent"。大小写不敏感。
var doneColumnRe = regexp.MustCompile(`(?i)\b(done|complete|completed|shipped|archive|archived|closed|cancel|canceled|cancelled)\b`)

// relativeAge:紧凑相对龄("5m ago"/"in 2h")。小模型判预计算 delta 远比
// 减两个绝对时间戳可靠——光秃秃的 updated_at 无 now 锚时它会瞎猜新鲜度
// (曾把 22 秒前的触碰读成 ~22h,卡片被无声饿死)。
func relativeAge(iso string, nowMS int64) string {
	t := parseISOms(iso)
	ms := nowMS - t
	abs := ms
	if abs < 0 {
		abs = -abs
	}
	d := abs / 86_400_000
	h := abs / 3_600_000
	m := abs / 60_000
	s := (abs + 500) / 1000
	var span string
	switch {
	case d > 0:
		span = fmt.Sprintf("%dd", d)
	case h > 0:
		span = fmt.Sprintf("%dh", h)
	case m > 0:
		span = fmt.Sprintf("%dm", m)
	default:
		span = fmt.Sprintf("%ds", s)
	}
	if ms >= 0 {
		return span + " ago"
	}
	return "in " + span
}

// RenderAgendaForClassifier:议程 → 分类器的紧凑文本。卡/停摆按 id 排序
// 保 prompt-cache 稳定;新鲜度相对 now 渲染。
func RenderAgendaForClassifier(agenda AgentAgenda, nowMS int64) string {
	var lines []string
	cards := append([]AgendaCard{}, agenda.Cards...)
	sort.Slice(cards, func(i, j int) bool { return cards[i].ID < cards[j].ID })
	stalls := append([]StalledConvo{}, agenda.Stalls...)
	sort.Slice(stalls, func(i, j int) bool { return stalls[i].ConversationID < stalls[j].ConversationID })

	if len(cards) > 0 {
		lines = append(lines, fmt.Sprintf("Kanban cards (%d):", len(cards)))
		for _, c := range cards {
			tag := "mentioned"
			if c.AssigneeID != nil {
				tag = "assigned"
			}
			// TS `"${c.title}"` 手工引号,不转义(%q 会 Go 引号化)。
			lines = append(lines, fmt.Sprintf(`- [%s] "%s" in %s / %s (id=%s, updated %s, %s)`,
				tag, c.Title, c.BoardTitle, c.ColumnTitle, c.ID, c.UpdatedAt, relativeAge(c.UpdatedAt, nowMS)))
		}
	}
	if len(stalls) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf("Conversations you're in that have gone quiet (%d) — judge each: genuinely WAITING on someone, or already CONCLUDED/wound down?", len(stalls)))
		for _, st := range stalls {
			who := fmt.Sprintf("%s spoke last, you haven't responded", st.LastAuthorName)
			if st.LastAuthorIsSelf {
				who = "YOU spoke last, no reply since"
			}
			title := st.ConversationID
			if st.Title != nil {
				title = *st.Title
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s (%s) — %s. Last few messages:", st.Kind, title, st.ConversationID, who))
			tail := st.RecentTail
			if tail == "" {
				tail = st.LastBody
			}
			for _, ln := range strings.Split(tail, "\n") {
				lines = append(lines, "    "+ln)
			}
		}
	}
	if len(agenda.Events) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf("Calendar events in current slot (%d):", len(agenda.Events)))
		for _, e := range agenda.Events {
			prompt := ""
			if e.AgentPrompt != nil && *e.AgentPrompt != "" {
				prompt = " — prompt: " + sliceUTF16(*e.AgentPrompt, 140)
			}
			lines = append(lines, fmt.Sprintf(`- "%s" at %s (%s)%s`, e.Title, e.StartAt, relativeAge(e.StartAt, nowMS), prompt))
		}
	}
	if len(stalls) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "Current stall timing:")
		for _, st := range stalls {
			lines = append(lines, fmt.Sprintf("- %s: %dm silent", st.ConversationID, st.MinutesSilent))
		}
	}
	if len(lines) == 0 {
		return "(empty)"
	}
	return strings.Join(lines, "\n")
}

// BuildAgendaClassifierRequest:空议程即决;否则组 {instructions, input}。
func BuildAgendaClassifierRequest(persona *Persona, agenda AgentAgenda, nowMS int64) AgendaClassifierRequest {
	if len(agenda.Cards) == 0 && len(agenda.Events) == 0 && len(agenda.Stalls) == 0 {
		return AgendaClassifierRequest{Verdict: &AgendaVerdict{Actionable: false, Focus: "", Reason: "empty agenda"}}
	}
	rendered := RenderAgendaForClassifier(agenda, nowMS)
	styleHint := persona.Style
	if len(styleHint) > 400 {
		styleHint = styleHint[:400]
	}
	if styleHint == "" {
		styleHint = "(none)"
	}
	instructions := `You are Cumora's heartbeat agenda triage. Given an agent's currently-assigned Kanban cards and their currently-due calendar events, decide whether the agent should wake up RIGHT NOW to act on something, or stay quiet.

Decide "actionable: true" only when at least one item is concrete, fresh, AND clearly belongs to this agent's role. Reject:
- vague brainstorming cards with no owner action
- cards already in a done/archive-style column
- events that are personal markers (no agent_prompt)
- duplicates of work the agent has obviously already started

ORDERING: list order is for prompt-cache stability, not priority. Judge urgency from each card's updated timestamp, each event's start time, and each stalled conversation's current silence duration and message content.

STALLED CONVERSATIONS: for each, READ the recent messages shown and judge whether it is genuinely WAITING or already CONCLUDED. actionable=true ONLY when the recent messages show a CONCRETE unanswered ask directed at someone, or an explicitly in-progress step plainly waiting on a next move. It is NOT actionable (false) when the thread has CONCLUDED or socially CLOSED — participants exchanging wrap-up / closing / acknowledgement remarks, a conclusion or result already reached, nothing pending and no one waiting on a specific next step — no matter how "quiet" it now is. A merely quiet conversation is NOT a stall; "someone spoke last and I didn't reply" is NOT by itself a reason (most messages need no reply). Resurrecting a finished conversation with a late reply is a failure — when in doubt that it's truly still waiting, choose false.

Reply ONLY as strict JSON: {"actionable": boolean, "focus": "one-line focus for the agent if actionable, else empty", "reason": "short factual reason"}.`
	role := ""
	if persona.Role != "" {
		role = ", " + persona.Role
	}
	input := fmt.Sprintf("Agent: %s%s\nPersona / style:\n%s\n\nCurrent time: %s\nCurrent agenda for this agent:\n%s\n\nReply as strict JSON.",
		persona.Name, role, styleHint, isoNow(nowMS), rendered)
	return AgendaClassifierRequest{Instructions: instructions, Input: input}
}

func isoNow(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// AgendaDeterministicFallback:分类器不可用时的回退。默认 fail-closed,
// 但窄保留一个保守情形,断供期停摆安全网不至于 100% 失灵:恰好一条
// 近期停摆、是别人说的最后一句话、本 agent 欠回复。
const stallFallbackMaxMin = 30

func AgendaDeterministicFallback(agenda AgentAgenda) AgendaVerdict {
	var recentAwaitingMe []StalledConvo
	for _, st := range agenda.Stalls {
		if !st.LastAuthorIsSelf && st.MinutesSilent <= stallFallbackMaxMin {
			recentAwaitingMe = append(recentAwaitingMe, st)
		}
	}
	if len(agenda.Cards) == 0 && len(agenda.Events) == 0 && len(recentAwaitingMe) == 1 {
		st := recentAwaitingMe[0]
		title := st.ConversationID
		if st.Title != nil {
			title = *st.Title
		}
		return AgendaVerdict{
			Actionable: true,
			Focus:      fmt.Sprintf("Reply to %s in %s — they spoke last (%dm ago) and you haven't responded.", st.LastAuthorName, title, st.MinutesSilent),
			Reason:     AgendaClassifierError + " (deterministic fallback: single recent awaiting-you stall)",
		}
	}
	return AgendaVerdict{Actionable: false, Focus: "", Reason: AgendaClassifierError}
}

/* ───────── 收集(agenda.ts 的 DB 面) ───────── */

const (
	calendarLookaheadMS  = 30 * 60_000
	calendarLookbehindMS = 15 * 60_000
)

func envIntOr(name string, def int64) int64 {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def
	}
	n := int64(0)
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int64(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

// 停摆窗与 nudge 冷却:env 可覆盖(fallback 用短冷却,理由见 claimStallNudge)。
func stallMinMS() int64 { return envIntOr("CUMORA_STALL_MIN_MS", 5*60_000) }
func stallMaxMS() int64 { return envIntOr("CUMORA_STALL_MAX_MS", 6*60*60_000) }
func nudgeCooldownMS() int64 {
	return envIntOr("CUMORA_NUDGE_COOLDOWN_MS", 45*60_000)
}
func nudgeCooldownFallbackMS() int64 {
	return envIntOr("CUMORA_NUDGE_COOLDOWN_FALLBACK_MS", 5*60_000)
}

// ClaimStallNudge:认领先前停摆会话的 nudge 权。首个调用者(全体成员
// agent 间)拿到 true;其余人——含同 agent 后续心跳——冷期内 false。
// fallback 来源用短 TTL:被唤醒的大脑若婉拒,45min 锁不应永久封死其他
// 成员;另配 declines 计数(≥3 次无后续发帖即停)防 5min 重烧 token。
func (s *Service) ClaimStallNudge(conversationID string, source string) bool {
	rdb := s.redis()
	if rdb == nil {
		return false
	}
	declineKey := "cumora:nudge-declines:" + conversationID
	if source == "fallback" {
		if v, err := rdb.Get(ctxBG, declineKey).Result(); err == nil {
			if n := parseInt(v); n >= 3 {
				return false
			}
		}
	}
	key := "cumora:nudge:" + conversationID
	cooldown := nudgeCooldownMS()
	if source == "fallback" {
		cooldown = nudgeCooldownFallbackMS()
	}
	ttl := (cooldown + 999) / 1000
	ok, err := rdb.SetNX(ctxBG, key, "1", time.Duration(ttl)*time.Second).Result()
	if err != nil {
		return false
	}
	if ok && source == "fallback" {
		pipe := rdb.Pipeline()
		pipe.Incr(ctxBG, declineKey)
		pipe.Expire(ctxBG, declineKey, time.Duration((nudgeCooldownMS()+999)/1000)*time.Second)
		_, _ = pipe.Exec(ctxBG)
	}
	return ok
}

func parseInt(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// GatherAgentAgenda:公开入口。空结果 = 该 agent 盘上无事,调用方回退
// 通用 idle 行为。stalls 查询失败按空处理(不拖垮卡/事件)。
func (s *Service) GatherAgentAgenda(ctx context.Context, agentID, companyID string) (AgentAgenda, error) {
	agenda := AgentAgenda{Cards: []AgendaCard{}, Events: []AgendaEvent{}, Stalls: []StalledConvo{}}
	cards, err := s.loadAssignedCards(ctx, agentID, companyID)
	if err != nil {
		return agenda, err
	}
	events, err := s.loadDueEvents(ctx, agentID, companyID)
	if err != nil {
		return agenda, err
	}
	stalls, err := s.loadStalledConversations(ctx, agentID, companyID)
	if err != nil {
		slog.Warn("[agenda] loadStalledConversations failed", "err", err)
		stalls = nil
	}
	agenda.Cards = cards
	agenda.Events = events
	agenda.Stalls = stalls
	return agenda, nil
}

// loadAssignedCards:非 done 列里指派给/点名该 agent 的卡。紧上限——
// 绝不喂分类器一面墙;30 天未动的卡视作弃置忽略。
func (s *Service) loadAssignedCards(ctx context.Context, agentID, companyID string) ([]AgendaCard, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id, c.board_id, b.title AS board_title,
		       c.column_id, col.title AS column_title,
		       c.title, c.description, c.assignee_id,
		       COALESCE(c.mentions, '[]'::jsonb) AS mentions,
		       c.updated_at::text AS updated_at
		  FROM board_cards c
		  JOIN boards b ON b.id = c.board_id
		  JOIN board_columns col ON col.id = c.column_id
		 WHERE b.company_id = $1
		   AND (c.assignee_id = $2 OR c.mentions @> to_jsonb($2::text))
		   AND c.updated_at > NOW() - INTERVAL '30 days'
		 ORDER BY c.updated_at DESC
		 LIMIT 20`, companyID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgendaCard{}
	for rows.Next() {
		var c AgendaCard
		var mentions []byte
		if err := rows.Scan(&c.ID, &c.BoardID, &c.BoardTitle, &c.ColumnID, &c.ColumnTitle,
			&c.Title, &c.Description, &c.AssigneeID, &mentions, &c.UpdatedAt); err != nil {
			return nil, err
		}
		if !doneColumnRe.MatchString(c.ColumnTitle) {
			c.Mentions = []string{}
			_ = jsonUnmarshal(mentions, &c.Mentions)
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// loadDueEvents:当前槽窗内的日历事件(不含 recurrence 规则语义——
// 分类器只需下一个具体时刻,真正的派发数学在日历派发器)。
func (s *Service) loadDueEvents(ctx context.Context, agentID, companyID string) ([]AgendaEvent, error) {
	now := time.Now()
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, title, description, start_at::text AS start_at,
		       agent_prompt, target_conversation_id
		  FROM calendar_events
		 WHERE company_id = $1
		   AND assignee_id = $2
		   AND status = 'active'
		   AND start_at BETWEEN $3 AND $4
		 ORDER BY start_at ASC
		 LIMIT 10`,
		companyID, agentID, now.Add(-calendarLookbehindMS*time.Millisecond), now.Add(calendarLookaheadMS*time.Millisecond))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgendaEvent{}
	for rows.Next() {
		var e AgendaEvent
		if err := rows.Scan(&e.ID, &e.Title, &e.Description, &e.StartAt, &e.AgentPrompt, &e.TargetConversationID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// loadStalledConversations:成员会话中"最后一条文本消息"落在静默窗
// [STALL_MIN_MS, STALL_MAX_MS] 内者。conversation_members 索引定位
// 会话 + LATERAL 取每会话最新消息(#137 前为 GIN + enable_seqscan=off)。
func (s *Service) loadStalledConversations(ctx context.Context, agentID, companyID string) ([]StalledConvo, error) {
	var out []StalledConvo
	rows, err := s.DB.QueryContext(ctx, `
			WITH convos AS (
			  SELECT c.id, c.kind, c.title
			    FROM conversation_members cmv
			    JOIN conversations c ON c.id = cmv.conversation_id
			   WHERE cmv.participant_id = $1 AND c.company_id = $2
			)
			SELECT co.id AS conversation_id, co.kind, co.title,
			       m.id AS last_message_id, m.author_id AS last_author_id,
			       COALESCE(p.name, m.author_id) AS last_author_name,
			       (m.author_id = $1) AS last_author_is_self,
			       LEFT(m.body, 160) AS last_body,
			       (EXTRACT(EPOCH FROM (NOW() - m.created_at)) / 60)::int::text AS minutes_silent,
			       (
			         SELECT string_agg(t.line, E'\n' ORDER BY t.created_at ASC)
			           FROM (
			             SELECT COALESCE(tp.name, tm.author_id) || ': ' || LEFT(tm.body, 100) AS line, tm.created_at
			               FROM messages tm
			               LEFT JOIN participants tp ON tp.id = tm.author_id AND tp.company_id = $2
			              WHERE tm.conversation_id = co.id AND tm.kind = 'text'
			              ORDER BY tm.created_at DESC
			              LIMIT 6
			           ) t
			       ) AS recent_tail
			  FROM convos co
			  JOIN LATERAL (
			    SELECT * FROM messages mm
			     WHERE mm.conversation_id = co.id AND mm.kind = 'text'
			     ORDER BY mm.created_at DESC
			     LIMIT 1
			  ) m ON true
			  LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = $2
			 WHERE m.created_at <= NOW() - ($3::double precision * INTERVAL '1 millisecond')
			   AND m.created_at >= NOW() - ($4::double precision * INTERVAL '1 millisecond')
			 ORDER BY m.created_at DESC
			 LIMIT 10`,
		agentID, companyID, stallMinMS(), stallMaxMS())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			st                StalledConvo
			title             sql.NullString
			minutesSilentText string
			recentTail        sql.NullString
		)
		if err := rows.Scan(&st.ConversationID, &st.Kind, &title, &st.LastMessageID, &st.LastAuthorID,
			&st.LastAuthorName, &st.LastAuthorIsSelf, &st.LastBody, &minutesSilentText, &recentTail); err != nil {
			return nil, err
		}
		if title.Valid {
			t := title.String
			st.Title = &t
		}
		st.MinutesSilent = int(parseInt(minutesSilentText))
		st.RecentTail = recentTail.String
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

/* ───────── brief 渲染 + 终判(agenda.ts server 侧) ───────── */

// RenderAgendaBrief:大脑 turn 的 backgroundBrief.body。带足特异性(id/
// 标题/槽时刻/agent_prompt)让 agent 不必重查即可行动,但保持简练。
// 开头显式覆盖默认"什么都不做"姿态——cerebellum 已判此事值得叫大脑。
func RenderAgendaBrief(agenda AgentAgenda, focus string) string {
	lines := []string{
		`This wake comes from your own agenda — Kanban cards assigned to you and Calendar slots that are due now. The system has already triaged that there is real work to progress here, so act: pick the most timely item, move it forward (write the card comment, ship the deliverable, send the DM, draft the doc), and call set_turn_status when done. Override the generic "default to doing nothing" stance for this wake.`,
		"",
	}
	if focus != "" {
		lines = append(lines, "Focus from agenda triage: "+focus, "")
	}
	if len(agenda.Cards) > 0 {
		lines = append(lines, fmt.Sprintf("Your active Kanban cards (%d):", len(agenda.Cards)))
		for _, c := range agenda.Cards {
			tag := "@you mentioned"
			if c.AssigneeID != nil {
				tag = "assigned to you"
			}
			lines = append(lines, fmt.Sprintf("- %s  [%s / %s]  %s  (%s)", c.ID, c.BoardTitle, c.ColumnTitle, c.Title, tag))
			if c.Description != nil && *c.Description != "" {
				// TS .replace(/\n/g, ' ').slice(0,200):只换行折叠(空白/制表保留),
				// UTF-16 码元截断。
				d := strings.ReplaceAll(*c.Description, "\n", " ")
				lines = append(lines, "    "+sliceUTF16(d, 200))
			}
		}
		lines = append(lines, "")
		lines = append(lines, `Inspect with: bash("cumora kanban show <board_id>") or bash("cumora card show <card_id>").`)
	}
	if len(agenda.Events) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf("Calendar events in your current slot (%d):", len(agenda.Events)))
		for _, e := range agenda.Events {
			lines = append(lines, fmt.Sprintf("- %s  at %s  %s", e.ID, e.StartAt, e.Title))
			if e.AgentPrompt != nil && *e.AgentPrompt != "" {
				lines = append(lines, "    prompt: "+sliceUTF16(*e.AgentPrompt, 240))
			}
		}
	}
	if len(agenda.Stalls) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, fmt.Sprintf(`Conversations that have gone quiet — possible follow-ups (%d). FIRST decide if each is genuinely WAITING or already DONE. If the recent messages show the activity CONCLUDED or the thread socially closed (wrap-ups, closing / acknowledgement remarks, a result already reached, nothing pending, no one waiting on a specific next step) → it is FINISHED: do NOT post. Reviving a finished thread with a late message is noise, not teamwork. Only act when someone is plainly waiting:`, len(agenda.Stalls)))
		for _, st := range agenda.Stalls {
			who := fmt.Sprintf("%s spoke last", st.LastAuthorName)
			if st.LastAuthorIsSelf {
				who = "YOU spoke last"
			}
			title := ""
			if st.Title != nil {
				title = *st.Title
			}
			lines = append(lines, fmt.Sprintf(`- %s [%s] "%s" — %s, %dm ago. Recent:`, st.ConversationID, st.Kind, title, who, st.MinutesSilent))
			tail := st.RecentTail
			if tail == "" {
				tail = st.LastBody
			}
			for _, ln := range strings.Split(tail, "\n") {
				lines = append(lines, "    "+ln)
			}
		}
		lines = append(lines, `If (and ONLY if) one is genuinely unfinished and someone is waiting: read the room (`+"`cumora messages <id>`"+`), then send AT MOST ONE message — answer it if it needs your answer, or a single brief nudge. Never nag, never pile on, never revive a finished thread.`)
	}
	return strings.Join(lines, "\n")
}

// AgendaOutcome:finalizeAgendaVerdict 的产物(/agenda 与 /agenda/verdict
// 同一尾部,保证同一 verdict 两路返回字节同形)。
type AgendaOutcome struct {
	Actionable bool    `json:"actionable"`
	Focus      *string `json:"focus,omitempty"`
	Brief      *string `json:"brief,omitempty"`
	Cards      int     `json:"cards"`
	Events     int     `json:"events"`
	Stalls     int     `json:"stalls"`
	Reason     *string `json:"reason,omitempty"`
}

// FinalizeAgendaVerdict:cerebellum 说 yes 后逐 stall 认领(恰好一个
// agent 去 nudge、同一停摆态绝不两次),只留本 agent 赢到的 stall;
// 卡/事件 per-agent 无竞态直通。分类器错误来源按 fallback 短冷却。
// 停摆是唯一动因且全被别人认走 → 不值得叫大脑。
func (s *Service) FinalizeAgendaVerdict(ctx context.Context, agenda AgentAgenda, verdict AgendaVerdict) AgendaOutcome {
	if !verdict.Actionable {
		return AgendaOutcome{
			Actionable: false,
			Cards:      len(agenda.Cards),
			Events:     len(agenda.Events),
			Stalls:     len(agenda.Stalls),
			Reason:     &verdict.Reason,
		}
	}
	nudgeSource := "classified"
	if strings.HasPrefix(verdict.Reason, AgendaClassifierError) {
		nudgeSource = "fallback"
	}
	var won []StalledConvo
	for _, st := range agenda.Stalls {
		if s.ClaimStallNudge(st.ConversationID, nudgeSource) {
			won = append(won, st)
		}
	}
	final := AgentAgenda{Cards: agenda.Cards, Events: agenda.Events, Stalls: won}
	if len(final.Cards) == 0 && len(final.Events) == 0 && len(final.Stalls) == 0 {
		reason := "stall already claimed by another agent"
		return AgendaOutcome{Actionable: false, Cards: 0, Events: 0, Stalls: 0, Reason: &reason}
	}
	brief := RenderAgendaBrief(final, verdict.Focus)
	return AgendaOutcome{
		Actionable: true,
		Focus:      &verdict.Focus,
		Brief:      &brief,
		Cards:      len(final.Cards),
		Events:     len(final.Events),
		Stalls:     len(final.Stalls),
	}
}

/* ───────── cerebellum 路由(cerebellum-route.ts + settings 读侧) ───────── */

// CerebellumSettings:app_settings 里的五个明文域(#22:DB 读,管理端
// 改动下次调用即生效,无需重启)。
type CerebellumSettings struct {
	Route       string // 'remote' | 'byoa'
	LocalEngine string
	Provider    string
	BaseURL     string
	Model       string
}

func (s *Service) GetCerebellumSettings(ctx context.Context) CerebellumSettings {
	def := CerebellumSettings{Route: "remote", LocalEngine: "claude", Provider: "", BaseURL: "", Model: ""}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT key, value FROM app_settings WHERE key = ANY($1::text[])`,
		pqArray([]string{
			"cerebellum_route", "cerebellum_local_engine", "cerebellum_provider",
			"cerebellum_base_url", "cerebellum_model",
		}))
	if err != nil {
		return def
	}
	defer rows.Close()
	vals := map[string]string{}
	for rows.Next() {
		var k string
		var v []byte
		if rows.Scan(&k, &v) != nil {
			continue
		}
		var str string
		if jsonUnmarshal(v, &str) == nil {
			vals[k] = str
		}
	}
	out := def
	if vals["cerebellum_route"] == "byoa" || vals["cerebellum_route"] == "remote" {
		out.Route = vals["cerebellum_route"]
	}
	if vals["cerebellum_local_engine"] != "" {
		out.LocalEngine = vals["cerebellum_local_engine"]
	}
	out.Provider = vals["cerebellum_provider"]
	out.BaseURL = strings.TrimRight(vals["cerebellum_base_url"], "/")
	out.Model = vals["cerebellum_model"]
	return out
}

// ResolveCerebellumRouteForAgent:部署默认(byoa?)→ 该 agent 的 Computer
// 在线且宣告 localEngine 才落 byoa;其余一律 remote——暂时不可用的本地
// 引擎不得无声饿死 agent 的 cerebellum 调用。
func (s *Service) ResolveCerebellumRouteForAgent(ctx context.Context, agentID string) string {
	settings := s.GetCerebellumSettings(ctx)
	if settings.Route != "byoa" {
		return "remote"
	}
	var status string
	var engines []byte
	err := s.DB.QueryRowContext(ctx, `
		SELECT c.status, c.available_engines
		  FROM participants p
		  JOIN computers c ON c.id = p.computer_id
		 WHERE p.id = $1 AND p.kind = 'agent' AND c.revoked_at IS NULL
		 LIMIT 1`, agentID).Scan(&status, &engines)
	if err != nil {
		return "remote"
	}
	if status != "online" {
		return "remote"
	}
	var engineList []string
	_ = jsonUnmarshal(engines, &engineList)
	for _, e := range engineList {
		if e == settings.LocalEngine {
			return "byoa"
		}
	}
	return "remote"
}

/* ───────── remote 分类(classifyAgendaActionable,#89) ───────── */

// CerebellumApiKeyPlaintext:app_settings.cerebellum_api_key 的 AES-256-GCM
// 解密("iv.tag.ciphertext" 各 base64;主键 = sha256(CUMORA_SECRETS_KEY))。
// 任何失败(缺主键/主键轮换/坏数据)都返回 "" —— 按 ADR 0001,丢失主键
// 只是让配置"看起来未设置",绝不抛错。
func (s *Service) CerebellumApiKeyPlaintext(ctx context.Context) string {
	master := os.Getenv("CUMORA_SECRETS_KEY")
	if master == "" {
		return ""
	}
	var stored []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key = 'cerebellum_api_key' LIMIT 1`).Scan(&stored)
	if err != nil {
		return ""
	}
	var enc string
	if jsonUnmarshal(stored, &enc) != nil || enc == "" {
		return ""
	}
	parts := strings.Split(enc, ".")
	if len(parts) != 3 {
		return ""
	}
	iv, err1 := base64.StdEncoding.DecodeString(parts[0])
	tag, err2 := base64.StdEncoding.DecodeString(parts[1])
	ct, err3 := base64.StdEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	key := sha256.Sum256([]byte(master))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return ""
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return ""
	}
	if len(tag) != gcm.Overhead() {
		return ""
	}
	plain, err := gcm.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return ""
	}
	return string(plain)
}

// cerebellumRemoteConfigured:适配器可达的最小配置(baseUrl + apiKey)。
func (s *Service) cerebellumRemoteConfigured(ctx context.Context) (baseURL, apiKey string, ok bool) {
	settings := s.GetCerebellumSettings(ctx)
	apiKey = s.CerebellumApiKeyPlaintext(ctx)
	return settings.BaseURL, apiKey, settings.BaseURL != "" && apiKey != ""
}

var agendaFenceOpen = regexp.MustCompile(`(?i)^` + "```" + `(?:json)?\s*`)
var agendaFenceClose = regexp.MustCompile("(?i)\\s*```$")
var agendaActionableSalvage = regexp.MustCompile(`(?i)["']?actionable["']?\s*:\s*(true|false|1|0|["']true["']|["']false["'])`)

// agendaSalvageStringField:JSON.parse 失败后的逐字段抢救(同 TS;RE2 无
// 反向引用,用双/单引号交替分支表达 "开头引号=结尾引号")。
func agendaSalvageStringField(candidate, name string) string {
	head := `(?i)["']?` + regexp.QuoteMeta(name) + `["']?\s*:\s*`
	re := regexp.MustCompile(head + `"([^"]*)"|` + head + `'([^']*)'`)
	m := re.FindStringSubmatch(candidate)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// AgendaParsedVerdict:parseAgendaVerdict 的中间形。
type AgendaParsedVerdict struct {
	Actionable bool
	Focus      string
	Reason     string
}

// ParseAgendaVerdict:剥 ```json 围栏 → 首尾大括号截取 → JSON.parse;
// 失败退保守字段抢救。返回 nil = 无可恢复判定。
func ParseAgendaVerdict(raw string) *AgendaParsedVerdict {
	unfenced := agendaFenceClose.ReplaceAllString(agendaFenceOpen.ReplaceAllString(strings.TrimSpace(raw), ""), "")
	first := strings.Index(unfenced, "{")
	if first < 0 {
		return nil
	}
	candidate := unfenced[first:]
	if last := strings.LastIndex(unfenced, "}"); last > first {
		candidate = unfenced[first : last+1]
	}
	var parsed struct {
		Actionable any `json:"actionable"`
		Focus      any `json:"focus"`
		Reason     any `json:"reason"`
	}
	if err := jsonUnmarshal([]byte(candidate), &parsed); err == nil {
		return coerceAgendaVerdictAny(parsed.Actionable, parsed.Focus, parsed.Reason)
	}
	m := agendaActionableSalvage.FindStringSubmatch(candidate)
	if m == nil {
		return nil
	}
	token := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(m[1], `"`, ""), `'`, ""))
	actionable := token == "true" || token == "1"
	focus := agendaSalvageStringField(candidate, "focus")
	reason := agendaSalvageStringField(candidate, "reason")
	if actionable && strings.TrimSpace(focus) == "" {
		return &AgendaParsedVerdict{Actionable: false, Focus: "", Reason: "malformed positive verdict without focus"}
	}
	return &AgendaParsedVerdict{Actionable: actionable, Focus: focus, Reason: reason}
}

// coerceAgendaVerdictAny:JSON 成功路径的收窄 —— true/"true"/1 才算
// actionable(模型答 "no" 之类的自然语言一律视为否),focus/reason 钳 240。
func coerceAgendaVerdictAny(a, focus, reason any) *AgendaParsedVerdict {
	actionable := a == true || a == "true" || a == float64(1)
	return &AgendaParsedVerdict{Actionable: actionable, Focus: jsStringClamp(focus, 240), Reason: jsStringClamp(reason, 240)}
}

// jsStringClamp:JS String(v ?? ”) 语义 —— 数值/布尔转字符串,数组按
// JS 逗号连接,统一 UTF-16 钳长。
func jsStringClamp(v any, max int) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return sliceUTF16(t, max)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if e == nil {
				parts = append(parts, "")
				continue
			}
			parts = append(parts, jsStringClamp(e, 1<<30))
		}
		return sliceUTF16(strings.Join(parts, ","), max)
	case float64:
		// JS Number→String:整数值不带小数点;Go %v 同形。
		return sliceUTF16(strconv.FormatFloat(t, 'g', -1, 64), max)
	case bool:
		return sliceUTF16(strconv.FormatBool(t), max)
	default:
		return sliceUTF16(fmt.Sprint(t), max)
	}
}

// ClassifyAgendaActionable:remote 路由的云分类 —— 通用 cerebellum 适配器
// (任意 Chat-Completions 兼容供应商)或 legacy tracked OpenAI 客户端;
// 任何失败退确定性回退,分类器断供绝不烧脑调用。
func (s *Service) ClassifyAgendaActionable(ctx context.Context, persona *Persona, companyID, agentID string, agenda AgentAgenda, nowMS int64) AgendaVerdict {
	built := BuildAgendaClassifierRequest(persona, agenda, nowMS)
	if built.Verdict != nil {
		return *built.Verdict
	}
	if agentID != "" && s.ResolveCerebellumRouteForAgent(ctx, agentID) == "byoa" {
		slog.Warn("[agenda] classifyAgendaActionable called for byoa-routed agent " + agentID + " — caller should use the /runtime/agenda daemon-poll path instead of the remote classifier")
		return AgendaVerdict{Actionable: false, Focus: "", Reason: AgendaClassifierError}
	}
	adapterBase, adapterKey, useAdapter := s.cerebellumRemoteConfigured(ctx)
	settings := s.GetCerebellumSettings(ctx)
	model := supportModelEnv()
	if useAdapter && settings.Model != "" {
		model = settings.Model
	}
	args := cliResponsesArgs{
		Model:           model,
		Instructions:    built.Instructions,
		Input:           built.Input,
		JSONMode:        true,
		MaxOutputTokens: 2000,
		ReasoningEffort: "minimal",
	}
	t0 := time.Now()
	var outputText string
	var usage *costing.TokenUsage
	var err error
	if useAdapter {
		// 适配器路径不走 llm 台账(TS ponytail 备注);Chat-Completions
		// 翻译与 novita 分支同构。
		outputText, usage, err = s.cerebellumResponsesCreate(ctx, adapterBase, adapterKey, args)
	} else {
		agentArg, tenantArg := agentID, companyID
		record := func(status string, errMsg *string) {
			obs.RecordLlmCall(s.DB, obs.LlmCallRecord{
				Purpose: "agenda", CompanyID: &tenantArg, AgentID: &agentArg, Source: "cloud",
				Model: model, Usage: usage, LatencyMS: msSince(t0), Status: status, Error: errMsg,
				Extras: map[string]any{
					"cards":   len(agenda.Cards),
					"events":  len(agenda.Events),
					"stalls":  len(agenda.Stalls),
					"persona": persona.Name,
				},
			})
		}
		var res cliResponsesResult
		res, err = s.cliResponsesCreate(ctx, companyID, args)
		if err != nil {
			msg := err.Error()
			record("failed", &msg)
		} else {
			usage = res.Usage
			record("ok", nil)
		}
		outputText = res.OutputText
	}
	if err == nil {
		if parsed := ParseAgendaVerdict(outputText); parsed != nil {
			return AgendaVerdict{Actionable: parsed.Actionable, Focus: parsed.Focus, Reason: parsed.Reason}
		}
		err = fmt.Errorf("agenda classifier returned no recoverable verdict")
	}
	slog.Warn("[agenda] classifier failed", "err", err.Error())
	return AgendaDeterministicFallback(agenda)
}

// cerebellumResponsesCreate:Responses 参数 → Chat-Completions 翻译
// (cerebellum-adapter.ts 的非流式分支;模型名原样透传)。
func (s *Service) cerebellumResponsesCreate(ctx context.Context, baseURL, apiKey string, args cliResponsesArgs) (string, *costing.TokenUsage, error) {
	body := map[string]any{
		"model":    args.Model,
		"messages": novitaChatMessages(args.Instructions, args.Input),
		"stream":   false,
	}
	if args.MaxOutputTokens > 0 {
		body["max_tokens"] = args.MaxOutputTokens
	}
	if args.JSONMode {
		body["response_format"] = map[string]any{"type": "json_object"}
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+apiKey)
	resp, err := httpClientLLM.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%d %s", resp.StatusCode, truncateRunesSimple(string(raw), 400))
	}
	out, err := parseNovitaChatCompletion(raw)
	if err != nil {
		return "", nil, err
	}
	return out.OutputText, out.Usage, nil
}
