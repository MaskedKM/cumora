// /runtime/cli calendar(#89):list / create / update / run-now /
// dispatches / cancel / delete。dispatch 引擎复用 domains/calendar 的
// DispatchEvent(与 REST 同源)。
package runtime

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	domcalendar "github.com/MaskedKM/cumora/apps/server-go/internal/domains/calendar"
)

func cliCalendarVisibilityClause(meIdx int) string {
	return fmt.Sprintf(`(is_private = false OR created_by = $%d OR assignee_id = $%d)`, meIdx, meIdx)
}

func (s *Service) publishCalendarCli(companyID, kind, eventID, actorID string) {
	payload, err := jsonMarshalOrdered(map[string]any{
		"type":    "calendar.changed",
		"companyId": companyID,
		"kind":    kind,
		"eventId": eventID,
		"actorId": actorID,
	})
	if err == nil {
		_ = s.publishRaw("cumora:calendar.events", payload)
	}
}

// buildRecurrenceJSON:TS 字面量 {freq, interval, byweekday?, until, count}
// 的键序手拼(byweekday 未传时整键省略 —— JSON.stringify 丢 undefined)。
func buildRecurrenceJSON(freq string, interval int, byweekday []int, until string, count *int) string {
	var sb strings.Builder
	sb.WriteString(`{"freq":` + string(cliJSONString(freq)) + `,"interval":` + strconv.Itoa(interval))
	if byweekday != nil {
		parts := make([]string, len(byweekday))
		for i, n := range byweekday {
			parts[i] = strconv.Itoa(n)
		}
		sb.WriteString(`,"byweekday":[` + strings.Join(parts, ",") + `]`)
	}
	untilJSON := "null"
	if until != "" {
		untilJSON = string(cliJSONString(until))
	}
	sb.WriteString(`,"until":` + untilJSON)
	if count != nil {
		sb.WriteString(`,"count":` + strconv.Itoa(*count))
	} else {
		sb.WriteString(`,"count":null`)
	}
	sb.WriteString(`}`)
	return sb.String()
}

// cliParseRecurrenceFlags:--every/--interval/--byweekday/--until/--count 的
// 公共解析;返回 (json, errMsg)。
func cliParseRecurrenceFlags(parsed cliParsed) (string, string) {
	freq := fmt.Sprint(parsed.flags["every"])
	switch freq {
	case "daily", "weekly", "monthly", "yearly":
	default:
		if freq == "true" {
			freq = ""
		}
		return "", "--every must be daily|weekly|monthly|yearly (got: " + freq + ")"
	}
	interval := 1
	if v, ok := parsed.flags["interval"]; ok {
		n, valid := jsFloorNumber(v)
		if valid && n != 0 {
			interval = maxInt(1, n)
		}
	}
	var byweekday []int
	if v, ok := parsed.flagStr("byweekday"); ok {
		for _, part := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err == nil && n >= 0 && n <= 6 {
				byweekday = append(byweekday, n)
			}
		}
	}
	until := parsed.flagStrOr("until", "")
	var count *int
	if v, ok := parsed.flags["count"]; ok {
		if n, valid := jsFloorNumber(v); valid {
			count = &n
		}
	}
	return buildRecurrenceJSON(freq, interval, byweekday, until, count), ""
}

