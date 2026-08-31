// domains/calendar —— 日历域(#57):事件 CRUD(隐私可见性子句贯穿
// 读/改/删)、手动触发 dispatch、dispatch 历史。调度 tick(到期扫描+
// reminder)归 #60 运行时服务面;本包只交付 dispatchEvent 供 run-now
// 调用。行为对齐 已退役 TS server 的 api/router.ts 的 /calendar 段与
// 已退役 TS server 的 calendar.ts 的 dispatchEvent。
package calendar

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/calendar"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

const calendarSelect = `id, company_id, created_by, kind, title, description,
  assignee_id, target_conversation_id, agent_prompt, start_at, end_at, all_day,
  recurrence, status, last_fired_at,
  reminder_minutes_before, reminder_channel,
  is_private,
  created_at, updated_at`

// visibilityClause 对齐 router.ts 的 calendarVisibilityClause:公开行全员
// 可见;私密行仅 creator/assignee;任一端是 agent 时公司 owner 亦可见
// (监督可见性,不泄漏人与人私密事件)。meIdx/companyIdx 为占位符序号。
const visibilityClause = `(
    is_private = false
    OR created_by = $%d
    OR assignee_id = $%d
    OR (
      EXISTS (SELECT 1 FROM companies WHERE id = $%d AND owner_user_id = $%d)
      AND (
        created_by IN (SELECT id FROM participants WHERE company_id = $%d AND kind = 'agent')
        OR assignee_id IN (SELECT id FROM participants WHERE company_id = $%d AND kind = 'agent')
      )
    )
  )`

func vis(meIdx, companyIdx int) string {
	return fmt.Sprintf(visibilityClause, meIdx, meIdx, companyIdx, meIdx, companyIdx, companyIdx)
}

