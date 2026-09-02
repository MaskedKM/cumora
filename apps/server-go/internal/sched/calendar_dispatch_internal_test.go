package sched

import (
	"testing"
	"time"
)

func dt(s string) time.Time { t, _ := time.Parse(time.RFC3339, s); return t }

// dispatchSlot:one-shot 与循环的到期/迟到窗判定(#263)。
func TestDispatchSlotOneShot(t *testing.T) {
	now := dt("2026-09-02T12:00:00Z")
	// 未到 → 不投。
	if _, due := dispatchSlot(dt("2026-09-02T12:01:00Z"), nil, now); due {
		t.Fatal("future one-shot 不得投递")
	}
	// 刚到 → 投。
	slot, due := dispatchSlot(dt("2026-09-02T11:59:30Z"), nil, now)
	if !due || !slot.Equal(dt("2026-09-02T11:59:30Z")) {
		t.Fatalf("刚到的事件应投: due=%v slot=%v", due, slot)
	}
	// 迟到 >5min → 不补(grilling)。
	if _, due := dispatchSlot(dt("2026-09-02T11:54:59Z"), nil, now); due {
		t.Fatal("迟到 >5min 不得补跑")
	}
	// 迟到 4min → 投。
	if _, due := dispatchSlot(dt("2026-09-02T11:56:00Z"), nil, now); !due {
		t.Fatal("迟到 ≤5min 应补投")
	}
}

func TestDispatchSlotRecurring(t *testing.T) {
	now := dt("2026-09-02T12:00:00Z")
	daily := &recurrenceRule{Freq: "daily", Interval: 1}
	// 09/01 08:00 起每日:最近 ≤now 的槽位是今天 08:00(4min 前?否,4h 前)
	// ——迟到超窗不投;把 start 定在今天 11:58 则投。
	if _, due := dispatchSlot(dt("2026-08-30T08:00:00Z"), daily, now); due {
		t.Fatal("今日槽位已过 4h,迟到超窗不得补")
	}
	slot, due := dispatchSlot(dt("2026-09-02T11:58:00Z"), daily, now)
	if !due || !slot.Equal(dt("2026-09-02T11:58:00Z")) {
		t.Fatalf("今日槽位在容忍窗内应投: due=%v slot=%v", due, slot)
	}
	// 多槽位跨过:09/01 23:00 起每日 → 昨日 23:00 与今日 23:00,now=今
	// 12:00 → 最近 ≤now 是昨日 23:00(超窗);下一槽今日 23:00 未到 → 不投。
	if _, due := dispatchSlot(dt("2026-09-01T23:00:00Z"), daily, now); due {
		t.Fatal("跨槽场景应命中已过窗的槽位而非未来槽")
	}
	// weekly+byweekday:周一槽,now 周三 → 上周一超窗不投。
	weekly := &recurrenceRule{Freq: "weekly", Interval: 1, Byweekday: []int{1}}
	if _, due := dispatchSlot(dt("2026-08-31T09:00:00Z"), weekly, now); due {
		t.Fatal("weekly 上周槽位超窗")
	}
}

func TestSweeperClassOf(t *testing.T) {
	if c := sweeperClassOf("[turn-fail class=network attempts=1] local claude failed"); c != "network" {
		t.Fatalf("network: %q", c)
	}
	if c := sweeperClassOf("[turn-fail class=context-overflow attempts=2] x"); c != "context-overflow" {
		t.Fatalf("context-overflow: %q", c)
	}
	if c := sweeperClassOf("local claude failed (exit 1)"); c != "" {
		t.Fatalf("无标不分类: %q", c)
	}
	if !sweeperRetryableClasses["engine-timeout"] || sweeperRetryableClasses["credential"] {
		t.Fatal("白名单镜像失真")
	}
}

// 循环终止语义(评审 #327 P1-3:count/until/interval>1 与 ==now 边界)。
func TestDispatchSlotTermination(t *testing.T) {
	now := dt("2026-09-02T12:00:00Z")
	// count=2,每日:start 09/01 08:00 → 槽位 09/01、09/02;now 在 09/02
	// 11:56 → 命中第 2 槽(在窗)。
	cnt := 2
	rule := &recurrenceRule{Freq: "daily", Interval: 1, Count: &cnt}
	slot, due := dispatchSlot(dt("2026-09-01T11:58:00Z"), rule, dt("2026-09-02T11:59:00Z"))
	if !due || !slot.Equal(dt("2026-09-02T11:58:00Z")) {
		t.Fatalf("count=2 第二槽应投: due=%v slot=%v", due, slot)
	}
	// count 耗尽:now 在 09/03 → 第三槽不存在 → 不投。
	if _, due := dispatchSlot(dt("2026-09-01T11:58:00Z"), rule, dt("2026-09-03T11:59:00Z")); due {
		t.Fatal("count 耗尽后不得再投")
	}
	// until 截止:until=09/01,now=09/02 → 无有效槽。
	u := &recurrenceRule{Freq: "daily", Interval: 1, Until: "2026-09-01T23:59:00Z"}
	if _, due := dispatchSlot(dt("2026-09-01T11:58:00Z"), u, dt("2026-09-02T11:59:00Z")); due {
		t.Fatal("until 之后不得再投")
	}
	// interval=2(隔日):start 08/31 08:00,now=09/02 11:58 → 槽 09/02 08:00。
	i2 := &recurrenceRule{Freq: "daily", Interval: 2}
	slot, due = dispatchSlot(dt("2026-08-31T11:58:00Z"), i2, dt("2026-09-02T11:59:00Z"))
	if !due || !slot.Equal(dt("2026-09-02T11:58:00Z")) {
		t.Fatalf("interval=2 槽位: due=%v slot=%v", due, slot)
	}
	// slot == now(边界,含端投递)。
	if _, due := dispatchSlot(dt("2026-09-02T12:00:00Z"), nil, now); !due {
		t.Fatal("slot==now 应投(含端)")
	}
	// monthly 步进。
	m := &recurrenceRule{Freq: "monthly", Interval: 1}
	slot, due = dispatchSlot(dt("2026-08-02T11:58:00Z"), m, dt("2026-09-02T11:59:00Z"))
	if !due || !slot.Equal(dt("2026-09-02T11:58:00Z")) {
		t.Fatalf("monthly 槽位: due=%v slot=%v", due, slot)
	}
}
