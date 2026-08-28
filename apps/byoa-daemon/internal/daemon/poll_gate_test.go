// #134:pollLoop 的 SSE 健康门控——健康期(最近有帧/ping)静默,
// 断流/未连上才轮询兜底。streamHealthy 边界 + pollLoop 行为双测。
package daemon

import (
	"testing"
	"time"
)

func TestStreamHealthySemantics(t *testing.T) {
	var r AgentRunner
	if r.streamHealthy() {
		t.Fatal("从未连上(streamAlive=0)应视为不健康——冷启动期轮询兜底必须开")
	}
	r.markStreamAlive()
	if !r.streamHealthy() {
		t.Fatal("刚有帧应健康")
	}
	r.streamAlive.Store(time.Now().Add(-sseHealthGrace - time.Second).UnixNano())
	if r.streamHealthy() {
		t.Fatalf("超过宽限窗(%s)无帧应不健康", sseHealthGrace)
	}
	r.markStreamAlive()
	if !r.streamHealthy() {
		t.Fatal("恢复供帧应即刻恢复健康")
	}
}

func TestPollLoopGatedOnStreamHealth(t *testing.T) {
	isolateHome(t)
	t.Setenv("CUMORA_INBOX_POLL_MS", "40")
	adapter := &sessionAdapter{id: "claude"}
	r := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, AgentInfo{ID: "a-gate", Name: "G"}, adapter)
	r.wg.Add(1)
	go r.pollLoop()
	t.Cleanup(func() { r.BeginStop(); r.cancel(); r.wg.Wait() })

	debounceArmed := func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.wakeDebounce != nil
	}

	// 健康期:40ms 一拍,300ms 内多拍全部静默,不武装唤醒
	//(旧行为每拍必打 inbox——N 个 idle agent = N×3 次/分钟)。
	r.markStreamAlive()
	time.Sleep(300 * time.Millisecond)
	if debounceArmed() {
		t.Fatal("SSE 健康期 pollLoop 不得武装唤醒(门控失效)")
	}

	// 断流:回拨到宽限窗外 → 下一拍走轮询兜底(scheduleWake 武装防抖)。
	r.streamAlive.Store(time.Now().Add(-2 * sseHealthGrace).UnixNano())
	armed := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if debounceArmed() {
			armed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !armed {
		t.Fatal("SSE 不健康后 pollLoop 应恢复轮询兜底(武装唤醒防抖)")
	}
}

// 评审 P2 安全网:流判健康但服务端事件链已聋(Redis 断连、ping 照流)
// ——健康期由低频安全网拍补拾取,防完全静默。
func TestPollLoopSafetyNetWhileHealthy(t *testing.T) {
	isolateHome(t)
	t.Setenv("CUMORA_INBOX_POLL_MS", "600000") // 主拍静默,只看安全网
	t.Setenv("CUMORA_HEALTHY_POLL_MS", "60")
	adapter := &sessionAdapter{id: "claude"}
	r := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, AgentInfo{ID: "a-safe", Name: "S"}, adapter)
	r.wg.Add(1)
	go r.pollLoop()
	t.Cleanup(func() { r.BeginStop(); r.cancel(); r.wg.Wait() })

	r.markStreamAlive()
	armed := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		r.mu.Lock()
		a := r.wakeDebounce != nil
		r.mu.Unlock()
		if a {
			armed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !armed {
		t.Fatal("健康期安全网轮询未生效(聋但 ping 着的场景将无限静默)")
	}
}
