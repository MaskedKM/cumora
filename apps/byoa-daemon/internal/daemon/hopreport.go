// daemon 包 hopreport —— 逐跳轨迹的缓冲上报(对齐 daemon.ts HopReporter):
// 引擎会话每条助手消息/turn-completed 一跳,这里攒批 POST 到
// /runtime/llm-calls,让统一台账看到与云侧同粒度的 BYOA 轨迹,不为每跳
// 付一次往返。纪律:fire-and-forget(网络抖动绝不影响产出它的引擎轮)、
// 有界队列(硬故障下不爆内存,丢最旧并告警一次)、窗口/满批双触发、
// turn 结束可 await 的 flush。
package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type pendingHop struct {
	Source         string         `json:"source"` // byoa-claude / byoa-codex / …
	Purpose        string         `json:"purpose"`
	RunID          string         `json:"runId,omitempty"`
	ConversationID string         `json:"conversationId,omitempty"`
	Model          string         `json:"model"`
	Usage          *EngineUsage   `json:"usage"`
	LatencyMS      *int64         `json:"latencyMs,omitempty"`
	Status         string         `json:"status"`
	Error          string         `json:"error,omitempty"`
	Extras         map[string]any `json:"extras,omitempty"`
}

const (
	hopWindowMS  = 250
	hopFlushAt   = 10
	hopMaxBuffer = 500
)

type hopReporter struct {
	serverURL string
	getToken  func(ctx context.Context) (string, error)

	mu      sync.Mutex
	buf     []pendingHop
	timer   *time.Timer
	stopped bool
	// 溢出窗口内只告警一次(防日志洪水)。
	warnedOverflow bool
}

func newHopReporter(serverURL string, getToken func(ctx context.Context) (string, error)) *hopReporter {
	return &hopReporter{serverURL: serverURL, getToken: getToken}
}

func (h *hopReporter) push(hop pendingHop) {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	if len(h.buf) >= hopMaxBuffer {
		dropped := h.buf[0]
		h.buf = h.buf[1:]
		if !h.warnedOverflow {
			h.warnedOverflow = true
			time.AfterFunc(10*time.Second, func() {
				h.mu.Lock()
				h.warnedOverflow = false
				h.mu.Unlock()
			})
			slog.Warn("[hop-reporter] buffer overflow — dropped oldest hop",
				"purpose", dropped.Purpose, "model", dropped.Model)
		}
	}
	h.buf = append(h.buf, hop)
	flushNow := len(h.buf) >= hopFlushAt
	var armTimer bool
	if !flushNow && h.timer == nil {
		armTimer = true
	}
	h.mu.Unlock()
	if flushNow {
		go h.flush()
		return
	}
	if armTimer {
		h.mu.Lock()
		if h.timer == nil {
			h.timer = time.AfterFunc(hopWindowMS*time.Millisecond, func() {
				h.mu.Lock()
				h.timer = nil
				h.mu.Unlock()
				go h.flush()
			})
		}
		h.mu.Unlock()
	}
}

// flush:发走当前排队的全部(快照+清空;await 期间的新 push 进新缓冲)。
// 幂等且可重入。批内按 source 分组——服务端每次调用只吃一个 source。
func (h *hopReporter) flush() {
	h.mu.Lock()
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
	if len(h.buf) == 0 {
		h.mu.Unlock()
		return
	}
	batch := h.buf
	h.buf = nil
	h.mu.Unlock()
	bySource := map[string][]pendingHop{}
	var order []string
	for _, hop := range batch {
		if _, ok := bySource[hop.Source]; !ok {
			order = append(order, hop.Source)
		}
		bySource[hop.Source] = append(bySource[hop.Source], hop)
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout())
	defer cancel()
	token, err := h.getToken(ctx)
	if err != nil {
		return // token 不可得——丢弃,daemon 自会恢复
	}
	for _, source := range order {
		runtimeBest(ctx, h.serverURL, "/llm-calls", token, map[string]any{
			"source":        source,
			"hops":          bySource[source],
			"daemonVersion": currentVersion(),
		})
	}
}

func (h *hopReporter) stop() {
	h.mu.Lock()
	h.stopped = true
	h.mu.Unlock()
	h.flush() // 最后一冲——尽力而为,fire-and-forget
}
