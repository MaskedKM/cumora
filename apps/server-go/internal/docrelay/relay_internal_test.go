// docrelay 单测(#251,包内首组):订阅表互斥与 refcount 守恒——
// Subscribe 失败回滚登记、refcounts 归零才注销 sidecar、重复 Unsubscribe
// 幂等;fanout 回声抑制(OriginID)、nil 回调订阅不打断扇出、畸形事件
// 拒发;Redis 不可达降级态快败。真 Redis 扇出泵与 sidecar 真身归镜像
// 套件;这里用 httptest 假 sidecar + 直调 fanout 钉 relay 自身语义,
// -race 下并发 Subscribe/Unsubscribe/fanout 再钉表一致性。
package docrelay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
)

/* ───────── 假 sidecar:记录命中,可控失败 ───────── */

type fakeSidecar struct {
	mu        sync.Mutex
	subs      []string // documentId 命中 /internal/doc/subscribe
	unsubs    []string // documentId 命中 /internal/doc/unsubscribe
	state     string   // subscribe 回的全量 state 原文
	failSub   bool     // true = subscribe 回 500(触发登记回滚路径)
	failUnsub bool     // true = unsubscribe 回 500(注销失败只告警)
}

func (f *fakeSidecar) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/internal/doc/subscribe":
			if f.failSub {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			f.subs = append(f.subs, body["documentId"])
		case "/internal/doc/unsubscribe":
			f.unsubs = append(f.unsubs, body["documentId"])
			if f.failUnsub {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"stateB64": base64.StdEncoding.EncodeToString([]byte(f.state)),
		})
	})
}

func (f *fakeSidecar) snapshot() (subs, unsubs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subs...), append([]string(nil), f.unsubs...)
}

// newTestRelay:挂假 sidecar 的 relay(timeout 2s)。Boot 不调(无需泵)。
func newTestRelay(t *testing.T, f *fakeSidecar) *Relay {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok", 2000, "inst-test")
}

func mustSubscribe(t *testing.T, r *Relay, doc, origin string, onUpdate func([]byte, string)) *Subscriber {
	t.Helper()
	return mustSubscribeSub(t, r, doc, &Subscriber{OriginID: origin, OnUpdate: onUpdate})
}

func mustSubscribeSub(t *testing.T, r *Relay, doc string, s *Subscriber) *Subscriber {
	t.Helper()
	if _, err := r.Subscribe(context.Background(), doc, "co-1", s); err != nil {
		t.Fatalf("subscribe %s: %v", doc, err)
	}
	return s
}

// hitLog:并发安全的回调命中登记(originID → 载荷原文)。
type hitLog struct {
	mu  sync.Mutex
	got map[string]string
}

func newHitLog() *hitLog { return &hitLog{got: map[string]string{}} }

func (h *hitLog) record(origin, payload string) {
	h.mu.Lock()
	h.got[origin] = payload
	h.mu.Unlock()
}

func (h *hitLog) snapshot() map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.got)
}

