// engines_protocol_test —— #64 验收:claude/codex 适配器的协议级测试。
// 假 CLI(PATH 上的 sh 脚本)按各引擎的真实线上协议说话:claude 的
// stream-json 事件流、codex 的 app-server JSON-RPC 握手/turn 生命周期。
// 不碰真实引擎、不碰网络;真机冒烟在 smoke_real_test.go(env 门控)。
package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngineDir:临时 bin 目录 + 观测目录,脚本经 $FAKE_T 输出观测物。
// fakeCodexHome:预垫 .git 的临时 home——app-server 的 StartSession 需要
// 垫仓库,而 alpine 工具镜像可能没有 git(真机 daemon 环境必有)。
func fakeCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".git"), 0o755)
	return home
}

func fakeEngineDir(t *testing.T) (bin, obs string) {
	t.Helper()
	root := t.TempDir()
	bin, obs = filepath.Join(root, "bin"), filepath.Join(root, "obs")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.MkdirAll(obs, 0o755)
	t.Setenv("FAKE_T", obs)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin, obs
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readObs(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_T"), name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func waitFileContains(t *testing.T, name, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_T"), name)); err == nil && strings.Contains(string(b), needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting %s to contain %q; got:\n%s", name, needle, readObsQuiet(t, name))
}

func readObsQuiet(t *testing.T, name string) string {
	b, _ := os.ReadFile(filepath.Join(os.Getenv("FAKE_T"), name))
	return string(b)
}

/* ───────── Claude:持久会话 ───────── */

// happy 路径:init(session_id)→ assistant(usage/content)→ user(tool_result
// 边界)→ result(轮总 usage)。第二输入行原样回显进观测。
const fakeClaudeHappy = `#!/bin/sh
echo "$@" > "$FAKE_T/argv.txt"
n=0
while IFS= read -r line; do
  n=$((n+1))
  printf '%s\n' "$line" >> "$FAKE_T/stdin.log"
  if [ "$n" -eq 1 ]; then
    printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-fake-1"}'
    printf '%s\n' '{"type":"assistant","message":{"model":"claude-fake-4","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"working"},{"type":"tool_use","id":"t1"}]}}'
    printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}'
    printf '%s\n' '{"type":"result","is_error":false,"session_id":"sess-fake-1","usage":{"input_tokens":12,"output_tokens":6,"cache_read_input_tokens":3}}'
  else
    printf '%s\n' '{"type":"result","is_error":false,"session_id":"sess-fake-2","usage":{"input_tokens":1,"output_tokens":1}}'
  fi
done
`

func TestClaudeSessionHappyPath(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "claude"), fakeClaudeHappy)

	var mu sync.Mutex
	var hops []HopReport
	sess := claudeAdapter{}.StartSession(SessionArgs{
		Home:           t.TempDir(),
		Env:            os.Environ(),
		Model:          "claude-fake-4",
		StandingPrompt: "be the wall",
		OnLog:          func(string) {},
		OnHopUsage:     func(r HopReport) { mu.Lock(); hops = append(hops, r); mu.Unlock() },
	})
	if sess == nil {
		t.Fatal("StartSession returned nil")
	}
	defer sess.Stop()
	if !sess.CarriesStandingPrompt() {
		t.Fatal("standing prompt must be carried out-of-band")
	}

	res := sess.Send("turn one")
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("turn one: %+v", res)
	}
	if res.SessionID != "sess-fake-1" {
		t.Fatalf("session id: %q", res.SessionID)
	}
	if res.Usage == nil || res.Usage.InputTokens == nil || *res.Usage.InputTokens != 12 {
		t.Fatalf("result usage not captured: %+v", res.Usage)
	}
	if res.Model != "claude-fake-4" {
		t.Fatalf("model: %q", res.Model)
	}
	mu.Lock()
	if len(hops) != 1 {
		t.Fatalf("expected 1 hop, got %d", len(hops))
	}
	hop := hops[0]
	mu.Unlock()
	if hop.Model != "claude-fake-4" || hop.HopIndex != 1 || hop.ToolUses != 1 || hop.TextChars != len("working") {
		t.Fatalf("hop enrichment: %+v", hop)
	}

	// standing prompt 落盘 + argv 带外投递旗。
	if got := readObs(t, "argv.txt"); !strings.Contains(got, "--append-system-prompt-file") {
		t.Fatalf("argv missing standing flag: %s", got)
	}

	// 第二轮:同进程续命,不重 spawn。
	res2 := sess.Send("turn two")
	if res2.ExitCode != 0 || res2.SessionID != "sess-fake-2" {
		t.Fatalf("turn two: %+v", res2)
	}
	if got := readObs(t, "stdin.log"); strings.Count(got, "\n") != 2 {
		t.Fatalf("stdin lines: %d (persistent session must feed exactly one line per turn)", strings.Count(got, "\n"))
	}
}

