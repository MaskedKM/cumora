// docrelay —— Go server 侧文档 relay(#55 · ADR 0004),对端为
// server/src/documents/relay.ts。职责:①持有本实例 WS 订阅表,把
// sidecar 经 Redis CH_DOC_UPDATE/CH_DOC_AWARENESS 扇出的事件按
// originId 回声抑制后推给各 WS 订阅者;②把客户端的房间操作经
// sidecar 内表面 HTTP 转发(协议契约见 apps/yjs-sidecar/src/http.ts)。
// relay 不缓存任何 Y.Doc 状态——sidecar 无状态、冷加载自 pg。
package docrelay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/redis/go-redis/v9"
)

// Subscriber 对齐 DocSubscriber:originId 用于回声抑制,回调把
// 二进制 update 交回 WS 网关封帧。
type Subscriber struct {
	OriginID    string
	OnUpdate    func(update []byte, originID string)
	OnAwareness func(update []byte, originID string)
}

type Relay struct {
	sidecarURL string
	token      string
	timeout    time.Duration
	origin     string // instance:<INSTANCE_ID>,sidecar 订阅的 subscriberId

	httpClient *http.Client

	// off = Redis 不可达的降级态:订阅必须快败(否则客户端拿到永远收
	// 不到扇出的半开协同会话——TS 无此分裂态)。
	off bool

	mu        sync.Mutex
	subs      map[string]map[*Subscriber]struct{} // documentId → 订阅集合
	refcounts map[string]int
}

func New(sidecarURL, token string, timeoutMS int, instanceID string) *Relay {
	return &Relay{
		sidecarURL: sidecarURL,
		token:      token,
		timeout:    time.Duration(timeoutMS) * time.Millisecond,
		origin:     "instance:" + instanceID,
		httpClient: &http.Client{},
		subs:       map[string]map[*Subscriber]struct{}{},
		refcounts:  map[string]int{},
	}
}

// Boot 订阅 sidecar 的两条扇出通道;断线由 go-redis PubSub 内部重连,
// 通道随 Receive 循环恢复。进程生命周期内常驻。rdb 为 nil(redis 不可达
// 降级)时标记 off——协同面整体快败,HTTP 域照常。
func (r *Relay) Boot(ctx context.Context, rdb *redis.Client) {
	if rdb == nil {
		r.off = true
		slog.Warn("docrelay off — redis unreachable; doc collab unavailable this run")
		return
	}
	if r.token == "" {
		slog.Warn("YJS_SIDECAR_TOKEN 未配置 —— 文档协同将在运行期 401(设 token 或停用文档功能)")
	}
	go func() {
		for {
			if err := r.pump(ctx, rdb); err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
		}
	}()
}

func (r *Relay) pump(ctx context.Context, rdb *redis.Client) error {
	sub := rdb.Subscribe(ctx, events.ChDocUpdate, events.ChDocAware)
	defer sub.Close()
	slog.Info("docrelay subscribed", "channels", []string{events.ChDocUpdate, events.ChDocAware}, "sidecar", r.sidecarURL)
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("redis pubsub closed")
			}
			r.fanout(msg.Channel, []byte(msg.Payload))
		}
	}
}

type sidecarDocEvent struct {
	DocumentID string `json:"documentId"`
	OriginID   string `json:"originId"`
	UpdateB64  string `json:"updateB64"`
}

func (r *Relay) fanout(channel string, payload []byte) {
	var ev sidecarDocEvent
	if err := json.Unmarshal(payload, &ev); err != nil || ev.DocumentID == "" {
		return
	}
	update, err := base64.StdEncoding.DecodeString(ev.UpdateB64)
	if err != nil {
		return
	}
	r.mu.Lock()
	subs := make([]*Subscriber, 0, len(r.subs[ev.DocumentID]))
	for s := range r.subs[ev.DocumentID] {
		subs = append(subs, s)
	}
	r.mu.Unlock()
	for _, s := range subs {
		if s.OriginID == ev.OriginID {
			continue // 发起者回声抑制
		}
		// 单个死订阅不得打断扇出
		if channel == events.ChDocUpdate && s.OnUpdate != nil {
			s.OnUpdate(update, ev.OriginID)
		} else if channel == events.ChDocAware && s.OnAwareness != nil {
			s.OnAwareness(update, ev.OriginID)
		}
	}
}

// sidecar 内表面 HTTP POST(Bearer token + 超时)。
func (r *Relay) call(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, r.sidecarURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+r.token)
	res, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return fmt.Errorf("sidecar %s → %d %s", path, res.StatusCode, text)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(out)
}

