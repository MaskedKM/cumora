// grok_cursor_test —— #66 验收:grok(ACP stdio 持久会话 + 一次性)与
// cursor(纯一次性 stream-json)适配器的协议级测试。假 CLI 按 ACP/
// stream-json 真实线上协议说话;真机冒烟 env 门控另置。
package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

/* ───────── grok:launcher ───────── */

func TestResolveGrokBin(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "grok"), []byte("#!/bin/sh\n"), 0o755)
	if got := resolveGrokBin([]string{"PATH=" + bin}); got != filepath.Join(bin, "grok") {
		t.Fatalf("PATH wins: %q", got)
	}
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".grok", "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".grok", "bin", "grok"), []byte("#!/bin/sh\n"), 0o755)
	if got := resolveGrokBin([]string{"PATH=" + root + "/empty", "HOME=" + home}); got != filepath.Join(home, ".grok", "bin", "grok") {
		t.Fatalf("~/.grok/bin fallback: %q", got)
	}
	if got := resolveGrokBin([]string{"PATH=" + root + "/empty", "HOME=" + t.TempDir()}); got != "" {
		t.Fatalf("absent: %q", got)
	}
}

/* ───────── grok:ACP 会话 ───────── */

const fakeGrokAcp = `#!/bin/sh
echo "$@" > "$FAKE_T/grok-argv.txt"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([a-zA-Z/._]*\)".*/\1/p')
  case "$method" in
    initialize)
      printf '%s\n' "$line" > "$FAKE_T/grok-initialize.json"
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    session/new)
      printf '%s\n' "$line" > "$FAKE_T/grok-sessionnew.json"
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"grok-sess-1"}}\n' "$id"
      ;;
    session/prompt)
      printf '%s\n' "$line" > "$FAKE_T/grok-prompt.json"
      printf '{"jsonrpc":"2.0","method":"_x.ai/models/update","params":{"currentModelId":"grok-4.5-fast"}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","title":"Read inbox"}}}\n'
      printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"working on it"}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"_meta":{"usage":{"input_tokens":50,"outputTokens":25,"cache_read_input_tokens":10}}}}\n' "$id"
      ;;
    session/load)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"message":"session expired"}}\n' "$id"
      ;;
  esac
done
`

func TestGrokSessionHandshakeAndTurn(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "grok"), fakeGrokAcp)
	var mu sync.Mutex
	var hops []HopReport
	var logs []string
	sess := (grokAdapter{}).StartSession(SessionArgs{
		Home:           t.TempDir(),
		Env:            os.Environ(),
		StandingPrompt: "stand by",
		OnLog:          func(l string) { mu.Lock(); logs = append(logs, l); mu.Unlock() },
		OnHopUsage:     func(r HopReport) { mu.Lock(); hops = append(hops, r); mu.Unlock() },
	})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()
	if !sess.CarriesStandingPrompt() {
		t.Fatal("rules meta must carry the standing prompt")
	}
	waitUntil(t, 5*time.Second, "session ready", func() bool { return sess.SessionID() == "grok-sess-1" })
	// standing prompt 随 session/new 的 _meta.rules 投递。
	if got := readObs(t, "grok-sessionnew.json"); !strings.Contains(got, `"rules":"stand by"`) || !strings.Contains(got, `"yoloMode":true`) {
		t.Fatalf("session/new _meta shape: %s", got)
	}
	res := sess.Send("grok turn")
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("turn: %+v", res)
	}
	if res.SessionID != "grok-sess-1" {
		t.Fatalf("sid: %q", res.SessionID)
	}
	// 双命名 usage 折算(result._meta.usage:input_tokens snake +
	// outputTokens/cache_read_input_tokens camel)。
	if res.Usage == nil || *res.Usage.InputTokens != 50 || *res.Usage.OutputTokens != 25 || *res.Usage.CacheReadInputTokens != 10 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	// 模型归因:_x.ai/models/update 播报的实跑模型。
	if res.Model != "grok-4.5-fast" {
		t.Fatalf("model: %q", res.Model)
	}
	mu.Lock()
	if len(hops) != 1 || hops[0].Model != "grok-4.5-fast" {
		t.Fatalf("hops: %+v", hops)
	}
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "[grok] tool Read inbox") || !strings.Contains(joined, "[grok] » working on it") {
		t.Fatalf("session/update signal lines; logs:\n%s", joined)
	}
	// steer:no-op + 一次性告警。
	sess.Steer("ping")
	sess.Steer("ping again")
	mu.Lock()
	total := strings.Count(strings.Join(logs, "\n"), "same-turn steer is not supported")
	mu.Unlock()
	if total != 1 {
		t.Fatalf("steer warning must fire exactly once, got %d", total)
	}
	// MINOR 2:initialize 握手形状钉死(protocolVersion/fs 关闭)。
	if got := readObs(t, "grok-initialize.json"); !strings.Contains(got, `"protocolVersion":1`) || !strings.Contains(got, `"readTextFile":false`) || !strings.Contains(got, `"writeTextFile":false`) {
		t.Fatalf("initialize handshake shape: %s", got)
	}
}

