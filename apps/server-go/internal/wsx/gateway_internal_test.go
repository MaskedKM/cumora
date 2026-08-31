// gateway 端到端单测(#251):httptest 真 ws 连接对上走真 handle(),钉
// #197 评审 P1 的握手序与生命周期语义——①hello 必须是该连接的首帧
// (先于任何聊天帧,桥持续扇出的竞争下成立);②doc 面必须先 subscribe
// (未订阅的 update 是静默 no-op,无 sidecar 调用);③连接关闭时 docSubs
// 全数归还 relay、hub 注销、在场翻 resting(防房间泄漏)。DB 经
// database/sql 假 driver(connector 直挂,零新依赖),sidecar 经 httptest
// 假件——只解网关自身的序与生命周期,真 pg/Redis 面归镜像套件。
package wsx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
	"github.com/coder/websocket"
)

/* ───────── 假 DB:票据消费 / 成员资格快照 / 文档租户解析 ───────── */

// fakeGatewayDB:handle() 触库面的全部三条查询,按 SQL 形状分派。票据按
// token_hash(哈希原文注册)映射到用户,成员资格按 userID 过滤——两条
// 连接可带不同租户集,供隔离路径用。
type fakeGatewayDB struct {
	usersByHash map[string]string   // HashToken(票据) → userID
	memberships map[string][]string // userID → companyID 列表
	docCompany  string              // docCompanyFor 的解析值(成员校验通过)
}

func (f *fakeGatewayDB) connector() driver.Connector {
	return fakeDBConnector{f}
}

type fakeDBConnector struct{ f *fakeGatewayDB }

func (c fakeDBConnector) Connect(context.Context) (driver.Conn, error) {
	return fakeDBConn{f: c.f}, nil
}
func (c fakeDBConnector) Driver() driver.Driver { return fakeDBDriver{} }

type fakeDBDriver struct{}

func (fakeDBDriver) Open(string) (driver.Conn, error) {
	return fakeDBConn{f: &fakeGatewayDB{}}, nil
}

type fakeDBConn struct{ f *fakeGatewayDB }

func (fakeDBConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake: prepare unsupported")
}
func (fakeDBConn) Close() error              { return nil }
func (fakeDBConn) Begin() (driver.Tx, error) { return nil, errors.New("fake: no tx") }

func (c fakeDBConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(q, "ws_tickets"): // UPDATE … RETURNING(无 FROM 子句)
		user, ok := c.f.usersByHash[fmt.Sprint(args[0].Value)]
		if !ok {
			return &fakeRows{cols: []string{"user_id"}}, nil // 0 行 = 票据无效
		}
		return &fakeRows{cols: []string{"user_id"}, vals: [][]driver.Value{{user}}}, nil
	case strings.Contains(q, "FROM company_members WHERE"):
		vals := [][]driver.Value{}
		for _, co := range c.f.memberships[fmt.Sprint(args[0].Value)] {
			vals = append(vals, []driver.Value{co})
		}
		return &fakeRows{cols: []string{"company_id"}, vals: vals}, nil
	case strings.Contains(q, "FROM documents d"):
		return &fakeRows{cols: []string{"company_id"}, vals: [][]driver.Value{{c.f.docCompany}}}, nil
	}
	return nil, fmt.Errorf("fake db: unexpected query: %s", q)
}

type fakeRows struct {
	cols []string
	vals [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.vals) {
		return io.EOF
	}
	copy(dest, r.vals[r.i])
	r.i++
	return nil
}

/* ───────── 假 sidecar:记录 subscribe/unsubscribe/update 命中 ───────── */

type fakeSidecar struct {
	mu      sync.Mutex
	subs    []string
	unsubs  []string
	updates []string
	state   string // subscribe 回的全量 state 原文
	failSub bool   // true = /internal/doc/subscribe 回 500
}

