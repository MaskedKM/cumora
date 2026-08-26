// runner_session_test —— #64 验收:daemon 侧会话接线的协议级测试——
// 持久会话优先于一次性 Run、standing prompt 带外/内联合并、逐跳台账上
// 送、busy 窗的直接 ping steer 门(DM/@我/人类 vs 群消息节流去重)。
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* ───────── 测试替身 ───────── */

// fakeEngineSession:记录 Send/Steer 的可控会话。
type fakeEngineSession struct {
	mu      sync.Mutex
	sends   []string
	steers  []string
	sid     string
	alive   bool
	carries bool
	// sendResult:每次 Send 的返回(缺省零值成功)。
	sendResult func(prompt string) RunResult
	// onSend:Send 时的副作用钩子。
	onSend func(prompt string)
	// onHop:由 sessionAdapter.StartSession 注入(runner 传给真实会话的
	// 逐跳回调)——Send 时真发一跳,台账链路才能被测到。
	onHop func(HopReport)
}

func (f *fakeEngineSession) Send(prompt string) RunResult {
	f.mu.Lock()
	f.sends = append(f.sends, prompt)
	hook, onHop := f.onSend, f.onHop
	f.mu.Unlock()
	if hook != nil {
		hook(prompt)
	}
	if onHop != nil {
		in, out := int64(11), int64(7)
		onHop(HopReport{Model: "claude-fake-hop", Usage: EngineUsage{InputTokens: &in, OutputTokens: &out}, HopIndex: 1, ToolUses: 1, TextChars: 20})
	}
	if f.sendResult != nil {
		return f.sendResult(prompt)
	}
	return RunResult{ExitCode: 0, SessionID: f.sid}
}

func (f *fakeEngineSession) Steer(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers = append(f.steers, text)
}

func (f *fakeEngineSession) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive
}

func (f *fakeEngineSession) SessionID() string { return f.sid }

func (f *fakeEngineSession) CarriesStandingPrompt() bool { return f.carries }

func (f *fakeEngineSession) Stop() {
	f.mu.Lock()
	f.alive = false
	f.mu.Unlock()
}

// sessionAdapter:可编排的适配器——StartSession 返回注入的会话(nil = 降级)。
type sessionAdapter struct {
	id        string
	session   EngineSession
	runCalls  []RunArgs
	startArgs []SessionArgs
}

func (a *sessionAdapter) ID() string  { return a.id }
func (a *sessionAdapter) Bin() string { return a.id }

func (a *sessionAdapter) SeedHome(home string, p Persona) error { return nil }

func (a *sessionAdapter) StartSession(args SessionArgs) EngineSession {
	a.startArgs = append(a.startArgs, args)
	if fs, ok := a.session.(*fakeEngineSession); ok {
		fs.mu.Lock()
		fs.onHop = args.OnHopUsage
		fs.mu.Unlock()
	}
	return a.session
}

func (a *sessionAdapter) Run(ctx context.Context, in RunArgs) RunResult {
	a.runCalls = append(a.runCalls, in)
	return RunResult{ExitCode: 0, SessionID: "sess-oneshot"}
}

func (a *sessionAdapter) Classify(ctx context.Context, args ClassifyArgs) ClassifyResult {
	return ClassifyResult{Text: "stub"}
}

func (a *sessionAdapter) Probe(ctx context.Context, args ProbeArgs) ClassifyResult {
	return ClassifyResult{Text: "OK"}
}

func (a *sessionAdapter) ProbeWake(ctx context.Context, args WakeProbeArgs) WakeProbeResult {
	return WakeProbeResult{OK: true, Skipped: true}
}

/* ───────── stub server 扩展:/runtime/llm-calls 观测 ───────── */

func (s *stubServer) llmCallsBodies() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any{}, s.llmCalls...)
}

func registerLlmCallsStub(s *stubServer, mux *http.ServeMux) {
	mux.HandleFunc("POST /runtime/llm-calls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.llmCalls = append(s.llmCalls, body)
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})
}

