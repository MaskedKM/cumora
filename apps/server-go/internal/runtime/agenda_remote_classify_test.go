package runtime

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

// hashStrJS 必须按 UTF-16 code unit 计(FNV-1a)——非 BMP 字符占 2 单元。
func TestHashStrJS(t *testing.T) {
	if hashStrJS("iris") == hashStrJS("bram") {
		t.Fatal("distinct ids must hash differently")
	}
	// "😀" = 2 个 UTF-16 单元;与逐字节实现必须不同(手算校验一次)。
	if hashStrJS("😀") == hashStrJS("\U0001F600"[:1]) {
		t.Fatal("surrogate pair handling broken")
	}
	// 同输入稳定。
	if hashStrJS("cumora") != hashStrJS("cumora") {
		t.Fatal("hash must be deterministic")
	}
}

// visualSignatureFor:同 id+gender 稳定;gender 分池必须给出不同呈现。
func TestVisualSignatureFor(t *testing.T) {
	a := visualSignatureFor("iris", genderFeminine)
	b := visualSignatureFor("iris", genderFeminine)
	if a != b {
		t.Fatal("same id+gender must produce the same signature")
	}
	m := visualSignatureFor("iris", genderMasculine)
	if a.Presentation == m.Presentation && a.Wardrobe == m.Wardrobe {
		t.Fatal("gender pools must stratify presentation/wardrobe")
	}
	if a.Gender != genderFeminine || m.Gender != genderMasculine {
		t.Fatal("gender must round-trip on the signature")
	}
}