// docEvent:sidecar → relay 扇出通道上的载荷形状。
func docEvent(t *testing.T, doc, origin, update string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"documentId": doc, "originId": origin,
		"updateB64": base64.StdEncoding.EncodeToString([]byte(update)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

/* ───────── 订阅表互斥 / refcount 守恒 ───────── */

// TestSubscribeFailureRollsBackRegistration:sidecar 订阅失败 → Subscribe
// 上抛,且本地登记与 refcount 必须回滚(否则半登记订阅永远收不到扇出,
// 还把该 doc 的 refcount 永久垫高、注销永不发生)。
func TestSubscribeFailureRollsBackRegistration(t *testing.T) {
	sc := &fakeSidecar{state: "s", failSub: true}
	r := newTestRelay(t, sc)
	s := &Subscriber{OriginID: "ws-1"}
	if _, err := r.Subscribe(context.Background(), "d1", "co-1", s); err == nil {
		t.Fatal("subscribe against failing sidecar must error")
	}
	r.mu.Lock()
	subs, refs := len(r.subs["d1"]), r.refcounts["d1"]
	r.mu.Unlock()
	if subs != 0 || refs != 0 {
		t.Fatalf("failed subscribe must roll back: subs=%d refcount=%d", subs, refs)
	}
	if _, unsubs := sc.snapshot(); len(unsubs) != 0 {
		t.Fatalf("rollback path must not call sidecar unsubscribe, got %v", unsubs)
	}
}

// TestUnsubscribeOnlyAtZeroRefcount:同 doc 两个订阅者,逐个归还——
// refcount 未归零不注销 sidecar(第一条 Unsubscribe 无 HTTP 调用),归零
// 恰好注销一次;注销后表两清。重复 Unsubscribe 同一订阅者是幂等 no-op,
// 不产生第二次 sidecar 调用。
func TestUnsubscribeOnlyAtZeroRefcount(t *testing.T) {
	sc := &fakeSidecar{state: "s"}
	r := newTestRelay(t, sc)
	s1 := mustSubscribe(t, r, "d1", "ws-1", nil)
	s2 := mustSubscribe(t, r, "d1", "ws-2", nil)

	r.Unsubscribe("d1", s1)
	if _, unsubs := sc.snapshot(); len(unsubs) != 0 {
		t.Fatalf("refcount 2→1 must not unregister sidecar, got %v", unsubs)
	}
	r.mu.Lock()
	left := len(r.subs["d1"])
	r.mu.Unlock()
	if left != 1 {
		t.Fatalf("one subscriber must remain, got %d", left)
	}

	r.Unsubscribe("d1", s2) // 归零 → 注销恰一次
	if _, unsubs := sc.snapshot(); len(unsubs) != 1 || unsubs[0] != "d1" {
		t.Fatalf("zero refcount must unregister sidecar exactly once, got %v", unsubs)
	}
	r.mu.Lock()
	subsN, refsN := len(r.subs["d1"]), r.refcounts["d1"]
	r.mu.Unlock()
	if subsN != 0 || refsN != 0 {
		t.Fatalf("table must drain at zero: subs=%d refs=%d", subsN, refsN)
	}

	r.Unsubscribe("d1", s2) // 重复归还:幂等 no-op
	if _, unsubs := sc.snapshot(); len(unsubs) != 1 {
		t.Fatalf("duplicate unsubscribe must be a no-op, got %v", unsubs)
	}
}

// TestUnsubscribeFailureStillDrainsTable:注销 sidecar 失败(500)只告警,
// 本地表仍两清——sidecar 无状态,残留订阅随其重启回收,本地不得因远端
// 故障拒绝归还(否则连接泄漏)。
func TestUnsubscribeFailureStillDrainsTable(t *testing.T) {
	sc := &fakeSidecar{state: "s", failUnsub: true}
	r := newTestRelay(t, sc)
	s := mustSubscribe(t, r, "d1", "ws-1", nil)
	r.Unsubscribe("d1", s)
	r.mu.Lock()
	subsN, refsN := len(r.subs["d1"]), r.refcounts["d1"]
	r.mu.Unlock()
	if subsN != 0 || refsN != 0 {
		t.Fatalf("local table must drain despite sidecar failure: subs=%d refs=%d", subsN, refsN)
	}
}

/* ───────── fanout:回声抑制 / 死订阅不打断 / 畸形拒发 ───────── */

// TestFanoutEchoSuppressionAndNilCallbackSkip:发起者(OriginID 相同)不
// 收自己的回声;其他订阅者收到原样 update 字节;nil 回调订阅(死订阅)
// 被跳过且不打断其余订阅者的投递。
func TestFanoutEchoSuppressionAndNilCallbackSkip(t *testing.T) {
	r := newTestRelay(t, &fakeSidecar{state: "s"})
	hits := newHitLog()
	recording := func(origin string) *Subscriber {
		return &Subscriber{OriginID: origin, OnUpdate: func(u []byte, _ string) {
			hits.record(origin, string(u))
		}}
	}

	mustSubscribeSub(t, r, "d1", recording("ws-1")) // 发起者:断言零回调
	mustSubscribeSub(t, r, "d1", recording("ws-2"))
	mustSubscribeSub(t, r, "d1", recording("ws-3"))
	// 死订阅:OnUpdate/OnAwareness 双 nil——必须被跳过,且不打断 ws-2/
	// ws-3 的投递。
	mustSubscribe(t, r, "d1", "ws-nil", nil)

	r.fanout(events.ChDocUpdate, docEvent(t, "d1", "ws-1", "payload-1"))

	got := hits.snapshot()
	if got["ws-1"] != "" {
		t.Fatalf("originator must not receive its own echo, got %q", got["ws-1"])
	}
	if got["ws-2"] != "payload-1" || got["ws-3"] != "payload-1" {
		t.Fatalf("live subscribers must receive verbatim update, got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("exactly two deliveries expected, got %v", got)
	}
}

// TestFanoutAwarenessChannel:ChDocAware 事件只走 OnAwareness 回调(与
// OnUpdate 分流),回声抑制同款(发起者不收自己的 awareness)。
func TestFanoutAwarenessChannel(t *testing.T) {
	r := newTestRelay(t, &fakeSidecar{state: "s"})
	hits := newHitLog()
	aware := func(origin string) *Subscriber {
		return &Subscriber{OriginID: origin, OnAwareness: func(u []byte, _ string) {
			hits.record(origin, string(u))
		}}
	}
	mustSubscribeSub(t, r, "d1", aware("ws-1")) // 发起者:断言零回调
	mustSubscribeSub(t, r, "d1", aware("ws-2")) // awareness 半边:OnAwareness 在
	// update 半边:OnUpdate 在,awareness 事件不得触发它
	mustSubscribe(t, r, "d1", "ws-3", func(u []byte, _ string) {
		hits.record("ws-3", string(u))
	})

	r.fanout(events.ChDocAware, docEvent(t, "d1", "ws-1", "cursor"))

	got := hits.snapshot()
	if got["ws-1"] != "" {
		t.Fatalf("originator must not receive its own awareness echo, got %q", got["ws-1"])
	}
	if got["ws-2"] != "cursor" {
		t.Fatalf("awareness must reach OnAwareness verbatim, got %v", got)
	}
	if got["ws-3"] != "" {
		t.Fatalf("awareness event must not fire OnUpdate, got %v", got)
	}
}

// TestFanoutDropsMalformed:非 JSON / 缺 documentId / 坏 base64 一律拒发,
// 不触任何回调。
func TestFanoutDropsMalformed(t *testing.T) {
	r := newTestRelay(t, &fakeSidecar{state: "s"})
	var hits atomic.Int32
	mustSubscribe(t, r, "d1", "ws-1", func([]byte, string) { hits.Add(1) })
	r.fanout(events.ChDocUpdate, []byte("not json"))
	r.fanout(events.ChDocUpdate, []byte(`{"originId":"ws-2","updateB64":""}`))                      // 缺 documentId
	r.fanout(events.ChDocUpdate, []byte(`{"documentId":"d1","originId":"ws-2","updateB64":"@@@"}`)) // 坏 base64
	if n := hits.Load(); n != 0 {
		t.Fatalf("malformed events must not fan out, got %d callbacks", n)
	}
}

// TestOffFastFail:Redis 不可达降级态(Boot(ctx, nil) → off)订阅快败——
// 否则客户端拿到永远收不到扇出的半开协同会话。
func TestOffFastFail(t *testing.T) {
	sc := &fakeSidecar{state: "s"}
	r := newTestRelay(t, sc)
	r.Boot(context.Background(), nil)
	if _, err := r.Subscribe(context.Background(), "d1", "co-1", &Subscriber{OriginID: "ws-1"}); err == nil {
		t.Fatal("off relay must fail subscribe fast")
	}
	if subs, _ := sc.snapshot(); len(subs) != 0 {
		t.Fatalf("off relay must not call sidecar, got %v", subs)
	}
}

/* ───────── 并发面(-race) ───────── */

// TestConcurrentSubscribeUnsubscribeFanout:多 goroutine 并发
// Subscribe/Unsubscribe 与 fanout 交错。收束后表必须自洽:每个 doc 的
// refcount 与订阅集基数一致(或同为零/同缺席),无负计数、无孤儿集。
func TestConcurrentSubscribeUnsubscribeFanout(t *testing.T) {
	sc := &fakeSidecar{state: "s"}
	r := newTestRelay(t, sc)
	var hits atomic.Int32
	newSub := func() *Subscriber {
		return &Subscriber{OriginID: "ws-race", OnUpdate: func([]byte, string) { hits.Add(1) }}
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s := newSub()
				if _, err := r.Subscribe(context.Background(), "d1", "co-1", s); err != nil {
					t.Errorf("subscribe: %v", err)
					return
				}
				r.Unsubscribe("d1", s)
			}
		}()
	}
	for w := 0; w < 4; w++ { // 扇出与表变更交错(快照在锁下,回调在锁外)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r.fanout(events.ChDocUpdate, docEvent(t, "d1", "ws-other", "x"))
			}
		}()
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	subsN, refs := len(r.subs["d1"]), r.refcounts["d1"]
	if subsN != refs {
		t.Fatalf("table inconsistent after race: subs=%d refcount=%d", subsN, refs)
	}
	if refs < 0 {
		t.Fatalf("negative refcount: %d", refs)
	}
}