// jsFloorNumber:JS Math.floor(Number(v)) 语义;不可解析返回 valid=false。
func jsFloorNumber(v any) (int, bool) {
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		return int(floorJS(f)), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func floorJS(f float64) float64 {
	if f < 0 {
		return -float64(int(-f))
	}
	return float64(int(f))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Service) cliCmdCalendar(ctx context.Context, parsed cliParsed) cliResult {
	op := "list"
	if len(parsed.positional) > 0 && parsed.positional[0] != "" {
		op = parsed.positional[0]
	}
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
	switch op {
	case "list":
		return s.cliCalendarList(ctx, parsed, me, companyID)
	case "create":
		return s.cliCalendarCreate(ctx, parsed, me, companyID)
	case "update", "edit":
		return s.cliCalendarUpdate(ctx, parsed, op, me, companyID)
	case "run-now":
		return s.cliCalendarRunNow(ctx, parsed, me, companyID)
	case "dispatches":
		return s.cliCalendarDispatches(ctx, parsed, companyID)
	case "cancel", "delete":
		return s.cliCalendarCancelDelete(ctx, parsed, op, me, companyID)
	}
	return cliErr("usage: calendar <list|create|update|run-now|dispatches|cancel|delete> [...]")
}

func (s *Service) cliCalendarList(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	all := parsed.flagTruey("all")
	args := []any{companyID, me}
	where := `company_id = $1`
	if all {
		// --all 扩到全工作区,但私有行仍按可见性隐藏;窄路径天然自我过滤。
		where += ` AND ` + cliCalendarVisibilityClause(2)
	} else {
		where += ` AND (assignee_id = $2 OR created_by = $2)`
	}
	if v, ok := parsed.flags["status"]; ok {
		args = append(args, fmt.Sprint(v))
		where += fmt.Sprintf(` AND status = $%d`, len(args))
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, title, kind, status, assignee_id, start_at, recurrence,
		        target_conversation_id, is_private
		   FROM calendar_events WHERE `+where+`
		   ORDER BY start_at ASC LIMIT 200`, args...)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID                  string     `json:"id"`
		Title               string     `json:"title"`
		Kind                string     `json:"kind"`
		Status              string     `json:"status"`
		AssigneeID          *string    `json:"assignee_id"`
		StartAt             cliISOTime `json:"start_at"`
		Recurrence          cliRawJSON `json:"recurrence"`
		TargetConversationID *string   `json:"target_conversation_id"`
		IsPrivate           bool       `json:"is_private"`
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Title, &r.Kind, &r.Status, &r.AssigneeID, &r.StartAt, &r.Recurrence, &r.TargetConversationID, &r.IsPrivate); err != nil {
			return cliErrThrow(err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(list)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	scopeSuffix := " for " + me
	if all {
		scopeSuffix = " in workspace"
	}
	if len(list) == 0 {
		allTag := ""
		if all {
			allTag = " [workspace]"
		}
		return cliOK(fmt.Sprintf("(no calendar events for %s%s)", me, allTag))
	}
	lines := []string{fmt.Sprintf("%d calendar event(s)%s:", len(list), scopeSuffix), ""}
	for _, r := range list {
		rec := parseRecurrenceBrief(r.Recurrence)
		who := ""
		if r.AssigneeID != nil {
			who = " → @" + *r.AssigneeID
		}
		lock := ""
		if r.IsPrivate {
			lock = " 🔒"
		}
		lines = append(lines, "  ["+utf16PadEnd(r.Status, 7)+"] "+utf16PadEnd(utf16Slice(r.ID, 14), 15)+" "+
			isoMilli(time.Time(r.StartAt))[:16]+" · "+rec+who+lock+"  "+r.Title)
	}
	return cliOK(strings.Join(lines, "\n"))
}

// parseRecurrenceBrief:list 行的 `every N freq` / `one-shot` 摘要。
func parseRecurrenceBrief(raw cliRawJSON) string {
	if raw == nil || string(raw) == "null" {
		return "one-shot"
	}
	var rec struct {
		Freq    string `json:"freq"`
		Interval *int  `json:"interval"`
	}
	if jsonUnmarshal(raw, &rec) != nil || rec.Freq == "" {
		return "one-shot"
	}
	interval := 1
	if rec.Interval != nil {
		interval = *rec.Interval
	}
	return fmt.Sprintf("every %d %s", interval, rec.Freq)
}

func (s *Service) cliCalendarCreate(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	title := strings.TrimSpace(strings.Join(positionalFrom(parsed, 1), " "))
	if title == "" {
		return cliErr(`usage: calendar create "<title>" --at <iso> [flags]`)
	}
	startStr := parsed.flagStrOr("at", "")
	if startStr == "" {
		return cliErr("--at <iso-timestamp> is required")
	}
	start, ok := parseJSDate(startStr)
	if !ok {
		return cliErr("invalid --at: " + startStr)
	}
	assigneeID := flagStrOrNil(parsed, "assignee")
	agentPrompt := flagStrOrNil(parsed, "prompt")
	targetConvo := flagStrOrNil(parsed, "in")
	kind := "personal"
	if parsed.flagStrOr("kind", "") == "personal" {
		kind = "personal"
	} else if assigneeID != nil || agentPrompt != nil {
		kind = "agent_task"
	}
	if kind == "agent_task" && assigneeID == nil {
		return cliErr("agent_task events need an --assignee")
	}
	var recurrenceJSON any
	if _, hasEvery := parsed.flags["every"]; hasEvery {
		j, msg := cliParseRecurrenceFlags(parsed)
		if msg != "" {
			return cliErr(msg)
		}
		recurrenceJSON = j
	}
	// 提醒对:--remind <分钟> 配 --remind-channel(缺省 toast)。
	isPrivate := parsed.flagTruey("private")
	var reminderMinutes any
	var reminderChannel any
	if v, has := parsed.flags["remind"]; has {
		n, valid := jsFloorNumber(v)
		if !valid || n < 0 {
			return cliErr("--remind must be minutes (got: " + fmt.Sprint(v) + ")")
		}
		reminderMinutes = n
		ch := parsed.flagStrOr("remind-channel", "toast")
		switch ch {
		case "toast", "email", "both":
			reminderChannel = ch
		default:
			return cliErr("--remind-channel must be toast|email|both (got: " + ch + ")")
		}
	}
	// 双层防重:并发同伴(tenant claim)+ 顺序重复(15 分钟窗)。
	if blocked := s.cliTryClaimTenantWork(companyID, me, "calendar-create", title); blocked != nil {
		return *blocked
	}
	defer s.ReleaseWork("tenant:"+companyID, me, "calendar-create", title)
	// 私有事件两侧都豁免:私有提醒不是共享工作,也不得经 HELD 信封泄题。
	if !isPrivate {
		normTitle := NormalizeWorkSubject(title)
		calHoldScope := "calendar-create:" + normTitle
		forceArmed := false
		if parsed.flagTruey("force") {
			forceArmed = s.ConsumeHold(me, calHoldScope).Armed
		}
		if !forceArmed {
			rows, err := s.DB.QueryContext(ctx,
				`SELECT id, title, created_by, created_at FROM calendar_events
				  WHERE company_id = $1 AND created_by <> $2
				    AND status = 'active' AND is_private = FALSE
				    AND created_at > NOW() - INTERVAL '15 minutes'
				  ORDER BY created_at DESC LIMIT 50`, companyID, me)
			if err != nil {
				return cliErrThrow(err)
			}
			defer rows.Close()
			type dupRow struct {
				id, title, createdBy string
				createdAt            time.Time
			}
			var dups []dupRow
			for rows.Next() {
				var d dupRow
				if err := rows.Scan(&d.id, &d.title, &d.createdBy, &d.createdAt); err != nil {
					return cliErrThrow(err)
				}
				dups = append(dups, d)
			}
			if err := rows.Err(); err != nil {
				return cliErrThrow(err)
			}
			for _, d := range dups {
				if NormalizeWorkSubject(d.title) == normTitle {
					s.RecordHold(me, calHoldScope, nil)
					ageSec := (time.Since(d.createdAt).Milliseconds() + 500) / 1000
					if ageSec < 1 {
						ageSec = 1
					}
					return cliErrCode(
						fmt.Sprintf("HELD — event NOT created. %s already scheduled %q (%s) %ds ago — ", d.createdBy, d.title, d.id, ageSec)+
							`this work is DONE; a second copy double-books everyone. `+
							"Inspect theirs instead: `cumora calendar list` / `cumora calendar update "+d.id+" ...` if it needs changes. "+
							`If you GENUINELY need a separate same-title event, rerun with --force `+
							`(--force only works after you've been shown this hold — passing it preemptively does nothing).`, 2)
				}
			}
		}
	}
	// ce-<uuid v4 形>:与 TS randomUUID() 同构(36 位含连字符)。
	id := "ce-" + jsUUID()
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO calendar_events
		   (id, company_id, created_by, kind, title, assignee_id,
		    target_conversation_id, agent_prompt, start_at, recurrence,
		    reminder_minutes_before, reminder_channel, status, is_private)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,'active',$13)`,
		id, companyID, me, kind, title, assigneeID, targetConvo, agentPrompt, start,
		recurrenceJSON, reminderMinutes, reminderChannel, isPrivate); err != nil {
		return cliErrThrow(err)
	}
	s.publishCalendarCli(companyID, "event.created", id, me)
	recNote := ""
	if recurrenceJSON != nil {
		var rec struct {
			Freq     string `json:"freq"`
			Interval *int   `json:"interval"`
		}
		_ = jsonUnmarshal([]byte(recurrenceJSON.(string)), &rec)
		interval := 1
		if rec.Interval != nil {
			interval = *rec.Interval
		}
		recNote = fmt.Sprintf(" · every %d %s", interval, rec.Freq)
	}
	assigneeNote := ""
	if s, ok := assigneeID.(string); ok {
		assigneeNote = " → @" + s
	}
	remindNote := ""
	if reminderMinutes != nil {
		remindNote = fmt.Sprintf(" · remind %dm before (%s)", reminderMinutes.(int), reminderChannel.(string))
	}
	lockNote := ""
	if isPrivate {
		lockNote = " · 🔒 private"
	}
	effect := cliSideEffect{
		"event":                "calendar.event_created",
		"command":              "calendar create",
		"calendarEventId":      id,
		"actorId":              me,
		"companyId":            companyID,
		"title":                title,
		"kind":                 kind,
		"assigneeId":           assigneeID,
		"targetConversationId": targetConvo,
		"startAt":              isoMilli(start),
		"visibleToUser":        true,
	}
	if recurrenceJSON != nil {
		effect["recurrence"] = rawJSONAsAny(recurrenceJSON.(string))
	}
	if reminderMinutes != nil {
		effect["reminderMinutesBefore"] = reminderMinutes
		effect["reminderChannel"] = reminderChannel
	}
	return cliOK(fmt.Sprintf("scheduled %s: %q at %s%s%s%s%s", id, title, isoMilli(start), recNote, assigneeNote, remindNote, lockNote), effect)
}

// jsUUID:TS randomUUID() 的 v4 形(36 位,4 固定位)。
func jsUUID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x%x%x%x%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

func rawJSONAsAny(s string) any {
	var v any
	if jsonUnmarshal([]byte(s), &v) == nil {
		return v
	}
	return nil
}

// flagStrOrNil:flag 存在且为 string 时的可空参数。
func flagStrOrNil(p cliParsed, key string) any {
	if s, ok := p.flagStr(key); ok && s != "" {
		return s
	}
	return nil
}

func (s *Service) cliCalendarUpdate(ctx context.Context, parsed cliParsed, op, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: calendar " + op + ` <event_id> [--title "..."] [--at <iso>] [--status active|cancelled|done] [flags]`)
	}
	id := parsed.positional[1]
	// 可见性守卫折进 UPDATE 的同一条规则:看不到改不了。
	var one int
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM calendar_events
		  WHERE id = $1 AND company_id = $2 AND `+cliCalendarVisibilityClause(3)+` LIMIT 1`,
		id, companyID, me).Scan(&one)
	if err == sql.ErrNoRows {
		return cliErr("no event " + id)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	var sets []string
	var params []any
	push := func(column string, value any) {
		params = append(params, value)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(params)))
	}
	if v, ok := parsed.flags["title"]; ok {
		title := utf16Slice(strings.TrimSpace(fmt.Sprint(v)), 200)
		if title == "" {
			return cliErr("--title cannot be empty")
		}
		push("title", title)
	}
	if v, ok := parsed.flags["description"]; ok {
		val := utf16Slice(fmt.Sprint(v), 4000)
		if val == "" {
			push("description", nil)
		} else {
			push("description", val)
		}
	}
	if v, ok := parsed.flags["kind"]; ok {
		kind := fmt.Sprint(v)
		if kind != "personal" && kind != "agent_task" {
			return cliErr("--kind must be personal|agent_task")
		}
		push("kind", kind)
	}
	if v, ok := parsed.flags["assignee"]; ok {
		assignee := strings.TrimSpace(fmt.Sprint(v))
		if assignee == "" || assignee == "null" || assignee == "-" {
			push("assignee_id", nil)
		} else {
			push("assignee_id", assignee)
		}
	}
	if v, ok := parsed.flags["prompt"]; ok {
		val := utf16Slice(fmt.Sprint(v), 8000)
		if val == "" {
			push("agent_prompt", nil)
		} else {
			push("agent_prompt", val)
		}
	}
	if v, ok := parsed.flags["in"]; ok {
		target := strings.TrimSpace(fmt.Sprint(v))
		if target == "" || target == "null" || target == "-" {
			push("target_conversation_id", nil)
		} else {
			push("target_conversation_id", target)
		}
	}
	if v, ok := parsed.flags["at"]; ok {
		start, valid := parseJSDate(fmt.Sprint(v))
		if !valid {
			return cliErr("invalid --at: " + fmt.Sprint(v))
		}
		push("start_at", start)
	}
	if v, ok := parsed.flags["end"]; ok {
		raw := strings.TrimSpace(fmt.Sprint(v))
		if raw == "" || raw == "null" || raw == "-" {
			push("end_at", nil)
		} else {
			end, valid := parseJSDate(raw)
			if !valid {
				return cliErr("invalid --end: " + raw)
			}
			push("end_at", end)
		}
	}
	if v, ok := parsed.flags["status"]; ok {
		status := fmt.Sprint(v)
		switch status {
		case "active", "cancelled", "done":
		default:
			return cliErr("--status must be active|cancelled|done")
		}
		push("status", status)
	}
	if v, ok := parsed.flags["remind"]; ok {
		raw := strings.TrimSpace(fmt.Sprint(v))
		if raw == "" || raw == "null" || raw == "-" {
			push("reminder_minutes_before", nil)
		} else {
			n, valid := jsFloorNumber(raw)
			if !valid || n < 0 || n > 14*24*60 {
				return cliErr(fmt.Sprintf("--remind must be minutes in [0, 20160] (got: %s)", raw))
			}
			push("reminder_minutes_before", n)
		}
	}
	if v, ok := parsed.flags["remind-channel"]; ok {
		ch := strings.TrimSpace(fmt.Sprint(v))
		if ch == "" || ch == "null" || ch == "-" {
			push("reminder_channel", nil)
		} else {
			switch ch {
			case "toast", "email", "both":
				push("reminder_channel", ch)
			default:
				return cliErr("--remind-channel must be toast|email|both")
			}
		}
	}
	// --private / --public 翻私有位;同时传 --private 赢(防御)。
	if _, ok := parsed.flags["private"]; ok {
		push("is_private", true)
	} else if _, ok := parsed.flags["public"]; ok {
		push("is_private", false)
	}
	if _, hasEvery := parsed.flags["every"]; hasEvery {
		j, msg := cliParseRecurrenceFlags(parsed)
		if msg != "" {
			return cliErr(msg)
		}
		params = append(params, j)
		sets = append(sets, fmt.Sprintf("recurrence = $%d::jsonb", len(params)))
	} else if _, hasClear := parsed.flags["clear-recurrence"]; hasClear {
		params = append(params, nil)
		sets = append(sets, fmt.Sprintf("recurrence = $%d::jsonb", len(params)))
	}
	if len(sets) == 0 {
		return cliErr("nothing to update — pass at least one calendar field flag")
	}
	sets = append(sets, "updated_at = NOW()")
	params = append(params, id, companyID)
	var (
		rowID, rowTitle, rowKind, rowStatus string
		rowAssignee, rowTarget              sql.NullString
		rowStart                            time.Time
	)
	err = s.DB.QueryRowContext(ctx,
		`UPDATE calendar_events SET `+strings.Join(sets, ", ")+
			fmt.Sprintf(` WHERE id = $%d AND company_id = $%d
			  RETURNING id, title, kind, status, assignee_id, target_conversation_id, start_at`,
				len(params)-1, len(params)), params...,
	).Scan(&rowID, &rowTitle, &rowKind, &rowStatus, &rowAssignee, &rowTarget, &rowStart)
	if err == sql.ErrNoRows {
		return cliErr("no event " + id)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	s.publishCalendarCli(companyID, "event.updated", id, me)
	return cliOK(fmt.Sprintf("updated %s: %q at %s (%s)", id, rowTitle, isoMilli(rowStart), rowStatus), cliSideEffect{
		"event":                "calendar.event_updated",
		"command":              "calendar " + op,
		"calendarEventId":      id,
		"actorId":              me,
		"companyId":            companyID,
		"title":                rowTitle,
		"kind":                 rowKind,
		"status":               rowStatus,
		"assigneeId":           nullStrAny(rowAssignee),
		"targetConversationId": nullStrAny(rowTarget),
		"startAt":              isoMilli(rowStart),
		"visibleToUser":        true,
	})
}

