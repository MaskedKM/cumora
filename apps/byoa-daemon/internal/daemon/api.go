// daemon 包 api —— 服务端 HTTP 面(对齐 daemon.ts 的 api/runtimeBest/
// runtimeGet):短请求一律带超时;runtime 面尽力而为、永不抛错。
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// apiCall:JSON API 调用;非 2xx 报 "METHOD path → HTTP n body" 形错误。
func apiCall(ctx context.Context, serverURL, method, path string, bearer string, body any, out any) error {
	cctx, cancel := context.WithTimeout(ctx, httpTimeout())
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(cctx, method, serverURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("%s %s → HTTP %d %s", method, path, res.StatusCode, truncate(string(raw), 200))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// runtimeBest:fire-and-forget runtime 调用(状态/runs)。观测面绝不打断
// agent 环路;超时让"停止应答的服务器"也会落到错误分支而不是挂死承诺。
func runtimeBest(ctx context.Context, serverURL, path, token string, body any) json.RawMessage {
	var out json.RawMessage
	_ = apiCall(ctx, serverURL, http.MethodPost, "/runtime"+path, token, body, &out)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
