// wakebus —— 唤醒总线(#60),对齐 server/src/agents/runtime/wake-bus.ts。
// 把 wake/steer 事件经 Redis pub/sub(cumora:wake:<agentId>)路由到持有该
// agent SSE 长连接的本实例:首个本地订阅者挂上时 SUBSCRIBE,最后一个断开
// 时 UNSUBSCRIBE;Deliver 发布事件并返回接收端数(0 = 全集群无在线 Pod,
// daemon 靠重连后的 inbox 兜底)。同 agent 允许多订阅(滚动重启的重叠窗口),
// Pod 按事件 id 去重。
package wakebus

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ChWakePrefix:每 agent 一条通道。前缀保持精简——每条 PUBLISH 都带着它上线。
const ChWakePrefix = "cumora:wake:"

func channel(agentID string) string { return ChWakePrefix + agentID }

// SSE 写超时:TS 版以 socket.writableLength > 1MB 断开落后订阅者防 OOM;
// Go 的 ResponseWriter 写路径内核缓冲有界、慢客户端只会阻塞写协程,这里用
// 写超时达成同等保护——10s 内写不下去即视为不可排空,断开(唤醒在 inbox
// 里有持久兜底,daemon 重连后补排)。
const sseWriteTimeout = 10 * time.Second

// ssePingEvery:每 25s 一条注释 ping,30s 空闲超时的代理下保活。
const ssePingEvery = 25 * time.Second

// uuidv4:随机 16 字节格式化为 8-4-4-4-12,与 TS randomUUID 同形。
func uuidv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见;退化为时间戳拼接,保唯一性优先。
		return fmt.Sprintf("%012x%d", time.Now().UnixNano(), time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type subscriber struct {
	agentID string
	w       http.ResponseWriter
	mu      sync.Mutex // ResponseWriter 非并发安全:ready/ping/fan-out三条路径互斥
	closed  bool
}

// writeSSE:带写超时的一帧写入;失败标记 closed(close 处理器会做回收)。
func (s *subscriber) writeSSE(frame string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	rc := http.NewResponseController(s.w)
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		s.closed = true
		return
	}
	rc.Flush()
}

// Bus:进程级单例。subs 按 agent 分组;一条 PubSub 连接承载全部通道,
// 与 TS 的单 redisSub 连接同构。
type Bus struct {
	rdb redis.UniversalClient

	mu       sync.Mutex
	subs     map[string]map[*subscriber]struct{}
	pubsub   *redis.PubSub
	psCancel context.CancelFunc
}

func New(rdb redis.UniversalClient) *Bus {
	return &Bus{rdb: rdb, subs: map[string]map[*subscriber]struct{}{}}
}

// ensurePubsub:惰性建立共享 PubSub + 分发协程(首个 attach 时)。
func (b *Bus) ensurePubsub() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pubsub != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	ps := b.rdb.Subscribe(ctx)
	if err := ps.Ping(ctx); err != nil {
		cancel()
		return err
	}
	b.pubsub = ps
	b.psCancel = cancel
	go func() {
		for msg := range ps.Channel() {
			b.fanOut(msg.Channel, msg.Payload)
		}
	}()
	return nil
}

// fanOut:把 Redis 消息扇出给该 agent 的全部本地订阅者。
func (b *Bus) fanOut(ch, payload string) {
	if len(ch) <= len(ChWakePrefix) || ch[:len(ChWakePrefix)] != ChWakePrefix {
		return
	}
	agentID := ch[len(ChWakePrefix):]
	var evt map[string]any
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return // 坏载荷直接丢,对齐 TS 的 try/JSON.parse/return
	}
	b.mu.Lock()
	set := b.subs[agentID]
	subs := make([]*subscriber, 0, len(set))
	for s := range set {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		s.deliver(evt)
	}
}

// deliver:按 TS 帧格式写出 event/id/data 三段。
func (s *subscriber) deliver(evt map[string]any) {
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	kind, _ := evt["kind"].(string)
	id, _ := evt["id"].(string)
	s.writeSSE("event: " + kind + "\n")
	s.writeSSE("id: " + id + "\n")
	s.writeSSE("data: " + string(data) + "\n\n")
}

