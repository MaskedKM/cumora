// #154(#155 评审 nit)回归钉:裸跑 daemon 的默认 server 必须是自托管
// 本机默认——fork 的任何默认值都不得回落官方云。
package daemon

import "testing"

func TestDefaultServerURLSelfHosted(t *testing.T) {
	t.Setenv("CUMORA_SERVER_URL", "")
	if got := defaultServerURL(); got != "http://127.0.0.1:5181" {
		t.Fatalf("defaultServerURL() = %q, want http://127.0.0.1:5181(自托管默认)", got)
	}
	t.Setenv("CUMORA_SERVER_URL", "https://cumora.example.com")
	if got := defaultServerURL(); got != "https://cumora.example.com" {
		t.Fatalf("CUMORA_SERVER_URL 覆盖失效: %q", got)
	}
}
