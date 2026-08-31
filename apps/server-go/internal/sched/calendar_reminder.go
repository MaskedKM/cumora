// sched 包 calendar_reminder —— 日历提醒发布方(#209):已退役 TS
// calendar.ts 的 startCalendarScheduler 提醒半边的 Go 移植。每分钟扫
// calendar_events(status=active 且双置了 reminder 提前量/渠道),找出
// "提醒窗口已开且未提醒过"的事件,先占 calendar_reminders 槽位(唯一键
// (event_id, scheduled_for) 防重启/多副本重发)再 fan-out:toast 恒发
// (Redis ChCalendarReminder → WS 桥按租户转发),email 仅在 Resend 已
// 配置时发(未配置静默跳过,对齐 TS 的 EMAIL_DOMAIN 门)。dispatch 半边
// (agent_task 到期投递)不在本票——TS 迁移时整只调度器被删,本文件只
// 补回消费端仍存活且常量已被桥订阅的 calendar.reminder。
package sched

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/calendar"
	core "github.com/MaskedKM/cumora/apps/server-go/internal/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

/* ───────────────────────── recurrence 数学(对齐 calendar.ts) ───────────────────────── */

// recurrenceRule:calendar_events.recurrence jsonb 的存储形状(域层
// parseRecurrence 归一后落库:{freq,interval,until,count,byweekday?})。
// until/count 的 null 落成零值/nil。
type recurrenceRule struct {
	Freq      string `json:"freq"`
	Interval  int    `json:"interval"`
	Byweekday []int  `json:"byweekday"`
	Until     string `json:"until"`
	Count     *int   `json:"count"`
}

// parseRecurrenceRule:jsonb 原文 → 规则;NULL/不可解析 → nil(= one-shot,
// 提醒只看 start_at)。宽容解析:历史行由域层归一,坏形状按无循环处理。
func parseRecurrenceRule(raw []byte) *recurrenceRule {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var r recurrenceRule
	if json.Unmarshal(raw, &r) != nil || r.Freq == "" {
		return nil
	}
	if r.Interval < 1 {
		r.Interval = 1
	}
	return &r
}

// addDays/addMonths/addYears:JS Date 的 setUTCDate/setUTCMonth/
// setUTCFullYear 平价。Go time.AddDate 对溢出日(如 1/31 + 1 月)的
// 归一化与 JS Date 滚动语义一致;所有步进先归 UTC(JS 亦是 UTC 数学)。
func addDays(t time.Time, n int) time.Time   { return t.UTC().AddDate(0, 0, n) }
func addMonths(t time.Time, n int) time.Time { return t.UTC().AddDate(0, n, 0) }
func addYears(t time.Time, n int) time.Time  { return t.UTC().AddDate(n, 0, 0) }
func jsWeekday(t time.Time) int              { return int(t.UTC().Weekday()) } // 0=周日 … 6=周六

// stepOnce:从 from 前进一个 rrule「interval」(对齐 calendar.ts stepOnce)。
// weekly + byweekday 时逐日扫(上限 14 次,足够覆盖一周内全部许可日)。
func stepOnce(from time.Time, rule *recurrenceRule) time.Time {
	interval := rule.Interval
	if interval < 1 {
		interval = 1
	}
	switch rule.Freq {
	case "daily":
		return addDays(from, interval)
	case "weekly":
		if len(rule.Byweekday) == 0 {
			return addDays(from, interval*7)
		}
		candidate := addDays(from, 1)
		if interval > 1 {
			candidate = addDays(from, (interval-1)*7+1)
		}
		allowed := map[int]bool{}
		for _, d := range rule.Byweekday {
			allowed[d] = true
		}
		for i := 0; i < 14; i++ {
			if allowed[jsWeekday(candidate)] {
				return candidate
			}
			candidate = addDays(candidate, 1)
		}
		return candidate
	case "monthly":
		return addMonths(from, interval)
	default: // yearly
		return addYears(from, interval)
	}
}

