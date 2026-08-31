// delta 上报器单测(#210):窗口聚团 / 序号递增 / 尾帧+done 终结 /
// 64KB 截停 / 无锚定丢弃 / assistant 文本块抽取。httptest 假服务端抓帧,
// 窗口收窄到 20ms 让节奏可控。
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type deltaFrame struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Delta          string `json:"delta"`
	Sequence       int    `json:"sequence"`
	Done           bool   `json:"done"`
}

type deltaSink struct {
	mu     sync.Mutex
	frames []deltaFrame
	ids    []string // bearer tokens
}

func newDeltaSink(t *testing.T) (*deltaSink, *httptest.Server) {
	t.Helper()
	sink := &deltaSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runtime/message-delta" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if tok := r.Header.Get("Authorization"); tok != "Bearer tok" {
			t.Errorf("missing/incorrect bearer: %q", tok)
		}
		var f deltaFrame
		_ = json.NewDecoder(r.Body).Decode(&f)
		sink.mu.Lock()
		sink.frames = append(sink.frames, f)
		sink.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return sink, srv
}

func (s *deltaSink) snapshot() []deltaFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]deltaFrame(nil), s.frames...)
}

func (s *deltaSink) waitFor(t *testing.T, n int) []deltaFrame {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.snapshot(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("frames did not reach %d, got %+v", n, s.snapshot())
	return nil
}

func TestDeltaReporterCoalescesWithinWindow(t *testing.T) {
	prev := deltaFlushWindow
	deltaFlushWindow = 20 * time.Millisecond
	defer func() { deltaFlushWindow = prev }()

	sink, srv := newDeltaSink(t)
	d := newDeltaReporter(srv.URL, func(context.Context) (string, error) { return "tok", nil })

	// 窗口内三个 token 级碎片 → 一帧合并。
	d.push("c1", "Hel")
	d.push("c1", "lo")
	d.push("c1", " world")
	frames := sink.waitFor(t, 1)
	if frames[0].Delta != "Hello world" || frames[0].Done || frames[0].Sequence != 1 {
		t.Fatalf("first frame mismatch: %+v", frames[0])
	}
	if frames[0].MessageID == "" || !strings.HasPrefix(frames[0].MessageID, "ds-") {
		t.Fatalf("stream id must be ds-<hex>, got %q", frames[0].MessageID)
	}

	// 下一窗:新前缀,序号递增,流 id 不变。
	time.Sleep(30 * time.Millisecond)
	d.push("c1", "!")
	frames = sink.waitFor(t, 2)
	if frames[1].Delta != "!" || frames[1].Sequence != 2 || frames[1].MessageID != frames[0].MessageID {
		t.Fatalf("second frame mismatch: %+v (want id %s)", frames[1], frames[0].MessageID)
	}
}

func TestDeltaReporterFinishFlushesTailThenDone(t *testing.T) {
	prev := deltaFlushWindow
	deltaFlushWindow = 500 * time.Millisecond // 窗口内 push,全靠 finish 冲
	defer func() { deltaFlushWindow = prev }()

	sink, srv := newDeltaSink(t)
	d := newDeltaReporter(srv.URL, func(context.Context) (string, error) { return "tok", nil })

	d.push("c1", "partial tail")
	d.finish() // 同步:尾帧 + done(前端 done 语义是弃尾,尾帧必须先行)

	frames := sink.waitFor(t, 2)
	if frames[0].Delta != "partial tail" || frames[0].Done || frames[0].Sequence != 1 {
		t.Fatalf("tail frame must precede done: %+v", frames[0])
	}
	if !frames[1].Done || frames[1].Delta != "" || frames[1].Sequence != 2 || frames[1].MessageID != frames[0].MessageID {
		t.Fatalf("terminal frame must be empty-delta done: %+v", frames[1])
	}

	// finish 后再 push = 新 turn 的输入?不——调用方纪律是每 turn 一只
	// reporter;这里只验证不死锁、不panic。
	d.push("c1", "after")
	d.finish()
}

// #253 评审 P3-1:超 16K 字节的单块必须拆帧顺序上送——server 侧按
// UTF-16 码元截 16K,而 UTF-8 字节数 ≥ 码元数恒成立,16K 字节分帧永不
// 触截断,长代码回答不再出现中段空洞。
func TestDeltaReporterSplitsOversizeFrames(t *testing.T) {
	prev := deltaFlushWindow
	deltaFlushWindow = 500 * time.Millisecond // 窗口内 push,靠 finish 冲
	defer func() { deltaFlushWindow = prev }()

	sink, srv := newDeltaSink(t)
	d := newDeltaReporter(srv.URL, func(context.Context) (string, error) { return "tok", nil })

	// 90KB 输入先被 64KB 流帽 rune 安全截停(21845 码=65535B 入流),
	// 再按 16K 字节分帧:15999B×4 + 1539B → 5 帧文本 + 1 帧 done。
	big := strings.Repeat("码", 30000)
	want := strings.Repeat("码", 21845) // trimToBytes(65536) 的 rune 安全截
	d.push("c1", big)
	d.finish()

	frames := sink.waitFor(t, 6)
	var got strings.Builder
	for i, f := range frames[:5] {
		if f.Done {
			t.Fatalf("frame %d must carry text, got done", i)
		}
		if len(f.Delta) > deltaFrameChunkBytes {
			t.Fatalf("frame %d oversized: %d bytes", i, len(f.Delta))
		}
		if f.Sequence != i+1 {
			t.Fatalf("frame %d sequence must be contiguous: got %d", i, f.Sequence)
		}
		if i > 0 && f.MessageID != frames[0].MessageID {
			t.Fatalf("frame %d stream id drifted", i)
		}
		got.WriteString(f.Delta)
	}
	if got.String() != want {
		t.Fatalf("chunked replay must equal capped original: got %d bytes, want %d", got.Len(), len(want))
	}
	if !frames[5].Done || frames[5].Delta != "" || frames[5].Sequence != 6 {
		t.Fatalf("terminal must be empty done with next sequence: %+v", frames[5])
	}
}

func TestDeltaReporterCapAndUnanchored(t *testing.T) {
	prev := deltaFlushWindow
	deltaFlushWindow = 10 * time.Millisecond
	defer func() { deltaFlushWindow = prev }()

	sink, srv := newDeltaSink(t)
	d := newDeltaReporter(srv.URL, func(context.Context) (string, error) { return "tok", nil })

	// 无锚定会话:直接丢弃,不出帧。
	d.push("", "nowhere")
	time.Sleep(40 * time.Millisecond)
	if got := len(sink.snapshot()); got != 0 {
		t.Fatalf("unanchored text must not report, got %d frames", got)
	}

	// 64KB 帽:第二块按帽截(rune 安全),超帽后的增量静默截停。
	big := strings.Repeat("a", 40*1024)
	d.push("c1", big)
	d.push("c1", big)   // 80KB > 64KB cap:末 16KB 按帽截
	d.push("c1", "xxx") // 已满帽:不再接受
	// #253 评审 P3-1 起,帽内文本也按 16K 字节分帧:总字节仍是帽,
	// 但帧形为 ≤16K 的连续序号多帧(65536B → 5 帧)。
	frames := sink.waitFor(t, 5)
	gotBytes := 0
	for i, f := range frames {
		if len(f.Delta) > deltaFrameChunkBytes {
			t.Fatalf("frame %d oversized: %d bytes", i, len(f.Delta))
		}
		if f.Sequence != i+1 {
			t.Fatalf("frame %d sequence must be contiguous: %d", i, f.Sequence)
		}
		gotBytes += len(f.Delta)
	}
	if gotBytes != deltaStreamCap {
		t.Fatalf("stream must stop at the %d-byte cap, got %d bytes", deltaStreamCap, gotBytes)
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(sink.snapshot()); got != 5 {
		t.Fatalf("over-cap text must not report more frames, got %d", got)
	}
	d.finish()
}

func TestAssistantTextBlocks(t *testing.T) {
	content := []any{
		map[string]any{"type": "thinking", "thinking": "hidden"},
		map[string]any{"type": "text", "text": "first"},
		map[string]any{"type": "tool_use", "id": "t1", "name": "Bash"},
		map[string]any{"type": "text", "text": "  second  "},
	}
	if got := assistantTextBlocks(content); got != "first\n\nsecond" {
		t.Fatalf("want paragraph-joined text blocks, got %q", got)
	}
	if got := assistantTextBlocks(nil); got != "" {
		t.Fatalf("nil content must yield empty, got %q", got)
	}
	if got := assistantTextBlocks([]any{map[string]any{"type": "text", "text": "   "}}); got != "" {
		t.Fatalf("blank text must yield empty, got %q", got)
	}
}
