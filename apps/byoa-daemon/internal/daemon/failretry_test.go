// failretry_test —— #262 验收:runTurn 失败分类重试环(评审 #276 P2-3)。
// network 失败重试一次成功 / credential 不重试 / context-overflow 弃会话
// 清 resume id 后换全新开。
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func retryTestStack(t *testing.T, inboxRows []map[string]any) (*stubServer, *DaemonConfig) {
	t.Helper()
	t.Setenv("CUMORA_INBOX_POLL_MS", "600000")
	t.Setenv("CUMORA_TURN_RETRY_BACKOFF_MS", "1")
	return newSessionTestStack(t, inboxRows)
}

func finishSummaries(stub *stubServer) []string {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	out := make([]string, 0, len(stub.runFinishes))
	for _, b := range stub.runFinishes {
		if s, ok := b["summary"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// network 类失败 → 本地重试一次成功:总 sends=2,成功 summary 带
// [turn-recovered attempts=1]。
func TestRunTurnRetriesRetryableFailure(t *testing.T) {
	isolateHome(t)
	stub, cfg := retryTestStack(t, []map[string]any{
		{"id": "m1", "conversation_id": "cv-1", "kind": "text", "body": "hello", "author_name": "Ann"},
	})
	var calls atomic.Int32
	sess := &fakeEngineSession{sid: "sess-retry", alive: true, carries: true,
		sendResult: func(string) RunResult {
			if calls.Add(1) == 1 {
				return RunResult{ExitCode: 1, Err: "fetch failed: dial tcp: connection refused", SessionID: "sess-retry"}
			}
			return RunResult{ExitCode: 0, SessionID: "sess-retry"}
		},
	}
	adapter := &sessionAdapter{id: "claude", session: sess}
	r := newAgentRunner(cfg, AgentInfo{ID: "a1", Name: "Atlas", Engine: strp("claude")}, adapter)
	r.Start()
	t.Cleanup(func() { r.Stop(); r.wg.Wait() })

	if err := r.runTurn("sse-wake"); err != nil {
		t.Fatalf("重试后成功仍报错: %v", err)
	}
	sess.mu.Lock()
	sends := len(sess.sends)
	sess.mu.Unlock()
	if sends != 2 {
		t.Fatalf("应 1 失败 + 1 重试(sends=2),got %d", sends)
	}
	waitUntil(t, 5*time.Second, "recovered summary", func() bool {
		for _, s := range finishSummaries(stub) {
			if strings.Contains(s, "turn-recovered attempts=1") {
				return true
			}
		}
		return false
	})
}

// credential 类失败 → 不重试(sends=1),runTurn 报错,summary 带 class=credential。
func TestRunTurnCredentialFailureNotRetried(t *testing.T) {
	isolateHome(t)
	stub, cfg := retryTestStack(t, []map[string]any{
		{"id": "m1", "conversation_id": "cv-1", "kind": "text", "body": "hello", "author_name": "Ann"},
	})
	var calls atomic.Int32
	sess := &fakeEngineSession{sid: "sess-auth", alive: true, carries: true,
		sendResult: func(string) RunResult {
			calls.Add(1)
			return RunResult{ExitCode: 1, Err: "API error 401 unauthorized: invalid api key", SessionID: "sess-auth"}
		},
	}
	adapter := &sessionAdapter{id: "claude", session: sess}
	r := newAgentRunner(cfg, AgentInfo{ID: "a2", Name: "Iris", Engine: strp("claude")}, adapter)
	r.Start()
	t.Cleanup(func() { r.Stop(); r.wg.Wait() })

	err := r.runTurn("poll")
	if err == nil {
		t.Fatal("credential 失败必须上抛错误")
	}
	if !strings.Contains(err.Error(), "local claude failed") {
		t.Fatalf("错误形态: %v", err)
	}
	sess.mu.Lock()
	sends := len(sess.sends)
	sess.mu.Unlock()
	if sends != 1 {
		t.Fatalf("credential 不重试(sends=1),got %d", sends)
	}
	waitUntil(t, 5*time.Second, "failed summary", func() bool {
		for _, s := range finishSummaries(stub) {
			if strings.Contains(s, "class=credential attempts=1") {
				return true
			}
		}
		return false
	})
}

// context-overflow:resume-unsafe → 会话被弃(Stop)、盘上 resume id 清空,
// 重试从全新会话开始(第二次 StartSession 无 resume)。
func TestRunTurnContextOverflowDropsSession(t *testing.T) {
	isolateHome(t)
	_, cfg := retryTestStack(t, []map[string]any{
		{"id": "m1", "conversation_id": "cv-1", "kind": "text", "body": "hello", "author_name": "Ann"},
	})
	var calls atomic.Int32
	sess := &fakeEngineSession{sid: "sess-overflow", alive: true, carries: true,
		sendResult: func(string) RunResult {
			if calls.Add(1) == 1 {
				return RunResult{ExitCode: 1, Err: "prompt is too long: context window exceeded", SessionID: "sess-overflow"}
			}
			return RunResult{ExitCode: 0, SessionID: "sess-fresh"}
		},
	}
	adapter := &sessionAdapter{id: "claude", session: sess}
	r := newAgentRunner(cfg, AgentInfo{ID: "a3", Name: "Nova", Engine: strp("claude")}, adapter)
	r.setSessionID("sess-old")
	r.Start()
	t.Cleanup(func() { r.Stop(); r.wg.Wait() })

	if err := r.runTurn("poll"); err != nil {
		t.Fatalf("换新会话后应成功: %v", err)
	}
	sess.mu.Lock()
	alive, sends := sess.alive, len(sess.sends)
	sess.mu.Unlock()
	if alive {
		t.Fatal("resume-unsafe 失败后旧会话必须被 Stop(alive=false)")
	}
	if sends != 2 {
		t.Fatalf("overflow 可重试但须换会话(sends=2),got %d", sends)
	}
	if len(adapter.startArgs) != 2 {
		t.Fatalf("应孵化两次(弃会话+换新),got %d", len(adapter.startArgs))
	}
	if adapter.startArgs[1].ResumeSessionID != "" {
		t.Fatalf("重试孵化必须无 resume id(已弃),got %q", adapter.startArgs[1].ResumeSessionID)
	}
	b, _ := os.ReadFile(filepath.Join(sessionsDir(), "a3.session"))
	if strings.TrimSpace(string(b)) == "sess-old" {
		t.Fatal("盘上 resume id 未被清空")
	}
}