func TestGrokSessionLoadFallsBackToFresh(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "grok"), fakeGrokAcp)
	var logs []string
	sess := (grokAdapter{}).StartSession(SessionArgs{
		Home:            t.TempDir(),
		Env:             os.Environ(),
		ResumeSessionID: "grok-stale",
		OnLog:           func(l string) { logs = append(logs, l) },
	})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()
	waitUntil(t, 5*time.Second, "fresh session after load failure", func() bool { return sess.SessionID() == "grok-sess-1" })
	if !strings.Contains(strings.Join(logs, "\n"), "session/load failed (session expired) — starting a fresh session") {
		t.Fatalf("load fallback must log; logs:\n%s", strings.Join(logs, "\n"))
	}
	res := sess.Send("after fallback")
	if res.ExitCode != 0 {
		t.Fatalf("turn after fallback: %+v", res)
	}
}

func TestGrokSessionBusyGate(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "grok"), `#!/bin/sh
while IFS= read -r line; do
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([a-zA-Z/._]*\)".*/\1/p')
  case "$method" in
    initialize) id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'); printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
    session/new) id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'); printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"s"}}\n' "$id" ;;
    session/prompt) printf '%s\n' "$line" > "$FAKE_T/grok-busy.json"; sleep 1; id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p'); printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
  esac
done
`)
	sess := (grokAdapter{}).StartSession(SessionArgs{Home: t.TempDir(), Env: os.Environ(), OnLog: func(string) {}})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()
	waitUntil(t, 5*time.Second, "ready", func() bool { return sess.SessionID() == "s" })
	resCh := make(chan RunResult, 1)
	go func() { resCh <- sess.Send("first") }()
	waitFileContains(t, "grok-busy.json", "first", 5*time.Second)
	second := sess.Send("second")
	if second.ExitCode == 0 || !strings.Contains(second.Err, "busy") {
		t.Fatalf("busy gate: %+v", second)
	}
	if r := <-resCh; r.ExitCode != 0 {
		t.Fatalf("first turn: %+v", r)
	}
}

/* ───────── grok:适配器面 ───────── */

func TestGrokClassifyEnvelope(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "grok"), `#!/bin/sh
echo "$@" > "$FAKE_T/grok-classify-argv.txt"
printf '%s\n' '{"text":"GROK-VERDICT","usage":{"input_tokens":8,"output_tokens":3}}'
`)
	res := (grokAdapter{}).Classify(context.Background(), ClassifyArgs{Cwd: t.TempDir(), Prompt: "p", Env: os.Environ(), OnLog: func(string) {}})
	if res.Err != "" || res.Text != "GROK-VERDICT" {
		t.Fatalf("classify: %+v", res)
	}
	if res.Usage == nil || *res.Usage.InputTokens != 8 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	argv := readObs(t, "grok-classify-argv.txt")
	for _, want := range []string{"--model", "grok-4.5", "--output-format", "json", "--always-approve", "--no-auto-update"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("classify argv missing %q: %s", want, argv)
		}
	}
}