func nullStrAny(ns sql.NullString) any {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func (s *Service) cliCalendarRunNow(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: calendar run-now <event_id>")
	}
	id := parsed.positional[1]
	// 私有门:看得到才派得发;dispatch 引擎与 REST 同源(domains/calendar)。
	query := `SELECT id, company_id, created_by, kind, title, description, assignee_id,
	                 target_conversation_id, agent_prompt, start_at, end_at, all_day,
	                 recurrence, status, last_fired_at,
	                 reminder_minutes_before, reminder_channel, is_private,
	                 created_at, updated_at
	            FROM calendar_events
	           WHERE id = $1 AND company_id = $2 AND ` + cliCalendarVisibilityClause(3)
	e, ok := domcalendar.ScanEventRow(s.DB.QueryRowContext(ctx, query, id, companyID, me))
	if !ok {
		return cliErr("no event " + id)
	}
	result := domcalendar.DispatchEvent(ctx, s.DB, e, time.Now())
	s.publishCalendarCli(companyID, "event.dispatched", id, me)
	return cliOK("dispatched "+id+": "+compactJSON(result), cliSideEffect{
		"event":           "calendar.event_dispatched",
		"command":         "calendar run-now",
		"calendarEventId": id,
		"actorId":         me,
		"companyId":       companyID,
		"result":          result,
		"visibleToUser":   true,
	})
}

