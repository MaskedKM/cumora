package httpx

import "testing"

// 期望值由 node `String(v)` 实测生成(2026-08-28),锁定 ECMAScript 语义。
func TestJSToString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "null"},
		{"empty string", "", ""},
		{"string", "abc", "abc"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"zero", float64(0), "0"},
		{"neg zero", mathNegZero(), "0"},
		{"one", float64(1), "1"},
		{"neg one", float64(-1), "-1"},
		{"one point five", 1.5, "1.5"},
		{"hundred", float64(100), "100"},
		{"int-ish", float64(123456789), "123456789"},
		{"1e20 full digits", 1e20, "100000000000000000000"},
		{"1e21 exponent", 1e21, "1e+21"},
		{"1.5e21 exponent", 1.5e21, "1.5e+21"},
		{"1e-6 decimal", 1e-6, "0.000001"},
		{"1e-7 exponent", 1e-7, "1e-7"},
		{"half", 0.5, "0.5"},
		{"tenth", 0.1, "0.1"},
		{"third shortest round-trip", 1.0 / 3.0, "0.3333333333333333"},
		{"beyond 2^53 rounds", float64(9007199254740993), "9007199254740992"},
		// F-01(评审):(2^53, 1e21) 内精确展开长于最短表示的整数——
		// 必须按最短往返取整,与 JS String() 一致(node 实测)。
		{"19-digit snowflake rounds", float64(1234567890123456789), "1234567890123456800"},
		{"21-digit minus one rounds", float64(999999999999999868928), "999999999999999900000"},
		{"object", map[string]any{}, "[object Object]"},
		{"empty array", []any{}, ""},
		{"array", []any{float64(1), float64(2)}, "1,2"},
		{"array with null", []any{nil, float64(1)}, ",1"},
		{"nested arrays flatten", []any{[]any{float64(1), float64(2)}, float64(3)}, "1,2,3"},
		{"array of object", []any{map[string]any{}}, "[object Object]"},
		{"array mixes bool/string", []any{true, "x"}, "true,x"},
	}
	for _, c := range cases {
		if got := JSToString(c.in); got != c.want {
			t.Errorf("%s: JSToString(%v) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func mathNegZero() float64 {
	zero := 0.0
	return -zero
}
