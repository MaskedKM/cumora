// #136 回归钉:Server 超时兜底必须在场,且 WriteTimeout 保持 0
// (全局写期限会掐死 SSE/WS 长响应;非流式面走 httpx.WriteDeadline)。
package main

import (
	"net/http"
	"testing"
)

func TestNewHTTPServerTimeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, want > 0", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout <= 0 {
		t.Fatalf("ReadTimeout = %v, want > 0(框住停滞的请求体)", srv.ReadTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatalf("IdleTimeout = %v, want > 0(回收 keep-alive 空闲连接)", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v, must stay 0(SSE/WS 共享本 Server)", srv.WriteTimeout)
	}
}
