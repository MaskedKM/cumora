// push/fanout —— 触达扇出:sendToUsers(双平台)、notifyMessage(消息
// 推送载荷)、computeMessageRecipients(成员−作者−静音−在线)。对齐
// 已退役 TS server 的 push.ts 下半部。
package push

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/push"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func disableDeviceByID(ctx context.Context, db *sql.DB, id string) {
	_, _ = db.ExecContext(ctx, `UPDATE push_devices SET disabled_at = NOW() WHERE id = $1`, id)
}

func touchDeviceByID(ctx context.Context, db *sql.DB, id string) {
	_, _ = db.ExecContext(ctx, `UPDATE push_devices SET last_seen_at = NOW() WHERE id = $1`, id)
}

// SendToUsers:目标用户的全部活跃 ios/android 设备;凭据缺失的平台静默
// 跳过;死令牌软禁用;返回成功投递数。
func SendToUsers(ctx context.Context, db *sql.DB, userIDs []string, payload Payload) int {
	if len(userIDs) == 0 {
		return 0
	}
	apnsOn := apnsOn()
	fcmOn := fcmOn()
	if !apnsOn && !fcmOn {
		return 0
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, user_id, token, platform FROM push_devices
		 WHERE user_id = ANY($1::text[]) AND platform IN ('ios','android') AND disabled_at IS NULL`,
		userIDs)
	if err != nil {
		return 0
	}
	type device struct {
		id, userID, token, platform string
	}
	devices := []device{}
	for rows.Next() {
		var d device
		if rows.Scan(&d.id, &d.userID, &d.token, &d.platform) == nil {
			devices = append(devices, d)
		}
	}
	rows.Close()
	var delivered atomic.Int64
	done := make(chan struct{}, len(devices))
	for _, d := range devices {
		go func(d device) {
			defer func() { done <- struct{}{} }()
			switch d.platform {
			case "android":
				if !fcmOn {
					return
				}
				res := sendOneFcm(d.token, payload)
				if res.ok {
					delivered.Add(1)
					touchDeviceByID(ctx, db, d.id)
					return
				}
				if res.dead {
					disableDeviceByID(ctx, db, d.id)
					return
				}
			default: // ios
				if !apnsOn {
					return
				}
				res := sendOneApns(d.token, payload)
				if res.ok {
					delivered.Add(1)
					touchDeviceByID(ctx, db, d.id)
					return
				}
				if res.status == 410 || deadApnsReasons[res.reason] {
					disableDeviceByID(ctx, db, d.id)
					return
				}
			}
		}(d)
	}
	for range devices {
		<-done
	}
	return int(delivered.Load())
}

// trimBody:空白折叠后 240 字符封顶(超出加省略号,237+…)。
func trimBody(body string) string {
	trimmed := strings.Join(strings.Fields(body), " ")
	if utf16Len(trimmed) <= 240 {
		return trimmed
	}
	return utf16Slice(trimmed, 237) + "…"
}

// NotifyMessage:新消息推送(标题 = 作者 · 会话名;threadId 折叠栈)。
func NotifyMessage(ctx context.Context, db *sql.DB, args struct {
	ConversationID    string
	ConversationTitle *string
	AuthorID          string
	AuthorName        string
	MessageID         string
	Body              string
	CompanyID         string
	RecipientUserIDs  []string
}) {
	if len(args.RecipientUserIDs) == 0 {
		return
	}
	title := args.AuthorName
	if args.ConversationTitle != nil && *args.ConversationTitle != "" {
		title = args.AuthorName + " · " + *args.ConversationTitle
	}
	body := trimBody(args.Body)
	if body == "" {
		body = "(empty)"
	}
	SendToUsers(ctx, db, args.RecipientUserIDs, Payload{
		Title: title, Body: body, ThreadID: args.ConversationID,
		Data: map[string]any{
			"conversationId": args.ConversationID,
			"messageId":      args.MessageID,
			"companyId":      args.CompanyID,
			"kind":           "message",
		},
	})
}

// ComputeMessageRecipients:会话成员 − 作者 − 静音 − 在线(avail)。
func ComputeMessageRecipients(ctx context.Context, db *sql.DB, conversationID, authorID string) []string {
	rows, err := db.QueryContext(ctx, `
		WITH convo AS (SELECT members FROM conversations WHERE id = $1)
		SELECT u.id AS user_id FROM users u
		 JOIN convo c ON u.id = ANY(SELECT jsonb_array_elements_text(c.members))
		WHERE u.id <> $2
		  AND NOT EXISTS (
		    SELECT 1 FROM conversation_mutes m
		     WHERE m.conversation_id = $1 AND m.user_id = u.id
		       AND (m.muted_until IS NULL OR m.muted_until > NOW()))
		  AND NOT EXISTS (
		    SELECT 1 FROM participants p WHERE p.id = u.id AND p.status = 'avail')`,
		conversationID, authorID)
	if err != nil {
		return nil
	}
	out := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	rows.Close()
	return out
}

/* ───────────── HTTP 端点 ───────────── */

// Server:contract.push ServerInterface 的域实现(#187 机械迁移,
// documents 范式)。方法体自原闭包工厂/直接 handler 原样搬运。
type Server struct{ DB *sql.DB }

// 编译期接口把关:规范改动 operation 而域未跟 = 构建红。
var _ contract.ServerInterface = (*Server)(nil)

// Mount:注册串来自契约生成物(pattern 即规范,#139)。
func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

func newID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	h := hex.EncodeToString(b)
	return prefix + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func decodeBodyJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func capRunes(s string, max int) string {
	return utf16Slice(s, max)
}

// utf16Len:UTF-16 码元数(JS string.length 语义)。
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16Slice:按 UTF-16 码元截断;边界落在代理对内部时保整字(与
// TS slice 的裂代理边缘差一个字符,可接受)。
func utf16Slice(s string, max int) string {
	n := 0
	for i, r := range s {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if n+units > max {
			return s[:i]
		}
		n += units
	}
	return s
}

func (s *Server) RegisterPushDevice(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		Platform    string  `json:"platform"`
		Token       string  `json:"token"`
		AppVersion  *string `json:"appVersion"`
		DeviceModel *string `json:"deviceModel"`
	}
	_ = decodeBodyJSON(r, &body)
	platform := body.Platform
	if platform != "ios" && platform != "android" && platform != "web" {
		httpx.WriteError(w, http.StatusBadRequest, "platform must be ios | android | web")
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		httpx.WriteError(w, http.StatusBadRequest, "token required")
		return
	}
	if utf16Len(token) > 1024 {
		httpx.WriteError(w, http.StatusBadRequest, "token too long")
		return
	}
	// 缺省(nil)→ NULL(COALESCE 保旧);空串 → ''(TS 存空串覆盖)
	var appV, modelV any
	if body.AppVersion != nil {
		appV = capRunes(*body.AppVersion, 64)
	}
	if body.DeviceModel != nil {
		modelV = capRunes(*body.DeviceModel, 128)
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO push_devices (id, user_id, platform, token, app_version, device_model)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (platform, token) DO UPDATE SET
		  user_id = EXCLUDED.user_id,
		  last_seen_at = NOW(),
		  disabled_at = NULL,
		  app_version = COALESCE(EXCLUDED.app_version, push_devices.app_version),
		  device_model = COALESCE(EXCLUDED.device_model, push_devices.device_model)`,
		newID("pd-"), uid, platform, token, appV, modelV); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "insert failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) UnregisterPushDevice(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	_ = decodeBodyJSON(r, &body)
	token := strings.TrimSpace(body.Token)
	if token == "" {
		httpx.WriteError(w, http.StatusBadRequest, "token required")
		return
	}
	_, _ = s.DB.ExecContext(r.Context(), `
		UPDATE push_devices SET disabled_at = NOW()
		 WHERE token = $1 AND user_id = $2 AND disabled_at IS NULL`, token, uid)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
