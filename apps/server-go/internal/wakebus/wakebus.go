// wakebus —— 唤醒总线(#60),对齐 已退役 TS server 的 agents/runtime/wake-bus.ts。
// 把 wake/steer 事件经 Redis pub/sub(cumora:wake:<agentId>)路由到持有该
// agent SSE 长连接的本实例:首个本地订阅者挂上时 SUBSCRIBE,最后一个断开
// 时 UNSUBSCRIBE;Deliver 发布事件并返回接收端数(0 = 全集群无在线 Pod,
// daemon 靠重连后的 inbox 兜底)。同 agent 允许多订阅(滚动重启的重叠窗口),
// Pod 按事件 id 去重。
//
// 分发模型(评审 M2 修正):Redis 分发协程绝不直接写 ResponseWriter——
// 一个停止读取的慢客户端会让写阻塞到写超时,把总线卡在它身上、并让
// go-redis Channel() 的有界缓冲溢出丢事件(全集群静默丢唤醒)。对齐 TS
// 的背压语义(writableLength > 1MB 即弃该订阅者):每订阅者一条有界事件
// 队列 + 一个专职写协程;队列写满(不排空)即弃该订阅者——唤醒在 inbox
// 里有持久兜底,daemon 重连后补排。
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

const (
	// sseWriteTimeout:单帧 SSE 写超时(TS 以 socket.writableLength > 1MB
	// 断开落后订阅者;Go 写路径内核缓冲有界,以写超时达成同等保护)。
	sseWriteTimeout = 10 * time.Second
	// ssePingEvery:每 25s 一条注释 ping,30s 空闲超时的代理下保活。
	ssePingEvery = 25 * time.Second
	// subQueueDepth:每订阅者的事件队列深度。写满 = 客户端不排空 → 弃之
	// (对齐 TS 的 1MB 背压断连;64 条小事件远小于 1MB 但同向同义,
	// 且每事件有 id,重连的 Pod 去重兜底)。
	subQueueDepth = 64
)

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
	// ch:待写事件的有界队列;close 即通知写协程收尾。仅经 bus.mu 保护。
	ch     chan map[string]any
	closed bool
}

// writeSSE:带写超时的一帧写出;失败由调用方处置(弃订阅者)。
func (s *subscriber) writeSSE(frame string) bool {
	rc := http.NewResponseController(s.w)
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		return false
	}
	rc.Flush()
	return true
}

// deliver:按 TS 帧格式写出 event/id/data 三段。
func (s *subscriber) deliver(evt map[string]any) bool {
	data, err := json.Marshal(evt)
	if err != nil {
		return true
	}
	kind, _ := evt["kind"].(string)
	id, _ := evt["id"].(string)
	if !s.writeSSE("event: " + kind + "\n") {
		return false
	}
	if !s.writeSSE("id: " + id + "\n") {
		return false
	}
	return s.writeSSE("data: " + string(data) + "\n\n")
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
// Ping 失败必须 Close——留着一个不断重连的 PubSub 对象,Redis 故障期
// 每次重试都泄漏一个(评审 M10)。
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
		_ = ps.Close()
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

// fanOut:Redis 消息 → 该 agent 的全部本地订阅者队列(非阻塞投递)。
// 分发协程永不被慢订阅者阻塞——满队即弃该订阅者(背压保护)。
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
		b.enqueue(s, evt)
	}
}

// enqueue:非阻塞投递;满队 = 不可排空 → 弃置该订阅者并从表中摘除
// (写协程随队列 close 而退,HTTP 响应随下一次写失败或客户端断开关闭)。
func (b *Bus) enqueue(s *subscriber, evt map[string]any) {
	b.mu.Lock()
	if s.closed {
		b.mu.Unlock()
		return
	}
	select {
	case s.ch <- evt:
		b.mu.Unlock()
	default:
		s.closed = true
		set := b.subs[s.agentID]
		if set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(b.subs, s.agentID)
			}
		}
		b.mu.Unlock()
		slog.Warn("[wake-bus] subscriber not draining — dropped (inbox 兜底)", "agent", s.agentID)
		close(s.ch)
	}
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
		if len(set) > 0 {
			out = append(out, id)
		}
	}
	return out
}

// Attach:把一条 SSE 长响应挂上总线。写头、(必要时)SUBSCRIBE 通道、
// 发 ready 帧、起写协程(事件 + 25s ping),然后保持响应打开直到客户端
// 断开(reqCtx 取消)。SUBSCRIBE 必须先于 ready——否则间隙期到达的唤醒
// 会被静默丢掉(inbox 兜底存在,但能不依赖兜底就不依赖)。
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

	s := &subscriber{agentID: agentID, w: w, ch: make(chan map[string]any, subQueueDepth)}

	b.mu.Lock()
	set := b.subs[agentID]
	if set == nil {
		set = map[*subscriber]struct{}{}
		b.subs[agentID] = set
	}
	isFirst := len(set) == 0
	set[s] = struct{}{}
	local := len(set) // 锁内取值,防与并发 detach 的删集竞态
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
	slog.Info("[wake-bus] +sub", "agent", agentID, "local", local, "redisSubscribed", isFirst)

	if !s.writeSSE("event: ready\ndata: {\"agentId\":\"" + agentID + "\",\"at\":" +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + "}\n\n") {
		b.detach(s)
		return
	}

	// 写协程:本订阅者全部 SSE 写出在此串行化(事件帧 + ping),读侧
	// 断开(reqCtx 取消)立即退出——清理不等 ping 节拍(评审 M1)。
	ping := time.NewTicker(ssePingEvery)
	defer ping.Stop()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-reqCtx.Done():
				return
			case <-ping.C:
				if !s.writeSSE(": ping " + strconv.FormatInt(time.Now().UnixMilli(), 10) + "\n\n") {
					return
				}
			case evt, ok := <-s.ch:
				if !ok {
					return
				}
				if !s.deliver(evt) {
					return
				}
			}
		}
	}()
	<-reqCtx.Done()
	<-writerDone
	b.detach(s)
}

// detach:写协程退出后由 Attach 调用——关队列、摘订阅者;最后一个断开
// 时 UNSUBSCRIBE(尽力而为,失败只留下扇出给空集的微小泄漏,下次 attach
// 同 agent 时自愈)。
func (b *Bus) detach(s *subscriber) {
	b.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
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