type eventRow struct {
	ID                 string
	CompanyID          string
	CreatedBy          string
	Kind               string
	Title              string
	Description        sql.NullString
	AssigneeID         sql.NullString
	TargetConversation sql.NullString
	AgentPrompt        sql.NullString
	StartAt            time.Time
	EndAt              sql.NullTime
	AllDay             bool
	Recurrence         []byte // jsonb 原文(nil = NULL)
	Status             string
	LastFiredAt        sql.NullTime
	ReminderMinutes    sql.NullInt64
	ReminderChannel    sql.NullString
	IsPrivate          bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

const eventScan = calendarSelect

// EventRow / ScanEventRow:导出给 runtime /runtime/cli 的 run-now 复用
// 同一个 dispatch 引擎(eventRow 本体保持包内)。
type EventRow = eventRow

func ScanEventRow(row interface{ Scan(...any) error }) (EventRow, bool) {
	return scanEvent(row)
}

func scanEvent(row interface{ Scan(...any) error }) (eventRow, bool) {
	var e eventRow
	var rec []byte
	if err := row.Scan(&e.ID, &e.CompanyID, &e.CreatedBy, &e.Kind, &e.Title, &e.Description,
		&e.AssigneeID, &e.TargetConversation, &e.AgentPrompt, &e.StartAt, &e.EndAt, &e.AllDay,
		&rec, &e.Status, &e.LastFiredAt, &e.ReminderMinutes, &e.ReminderChannel,
		&e.IsPrivate, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return e, false
	}
	e.Recurrence = rec
	return e, true
}

func ns(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

func nt(t sql.NullTime) any {
	if t.Valid {
		return t.Time.UTC()
	}
	return nil
}

// ntms:nullable 时间戳,ISOms 格式(toISOString 平价)。
func ntms(t sql.NullTime) any {
	if t.Valid {
		return httpx.ISOms(t.Time)
	}
	return nil
}

// toPayload 对齐 rowToCalendarEvent:全字段 camelCase,recurrence 原样
// 透传(客户端解析;NULL → null)。
func (e eventRow) toPayload() map[string]any {
	var rec any
	if len(e.Recurrence) > 0 {
		var parsed any
		if json.Unmarshal(e.Recurrence, &parsed) == nil {
			rec = parsed
		} else {
			rec = nil
		}
	} else {
		rec = nil
	}
	var remMin any
	if e.ReminderMinutes.Valid {
		remMin = e.ReminderMinutes.Int64
	} else {
		remMin = nil
	}
	return map[string]any{
		"id": e.ID, "companyId": e.CompanyID, "createdBy": e.CreatedBy,
		"kind": e.Kind, "title": e.Title, "description": ns(e.Description),
		"assigneeId": ns(e.AssigneeID), "targetConversationId": ns(e.TargetConversation),
		"agentPrompt": ns(e.AgentPrompt), "startAt": httpx.ISOms(e.StartAt), "endAt": ntms(e.EndAt),
		"allDay": e.AllDay, "recurrence": rec, "status": e.Status,
		"lastFiredAt": ntms(e.LastFiredAt), "reminderMinutesBefore": remMin,
		"reminderChannel": ns(e.ReminderChannel), "isPrivate": e.IsPrivate,
		"createdAt": httpx.ISOms(e.CreatedAt), "updatedAt": httpx.ISOms(e.UpdatedAt),
	}
}

func isKind(v string) bool { return v == "personal" || v == "agent_task" }
func isStatus(v string) bool {
	return v == "active" || v == "paused" || v == "done" || v == "cancelled"
}
func isReminderChannel(v string) bool { return v == "toast" || v == "email" || v == "both" }

// parseRecurrence 对齐 router.ts:对象形状校验,freq 白名单,interval
// floor≥1,byweekday 过滤 0-6 整数(全滤掉则 undefined),until 归一
// ISO,count 正整数。nil = 未提供/显式 null。
func parseRecurrence(raw json.RawMessage) (map[string]any, int, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, 0, ""
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, http.StatusBadRequest, "recurrence must be an object"
	}
	freq, _ := r["freq"].(string)
	if freq != "daily" && freq != "weekly" && freq != "monthly" && freq != "yearly" {
		return nil, http.StatusBadRequest, "recurrence.freq must be daily|weekly|monthly|yearly"
	}
	interval := 1
	if v, ok := r["interval"]; ok && v != nil {
		if raw, ok2 := json.Marshal(v); ok2 == nil {
			if f, ok3 := tsNumber(raw); ok3 {
				interval = int(f) // TS Math.floor(Number(r.interval ?? 1));Number('2')=2
			}
		}
	}
	if interval < 1 {
		interval = 1
	}
	// TS 的返回字面量恒含 until/count 键(缺省为 null);byweekday 仅在
	// 非空时出现(undefined 键 JSON.stringify 会丢弃)。
	out := map[string]any{"freq": freq, "interval": interval, "until": nil, "count": nil}
	if arr, ok := r["byweekday"].([]any); ok {
		days := []any{}
		for _, d := range arr {
			// TS:map(Number) 后过滤 Number.isInteger —— 字符串数字可被 Number 强转
			var f float64
			switch t := d.(type) {
			case float64:
				f = t
			case string:
				if pf, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
					f = pf
				} else {
					continue
				}
			default:
				continue
			}
			if f == float64(int(f)) && f >= 0 && f <= 6 {
				days = append(days, int(f))
			}
		}
		if len(days) > 0 {
			out["byweekday"] = days
		}
	}
	if u, ok := r["until"].(string); ok && strings.TrimSpace(u) != "" {
		d, ok2 := tsDate(u)
		if !ok2 {
			return nil, http.StatusBadRequest, "recurrence.until must be a valid ISO timestamp"
		}
		out["until"] = httpx.ISOms(d)
	}
	if c, ok := r["count"]; ok && c != nil {
		raw, _ := json.Marshal(c)
		f, ok2 := tsNumber(raw)
		if !ok2 || f < 1 {
			return nil, http.StatusBadRequest, "recurrence.count must be a positive integer"
		}
		out["count"] = int(f) // TS Math.floor(Number('3'))=3
	}
	return out, 0, ""
}

// text:TS `.trim().slice(0, N)` —— UTF-16 码元截断(#141 rider:
// rune 截断在代理对边界漂移,长 emoji 标题会差 1 字)。
func text(v string, max int) string {
	return httpx.UTF16Cap(strings.TrimSpace(v), max)
}

// capOnly:TS 的 String(x).slice(0,N) 语义 —— 不 trim(description/
// agentPrompt 在 baseline 不 trim,trim 会改存库数据),仅 rune 截断。
func capOnly(v string, max int) string {
	return httpx.UTF16Cap(v, max)
}

// tsString:TS String(x) 强转(JSON 标量 → 字符串;null 字面量除外,
// 由调用方先行判空)。数字/布尔用其 JSON 字面量(String(42)="42")。
func tsString(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return "null"
	default:
		return string(raw)
	}
}