// nextOccurrenceOnOrAfter:从 seed start_at 起步行,返回 ≥ after 的第一个
// 槽位;无循环时唯一槽位是 start_at 本身。终止:until 之前 / count 耗尽
// / 5000 步防御上限(对齐 calendar.ts,坏规则不许拖死 tick)。
func nextOccurrenceOnOrAfter(startAt time.Time, rule *recurrenceRule, after time.Time) (time.Time, bool) {
	if rule == nil {
		if !startAt.Before(after) {
			return startAt, true
		}
		return time.Time{}, false
	}
	var until time.Time // 零值 = 无界(JS until 为 NaN 时比较恒假,等价)
	if rule.Until != "" {
		if u, ok := parseISO(rule.Until); ok {
			until = u
		}
	}
	maxCount := math.MaxInt32
	if rule.Count != nil && *rule.Count > 0 {
		maxCount = *rule.Count
	}
	current := startAt
	fired := 1 // start_at 记为第 1 次(JS 同)
	for i := 0; i < 5000; i++ {
		if !until.IsZero() && current.After(until) {
			return time.Time{}, false
		}
		if fired > maxCount {
			return time.Time{}, false
		}
		if !current.Before(after) {
			return current, true
		}
		current = stepOnce(current, rule)
		fired++
	}
	return time.Time{}, false
}

// parseISO:recurrence.until 的宽容解析(RFC3339 → 纯日期按 UTC),失败
// ok=false(调用方按无界处理)。
func parseISO(s string) (time.Time, bool) {
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
	return time.Time{}, false
}

/* ───────────────────────── 提醒窗口判定 ───────────────────────── */

// reminderSlot:事件在 now 时刻唯一应触发的提醒槽位(occurrence),及其
// 窗口 [occurrence-lead, occurrence) 是否包含 now。
//
// TS tickCalendar 的判定是「slot = 下一个未处理槽位(now ∈ [slot-lead,
// slot) 则发)」,slot 由 dispatch 循环推进 last_fired_at 间接前进;Go 侧
// dispatch 半边未移植、last_fired_at 不自动前进,故改写为直接枚举:取
// 「第一个 > now 的槽位 O」——窗口已关的槽位(O ≤ now,TS 里也不会再发,
// 唯一键同样拦住)自然跳过,O 的窗口开着即命中。one-shot 语义与 TS 完全
// 一致;循环事件等价于「每个槽位各提醒一次」,落库去重仍按
// (event_id, scheduled_for) 逐槽位一行。
func reminderSlot(startAt time.Time, rule *recurrenceRule, lead time.Duration, now time.Time) (time.Time, bool) {
	// after = now+1ns:严格排除 O == now(窗口上界开区间,TS now < slot)。
	slot, ok := nextOccurrenceOnOrAfter(startAt, rule, now.Add(time.Nanosecond))
	if !ok {
		return time.Time{}, false
	}
	return slot, !now.Before(slot.Add(-lead))
}

// reminderLeadMinutes:载荷里的 leadMinutes(对齐 TS
// Math.max(0, Math.round((occurrence-now)/60000));正数域 JS/Go 的
// half-up 取整一致)。
func reminderLeadMinutes(occurrence, now time.Time) int {
	ms := occurrence.Sub(now).Milliseconds()
	m := int(math.Round(float64(ms) / 60000))
	if m < 0 {
		return 0
	}
	return m
}

/* ───────────────────────── 收件人解析 ───────────────────────── */

// reminderRecipient:一个 (用户 id, 可选 email)。toast 用 id,email 用地址。
type reminderRecipient struct {
	userID string
	email  sql.NullString
}

// resolveReminderRecipients:对齐 resolveReminderRecipients——creator 恒在
// (Set 插入序:creator 先),assignee 仅当是 human participant(agent 有
// dispatch,不要提前 toast);再批量取 users.email(无地址 → NULL,仅
// toast 可达)。users.id == human participants.id。
func (s *S) resolveReminderRecipients(ctx context.Context, e calendar.EventRow) []reminderRecipient {
	ids := []string{e.CreatedBy}
	seen := map[string]bool{e.CreatedBy: true}
	if e.AssigneeID.Valid {
		var isHuman bool
		if err := s.DB.QueryRowContext(ctx,
			`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 AND kind = 'human' LIMIT 1`,
			e.AssigneeID.String, e.CompanyID).Scan(&isHuman); err == nil && isHuman && !seen[e.AssigneeID.String] {
			seen[e.AssigneeID.String] = true
			ids = append(ids, e.AssigneeID.String)
		}
	}
	emails := map[string]sql.NullString{}
	if rows, err := s.DB.QueryContext(ctx,
		`SELECT id, email FROM users WHERE id = ANY($1::text[])`, ids); err == nil {
		for rows.Next() {
			var id string
			var mail sql.NullString
			if rows.Scan(&id, &mail) == nil {
				emails[id] = mail
			}
		}
		rows.Close()
	}
	out := make([]reminderRecipient, 0, len(ids))
	for _, id := range ids {
		out = append(out, reminderRecipient{userID: id, email: emails[id]})
	}
	return out
}

