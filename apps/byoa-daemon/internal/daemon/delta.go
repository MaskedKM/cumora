// daemon 包 delta —— #210 流式增量上报:引擎产出的文本前缀按块报给
// 服务端 /runtime/message-delta(服务端按租户广播 message.delta,前端
// 增量渲染)。断流侦察结论:token 级流断在 engine→shim 边界(引擎把
// 完整回复体作为 cumora shim 的 argv 一次性交付),daemon 能看到的增
// 量只有引擎自身的流输出——Claude 逐跳 assistant 文本块、codex
// item/agentMessage/delta 真 token 流。本上报器不区分来源,统一按
// “已产出前缀”合并上送:
//   - 频率合并:同一 (turn, conversation) 流内 ≥200ms 才出一帧(块级
//     文本天然粗;codex token 流靠窗口聚团),帧在飞时新文本只入缓冲;
//   - 幂等终局:终局恒以 /runtime/cli reply 的 message.new 为准——delta
//     只上屏不入库,turn 结束 finish() 补 done=true(前端退场兜底;回帖
//     已落地时 message.new 先到,前端早已收口,done 落空无害);
//   - 归因:目标会话由 runner 按 wake 锚定(与 hop 台账同款归因纪律),
//     无锚定会话的文本直接丢弃——宁缺勿错投。
package daemon

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// deltaStreamCap:单流上报总量帽(字节),对齐前端 64KB 合帧阀——
	// 超帽即引擎在无界独白,截停上报,终局 message.new 兜底。
	deltaStreamCap = 64 * 1024
	// deltaSendTimeout:单帧上报上限(短请求,慢服务器不积压下一帧)。
	deltaSendTimeout = 5 * time.Second
)

// deltaFlushWindow:同一流两帧的最小间隔(#210 防风暴:块级 ≥200ms,
// token 级增量在窗口内聚团)。var 供测试收窄窗口。
var deltaFlushWindow = 200 * time.Millisecond

// deltaStream:一个 (turn, conversation) 的在途流。id 为 daemon 铸造的
// 流 id(ds-<hex>);终局 message.new 的 id 与它无关——前端按
// (conversationId, authorId) 收口换真消息,不按 id 配对。
type deltaStream struct {
	id       string
	sequence int
	buffered strings.Builder
	sent     int // 已上送字节数(含在飞)
	inflight atomic.Bool
	timer    *time.Timer
	lastSend time.Time
}

// deltaReporter:每 runner 一份;stream 按 conversationId 分桶,一个
// turn 可能锚定多个会话(steer 重锚)。
type deltaReporter struct {
	serverURL string
	token     func(ctx context.Context) (string, error)

	mu      sync.Mutex
	streams map[string]*deltaStream
}

func newDeltaReporter(serverURL string, token func(ctx context.Context) (string, error)) *deltaReporter {
	return &deltaReporter{
		serverURL: serverURL,
		token:     token,
		streams:   map[string]*deltaStream{},
	}
}

// push:追加一段已产出文本(conversationID 为空 = 无锚定,丢弃)。
// 永不阻塞调用方(引擎泵协程):出站一律异步,窗口未满只入缓冲。
func (d *deltaReporter) push(conversationID, text string) {
	if d == nil || conversationID == "" || text == "" {
		return
	}
	d.mu.Lock()
	st := d.streams[conversationID]
	if st == nil {
		st = &deltaStream{id: deltaStreamID(), lastSend: time.Now()}
		d.streams[conversationID] = st
	}
	if budget := deltaStreamCap - st.sent - st.buffered.Len(); budget <= 0 {
		d.mu.Unlock()
		return // 超帽截停——终局 message.new 兜底
	} else if len(text) > budget {
		st.buffered.WriteString(trimToBytes(text, budget)) // 末块按帽截(rune 安全)
		d.mu.Unlock()
		return
	}
	st.buffered.WriteString(text)
	if st.inflight.Load() {
		d.mu.Unlock()
		return // 在飞帧返回后由 drain 决定续冲
	}
	elapsed := time.Since(st.lastSend)
	if elapsed >= deltaFlushWindow {
		d.flushLocked(conversationID, st)
		d.mu.Unlock()
		return
	}
	if st.timer == nil {
		remaining := deltaFlushWindow - elapsed
		st.timer = time.AfterFunc(remaining, func() { d.tick(conversationID) })
	}
	d.mu.Unlock()
}