// tsNumber:TS Number(x)(数值标量;非数值 → NaN,由 min/max 校验拒绝)。
func tsNumber(raw json.RawMessage) (float64, bool) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case bool:
		return 0, false
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// tsBool:TS Boolean(x)(字符串非空即真;0/null/” 假)。
func tsBool(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	default:
		return false
	}
}

// tsDate:TS new Date(x) 的宽限度 —— RFC3339、date-only(JS 规范:
// 纯日期按 UTC)、无时区 date-time(按本地,与 JS 一致);失败 ok=false。
func tsDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func newID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func publishCalendarChange(ctx context.Context, kind, eventID, companyID, actorID string) {
	payload, _ := json.Marshal(map[string]any{
		"type": "calendar.changed", "kind": kind, "eventId": eventID,
		"companyId": companyID, "actorId": actorID,
	})
	_ = events.PublishRaw(ctx, events.ChCalendarEvents, payload)
}

// Server:contract.calendar ServerInterface 的域实现(#187 机械迁移,
// documents 范式)。方法体自原闭包工厂原样搬运。
type Server struct{ DB *sql.DB }

// 编译期接口把关:规范改动 operation 而域未跟 = 构建红。
var _ contract.ServerInterface = (*Server)(nil)

// Mount:注册串来自契约生成物(pattern 即规范,#139)。
func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