func TestGrokRunOneShot(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "grok"), `#!/bin/sh
echo "$@" > "$FAKE_T/grok-run-argv.txt"
printf '%s\n' '{"session_id":"g-1","type":"result","usage":{"input_tokens":1,"output_tokens":2},"model":"grok-4.5"}'
`)
	res := (grokAdapter{}).Run(context.Background(), RunArgs{Home: t.TempDir(), Prompt: "go", Env: os.Environ(), Model: "grok-4.5", ResumeSessionID: "g-prev", OnLog: func(string) {}})
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("run: %+v", res)
	}
	argv := readObs(t, "grok-run-argv.txt")
	for _, want := range []string{"-p", "--resume", "g-prev", "--model", "grok-4.5", "--output-format", "streaming-messages-json", "--always-approve", "--no-auto-update", "go"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("run argv missing %q: %s", want, argv)
		}
	}
}

func TestGrokProbeWakeHandshakeAndGates(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "grok"), `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([a-zA-Z/._]*\)".*/\1/p')
  case "$method" in
    initialize) printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
    session/new) printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"s"}}\n' "$id" ;;
  esac
done
`)
	r := (grokAdapter{}).ProbeWake(context.Background(), WakeProbeArgs{Cwd: t.TempDir(), Env: os.Environ()})
	if !r.OK || r.Detail != "" {
		t.Fatalf("probeWake: %+v", r)
	}
	t.Setenv("CUMORA_GROK_ARGS", "--x")
	if r2 := (grokAdapter{}).ProbeWake(context.Background(), WakeProbeArgs{}); !r2.Skipped {
		t.Fatalf("args gate: %+v", r2)
	}
	t.Setenv("CUMORA_GROK_ARGS", "")
	t.Setenv("CUMORA_GROK_NO_ACP", "1")
	if r3 := (grokAdapter{}).ProbeWake(context.Background(), WakeProbeArgs{}); !r3.Skipped {
		t.Fatalf("opt-out gate: %+v", r3)
	}
	t.Setenv("CUMORA_GROK_NO_ACP", "")
	if (grokAdapter{}).StartSession(SessionArgs{Env: os.Environ()}) == nil {
		t.Fatal("ACP must be available without gates")
	}
}

func TestGrokSeedHomeWriteOnce(t *testing.T) {
	home := t.TempDir()
	p := Persona{ID: "a1", Name: "Atlas", Role: strp("Tester"), SystemPrompt: strp("Be terse.")}
	if err := (grokAdapter{}).SeedHome(home, p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	if string(b) != mustGolden(t, "persona_full.txt") {
		t.Fatal("grok AGENTS.md uses the DEFAULT persona header (like TS)")
	}
	// write-once:既有 AGENTS.md 不覆盖(grok 把规则并入会话状态,重写=重置)。
	agentWritten := "agent's own rules\n"
	_ = os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte(agentWritten), 0o644)
	if err := (grokAdapter{}).SeedHome(home, p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	if string(got) != agentWritten {
		t.Fatal("grok seedHome must be write-once")
	}
}

/* ───────── cursor ───────── */

func fakeCursorBin(t *testing.T, body string) {
	t.Helper()
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "cursor-agent"), body)
}

