// oauth_providers_test.go —— AuthProviders 探活端点单测(#284):配置态
// 只读映射,不回显配置内容;env 面逐 provider 独立。
package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthProvidersReflectsConfig(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "gh-id")
	t.Setenv("GITHUB_CLIENT_SECRET", "gh-secret")
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")

	s := &Server{}
	w := httptest.NewRecorder()
	s.AuthProviders(w, httptest.NewRequest(http.MethodGet, "/api/auth/providers", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if !body["github"] || body["google"] {
		t.Fatalf("配置态映射: %+v", body)
	}
}

func TestAuthProvidersAllUnconfigured(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "")
	t.Setenv("GITHUB_CLIENT_SECRET", "")
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")

	s := &Server{}
	w := httptest.NewRecorder()
	s.AuthProviders(w, httptest.NewRequest(http.MethodGet, "/api/auth/providers", nil))

	var body map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body["github"] || body["google"] {
		t.Fatalf("全未配应双 false: %+v", body)
	}
}