// Subscribe 登记本地 WS 订阅并向 sidecar 取全量初始状态(一次调用同时
// 完成登记与取态——sidecar 侧按 (doc, subscriberId) 幂等)。侧失败回滚
// 本地登记后向上抛,WS 网关给客户端发 doc.error。
func (r *Relay) Subscribe(ctx context.Context, documentID, companyID string, s *Subscriber) ([]byte, error) {
	if r.off {
		return nil, fmt.Errorf("docrelay off (redis unreachable)")
	}
	r.mu.Lock()
	set := r.subs[documentID]
	if set == nil {
		set = map[*Subscriber]struct{}{}
		r.subs[documentID] = set
	}
	set[s] = struct{}{}
	r.refcounts[documentID]++
	r.mu.Unlock()

	var out struct {
		StateB64 string `json:"stateB64"`
	}
	if err := r.call(ctx, "/internal/doc/subscribe", map[string]string{
		"documentId": documentID, "companyId": companyID, "subscriberId": r.origin,
	}, &out); err != nil {
		r.mu.Lock()
		delete(set, s)
		if len(set) == 0 {
			delete(r.subs, documentID)
		}
		r.refcounts[documentID]--
		if r.refcounts[documentID] <= 0 {
			delete(r.refcounts, documentID)
		}
		r.mu.Unlock()
		return nil, err
	}
	state, err := base64.StdEncoding.DecodeString(out.StateB64)
	if err != nil {
		return nil, err
	}
	return state, nil
}

// Unsubscribe 归还引用;归零时注销 sidecar 订阅(失败仅告警——
// sidecar 无状态,残留订阅随其重启自然回收)。
func (r *Relay) Unsubscribe(documentID string, s *Subscriber) {
	r.mu.Lock()
	set := r.subs[documentID]
	if set == nil {
		r.mu.Unlock()
		return
	}
	if _, ok := set[s]; !ok {
		r.mu.Unlock()
		return
	}
	delete(set, s)
	r.refcounts[documentID]--
	left := r.refcounts[documentID]
	if left <= 0 {
		delete(r.subs, documentID)
		delete(r.refcounts, documentID)
	}
	r.mu.Unlock()
	if left <= 0 {
		if err := r.call(context.Background(), "/internal/doc/unsubscribe", map[string]string{
			"documentId": documentID, "subscriberId": r.origin,
		}, nil); err != nil {
			slog.Warn("sidecar unsubscribe failed", "documentId", documentID, "err", err)
		}
	}
}

// ApplyLocalUpdate 把一个客户端 origin 的 Yjs 增量交给 sidecar
// (应用→持久化→Redis 扇出;回声经 originId 抑制)。
func (r *Relay) ApplyLocalUpdate(ctx context.Context, documentID, companyID, originID, userID string, update []byte) error {
	return r.call(ctx, "/internal/doc/update", map[string]string{
		"documentId": documentID, "companyId": companyID, "originId": originID,
		"userId": userID, "updateB64": base64.StdEncoding.EncodeToString(update),
	}, nil)
}

// BroadcastAwareness 仅扇出 awareness(不落库)。
func (r *Relay) BroadcastAwareness(ctx context.Context, documentID, companyID, originID string, update []byte) error {
	return r.call(ctx, "/internal/doc/awareness", map[string]string{
		"documentId": documentID, "companyId": companyID, "originId": originID,
		"updateB64": base64.StdEncoding.EncodeToString(update),
	}, nil)
}

// ReadDocumentText 读文档当前文本快照(agent CLI 面)。
func (r *Relay) ReadDocumentText(ctx context.Context, documentID, companyID string) (string, error) {
	var out struct {
		Text string `json:"text"`
	}
	if err := r.call(ctx, "/internal/doc/read-text", map[string]string{
		"documentId": documentID, "companyId": companyID,
	}, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}

// AgentEditResult 对齐 relay.AgentEditResult(imagePlaced 是
// 'absolute'|'anchor'|'anchor-missed'|null —— 指针保 null 语义)。
type AgentEditResult struct {
	Replaced       int     `json:"replaced"`
	ImagePlaced    *string `json:"imagePlaced"`
	ImagesDeleted  int     `json:"imagesDeleted"`
	BlocksReplaced int     `json:"blocksReplaced"`
}

// ApplyAgentEdit 结构化 agent 编辑(agent CLI 面)。
func (r *Relay) ApplyAgentEdit(ctx context.Context, documentID, companyID, agentID string, ops []map[string]any) (AgentEditResult, error) {
	var out AgentEditResult
	err := r.call(ctx, "/internal/doc/agent-edit", map[string]any{
		"documentId": documentID, "companyId": companyID, "agentId": agentID, "ops": ops,
	}, &out)
	return out, err
}
