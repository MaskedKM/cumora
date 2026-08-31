// wsx —— WebSocket 网关:/ws 升级(一次性 ws-ticket 鉴权,对齐 TS
// ws.ts 的 ?t= 票据面)+ 文档协同帧(doc.*)+ 聊天推送面(#202:公司域
// Redis 事件按租户成员资格转发,见 bridge.go)+ hello 握手帧(#198,
// 客户端以收到 hello = 连接(重连)完成,据此重建 doc 订阅)+ 人在场
// 翻转(presence.go)。
package wsx

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
)

const maxFrameBytes = 4 * 1024 * 1024 // 对齐 TS WebSocketServer maxPayload

type conn struct {
	ws       *websocket.Conn
	userID   string
	originID string
	mu       sync.Mutex // 帧写入串行化
	// 该连接上的文档订阅;关闭时全部归还(防房间泄漏)。
	docSubs map[string]*docSub
	// documentId → company_id,随 doc.subscribe 的成员资格校验一并解析
	// 缓存(#216):doc.update/awareness/mention 高频帧不再逐帧打库。
	// 成员资格语义与聊天面的 companies 握手快照一致——连接期内不刷新,
	// 变更由新连接带入。
	companies    map[string]struct{}
	docCompanies map[string]string
	// 聊天帧出站队列(#202):桥/写协程解耦,慢客户端只堵自己;doc 帧
	// (#216)同走此队列——relay 扇出协程不被任何订阅者的慢写阻塞。
	outbound chan []byte
	// 出站字节累计(#236):outbound 内帧 + 写协程在途帧的 len 之和,
	// 是背压的第一道界(对齐 TS bufferedAmount 预算,见 bridge.go 两档
	// 常量)。守恒规则:入队前占位、写出后归还、投递失败回滚,全部在
	// outmu 下原子进行。必须独立于 c.mu——入队发生在扇出协程上,若与
	// 写协程的慢写(持 c.mu 最长 10s)共锁,#216 解掉的"慢客户端拖住
	// 扇出"会从预算路径回流。
	outmu         sync.Mutex
	outBytes      int
	dropAnnounced uint32 // 背压丢帧只告警一次(atomic)
	docClosed     uint32 // doc 帧掐线只告警一次(独立于丢帧,atomic)
	wcancel       context.CancelFunc
}

// docSub:一条文档订阅的本地登记(relay Subscriber + 订阅期解析的
// companyID 缓存,#216)。
type docSub struct {
	sub       *docrelay.Subscriber
	companyID string
}

func (c *conn) send(payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = c.ws.Write(ctx, websocket.MessageText, raw)
}

// PresenceSetter:在场翻转的落库+广播面(runtime.SetStatus)。
type PresenceSetter interface {
	SetStatus(ctx context.Context, participantID, status string) error
}

type Gateway struct {
	db         *sql.DB
	relay      *docrelay.Relay
	rdb        *redis.Client // nil → 聊天桥停用(协同面同款降级)
	instanceID string
	hub        hub
	presence   PresenceSetter // nil → 不做在场翻转
	humans     humanCounters
}

func Mount(mux *http.ServeMux, db *sql.DB, relay *docrelay.Relay, rdb *redis.Client, instanceID string, presence PresenceSetter) {
	g := &Gateway{db: db, relay: relay, rdb: rdb, instanceID: instanceID, presence: presence}
	g.bootBridge(context.Background())
	mux.HandleFunc("GET /ws", g.handle)
}

// consumeWsTicket 原子消费一次性票据(对齐 auth.consumeWsTicket:
// UPDATE…WHERE used_at IS NULL AND expires_at > NOW())。
func (g *Gateway) consumeWsTicket(ctx context.Context, ticket string) (string, bool) {
	var userID string
	err := g.db.QueryRowContext(ctx, `
		UPDATE ws_tickets SET used_at = NOW()
		 WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW()
		 RETURNING user_id`, authn.HashToken(ticket)).Scan(&userID)
	if err != nil {
		return "", false
	}
	return userID, true
}