func (s *Server) ListCalendarEvents(w http.ResponseWriter, r *http.Request, params contract.ListCalendarEventsParams) {
	me, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	// 可选窗口:from=「窗口内开始 OR 循环+active」,to=开始不晚于上界
	args := []any{companyID, me}
	sqlStr := `SELECT ` + eventScan + ` FROM calendar_events
	 WHERE company_id = $1 AND ` + vis(2, 1)
	// TS:同名多值 → express 给数组 → new Date(数组)=NaN → 忽略过滤
	qvals := r.URL.Query()
	if len(qvals["from"]) == 1 {
		if d, ok := tsDate(qvals["from"][0]); ok {
			args = append(args, d)
			sqlStr += fmt.Sprintf(` AND (start_at >= $%d OR (recurrence IS NOT NULL AND status = 'active'))`, len(args))
		}
	}
	if len(qvals["to"]) == 1 {
		if d, ok := tsDate(qvals["to"][0]); ok {
			args = append(args, d)
			sqlStr += fmt.Sprintf(` AND start_at <= $%d`, len(args))
		}
	}
	sqlStr += ` ORDER BY start_at ASC LIMIT 1000`
	rows, err := s.DB.QueryContext(r.Context(), sqlStr, args...)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		if e, ok := scanEvent(rows); ok {
			out = append(out, e.toPayload())
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) CreateCalendarEvent(w http.ResponseWriter, r *http.Request) {
	me, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		httpx.WriteError(w, http.StatusBadRequest, "body required")
		return
	}
	title := ""
	if raw, ok := body["title"]; ok && string(raw) != "null" {
		title = text(tsString(raw), 200)
	}
	if title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "title required")
		return
	}
	kind := "personal"
	if raw, ok := body["kind"]; ok {
		if s := tsString(raw); isKind(s) {
			kind = s
		}
	}
	// baseline:String(x).slice —— 不 trim(改 trim 即改存库数据);null → NULL
	var description sql.NullString
	if raw, ok := body["description"]; ok && string(raw) != "null" {
		description = sql.NullString{String: capOnly(tsString(raw), 4000), Valid: true}
	}
	trimOrNull := func(k string) sql.NullString {
		if raw, ok := body[k]; ok && string(raw) != "null" {
			if t := strings.TrimSpace(tsString(raw)); t != "" {
				return sql.NullString{String: t, Valid: true}
			}
		}
		return sql.NullString{}
	}
	assigneeID := trimOrNull("assigneeId")
	targetConv := trimOrNull("targetConversationId")
	var agentPrompt sql.NullString
	if raw, ok := body["agentPrompt"]; ok && string(raw) != "null" {
		agentPrompt = sql.NullString{String: capOnly(tsString(raw), 8000), Valid: true}
	}
	startAt, startOK := func() (time.Time, bool) {
		if raw, ok := body["startAt"]; ok {
			return tsDate(tsString(raw))
		}
		return time.Time{}, false
	}()
	if !startOK {
		httpx.WriteError(w, http.StatusBadRequest, "startAt must be a valid ISO timestamp")
		return
	}
	var endAt sql.NullTime
	if raw, ok := body["endAt"]; ok {
		if d, ok2 := tsDate(tsString(raw)); ok2 {
			endAt = sql.NullTime{Time: d, Valid: true}
		}
	}
	var allDay bool
	if raw, ok := body["allDay"]; ok {
		allDay = tsBool(raw)
	}
	var recurrence []byte
	if raw, ok := body["recurrence"]; ok {
		parsed, code, msg := parseRecurrence(raw)
		if code != 0 {
			httpx.WriteError(w, code, msg)
			return
		}
		if parsed != nil {
			recurrence, _ = json.Marshal(parsed)
		}
	}
	status := "active"
	if raw, ok := body["status"]; ok {
		if s := tsString(raw); isStatus(s) {
			status = s
		}
	}
	// reminder 双置校验:非空 channel 须配正提前量,反之亦然。
	var reminderMinutes sql.NullInt64
	var reminderChannel sql.NullString
	if raw, ok := body["reminderMinutesBefore"]; ok && string(raw) != "null" {
		f, ok2 := tsNumber(raw)
		n := int64(f) // TS Math.floor(先取整再校验;负数在本域必被拒)
		if !ok2 || n < 0 || n > 14*24*60 {
			httpx.WriteError(w, http.StatusBadRequest, "reminderMinutesBefore must be a non-negative integer (≤ 2 weeks)")
			return
		}
		reminderMinutes = sql.NullInt64{Int64: n, Valid: true}
	}
	if raw, ok := body["reminderChannel"]; ok && string(raw) != "null" {
		s := tsString(raw)
		if !isReminderChannel(s) {
			httpx.WriteError(w, http.StatusBadRequest, "reminderChannel must be toast|email|both")
			return
		}
		reminderChannel = sql.NullString{String: s, Valid: true}
	}
	if reminderMinutes.Valid != reminderChannel.Valid {
		httpx.WriteError(w, http.StatusBadRequest, "reminderMinutesBefore and reminderChannel must both be set or both null")
		return
	}
	var isPrivate bool
	if raw, ok := body["isPrivate"]; ok {
		isPrivate = tsBool(raw)
	}
	if kind == "agent_task" && !assigneeID.Valid {
		httpx.WriteError(w, http.StatusBadRequest, "agent_task events require an assigneeId")
		return
	}
	// 跨租户安全预检:assignee 与目标会话须属本公司。
	if assigneeID.Valid {
		var exists bool
		if err := s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`,
			assigneeID.String, companyID).Scan(&exists); err != nil || !exists {
			httpx.WriteError(w, http.StatusBadRequest, "assigneeId not found in this team")
			return
		}
	}
	if targetConv.Valid {
		var exists bool
		if err := s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM conversations WHERE id = $1 AND company_id = $2 LIMIT 1`,
			targetConv.String, companyID).Scan(&exists); err != nil || !exists {
			httpx.WriteError(w, http.StatusBadRequest, "targetConversationId not found in this team")
			return
		}
	}
	id := newID("ce-")
	var e eventRow
	insertErr := s.DB.QueryRowContext(r.Context(), `
		INSERT INTO calendar_events
		  (id, company_id, created_by, kind, title, description, assignee_id,
		   target_conversation_id, agent_prompt, start_at, end_at, all_day,
		   recurrence, status, reminder_minutes_before, reminder_channel, is_private)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::jsonb,$14,$15,$16,$17)
		RETURNING `+eventScan,
		id, companyID, me, kind, title, description, assigneeID, targetConv, agentPrompt,
		startAt, endAt, allDay, recurrenceByte(recurrence), status, reminderMinutes, reminderChannel, isPrivate).
		Scan(&e.ID, &e.CompanyID, &e.CreatedBy, &e.Kind, &e.Title, &e.Description,
			&e.AssigneeID, &e.TargetConversation, &e.AgentPrompt, &e.StartAt, &e.EndAt, &e.AllDay,
			&recurrence, &e.Status, &e.LastFiredAt, &e.ReminderMinutes, &e.ReminderChannel,
			&e.IsPrivate, &e.CreatedAt, &e.UpdatedAt)
	if insertErr != nil {
		httpx.WriteInternalError(w, r, insertErr)
		return
	}
	e.Recurrence = recurrence
	publishCalendarChange(r.Context(), "event.created", id, companyID, me)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"event": e.toPayload()})
}

