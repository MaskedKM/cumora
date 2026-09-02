package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// 批处理器:seq 单调、finish 冲净尾批、超帽封顶一条 note。
func TestTranscriptBatcherSeqAndFinish(t *testing.T) {
	var mu sync.Mutex
	var got [][]map[string]any
	flush := func(_ string, entries []map[string]any) {
		mu.Lock()
		got = append(got, entries)
		mu.Unlock()
	}
	b := newTranscriptBatcher("run-1", flush)
	b.emit(TranscriptEntry{Type: "text", Content: "a"})
	b.emit(TranscriptEntry{Type: "tool_use", Tool: "bash", Input: map[string]any{"command": "ls"}})
	b.finish()
	time.Sleep(50 * time.Millisecond) // 等 tick 循环退出
	mu.Lock()
	defer mu.Unlock()
	total := 0
	var seqs []int
	for _, batch := range got {
		total += len(batch)
		for _, e := range batch {
			seqs = append(seqs, e["seq"].(int))
		}
	}
	if total != 2 {
		t.Fatalf("finish 应冲净 2 条,got %d", total)
	}
	if seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("seq 应单调 1,2: %v", seqs)
	}
}

func TestTranscriptBatcherCap(t *testing.T) {
	b := newTranscriptBatcher("run-2", func(string, []map[string]any) {})
	for i := 0; i < transcriptRunCap+50; i++ {
		b.emit(TranscriptEntry{Type: "text", Content: "x"})
	}
	b.mu.Lock()
	count, capped, lastType := b.count, b.capped, b.entries[len(b.entries)-1]["type"]
	b.mu.Unlock()
	if count != transcriptRunCap {
		t.Fatalf("count 应封在 %d,got %d", transcriptRunCap, count)
	}
	if !capped || lastType != "note" {
		t.Fatal("超帽后应追加一条 note 封顶说明")
	}
	b.finish()
}

// claude content 提取:assistant(text/thinking/tool_use)与 user(tool_result)。
func TestContentTranscriptEntries(t *testing.T) {
	assistant := []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "thinking", "thinking": "hmm"},
		map[string]any{"type": "tool_use", "name": "bash", "input": map[string]any{"cmd": "ls"}},
	}
	got := contentTranscriptEntries("assistant", assistant)
	if len(got) != 3 || got[0].Type != "text" || got[1].Type != "thinking" ||
		got[2].Type != "tool_use" || got[2].Tool != "bash" {
		t.Fatalf("assistant 提取: %+v", got)
	}
	user := []any{map[string]any{"type": "tool_result", "content": []any{map[string]any{"type": "text", "text": "out"}}}}
	got = contentTranscriptEntries("user", user)
	if len(got) != 1 || got[0].Type != "tool_result" || !strings.Contains(got[0].Content, "out") {
		t.Fatalf("user tool_result 提取: %+v", got)
	}
}
