// domains/documents —— 文档域(#55):CRUD + 协作者治理 + doc.changed
// 事件。协同编辑的 WS 帧面在 wsx 网关、房间状态在 yjs-sidecar。
// 行为对齐 server/src/api/router.ts 的 /documents 段。
package documents

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// shortHex:n 字节随机数的 hex(id 段对齐 TS randomUUID().replace(/-/g,”).slice(0,2n))。
func shortHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type docRow struct {
	id             string
	title          string
	createdBy      string
	conversationID sql.NullString
	createdAt      time.Time
	updatedAt      time.Time
}

const docSelect = `SELECT id, title, created_by, conversation_id, created_at, updated_at
	 FROM documents WHERE id = $1 AND company_id = $2`

func scanDoc(row interface{ Scan(...any) error }) (docRow, bool) {
	var d docRow
	if row.Scan(&d.id, &d.title, &d.createdBy, &d.conversationID, &d.createdAt, &d.updatedAt) != nil {
		return d, false
	}
	return d, true
}

func toDocPayload(d docRow) map[string]any {
	var convo any
	if d.conversationID.Valid {
		convo = d.conversationID.String
	}
	return map[string]any{
		"id": d.id, "title": d.title, "createdBy": d.createdBy,
		"conversationId": convo, "createdAt": d.createdAt.UTC(), "updatedAt": d.updatedAt.UTC(),
	}
}

// text:TS `.trim().slice(0, N)` —— UTF-16 码元截断(#141 rider:
// rune 截断在代理对边界漂移,长 emoji 标题会差 1 字)。
func text(v string, max int) string {
	return httpx.UTF16Cap(strings.TrimSpace(v), max)
}

// privileged 对齐 PRIVILEGED_ROLES:creator-or-owner/admin 治理规则。
func privileged(db *sql.DB, companyID, userID string) bool {
	var role string
	if err := db.QueryRow(
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2`,
		companyID, userID).Scan(&role); err != nil {
		return false
	}
	return role == "owner" || role == "admin"
}

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/documents", list(db))
	mux.HandleFunc("POST /api/documents", create(db))
	mux.HandleFunc("GET /api/documents/{id}", get(db))
	mux.HandleFunc("PUT /api/documents/{id}", updateTitle(db))
	mux.HandleFunc("PUT /api/documents/{id}/collaborators", setCollaborators(db))
	mux.HandleFunc("DELETE /api/documents/{id}", del(db))
}

func list(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, title, created_by, conversation_id, created_at, updated_at
			  FROM documents WHERE company_id = $1
			 ORDER BY updated_at DESC LIMIT 200`, companyID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			if d, ok := scanDoc(rows); ok {
				out = append(out, toDocPayload(d))
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"documents": out})
	}
}

func create(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		var body struct {
			Title          string `json:"title"`
			ConversationID string `json:"conversationId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		title := text(body.Title, 200)
		if title == "" {
			title = "Untitled"
		}
		// 可选钉到会话;校验属本租户。原值不 trim(TS 语义:`" conv-x "`
		// 这样的值在基线里 404,trim 会静默"修好"它)。
		var convo sql.NullString
		if body.ConversationID != "" {
			var exists bool
			if err := db.QueryRowContext(r.Context(),
				`SELECT 1 FROM conversations WHERE id = $1 AND company_id = $2 LIMIT 1`,
				body.ConversationID, companyID).Scan(&exists); err != nil || !exists {
				httpx.WriteError(w, http.StatusNotFound, "conversation not found")
				return
			}
			convo = sql.NullString{String: body.ConversationID, Valid: true}
		}
		id := "doc_" + shortHex(8)
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO documents (id, company_id, title, created_by, conversation_id)
			VALUES ($1, $2, $3, $4, $5)`, id, companyID, title, uid, convo); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "insert failed")
			return
		}
		var d docRow
		err := db.QueryRowContext(r.Context(), docSelect, id, companyID).
			Scan(&d.id, &d.title, &d.createdBy, &d.conversationID, &d.createdAt, &d.updatedAt)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "readback failed")
			return
		}
		events.DocChanged(r.Context(), companyID, id, "document.created", uid)
		httpx.WriteJSON(w, http.StatusCreated, toDocPayload(d))
	}
}