// steer:排队的消息在 user(tool_result)边界注入运行中 turn。
const fakeClaudeSteer = `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$FAKE_T/stdin.log"
  # 先落日志再留 0.3s 窗——测试看到日志后 Steer 入队,用户边界事件到达
  # 时队列已非空,flush 才有货可冲。
  sleep 0.3
  printf '%s\n' '{"type":"assistant","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"x"}]}}'
  printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result"}]}}'
  IFS= read -r steer
  printf '%s\n' "$steer" >> "$FAKE_T/stdin.log"
  printf '%s\n' '{"type":"result","is_error":false,"session_id":"sess-steer","usage":{"input_tokens":2,"output_tokens":2}}'
done
`

func TestClaudeSessionSteerFlushesAtUserBoundary(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "claude"), fakeClaudeSteer)
	sess := claudeAdapter{}.StartSession(SessionArgs{Home: t.TempDir(), Env: os.Environ(), OnLog: func(string) {}})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()

	resCh := make(chan RunResult, 1)
	go func() { resCh <- sess.Send("main task") }()
	waitFileContains(t, "stdin.log", "main task", 5*time.Second)
	sess.Steer("steer me now")
	var res RunResult
	select {
	case res = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never settled after steer")
	}
	if res.ExitCode != 0 {
		t.Fatalf("steered turn failed: %+v", res)
	}
	got := readObs(t, "stdin.log")
	if !strings.Contains(got, "steer me now") {
		t.Fatalf("steer text not injected at user boundary; stdin:\n%s", got)
	}
	if !strings.Contains(got, `"type":"user"`) || !strings.Contains(got, `"role":"user"`) {
		t.Fatalf("steer must be a stream-json user message; stdin:\n%s", got)
	}
}

// 死亡传播:进程退出时在飞 turn 立即带错结算,空闲死亡留痕。
func TestClaudeSessionDeathMidTurn(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "claude"), `#!/bin/sh
sleep 0.2
exit 7
`)
	var logs []string
	sess := claudeAdapter{}.StartSession(SessionArgs{Home: t.TempDir(), Env: os.Environ(), OnLog: func(l string) { logs = append(logs, l) }})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()
	res := sess.Send("doomed")
	if res.ExitCode == 0 || res.Err == "" {
		t.Fatalf("mid-turn death must fail the turn: %+v", res)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "engine process died MID-TURN") {
		t.Fatalf("death must leave a trace; logs:\n%s", joined)
	}
	if sess.Alive() {
		t.Fatal("session must not be alive after death")
	}
}

// busy 门:同会话并发 Send 第二个立即拒绝。
func TestClaudeSessionBusyGate(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "claude"), `#!/bin/sh
IFS= read -r line
sleep 1
printf '%s\n' '{"type":"result","is_error":false,"session_id":"s","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	sess := claudeAdapter{}.StartSession(SessionArgs{Home: t.TempDir(), Env: os.Environ(), OnLog: func(string) {}})
	defer sess.Stop()
	resCh := make(chan RunResult, 1)
	go func() { resCh <- sess.Send("first") }()
	waitUntil(t, 2*time.Second, "first turn in flight", func() bool { return true })
	// 稍等确保 first 已真正发出(脚本已读到行)。
	time.Sleep(150 * time.Millisecond)
	second := sess.Send("second")
	if second.ExitCode == 0 || !strings.Contains(second.Err, "busy") {
		t.Fatalf("second concurrent send must be rejected as busy: %+v", second)
	}
	select {
	case <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("first turn never settled")
	}
}

/* ───────── Claude:一次性 run / classify / probeWake ───────── */

func TestClaudeAdapterRunOneShot(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	// POSIX:提示词走 argv(TS 同——stdin 只在 Windows shell 形态使用)。
	writeScript(t, filepath.Join(bin, "claude"), `#!/bin/sh
