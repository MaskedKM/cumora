// #211 livez 扩 Redis 硬依赖:两种状态(200 假绿收口)+ nil 注入的
// 旧语义回退 + MountHealth 路由接线。纯 HTTP 行为,探活闭包用替身。
package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLivezGreenWhenRedisReachable(t *testing.T) {
	rec := httptest.NewRecorder()
	Livez(func(context.Context) error { return nil }, rec, httptest.NewRequest(http.MethodGet, "/api/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), `"ts"`) {
		t.Fatalf("body=%q, want {ok:true, ts} baseline 形状", rec.Body.String())
	}
}

func TestLivezRedWhenRedisUnreachable(t *testing.T) {
	rec := httptest.NewRecorder()
	Livez(func(context.Context) error { return errors.New("redis: connection refused") },
		rec, httptest.NewRequest(http.MethodGet, "/api/livez", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want 503(Redis 硬依赖不可达,livez 必须变红而非假绿)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"ok":false`) || !strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("body=%q, want {ok:false, error:…}", rec.Body.String())
	}
}

func TestLivezNilPingKeepsLegacySemantics(t *testing.T) {
	// nil 注入(嵌入/测试场景)= 无依赖活探,保持 #51 起的旧语义。
	rec := httptest.NewRecorder()
	Livez(nil, rec, httptest.NewRequest(http.MethodGet, "/api/livez", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200(nil ping 下退回无依赖活探)", rec.Code)
	}
}

func TestMountHealthWiresLivezPing(t *testing.T) {
	// 根 mux 挂载必须把 rping 接进 livez(生产真正被调的那条注册路径);
	// /api/health 的 pg 探活形状此处用替身池一并钉住。
	mux := http.NewServeMux()
	MountHealth(mux, stubPool{err: errors.New("pg down")}, func(context.Context) error {
		return errors.New("redis down")
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/livez", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("livez code=%d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health code=%d, want 503", rec.Code)
	}
}

type stubPool struct{ err error }

func (p stubPool) PingContext(context.Context) error { return p.err }
