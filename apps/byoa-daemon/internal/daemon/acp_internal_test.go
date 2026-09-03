// acp 基类测试(#268):假 ACP fork 引擎跑通完整会话(initialize →
// session/new → session/prompt → 流事件 → usage 应答),证明"接一个
// 同协议新引擎 = 一个描述符条目"。协议序是顺序请求-应答,假引擎按行序
// 回放即够,无需真解析。
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAcpFork:顺序应答的假 ACP 引擎。reqID 分配确定性:initialize=1,
// session/new=2,session/prompt=3。
const fakeAcpFork = `#!/bin/sh
read init
printf '{"jsonrpc":"2.0","id":1,"result":{"sessionId":""}}\n'
read newsess
printf '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s-fork-1"}}\n'
read prompt
printf '{"jsonrpc":"2.0","method":"fork/models","params":{"currentModelId":"fork-mini"}}\n'
printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","title":"fork-grep"}}}\n'
printf '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"text":"hi from fork"}}}}\n'
printf '{"jsonrpc":"2.0","id":3,"result":{"usage":{"input_tokens":11,"output_tokens":7}}}\n'
cat >/dev/null
`

func TestAcpSessionForkDescriptorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fork-agent")
	writeScript(t, bin, fakeAcpFork)

	var mu sync.Mutex
	var logs []string
	var hops []HopReport
	var transcript []TranscriptEntry

	s := newAcpSession(acpSessionConfig{
		EngineID:         "fork",
		Bin:              bin,
		SpawnArgs:        []string{},
		SessionNewParams: map[string]any{"cwd": dir, "mcpServers": []any{}},
		OnNotify: func(s *acpSession, method string, params map[string]any) {
			if method == "fork/models" {
				if id, ok := params["currentModelId"].(string); ok {
					s.curModel = id
				}
			}
		},
	}, SessionArgs{
		Home:         dir,
		Env:          []string{"PATH=" + os.Getenv("PATH")},
		OnLog:        func(line string) { mu.Lock(); logs = append(logs, line); mu.Unlock() },
		OnHopUsage:   func(h HopReport) { mu.Lock(); hops = append(hops, h); mu.Unlock() },
		OnTranscript: func(e TranscriptEntry) { mu.Lock(); transcript = append(transcript, e); mu.Unlock() },
	})
	defer s.Stop()

	if !s.Alive() {
		t.Fatal("session must come up alive")
	}
	if s.CarriesStandingPrompt() {
		t.Fatal("no standing prompt given — CarriesStandingPrompt must be false")
	}

	res := s.Send("hello fork")
	if res.ExitCode != 0 {
		t.Fatalf("turn failed: %+v", res)
	}
	if res.SessionID != "s-fork-1" {
		t.Fatalf("session id = %q, want s-fork-1", res.SessionID)
	}
	// usage 对账:snake 命名透传
	if res.Usage == nil || res.Usage.InputTokens == nil || *res.Usage.InputTokens != 11 ||
		res.Usage.OutputTokens == nil || *res.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	// 引擎通知钩子播报的模型成为台账与结果模型
	if res.Model != "fork-mini" {
		t.Fatalf("model = %q, want fork-mini (OnNotify curModel)", res.Model)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hops) != 1 || hops[0].Model != "fork-mini" || hops[0].Usage.InputTokens == nil {
		t.Fatalf("hops = %+v", hops)
	}
	// #260 转录:tool_use + text 两类流事件
	if len(transcript) != 2 || transcript[0].Type != "tool_use" || transcript[0].Tool != "fork-grep" ||
		transcript[1].Type != "text" || transcript[1].Content != "hi from fork" {
		t.Fatalf("transcript = %+v", transcript)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "[fork] tool fork-grep") {
		t.Fatalf("log prefix must come from the descriptor EngineID: %q", joined)
	}
}

func TestAcpSessionBusyGateAndSteerWarn(t *testing.T) {
	// busy 门与 steer 告警不需要真引擎 —— 用慢速假引擎占住一个 turn。
	dir := t.TempDir()
	bin := filepath.Join(dir, "fork-slow")
	writeScript(t, bin, `#!/bin/sh
read init
printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'
read newsess
printf '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"s-slow"}}\n'
read prompt
sleep 2
`)
	var mu sync.Mutex
	var logs []string
	s := newAcpSession(acpSessionConfig{
		EngineID:         "fork",
		Bin:              bin,
		SessionNewParams: map[string]any{"cwd": dir},
		SteerWarnLine:    "[fork] no mid-turn steer — rides the next wake",
	}, SessionArgs{
		Home:  dir,
		Env:   []string{"PATH=" + os.Getenv("PATH")},
		OnLog: func(line string) { mu.Lock(); logs = append(logs, line); mu.Unlock() },
	})
	defer s.Stop()

	done := make(chan RunResult, 1)
	go func() { done <- s.Send("slow turn") }()
	// 等到 turn 真正占住(轮询 pending;假引擎 sleep 保证窗口)。
	deadline := retryUntil(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.pending != nil
	}, "turn in flight")
	_ = deadline

	busy := s.Send("second turn")
	if busy.ExitCode == 0 || !strings.Contains(busy.Err, "busy") {
		t.Fatalf("second Send must hit the busy gate: %+v", busy)
	}

	s.Steer("ping")
	s.Steer("ping")
	mu.Lock()
	defer mu.Unlock()
	warns := 0
	for _, l := range logs {
		if strings.Contains(l, "no mid-turn steer") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("steer warn must fire exactly once, got %d in %v", warns, logs)
	}

	// 收尾:Stop 让阻塞的 Send 带出死亡结果,防 goroutine 泄漏挂测试。
	s.Stop()
	<-done
}

// retryUntil:50ms 轮询至谓词真(2s 上限)。
func retryUntil(t *testing.T, pred func() bool, what string) bool {
	t.Helper()
	for i := 0; i < 40; i++ {
		if pred() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return false
}