func (f *fakeSidecar) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		switch r.URL.Path {
		case "/internal/doc/subscribe":
			if f.failSub {
				f.mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.subs = append(f.subs, body["documentId"])
		case "/internal/doc/unsubscribe":
			f.unsubs = append(f.unsubs, body["documentId"])
		case "/internal/doc/update":
			f.updates = append(f.updates, body["documentId"])
		}
		f.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"stateB64": base64.StdEncoding.EncodeToString([]byte(f.state)),
		})
	})
}

func (f *fakeSidecar) snapshot() (subs, unsubs, updates []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.subs), slices.Clone(f.unsubs), slices.Clone(f.updates)
}

/* ───────── 网关测试台 ───────── */

// newTestGateway:真 Gateway(真 handle/readLoop/写协程/桥面)+ 假 DB +
// httptest 假 sidecar。返回的服务器可直接 ws dial。
func newTestGateway(t *testing.T, db *fakeGatewayDB, sc *fakeSidecar, presence PresenceSetter) (*Gateway, *httptest.Server) {
	t.Helper()
	scsrv := httptest.NewServer(sc.handler())
	t.Cleanup(scsrv.Close)
	g := &Gateway{
		db: sql.OpenDB(db.connector()), relay: docrelay.New(scsrv.URL, "", 2000, "inst-test"),
		instanceID: "inst-test", presence: presence,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", g.handle)
	wssrv := httptest.NewServer(mux)
	t.Cleanup(wssrv.Close)
	return g, wssrv
}

// dialGateway:以一次性票据 dial,客户端读上限放到网关同款 4MB。
func dialGateway(t *testing.T, srv *httptest.Server, ticket string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cl, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?t="+ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = cl.CloseNow() })
	cl.SetReadLimit(maxFrameBytes)
	return cl
}

func wsWrite(t *testing.T, cl *websocket.Conn, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := cl.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("client write: %v", err)
	}
}

// readJSON:读一帧并解到 out;超时/畸形即 fatal。
func readJSON(t *testing.T, cl *websocket.Conn, wait time.Duration, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, data, err := cl.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("frame not json: %s", data)
	}
}