// tick:窗口到点——把缓冲冲出去(流已被 finish 收走则自然落空)。
func (d *deltaReporter) tick(conversationID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.streams[conversationID]
	if st == nil {
		return
	}
	st.timer = nil
	if !st.inflight.Load() && st.buffered.Len() > 0 {
		d.flushLocked(conversationID, st)
	}
}

// flushLocked:领走缓冲、定序、异步上送(调用方持 mu)。
func (d *deltaReporter) flushLocked(conversationID string, st *deltaStream) {
	text := st.buffered.String()
	st.buffered.Reset()
	st.sequence++
	seq := st.sequence
	st.lastSend = time.Now()
	st.sent += len(text)
	st.inflight.Store(true)
	go func() {
		d.post(conversationID, st.id, text, seq, false)
		st.inflight.Store(false)
		d.drain(conversationID, st)
	}()
}

// drain:一帧落定——窗口已满即续冲,否则武装下一拍(停流后静默)。
func (d *deltaReporter) drain(conversationID string, st *deltaStream) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.streams[conversationID] != st {
		return // 流已被 finish 收走
	}
	if st.buffered.Len() == 0 || st.inflight.Load() {
		return
	}
	elapsed := time.Since(st.lastSend)
	if elapsed >= deltaFlushWindow {
		d.flushLocked(conversationID, st)
		return
	}
	if st.timer == nil {
		st.timer = time.AfterFunc(deltaFlushWindow-elapsed, func() { d.tick(conversationID) })
	}
}

// finish:turn 终结——冲净尾帧再补 done=true(done 帧不带文本:前端
// 对 done 的语义是弃尾退场,尾巴必须先行成帧)。同步尽力:最多等在飞
// 帧 2s,超时放弃该流(前端 45s typing 兜底退场)。
func (d *deltaReporter) finish() {
	if d == nil {
		return
	}
	d.mu.Lock()
	old := d.streams
	d.streams = map[string]*deltaStream{}
	d.mu.Unlock()
	for conversationID, st := range old {
		if st.timer != nil {
			st.timer.Stop()
		}
		deadline := time.Now().Add(2 * time.Second)
		for st.inflight.Load() && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if st.inflight.Load() {
			continue // 在飞帧卡死——放弃 done,交前端兜底
		}
		tail := st.buffered.String()
		seq := st.sequence
		if tail != "" {
			seq++
			d.post(conversationID, st.id, tail, seq, false)
		}
		d.post(conversationID, st.id, "", seq+1, true)
	}
}

// post:单帧上报,尽力而为(失败只 Debug——delta 是瞬态体验,重试无义
// 义,下一帧自会带上更新的前缀)。
func (d *deltaReporter) post(conversationID, streamID, text string, sequence int, done bool) {
	ctx, cancel := context.WithTimeout(context.Background(), deltaSendTimeout)
	defer cancel()
	token, err := d.token(ctx)
	if err != nil {
		slog.Debug("[delta] token mint failed — dropping frame", "convo", conversationID, "err", err)
		return
	}
	if err := apiCall(ctx, d.serverURL, http.MethodPost, "/runtime/message-delta", token, map[string]any{
		"conversationId": conversationID,
		"messageId":      streamID,
		"delta":          text,
		"sequence":       sequence,
		"done":           done,
	}, nil); err != nil {
		slog.Debug("[delta] frame dropped", "convo", conversationID, "seq", sequence, "done", done, "err", err)
	}
}

// trimToBytes:按字节帽截串,回退到 rune 边界(劈半的多字节字符会毒化
// 前端的 markdown 解析)。
func trimToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// deltaStreamID:daemon 铸的在途流 id(ds-<8hex>)。crypto/rand,不占
// math/rand 全局源;失败退时间戳(id 只需不撞车,无安全语义)。
func deltaStreamID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ds-%x", time.Now().UnixNano())
	}
	return fmt.Sprintf("ds-%x", b)
}
