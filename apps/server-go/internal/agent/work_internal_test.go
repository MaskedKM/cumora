package agent

import (
	"strings"
	"testing"
)

// 长 CJK 主题的 dedup 键不再过度相撞(#94 的真动机:80 字节截断时
// 两个 81 汉字的主题会塌到同一键拒掉第二个 claim,TS 侧不会)。
// (#140 自 runtime/scheduler_internal_test.go 随 worklog 拆入。)
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
