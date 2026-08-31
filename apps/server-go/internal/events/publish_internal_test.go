package events

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// capturePublisher:抓通道与载荷的假发布方。
type capturePublisher struct {
	mu       sync.Mutex
	payloads map[string][][]byte
}

func (c *capturePublisher) Publish(_ context.Context, channel string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.payloads == nil {
		c.payloads = map[string][][]byte{}
	}
	c.payloads[channel] = append(c.payloads[channel], payload)
	return nil
}

func (c *capturePublisher) take(t *testing.T, channel string) []json.RawMessage {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []json.RawMessage
	for _, p := range c.payloads[channel] {
		out = append(out, json.RawMessage(p))
	}
	return out
}

// TestMessageDeltaPayloadShape:#210 发布方 —— 载荷 = 契约 MessageDeltaEvent
// (生成物字段),通道 = ChMessageDelta;companyId 必打标(空串省键会被
// wsx 桥拒路由,发布即死帧——守卫在 MessageDelta 里直接拦)。
func TestMessageDeltaPayloadShape(t *testing.T) {
	cap := &capturePublisher{}
	prev := active
	SetPublisher(cap)
	defer SetPublisher(prev)

	MessageDelta(context.Background(), "co-1", "cv-1", "ds-abc", "a-1", "Hel", 1, false)
	MessageDelta(context.Background(), "co-1", "cv-1", "ds-abc", "a-1", "lo", 2, true)

	frames := cap.take(t, ChMessageDelta)
	if len(frames) != 2 {
		t.Fatalf("want 2 delta frames on %s, got %d", ChMessageDelta, len(frames))
	}
	var first struct {
		Type           string `json:"type"`
		CompanyID      string `json:"companyId"`
		ConversationID string `json:"conversationId"`
		MessageID      string `json:"messageId"`
		AuthorID       string `json:"authorId"`
		Delta          string `json:"delta"`
		Sequence       int    `json:"sequence"`
		Done           bool   `json:"done"`
	}
	if err := json.Unmarshal(frames[0], &first); err != nil {
		t.Fatalf("frame not valid JSON: %v", err)
	}
	if first.Type != EventMessageDelta || first.CompanyID != "co-1" || first.ConversationID != "cv-1" ||
		first.MessageID != "ds-abc" || first.AuthorID != "a-1" || first.Delta != "Hel" || first.Sequence != 1 || first.Done {
		t.Fatalf("delta frame shape mismatch: %s", frames[0])
	}
	var last struct {
		Delta string `json:"delta"`
		Done  bool   `json:"done"`
	}
	if err := json.Unmarshal(frames[1], &last); err != nil {
		t.Fatalf("terminal frame not valid JSON: %v", err)
	}
	if !last.Done || last.Delta != "lo" {
		t.Fatalf("terminal frame must carry done=true with its tail, got %s", frames[1])
	}
}

// TestMessageDeltaRefusesUntagged:空 companyId 是死帧(wsx 桥拒路由)——
// 发布方守卫直接不发,而不是发出一条永远无人收到的事件。
func TestMessageDeltaRefusesUntagged(t *testing.T) {
	cap := &capturePublisher{}
	prev := active
	SetPublisher(cap)
	defer SetPublisher(prev)

	MessageDelta(context.Background(), "", "cv-1", "ds-abc", "a-1", "Hel", 1, false)
	if frames := cap.take(t, ChMessageDelta); len(frames) != 0 {
		t.Fatalf("empty-companyId delta must not publish, got %d frames", len(frames))
	}
}
