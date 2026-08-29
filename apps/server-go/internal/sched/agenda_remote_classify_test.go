package sched

import (
	"testing"
)

// ParseAgendaVerdict 与 TS agenda-triage-core.test.ts 的关键案例对齐。
func TestParseAgendaVerdict(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *AgendaParsedVerdict
	}{
		{"plain", `{"actionable":true,"focus":"card-1","reason":"due"}`,
			&AgendaParsedVerdict{Actionable: true, Focus: "card-1", Reason: "due"}},
		{"fenced", "```json\n{\"actionable\":false,\"focus\":\"\",\"reason\":\"quiet\"}\n```",
			&AgendaParsedVerdict{Actionable: false, Focus: "", Reason: "quiet"}},
		{"string-no", `{"actionable":"no","focus":"x","reason":"y"}`,
			&AgendaParsedVerdict{Actionable: false, Focus: "x", Reason: "y"}},
		{"string-true", `{"actionable":"true","focus":"f","reason":"r"}`,
			&AgendaParsedVerdict{Actionable: true, Focus: "f", Reason: "r"}},
		{"number-one", `{"actionable":1,"focus":"f","reason":"r"}`,
			&AgendaParsedVerdict{Actionable: true, Focus: "f", Reason: "r"}},
		// 截断的 reason 没有闭引号 —— TS 的 \1 反向引用同样抢救不出,空串。
		{"truncated-salvage", `{"actionable": true, "focus": "finish the deck", "reason": "ow`,
			&AgendaParsedVerdict{Actionable: true, Focus: "finish the deck", Reason: ""}},
		// 无 '{' 直接 null;有 '{' 但 actionable 无 focus → malformed 收窄。
		{"salvage-no-brace", `blah actionable: true, tail`, nil},
		{"salvage-no-focus", `{actionable: true, tail`,
			&AgendaParsedVerdict{Actionable: false, Focus: "", Reason: "malformed positive verdict without focus"}},
		{"garbage", `no verdict here`, nil},
		// TS coerceAgendaVerdict:数值 focus/reason 走 String() 收窄(不是拒收)。
		{"numeric-fields", `{"actionable":true,"focus":123,"reason":456}`,
			&AgendaParsedVerdict{Actionable: true, Focus: "123", Reason: "456"}},
		{"bool-reason", `{"actionable":false,"focus":"","reason":true}`,
			&AgendaParsedVerdict{Actionable: false, Focus: "", Reason: "true"}},
	}
	for _, c := range cases {
		got := ParseAgendaVerdict(c.raw)
		if c.want == nil {
			if got != nil {
				t.Fatalf("%s: want nil, got %+v", c.name, got)
			}
			continue
		}
		if got == nil {
			t.Fatalf("%s: want %+v, got nil", c.name, c.want)
		}
		if *got != *c.want {
			t.Fatalf("%s: want %+v, got %+v", c.name, c.want, got)
		}
	}
}
