// httpx —— 域路由的公共助手(#51)。契约事实源:packages/contract/openapi.yaml;
// 镜像断言(server/src/__integration__/mirror-*.test.ts)是行为基准。
package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
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

// WriteInternalError:TS errorHandler 非 HttpError 分支的合一(#141,
// 122 处裸 err.Error() 500 收编)——恒 slog 排障;对外复刻 TS 的 dev/prod
// 分流:NODE_ENV≠production 保留 err.message(TS 设计特性:不翻日志即可
// 排障),production 收敛通用文案关泄漏。三处不进本助手:devtools 头像
// 502 与 apple 400 是 TS baseline 无条件透传;waitlist approve 的受控
// 域错走原 WriteError。
func WriteInternalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Warn("api 500", "method", r.Method, "path", r.URL.Path, "err", err)
	msg := err.Error()
	if os.Getenv("NODE_ENV") == "production" {
		msg = "internal server error"
	}
	WriteError(w, http.StatusInternalServerError, msg)
}

// MountHealth 挂 /api/health(带 pg 探活)与 /api/livez(无依赖)。
// 形状对齐 TS baseline:{ok, ts(ms)};池耗尽时 1s 超时兜底返回确定性 503。
func MountHealth(mux *http.ServeMux, pool interface {
	PingContext(ctx context.Context) error
}) {
	mux.HandleFunc("GET /api/livez", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().UnixMilli()})
	})
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := pool.PingContext(ctx); err != nil {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().UnixMilli()})
	})
}

// ISOms 对齐 JS Date.toISOString():恒为 UTC + 毫秒三位小数
// (RFC3339Nano 会按需省略尾零,pg 的 µs 精度也会透出)。日历域(#57)
// 起统一采用;其余域在被触及时迁移。
func ISOms(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
