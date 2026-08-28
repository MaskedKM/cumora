// push 超时兜底单测(#136):FCM 客户端必须带超时,挂起端点快速失败
// 而非无限阻塞扇出 goroutine。纯 HTTP 行为,无 DB。
package push

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 回归钉:曾用无超时 http.DefaultClient(令牌兑换 + 发送两条链路)。
func TestFcmClientHasTimeout(t *testing.T) {
	if fcmClient.Timeout <= 0 {
		t.Fatalf("fcmClient.Timeout = %v, want > 0", fcmClient.Timeout)
	}
}

// 挂起端点必须在客户端超时内返回 fetch-error(注入 50ms 超时替身,
// 生产值 15s——同一机制)。
func TestSendOneFcmTimesOutOnHangingEndpoint(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer hang.Close()

	oldFmt, oldClient := fcmSendURLFmt, fcmClient
	fcmSendURLFmt = hang.URL + "/v1/projects/%s/messages:send"
	fcmClient = &http.Client{Timeout: 50 * time.Millisecond}
	defer func() { fcmSendURLFmt, fcmClient = oldFmt, oldClient }()

	fcmAccount = &serviceAccount{ProjectID: "p", ClientEmail: "e@x", PrivateKey: "k"}
	fcmTokenVal, fcmTokenTime = "test-token", time.Now()
	defer func() { fcmAccount = nil; fcmTokenVal, fcmTokenTime = "", time.Time{} }()

	start := time.Now()
	res := sendOneFcm("device-token", Payload{Title: "t", Body: "b"})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("sendOneFcm 耗时 %s,应在客户端超时内快速返回", elapsed)
	}
	if res.ok || res.reason != "fetch-error" {
		t.Fatalf("res = %+v, want reason=fetch-error", res)
	}
}