func TestCursorTurnFoldsStream(t *testing.T) {
	fakeCursorBin(t, `#!/bin/sh
echo "$@" > "$FAKE_T/cursor-argv.txt"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"cur-sess-1","model":"cursor-fast"}'
printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"CURSOR-REPLY"}]}}'
printf '%s\n' '{"type":"result","is_error":false,"usage":{"inputTokens":7,"outputTokens":4,"cacheReadTokens":2,"cacheWriteTokens":1}}'
`)
	var hops []HopReport
	res, text := func() (RunResult, string) {
		return (cursorAdapter{}).turn(context.Background(), "do it", t.TempDir(), os.Environ(), func(string) {}, "pinned-model", "cur-prev", func(r HopReport) { hops = append(hops, r) })
	}()
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("turn: %+v", res)
	}
	argv := readObs(t, "cursor-argv.txt")
	for _, want := range []string{"-p", "--resume", "cur-prev", "--model", "pinned-model", "--output-format", "stream-json", "--force", "--trust", "do it"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("turn argv missing %q: %s", want, argv)
		}
	}
	if res.SessionID != "cur-sess-1" || text != "CURSOR-REPLY" {
		t.Fatalf("fold: sid=%q text=%q", res.SessionID, text)
	}
	// init 报告的实跑模型胜过 pin。
	if res.Model != "cursor-fast" {
		t.Fatalf("model: %q", res.Model)
	}
	if res.Usage == nil || *res.Usage.InputTokens != 7 || *res.Usage.CacheCreationInputTokens != 1 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	if len(hops) != 1 || hops[0].Model != "cursor-fast" || hops[0].TextChars != len("CURSOR-REPLY") {
		t.Fatalf("hops: %+v", hops)
	}
}

func TestCursorStreamIsTruth(t *testing.T) {
	// is_error:true + 退出码 0 → 失败轮(流说了算,退出码不背书)。
	fakeCursorBin(t, `#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s","model":"m"}'
printf '%s\n' '{"type":"result","is_error":true,"result":"model unavailable"}'
`)
	res, _ := (cursorAdapter{}).turn(context.Background(), "p", t.TempDir(), os.Environ(), nil, "", "", nil)
	if res.ExitCode == 0 || !strings.Contains(res.Err, "model unavailable") {
		t.Fatalf("is_error must fail the turn: %+v", res)
	}
	// 干净退出但无 result(run 的 requireResult)→ 失败;classify 容忍。
	fakeCursorBin(t, `#!/bin/sh
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s","model":"m"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"partial text"}]}}'
`)
	res2, _ := (cursorAdapter{}).turn(context.Background(), "p", t.TempDir(), os.Environ(), nil, "", "", nil)
	if res2.ExitCode == 0 || !strings.Contains(res2.Err, "without a result event") {
		t.Fatalf("requireResult: %+v", res2)
	}
	r3 := (cursorAdapter{}).Classify(context.Background(), ClassifyArgs{Cwd: t.TempDir(), Prompt: "p", Env: os.Environ()})
	if r3.Err != "" || r3.Text != "partial text" {
		t.Fatalf("classify tolerates no-result streams: %+v", r3)
	}
}

func TestCursorAskModeAndOverrides(t *testing.T) {
	fakeCursorBin(t, `#!/bin/sh
echo "$@" > "$FAKE_T/cursor-ask-argv.txt"
printf '%s\n' '{"type":"result","is_error":false,"usage":{"inputTokens":1,"outputTokens":1}}'
`)
	res := (cursorAdapter{}).Classify(context.Background(), ClassifyArgs{Cwd: t.TempDir(), Prompt: "q", Env: os.Environ()})
	if res.Err != "" {
		t.Fatalf("classify: %+v", res)
	}
	argv := readObs(t, "cursor-ask-argv.txt")
	if !strings.Contains(argv, "--mode ask") || strings.Contains(argv, "--force") {
		t.Fatalf("ask mode must be read-only: %s", argv)
	}
	// triage 旗覆盖 → 纯 print。
	t.Setenv("CUMORA_TRIAGE_ARGS", "--custom")
	res2 := (cursorAdapter{}).Classify(context.Background(), ClassifyArgs{Cwd: t.TempDir(), Prompt: "q2", Env: os.Environ()})
	_ = res2
	if got := readObs(t, "cursor-ask-argv.txt"); !strings.Contains(got, "--custom") || strings.Contains(got, "--mode") {
		t.Fatalf("triage override argv: %s", got)
	}
	t.Setenv("CUMORA_TRIAGE_ARGS", "")
	// CUMORA_CURSOR_ARGS 整套覆盖保 --resume。
	t.Setenv("CUMORA_CURSOR_ARGS", "--my-flags")
	res3 := (cursorAdapter{}).Run(context.Background(), RunArgs{Home: t.TempDir(), Prompt: "r", Env: os.Environ(), ResumeSessionID: "c-prev"})
	_ = res3
	if got := readObs(t, "cursor-ask-argv.txt"); !strings.Contains(got, "--my-flags") || !strings.Contains(got, "--resume c-prev") || !strings.Contains(got, "r") {
		t.Fatalf("cursor args override: %s", got)
	}
}

