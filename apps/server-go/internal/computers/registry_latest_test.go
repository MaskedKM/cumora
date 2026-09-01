package computers

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 2026-09-01 修复钉子:latest 必须来自自家 GitHub release(CUMORA_UPDATE_API
// 可覆盖),不得再指向上游 npm —— v0.11.0 横幅事故。
func TestGetLatestDaemonVersionFromOwnReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases/latest" {
			t.Errorf("must query /releases/latest, got %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.3.0-go.5"}`))
	}))
	defer srv.Close()
	t.Setenv("CUMORA_UPDATE_API", srv.URL)

	resetLatestForTest()
	got := getLatestDaemonVersion()
	if got == nil || *got != "v0.3.0-go.5" {
		t.Fatalf("latest = %v, want v0.3.0-go.5", got)
	}

	// 缓存:第二次不回源(服务器计数)。
	var mu sync.Mutex
	hits := 0
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	defer srv2.Close()
	t.Setenv("CUMORA_UPDATE_API", srv2.URL)
	resetLatestForTest()
	_ = getLatestDaemonVersion()
	_ = getLatestDaemonVersion()
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("TTL 内应只回源一次,got %d hits", hits)
	}
}

// 失败保旧:上游 500 时保留上次成功值。
func TestGetLatestDaemonVersionKeepsOldOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("CUMORA_UPDATE_API", srv.URL)
	resetLatestForTest()
	setLatestForTest("v0.2.9", time.Now())
	got := getLatestDaemonVersion()
	if got == nil || *got != "v0.2.9" {
		t.Fatalf("failure must keep old value, got %v", got)
	}
}
