package wsx

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func newTestConn(companies ...string) *conn {
	set := map[string]struct{}{}
	for _, c := range companies {
		set[c] = struct{}{}
	}
	return &conn{companies: set, outbound: make(chan []byte, 2)}
}

// newDeepTestConn:深度帽拉满(256)的测试连接,让字节档独立于条数档
// 起效(#236)。
func newDeepTestConn() *conn {
	return &conn{
		companies: map[string]struct{}{}, docSubs: map[string]*docSub{},
		docCompanies: map[string]string{}, outbound: make(chan []byte, outboundCap),
	}
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
	g.fanout([]byte(`not json`))                                              // 解析失败

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
	// #236:被深度帽拦下的帧必须回滚字节占位——计数只认真正进了队列的帧。
	if c.outBytes != 2 {
		t.Fatalf("byte counter must equal queued bytes after rollback, got %d", c.outBytes)
	}
}

/* ───────── 出站字节预算(#236:2MB 丢帧档 / 8MB 掐线档)───────── */

// TestEnqueueChatByteBudgetDrop:聊天帧把累计推过 2MB 档即丢(逐帧判断,
// 非粘滞),深度帽远未触及——证字节档独立起效、计数只认在队帧。
func TestEnqueueChatByteBudgetDrop(t *testing.T) {
	c := newDeepTestConn()
	big := bytes.Repeat([]byte{'a'}, 1_900_000)
	over := bytes.Repeat([]byte{'b'}, 200_000) // 1.9MB+0.2MB=2.1MB > 2MB
	small := []byte(`{"type":"typing"}`)

	c.enqueueChat(big)
	c.enqueueChat(over) // 丢
	if got := len(c.outbound); got != 1 {
		t.Fatalf("over-budget chat frame must be dropped, queued=%d", got)
	}
	if c.dropAnnounced != 1 {
		t.Fatalf("drop must be announced once")
	}
	if c.outBytes != 1_900_000 {
		t.Fatalf("counter must track queued bytes only, got %d", c.outBytes)
	}

	// 预算逐帧判断:超限帧被丢后,预算内的小帧照常放行(非粘滞拒绝)。
	c.enqueueChat(small)
	if got := len(c.outbound); got != 2 {
		t.Fatalf("small frame within budget must pass, queued=%d", got)
	}
	if c.outBytes != 1_900_000+len(small) {
		t.Fatalf("counter must include the small frame, got %d", c.outBytes)
	}
	// 聊天面只有丢帧语义:不见 doc 掐线标志(连接保活由
	// TestChatDropKeepsConnectionWritable 端到端验证)。
	if c.docClosed != 0 {
		t.Fatalf("chat overflow must not fire the doc kill flag")
	}
}

// TestOutboundBudgetReleasedAfterConsume:写协程落笔后预算归还——已发出
// 的帧不再占用额度,同尺寸的后续帧不被误丢。
func TestOutboundBudgetReleasedAfterConsume(t *testing.T) {
	c := newDeepTestConn()
	big := bytes.Repeat([]byte{'a'}, 1_900_000)
	c.enqueueChat(big) // 占位 1.9MB
	if got := len(c.outbound); got != 1 {
		t.Fatalf("first frame must enqueue, got %d", got)
	}
	over := bytes.Repeat([]byte{'b'}, 200_000)
	c.enqueueChat(over) // 2.1MB > 2MB → 丢
	if got := len(c.outbound); got != 1 {
		t.Fatalf("frame over budget must drop before release, queued=%d", got)
	}

	// 模拟写协程:取出首帧、落笔、归还预算(startWriter 的在途语义)。
	consumed := <-c.outbound
	c.releaseOutbound(len(consumed))

	c.enqueueChat(over) // 预算已归还 → 放行
	if got := len(c.outbound); got != 1 {
		t.Fatalf("frame must pass after budget release, queued=%d", got)
	}
	if c.outBytes != 200_000 {
		t.Fatalf("counter must settle at the queued frame, got %d", c.outBytes)
	}
}

