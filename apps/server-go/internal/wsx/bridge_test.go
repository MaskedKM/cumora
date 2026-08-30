package wsx

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

func newTestConn(companies ...string) *conn {
	set := map[string]struct{}{}
	for _, c := range companies {
		set[c] = struct{}{}
	}
	return &conn{companies: set, outbound: make(chan []byte, 2)}
}

func TestFanoutTenantFiltering(t *testing.T) {
	g := &Gateway{}
	inA := newTestConn("co-a")
	inB := newTestConn("co-b")
	g.hub.add(inA)
	g.hub.add(inB)

	msg := `{"type":"message.new","companyId":"co-a","conversationId":"cv1","message":{"id":"m1"}}`
	g.fanout([]byte(msg))

	if got := len(inA.outbound); got != 1 {
		t.Fatalf("member conn got %d frames, want 1", got)
	}
	if raw := <-inA.outbound; string(raw) != msg {
		t.Fatalf("forwarded payload not verbatim: %s", raw)
	}
	if got := len(inB.outbound); got != 0 {
		t.Fatalf("non-member conn got %d frames, want 0 (tenant isolation)", got)
	}
}

func TestFanoutDropsUntaggedAndMalformed(t *testing.T) {
	g := &Gateway{}
	c := newTestConn("co-a")
	g.hub.add(c)

	g.fanout([]byte(`{"type":"typing","conversationId":"cv1","done":false}`)) // 无 companyId
	g.fanout([]byte(`not json`))                                             // 解析失败

	if got := len(c.outbound); got != 0 {
		t.Fatalf("untagged/malformed events must not route, got %d frames", got)
	}
}

func TestEnqueueChatBackpressure(t *testing.T) {
	c := newTestConn("co-a") // outbound 容量 2
	for i := 0; i < 5; i++ {
		c.enqueueChat([]byte{'x'})
	}
	if got := len(c.outbound); got != 2 {
		t.Fatalf("queue must cap at capacity, got %d", got)
	}
	if c.dropAnnounced != 1 {
		t.Fatalf("drop flag not announced")
	}
}

func TestHelloFrameShape(t *testing.T) {
	f := helloFrame("app-go-test01")
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type       string `json:"type"`
		InstanceID string `json:"instanceId"`
		Ts         int64  `json:"ts"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "hello" || got.InstanceID != "app-go-test01" {
		t.Fatalf("bad hello frame: %s", b)
	}
	if got.Ts <= 0 {
		t.Fatalf("hello ts must be epoch ms, got %d", got.Ts)
	}
}

/* ───────── presence 计数(翻转异步,轮询收敛) ───────── */

type fakePresence struct {
	mu    sync.Mutex
	calls []string // "user:status"
}

func (f *fakePresence) SetStatus(_ context.Context, participantID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, participantID+":"+status)
	return nil
}

func (f *fakePresence) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func waitForCalls(t *testing.T, f *fakePresence, want int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("presence calls did not reach %d, got %v", want, f.snapshot())
	return nil
}

func TestPresenceCounterFlips(t *testing.T) {
	f := &fakePresence{}
	g := &Gateway{presence: f}

	g.humanConnect("u1")
	g.humanConnect("u1") // 第二条连接:不重复翻
	g.humanConnect("u2")
	got := waitForCalls(t, f, 2) // 翻转是并发 goroutine,只保证集合
	if len(got) != 2 {
		t.Fatalf("want exactly two first-connection flips, got %v", got)
	}
	seen := map[string]bool{}
	for _, c := range got {
		seen[c] = true
	}
	if !seen["u1:avail"] || !seen["u2:avail"] {
		t.Fatalf("want u1:avail + u2:avail only, got %v", got)
	}

	g.humanDisconnect("u1") // 仍有 1 条:不翻
	g.humanDisconnect("u1") // 归零:resting
	g.humanDisconnect("u1") // 重复拆除:防御路径,不翻
	final := waitForCalls(t, f, 3)
	sort.Strings(final)
	want := []string{"u1:avail", "u1:resting", "u2:avail"}
	if !slices.Equal(final, want) {
		t.Fatalf("want exactly %v (one resting on last disconnect), got %v", want, final)
	}
}
