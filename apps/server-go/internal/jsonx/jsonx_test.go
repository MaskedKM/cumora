package jsonx

import "testing"

func TestMustJSONHappy(t *testing.T) {
	got := string(MustJSON(map[string]any{"a": 1}))
	if got != `{"a":1}` {
		t.Fatalf("happy path: got %s", got)
	}
	if MustJSONString(map[string]any{"a": 1}) != `{"a":1}` {
		t.Fatalf("MustJSONString happy path mismatch")
	}
}

// 不可序列化值(chan)兜底 "{}" —— 统一前的三种私有语义里,静默 nil/""
// 会产出非法 JSON,此处钉死安全侧。
func TestMustJSONFallback(t *testing.T) {
	if got := string(MustJSON(make(chan int))); got != "{}" {
		t.Fatalf("fallback: got %q, want {}", got)
	}
	if got := MustJSONString(make(chan int)); got != "{}" {
		t.Fatalf("MustJSONString fallback: got %q, want {}", got)
	}
}
