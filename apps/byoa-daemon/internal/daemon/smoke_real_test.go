// smoke_real_test —— #64 验收:真实 CLI 的引擎冒烟(环境变量门控,
// 默认跳过——CI 无真实引擎/凭证):
//
//	CUMORA_SMOKE_CLAUDE=1 ./smoke.test -test.run TestRealClaudeSmoke -test.v
//	CUMORA_SMOKE_CODEX=1 ./smoke.test -test.run TestRealCodexSmoke  -test.v
//
// 覆盖:持久会话两轮(第二轮证明 --resume/thread 上下文连续)、standing
// prompt 带外投递可观察(哨兵 token 只存在于系统提示,轮提示词没有)、
// probe/probeWake 真实握手。宿主机跑(docker 工具容器里没有引擎):
//
//	cd apps/byoa-daemon && ./godocker.sh test -c ./internal/daemon -o /tmp/smoke.test
//	CUMORA_SMOKE_CLAUDE=1 /tmp/smoke.test -test.run TestRealClaudeSmoke -test.v
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// smokeStandingPrompt:带哨兵 token 的最小 standing prompt——token 不进任何
// 轮提示词,agent 能复述即证明带外通道(--append-system-prompt-file /
// developerInstructions)真实生效。
const smokeStandingPrompt = "You are a Cumora SMOKE agent. Your standing secret token is: PINEAPPLE-MANGO-77. " +
	"When asked for your standing secret token, reply with the token exactly and nothing else."

func realCLISmoke(t *testing.T, engine string) (EngineAdapter, string) {
	t.Helper()
	home := t.TempDir()
	var adapter EngineAdapter
	switch engine {
	case "claude":
		adapter = claudeAdapter{}
	case "codex":
		adapter = codexAdapter{}
		if err := ensureGitRepoForCodex(home); err != nil {
			t.Fatalf("git bootstrap (real machine must have git): %v", err)
		}
	}
	return adapter, home
}

// smokeLog:捕获会话日志行(claude 的 stream-json 事件行含 assistant 文本;
// codex 的 `[codex] » <text>` 信号行含最终答复)——文本断言的唯一可观
// 测面:RunResult 不带引擎文本(TS 同——agent 经 cumora CLI 行动)。
type smokeLog struct {
	mu    sync.Mutex
	lines []string
}

func newSmokeLog() *smokeLog { return &smokeLog{} }

func (l *smokeLog) log(line string) {
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
}

func (l *smokeLog) has(substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func smokeTwoTurnsAndStanding(t *testing.T, sess EngineSession, logs *smokeLog, marker string) {
	t.Helper()

	// 轮 1:固定口令——拿 session id、验证引擎真跑。
	res1 := sess.Send(fmt.Sprintf("Reply with exactly this token and nothing else: %s", marker))
	if res1.ExitCode != 0 || res1.Err != "" {
		t.Fatalf("smoke turn 1 failed: %+v", res1)
	}
	if sess.SessionID() == "" {
		t.Fatal("no session id after turn 1 (resume chain broken)")
	}

	// 轮 2(同进程连续上下文):复述上一轮口令——上下文连续性。
	res2 := sess.Send("What exact token did I ask you to reply with in my previous message? Reply with just the token.")
	if res2.ExitCode != 0 || res2.Err != "" {
		t.Fatalf("smoke turn 2 (continuity) failed: %+v", res2)
	}
	if !logs.has(marker) {
		t.Fatalf("continuity broken: agent never echoed %s in turn 2 output; log tail: %s", marker, logs.tail())
	}

	// 轮 3:standing 哨兵——只可能来自带外系统提示。
	res3 := sess.Send("What is your standing secret token? Reply with just the token.")
	if res3.ExitCode != 0 || res3.Err != "" {
		t.Fatalf("smoke turn 3 (standing reveal) failed: %+v", res3)
	}
	if !logs.has("PINEAPPLE-MANGO-77") {
		t.Fatalf("standing prompt not delivered out-of-band: token never surfaced; log tail: %s", logs.tail())
	}
}

func (l *smokeLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := len(l.lines) - 10
	if start < 0 {
		start = 0
	}
	return strings.Join(l.lines[start:], "\n")
}

func TestRealClaudeSmoke(t *testing.T) {
	if os.Getenv("CUMORA_SMOKE_CLAUDE") != "1" {
		t.Skip("real-CLI smoke: set CUMORA_SMOKE_CLAUDE=1 (host machine with a working claude CLI)")
	}
	adapter, home := realCLISmoke(t, "claude")

	// probeWake:持久路径旗接受度(--append-system-prompt-file)。
	pw := adapter.ProbeWake(context.Background(), WakeProbeArgs{Cwd: t.TempDir(), Env: os.Environ()})
	if !pw.OK {
		t.Fatalf("claude probeWake: %+v", pw)
	}

	logs := newSmokeLog()
	sess := adapter.StartSession(SessionArgs{
		Home:           home,
		Env:            os.Environ(),
		StandingPrompt: smokeStandingPrompt,
		OnLog: func(l string) {
			t.Log("[claude] " + l)
			logs.log(l)
		},
		OnHopUsage: func(r HopReport) { t.Logf("[claude] hop model=%s usage=%+v", r.Model, r.Usage) },
	})
	if sess == nil {
		t.Fatal("claude StartSession returned nil (CUMORA_CLAUDE_ARGS set?)")
	}
	defer sess.Stop()
	if !sess.CarriesStandingPrompt() {
		t.Fatal("standing prompt file must be carried")
	}
	if _, err := os.Stat(filepath.Join(home, ".cumora-standing-prompt.md")); err != nil {
		t.Fatalf("standing file not written: %v", err)
	}
	smokeTwoTurnsAndStanding(t, sess, logs, "CLAUDE-SMOKE-OK")
}

func TestRealCodexSmoke(t *testing.T) {
	if os.Getenv("CUMORA_SMOKE_CODEX") != "1" {
		t.Skip("real-CLI smoke: set CUMORA_SMOKE_CODEX=1 (host machine with a working codex CLI)")
	}
	adapter, home := realCLISmoke(t, "codex")

	pw := adapter.ProbeWake(context.Background(), WakeProbeArgs{Cwd: home, Env: os.Environ()})
	if !pw.OK {
		t.Fatalf("codex probeWake (app-server handshake): %+v", pw)
	}

	logs := newSmokeLog()
	sess := adapter.StartSession(SessionArgs{
		Home:           home,
		Env:            os.Environ(),
		StandingPrompt: smokeStandingPrompt,
		OnLog: func(l string) {
			t.Log("[codex] " + l)
			logs.log(l)
		},
		OnHopUsage: func(r HopReport) { t.Logf("[codex] hop model=%s usage=%+v latency=%v", r.Model, r.Usage, r.LatencyMS) },
	})
	if sess == nil {
		t.Fatal("codex StartSession returned nil (CUMORA_CODEX_ARGS / NO_APP_SERVER / windows?)")
	}
	defer sess.Stop()
	if !sess.CarriesStandingPrompt() {
		t.Fatal("developerInstructions standing prompt must be carried")
	}
	smokeTwoTurnsAndStanding(t, sess, logs, "CODEX-SMOKE-OK")
}
