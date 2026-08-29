package runtime

import (
	"strings"
	"testing"
)

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