echo "$@" > "$FAKE_T/run-argv.txt"
printf '%s\n' '{"type":"assistant","message":{"model":"claude-run-9","usage":{"input_tokens":3,"output_tokens":4},"content":[{"type":"text","text":"hi"}]}}'
printf '%s\n' '{"type":"result","is_error":false,"session_id":"sess-run-1","usage":{"input_tokens":3,"output_tokens":4}}'
`)
	var hops []HopReport
	res := claudeAdapter{}.Run(context.Background(), RunArgs{
		Home:            t.TempDir(),
		Prompt:          "one shot prompt",
		Env:             os.Environ(),
		Model:           "claude-run-9",
		ResumeSessionID: "sess-prev",
		OnLog:           func(string) {},
		OnHopUsage:      func(r HopReport) { hops = append(hops, r) },
	})
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("run: %+v", res)
	}
	argv := readObs(t, "run-argv.txt")
	for _, want := range []string{"one shot prompt", "--resume", "sess-prev", "--model", "claude-run-9", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv missing %q: %s", want, argv)
		}
	}
	if res.SessionID != "sess-run-1" || res.Usage == nil || *res.Usage.OutputTokens != 4 {
		t.Fatalf("sniff: %+v", res)
	}
	if len(hops) != 1 || hops[0].Model != "claude-run-9" {
		t.Fatalf("one-shot hops: %+v", hops)
	}
}

func TestClaudeClassifyEnvelopeUnwrap(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "claude"), `#!/bin/sh
echo "$@" > "$FAKE_T/classify-argv.txt"
IFS= read -r prompt
printf '%s\n' "$prompt" > "$FAKE_T/classify-stdin.txt"
printf '{"result":"VERDICT-OK","usage":{"input_tokens":7,"output_tokens":2}}'
`)
	res := claudeAdapter{}.Classify(context.Background(), ClassifyArgs{
		Cwd:    t.TempDir(),
		Prompt: "triage this",
		Env:    os.Environ(),
		OnLog:  func(string) {},
	})
	if res.Err != "" {
		t.Fatalf("classify: %+v", res)
	}
	if res.Text != "VERDICT-OK" {
		t.Fatalf("envelope unwrap: %q", res.Text)
	}
	if res.Usage == nil || *res.Usage.InputTokens != 7 {
		t.Fatalf("usage: %+v", res.Usage)
	}
	argv := readObs(t, "classify-argv.txt")
	for _, want := range []string{"--model", "haiku", "--output-format", "json", "--strict-mcp-config"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("classify argv missing %q: %s", want, argv)
		}
	}
}

func TestClaudeProbeWakeFlagAcceptance(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "claude"), `#!/bin/sh
echo "$@" > "$FAKE_T/pw-argv.txt"
printf 'OK'
`)
	cwd := t.TempDir()
	r := claudeAdapter{}.ProbeWake(context.Background(), WakeProbeArgs{Cwd: cwd, Env: os.Environ()})
	if !r.OK || r.Skipped {
		t.Fatalf("probeWake should pass on flag-accepting CLI: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cumora-doctor-standing.md")); err != nil {
		t.Fatalf("probe must write the standing file: %v", err)
	}
	// 覆盖参数 → skipped(唤醒折叠为一次性路径)。
	t.Setenv("CUMORA_CLAUDE_ARGS", "--my-flags")
	r2 := claudeAdapter{}.ProbeWake(context.Background(), WakeProbeArgs{Cwd: cwd, Env: os.Environ()})
	if !r2.Skipped {
		t.Fatalf("CUMORA_CLAUDE_ARGS must mark probeWake skipped: %+v", r2)
	}
	// 旗被拒(CLI 报错退出)→ ok=false 且 detail 含显著错误。
	writeScript(t, filepath.Join(bin, "claude"), `#!/bin/sh
