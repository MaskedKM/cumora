// daemon 包 transcript —— #260 工具级执行转录:引擎事件 → 条目 → 批量
// 上送(POST /runtime/transcript-batch)。批处理器 200ms 冲刷 + finish
// 兜底;单 run 2000 条帽;上送 best-effort(转录是观测面,不打断 turn)。
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

// TranscriptEntry:一条转录(引擎事件的最小投影)。Seq 由 runner 侧统一
// 分配(引擎只报内容)。
type TranscriptEntry struct {
	Type    string // text | thinking | tool_use | tool_result | note
	Tool    string
	Content string
	Input   any
}

const (
	transcriptRunCap       = 2000 // 单 run 条帽(超出静默丢弃,note 一条封顶说明)
	transcriptFlushMS      = 200 * time.Millisecond
	transcriptContentCapDa = 8 * 1024 // daemon 侧预截(content)
)

// transcriptBatcher:一轮 run 的收集+冲刷。goroutine 安全。
type transcriptBatcher struct {
	mu      sync.Mutex
	seq     int
	count   int
	capped  bool
	entries []map[string]any
	runID   string

	flush func(runID string, entries []map[string]any)
}

func newTranscriptBatcher(runID string, flush func(string, []map[string]any)) *transcriptBatcher {
	b := &transcriptBatcher{runID: runID, flush: flush}
	go func() {
		t := time.NewTicker(transcriptFlushMS)
		defer t.Stop()
		for range t.C {
			if b.tick() {
				return
			}
		}
	}()
	return b
}

// tick:冲刷一轮;返回 true = 已 finish(循环退场)。
func (b *transcriptBatcher) tick() bool {
	// flush 锁内快照(评审 #326 P1-3:锁外重读与 finish 置 nil 撞窗会
	// nil 调用 panic 掉 ticker goroutine = daemon 整进程崩)。
	b.mu.Lock()
	flushFn := b.flush
	pending := b.entries
	b.entries = nil
	b.mu.Unlock()
	if flushFn == nil {
		return true
	}
	if len(pending) > 0 {
		flushFn(b.runID, pending)
	}
	return false
}

// emit:追加一条(分配 seq;超帽后只留一条 note 封顶)。
func (b *transcriptBatcher) emit(e TranscriptEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.count >= transcriptRunCap {
		if !b.capped {
			b.capped = true
			b.seq++
			b.entries = append(b.entries, map[string]any{
				"seq": b.seq, "type": "note",
				"content": "transcript cap reached (2000 entries) — remaining events dropped",
			})
		}
		return
	}
	b.count++
	b.seq++
	// 字节帽 + rune 边界(评审 #326 P2-6:与服务端字节语义对齐,rune 帽
	// 会让 CJK 预截失效被服务端二次再截)。
	content := e.Content
	if len(content) > transcriptContentCapDa {
		content = trimRunesSafeDa(content, transcriptContentCapDa)
	}
	b.entries = append(b.entries, map[string]any{
		"seq": b.seq, "type": e.Type, "tool": e.Tool, "content": content, "input": clampInputDa(e.Input),
	})
}

// finish:turn 结算——冲净尾批再停循环。
func (b *transcriptBatcher) finish() {
	b.mu.Lock()
	flushFn := b.flush
	b.flush = nil // tick 见 nil 即退
	pending := b.entries
	b.entries = nil
	b.mu.Unlock()
	if len(pending) > 0 && flushFn != nil {
		flushFn(b.runID, pending)
	}
}

// postTranscriptBatch:runner 侧的上送实现(best-effort,不打断 turn)。
func (r *AgentRunner) postTranscriptBatch(runID string, entries []map[string]any) {
	token, err := r.ensureToken()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 服务端单批帽 100——不分块会被整批 400(评审 #326 P1-4)。
	for i := 0; i < len(entries); i += transcriptServerBatchCap {
		end := i + transcriptServerBatchCap
		if end > len(entries) {
			end = len(entries)
		}
		_ = apiCall(ctx, r.cfg.ServerURL, http.MethodPost, "/runtime/transcript-batch", token,
			map[string]any{"runId": runID, "entries": entries[i:end]}, nil)
	}
}

const transcriptServerBatchCap = 100

// contentTranscriptEntries:claude stream-json 的 content 块数组 → 转录
// 条目。assistant 消息产出 text/thinking/tool_use;user 消息(tool_result
// 回显)产出 tool_result。块形状不熟识的静默跳过。
func contentTranscriptEntries(evType string, content any) []TranscriptEntry {
	items, ok := content.([]any)
	if !ok {
		return nil
	}
	isUser := evType == "user"
	var out []TranscriptEntry
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		ty, _ := m["type"].(string)
		switch {
		case ty == "text" && !isUser:
			if t, _ := m["text"].(string); t != "" {
				out = append(out, TranscriptEntry{Type: "text", Content: t})
			}
		case ty == "thinking" && !isUser:
			if t, _ := m["thinking"].(string); t != "" {
				out = append(out, TranscriptEntry{Type: "thinking", Content: t})
			}
		case ty == "tool_use" && !isUser:
			name, _ := m["name"].(string)
			out = append(out, TranscriptEntry{Type: "tool_use", Tool: name, Input: m["input"]})
		case ty == "tool_result" && isUser:
			var c string
			switch v := m["content"].(type) {
			case string:
				c = v
			case []any: // [{type:text,text:...}] 数组形态
				for _, sub := range v {
					if sm, ok := sub.(map[string]any); ok {
						if t, _ := sm["text"].(string); t != "" {
							c += t
						}
					}
				}
			}
			out = append(out, TranscriptEntry{Type: "tool_result", Content: c})
		}
	}
	return out
}

// trimRunesSafeDa:字节帽内按 rune 边界截断。
func trimRunesSafeDa(s string, capB int) string {
	if len(s) <= capB {
		return s
	}
	cut := capB
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// clampInputDa:input 序列化超帽降级为截断提示(评审 #326 P2-5:emit 侧
// 即钳,防大 input 批量整批 400 / 放大驻留)。
func clampInputDa(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if len(b) <= transcriptContentCapDa {
		return v
	}
	return map[string]any{"truncated": true, "prefix": trimRunesSafeDa(string(b), transcriptContentCapDa)}
}