// docCompanyFor 对齐 ws.ts:文档不存在与无租户成员资格同一姿态(不泄存在性)。
func (g *Gateway) docCompanyFor(ctx context.Context, documentID, userID string) (string, bool) {
	var companyID string
	err := g.db.QueryRowContext(ctx, `
		SELECT d.company_id FROM documents d
		 JOIN company_members m ON m.company_id = d.company_id AND m.user_id = $2
		WHERE d.id = $1 LIMIT 1`, documentID, userID).Scan(&companyID)
	if err != nil {
		return "", false
	}
	return companyID, true
}

// loadMemberships 对齐 TS loadMemberships:连接时刻快照,连接期内不
// 刷新(成员变更由新连接带入)。查询失败按空集处理——聊天面默认拒绝
// 路由(deny-by-default),doc 面照旧走 per-doc 校验,不受影响。
func (g *Gateway) loadMemberships(userID string) map[string]struct{} {
	out := map[string]struct{}{}
	rows, err := g.db.Query(`SELECT company_id FROM company_members WHERE user_id = $1`, userID)
	if err != nil {
		slog.Warn("ws loadMemberships failed — chat push denied for this connection", "user", userID, "err", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var companyID string
		if err := rows.Scan(&companyID); err != nil {
			continue
		}
		out[companyID] = struct{}{}
	}
	return out
}

// helloFrame 对齐 TS ws.ts:握手完成即发,重连(新连接替换旧连接)
// 同样发。客户端(WsClient/yjsClient 及各 store)以收到 hello = 连接
// (重连)完成,据此重放 doc.subscribe、冲刷断线攒批、重引数据。
func helloFrame(instanceID string) map[string]any {
	return map[string]any{"type": "hello", "instanceId": instanceID, "ts": time.Now().UnixMilli()}
}

func (g *Gateway) handle(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("t")
	if ticket == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "ticket required")
		return
	}
	// 先升级再消费票据,对齐 TS(在 connection 回调里校验):升级被中间层
	// 掐掉的合法客户端不烧掉它的一次性票据。
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// TS ws 服务器不做 origin 校验(浏览器同站由部署层保证)。
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	// 库默认读上限只有 32KB;TS WebSocketServer maxPayload=4MB。不调
	// SetReadLimit,大文档的初态同步/大粘贴会被 1009 断连。
	ws.SetReadLimit(maxFrameBytes)
	userID, ok := g.consumeWsTicket(r.Context(), ticket)
	if !ok {
		_ = ws.Close(websocket.StatusPolicyViolation, "invalid or expired ticket")
		return
	}
	c := &conn{
		ws: ws, userID: userID, originID: "ws-" + authn.NewToken()[:12],
		docSubs: map[string]*docSub{}, companies: g.loadMemberships(userID), docCompanies: map[string]string{},
		outbound: make(chan []byte, outboundCap),
	}
	// hello 必须先于注册/写协程发出(TS 单线程下的既成不变量):保证
	// 它是该连接的首帧,客户端"收到 hello = 连接完成"的语义不被抢先的
	// 聊天帧打破。c.send 在 mu 下完成写,后续 writer 的帧只能排其后。
	// 拆链在 readLoop 的 defer 单点完成(hub/在场/doc 订阅/写协程)。
	c.send(helloFrame(g.instanceID))
	g.hub.add(c)
	g.humanConnect(c.userID)
	c.startWriter()
	go g.readLoop(c)
}

func (g *Gateway) readLoop(c *conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 关闭时拆链:归还全部文档订阅、注销连接、在场 1→0、停写协程。
	defer func() {
		g.hub.remove(c)
		g.humanDisconnect(c.userID)
		for docID, ds := range c.docSubs {
			g.relay.Unsubscribe(docID, ds.sub)
		}
		c.wcancel()
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
	}()
	// 心跳:TS 的 ping/isAlive 半开检测由库内 ping 承担。
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				if err := c.ws.Ping(pctx); err != nil {
					pcancel()
					cancel()
					return
				}
				pcancel()
			}
		}
	}()
	for {
		mt, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageText {
			continue
		}
		if len(data) > maxFrameBytes {
			// 超限帧:断开(对齐 ws 库 maxPayload 拒绝语义)。
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		typ, _ := msg["type"].(string)
		if !hasDocPrefix(typ) {
			continue // 非 doc 帧:消息面归 #60,静默忽略
		}
		if err := g.handleDocFrame(ctx, c, msg); err != nil {
			// 对齐 TS:doc.* 帧的服务端错误统一 doc.error 'server error'
			docID, _ := msg["documentId"].(string)
			c.send(map[string]any{"type": "doc.error", "documentId": docID, "error": "server error"})
		}
	}
}