func (s *Service) cliCalendarDispatches(ctx context.Context, parsed cliParsed, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: calendar dispatches <event_id>")
	}
	id := parsed.positional[1]
	rows, err := s.DB.QueryContext(ctx,
		`SELECT cd.id, cd.event_id, cd.scheduled_for, cd.dispatched_at, cd.status,
		        cd.conversation_id, cd.message_id, cd.error
		   FROM calendar_dispatches cd
		   JOIN calendar_events ce ON ce.id = cd.event_id
		  WHERE cd.event_id = $1 AND ce.company_id = $2
		  ORDER BY cd.scheduled_for DESC LIMIT 200`, id, companyID)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID            string      `json:"id"`
		EventID       string      `json:"event_id"`
		ScheduledFor  cliISOTime  `json:"scheduled_for"`
		DispatchedAt  *cliISOTime `json:"dispatched_at"`
		Status        string      `json:"status"`
		ConversationID *string    `json:"conversation_id"`
		MessageID     *string     `json:"message_id"`
		Error         *string     `json:"error"`
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.EventID, &r.ScheduledFor, &r.DispatchedAt, &r.Status, &r.ConversationID, &r.MessageID, &r.Error); err != nil {
			return cliErrThrow(err)
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(all)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	if len(all) == 0 {
		return cliOK("(no dispatches for " + id + ")")
	}
	lines := []string{fmt.Sprintf("%d dispatch(es) for %s:", len(all), id), ""}
	for _, r := range all {
		convo := "-"
		if r.ConversationID != nil {
			convo = *r.ConversationID
		}
		msgID := ""
		if r.MessageID != nil {
			msgID = *r.MessageID
		}
		errNote := ""
		if r.Error != nil {
			errNote = " · " + *r.Error
		}
		lines = append(lines, "  ["+r.Status+"] "+isoMilli(time.Time(r.ScheduledFor))+" → "+convo+" "+msgID+errNote)
	}
	return cliOK(strings.Join(lines, "\n"))
}