func recurrenceByte(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (s *Server) GetCalendarEvent(w http.ResponseWriter, r *http.Request, id string) {
	me, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var e eventRow
	var rec []byte
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT `+eventScan+` FROM calendar_events
		 WHERE id = $1 AND company_id = $2 AND `+vis(3, 2)+` LIMIT 1`,
		id, companyID, me).
		Scan(&e.ID, &e.CompanyID, &e.CreatedBy, &e.Kind, &e.Title, &e.Description,
			&e.AssigneeID, &e.TargetConversation, &e.AgentPrompt, &e.StartAt, &e.EndAt, &e.AllDay,
			&rec, &e.Status, &e.LastFiredAt, &e.ReminderMinutes, &e.ReminderChannel,
			&e.IsPrivate, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			httpx.WriteError(w, http.StatusNotFound, "event not found")
		} else {
			httpx.WriteInternalError(w, r, err)
		}
		return
	}
	e.Recurrence = rec
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"event": e.toPayload()})
}

func (s *Server) UpdateCalendarEvent(w http.ResponseWriter, r *http.Request, id string) {
	me, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		httpx.WriteError(w, http.StatusBadRequest, "body required")
		return
	}
	// 隐私守卫:看不到的行也改不了(同 GET 的可见性子句,404 不泄存在)。
	var visible bool
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM calendar_events WHERE id = $1 AND company_id = $2 AND `+vis(3, 2)+` LIMIT 1`,
		id, companyID, me).Scan(&visible); err != nil || !visible {
		httpx.WriteError(w, http.StatusNotFound, "event not found")
		return
	}
	sets := []string{}
	args := []any{}
	push := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	has := func(k string) bool {
		_, ok := body[k]
		return ok
	}
	rawOf := func(k string) json.RawMessage { return body[k] }
	isNull := func(k string) bool { return string(rawOf(k)) == "null" }
	strOf := func(k string) string { return tsString(rawOf(k)) }
	if has("title") {
		t := text(strOf("title"), 200)
		if t == "" {
			httpx.WriteError(w, http.StatusBadRequest, "title cannot be empty")
			return
		}
		push("title", t)
	}
	if has("kind") {
		k := strOf("kind")
		if !isKind(k) {
			httpx.WriteError(w, http.StatusBadRequest, "invalid kind")
			return
		}
		push("kind", k)
	}
	if has("description") {
		if isNull("description") {
			push("description", nil)
		} else {
			push("description", text(strOf("description"), 4000))
		}
	}
	if has("assigneeId") {
		if isNull("assigneeId") {
			push("assignee_id", nil)
		} else {
			push("assignee_id", strOf("assigneeId"))
		}
	}
	if has("targetConversationId") {
		if isNull("targetConversationId") {
			push("target_conversation_id", nil)
		} else {
			push("target_conversation_id", strOf("targetConversationId"))
		}
	}
	if has("agentPrompt") {
		if isNull("agentPrompt") {
			push("agent_prompt", nil)
		} else {
			push("agent_prompt", text(strOf("agentPrompt"), 8000))
		}
	}
	if has("startAt") {
		d, ok2 := tsDate(strOf("startAt"))
		if !ok2 {
			httpx.WriteError(w, http.StatusBadRequest, "invalid startAt")
			return
		}
		push("start_at", d)
	}
	if has("endAt") {
		if isNull("endAt") {
			push("end_at", nil)
		} else {
			d, ok2 := tsDate(strOf("endAt"))
			if !ok2 {
				httpx.WriteError(w, http.StatusBadRequest, "invalid endAt")
				return
			}
			push("end_at", d)
		}
	}
	if has("allDay") {
		push("all_day", tsBool(rawOf("allDay")))
	}
	if has("recurrence") {
		parsed, code, msg := parseRecurrence(rawOf("recurrence"))
		if code != 0 {
			httpx.WriteError(w, code, msg)
			return
		}
		if parsed == nil {
			args = append(args, nil)
			sets = append(sets, fmt.Sprintf("recurrence = $%d::jsonb", len(args)))
		} else {
			raw, _ := json.Marshal(parsed)
			args = append(args, raw)
			sets = append(sets, fmt.Sprintf("recurrence = $%d::jsonb", len(args)))
		}
	}
	if has("status") {
		s := strOf("status")
		if !isStatus(s) {
			httpx.WriteError(w, http.StatusBadRequest, "invalid status")
			return
		}
		push("status", s)
	}
	if has("reminderMinutesBefore") {
		if isNull("reminderMinutesBefore") {
			push("reminder_minutes_before", nil)
		} else {
			f, ok2 := tsNumber(rawOf("reminderMinutesBefore"))
			n := int64(f) // TS Math.floor
			if !ok2 || n < 0 || n > 14*24*60 {
				httpx.WriteError(w, http.StatusBadRequest, "reminderMinutesBefore must be a non-negative integer (≤ 2 weeks)")
				return
			}
			push("reminder_minutes_before", n)
		}
	}
	if has("reminderChannel") {
		if isNull("reminderChannel") {
			push("reminder_channel", nil)
		} else {
			s := strOf("reminderChannel")
			if !isReminderChannel(s) {
				httpx.WriteError(w, http.StatusBadRequest, "reminderChannel must be toast|email|both")
				return
			}
			push("reminder_channel", s)
		}
	}
	if has("isPrivate") {
		push("is_private", tsBool(rawOf("isPrivate")))
	}
	if len(sets) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "no updatable fields")
		return
	}
	sets = append(sets, "updated_at = NOW()")
	args = append(args, id, companyID)
	sqlStr := fmt.Sprintf(`UPDATE calendar_events SET %s
		WHERE id = $%d AND company_id = $%d RETURNING %s`,
		strings.Join(sets, ", "), len(args)-1, len(args), eventScan)
	var e eventRow
	var rec []byte
	err := s.DB.QueryRowContext(r.Context(), sqlStr, args...).
		Scan(&e.ID, &e.CompanyID, &e.CreatedBy, &e.Kind, &e.Title, &e.Description,
			&e.AssigneeID, &e.TargetConversation, &e.AgentPrompt, &e.StartAt, &e.EndAt, &e.AllDay,
			&rec, &e.Status, &e.LastFiredAt, &e.ReminderMinutes, &e.ReminderChannel,
			&e.IsPrivate, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			httpx.WriteError(w, http.StatusNotFound, "event not found")
		} else {
			httpx.WriteInternalError(w, r, err)
		}
		return
	}
	e.Recurrence = rec
	publishCalendarChange(r.Context(), "event.updated", id, companyID, me)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"event": e.toPayload()})
}