printf 'error: unknown option --append-system-prompt-file\n' >&2
exit 64
`)
	t.Setenv("CUMORA_CLAUDE_ARGS", "")
	r3 := claudeAdapter{}.ProbeWake(context.Background(), WakeProbeArgs{Cwd: cwd, Env: os.Environ()})
	if r3.OK || r3.Detail == "" {
		t.Fatalf("rejected flag must fail probeWake with detail: %+v", r3)
	}
}

func TestClaudeStartSessionRespectsArgsOverride(t *testing.T) {
	t.Setenv("CUMORA_CLAUDE_ARGS", "--custom")
	if (claudeAdapter{}).StartSession(SessionArgs{Home: t.TempDir(), Env: os.Environ()}) != nil {
		t.Fatal("custom args override must disable the persistent path")
	}
}

/* ───────── Codex:app-server JSON-RPC 会话 ───────── */

const fakeCodexAppServer = `#!/bin/sh
echo "$@" > "$FAKE_T/codex-argv.txt"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([a-zA-Z/]*\)".*/\1/p')
  case "$method" in
    initialize)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"info":"fake"}}\n' "$id"
      ;;
    thread/start|thread/resume)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"th-fake-1"}}}\n' "$id"
      ;;
    turn/start)
      printf '%s\n' "$line" > "$FAKE_T/turnstart.json"
      printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":100,"cachedInputTokens":40,"outputTokens":20,"reasoningOutputTokens":5}}}}\n'
      printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"turn-9"}}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"item/started","params":{"item":{"type":"agentMessage"}}}\n'
      printf '{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"tokenUsage":{"total":{"inputTokens":150,"cachedInputTokens":50,"outputTokens":30,"reasoningOutputTokens":5}}}}\n'
      # 轮在此逗留 1s——测试的 Steer 须落在轮存活窗内(expectedTurnId 已
      # 知、turn/completed 未到),否则 steer 语义无从验证。
      sleep 1
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"turn":{"status":"completed"}}}\n'
      ;;
    turn/steer)
      printf '%s\n' "$line" > "$FAKE_T/steer.json"
      ;;
  esac
done
`

func TestCodexSessionHandshakeAndTurn(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "codex"), fakeCodexAppServer)

	var mu sync.Mutex
	var hops []HopReport
	var logs []string
	sess := codexAdapter{}.StartSession(SessionArgs{
		Home:           fakeCodexHome(t),
		Env:            os.Environ(),
		Model:          "gpt-fake-5",
		StandingPrompt: "developer says hi",
		OnLog:          func(l string) { mu.Lock(); logs = append(logs, l); mu.Unlock() },
		OnHopUsage:     func(r HopReport) { mu.Lock(); hops = append(hops, r); mu.Unlock() },
	})
	if sess == nil {
		t.Fatal("StartSession returned nil")
	}
	defer sess.Stop()
	if !sess.CarriesStandingPrompt() {
		t.Fatal("developerInstructions standing prompt must be carried")
	}
	waitUntil(t, 5*time.Second, "thread ready", func() bool { return sess.SessionID() == "th-fake-1" })

	res := sess.Send("codex turn one")
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("turn: %+v", res)
	}
	if res.SessionID != "th-fake-1" {
		t.Fatalf("thread id: %q", res.SessionID)
	}
	// 差值用量:第二轮总量(150/50/35)− 起始(0)→ input=150−cached50=100,
	// cache_read=50,output=35。
	if res.Usage == nil {
		t.Fatal("usage missing")
	}
	if *res.Usage.InputTokens != 100 || *res.Usage.CacheReadInputTokens != 50 || *res.Usage.OutputTokens != 35 {
		t.Fatalf("turn delta usage: %+v", res.Usage)
	}
	mu.Lock()
	if len(hops) != 1 || hops[0].Model != "gpt-fake-5" || hops[0].HopIndex != 1 {
		t.Fatalf("codex hops: %+v", hops)
	}
	mu.Unlock()
	if got := readObs(t, "turnstart.json"); !strings.Contains(got, `"threadId":"th-fake-1"`) || !strings.Contains(got, "codex turn one") {
		t.Fatalf("turn/start shape: %s", got)
	}
	// standing prompt 走线程参数:thread/start 虽由握手期发出(本假端未存),
	// 但 argv 必须是 app-server --listen stdio://。
	if got := readObs(t, "codex-argv.txt"); strings.TrimSpace(got) != "app-server --listen stdio://" {
		t.Fatalf("codex argv: %q", got)
	}
}

// resume 失败 → 换全新线程(thread/start),agent 不楔死。
const fakeCodexResumeFail = `#!/bin/sh
saw_resume=0
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([a-zA-Z/]*\)".*/\1/p')
  case "$method" in
    initialize)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    thread/resume)
      printf '{"jsonrpc":"2.0","id":%s,"error":{"message":"thread not found"}}\n' "$id"
      ;;
    thread/start)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"th-fresh"}}}\n' "$id"
      ;;
    turn/start)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"t1"}}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"turn":{"status":"completed"}}}\n'
      ;;
  esac
