// scheduler 纯函数单测(#62):静音投递契约的边界表(TS 正则
// `(^|[^\w@])@id(?![\w-])`/i 的手工扫描等价)+ 低优先级预算窗。
// 仅纯逻辑,无 DB/Redis 依赖。
package runtime

import (
	"strings"
	"testing"
)

func TestShouldDeliverToMutedAgent(t *testing.T) {
	agentID := "ag-rob1"
	other := "ag-other"
	quotedSelf := agentID
	quotedOther := other
	cases := []struct {
		name   string
		kind   string
		body   string
		quoted *string
		want   bool
	}{
		{"direct always delivers", "direct", "anything", nil, true},
		{"quote-reply to own message", "group", "re: this", &quotedSelf, true},
		{"quote-reply to peer message", "group", "re: this", &quotedOther, false},
		{"plain group chatter stays muted", "group", "hello team", nil, false},
		{"exact mention delivers", "group", "hey @ag-rob1 look", nil, true},
		{"mention mid-sentence delivers", "group", "ping @ag-rob1 please", nil, true},
		{"mention start of body delivers", "group", "@ag-rob1 first", nil, true},
		{"mention punctuation boundary delivers", "group", "(@ag-rob1)", nil, true},
		{"mention case-insensitive delivers", "group", "hey @AG-ROB1 look", nil, true},
		{"longer id is not a mention", "group", "hey @ag-rob12 look", nil, false},
		{"prefix word char blocks mention", "group", "email ag@ag-rob1 nope", nil, false},
		{"double-at blocks mention", "group", "hey @@ag-rob1 nope", nil, false},
		{"mention followed by hyphen blocks", "group", "hey @ag-rob1-2 nope", nil, false},
		{"substring id is not a mention", "group", "hey @ag-rob nope", nil, false},
		{"mention without leading space after newline delivers", "group", "line1\n@ag-rob1 line2", nil, true},
		{"mention at end of body delivers", "group", "hey @ag-rob1", nil, true},
		{"non-ASCII char before mention is a valid boundary (İ)", "group", "\u0130@ag-rob1 look", nil, true},
		{"non-ASCII char before mention is a valid boundary (K)", "group", "\u212a@ag-rob1 look", nil, true},
		{"CJK char before mention is a valid boundary", "group", "你好@ag-rob1 看看", nil, true},
		{"lowercase fold mention delivers", "group", "hey @ag-Rob1 look", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldDeliverToMutedAgent(agentID, tc.kind, tc.body, tc.quoted)
			if got != tc.want {
				t.Fatalf("ShouldDeliverToMutedAgent(%q, %q) = %v, want %v", tc.kind, tc.body, got, tc.want)
			}
		})
	}
}

func TestConsumeLowPriorityWakeBudget(t *testing.T) {
	ResetLowPriorityWakeBudgetForTests()
	for i := 0; i < lowPriorityWakeBudgetPerMin; i++ {
		if !consumeLowPriorityWakeBudget() {
			t.Fatalf("wake %d within budget was dropped", i+1)
		}
	}
	if consumeLowPriorityWakeBudget() {
		t.Fatal("wake beyond budget was allowed")
	}
	if consumeLowPriorityWakeBudget() {
		t.Fatal("second over-budget wake was allowed")
	}
}

// #94:UTF-16 码元截断族 —— 长 CJK/代理对用例(TS .slice(n) 按 UTF-16
// 码元,字节截/rune 截均漂移)。
func TestSliceUTF16(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{strings.Repeat("汉", 100), 80, strings.Repeat("汉", 80)},
		{strings.Repeat("汉", 100) + "x", 81, strings.Repeat("汉", 81)},
		// 代理对:emoji 在 TS 计 2 码元,80 码元内只放 40 个。
		{strings.Repeat("🙂", 80), 80, strings.Repeat("🙂", 40)},
		// 边界:80 个 emoji 全留在 80 码元限下 = 恰好 40;多 1 即截去。
		{strings.Repeat("🙂", 41), 80, strings.Repeat("🙂", 40)},
		{"", 10, ""},
	}
	for i, tc := range cases {
		if got := sliceUTF16(tc.in, tc.n); got != tc.want {
			t.Fatalf("case %d: sliceUTF16(%q, %d) = %q, want %q", i, tc.in, tc.n, got, tc.want)
		}
	}
}

// 长 CJK 主题的 dedup 键不再过度相撞(#94 的真动机:80 字节截断时
// 两个 81 汉字的主题会塌到同一键拒掉第二个 claim,TS 侧不会)。
func TestNormalizeWorkSubjectCJK(t *testing.T) {
	a := strings.Repeat("汉", 79) + "A"
	b := strings.Repeat("汉", 79) + "B"
	if NormalizeWorkSubject(a) == NormalizeWorkSubject(b) {
		t.Fatalf("distinct 80-码元 CJK subjects collapsed to the same dedup key")
	}
	if got := NormalizeWorkSubject(strings.Repeat("汉", 90)); len(got) == 0 {
		t.Fatal("normalize emptied a CJK subject")
	}
}
