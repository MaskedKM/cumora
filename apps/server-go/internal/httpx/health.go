// httpx —— 域路由的公共助手(#51)。契约事实源:packages/contract/openapi.yaml;
// 镜像断言(server/src/__integration__/mirror-*.test.ts)是行为基准。
package httpx

import (
	"encoding/json"
	"net/http"
	"time"
)

// WriteJSON 统一 JSON 响应与错误形状({error: string},与 TS baseline 对齐)。
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// MountHealth 挂 /api/health(带 pg 探活)与 /api/livez(无依赖)——
// 形状对齐 TS baseline:{ok, ts} / {ok}。pool 接受 *sql.DB。
func MountHealth(mux *http.ServeMux, pool interface{ Ping() error }) {
	mux.HandleFunc("GET /api/livez", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		if err := pool.Ping(); err != nil {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": nowUnix()})
	})
}

func nowUnix() int64 { return time.Now().Unix() }