func newSessionTestStack(t *testing.T, inboxRows []map[string]any) (*stubServer, *DaemonConfig) {
	t.Helper()
	stub := newStubServer()
	mux := stub.handler().(*http.ServeMux)
	registerLlmCallsStub(stub, mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	stub.mu.Lock()
	stub.inboxRows = inboxRows
	stub.mu.Unlock()
	return stub, &DaemonConfig{ServerURL: srv.URL, ComputerID: "comp-stub-1", DeviceToken: "dev-token-1"}
}

/* ───────── turnPrompt 合并语义 ───────── */

func TestTurnPromptMergeSemantics(t *testing.T) {
	isolateHome(t)
	adapter := &sessionAdapter{id: "claude"}
	r := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, AgentInfo{ID: "agent-merge", Name: "M"}, adapter)

	carrying := &fakeEngineSession{carries: true, alive: true}
	if got := r.turnPrompt(carrying, "DELTA"); got != "DELTA" {
		t.Fatalf("carrying session must receive ONLY the delta, got %d bytes", len(got))
	}
	merged := r.turnPrompt(nil, "DELTA")
	if !strings.Contains(merged, "════════") || !strings.Contains(merged, "DELTA") {
		t.Fatal("one-shot prompt must inline standing + separator + delta")
	}
	golden := mustGolden(t, "standing_prompt.txt")
	want := strings.Replace(golden, "--assignee test-agent ", "--assignee agent-merge ", 1)
	if !strings.HasPrefix(merged, want) {
		t.Fatalf("inlined standing prompt drift (want %d-byte prefix)", len(want))
	}
}

/* ───────── runTurn:持久会话路由 + 台账上送 ───────── */

func TestRunTurnUsesPersistentSessionAndReportsHops(t *testing.T) {
	isolateHome(t)
	t.Setenv("CUMORA_INBOX_POLL_MS", "600000")
	stub, cfg := newSessionTestStack(t, []map[string]any{
		{"id": "m1", "conversation_id": "cv-1", "kind": "text", "body": "hello", "author_name": "Ann"},
	})
	sess := &fakeEngineSession{sid: "sess-live", alive: true, carries: true}
	adapter := &sessionAdapter{id: "claude", session: sess}
	r := newAgentRunner(cfg, AgentInfo{ID: "a1", Name: "Atlas", Engine: strp("claude")}, adapter)
	r.Start()
	t.Cleanup(func() { r.Stop(); r.wg.Wait() })

	if err := r.runTurn("sse-wake"); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if len(adapter.runCalls) != 0 {
		t.Fatalf("persistent session available — one-shot Run must NOT fire, got %d", len(adapter.runCalls))
	}
	sess.mu.Lock()
	sends, sid := len(sess.sends), sess.sid
	sess.mu.Unlock()
	if sends != 1 {
		t.Fatalf("session sends: %d", sends)
	}
	// carries=true → 只发增量:不带 ══ 分隔器与 standing 正文。
	if got := func() string { sess.mu.Lock(); defer sess.mu.Unlock(); return sess.sends[0] }(); strings.Contains(got, "════════") {
		t.Fatal("carrying session must not receive the inlined standing prompt")
	} else if !strings.Contains(got, "cv-1") || !strings.Contains(got, "hello") {
		t.Fatalf("delta must carry the inbox digest; got:\n%s", got)
	}
	// StartSession 收到 golden 级 standing prompt + resume(盘上无 → 空)。
	if len(adapter.startArgs) != 1 {
		t.Fatalf("startArgs: %d", len(adapter.startArgs))
	}
	sa := adapter.startArgs[0]
	golden := mustGolden(t, "standing_prompt.txt")
	want := strings.Replace(golden, "--assignee test-agent ", "--assignee a1 ", 1)
	if sa.StandingPrompt != want {
		t.Fatalf("standing prompt drift (%d vs %d bytes)", len(sa.StandingPrompt), len(want))
	}
	if sa.ResumeSessionID != "" {
		t.Fatalf("fresh runner must resume nothing, got %q", sa.ResumeSessionID)
	}
	// session id 落盘 + 重启恢复源。
	b, _ := os.ReadFile(filepath.Join(sessionsDir(), "a1.session"))
	if strings.TrimSpace(string(b)) != sid || sid != "sess-live" {
		t.Fatalf("session persisted: %q (sess.sid=%q)", string(b), sid)
	}
	// 逐跳台账:hop → /runtime/llm-calls(source=byoa-claude,挂 runId)。
	waitUntil(t, 5*time.Second, "llm-calls batch posted", func() bool { return len(stub.llmCallsBodies()) > 0 })
	bodies := stub.llmCallsBodies()
	body := bodies[0]
	if body["source"] != "byoa-claude" {
		t.Fatalf("source: %v", body["source"])
	}
	hops, _ := body["hops"].([]any)
	if len(hops) != 1 {
		t.Fatalf("hops: %d", len(hops))
	}
	hop, _ := hops[0].(map[string]any)
	if hop["purpose"] != "agent-turn" || hop["model"] == "" {
		t.Fatalf("hop shape: %v", hop)
	}

}

