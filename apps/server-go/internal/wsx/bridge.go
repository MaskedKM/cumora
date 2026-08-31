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

// outboundCap:聊天帧每连接的排队上限。TS 按 bufferedAmount 字节预算
// (2MB 丢帧 / 8MB 掐线);coder/websocket 的 Write 自带流控+超时,内存
// 风险集中在"慢客户端堵住共享扇出循环",这里用有界队列等价表达:满则
// 丢帧让该连接自己落后,写超时最终掐线触发重连重同步。
const outboundCap = 256

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
				if !c.writeRaw(raw) {
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

// enqueueChat 非阻塞投递:队列满 = 该连接严重落后,丢帧(告警一次);
// 对端最终由写超时/心跳判定死亡并重连,靠 hello 重引导补齐。
func (c *conn) enqueueChat(raw []byte) {
	select {
	case c.outbound <- raw:
	default:
		if atomic.CompareAndSwapUint32(&c.dropAnnounced, 0, 1) {
			slog.Warn("ws client behind — dropping chat frames until it drains or reconnects", "user", c.userID)
		}
	}
}

// enqueueDoc:doc 帧与聊天帧同走有界出站队列(#216)。与聊天帧的丢帧
// 语义不同:doc.update 是 yjs 增量,静默丢失会让该客户端的状态悄然
// 分歧(hello 重同步不覆盖协同帧)——所以队列满时**直接掐线**,让
// 客户端重连重订阅、从 sidecar 重取全量 state,这是唯一安全路径。
// 掐线在 fanout 协程上是非阻塞的(cancel+Close 立即返回,拆链由
// readLoop 的 defer 统一完成)。
func (c *conn) enqueueDoc(payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	select {
	case c.outbound <- raw:
	default:
		if atomic.CompareAndSwapUint32(&c.dropAnnounced, 0, 1) {
			slog.Warn("ws doc consumer behind — closing connection for full resync", "user", c.userID)
		}
		if c.wcancel != nil {
			c.wcancel()
		}
		_ = c.ws.Close(websocket.StatusGoingAway, "doc consumer too slow")
	}
}