// Deliver:为 agent 铸一枚事件并发布到集群,返回 Redis 报告的接收端数。
// partial 携带除 id/at 外的全部字段(kind 必填)。
func (b *Bus) Deliver(agentID string, partial map[string]any) (int64, error) {
	kind, _ := partial["kind"].(string)
	evt := make(map[string]any, len(partial)+2)
	for k, v := range partial {
		evt[k] = v
	}
	evt["id"] = kind + "-" + uuidv4()
	evt["at"] = time.Now().UnixMilli()
	data, err := json.Marshal(evt)
	if err != nil {
		return 0, err
	}
	return b.rdb.Publish(context.Background(), channel(agentID), data).Result()
}

// DeliverSteer:steer 味道的 Deliver——Pod 的 SSE 按 event: 行路由,
// 订阅 wake 的 Pod 自动也收 steer,无需单独通道。
func (b *Bus) DeliverSteer(agentID string, payload map[string]any) (int64, error) {
	p := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		p[k] = v
	}
	p["kind"] = "steer"
	return b.Deliver(agentID, p)
}

// ListLocalSubscribedAgents:诊断面——本地仍有活跃订阅者的 agent。
func (b *Bus) ListLocalSubscribedAgents() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.subs))
	for id, set := range b.subs {
		for s := range set {
			if !s.isClosed() {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

func (s *subscriber) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Attach:把一条 SSE 长响应挂上总线。写头、(必要时)SUBSCRIBE 通道、
// 发 ready 帧、起 25s ping,然后保持响应打开直到客户端断开(reqCtx
// 取消)。SUBSCRIBE 必须先于 ready——否则间隙期到达的唤醒会被静默丢掉
// (inbox 兜底存在,但能不依赖兜底就不依赖)。
func (b *Bus) Attach(agentID string, w http.ResponseWriter, reqCtx context.Context) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 关 nginx 缓冲
	rc := http.NewResponseController(w)
	rc.Flush()

	if err := b.ensurePubsub(); err != nil {
		// Redis 不可达:SSE 会话收不到任何扇出,等同半开——直接 503。
		http.Error(w, "wake bus unavailable", http.StatusServiceUnavailable)
		return
	}

	s := &subscriber{agentID: agentID, w: w}

	b.mu.Lock()
	set := b.subs[agentID]
	if set == nil {
		set = map[*subscriber]struct{}{}
		b.subs[agentID] = set
	}
	isFirst := len(set) == 0
	set[s] = struct{}{}
	b.mu.Unlock()

	if isFirst {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := b.pubsub.Subscribe(ctx, channel(agentID))
		cancel()
		if err != nil {
			b.mu.Lock()
			delete(set, s)
			if len(set) == 0 {
				delete(b.subs, agentID)
			}
			b.mu.Unlock()
			slog.Warn("[wake-bus] redis subscribe failed", "agent", agentID, "err", err)
			http.Error(w, "wake bus subscribe failed", http.StatusServiceUnavailable)
			return
		}
	}
	slog.Info("[wake-bus] +sub", "agent", agentID, "local", len(set), "redisSubscribed", isFirst)

	s.writeSSE("event: ready\ndata: {\"agentId\":\"" + agentID + "\",\"at\":" +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + "}\n\n")

	ping := time.NewTicker(ssePingEvery)
	defer ping.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ping.C:
				if s.isClosed() {
					return
				}
				s.writeSSE(": ping " + strconv.FormatInt(time.Now().UnixMilli(), 10) + "\n\n")
			}
		}
	}()
	// 阻塞到客户端断开(req context 取消)再走清理。
	<-reqCtx.Done()
	<-done

	b.cleanup(s)
}

// cleanup:摘除订阅者;最后一个断开时 UNSUBSCRIBE(尽力而为,
// 失败只留下扇出给空集的微小泄漏,下次 attach 同 agent 时自愈)。
func (b *Bus) cleanup(s *subscriber) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	b.mu.Lock()
	set := b.subs[s.agentID]
	if set != nil {
		delete(set, s)
		if len(set) == 0 {
			delete(b.subs, s.agentID)
		}
	}
	last := set != nil && len(set) == 0
	ps := b.pubsub
	b.mu.Unlock()

	slog.Info("[wake-bus] -sub", "agent", s.agentID, "last", last)
	if last && ps != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ps.Unsubscribe(ctx, channel(s.agentID)); err != nil {
			slog.Warn("[wake-bus] redis unsubscribe failed", "agent", s.agentID, "err", err)
		}
	}
}