func TestRunTurnFallsBackToOneShotWithInlinedStanding(t *testing.T) {
	isolateHome(t)
	t.Setenv("CUMORA_INBOX_POLL_MS", "600000")
	stub, cfg := newSessionTestStack(t, []map[string]any{
		{"id": "m1", "conversation_id": "cv-2", "kind": "text", "body": "fallback"},
	})
	adapter := &sessionAdapter{id: "claude", session: nil} // 无持久模式
	_ = stub
	// 预置上一轮 session(盘上)→ 一次性路径必须带上 resume。
	_ = os.MkdirAll(sessionsDir(), 0o755)
	_ = os.WriteFile(filepath.Join(sessionsDir(), "a2.session"), []byte("sess-prev"), 0o600)
	r := newAgentRunner(cfg, AgentInfo{ID: "a2", Name: "Iris", Engine: strp("claude")}, adapter)
	r.Start()
	t.Cleanup(func() { r.Stop(); r.wg.Wait() })

	if err := r.runTurn("poll"); err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if len(adapter.runCalls) != 1 {
		t.Fatalf("one-shot run calls: %d", len(adapter.runCalls))
	}
	call := adapter.runCalls[0]
	if call.ResumeSessionID != "sess-prev" {
		t.Fatalf("one-shot must resume persisted session, got %q", call.ResumeSessionID)
	}
	if !strings.Contains(call.Prompt, "════════") {
		t.Fatal("one-shot prompt must inline the standing prompt (no out-of-band channel)")
	}
	// 结果 session id 落盘(供下轮 resume)。
	b, _ := os.ReadFile(filepath.Join(sessionsDir(), "a2.session"))
	if strings.TrimSpace(string(b)) != "sess-oneshot" {
		t.Fatalf("session advanced: %q", string(b))
	}
}

/* ───────── maybeSteer:直接 ping 注入 / 群消息节流去重 ───────── */