func (s *Server) DeleteCalendarEvent(w http.ResponseWriter, r *http.Request, id string) {
	me, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM calendar_events WHERE id = $1 AND company_id = $2 AND `+vis(3, 2),
		id, companyID, me)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "event not found")
		return
	}
	publishCalendarChange(r.Context(), "event.deleted", id, companyID, me)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) RunCalendarEventNow(w http.ResponseWriter, r *http.Request, id string) {
	me, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var e eventRow
	var rec []byte
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT `+eventScan+` FROM calendar_events
		 WHERE id = $1 AND company_id = $2 AND `+vis(3, 2),
		id, companyID, me).
		Scan(&e.ID, &e.CompanyID, &e.CreatedBy, &e.Kind, &e.Title, &e.Description,
			&e.AssigneeID, &e.TargetConversation, &e.AgentPrompt, &e.StartAt, &e.EndAt, &e.AllDay,
			&rec, &e.Status, &e.LastFiredAt, &e.ReminderMinutes, &e.ReminderChannel,
			&e.IsPrivate, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			httpx.WriteError(w, http.StatusNotFound, "event not found")
		} else {
			httpx.WriteInternalError(w, r, err)
		}
		return
	}
	e.Recurrence = rec
	// NOW() 为槽位;(event_id, scheduled_for) 唯一性吸收同分钟内连点。
	result := DispatchEvent(r.Context(), s.DB, e, time.Now())
	publishCalendarChange(r.Context(), "event.dispatched", e.ID, companyID, me)
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (s *Server) ListCalendarDispatches(w http.ResponseWriter, r *http.Request, id string) {
	me, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var visible bool
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM calendar_events WHERE id = $1 AND company_id = $2 AND `+vis(3, 2)+` LIMIT 1`,
		id, companyID, me).Scan(&visible); err != nil || !visible {
		httpx.WriteError(w, http.StatusNotFound, "event not found")
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT cd.id, cd.event_id, cd.scheduled_for, cd.dispatched_at, cd.status,
		       cd.conversation_id, cd.message_id, cd.error
		  FROM calendar_dispatches cd
		  JOIN calendar_events ce ON ce.id = cd.event_id
		 WHERE cd.event_id = $1 AND ce.company_id = $2
		 ORDER BY cd.scheduled_for DESC LIMIT 200`, id, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	type dispatch struct {
		ID             string `json:"id"`
		EventID        string `json:"eventId"`
		ScheduledFor   string `json:"scheduledFor"`
		DispatchedAt   string `json:"dispatchedAt"`
		Status         string `json:"status"`
		ConversationID any    `json:"conversationId"`
		MessageID      any    `json:"messageId"`
		Error          any    `json:"error"`
	}
	out := []dispatch{}
	for rows.Next() {
		var d dispatch
		var schedFor, dispatched time.Time
		var convo, msg, errTxt sql.NullString
		if rows.Scan(&d.ID, &d.EventID, &schedFor, &dispatched, &d.Status, &convo, &msg, &errTxt) == nil {
			d.ScheduledFor = httpx.ISOms(schedFor)
			d.DispatchedAt = httpx.ISOms(dispatched)
			d.ConversationID = ns(convo)
			d.MessageID = ns(msg)
			d.Error = ns(errTxt)
			out = append(out, d)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"dispatches": out})
}