// newWSConnPair:httptest 上拉一条真 ws 连接对。掐线路径要动真
// CloseNow(nil *websocket.Conn 会 panic,假件测不了),端到端存活性
// 也只有真连接可证。服务端 handler 落在 Read 上直到连接关闭。
func newWSConnPair(t *testing.T) (server *websocket.Conn, client *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- ws
		for { // 保住 hijacked 连接;CloseNow 后 Read 报错即退
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cl, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cl.CloseNow() })
	cl.SetReadLimit(4 * 1024 * 1024) // 库默认 32KB,放行测试用大帧
	select {
	case ws := <-accepted:
		return ws, cl
	case <-time.After(2 * time.Second):
		t.Fatal("server conn not accepted")
		return nil, nil
	}
}

// TestChatDropKeepsConnectionWritable:超 2MB 的聊天帧被丢,但连接存活。
// 丢帧断言先于写协程启动(无归还竞争,纯队列语义);随后启动写协程证
// 连接照常可写——预算内帧连续抵达客户端,顺序不乱。
func TestChatDropKeepsConnectionWritable(t *testing.T) {
	ws, cl := newWSConnPair(t)
	c := newDeepTestConn()

	big := bytes.Repeat([]byte{'a'}, 1_900_000)
	marker := []byte(`{"type":"typing","n":2}`)
	c.enqueueChat(big) // 1.9MB:占位成功
	c.enqueueChat(big) // 3.8MB > 2MB → 丢(此时无写协程,无归还竞争)
	if got := len(c.outbound); got != 1 {
		t.Fatalf("over-budget chat frame must be dropped, queued=%d", got)
	}
	if c.dropAnnounced != 1 {
		t.Fatalf("drop must be announced once")
	}

	c.ws = ws
	c.startWriter()
	t.Cleanup(c.wcancel)
	c.enqueueChat(marker) // 丢帧之后连接必须仍可写

	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	first := readFrame(t, cl, rctx)
	if len(first) != len(big) || !bytes.Equal(first[:3], []byte("aaa")) {
		t.Fatalf("first frame must be the 1.9MB payload, got %d bytes", len(first))
	}
	second := readFrame(t, cl, rctx)
	if string(second) != string(marker) {
		t.Fatalf("second frame must be the in-budget marker, got %s", second)
	}
	if c.docClosed != 0 {
		t.Fatalf("chat overflow must never kill the connection")
	}
}

func readFrame(t *testing.T, cl *websocket.Conn, ctx context.Context) []byte {
	t.Helper()
	_, data, err := cl.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	return data
}

// TestEnqueueDocByteBudgetKills:doc 帧档 8MB——单条 ~5.3MB(4MB Yjs
// update 的 base64+JSON 包装量级)在档内照常入队;累计将被推过 8MB 时
// 掐线:docClosed 置位、wcancel 触发、CloseNow 落到真连接(客户端读
// 侧随即观察到断开)。
func TestEnqueueDocByteBudgetKills(t *testing.T) {
	ws, cl := newWSConnPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newDeepTestConn()
	c.ws = ws
	c.wcancel = cancel

	docFrame := func(fill byte) {
		c.enqueueDoc(map[string]any{
			"type": "doc.update", "documentId": "d1",
			"updateB64": strings.Repeat(string(fill), 5_300_000),
		})
	}

	docFrame('A') // ~5.3MB < 8MB:入队,不掐线
	if got := len(c.outbound); got != 1 {
		t.Fatalf("doc frame within budget must enqueue, got %d", got)
	}
	if c.docClosed != 0 {
		t.Fatalf("doc frame within budget must not kill")
	}

	docFrame('B') // ~10.6MB > 8MB → 掐线,不入队
	if got := len(c.outbound); got != 1 {
		t.Fatalf("over-budget doc frame must not enqueue, got %d", got)
	}
	if c.docClosed != 1 {
		t.Fatalf("kill must be announced once")
	}
	select {
	case <-ctx.Done(): // wcancel 必须触发(停写协程 + readLoop 拆链)
	case <-time.After(2 * time.Second):
		t.Fatalf("wcancel must fire on doc overflow")
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rcancel()
	if _, _, err := cl.Read(rctx); err == nil {
		t.Fatalf("client must observe the close after CloseNow")
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
