// /runtime/cli 邮箱组(#89):inbox / glance / ack / mute / follow。
// 对齐 TS cli.ts 同名 cmd*;loadInbox 用 CLI 自己的 SELECT(与
// /runtime/inbox 的变体不同:无 project 列、排序仅 created_at)。
package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

/* ───────── inbox 行加载 ───────── */

type cliInboxRow struct {
	ID                string        `json:"id"`
	ConversationID    string        `json:"conversation_id"`
	ConversationTitle string        `json:"conversation_title"`
	ConversationKind  string        `json:"conversation_kind"`
	ConversationTopic *string       `json:"conversation_topic"`
	AuthorID          string        `json:"author_id"`
	AuthorName        string        `json:"author_name"`
	Body              string        `json:"body"`
	Kind              string        `json:"kind"`
	Sequence          int64         `json:"sequence"`
	CreatedAt         cliISOTime    `json:"created_at"`
	Attachment        cliAttachment `json:"attachment"`
	Poll              cliPoll       `json:"poll"`
	QuotedMessageID   *string       `json:"quoted_message_id"`
	Quoted            cliRawJSON    `json:"quoted"`
}

// quotedText:文本渲染所需的 quoted 子集(authorName 取 participants
// 名字的那个投影)。
type quotedText struct {
	ID         string `json:"id"`
	AuthorName string `json:"authorName"`
	Body       string `json:"body"`
}

func (r *cliInboxRow) quotedForRender() (quotedText, bool) {
	if r.Quoted == nil || string(r.Quoted) == "null" {
		return quotedText{}, false
	}
	var q quotedText
	if err := jsonUnmarshal(r.Quoted, &q); err != nil || q.ID == "" {
		return quotedText{}, false
	}
	return q, true
}