/* ───────────────────────── 单条提醒发送 ───────────────────────── */

// reminderPayload:ChCalendarReminder 载荷,字段与命名逐个对齐消费端契约
// (apps/web/src/api/client.ts 的 calendar.reminder 事件类型 + 已退役 TS
// 的 publish 字面量)。companyId 必带——wsx 桥按它过滤租户,缺键拒路由。
type reminderPayload struct {
	Type             string   `json:"type"`
	CompanyID        string   `json:"companyId"`
	EventID          string   `json:"eventId"`
	Title            string   `json:"title"`
	OccurrenceAt     string   `json:"occurrenceAt"`
	LeadMinutes      int      `json:"leadMinutes"`
	RecipientUserIDs []string `json:"recipientUserIds"`
	Kind             string   `json:"kind"`
	AssigneeID       *string  `json:"assigneeId"`
}

// emailReminderConfigured:email 出站门控。TS 只看 EMAIL_DOMAIN(其
// sendViaProvider 未配 RESEND_API_KEY 时内部 mock);Go 的平价是仅当
// Resend 真已配置才出网——RESEND_API_KEY 空(=mock)或 EMAIL_DOMAIN 空
// (拼不出 reminders@ 发件地址)都静默跳过 email,toast 不受影响(#209
// 对齐 TS:toast 恒发、email 按配置)。
func emailReminderConfigured() bool {
	return os.Getenv("RESEND_API_KEY") != "" && core.RootDomain() != ""
}

// sendCalendarReminder:对齐 sendReminder。先占 (event_id, scheduled_for)
// 槽(INSERT … ON CONFLICT DO NOTHING RETURNING;唯一键吸收并发 tick/
// 副本/重启重发,抢不到即 duplicate 收场——选 claim-first 而非事务,与
// 域内 DispatchEvent 的占位形态同构,抢占即天然互斥),再 fan-out,最后
// 回写 recipients/status/error。
func (s *S) sendCalendarReminder(ctx context.Context, e calendar.EventRow, occurrence, now time.Time) string {
	reminderID := newReminderID()
	var claimed string
	err := s.DB.QueryRowContext(ctx, `
		INSERT INTO calendar_reminders (id, event_id, company_id, scheduled_for, channel, recipients, status)
		VALUES ($1,$2,$3,$4,$5,'[]'::jsonb,'sent')
		ON CONFLICT (event_id, scheduled_for) DO NOTHING
		RETURNING id`,
		reminderID, e.ID, e.CompanyID, occurrence, e.ReminderChannel.String).Scan(&claimed)
	if err != nil {
		if err == sql.ErrNoRows {
			return "duplicate" // 他者已持有本槽位(并发 tick/副本/重启前发过)
		}
		slog.Warn("[calendar] reminder claim failed", "event", e.ID, "err", err)
		return "failed"
	}

	recipients := s.resolveReminderRecipients(ctx, e)
	if len(recipients) == 0 {
		_, _ = s.DB.ExecContext(ctx,
			`UPDATE calendar_reminders SET status = 'skipped' WHERE id = $1`, reminderID)
		return "skipped"
	}
	leadMinutes := reminderLeadMinutes(occurrence, now)
	channel := e.ReminderChannel.String
	recipientIDs := make([]string, 0, len(recipients))
	for _, r := range recipients {
		recipientIDs = append(recipientIDs, r.userID)
	}

	var errs []string
	// toast:恒走 Redis 广播(桥扇出到全公司,前端按 recipientUserIds 过滤)。
	if channel == "toast" || channel == "both" {
		var assignee *string
		if e.AssigneeID.Valid {
			assignee = &e.AssigneeID.String
		}
		payload, _ := json.Marshal(reminderPayload{
			Type: "calendar.reminder", CompanyID: e.CompanyID, EventID: e.ID,
			Title: e.Title, OccurrenceAt: httpx.ISOms(occurrence),
			LeadMinutes: leadMinutes, RecipientUserIDs: recipientIDs,
			Kind: e.Kind, AssigneeID: assignee,
		})
		if err := events.PublishRaw(ctx, events.ChCalendarReminder, payload); err != nil {
			errs = append(errs, "toast: "+err.Error())
		}
	}
	// email:一地址一封;无地址的收件人跳过(toast 仍可达),不因此失败。
	if channel == "email" || channel == "both" {
		if emailReminderConfigured() {
			dom := core.RootDomain()
			subject := sanitizeReminderSubject(e.Title, leadMinutes)
			for _, r := range recipients {
				if !r.email.Valid || r.email.String == "" {
					continue
				}
				res := core.SendViaProvider(ctx, core.SendArgs{
					From:          core.FormatAddress("reminders@"+dom, "Cumora Calendar"),
					To:            []string{r.email.String},
					Subject:       subject,
					Text:          renderReminderText(e, occurrence, leadMinutes),
					HTML:          renderReminderHtml(e, occurrence, leadMinutes),
					MessageID:     core.MintMessageId(),
					AutoSubmitted: "auto-generated",
				})
				if !res.OK && res.Error != "" {
					errs = append(errs, fmt.Sprintf("email[%s]: %s", r.email.String, res.Error))
				}
			}
		}
	}

	status := "sent"
	var errText any
	if len(errs) > 0 {
		status = "failed"
		errText = strings.Join(errs, "; ")
	}
	recJSON, _ := json.Marshal(recipientIDs)
	_, _ = s.DB.ExecContext(ctx, `
		UPDATE calendar_reminders
		   SET recipients = $2::jsonb, status = $3, error = $4
		 WHERE id = $1`, reminderID, string(recJSON), status, errText)
	return status
}