done
`

func TestCodexSessionResumeFallsBackToFreshThread(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "codex"), fakeCodexResumeFail)
	var logs []string
	sess := codexAdapter{}.StartSession(SessionArgs{
		Home:            fakeCodexHome(t),
		Env:             os.Environ(),
		ResumeSessionID: "th-stale",
		OnLog:           func(l string) { logs = append(logs, l) },
	})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()
	waitUntil(t, 5*time.Second, "fresh thread after resume failure", func() bool { return sess.SessionID() == "th-fresh" })
	if !strings.Contains(strings.Join(logs, "\n"), "starting a fresh thread") {
		t.Fatalf("resume fallback must log; logs:\n%s", strings.Join(logs, "\n"))
	}
	res := sess.Send("after fallback")
	if res.ExitCode != 0 || res.SessionID != "th-fresh" {
		t.Fatalf("turn after fallback: %+v", res)
	}
}

// 握手失败 → 会话自杀(!alive),后续 Send 报真因(daemon 弃尸重起)。
func TestCodexSessionHandshakeFailureKillsSession(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "codex"), `#!/bin/sh
IFS= read -r line
id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
printf '{"jsonrpc":"2.0","id":%s,"error":{"message":"config.toml is malformed"}}\n' "$id"
sleep 30
`)
	sess := codexAdapter{}.StartSession(SessionArgs{Home: fakeCodexHome(t), Env: os.Environ(), OnLog: func(string) {}})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()
	waitUntil(t, 5*time.Second, "session dies after handshake rejection", func() bool { return !sess.Alive() })
	res := sess.Send("late")
	if res.ExitCode == 0 || !strings.Contains(res.Err, "config.toml is malformed") {
		t.Fatalf("late send must surface the handshake error: %+v", res)
	}
}

// steer:turn/steer 带 expectedTurnId,仅在已知 turnId 后生效。
func TestCodexSessionSteerUsesExpectedTurnID(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "codex"), fakeCodexAppServer)
	sess := codexAdapter{}.StartSession(SessionArgs{Home: fakeCodexHome(t), Env: os.Environ(), Model: "m"})
	if sess == nil {
		t.Fatal("nil session")
	}
	defer sess.Stop()
	waitUntil(t, 5*time.Second, "thread ready", func() bool { return sess.SessionID() == "th-fake-1" })
	// 无 turn 在飞 → no-op(不发 turn/steer)。
	sess.Steer("too early")
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(os.Getenv("FAKE_T"), "steer.json")); err == nil {
		t.Fatal("steer without an active turn must be a no-op")
	}
	resCh := make(chan RunResult, 1)
	go func() { resCh <- sess.Send("slow turn") }()
	waitFileContains(t, "turnstart.json", "slow turn", 5*time.Second)
	// turn/start 应答已带 turn-9(假端同步回)——此刻 steer 应真发。
	sess.Steer("mid codex steer")
	waitFileContains(t, "steer.json", "mid codex steer", 5*time.Second)
	got := readObs(t, "steer.json")
	if !strings.Contains(got, `"expectedTurnId":"turn-9"`) || !strings.Contains(got, `"threadId":"th-fake-1"`) {
		t.Fatalf("turn/steer shape: %s", got)
	}
	select {
	case r := <-resCh:
		if r.ExitCode != 0 {
			t.Fatalf("steered turn failed: %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn never settled")
	}
}

/* ───────── Codex:一次性 / 探针 / seedHome ───────── */

func TestCodexAdapterRunOneShot(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "codex"), `#!/bin/sh
echo "$@" > "$FAKE_T/codex-run-argv.txt"
IFS= read -r prompt
printf '%s\n' "$prompt" > "$FAKE_T/codex-run-stdin.txt"
printf '%s\n' '{"session_id":"th-exec-1","type":"result","usage":{"input_tokens":9,"output_tokens":8},"model":"gpt-exec-1"}'
`)
	res := codexAdapter{}.Run(context.Background(), RunArgs{
		Home: t.TempDir(), Prompt: "codex one shot", Env: os.Environ(), Model: "gpt-exec-1",
	})
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("run: %+v", res)
	}
	argv := readObs(t, "codex-run-argv.txt")
	for _, want := range []string{"exec", "--model", "gpt-exec-1", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv missing %q: %s", want, argv)
		}
	}
	if got := strings.TrimSpace(readObs(t, "codex-run-stdin.txt")); got != "codex one shot" {
		t.Fatalf("prompt via stdin: %q", got)
	}
	if res.SessionID != "th-exec-1" || res.Model != "gpt-exec-1" {
		t.Fatalf("sniff: %+v", res)
	}
}

func TestCodexProbeWakeHandshake(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	writeScript(t, filepath.Join(bin, "codex"), fakeCodexAppServer)
	cwd := t.TempDir()
	// alpine 工具镜像可能无 git(握手正路径需要垫仓库)——无 git 只测
	// 折叠门,不 fail。
	if _, err := exec.LookPath("git"); err == nil {
		r := codexAdapter{}.ProbeWake(context.Background(), WakeProbeArgs{Cwd: cwd, Env: os.Environ()})
		if !r.OK || r.Detail != "" {
			t.Fatalf("probeWake: %+v", r)
		}
		if !pathExists(filepath.Join(cwd, ".git")) {
			t.Fatal("probeWake must bootstrap a git repo for app-server cwd")
		}
	}
	// 折叠门:自定义参数/显式退出 → skipped。
	t.Setenv("CUMORA_CODEX_ARGS", "--x")
	if r2 := (codexAdapter{}).ProbeWake(context.Background(), WakeProbeArgs{Cwd: cwd, Env: os.Environ()}); !r2.Skipped {
		t.Fatalf("CUMORA_CODEX_ARGS must skip: %+v", r2)
	}
	t.Setenv("CUMORA_CODEX_ARGS", "")
	t.Setenv("CUMORA_CODEX_NO_APP_SERVER", "1")
	if r3 := (codexAdapter{}).ProbeWake(context.Background(), WakeProbeArgs{Cwd: cwd, Env: os.Environ()}); !r3.Skipped {
		t.Fatalf("opt-out must skip: %+v", r3)
	}
}

func TestCodexStartSessionFallbacks(t *testing.T) {
	t.Setenv("CUMORA_CODEX_ARGS", "--x")
	if (codexAdapter{}).StartSession(SessionArgs{Home: t.TempDir(), Env: os.Environ()}) != nil {
		t.Fatal("args override must fall back to one-shot")
	}
	t.Setenv("CUMORA_CODEX_ARGS", "")
	t.Setenv("CUMORA_CODEX_NO_APP_SERVER", "1")
	if (codexAdapter{}).StartSession(SessionArgs{Home: t.TempDir(), Env: os.Environ()}) != nil {
		t.Fatal("explicit opt-out must fall back to one-shot")
	}
}

/* ───────── SeedHome ───────── */

func TestSeedHomeIdempotentAndNonDestructive(t *testing.T) {
	home := t.TempDir()
	p := Persona{ID: "a1", Name: "Atlas", Role: strp("Tester"), SystemPrompt: strp("Be terse.")}
	if err := (claudeAdapter{}).SeedHome(home, p); err != nil {
		t.Fatal(err)
	}
	claudeMD := filepath.Join(home, "CLAUDE.md")
	b, _ := os.ReadFile(claudeMD)
	want := mustGolden(t, "persona_full.txt")
	if string(b) != want {
		t.Fatalf("CLAUDE.md content drift (%d vs %d bytes)", len(b), len(want))
	}
	for _, dir := range []string{"memory", "notes", "workspace", filepath.Join(".claude", "skills")} {
		if !pathExists(filepath.Join(home, dir)) {
			t.Fatalf("missing %s", dir)
		}
	}
	// 记忆索引不覆盖。
	agentWritten := "- [Mine](mine.md) — agent's own line\n"
	_ = os.WriteFile(filepath.Join(home, "memory", "MEMORY.md"), []byte(agentWritten), 0o644)
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"custom":true}`), 0o644)
	if err := (claudeAdapter{}).SeedHome(home, p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(home, "memory", "MEMORY.md"))
	if string(got) != agentWritten {
		t.Fatal("seedHome must never clobber agent-written memory")
	}
	st, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if string(st) != `{"custom":true}` {
		t.Fatal("seedHome must not overwrite user settings.json")
	}
	// codex:AGENTS.md(内容保持 TS 默认 personaHeader)。
	if err := (codexAdapter{}).SeedHome(home, p); err != nil {
		t.Fatal(err)
	}
	agentsMD, _ := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	if string(agentsMD) != want {
		t.Fatalf("AGENTS.md content drift (%d vs %d bytes)", len(agentsMD), len(want))
	}
	if _, err := os.Stat(claudeMD); err != nil {
		t.Fatal("codex seedHome must not remove the claude layout")
	}
}
