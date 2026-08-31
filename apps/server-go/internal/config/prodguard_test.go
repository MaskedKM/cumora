package config

import "testing"

func kv(pairs ...string) func(string) string {
	return func(key string) string {
		for i := 0; i+1 < len(pairs); i += 2 {
			if pairs[i] == key {
				return pairs[i+1]
			}
		}
		return ""
	}
}

func TestProdEnvViolations(t *testing.T) {
	cases := []struct {
		name string
		env  func(string) string
		want int
	}{
		{"非生产全空照旧放行", kv(), 0},
		{"非生产开了 FAKE_AUTH 是合法测试形态", kv("CUMORA_GO_FAKE_AUTH", "1"), 0},
		{"生产漏 AGENT_RUNTIME_SECRET 拒启", kv("NODE_ENV", "production"), 1},
		{"生产密钥全空白视同未设", kv("NODE_ENV", "production", "AGENT_RUNTIME_SECRET", "  "), 1},
		{"生产开 FAKE_AUTH 拒启", kv("NODE_ENV", "production", "AGENT_RUNTIME_SECRET", "s", "CUMORA_GO_FAKE_AUTH", "1"), 1},
		{"生产两病并存两条都报", kv("NODE_ENV", "production", "CUMORA_GO_FAKE_AUTH", "1"), 2},
		{"生产密钥齐全放行", kv("NODE_ENV", "production", "AGENT_RUNTIME_SECRET", "s", "CUMORA_GO_FAKE_AUTH", "0"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProdEnvViolations(tc.env)
			if len(got) != tc.want {
				t.Fatalf("violations = %d (%v), want %d", len(got), got, tc.want)
			}
		})
	}
}

func TestEnvFallbackWarnings(t *testing.T) {
	if got := EnvFallbackWarnings(kv("NODE_ENV", "production", "CUMORA_SECRETS_KEY", "k")); len(got) != 0 {
		t.Fatalf("生产密钥齐全应零告警,得到 %v", got)
	}
	if got := EnvFallbackWarnings(kv("NODE_ENV", "production")); len(got) != 1 {
		t.Fatalf("生产缺 CUMORA_SECRETS_KEY 应恰好一条 Warn,得到 %v", got)
	}
	if got := EnvFallbackWarnings(kv()); len(got) != 1 {
		t.Fatalf("开发缺 AGENT_RUNTIME_SECRET 应恰好一条 Warn,得到 %v", got)
	}
	if got := EnvFallbackWarnings(kv("AGENT_RUNTIME_SECRET", "s")); len(got) != 0 {
		t.Fatalf("开发密钥齐全应零告警,得到 %v", got)
	}
}
