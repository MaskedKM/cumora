// jsstring —— ECMAScript ToString 对齐(#68 评审 F16):TS 侧
// `String(req.body?.x ?? ”)` 对非串值做强转(123→"123"、true→"true"、
// {}→"[object Object]"、[1,2]→"1,2"),Go 侧此前的 `s, _ := v.(string)`
// 把非串静默吞成 "",导致 400 分叉。这里按 JSON 可表达的值域实现
// ECMAScript Number/Array::toString 语义;null/undefined 的空串回落
// (`?? ”`)由调用方语义决定,不在本函数内。
package httpx

import (
	"math"
	"strconv"
	"strings"
)

// JSToString 对 JSON 解码值(any)复刻 JS String(v)。v 为 nil 时返回
// "null"(JS String(null));调用方要 `?? ”` 语义请先判 nil。
func JSToString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberToString(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			// Array::join:null/undefined 元素成 "",其余递归 ToString。
			if e == nil {
				parts[i] = ""
				continue
			}
			parts[i] = JSToString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return "[object Object]"
	}
}

// jsNumberToString 按 ECMAScript Number::toString(10):
//   - |x| < 1e-6 或 ≥ 1e21 用指数式(指数无前导零,1e-7、1e+21);
//   - 其间用最短往返定点式(1e20 → "100000000000000000000"、
//     1/3 → "0.3333333333333333";(2^53,1e21) 整数经最短往返取整)。
//
// JSON 数字经 encoding/json 一律 float64,无 NaN/Infinity 面。
func jsNumberToString(x float64) string {
	if x == 0 {
		return "0" // 含 -0:JS String(-0) === "0"
	}
	abs := math.Abs(x)
	if abs < 1e-6 || abs >= 1e21 {
		s := strconv.FormatFloat(x, 'e', -1, 64)
		// Go "1e+21"/"1e-07" → JS "1e+21"/"1e-7":剥指数前导零,保符号。
		i := strings.IndexByte(s, 'e')
		mant, exp := s[:i], s[i+1:]
		sign := ""
		if exp[0] == '+' || exp[0] == '-' {
			sign, exp = string(exp[0]), exp[1:]
		}
		exp = strings.TrimLeft(exp, "0")
		if exp == "" {
			exp = "0"
		}
		return mant + "e" + sign + exp
	}
	// F-01(评审):整段窗口统一最短往返定点——(2^53, 1e21) 内的整数
	// 必须经最短往返取整(1234567890123456789 → "1234567890123456800"),
	// 按精确值展开会偏离 JS String()。
	return strconv.FormatFloat(x, 'f', -1, 64)
}

// JSStringOrNullish 复刻 `String(x ?? ”)`:JSON null(以及 Go 侧键缺失
// 传进的 nil)回落空串,其余按 JSToString。
func JSStringOrNullish(v any) string {
	if v == nil {
		return ""
	}
	return JSToString(v)
}

// JSTruthy 复刻 JS Boolean(x):false/0(-0)/""/null 为假;对象与数组
// (含空数组)恒真 —— `req.body?.color ? String(...) : null` 的门。
func JSTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	default:
		return true
	}
}

// UTF16Cap 复刻 JS s.slice(0, n):按 UTF-16 码元截断,边界裂代理保整字
// (与 TS 差半个字符,极端边缘)。此前各域 rune 截断会把 emoji 当 1 记
// (#118 F-04 统一);agents/uploads/email 的私有拷贝收敛到此。
func UTF16Cap(s string, max int) string {
	n := 0
	for i, r := range s {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if n+units > max {
			return s[:i]
		}
		n += units
	}
	return s
}
