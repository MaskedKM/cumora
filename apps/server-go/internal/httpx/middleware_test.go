// #136:WriteDeadline 写期限中间件——正常响应直通;期限过后的迟写
// 被掐断(挂起客户端不再无限占用 handler)。纯 HTTP 行为,无 DB。
package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriteDeadlinePassThrough(t *testing.T) {
	h := WriteDeadline(time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("code=%d body=%q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestWriteDeadlineKillsLateWrite(t *testing.T) {
	srv := httptest.NewServer(WriteDeadline(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	})))
	defer srv.Close()
	res, err := srv.Client().Get(srv.URL)
	if err == nil {
		defer res.Body.Close()
	}
	// 写期限在 handler 首写前已过:响应头都来不及写出,连接被掐——
	// 客户端不得拿到完整的 200 "late"。
	if err == nil && res.StatusCode == http.StatusOK {
		t.Fatal("迟写应被写期限掐断,却返回了 200")
	}
}