func (s *Service) cliLoadInbox(ctx context.Context, agentID string) ([]cliInboxRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT
        m.id,
        m.conversation_id,
        c.title AS conversation_title,
        c.kind  AS conversation_kind,
        c.topic AS conversation_topic,
        m.author_id,
        COALESCE(p.name, m.author_id) AS author_name,
        m.body,
        m.kind,
        m.sequence,
        m.created_at,
        m.attachment,
        m.poll,
        m.quoted_message_id,
        (
          SELECT jsonb_build_object(
            'id', qm.id,
            'authorId', qm.author_id,
            'authorName', COALESCE(qp.name, qm.author_id),
            'kind', qm.kind,
            'body', LEFT(qm.body, 240),
            'sequence', qm.sequence
          )
            FROM messages qm
            LEFT JOIN participants qp ON qp.id = qm.author_id AND qp.company_id = c.company_id
           WHERE qm.id = m.quoted_message_id
             AND qm.conversation_id = m.conversation_id
        ) AS quoted
       FROM messages m
       JOIN conversations c ON c.id = m.conversation_id
       LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = c.company_id
      WHERE c.members @> to_jsonb(ARRAY[$1::text])
        AND m.author_id <> $1
        AND m.created_at > COALESCE(
          (SELECT last_read_at FROM conversation_reads
            WHERE user_id = $1 AND conversation_id = c.id),
          '1970-01-01T00:00:00Z'::timestamptz)
        AND (
          c.kind = 'direct'
          OR NOT EXISTS (
            SELECT 1 FROM conversation_mutes mu
             WHERE mu.user_id = $1 AND mu.conversation_id = c.id
               AND (mu.muted_until IS NULL OR mu.muted_until > NOW())
          )
          OR EXISTS (
            SELECT 1 FROM regexp_matches(m.body, '@([[:alnum:]_-]+)', 'g') mention
             WHERE LOWER(mention[1]) = LOWER($1)
          )
          OR EXISTS (
            SELECT 1 FROM messages quoted
             WHERE quoted.id = m.quoted_message_id
               AND quoted.conversation_id = m.conversation_id
               AND quoted.author_id = $1
          )
        )
      ORDER BY m.created_at ASC
      LIMIT 200`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cliInboxRow
	for rows.Next() {
		var r cliInboxRow
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.ConversationTitle, &r.ConversationKind,
			&r.ConversationTopic, &r.AuthorID, &r.AuthorName, &r.Body, &r.Kind, &r.Sequence,
			&r.CreatedAt, &r.Attachment, &r.Poll, &r.QuotedMessageID, &r.Quoted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

/* ───────── inbox ───────── */

func (s *Service) cliCmdInbox(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	filterConvo := ""
	if len(parsed.positional) > 0 {
		filterConvo = parsed.positional[0]
	}
	items, err := s.cliLoadInbox(ctx, me)
	if err != nil {
		return cliErrThrow(err)
	}
	filtered := items
	if filterConvo != "" {
		filtered = nil
		for _, it := range items {
			if it.ConversationID == filterConvo {
				filtered = append(filtered, it)
			}
		}
	}
	if parsed.flagTruey("json") {
		if filtered == nil {
			filtered = []cliInboxRow{}
		}
		js, e := cliJSONList(filtered)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	if len(filtered) == 0 {
		return cliOK("(inbox empty for " + me + ")")
	}
	// 按会话分组(保持出现顺序)
	type convoGroup struct {
		id   string
		msgs []cliInboxRow
	}
	var order []string
	byConvo := map[string]*convoGroup{}
	for _, it := range filtered {
		g, ok := byConvo[it.ConversationID]
		if !ok {
			g = &convoGroup{id: it.ConversationID}
			byConvo[it.ConversationID] = g
			order = append(order, it.ConversationID)
		}
		g.msgs = append(g.msgs, it)
	}
	lines := []string{fmt.Sprintf("%d unread message(s) for %s, across %d conversation(s):", len(filtered), me, len(order)), ""}
	for _, convoID := range order {
		msgs := byConvo[convoID].msgs
		head := msgs[0]
		lines = append(lines, "# "+convoID+"  ["+head.ConversationKind+"]  \""+head.ConversationTitle+"\"")
		if head.ConversationTopic != nil {
			lines = append(lines, "  Topic: "+*head.ConversationTopic)
		}
		for _, m := range msgs {
			t := nodeHM(time.Time(m.CreatedAt))
			body := strings.ReplaceAll(utf16Slice(m.Body, 240), "\n", " \\n ")
			if m.Kind == "tool" {
				body = "[tool call]"
			}
			lines = append(lines, "  ["+m.ID+"]  "+t+"  "+m.AuthorName+": "+body)
			// 引文内联(单行缩进):不查第二次就能看见回复在回复什么。
			if m.QuotedMessageID != nil {
				if q, ok := m.quotedForRender(); ok {
					qBody := strings.ReplaceAll(utf16Slice(q.Body, 180), "\n", " \\n ")
					lines = append(lines, "    ↩ quoting ["+q.ID+"] "+q.AuthorName+": "+qBody)
				} else {
					lines = append(lines, "    ↩ quoting ["+*m.QuotedMessageID+"] (original deleted)")
				}
			}
			if m.Kind == "poll" && m.Poll.present {
				lines = append(lines, renderPollBlock(m.ID, m.Poll.parsed)...)
			}
			if att := renderAttachment(m.Attachment); att != "" {
				lines = append(lines, att)
			}
		}
		lines = append(lines, "")
	}
	lines = append(lines, "when you're done deciding what to do (reply / react / dm / nothing), run `cumora ack <convo_id>` to clear that conversation from your inbox so the next wake-up doesn't see it again. `cumora ack --all` clears everything in your inbox.")
	return cliOK(strings.Join(lines, "\n"))
}

/* ───────── glance ───────── */

func (s *Service) cliCmdGlance(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	convoID := parsed.flagStrOr("conversation", "")
	if convoID == "" && len(parsed.positional) > 0 {
		convoID = parsed.positional[0]
	}
	if convoID == "" {
		return cliErr("usage: glance --conversation <id>  (or: glance <id>)")
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT m.id, m.author_id, COALESCE(p.name, m.author_id) AS author_name,
		        m.kind, m.body, m.created_at, m.sequence
		   FROM messages m
		   JOIN conversations c ON c.id = m.conversation_id
		   LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = c.company_id
		  WHERE m.conversation_id = $1
		  ORDER BY m.created_at DESC
		  LIMIT 12`, convoID)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type recentRow struct {
		ID         string     `json:"id"`
		AuthorID   string     `json:"author_id"`
		AuthorName string     `json:"author_name"`
		Kind       string     `json:"kind"`
		Body       string     `json:"body"`
		CreatedAt  cliISOTime `json:"created_at"`
		Sequence   int64      `json:"sequence"`
	}
	var listed []recentRow
	for rows.Next() {
		var r recentRow
		if err := rows.Scan(&r.ID, &r.AuthorID, &r.AuthorName, &r.Kind, &r.Body, &r.CreatedAt, &r.Sequence); err != nil {
			return cliErrThrow(err)
		}
		listed = append(listed, r)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	recent := make([]recentRow, len(listed))
	for i, r := range listed {
		recent[len(listed)-1-i] = r
	}
	// 推进 Redis seen 边界:glance 刚把这些消息展示给 agent。与 messages
	// 同一条 Redis-only、失败开放路径,不动 conversation_reads。
	if len(recent) > 0 {
		s.RecordSeen(me, convoID, recent[len(recent)-1].Sequence)
	}
	// 不暴露"谁在写"名册:按序位对号入座(“我第 3 个 claim→我发第 3
	// 条”)的协同缺陷由此结构性消失——agent 能依据的唯一事实是"实际发
	// 出的最新消息",碰撞由 reply 的新鲜度门串行化。
	if parsed.flagTruey("json") {
		js, e := cliJSONStringify(map[string]any{
			"conversation_id": convoID,
			"recent":          recent,
		})
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	lines := []string{fmt.Sprintf("Glance into %s — last %d message(s):", convoID, len(recent)), ""}
	if len(recent) == 0 {
		lines = append(lines, "  (no messages yet)")
	} else {
		for _, m := range recent {
			t := nodeHM(time.Time(m.CreatedAt))
			tag := "   "
			if m.AuthorID == me {
				tag = "▸ME"
			}
			body := strings.ReplaceAll(utf16Slice(m.Body, 200), "\n", " \\n ")
			switch m.Kind {
			case "tool":
				body = "[tool call]"
			case "system":
				body = "[system]"
			}
			lines = append(lines, "  ["+m.ID+"] "+tag+" "+t+"  "+m.AuthorName+": "+body)
		}
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ───────── ack ───────── */

func (s *Service) cliCmdAck(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	ackOne := func(convoID string) error {
		_, err := s.DB.ExecContext(ctx,
			`INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
		     VALUES ($1, $2, NOW())
		     ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`,
			me, convoID)
		// ack = 对该会话收兵。任何未用的 HELD 确认作废——不得武装后续
		// turn 的抢先 --send-anyway(2026-07-08 的陈旧 "6" 路径)。
		s.ClearHold(me, "reply:"+convoID)
		return err
	}
	if parsed.flagTruey("all") {
		items, err := s.cliLoadInbox(ctx, me)
		if err != nil {
			return cliErrThrow(err)
		}
		seen := map[string]bool{}
		var convoIDs []string
		for _, it := range items {
			if !seen[it.ConversationID] {
				seen[it.ConversationID] = true
				convoIDs = append(convoIDs, it.ConversationID)
			}
		}
		for _, id := range convoIDs {
			if err := ackOne(id); err != nil {
				return cliErrThrow(err)
			}
		}
		return cliOK(fmt.Sprintf("acked %d conversation(s)", len(convoIDs)))
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: ack <conversation_id>  OR  ack --all")
	}
	convoID := parsed.positional[0]
	if err := ackOne(convoID); err != nil {
		return cliErrThrow(err)
	}
	return cliOK("acked " + convoID)
}

/* ───────── mute / follow ───────── */

var muteForRe = regexp.MustCompile(`(?i)^(\d+)(m|h|d|w)$`)

// parseMuteUntil:--until(ISO)或 --for(30m/2h/1d/1w,1 分钟–90 天)。
// 语义错误走 err() 出口(exit 1),与 TS catch 分支一致。
func parseMuteUntil(parsed cliParsed, now time.Time) (time.Time, bool, string) {
	untilRaw, hasUntil := parsed.flagStr("until")
	forRaw, hasFor := parsed.flagStr("for")
	if hasUntil && hasFor && untilRaw != "" && forRaw != "" {
		return time.Time{}, false, "use either --until or --for, not both"
	}
	if hasUntil && untilRaw != "" {
		until, ok := parseJSDate(untilRaw)
		if !ok {
			return time.Time{}, false, "invalid --until timestamp"
		}
		if !until.After(now) {
			return time.Time{}, false, "--until must be in the future"
		}
		return until, true, ""
	}
	if !hasFor || forRaw == "" {
		return time.Time{}, false, ""
	}
	m := muteForRe.FindStringSubmatch(strings.TrimSpace(forRaw))
	if m == nil {
		return time.Time{}, false, "invalid --for duration (use e.g. 30m, 2h, 1d, or 1w)"
	}
	amount, _ := strconv.Atoi(m[1])
	unitMs := map[string]int64{"m": 60_000, "h": 3_600_000, "d": 86_400_000, "w": 604_800_000}[strings.ToLower(m[2])]
	if amount < 1 || int64(amount)*unitMs > 90*86_400_000 {
		return time.Time{}, false, "--for duration must be between 1 minute and 90 days"
	}
	return now.Add(time.Duration(int64(amount)*unitMs) * time.Millisecond), true, ""
}

// parseJSDate:JS `new Date(s)` 的常用子集(ISO 8601 两种、日期、
// 日期+空格时间、时间无秒)。时区缺省按本地时区(JS 同)。
func parseJSDate(s string) (time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04",
		"2006-01-02", "2006-01-02T15:04:05.999999999",
		// PG timestamptz ::text 形态("... 05:05:09.123+00" / 无毫秒变体)
		"2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05-07:00",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (s *Service) cliCmdMute(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErrThrow(err)
	}
	if companyID == "" {
		return cliErr("unknown agent " + me + " (no company)")
	}
	if len(parsed.positional) > 0 && parsed.positional[0] == "list" {
		rows, err := s.DB.QueryContext(ctx,
			`SELECT c.id, c.title, mu.muted_until
		       FROM conversation_mutes mu
		       JOIN conversations c ON c.id = mu.conversation_id
		      WHERE mu.user_id = $1 AND c.company_id = $2
		        AND (mu.muted_until IS NULL OR mu.muted_until > NOW())
		      ORDER BY mu.muted_at DESC`, me, companyID)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type row struct {
			ID         string     `json:"id"`
			Title      string     `json:"title"`
			MutedUntil *time.Time `json:"muted_until"`
		}
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.ID, &r.Title, &r.MutedUntil); err != nil {
				return cliErrThrow(err)
			}
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			type jsonRow struct {
				ID         string     `json:"id"`
				Title      string     `json:"title"`
				MutedUntil *cliISOTime `json:"muted_until"`
			}
			var jr []jsonRow
			for _, r := range all {
				e := jsonRow{ID: r.ID, Title: r.Title}
				if r.MutedUntil != nil {
					t := cliISOTime(*r.MutedUntil)
					e.MutedUntil = &t
				}
				jr = append(jr, e)
			}
			js, e := cliJSONList(jr)
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		if len(all) == 0 {
			return cliOK("(no muted groups)")
		}
		var lines []string
		for _, r := range all {
			expiry := "until you follow it"
			if r.MutedUntil != nil {
				expiry = "until " + isoMilli(*r.MutedUntil)
			}
			lines = append(lines, "• "+r.ID+"  \""+r.Title+"\"  — "+expiry)
		}
		return cliOK(strings.Join(lines, "\n"))
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: mute <conversation_id> [--for 30m|2h|1d|1w] [--until <iso>]  OR  mute list")
	}
	conversationID := parsed.positional[0]
	until, hasUntil, msg := parseMuteUntil(parsed, time.Now())
	if msg != "" {
		return cliErr(msg)
	}
	var kind, title string
	var members cliStrArr
	err = s.DB.QueryRowContext(ctx,
		`SELECT kind, title, members FROM conversations WHERE id = $1 AND company_id = $2`,
		conversationID, companyID).Scan(&kind, &title, &members)
	if err == sql.ErrNoRows {
		return cliErr("conversation not found: " + conversationID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if !containsString(members, me) {
		return cliErr("you are not a member of " + conversationID)
	}
	if kind == "direct" {
		return cliErr("direct conversations always deliver; mute a group instead")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return cliErrThrow(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversation_mutes (user_id, conversation_id, muted_at, muted_until)
	     VALUES ($1, $2, NOW(), $3)
	     ON CONFLICT (user_id, conversation_id)
	     DO UPDATE SET muted_at = NOW(), muted_until = EXCLUDED.muted_until`,
		me, conversationID, sqlUTCTime(until, hasUntil)); err != nil {
		return cliErrThrow(err)
	}
	// 静音是明确的收兵:封住当前未读尾巴,follow 恢复后不回放积压。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
	     VALUES ($1, $2, NOW())
	     ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`,
		me, conversationID); err != nil {
		return cliErrThrow(err)
	}
	if err := tx.Commit(); err != nil {
		return cliErrThrow(err)
	}
	s.ClearHold(me, "reply:"+conversationID)
	expiry := " until you follow it again"
	if hasUntil {
		expiry = " until " + isoMilli(until)
	}
	return cliOK("Muted " + conversationID + " (\"" + title + "\")" + expiry + ". " +
		"New group messages will not wake you or enter your inbox. A direct @" + me +
		" mention or a reply quoting your message still gets through. " +
		"Resume with: cumora follow " + conversationID)
}

// isoMilli:JS Date.toISOString()(UTC, 毫秒 3 位)。
func isoMilli(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// sqlUTCTime:可空时间参数。
func sqlUTCTime(t time.Time, has bool) any {
	if !has {
		return nil
	}
	return t
}

func containsString(list cliStrArr, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (s *Service) cliCmdFollow(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: follow <conversation_id>")
	}
	conversationID := parsed.positional[0]
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErrThrow(err)
	}
	if companyID == "" {
		return cliErr("unknown agent " + me + " (no company)")
	}
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM conversation_mutes mu USING conversations c
		  WHERE mu.user_id = $1 AND mu.conversation_id = $2
		    AND c.id = mu.conversation_id AND c.company_id = $3`,
		me, conversationID, companyID)
	if err != nil {
		return cliErrThrow(err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return cliOK("Following " + conversationID + " again. New messages will resume normal inbox delivery.")
	}
	return cliOK(conversationID + " was not muted; normal delivery is already active.")
}