func get(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		var d docRow
		err := db.QueryRowContext(r.Context(), docSelect, r.PathValue("id"), companyID).
			Scan(&d.id, &d.title, &d.createdBy, &d.conversationID, &d.createdAt, &d.updatedAt)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, toDocPayload(d))
	}
}

func updateTitle(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		title := strings.TrimSpace(body.Title)
		if title == "" {
			httpx.WriteError(w, http.StatusBadRequest, "title required")
			return
		}
		title = text(title, 200)
		res, err := db.ExecContext(r.Context(), `
			UPDATE documents SET title = $1, updated_at = NOW()
			 WHERE id = $2 AND company_id = $3`, title, r.PathValue("id"), companyID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		events.DocChanged(r.Context(), companyID, r.PathValue("id"), "document.updated", uid)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "title": title})
	}
}

// setCollaborators:整组覆写参与者集合(工作区隐式成员的来源之一)。
// 治理 = creator 或 owner/admin(与 delete 同规则)。
func setCollaborators(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		var body struct {
			ParticipantIDs []any `json:"participantIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ParticipantIDs == nil {
			httpx.WriteError(w, http.StatusBadRequest, "participantIds (string[]) required")
			return
		}
		// TS:filter(typeof string).map(trim).filter(Boolean) 后去重保序。
		seen := map[string]bool{}
		ids := []string{}
		for _, v := range body.ParticipantIDs {
			s, ok := v.(string)
			if !ok {
				continue
			}
			t := strings.TrimSpace(s)
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			ids = append(ids, t)
		}
		if len(ids) > 100 {
			httpx.WriteError(w, http.StatusBadRequest, "too many participantIds (max 100)")
			return
		}
		var createdBy string
		err := db.QueryRowContext(r.Context(),
			`SELECT created_by FROM documents WHERE id = $1 AND company_id = $2`,
			r.PathValue("id"), companyID).Scan(&createdBy)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if createdBy != uid && !privileged(db, companyID, uid) {
			httpx.WriteError(w, http.StatusForbidden, "only the creator or an owner can edit collaborators")
			return
		}
		if len(ids) > 0 {
			rows, err := db.QueryContext(r.Context(), `
				SELECT id FROM participants
				 WHERE company_id = $1 AND id = ANY($2::text[]) AND departed_at IS NULL`,
				companyID, ids)
			if err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, "participants query failed")
				return
			}
			known := map[string]bool{}
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					known[id] = true
				}
			}
			rows.Close()
			missing := []string{}
			for _, id := range ids {
				if !known[id] {
					missing = append(missing, id)
				}
			}
			if len(missing) > 0 {
				httpx.WriteError(w, http.StatusBadRequest,
					"unknown active participant(s): "+strings.Join(missing, ", "))
				return
			}
		}
		raw, _ := json.Marshal(ids)
		if _, err := db.ExecContext(r.Context(), `
			UPDATE documents SET collaborators = $2::jsonb, updated_at = NOW() WHERE id = $1`,
			r.PathValue("id"), raw); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		events.DocChanged(r.Context(), companyID, r.PathValue("id"), "document.updated", uid)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "collaborators": ids})
	}
}

func del(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		var createdBy string
		err := db.QueryRowContext(r.Context(),
			`SELECT created_by FROM documents WHERE id = $1 AND company_id = $2`,
			r.PathValue("id"), companyID).Scan(&createdBy)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		if createdBy != uid && !privileged(db, companyID, uid) {
			httpx.WriteError(w, http.StatusForbidden, "only the creator or an owner can delete")
			return
		}
		if _, err := db.ExecContext(r.Context(),
			`DELETE FROM documents WHERE id = $1`, r.PathValue("id")); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "delete failed")
			return
		}
		events.DocChanged(r.Context(), companyID, r.PathValue("id"), "document.deleted", uid)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}
