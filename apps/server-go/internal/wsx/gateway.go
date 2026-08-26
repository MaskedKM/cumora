// wsx —— WebSocket 网关(#55 起步):/ws 升级(一次性 ws-ticket 鉴权,
// 对齐 TS ws.ts 的 ?t= 票据面)+ 文档协同帧(doc.*)。消息面帧
// (message/typing/presence)归 #60 运行时服务面,当前静默忽略——
// 客户端连上后不受影响,只是收不到会话事件。
package wsx

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/docrelay"
	"github.com/coder/websocket"
)

const maxFrameBytes = 4 * 1024 * 1024 // 对齐 TS WebSocketServer maxPayload

type conn struct {
	ws       *websocket.Conn
	userID   string
	originID string
	mu       sync.Mutex // 帧写入串行化
	// 该连接上的文档订阅;关闭时全部归还(防房间泄漏)。
	docSubs map[string]*docrelay.Subscriber
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

type Gateway struct {
	db    *sql.DB
	relay *docrelay.Relay
}

func Mount(mux *http.ServeMux, db *sql.DB, relay *docrelay.Relay) {
	g := &Gateway{db: db, relay: relay}
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

func (g *Gateway) handle(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("t")
	if ticket == "" {
		httpxWriteError(w, http.StatusUnauthorized, "ticket required")
		return
	}
	userID, ok := g.consumeWsTicket(r.Context(), ticket)
	if !ok {
		httpxWriteError(w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// TS ws 服务器不做 origin 校验(浏览器同站由部署层保证)。
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	c := &conn{ws: ws, userID: userID, originID: "ws-" + authn.NewToken()[:12], docSubs: map[string]*docrelay.Subscriber{}}
	go g.readLoop(c)
}

func (g *Gateway) readLoop(c *conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 关闭时归还全部文档订阅。
	defer func() {
		for docID, s := range c.docSubs {
			g.relay.Unsubscribe(docID, s)
		}
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
		if !strings_hasPrefixDoc(typ) {
			continue // 非 doc 帧:消息面归 #60,静默忽略
		}
		if err := g.handleDocFrame(ctx, c, msg); err != nil {
			// 对齐 TS:doc.* 帧的服务端错误统一 doc.error 'server error'
			docID, _ := msg["documentId"].(string)
			c.send(map[string]any{"type": "doc.error", "documentId": docID, "error": "server error"})
		}
	}
}

func strings_hasPrefixDoc(typ string) bool { return len(typ) >= 4 && typ[:4] == "doc." }

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
			OnUpdate: func(update []byte, originID string) {
				c.send(map[string]any{
					"type": "doc.update", "documentId": documentID,
					"updateB64": base64.StdEncoding.EncodeToString(update), "originId": originID,
				})
			},
			OnAwareness: func(update []byte, originID string) {
				c.send(map[string]any{
					"type": "doc.awareness", "documentId": documentID,
					"updateB64": base64.StdEncoding.EncodeToString(update), "originId": originID,
				})
			},
		}
		initial, err := g.relay.Subscribe(ctx, documentID, companyID, s)
		if err != nil {
			return err // 上层发 doc.error(登记已回滚)
		}
		c.docSubs[documentID] = s
		c.send(map[string]any{
			"type": "doc.sync", "documentId": documentID,
			"stateB64": base64.StdEncoding.EncodeToString(initial), "originId": c.originID,
		})
		return nil

	case "doc.unsubscribe":
		if s, ok := c.docSubs[documentID]; ok {
			g.relay.Unsubscribe(documentID, s)
			delete(c.docSubs, documentID)
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
		companyID, ok := g.docCompanyFor(ctx, documentID, c.userID)
		if !ok {
			return nil
		}
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
		companyID, ok := g.docCompanyFor(ctx, documentID, c.userID)
		if !ok {
			return nil
		}
		update, err := base64.StdEncoding.DecodeString(updateB64)
		if err != nil {
			return nil
		}
		return g.relay.BroadcastAwareness(ctx, documentID, companyID, c.originID, update)

	case "doc.mention.notify":
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
		companyID, ok := g.docCompanyFor(ctx, documentID, c.userID)
		if !ok {
			return nil
		}
		return g.processDocMention(ctx, documentID, companyID, c.userID, requested)
	}
	return nil
}

func httpxWriteError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
