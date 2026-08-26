// protocol_test —— #63 验收:stub server 驱动的协议级测试(daemon wire
// seam)。一个进程内 HTTP stub 实现服务端的配对/心跳/agent 同步/
// runtime-token/wake-stream(SSE)/runtime 数据面,把 daemon 当黑盒驱动,
// 断言两侧的线上行为:配对持久化、周期心跳、SSE 唤醒→run 生命周期
// (runs→finish)、session id 落盘并在下一轮 resume。
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubServer:服务端的最小协议实现 + 全部观测点。
type stubServer struct {
	mu sync.Mutex

	pairCalls  []map[string]any
	heartbeats []struct {
		version string
		auth    string
	}
	wokenStreams int // wake-stream 建连数

	agents []AgentInfo
	// wakeFn:每次 SSE 建连后由测试触发广播。
	wakeFn func(convo string)

	runCreates  []map[string]any
	runFinishes map[string]map[string]any
	currentRun  string
	nextRunID   int

	inboxRows []map[string]any

	agentTokensIssued int
}

func newStubServer() *stubServer {
	return &stubServer{runFinishes: map[string]map[string]any{}}
}

func (s *stubServer) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/computers/pair", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.pairCalls = append(s.pairCalls, body)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"computerId": "comp-stub-1", "deviceToken": "dev-token-1"})
	})

	mux.HandleFunc("POST /api/computers/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		v, _ := body["version"].(string)
		s.mu.Lock()
		s.heartbeats = append(s.heartbeats, struct {
			version string
			auth    string
		}{v, r.Header.Get("Authorization")})
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/computers/me/agents", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		agents := append([]AgentInfo{}, s.agents...)
		s.mu.Unlock()
		writeJSON(w, agents)
	})

	mux.HandleFunc("POST /api/agents/{id}/runtime-token", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.agentTokensIssued++
		s.mu.Unlock()
		writeJSON(w, map[string]any{"token": "agent-jwt-" + r.PathValue("id"), "expiresInSeconds": 3600})
	})

	// /runtime/*:鉴权即 agent JWT 形状(Stub 不验签,只验存在)。
	mux.HandleFunc("GET /runtime/wake-stream", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer agent-jwt-") {
			http.Error(w, `{"error":"missing bearer token"}`, 401)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl.Flush()
		wakeC := make(chan string, 8)
		s.mu.Lock()
		s.wokenStreams++
		s.wakeFn = func(convo string) { wakeC <- convo }
		s.mu.Unlock()
		fmt.Fprint(w, "event: ready\ndata: {\"agentId\":\"a1\",\"at\":1}\n\n")
		fl.Flush()
		for {
			select {
			case convo := <-wakeC:
				evt := map[string]any{"kind": "wake", "id": "w1", "at": time.Now().UnixMilli(), "reason": "message.new"}
				if convo != "" {
					evt["conversationId"] = convo
				}
				b, _ := json.Marshal(evt)
				fmt.Fprintf(w, "event: wake\nid: w1\ndata: %s\n\n", b)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	mux.HandleFunc("GET /runtime/inbox", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer agent-jwt-") {
			http.Error(w, `{"error":"missing bearer token"}`, 401)
			return
		}
		s.mu.Lock()
		rows := append([]map[string]any{}, s.inboxRows...)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"rows": rows})
	})

	mux.HandleFunc("POST /runtime/runs", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer agent-jwt-") {
			http.Error(w, `{"error":"missing bearer token"}`, 401)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.nextRunID++
		id := fmt.Sprintf("run-%d", s.nextRunID)
		s.currentRun = id
		s.runCreates = append(s.runCreates, body)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"runId": id})
	})

	mux.HandleFunc("POST /runtime/runs/{runId}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /runtime/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.runFinishes[r.PathValue("runId")] = body
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /runtime/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /runtime/conversation/mark-read", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// recordingEngine:stub 引擎——记录每次调用的输入,可编排会话 id 序列。
type recordingEngine struct {
	mu       sync.Mutex
	id       string
	calls    []TurnInput
	sessions []string // 第 n 次调用返回 sessions[n](越界取末元素)
}

func (e *recordingEngine) ID() string { return e.id }

func (e *recordingEngine) Run(ctx context.Context, in TurnInput) TurnResult {
	e.mu.Lock()
	e.calls = append(e.calls, in)
	n := len(e.calls) - 1
	sess := ""
	if len(e.sessions) > 0 {
		sess = e.sessions[min(n, len(e.sessions)-1)]
	}
	e.mu.Unlock()
	return TurnResult{SessionID: sess, Output: "stub turn output"}
}

func (s *stubServer) publishWake(convo string) {
	s.mu.Lock()
	fn := s.wakeFn
	s.mu.Unlock()
	if fn != nil {
		fn(convo)
	}
}

// isolateHome:HOME 指向临时目录(daemon 的 ~/.cumora 全部落进去)。
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// 测试节奏:压到毫秒级。
	t.Setenv("CUMORA_HEARTBEAT_MS", "40")
	t.Setenv("CUMORA_AGENT_POLL_MS", "60")
	t.Setenv("CUMORA_INBOX_POLL_MS", "80")
	t.Setenv("CUMORA_HTTP_TIMEOUT_MS", "2000")
	return dir
}

