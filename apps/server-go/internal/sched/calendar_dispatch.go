// sched 包 calendar_dispatch —— #263 例行事务的 dispatch 半边:到点的
// agent_task 日历事件(循环事件挂 runbook)由本 worker 周期投递并唤醒
// 受派 agent。投递本体复用 domains/calendar.DispatchEvent(TS 平价:占
// (event_id, scheduled_for) 槽位 + 目标会话系统消息 + dispatch 行),
// 本文件只补 #209 自认缺失的「周期扫描 + 唤醒」:
//
//	slot 判定 = 该事件 ≤ now 的最近一次 occurrence,且迟到 ≤ 5 分钟
//	(grilling #263:迟到 >5min 不补跑,等下一周期);occurrence 数学
//	复用 calendar_reminder.go 的 stepOnce 家族;幂等由 DispatchEvent 的
//	槽位唯一键承担(重启/多副本重扫 = duplicate)。
package sched

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/calendar"
)

// dispatchLateGrace:迟到容忍窗(grilling #263:>5min 不补跑)。
const dispatchLateGrace = 5 * time.Minute

// dispatchSlot:≤ now 的最近 occurrence 及其是否仍在迟到容忍窗内。
// 无循环:唯一槽位 start_at。有循环:自 start_at 逐步推进,保留最后
// 一个 ≤ now 的槽位(终止条件与 nextOccurrenceOnOrAfter 同族:until/
// count/5000 步防御)。
func dispatchSlot(startAt time.Time, rule *recurrenceRule, now time.Time) (time.Time, bool) {
	if rule == nil {
		if startAt.After(now) {
			return time.Time{}, false
		}
		return startAt, now.Sub(startAt) <= dispatchLateGrace
	}
	var until time.Time
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
	fired := 1
	last := time.Time{}
	for i := 0; i < 5000; i++ {
		if !until.IsZero() && current.After(until) {
			break
		}
		if fired > maxCount {
			break
		}
		if current.After(now) {
			break
		}
		last = current
		current = stepOnce(current, rule)
		fired++
	}
	if last.IsZero() {
		return time.Time{}, false
	}
	return last, now.Sub(last) <= dispatchLateGrace
}

// RunCalendarDispatchTick:一轮扫描投递。返回本轮成功 dispatch 的事件数。
func (s *S) RunCalendarDispatchTick(ctx context.Context) int {
	now := time.Now()
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, company_id, created_by, kind, title, description, assignee_id,
		       target_conversation_id, agent_prompt, start_at, end_at, all_day,
		       recurrence, status, last_fired_at,
		       reminder_minutes_before, reminder_channel, is_private,
		       created_at, updated_at
		  FROM calendar_events
		 WHERE status = 'active' AND kind = 'agent_task' AND assignee_id IS NOT NULL
		   AND start_at <= $1`, now)
	if err != nil {
		slog.Warn("[calendar] dispatch scan failed", "err", err)
		return 0
	}
	var events []calendar.EventRow
	for rows.Next() {
		if e, ok := calendar.ScanEventRow(rows); ok {
			events = append(events, e)
		}
	}
	rows.Close()

	dispatched := 0
	for _, e := range events {
		func(e calendar.EventRow) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Warn("[calendar] dispatch event panicked", "event", e.ID, "recover", rec)
				}
			}()
			slot, due := dispatchSlot(e.StartAt, parseRecurrenceRule(e.Recurrence), now)
			if !due {
				return
			}
			res := calendar.DispatchEvent(ctx, s.DB, e, slot)
			if res.Status != "dispatched" {
				return // duplicate(幂等命中)/skipped/failed:failed 留痕于 dispatch 行,不唤醒
			}
			dispatched++
			// 唤醒受派 agent:例行事务到点,带明确原因(daemon turnDelta 消费
			// reason;runbook 正文已在投递的系统消息里,inbox 读取链路自带)。
			assignee := e.AssigneeID.String
			title := truncateRunesSched(e.Title, 60)
			s.WakeOne(assignee, "calendar-due: "+title, nil, nil, nil)
		}(e)
	}
	return dispatched
}

func truncateRunesSched(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// StartCalendarDispatchScheduler:周期 tick;ENABLE_CALENDAR_DISPATCH=
// 'false' 或 CALENDAR_DISPATCH_INTERVAL_MS<=0 关闭(门控风格对齐
// ENABLE_*/INTERVAL 家族)。
func (s *S) StartCalendarDispatchScheduler() {
	if config.Getenv("ENABLE_CALENDAR_DISPATCH") == "false" {
		return
	}
	interval := calendarDispatchIntervalMS()
	if interval <= 0 {
		slog.Info("[calendar] dispatch scheduler disabled (CALENDAR_DISPATCH_INTERVAL_MS=0)")
		return
	}
	RunWorkerLoop(ctxBG, interval, "[calendar] dispatch", func(ctx context.Context) {
		if n := s.RunCalendarDispatchTick(ctx); n > 0 {
			slog.Info("[calendar] dispatch tick delivered", "count", n)
		}
	})
	slog.Info("[calendar] dispatch scheduler running", "interval_ms", interval)
}

func calendarDispatchIntervalMS() int64 {
	if n, ok := config.EnvIntRaw("CALENDAR_DISPATCH_INTERVAL_MS"); ok {
		return n
	}
	return 60_000
}
