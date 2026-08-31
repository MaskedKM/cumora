// wsx/bridge —— 聊天推送面(#202):订阅公司域 Redis 通道,按连接的
// 租户成员资格过滤、原样转发到已鉴权 WS 连接。对齐 TS ws.ts 的 Redis
// 桥(载荷不重打包,转发原始字节;无 companyId 的事件拒绝路由)。
// doc.update/doc.awareness 是房间域,由 docrelay 管,不经此桥。
package wsx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
)

// 出站背压两道界(#236)。主界按字节累计,对齐 TS ws.ts 的 bufferedAmount
// 两档预算(WS_MAX_BUFFERED_BYTES=2MB 超限丢帧、
// WS_TERMINATE_BUFFERED_BYTES=8MB 超限掐线):TS 直接测 socket 未冲刷
// 字节;coder/websocket 的 Write 同步落笔,进程侧积压全部体现为出站
// 队列里的帧,用"队列内帧 + 写协程在途帧"的字节累计等价表达。旧纯
// 条数界对 doc 帧失真——单条可达 ~5.3MB(4MB Yjs update 的 base64+
// JSON 包装),慢连接最坏积压 256×5.3MB≈GB 级;字节界把每连接最坏积
// 压压到 8MB 有界。条数深度帽保留为第二道界,兜海量小帧。
const (
	// chatOutboundBudget:聊天帧档(TS 2MB 丢帧档)。聊天帧可丢(客户端
	// 靠重连/重放补齐),超限即丢帧、连接保活——聊天帧小而密,2MB 积压
	// 已说明对端远落后。
	chatOutboundBudget = 2 * 1024 * 1024
	// docOutboundBudget:doc 帧档(TS 8MB 掐线档)。doc.update 是 yjs
	// 增量,没有静默丢帧的安全路径——预算抬到 8MB(正常协同单帧
	// ~5.3MB 仍可入队),超限即掐线逼全量重同步。
	docOutboundBudget = 8 * 1024 * 1024
	// outboundCap:条数深度帽(第二道界),字节界之上的廉价保险。
	outboundCap = 256
)

/* ───────── hub:已鉴权连接注册表 ───────── */

type hub struct {
	mu sync.RWMutex
	m  map[*conn]struct{}
}

func (h *hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[*conn]struct{}{}
	}
	h.m[c] = struct{}{}
}

func (h *hub) remove(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.m, c)
}

func (h *hub) each(fn func(*conn)) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.m {
		fn(c)
	}
}

/* ───────── Redis → WS 桥 ───────── */

// bootBridge 常驻订阅公司域通道;断线由 go-redis PubSub 内部重连 +
// pump 外层 2s 退避。rdb 为 nil(redis 不可达降级)时聊天推送整体停用,
// doc 面与 HTTP 域照常。
func (g *Gateway) bootBridge(ctx context.Context) {
	if g.rdb == nil {
		slog.Warn("wsx chat bridge off — redis unreachable; chat push unavailable this run")
		return
	}
	go func() {
		for {
			if err := g.pump(ctx, g.rdb); err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
		}
	}()
}

func (g *Gateway) pump(ctx context.Context, rdb *redis.Client) error {
	sub := rdb.Subscribe(ctx, events.CompanyChannels...)
	defer sub.Close()
	slog.Info("wsx chat bridge subscribed", "channels", len(events.CompanyChannels))
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("redis pubsub closed")
			}
			g.fanout([]byte(msg.Payload))
		}
	}
}

// fanout:解析载荷头部的 companyId,按成员资格投递;无租户标记的事件
// 一律不路由(TS 同款保守姿态——发布方漏打标是 bug,告警暴露)。
func (g *Gateway) fanout(payload []byte) {
	var head struct {
		CompanyID string `json:"companyId"`
	}
	if err := json.Unmarshal(payload, &head); err != nil {
		slog.Warn("wsx bridge dropping malformed event", "err", err)
		return
	}
	if head.CompanyID == "" {
		slog.Warn("wsx bridge dropping untagged event")
		return
	}
	g.hub.each(func(c *conn) {
		if _, ok := c.companies[head.CompanyID]; !ok {
			return
		}
		c.enqueueChat(payload)
	})
}

/* ───────── 逐连接出站:写协程 + 背压 ───────── */