// DispatchResult 对齐 calendar.ts dispatchEvent 的返回形状
// (字段省略语义:TS undefined = 键不存在,Go 用指针 nil = omitempty)。
type DispatchResult struct {
	Status         string  `json:"status"`
	MessageID      *string `json:"messageId,omitempty"`
	ConversationID *string `json:"conversationId,omitempty"`
	Error          *string `json:"error,omitempty"`
}

func strp(s string) *string { return &s }

// DispatchEvent 对齐 calendar.ts:先占 (event_id, scheduled_for) 槽
// (并发/副本互斥),personal 或无 assignee 记 skipped,解析目标会话
// (钉定会话须 assignee 是成员;否则回退 creator↔assignee 既有 DM,
// 不自动建),投递系统消息(author='calendar'),更新 dispatch 行。
func DispatchEvent(ctx context.Context, db *sql.DB, e eventRow, scheduledFor time.Time) DispatchResult {
	dispatchID := newID("cd-")
	var claimed string
	err := db.QueryRowContext(ctx, `
		INSERT INTO calendar_dispatches (id, event_id, company_id, scheduled_for, status)
		VALUES ($1,$2,$3,$4,'pending')
		ON CONFLICT (event_id, scheduled_for) DO NOTHING
		RETURNING id`, dispatchID, e.ID, e.CompanyID, scheduledFor).Scan(&claimed)
	if err != nil {
		msg := err.Error()
		if err == sql.ErrNoRows {
			return DispatchResult{Status: "duplicate"}
		}
		return DispatchResult{Status: "failed", Error: strp(msg)}
	}
	// personal 不投递(历史视图仍可见);无 assignee 同理。
	if e.Kind != "agent_task" || !e.AssigneeID.Valid {
		reason := "no assignee"
		if e.Kind == "personal" {
			reason = "personal event"
		}
		_, _ = db.ExecContext(ctx,
			`UPDATE calendar_dispatches SET status = 'skipped', error = $2 WHERE id = $1`, dispatchID, reason)
		return DispatchResult{Status: "skipped"}
	}
	convoID, ok := resolveTargetConversation(ctx, db, e)
	if !ok {
		_, _ = db.ExecContext(ctx,
			`UPDATE calendar_dispatches SET status = 'skipped', error = $2 WHERE id = $1`,
			dispatchID, "no target conversation")
		return DispatchResult{Status: "skipped", Error: strp("no target conversation")}
	}
	messageID, err := postDispatchMessage(ctx, db, e, convoID, scheduledFor)
	if err != nil {
		_, _ = db.ExecContext(ctx,
			`UPDATE calendar_dispatches SET status = 'failed', error = $2, conversation_id = $3 WHERE id = $1`,
			dispatchID, err.Error(), convoID)
		return DispatchResult{Status: "failed", Error: strp(err.Error()), ConversationID: strp(convoID)}
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE calendar_dispatches SET status = 'dispatched', conversation_id = $2, message_id = $3 WHERE id = $1`,
		dispatchID, convoID, messageID)
	return DispatchResult{Status: "dispatched", MessageID: strp(messageID), ConversationID: strp(convoID)}
}

func resolveTargetConversation(ctx context.Context, db *sql.DB, e eventRow) (string, bool) {
	if e.TargetConversation.Valid {
		var members []byte
		err := db.QueryRowContext(ctx,
			`SELECT members FROM conversations WHERE id = $1 AND company_id = $2 LIMIT 1`,
			e.TargetConversation.String, e.CompanyID).Scan(&members)
		if err != nil {
			return "", false
		}
		var arr []string
		if json.Unmarshal(members, &arr) != nil {
			return "", false
		}
		// assignee 不是目标会话成员 → 跳过(避免 agent 看不见的 @提及悬空)。
		if e.AssigneeID.Valid {
			for _, m := range arr {
				if m == e.AssigneeID.String {
					return e.TargetConversation.String, true
				}
			}
			return "", false
		}
		return e.TargetConversation.String, true
	}
	if !e.AssigneeID.Valid {
		return "", false
	}
	// 回退:creator↔assignee 既有 DM;不自动创建。
	var id string
	err := db.QueryRowContext(ctx, `
		SELECT c.id
		  FROM conversation_members ca
		  JOIN conversation_members cb ON cb.conversation_id = ca.conversation_id
		  JOIN conversations c ON c.id = ca.conversation_id
		 WHERE ca.participant_id = $2 AND cb.participant_id = $3
		   AND c.company_id = $1 AND c.kind = 'direct' LIMIT 1`,
		e.CompanyID, e.CreatedBy, e.AssigneeID.String).Scan(&id)
	if err != nil {
		return "", false
	}
	return id, true
}

// dispatchBody 字段顺序对齐 renderDispatchBody 的 JSON.stringify
// (消费者按 JSON 解析,顺序不影响语义,但保持逐字节平价)。
type dispatchBody struct {
	Kind               string          `json:"kind"`
	EventID            string          `json:"eventId"`
	EventKind          string          `json:"eventKind"`
	Title              string          `json:"title"`
	Description        *string         `json:"description"`
	AgentPrompt        *string         `json:"agentPrompt"`
	AssigneeID         *string         `json:"assigneeId"`
	TargetConversation *string         `json:"targetConversationId"`
	ScheduledFor       string          `json:"scheduledFor"`
	StartAt            string          `json:"startAt"`
	EndAt              *string         `json:"endAt"`
	AllDay             bool            `json:"allDay"`
	Recurrence         json.RawMessage `json:"recurrence"`
	CreatedBy          string          `json:"createdBy"`
}

func postDispatchMessage(ctx context.Context, db *sql.DB, e eventRow, convoID string, scheduledFor time.Time) (string, error) {
	// counter upsert:INSERT path seed 2(RETURNING next-1;#53 血泪)
	var sequence int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1`, convoID).Scan(&sequence); err != nil {
		return "", err
	}
	messageID := newID("m-")
	title := strings.TrimSpace(e.Title)
	if title == "" {
		title = "Calendar event"
	}
	var desc, prompt *string
	if e.Description.Valid && strings.TrimSpace(e.Description.String) != "" {
		s := strings.TrimSpace(e.Description.String)
		desc = &s
	}
	if e.AgentPrompt.Valid && strings.TrimSpace(e.AgentPrompt.String) != "" {
		s := strings.TrimSpace(e.AgentPrompt.String)
		prompt = &s
	}
	var endAt *string
	if e.EndAt.Valid {
		s := httpx.ISOms(e.EndAt.Time)
		endAt = &s
	}
	var rec json.RawMessage
	if len(e.Recurrence) > 0 {
		rec = json.RawMessage(e.Recurrence)
	}
	body, err := json.Marshal(dispatchBody{
		Kind: "calendar_event", EventID: e.ID, EventKind: e.Kind, Title: title,
		Description: desc, AgentPrompt: prompt,
		AssigneeID:         nsp(e.AssigneeID),
		TargetConversation: nsp(e.TargetConversation),
		ScheduledFor:       httpx.ISOms(scheduledFor),
		StartAt:            httpx.ISOms(e.StartAt),
		EndAt:              endAt, AllDay: e.AllDay,
		Recurrence: rec, CreatedBy: e.CreatedBy,
	})
	if err != nil {
		return "", err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		VALUES ($1,$2,'calendar','system',$3,$4,$5)`,
		messageID, convoID, body, sequence, e.CompanyID); err != nil {
		return "", err
	}
	_, _ = db.ExecContext(ctx, `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, convoID)
	events.MessageNew(ctx, e.CompanyID, convoID, map[string]any{
		"id": messageID, "conversationId": convoID, "authorId": "calendar",
		"kind": "system", "body": string(body), "sequence": sequence,
		"at": httpx.ISOms(time.Now()),
	})
	return messageID, nil
}

func nsp(s sql.NullString) *string {
	if s.Valid {
		return &s.String
	}
	return nil
}