func hasDocPrefix(typ string) bool { return len(typ) >= 4 && typ[:4] == "doc." }

func (g *Gateway) handleDocFrame(ctx context.Context, c *conn, msg map[string]any) error {
	typ, _ := msg["type"].(string)
	documentID, _ := msg["documentId"].(string)
	if documentID == "" {
		return nil
	}
	switch typ {
	case "doc.subscribe":
		if _, exists := c.docSubs[documentID]; exists {
			return nil // 幂等
		}
		companyID, ok := g.docCompanyFor(ctx, documentID, c.userID)
		if !ok {
			c.send(map[string]any{"type": "doc.error", "documentId": documentID, "error": "not found"})
			return nil
		}
		s := &docrelay.Subscriber{
			OriginID: c.originID,
			// #216:回调在 relay 的共享扇出协程上执行,绝不能同步写 WS
			// (原 c.send 持锁阻塞写,一个停滞客户端每帧最多拖住全实例
			// 的 doc 扇出 10s)——改投每连接有界出站队列,由写协程落笔。
			OnUpdate: func(update []byte, originID string) {
				c.enqueueDoc(map[string]any{
					"type": "doc.update", "documentId": documentID,
					"updateB64": base64.StdEncoding.EncodeToString(update), "originId": originID,
				})
			},
			OnAwareness: func(update []byte, originID string) {
				c.enqueueDoc(map[string]any{
					"type": "doc.awareness", "documentId": documentID,
					"updateB64": base64.StdEncoding.EncodeToString(update), "originId": originID,
				})
			},
		}
		initial, err := g.relay.Subscribe(ctx, documentID, companyID, s)
		if err != nil {
			return err // 上层发 doc.error(登记已回滚)
		}
		c.docSubs[documentID] = &docSub{sub: s, companyID: companyID}
		c.docCompanies[documentID] = companyID
		c.send(map[string]any{
			"type": "doc.sync", "documentId": documentID,
			"stateB64": base64.StdEncoding.EncodeToString(initial), "originId": c.originID,
		})
		return nil

	case "doc.unsubscribe":
		if ds, ok := c.docSubs[documentID]; ok {
			g.relay.Unsubscribe(documentID, ds.sub)
			delete(c.docSubs, documentID)
			delete(c.docCompanies, documentID)
		}
		return nil

	case "doc.update":
		if _, ok := c.docSubs[documentID]; !ok {
			return nil // 必须先订阅
		}
		updateB64, _ := msg["updateB64"].(string)
		if updateB64 == "" {
			return nil
		}
		// #216:companyID 用订阅期缓存(docCompanyFor 已在 subscribe 时
		// 校验过成员资格;每击键一批就查一次库的历史在此终结)。
		companyID := c.docCompanies[documentID]
		update, err := base64.StdEncoding.DecodeString(updateB64)
		if err != nil {
			return nil
		}
		return g.relay.ApplyLocalUpdate(ctx, documentID, companyID, c.originID, c.userID, update)

	case "doc.awareness":
		if _, ok := c.docSubs[documentID]; !ok {
			return nil
		}
		updateB64, _ := msg["updateB64"].(string)
		if updateB64 == "" {
			return nil
		}
		companyID := c.docCompanies[documentID] // #216:订阅期缓存
		update, err := base64.StdEncoding.DecodeString(updateB64)
		if err != nil {
			return nil
		}
		return g.relay.BroadcastAwareness(ctx, documentID, companyID, c.originID, update)

	case "doc.mention.notify":
		if _, ok := c.docSubs[documentID]; !ok {
			return nil // 必须先订阅(对齐 update/awareness;唯一发送方仅在 synced 后触发)
		}
		raw, _ := msg["mentionedIds"].([]any)
		requested := make([]string, 0, len(raw))
		for _, v := range raw {
			if id, ok := v.(string); ok {
				requested = append(requested, id)
			}
		}
		if len(requested) == 0 {
			return nil
		}
		companyID := c.docCompanies[documentID] // #216:订阅期缓存
		return g.processDocMention(ctx, documentID, companyID, c.userID, requested)
	}
	return nil
}