// startWriter:聊天帧的唯一写出方(与 c.send 共用 c.mu 串行化)。
// 写失败(对端死/超时)即自拆:取消自身 + 掐 ws → readLoop 的 Read
// 随之失败并走统一拆链。
func (c *conn) startWriter() {
	var ctx context.Context
	ctx, c.wcancel = context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case raw := <-c.outbound:
				ok := c.writeRaw(raw)
				// #236:落笔(无论成败)即归还字节预算——帧已离开
				// 进程缓冲,不得继续占用后续帧的额度。失败路径连接
				// 已死,归还只为计数守恒。
				c.releaseOutbound(len(raw))
				if !ok {
					c.wcancel()
					_ = c.ws.Close(websocket.StatusInternalError, "write stalled")
					return
				}
			}
		}
	}()
}

func (c *conn) writeRaw(raw []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, raw) == nil
}

// reserveOutbound:字节预算的检查+占位,一步原子(#236)。true = 该帧
// 允许入队,其字节数已计入累计;false = 超出该档预算(帧未计入),由
// 调用方按档分流:聊天帧丢帧、doc 帧掐线。占位先于信道投递——保证
// 写协程能取到的每一帧都已被计数(happens-before:同协程先占位后投
// 递),计数只可能短暂高计(占位与投递之间),永不低计,预算判断恒
// 保守在安全侧。
func (c *conn) reserveOutbound(n int, budget int) bool {
	c.outmu.Lock()
	defer c.outmu.Unlock()
	if c.outBytes+n > budget {
		return false
	}
	c.outBytes += n
	return true
}

// releaseOutbound:归还帧占用的字节额度——写协程落笔后(帧真正离开
// 进程,TS bufferedAmount 冲刷即减的同义)或投递失败回滚时调用。归
// 还的正是该帧自己占位的份额:写协程归还的帧必然来自信道(必先占位
// 过),回滚归还的帧必然没进信道(写协程无从取出)——两条路径的份
// 额互不相交,不会双扣。
func (c *conn) releaseOutbound(n int) {
	c.outmu.Lock()
	c.outBytes -= n
	c.outmu.Unlock()
}

// enqueueChat 非阻塞投递:超预算(字节累计超 2MB 档,或深度帽满)=
// 该连接严重落后,丢帧(告警一次);对端最终由写超时/心跳判定死亡并
// 重连,靠 hello 重引导补齐。
func (c *conn) enqueueChat(raw []byte) {
	if c.reserveOutbound(len(raw), chatOutboundBudget) {
		select {
		case c.outbound <- raw:
			return
		default:
			c.releaseOutbound(len(raw)) // 深度帽兜底:字节占位回滚
		}
	}
	if atomic.CompareAndSwapUint32(&c.dropAnnounced, 0, 1) {
		slog.Warn("ws client behind — dropping chat frames until it drains or reconnects", "user", c.userID)
	}
}

// enqueueDoc:doc 帧与聊天帧同走有界出站队列(#216),字节档抬到 8MB
// (#236)。与聊天帧的丢帧语义不同:doc.update 是 yjs 增量,静默丢失
// 会让该客户端的状态悄然分歧(hello 重同步不覆盖协同帧)——所以超
// 预算(或深度帽满)时**直接掐线**,让客户端重连重订阅、从 sidecar
// 重取全量 state,这是唯一安全路径。
// 掐线必须真非阻塞:CloseNow 只关 rwc(纯本地,无锁争用无网络等待);
// **不能用 Close()**——它会 5s 写 close 帧 + 5s 等对端握手,且与写
// 协程停滞中的 Write 抢同一把 writeFrameMu,恰好撞上"客户端停滞"
// 这个触发场景,fanout 协程还是会被拖住(评审 P1)。拆链由 readLoop
// 的 defer 统一完成。告警用独立标志:聊天丢帧的 dropAnnounced 先置位
// 时不能吞掉掐线这条更严重的日志。
func (c *conn) enqueueDoc(payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if c.reserveOutbound(len(raw), docOutboundBudget) {
		select {
		case c.outbound <- raw:
			return
		default:
			c.releaseOutbound(len(raw)) // 深度帽兜底:字节占位回滚
		}
	}
	if atomic.CompareAndSwapUint32(&c.docClosed, 0, 1) {
		slog.Warn("ws doc consumer behind — closing connection for full resync", "user", c.userID)
	}
	if c.wcancel != nil {
		c.wcancel()
	}
	_ = c.ws.CloseNow()
}
