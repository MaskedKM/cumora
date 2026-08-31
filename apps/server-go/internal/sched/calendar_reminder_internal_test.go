// calendar_reminder 纯函数单测(#209):循环槽位数学、提醒窗口判定
// (TS tickCalendar 语义的 Go 投影)、leadMinutes 取整与主题清洗。
// 仅纯逻辑,无 DB/Redis 依赖。
package sched

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, ok := parseISO(s)
	if !ok {
		t.Fatalf("bad time literal %q", s)
	}
	return ts
}

func TestNextOccurrenceOnOrAfter(t *testing.T) {
	seed := mustTime(t, "2026-01-01T09:00:00Z")
	interval := 1
	daily := &recurrenceRule{Freq: "daily", Interval: interval}

	cases := []struct {
		name   string
		seed   time.Time
		rule   *recurrenceRule
		after  time.Time
		want   string // "" = 无槽位(序列终止)
		exists bool
	}{
		{"one-shot future", seed, nil, seed.Add(-time.Hour), "2026-01-01T09:00:00Z", true},
		{"one-shot past", seed, nil, seed.Add(time.Second), "", false},
		{"one-shot at after", seed, nil, seed, "2026-01-01T09:00:00Z", true},
		{"daily next", seed, daily, seed.Add(time.Second), "2026-01-02T09:00:00Z", true},
		{"daily same", seed, daily, seed, "2026-01-01T09:00:00Z", true},
		{"interval 2 skips a day", seed, &recurrenceRule{Freq: "daily", Interval: 2}, seed.Add(time.Second), "2026-01-03T09:00:00Z", true},
		{"weekly plain hops 7d", seed, &recurrenceRule{Freq: "weekly", Interval: 1}, seed.Add(time.Second), "2026-01-08T09:00:00Z", true},
		// 2026-01-01 是周四;byweekday=[周一,周五] → 下一槽 1/2(周五),
		// 再下一槽 1/5(周一)。
		{"weekly byweekday picks friday", seed, &recurrenceRule{Freq: "weekly", Interval: 1, Byweekday: []int{1, 5}}, seed.Add(time.Second), "2026-01-02T09:00:00Z", true},
		{"weekly byweekday then monday", mustTime(t, "2026-01-02T09:00:00Z"), &recurrenceRule{Freq: "weekly", Interval: 1, Byweekday: []int{1, 5}}, mustTime(t, "2026-01-02T09:00:01Z"), "2026-01-05T09:00:00Z", true},
		{"monthly rolls date", mustTime(t, "2026-01-31T09:00:00Z"), &recurrenceRule{Freq: "monthly", Interval: 1}, mustTime(t, "2026-01-31T09:00:01Z"), "2026-03-03T09:00:00Z", true},
		{"yearly steps year", seed, &recurrenceRule{Freq: "yearly", Interval: 1}, seed.Add(time.Second), "2027-01-01T09:00:00Z", true},
		// until 含下一个槽位 → 仍发;until 早于下一槽位 → 终止。
		{"until admits slot", seed, &recurrenceRule{Freq: "daily", Interval: 1, Until: "2026-01-02T09:00:00Z"}, seed.Add(time.Second), "2026-01-02T09:00:00Z", true},
		{"until ends series", seed, &recurrenceRule{Freq: "daily", Interval: 1, Until: "2026-01-01T10:00:00Z"}, seed.Add(time.Second), "", false},
		// count 含 seed(=1):count=2 允许第二个槽位;count=1 只剩 seed,
		// seed 过后即终止。
		{"count admits second", seed, &recurrenceRule{Freq: "daily", Interval: 1, Count: intPtr(2)}, seed.Add(time.Second), "2026-01-02T09:00:00Z", true},
		{"count exhausted", seed, &recurrenceRule{Freq: "daily", Interval: 1, Count: intPtr(1)}, seed.Add(time.Second), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := nextOccurrenceOnOrAfter(c.seed, c.rule, c.after)
			if ok != c.exists {
				t.Fatalf("exists = %v, want %v", ok, c.exists)
			}
			if ok && !got.Equal(mustTime(t, c.want)) {
				t.Fatalf("slot = %s, want %s", got.Format(time.RFC3339), c.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func TestReminderSlotWindow(t *testing.T) {
	start := mustTime(t, "2026-08-31T12:00:00Z")
	lead := 10 * time.Minute
	now := func(s string) time.Time { return mustTime(t, s) }

	// one-shot:窗口 [11:50, 12:00)。
	cases := []struct {
		name string
		now  time.Time
		rule *recurrenceRule
		want bool
		slot string
	}{
		{"before window", now("2026-08-31T11:49:59Z"), nil, false, ""},
		{"window opens at lead edge", now("2026-08-31T11:50:00Z"), nil, true, "2026-08-31T12:00:00Z"},
		{"inside window", now("2026-08-31T11:55:00Z"), nil, true, "2026-08-31T12:00:00Z"},
		{"window closed at slot", now("2026-08-31T12:00:00Z"), nil, false, ""},
		{"window closed after slot", now("2026-08-31T18:00:00Z"), nil, false, ""},
		// 循环(daily):第一槽已过,第二槽窗口 [次日 11:50, 12:00) 命中
		// ——Go 无 dispatch 循环,last_fired_at 不前进,槽位直接枚举。
		{"recurring second occurrence window", now("2026-09-01T11:55:00Z"), &recurrenceRule{Freq: "daily", Interval: 1}, true, "2026-09-01T12:00:00Z"},
		{"recurring before second window", now("2026-09-01T11:00:00Z"), &recurrenceRule{Freq: "daily", Interval: 1}, false, ""},
		{"recurring between slots", now("2026-09-01T12:00:00Z"), &recurrenceRule{Freq: "daily", Interval: 1}, false, ""},
		// 提前量为 0:窗口退化为空集,永不触发(TS now < slot 恒真边界)。
		{"zero lead never fires", now("2026-08-31T11:59:59Z"), nil, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := lead
			if c.name == "zero lead never fires" {
				l = 0
			}
			slot, due := reminderSlot(start, c.rule, l, c.now)
			if due != c.want {
				t.Fatalf("due = %v, want %v", due, c.want)
			}
			if due && !slot.Equal(now(c.slot)) {
				t.Fatalf("slot = %s, want %s", slot.Format(time.RFC3339), c.slot)
			}
		})
	}
}

func TestReminderLeadMinutes(t *testing.T) {
	now := mustTime(t, "2026-08-31T12:00:00Z")
	cases := []struct {
		name       string
		occurrence time.Time
		want       int
	}{
		{"exact 10 min", now.Add(10 * time.Minute), 10},
		{"half rounds up", now.Add(90*time.Second + 500*time.Millisecond), 2},
		{"clamped at zero", now.Add(-5 * time.Minute), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reminderLeadMinutes(c.occurrence, now); got != c.want {
				t.Fatalf("leadMinutes = %d, want %d", got, c.want)
			}
		})
	}
}

func TestSanitizeReminderSubject(t *testing.T) {
	if got := sanitizeReminderSubject("Standup\n\r\t", 0); got != "Starting now: Standup   " {
		t.Fatalf("control chars not sanitized: %q", got)
	}
	if got := sanitizeReminderSubject("Sync", 30); got != "In 30 min: Sync" {
		t.Fatalf("minutes branch = %q", got)
	}
	if got := sanitizeReminderSubject("Review", 90); got != "In 2 hours: Review" {
		t.Fatalf("hours branch = %q", got)
	}
	if got := sanitizeReminderSubject("One", 60); got != "In 1 hour: One" {
		t.Fatalf("hour singular = %q", got)
	}
}