func TestCursorSeedHomeGolden(t *testing.T) {
	home := t.TempDir()
	p := Persona{ID: "a1", Name: "Atlas", Role: strp("Tester"), SystemPrompt: strp("Be terse.")}
	if err := (cursorAdapter{}).SeedHome(home, p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	got := string(b)
	goldenUpdate("persona_cursor.txt", got)
	if got != mustGolden(t, "persona_cursor.txt") {
		t.Fatalf("AGENTS.md drift (%d bytes)", len(b))
	}
	if !pathExists(filepath.Join(home, ".cursor", "skills")) {
		t.Fatal(".cursor/skills missing")
	}
	if (cursorAdapter{}).StartSession(SessionArgs{}) != nil {
		t.Fatal("cursor has no persistent session")
	}
}

func TestEngineBinCursorMapping(t *testing.T) {
	if engineBin("cursor") != "cursor-agent" {
		t.Fatalf("cursor maps to cursor-agent: %q", engineBin("cursor"))
	}
}

// MINOR 3:probe 的 argv 形状(grok small→grok-4.5;cursor small→
// CUMORA_TRIAGE_MODEL,缺省不加 --model)。
func TestGrokAndCursorProbeArgv(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "grok"), `#!/bin/sh
echo "$@" > "$FAKE_T/grok-probe-argv.txt"
printf '%s\n' '{"text":"OK"}'
`)
	res := grokAdapter{}.Probe(context.Background(), ProbeArgs{Tier: "small", Cwd: t.TempDir(), Env: os.Environ()})
	if res.Err != "" {
		t.Fatalf("grok probe: %+v", res)
	}
	if got := readObs(t, "grok-probe-argv.txt"); !strings.Contains(got, "--model grok-4.5") {
		t.Fatalf("grok small probe must pin grok-4.5: %s", got)
	}
	res = grokAdapter{}.Probe(context.Background(), ProbeArgs{Tier: "big", Cwd: t.TempDir(), Env: os.Environ()})
	if got := readObs(t, "grok-probe-argv.txt"); strings.Contains(got, "--model") {
		t.Fatalf("grok big probe must not pin: %s", got)
	}
	writeScript(t, filepath.Join(bin, "cursor-agent"), `#!/bin/sh
echo "$@" > "$FAKE_T/cursor-probe-argv.txt"
printf '%s\n' '{"type":"result","is_error":false}'
`)
	t.Setenv("CUMORA_TRIAGE_MODEL", "my-alias")
	if r := (cursorAdapter{}).Probe(context.Background(), ProbeArgs{Tier: "small", Cwd: t.TempDir(), Env: os.Environ()}); r.Err != "" {
		t.Fatalf("cursor probe small: %+v", r)
	}
	if got := readObs(t, "cursor-probe-argv.txt"); !strings.Contains(got, "--model my-alias") {
		t.Fatalf("cursor small probe must pin CUMORA_TRIAGE_MODEL: %s", got)
	}
	t.Setenv("CUMORA_TRIAGE_MODEL", "")
	if r := (cursorAdapter{}).Probe(context.Background(), ProbeArgs{Tier: "big", Cwd: t.TempDir(), Env: os.Environ()}); r.Err != "" {
		t.Fatalf("cursor probe big: %+v", r)
	}
	if got := readObs(t, "cursor-probe-argv.txt"); strings.Contains(got, "--model") {
		t.Fatalf("cursor big probe must not pin: %s", got)
	}
}