// startDaemonRunner:构造单 runner(绕过 doRun 的引擎探测——stub 引擎
// 直接注入),并保证收尾停止。
func startDaemonRunner(t *testing.T, cfg *DaemonConfig, agent AgentInfo, adapter EngineAdapter) *AgentRunner {
	t.Helper()
	r := newAgentRunner(cfg, agent, adapter)
	r.Start()
	t.Cleanup(func() {
		r.Stop()
		r.wg.Wait()
	})
	return r
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

/* ───────── 配对 ───────── */

func TestPairPersistsConfigAndRequestsShape(t *testing.T) {
	home := isolateHome(t)
	stub := newStubServer()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	// 配对前探测本地引擎:stub 一个"已安装"引擎——通过 PATH 注入。
	bin := filepath.Join(home, "bin")
	_ = os.MkdirAll(bin, 0o755)
	for _, id := range []string{"claude", "codex"} {
		_ = os.WriteFile(filepath.Join(bin, id), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	if err := doPair("PAIRCODE123", srv.URL, ""); err != nil {
		t.Fatalf("doPair: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil || cfg == nil {
		t.Fatalf("config not persisted: %v", err)
	}
	if cfg.ComputerID != "comp-stub-1" || cfg.DeviceToken != "dev-token-1" || cfg.ServerURL != srv.URL {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.pairCalls) != 1 {
		t.Fatalf("expected 1 pair call, got %d", len(stub.pairCalls))
	}
	body := stub.pairCalls[0]
	if body["code"] != "PAIRCODE123" {
		t.Errorf("pair body code: %v", body["code"])
	}
	if body["hostName"] == nil || body["hostName"] == "" {
		t.Errorf("pair body hostName missing: %v", body["hostName"])
	}
	engines, _ := body["engines"].([]any)
	if len(engines) < 2 {
		t.Errorf("pair body engines should list detected engines: %v", engines)
	}
	if v, _ := body["version"].(string); v == "" {
		t.Errorf("pair body version missing")
	}
	if sup, ok := body["supervised"].(bool); !ok {
		t.Errorf("pair body supervised must be an explicit bool, got %T", body["supervised"])
	} else if sup {
		t.Errorf("pair body supervised should be false without CUMORA_SUPERVISED")
	}
	// 探测序保持(claude/codex 按 EngineIDs 序)。
	if engines[0] != "claude" || engines[1] != "codex" {
		t.Errorf("pair body engines order: %v", engines)
	}
}

func TestPairPreferredEngineSortsFirst(t *testing.T) {
	home := isolateHome(t)
	stub := newStubServer()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	bin := filepath.Join(home, "bin")
	_ = os.MkdirAll(bin, 0o755)
	for _, id := range []string{"claude", "codex"} {
		_ = os.WriteFile(filepath.Join(bin, id), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	if err := doPair("CODE2", srv.URL, "codex"); err != nil {
		t.Fatalf("doPair --engine codex: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.pairCalls) != 1 {
		t.Fatalf("expected 1 pair call, got %d", len(stub.pairCalls))
	}
	engines, _ := stub.pairCalls[0]["engines"].([]any)
	if len(engines) == 0 || engines[0] != "codex" {
		t.Fatalf("--engine codex must sort first (server treats engines[0] as default), got %v", engines)
	}
}

func TestPairRejectsUnknownEngine(t *testing.T) {
	home := isolateHome(t)
	bin := filepath.Join(home, "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	err := doPair("CODE", "http://stub.invalid", "not-an-engine")
	if err == nil || !strings.Contains(err.Error(), "--engine must be one of") {
		t.Fatalf("expected --engine validation error, got %v", err)
	}
}

/* ───────── 心跳 + agent 同步 ───────── */

func TestHeartbeatAndAgentSync(t *testing.T) {
	isolateHome(t)
	// 配对态落盘,doRun 才会起;serverUrl 由参数覆盖(评审 M3:心跳必须
	// 由 daemon 主循环自己发出,测试代发无法钉住 daemon 行为)。
	if err := saveConfig(&DaemonConfig{ServerURL: "http://pending.invalid", ComputerID: "comp-stub-1", DeviceToken: "dev-token-1"}); err != nil {
		t.Fatal(err)
	}
	stub := newStubServer()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	RegisterAdapter(&recordingEngine{id: "claude"})
	// doRun 在心跳前先探测 PATH 引擎——放一个假可执行顶住探测。
	bin := filepath.Join(t.TempDir(), "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	stub.mu.Lock()
	stub.agents = []AgentInfo{{ID: "a1", Name: "Atlas", Engine: strPtr("claude")}}
	stub.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- doRun(ctx, srv.URL) }()

	// daemon 自己的心跳:设备令牌 Bearer + 版本号非空。
	waitUntil(t, 6*time.Second, "daemon-issued heartbeat with device bearer", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		for _, hb := range stub.heartbeats {
			if hb.auth == "Bearer dev-token-1" && hb.version != "" {
				return true
			}
		}
		return false
	})
	// agent 同步:doRun 挂载 runner → 建起 wake-stream。
	waitUntil(t, 6*time.Second, "agent sync hosted runner (wake-stream up)", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.wokenStreams >= 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("doRun returned error on context cancel: %v", err)
	}
}

/* ───────── 唤醒消费 → run 生命周期 ───────── */

func TestWakeConsumptionDrivesRunLifecycle(t *testing.T) {
	isolateHome(t)
	// 评审 M3:此前 inbox 恒非空 + 80ms 轮询,SSE 消费全删也能绿——
	// 这里禁用轮询、唤醒前 inbox 恒空,断言 run 只能由 SSE 唤醒产生。
	t.Setenv("CUMORA_INBOX_POLL_MS", "600000")
	stub := newStubServer()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	cfg := &DaemonConfig{ServerURL: srv.URL, ComputerID: "comp-stub-1", DeviceToken: "dev-token-1"}
	engine := &recordingEngine{id: "claude", sessions: []string{"sess-1", "sess-2"}}
	RegisterAdapter(engine)
	agent := AgentInfo{ID: "a1", Name: "Atlas", Engine: strPtr("claude")}

	startDaemonRunner(t, cfg, agent, engine)
	waitUntil(t, 5*time.Second, "wake-stream connected", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.wokenStreams >= 1
	})
	// 流已连、无唤醒、inbox 空 → 睡过 2.5s 防抖窗:重连补拍的防抖
	// 定时在空箱上空发,轮询已禁——此刻必须仍零 run。
	time.Sleep(2700 * time.Millisecond)
	stub.mu.Lock()
	runsBefore := len(stub.runCreates)
	stub.inboxRows = []map[string]any{{"id": "m1", "conversation_id": "cv-1", "kind": "text"}}
	stub.mu.Unlock()
	if runsBefore != 0 {
		t.Fatalf("runs created before any wake: %d (causality broken)", runsBefore)
	}

	stub.publishWake("cv-1")

	waitUntil(t, 6*time.Second, "run created + finished", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return len(stub.runCreates) >= 1 && len(stub.runFinishes) >= 1
	})

	stub.mu.Lock()
	created := stub.runCreates[0]
	trigger, _ := created["trigger"].(map[string]any)
	stub.mu.Unlock()
	if created["inboxCount"] != float64(1) {
		t.Errorf("run inboxCount: %v", created["inboxCount"])
	}
	if trigger["engine"] != "claude" {
		t.Errorf("run trigger.engine: %v", trigger["engine"])
	}
	if trigger["reason"] != "sse-wake" {
		t.Fatalf("run must originate from the SSE wake, got reason=%v", trigger["reason"])
	}

	stub.mu.Lock()
	var finish map[string]any
	for _, f := range stub.runFinishes {
		finish = f
	}
	stub.mu.Unlock()
	if finish["status"] != "completed" {
		t.Errorf("run finish status: %v (full: %v)", finish["status"], finish)
	}

	// session 落盘:第一轮返回 sess-1 → 文件在 ~/.cumora/sessions/a1.session。
	sessFile := filepath.Join(sessionsDir(), "a1.session")
	b, err := os.ReadFile(sessFile)
	if err != nil || strings.TrimSpace(string(b)) != "sess-1" {
		t.Fatalf("session file after run 1: %q err=%v", string(b), err)
	}

	// 第二次唤醒:引擎应收到 resume=sess-1,并推进到 sess-2。
	stub.publishWake("cv-1")
	waitUntil(t, 6*time.Second, "second engine call with resume", func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return len(engine.calls) >= 2 && engine.calls[1].ResumeSessionID == "sess-1"
	})
	b2, _ := os.ReadFile(sessFile)
	if strings.TrimSpace(string(b2)) != "sess-2" {
		t.Fatalf("session file after run 2: %q", string(b2))
	}
}

/* ───────── 会话恢复:重启后从盘上 resume ───────── */

func TestSessionRecoveryAcrossRestart(t *testing.T) {
	isolateHome(t)
	stub := newStubServer()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	cfg := &DaemonConfig{ServerURL: srv.URL, ComputerID: "comp-stub-1", DeviceToken: "dev-token-1"}
	engine := &recordingEngine{id: "claude", sessions: []string{"old-session"}}
	RegisterAdapter(engine)
	agent := AgentInfo{ID: "a9", Name: "Iris", Engine: strPtr("claude")}

	// 预置既有 session(上一世 daemon 留下)。
	_ = os.MkdirAll(sessionsDir(), 0o755)
	_ = os.WriteFile(filepath.Join(sessionsDir(), "a9.session"), []byte("old-session"), 0o600)

	// 与生命周期测试同因:inbox 门控到唤醒之后,run 只能由唤醒产生。
	t.Setenv("CUMORA_INBOX_POLL_MS", "600000")
	r1 := startDaemonRunner(t, cfg, agent, engine)
	waitUntil(t, 5*time.Second, "first stream", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.wokenStreams >= 1
	})
	stub.mu.Lock()
	stub.inboxRows = []map[string]any{{"id": "m1", "conversation_id": "cv-9", "kind": "text"}}
	stub.mu.Unlock()
	stub.publishWake("cv-9")
	waitUntil(t, 6*time.Second, "first run resumed old session", func() bool {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		return len(engine.calls) >= 1 && engine.calls[0].ResumeSessionID == "old-session"
	})
	r1.Stop()

	// 新 runner(模拟 daemon 重启):从盘上恢复,再唤醒 → resume=old-session。
	engine2 := &recordingEngine{id: "claude", sessions: []string{"new-session"}}
	RegisterAdapter(engine2)
	stub.mu.Lock()
	stub.wokenStreams = 0
	stub.mu.Unlock()
	startDaemonRunner(t, cfg, agent, engine2)
	waitUntil(t, 5*time.Second, "second daemon stream", func() bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.wokenStreams >= 1
	})
	stub.publishWake("cv-9")
	waitUntil(t, 6*time.Second, "restarted daemon resumes persisted session", func() bool {
		engine2.mu.Lock()
		defer engine2.mu.Unlock()
		return len(engine2.calls) >= 1 && engine2.calls[0].ResumeSessionID == "old-session"
	})
}

/* ───────── 运维子命令 ───────── */

func TestVersionAndDoctorParity(t *testing.T) {
	isolateHome(t)
	// --version 打印非空版本。
	ver := currentVersion()
	if strings.TrimSpace(ver) == "" {
		t.Fatal("version empty")
	}
	// doctor:未配对 + 无引擎时不 panic(输出形状人工可验)。
	doctor()
	// 未配对启动:非零路径(doRun 返回错误)。
	if err := doRun(context.Background(), ""); err == nil {
		t.Fatal("expected error when not paired")
	}
	// 空引擎探测:requireLocalEngine 报 TS 同错,doRun 包哨兵(退出码 70)。
	t.Setenv("PATH", "/nonexistent")
	if _, err := requireLocalEngine(); err == nil || !strings.Contains(err.Error(), "no supported local agent engine") {
		t.Fatalf("expected engine-missing error, got %v", err)
	}
}

func strPtr(s string) *string { return &s }