func TestMaybeSteerDirectMessageGate(t *testing.T) {
	isolateHome(t)
	t.Setenv("CUMORA_BYOA_STEER_GROUP_INTERVAL_MS", "60000")
	stub, cfg := newSessionTestStack(t, nil)
	sess := &fakeEngineSession{alive: true}
	adapter := &sessionAdapter{id: "claude", session: sess}
	r := newAgentRunner(cfg, AgentInfo{ID: "steerable", Name: "S", Engine: strp("claude")}, adapter)
	r.mu.Lock()
	r.engineSession = sess
	r.busy = true // steer 只在 turn 在飞时有意义
	r.mu.Unlock()
	t.Cleanup(func() { r.Stop(); r.wg.Wait() })

	setInbox := func(rows ...map[string]any) {
		stub.mu.Lock()
		stub.inboxRows = rows
		stub.mu.Unlock()
	}

	// 群聊 + 无 @:不是直接 ping → 内容无关群提点(选配默认开)。
	setInbox(map[string]any{"id": "g1", "conversation_id": "cv-g", "conversation_kind": "group", "author_kind": "agent", "body": "chatter", "author_name": "Bot"})
	r.maybeSteer("cv-g")
	sess.mu.Lock()
	steers := len(sess.steers)
	sess.mu.Unlock()
	if steers != 1 {
		t.Fatalf("group notice expected exactly one steer, got %d", steers)
	}
	sess.mu.Lock()
	first := sess.steers[0]
	sess.mu.Unlock()
	if !strings.Contains(first, "bodies withheld") || !strings.Contains(first, "cv-g") {
		t.Fatalf("group steer must be content-free: %q", first)
	}

	// 群提点节流:同窗第二条(不同 id)不再注入。
	setInbox(map[string]any{"id": "g2", "conversation_id": "cv-g", "conversation_kind": "group", "author_kind": "agent", "body": "more"})
	r.maybeSteer("cv-g")
	sess.mu.Lock()
	steers = len(sess.steers)
	sess.mu.Unlock()
	if steers != 1 {
		t.Fatalf("group steer must be throttled, got %d", steers)
	}

	// 直接 ping(DM)→ 注入并指示简答后继续。
	setInbox(map[string]any{"id": "d1", "conversation_id": "cv-d", "conversation_kind": "direct", "author_kind": "human", "body": "you there?", "author_name": "Ann"})
	r.maybeSteer("cv-d")
	sess.mu.Lock()
	if len(sess.steers) != 2 {
		sess.mu.Unlock()
		t.Fatalf("direct steer must fire, got %d", len(sess.steers))
	}
	direct := sess.steers[1]
	sess.mu.Unlock()
	if !strings.Contains(direct, "A direct message arrived") || !strings.Contains(direct, "you there?") || !strings.Contains(direct, "cv-d") {
		t.Fatalf("direct steer text: %q", direct)
	}

	// 重复唤醒同一条直接消息 → 去重。
	r.maybeSteer("cv-d")
	sess.mu.Lock()
	if len(sess.steers) != 2 {
		sess.mu.Unlock()
		t.Fatal("repeated wake for the same direct message must dedupe")
	}
	sess.mu.Unlock()

	// @我 的群消息也算直接 ping。
	setInbox(map[string]any{"id": "m2", "conversation_id": "cv-g2", "conversation_kind": "group", "author_kind": "agent", "body": "hey @steerable ping", "author_name": "Bot"})
	r.maybeSteer("cv-g2")
	sess.mu.Lock()
	if len(sess.steers) != 3 {
		sess.mu.Unlock()
		t.Fatalf("@mention steer must fire, got %d", len(sess.steers))
	}
	sess.mu.Unlock()
}

func TestMaybeSteerNoopWhenSessionMissing(t *testing.T) {
	isolateHome(t)
	_, cfg := newSessionTestStack(t, nil)
	adapter := &sessionAdapter{id: "claude", session: nil}
	r := newAgentRunner(cfg, AgentInfo{ID: "a5", Name: "N", Engine: strp("claude")}, adapter)
	// 无会话(或已死):不 panic、不访问 inbox 也无妨——直接返回。
	r.maybeSteer("cv-x")
}

// M2 回归:错误脱敏的顺序与形态——agent home(是 $HOME 的子路径)必须先
// 替换成 <agent home>,否则 $HOME 先吞掉它、细节泄漏为 ~ 路径。
func TestVisibleEngineErrorRedaction(t *testing.T) {
	isolateHome(t)
	adapter := &sessionAdapter{id: "claude"}
	r := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, AgentInfo{ID: "redact", Name: "R"}, adapter)
	raw := "process exited with code 7\n" +
		"Error: cannot read " + r.home + "/memory/MEMORY.md\n" +
		"config at " + homeDir() + "/.zcode/cli/config.json is malformed\n" +
		"\x1b[31mnoise\x1b[0m\r\n"
	got := r.visibleEngineError(7, raw)
	if !strings.Contains(got, "<agent home>/memory/MEMORY.md") {
		t.Fatalf("agent home must redact to <agent home>: %q", got)
	}
	if strings.Contains(got, r.home) || strings.Contains(got, homeDir()+"/.cumora") {
		t.Fatalf("agent home path leaked: %q", got)
	}
	if !strings.Contains(got, "~/.zcode/cli/config.json") {
		t.Fatalf("operator HOME must redact to ~: %q", got)
	}
	if strings.Contains(got, "\x1b[31m") || strings.Contains(got, "\r") {
		t.Fatalf("ANSI/CR must be stripped: %q", got)
	}
	if !strings.HasPrefix(got, "local claude failed (exit 7): ") {
		t.Fatalf("wrapper shape: %q", got)
	}
}