func newReminderID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "cr-" + hex.EncodeToString(b)
}

/* ───────────────────────── email 渲染(对齐 TS 模板) ───────────────────────── */

// ctrlRe:TS sanitizeReminderSubject 的字符类 [\r\n\t\x00-\x1f]——控制字符
// 全量换空格,阻断 header 注入。
func sanitizeReminderTitle(title string) string {
	var b strings.Builder
	for _, r := range title {
		if r == '\r' || r == '\n' || r == '\t' || r <= 0x1f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return httpx.UTF16Cap(b.String(), 160) // JS slice(0,160) 码元语义
}

func sanitizeReminderSubject(title string, leadMinutes int) string {
	t := sanitizeReminderTitle(title)
	if leadMinutes <= 1 {
		return "Starting now: " + t
	}
	if leadMinutes < 60 {
		return fmt.Sprintf("In %d min: %s", leadMinutes, t)
	}
	h := int(math.Round(float64(leadMinutes) / 60))
	plural := "s"
	if h == 1 {
		plural = ""
	}
	return fmt.Sprintf("In %d hour%s: %s", h, plural, t)
}

// jsUTCString:occurrence.toUTCString() 平价("Mon, 02 Jan 2006 15:04:05 GMT")。
func jsUTCString(t time.Time) string { return t.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") }

func renderReminderText(e calendar.EventRow, occurrence time.Time, leadMinutes int) string {
	plural := "s"
	if leadMinutes == 1 {
		plural = ""
	}
	lines := []string{
		fmt.Sprintf("Heads-up — %q is coming up.", e.Title),
		"",
		fmt.Sprintf("When: %s (in ~%d minute%s)", jsUTCString(occurrence), leadMinutes, plural),
	}
	if e.Description.Valid && e.Description.String != "" {
		lines = append(lines, "", e.Description.String)
	}
	if e.Kind == "agent_task" && e.AssigneeID.Valid {
		lines = append(lines, "", "Will be handed off to: @"+e.AssigneeID.String)
	}
	lines = append(lines, "", "—", "Cumora Calendar")
	return strings.Join(lines, "\n")
}

// htmlEsc:TS 模板的 esc()——仅 & < > "(不含单引号,平价优先)。
func htmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func renderReminderHtml(e calendar.EventRow, occurrence time.Time, leadMinutes int) string {
	title := htmlEsc(e.Title)
	when := htmlEsc(jsUTCString(occurrence))
	lead := fmt.Sprintf("%d minutes", leadMinutes)
	if leadMinutes == 1 {
		lead = "1 minute"
	}
	desc := ""
	if e.Description.Valid && e.Description.String != "" {
		desc = `<p style="color:#5B7186">` + htmlEsc(e.Description.String) + `</p>`
	}
	assign := ""
	if e.Kind == "agent_task" && e.AssigneeID.Valid {
		assign = `<p style="color:#5B7186">Will be handed off to <b>@` + htmlEsc(e.AssigneeID.String) + `</b>.</p>`
	}
	return `<!doctype html><html><body style="font-family:-apple-system,sans-serif;background:#FAFCFE;padding:24px">
    <div style="max-width:520px;margin:0 auto;background:#FFFFFF;border:1px solid #E5ECF2;border-radius:14px;padding:24px">
      <div style="color:#0078C8;font-size:11px;text-transform:uppercase;letter-spacing:0.08em;font-weight:700">Reminder · in ` + lead + `</div>
      <h1 style="margin:6px 0 12px;font-size:20px;color:#0A1B2E">` + title + `</h1>
      <p style="color:#5B7186;margin:0 0 4px">` + when + ` UTC</p>
      ` + desc + `
      ` + assign + `
      <hr style="border:none;border-top:1px solid #E5ECF2;margin:24px 0 12px"/>
      <p style="color:#94A8BC;font-size:11px;margin:0">Cumora Calendar</p>
    </div></body></html>`
}

/* ───────────────────────── tick 与调度 ───────────────────────── */

// calendarReminderIntervalMS:envIntRaw 语义(TS 家族「0=禁用」须原样
// 透传,envIntOr 的 0→默认会吞掉 kill-switch,#62 教训);默认 60s 对齐
// TS TICK_INTERVAL_MS。
func calendarReminderIntervalMS() int64 {
	if n, ok := envIntRaw("CALENDAR_REMINDER_INTERVAL_MS"); ok {
		return n
	}
	return 60_000
}

// RunCalendarReminderTick:一轮扫描(导出供测试/手动触发)。对齐 TS
// tickCalendar 的 reminder 半边:只取 active 且双置 reminder 的事件(无
// start_at 过滤——提醒窗口在 start_at 之前就开);逐事件算槽位,窗口
// 命中即发。逐条 panic 隔离,单事件异常不拖垮整轮。
func (s *S) RunCalendarReminderTick(ctx context.Context) int {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, company_id, created_by, kind, title, description, assignee_id,
		       target_conversation_id, agent_prompt, start_at, end_at, all_day,
		       recurrence, status, last_fired_at,
		       reminder_minutes_before, reminder_channel, is_private,
		       created_at, updated_at
		  FROM calendar_events
		 WHERE status = 'active'
		   AND reminder_minutes_before IS NOT NULL
		   AND reminder_channel IS NOT NULL`)
	if err != nil {
		slog.Warn("[calendar] reminder scan failed", "err", err)
		return 0
	}
	var events []calendar.EventRow
	for rows.Next() {
		if e, ok := calendar.ScanEventRow(rows); ok {
			events = append(events, e)
		}
	}
	rows.Close()

	reminded := 0
	for _, e := range events {
		func(e calendar.EventRow) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Warn("[calendar] reminder event panicked", "event", e.ID, "recover", rec)
				}
			}()
			// 双置由列约束保证(NOT NULL 过滤已保证非空);lead 按列值。
			// now 单点取值:窗口判定与 leadMinutes 共用同一时刻(TS tick 同)。
			now := time.Now()
			lead := time.Duration(e.ReminderMinutes.Int64) * time.Minute
			occurrence, due := reminderSlot(e.StartAt, parseRecurrenceRule(e.Recurrence), lead, now)
			if !due {
				return
			}
			if s.sendCalendarReminder(ctx, e, occurrence, now) == "sent" {
				reminded++
			}
		}(e)
	}
	return reminded
}

// StartCalendarReminderScheduler:周期 tick;ENABLE_CALENDAR_REMINDER
// ='false' 或 CALENDAR_REMINDER_INTERVAL_MS<=0 关闭(门控风格对齐
// ENABLE_*/INTERVAL 家族)。#215 形态:select{ctxBG.Done, ticker.C} +
// tick 级 panic 隔离,cancelBoot 即停。
func (s *S) StartCalendarReminderScheduler() {
	if getenv("ENABLE_CALENDAR_REMINDER") == "false" {
		return
	}
	interval := calendarReminderIntervalMS()
	if interval <= 0 {
		slog.Info("[calendar] reminder scheduler disabled (CALENDAR_REMINDER_INTERVAL_MS=0)")
		return
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctxBG.Done():
				return
			case <-ticker.C:
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							slog.Error("[calendar] reminder tick panicked", "recover", rec)
						}
					}()
					if n := s.RunCalendarReminderTick(ctxBG); n > 0 {
						slog.Info("[calendar] reminder tick sent", "count", n)
					}
				}()
			}
		}
	}()
	slog.Info("[calendar] reminder scheduler running", "interval_ms", interval)
}
