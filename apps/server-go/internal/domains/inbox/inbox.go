// domains/inbox —— #264 人侧 Inbox 分级:消费面(CRUD + 静音偏好)
// 与生成面(Emit)。分级语义:action_required 只留给"需要人裁决"
// (run 失败/卡片转 ready-for-human)——推送 + 弹条;attention(卡片指派
// 给你等)落账 + 轻徽标;info(例行完成)纯落账。type 是静音键
// (user_preferences.prefs.inboxMutedTypes):静音 = 不推送不弹条,条目
// 仍在列表(透明可查)。
package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/inbox"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/push"
)

// Server:contract.ServerInterface(inbox tag)的域实现。
type Server struct{ DB *sql.DB }

var _ contract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

const (
	listLimit = 200
	titleMax  = 200
	bodyMax   = 1000
)

/* ───────────── 消费面 ───────────── */

func (s *Server) ListInbox(w http.ResponseWriter, r *http.Request) {
	userID, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, severity, type, title, body, link_kind, link_id, read_at, created_at
		  FROM inbox_items WHERE company_id = $1 AND user_id = $2
		 ORDER BY created_at DESC LIMIT $3`, companyID, userID, listLimit)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	type item struct {
		ID        string `json:"id"`
		Severity  string `json:"severity"`
		Type      string `json:"type"`
		Title     string `json:"title"`
		Body      any    `json:"body"`
		LinkKind  any    `json:"linkKind"`
		LinkId    any    `json:"linkId"`
		Read      bool   `json:"read"`
		CreatedAt string `json:"createdAt"`
	}
	items := []item{}
	counts := map[string]int{"action_required": 0, "attention": 0, "info": 0}
	for rows.Next() {
		var it item
		var body, linkKind, linkID sql.NullString
		var readAt sql.NullTime
		var createdAt time.Time
		if rows.Scan(&it.ID, &it.Severity, &it.Type, &it.Title, &body, &linkKind, &linkID, &readAt, &createdAt) != nil {
			continue
		}
		it.Body = nullable(body)
		it.LinkKind = nullable(linkKind)
		it.LinkId = nullable(linkID)
		it.Read = readAt.Valid
		it.CreatedAt = httpx.ISOms(createdAt)
		if !it.Read {
			counts[it.Severity]++
		}
		items = append(items, it)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"counts":     map[string]any{"actionRequired": counts["action_required"], "attention": counts["attention"], "info": counts["info"]},
		"mutedTypes": MutedTypes(r.Context(), s.DB, userID),
	})
}

func (s *Server) MarkInboxItemRead(w http.ResponseWriter, r *http.Request, id string) {
	userID, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	res, err := s.DB.ExecContext(r.Context(),
		`UPDATE inbox_items SET read_at = now() WHERE id = $1 AND company_id = $2 AND user_id = $3 AND read_at IS NULL`,
		id, companyID, userID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "item not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) MarkAllInboxRead(w http.ResponseWriter, r *http.Request) {
	userID, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	_, _ = s.DB.ExecContext(r.Context(),
		`UPDATE inbox_items SET read_at = now() WHERE company_id = $1 AND user_id = $2 AND read_at IS NULL`,
		companyID, userID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) GetInboxMutes(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"types": MutedTypes(r.Context(), s.DB, userID)})
}

func (s *Server) SetInboxMutes(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var body struct {
		Types []string `json:"types"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	seen := map[string]bool{}
	types := make([]string, 0, len(body.Types))
	for _, t := range body.Types {
		t = strings.TrimSpace(t)
		if t == "" || len(t) > 64 || seen[t] {
			continue
		}
		seen[t] = true
		types = append(types, t)
	}
	if len(types) > 64 {
		httpx.WriteError(w, http.StatusBadRequest, "too many muted types")
		return
	}
	if err := setMutedTypes(r.Context(), s.DB, userID, types); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ───────────── 静音偏好(user_preferences.prefs.inboxMutedTypes) ───────────── */

// MutedTypes:静音 type 清单(无偏好/坏偏好 = 空)。
func MutedTypes(ctx context.Context, db *sql.DB, userID string) []string {
	var prefs []byte
	if err := db.QueryRowContext(ctx,
		`SELECT prefs FROM user_preferences WHERE user_id = $1 LIMIT 1`, userID).Scan(&prefs); err != nil {
		return []string{}
	}
	var parsed struct {
		InboxMutedTypes []string `json:"inboxMutedTypes"`
	}
	if json.Unmarshal(prefs, &parsed) != nil || parsed.InboxMutedTypes == nil {
		return []string{}
	}
	return parsed.InboxMutedTypes
}

func setMutedTypes(ctx context.Context, db *sql.DB, userID string, types []string) error {
	// prefs jsonb 整读 → 改一个键 → 整写(行缺失则插;ON CONFLICT 兜并发)。
	var prefs []byte
	err := db.QueryRowContext(ctx,
		`SELECT prefs FROM user_preferences WHERE user_id = $1 LIMIT 1`, userID).Scan(&prefs)
	var m map[string]any
	if err == sql.ErrNoRows {
		m = map[string]any{}
	} else if err != nil {
		return err
	} else if json.Unmarshal(prefs, &m) != nil {
		m = map[string]any{}
	}
	m["inboxMutedTypes"] = types
	b, _ := json.Marshal(m)
	_, err = db.ExecContext(ctx, `
		INSERT INTO user_preferences (user_id, prefs) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET prefs = EXCLUDED.prefs, updated_at = now()`,
		userID, b)
	return err
}

func nullable(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}

/* ───────────── 生成面:Emit ───────────── */

// Emit:落一条 inbox 条目 + WS 广播 + (action_required 且未静音)推送。
// 生成方(runtime finish / boards / calendar dispatch)尽力而为调用——
// 内部任何失败只记日志,绝不影响主路径。返回条目 id(失败空串)。
func Emit(ctx context.Context, db *sql.DB, companyID, userID, severity, typ, title, body, linkKind, linkID string) string {
	title = strings.TrimSpace(title)
	if len(title) > titleMax {
		title = title[:titleMax]
	}
	if len(body) > bodyMax {
		body = body[:bodyMax]
	}
	id := "inbx-" + httpx.UUIDHex()[:20]
	if _, err := db.ExecContext(ctx, `
		INSERT INTO inbox_items (id, company_id, user_id, severity, type, title, body, link_kind, link_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		id, companyID, userID, severity, typ, title, nullStr(body), nullStr(linkKind), nullStr(linkID)); err != nil {
		slog.Warn("[inbox] emit insert failed", "err", err)
		return ""
	}
	events.InboxNew(ctx, events.InboxNewEvent{
		CompanyID:       companyID,
		RecipientUserID: userID,
		ItemID:          id,
		Severity:        severity,
		ItemType:        typ,
		Title:           title,
		Body:            body,
		LinkKind:        linkKind,
		LinkID:          linkID,
	})
	// 推送纪律:只有 action_required 且该 type 未静音才走 push_devices;
	// attention/info 不推(桌面弹条由前端按 WS 帧的 severity 分级处理)。
	if severity == "action_required" && !containsType(MutedTypes(ctx, db, userID), typ) {
		push.SendToUsers(ctx, db, []string{userID}, push.Payload{
			Title: title,
			Body:  body,
			Data:  map[string]any{"kind": "inbox", "itemId": id, "severity": severity, "type": typ},
		})
	}
	return id
}

// CompanyOwner:公司 owner 的 user_id(生成面的默认收件人;BYOA 单运营
// 者主形态)。
func CompanyOwner(ctx context.Context, db *sql.DB, companyID string) string {
	var owner string
	if err := db.QueryRowContext(ctx,
		`SELECT owner_user_id FROM companies WHERE id = $1 LIMIT 1`, companyID).Scan(&owner); err != nil {
		return ""
	}
	return owner
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func containsType(list []string, t string) bool {
	for _, x := range list {
		if x == t {
			return true
		}
	}
	return false
}