func (s *Service) cliCalendarCancelDelete(ctx context.Context, parsed cliParsed, op, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: calendar " + op + " <event_id>")
	}
	id := parsed.positional[1]
	// 可见性折进 WHERE:rowCount 0 统一映射"no event",不向未授权者泄存在性。
	var res sql.Result
	var err error
	if op == "delete" {
		res, err = s.DB.ExecContext(ctx,
			`DELETE FROM calendar_events
			  WHERE id = $1 AND company_id = $2 AND `+cliCalendarVisibilityClause(3),
			id, companyID, me)
	} else {
		res, err = s.DB.ExecContext(ctx,
			`UPDATE calendar_events SET status = 'cancelled', updated_at = NOW()
			  WHERE id = $1 AND company_id = $2 AND `+cliCalendarVisibilityClause(3),
			id, companyID, me)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cliErr("no event " + id)
	}
	// delete → deleted 行被客户端丢弃;cancel → updated 客户端重取。
	kind := "event.updated"
	word := "cancelled"
	effectEvent := "calendar.event_cancelled"
	if op == "delete" {
		kind = "event.deleted"
		word = "deleted"
		effectEvent = "calendar.event_deleted"
	}
	s.publishCalendarCli(companyID, kind, id, me)
	return cliOK(word+" "+id, cliSideEffect{
		"event":           effectEvent,
		"command":         "calendar " + op,
		"calendarEventId": id,
		"actorId":         me,
		"companyId":       companyID,
		"visibleToUser":   true,
	})
}