// expectNoFrame:窗口内必须无帧(负断言)。注意:coder/websocket 的读超时
// 会关闭整条连接,故本断言只作该连接的最后一读。
func expectNoFrame(t *testing.T, cl *websocket.Conn, wait time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, _, err := cl.Read(ctx); err == nil {
		t.Fatalf("expected no frame within %s, got one", wait)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

func hubSize(g *Gateway) int {
	n := 0
	g.hub.each(func(*conn) { n++ })
	return n
}

/* ───────── ①hello 首帧序(#197 评审 P1) ───────── */

// TestGatewayHelloFirstFrameUnderChattyFanout:桥持续扇出 co-a 聊天事件
// 的竞争下,每条新连接的首帧必须是 hello——handle() 里 hello 的写出先于
// hub.add,抢先的聊天帧在结构上不可能;客户端"收到 hello = 连接完成,
// 据此重放 doc.subscribe/冲刷攒批"的语义由此锚定。
func TestGatewayHelloFirstFrameUnderChattyFanout(t *testing.T) {
	db := &fakeGatewayDB{
		usersByHash: map[string]string{authn.HashToken("t1"): "u1"},
		memberships: map[string][]string{"u1": {"co-a"}},
		docCompany:  "co-a",
	}
	g, srv := newTestGateway(t, db, &fakeSidecar{state: "seed"}, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ { // 两条并发扇出腿,放大与握手/注册的交错
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := []byte(`{"type":"message.new","companyId":"co-a","conversationId":"cv1","message":{"id":"m1"}}`)
			for {
				select {
				case <-stop:
					return
				default:
					g.fanout(payload)
					time.Sleep(200 * time.Microsecond)
				}
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()

	for i := 0; i < 5; i++ { // 多次连接把竞争做实
		cl := dialGateway(t, srv, "t1")
		var hello struct {
			Type       string `json:"type"`
			InstanceID string `json:"instanceId"`
		}
		readJSON(t, cl, 2*time.Second, &hello)
		if hello.Type != "hello" {
			t.Fatalf("first frame must be hello, got %q", hello.Type)
		}
		if hello.InstanceID != "inst-test" {
			t.Fatalf("hello instanceId = %q, want inst-test", hello.InstanceID)
		}
		// 首帧之后才允许出现聊天帧(不作为断言目标,只是排掉)。
		_ = cl.CloseNow()
	}
}

/* ───────── ②doc 面:先订阅,才有 sync/update ───────── */

// TestGatewayDocRequiresSubscribe:未订阅的 doc.update 是静默 no-op——无
// doc.error、无 sidecar 调用(必须先 subscribe 才建立房间)。另用一条连接
// 走完整序:hello → doc.subscribe → doc.sync(全量 state 来自 sidecar)→
// doc.update 送达 sidecar。
func TestGatewayDocRequiresSubscribe(t *testing.T) {
	db := &fakeGatewayDB{
		usersByHash: map[string]string{authn.HashToken("t1"): "u1"},
		memberships: map[string][]string{"u1": {"co-a"}},
		docCompany:  "co-a",
	}
	sc := &fakeSidecar{state: "seed-state"}
	_, srv := newTestGateway(t, db, sc, nil)

	// 腿一:未订阅先发 update → 无帧无 sidecar 调用。
	nosub := dialGateway(t, srv, "t1")
	var hello struct {
		Type string `json:"type"`
	}
	readJSON(t, nosub, 2*time.Second, &hello)
	wsWrite(t, nosub, map[string]any{
		"type": "doc.update", "documentId": "d-ghost",
		"updateB64": base64.StdEncoding.EncodeToString([]byte("x")),
	})
	expectNoFrame(t, nosub, 300*time.Millisecond) // 连接的最后一读
	if _, _, updates := sc.snapshot(); len(updates) != 0 {
		t.Fatalf("doc.update before subscribe must not reach sidecar, got %v", updates)
	}

	// 腿二:完整序 subscribe → sync → update。
	cl := dialGateway(t, srv, "t1")
	readJSON(t, cl, 2*time.Second, &hello)
	if hello.Type != "hello" {
		t.Fatalf("first frame must be hello, got %q", hello.Type)
	}
	wsWrite(t, cl, map[string]any{"type": "doc.subscribe", "documentId": "d1"})
	var sync struct {
		Type       string `json:"type"`
		DocumentID string `json:"documentId"`
		StateB64   string `json:"stateB64"`
	}
	readJSON(t, cl, 2*time.Second, &sync)
	if sync.Type != "doc.sync" || sync.DocumentID != "d1" {
		t.Fatalf("want doc.sync for d1, got %+v", sync)
	}
	if want := base64.StdEncoding.EncodeToString([]byte("seed-state")); sync.StateB64 != want {
		t.Fatalf("doc.sync state = %q, want sidecar state %q", sync.StateB64, want)
	}
	wsWrite(t, cl, map[string]any{
		"type": "doc.update", "documentId": "d1",
		"updateB64": base64.StdEncoding.EncodeToString([]byte("upd")),
	})
	waitUntil(t, "doc.update reaches sidecar", func() bool {
		_, _, updates := sc.snapshot()
		return slices.Contains(updates, "d1")
	})
}

/* ───────── ③连接关闭:docSubs 全归还 + hub 注销 + resting ───────── */

// TestGatewayCloseReturnsAllDocSubs:客户端断开后,readLoop 的拆链必须把
// docSubs 里的每条订阅归还 relay(refcounts 归零 → sidecar 收到逐 doc 的
// unsubscribe)、从 hub 注销、在场计数归零翻 resting——防房间/表泄漏。
func TestGatewayCloseReturnsAllDocSubs(t *testing.T) {
	db := &fakeGatewayDB{
		usersByHash: map[string]string{authn.HashToken("t1"): "u1"},
		memberships: map[string][]string{"u1": {"co-a"}},
		docCompany:  "co-a",
	}
	sc := &fakeSidecar{state: "seed"}
	pres := &fakePresence{}
	g, srv := newTestGateway(t, db, sc, pres)

	cl := dialGateway(t, srv, "t1")
	var hello struct {
		Type string `json:"type"`
	}
	readJSON(t, cl, 2*time.Second, &hello)
	for _, doc := range []string{"d1", "d2", "d3"} {
		wsWrite(t, cl, map[string]any{"type": "doc.subscribe", "documentId": doc})
		var sync struct {
			DocumentID string `json:"documentId"`
		}
		readJSON(t, cl, 2*time.Second, &sync) // 每订阅一条,收一条 sync
	}
	if subs, _, _ := sc.snapshot(); len(subs) != 3 {
		t.Fatalf("sidecar subscribe hits = %v, want 3 docs", subs)
	}

	_ = cl.CloseNow() // 客户端单方面断开
	waitUntil(t, "all doc subscriptions returned", func() bool {
		_, unsubs, _ := sc.snapshot()
		return slices.Contains(unsubs, "d1") && slices.Contains(unsubs, "d2") && slices.Contains(unsubs, "d3")
	})
	waitUntil(t, "conn removed from hub", func() bool { return hubSize(g) == 0 })
	final := waitForCalls(t, pres, 2) // avail(首连) + resting(末断)
	if !slices.Contains(final, "u1:resting") {
		t.Fatalf("last disconnect must flip resting, got %v", final)
	}
}

/* ───────── ④membership 过滤:真连接对上的租户隔离 ───────── */

// TestGatewayMembershipFilterRealConns:两条真连接带不同成员资格快照
// (loadMemberships),桥扇出按租户投递——成员收原样字节帧,非成员静默;
// 两条连接各自健康(互换事件可互达),证明过滤而非连接故障。
func TestGatewayMembershipFilterRealConns(t *testing.T) {
	db := &fakeGatewayDB{
		usersByHash: map[string]string{
			authn.HashToken("t-member"): "u-member",
			authn.HashToken("t-out"):    "u-out",
		},
		memberships: map[string][]string{"u-member": {"co-a"}, "u-out": {"co-b"}},
		docCompany:  "co-a",
	}
	g, srv := newTestGateway(t, db, &fakeSidecar{state: "seed"}, nil)

	member := dialGateway(t, srv, "t-member")
	outsider := dialGateway(t, srv, "t-out")
	var hello struct {
		Type string `json:"type"`
	}
	readJSON(t, member, 2*time.Second, &hello)
	readJSON(t, outsider, 2*time.Second, &hello)

	eventA := `{"type":"message.new","companyId":"co-a","conversationId":"cv1","message":{"id":"ma"}}`
	eventB := `{"type":"message.new","companyId":"co-b","conversationId":"cv2","message":{"id":"mb"}}`
	g.fanout([]byte(eventA))
	g.fanout([]byte(eventB))

	var gotA, gotB struct {
		Type      string `json:"type"`
		CompanyID string `json:"companyId"`
	}
	readJSON(t, member, 2*time.Second, &gotA)   // 成员收 co-a
	readJSON(t, outsider, 2*time.Second, &gotB) // 局外人收 co-b
	if gotA.CompanyID != "co-a" || gotB.CompanyID != "co-b" {
		t.Fatalf("routing wrong: member=%+v outsider=%+v", gotA, gotB)
	}

	// 隔离负断言(各自的最后一读):member 不收 co-b,outsider 不收 co-a。
	expectNoFrame(t, member, 300*time.Millisecond)
	expectNoFrame(t, outsider, 300*time.Millisecond)
}
