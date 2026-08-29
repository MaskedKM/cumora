// domains/projects —— 项目域(#68 补齐):列表/创建/更新(owner/admin)/
// 归档开关/会话挂接。行为对齐 router.ts 1661–1759。
package projects

import (
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/projects", list(db))
	mux.HandleFunc("POST /api/projects", create(db))
	mux.HandleFunc("PUT /api/projects/{id}", update(db))
	mux.HandleFunc("POST /api/projects/{id}/archive", archive(db))
	mux.HandleFunc("POST /api/conversations/{id}/project", attach(db))
}

// requireRole:owner/admin 门(TS requireCompanyRole;403 恒同文案)。
func requireRole(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	uid, companyID, ok := httpx.RequireCompany(w, r, db)
	if !ok {
		return "", false
	}
	var role string
	if err := db.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
		companyID, uid).Scan(&role); err != nil {
		role = "member"
	}
	if role != "owner" && role != "admin" {
		httpx.WriteError(w, http.StatusForbidden, "this action requires an owner or admin of the team")
		return "", false
	}
	return companyID, true
}

func decodeBody(r *http.Request) map[string]json.RawMessage {
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body
}

func bodyAny(body map[string]json.RawMessage, key string) (any, bool) {
	raw, ok := body[key]
	if !ok {
		return nil, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return v, true
}

func utf16Cap(s string, n int) string {
	count := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
}

func list(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, tenant, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, name, description, color, status,
			       created_at, archived_at,
			       (SELECT COUNT(*)::int FROM conversations WHERE project_id = projects.id)
			  FROM projects
			 WHERE company_id = $1
			 ORDER BY status ASC, created_at DESC`, tenant)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, name, status string
			var description, color sql.NullString
			var createdAt time.Time
			var archivedAt sql.NullTime
			var convoCount int
			if err := rows.Scan(&id, &name, &description, &color, &status, &createdAt, &archivedAt, &convoCount); err != nil {
				continue
			}
			row := map[string]any{
				"id": id, "name": name,
				"description": nullAny(description), "color": nullAny(color),
				"status": status, "createdAt": httpx.ISOms(createdAt),
				"conversationCount": convoCount,
			}
			if archivedAt.Valid {
				row["archivedAt"] = httpx.ISOms(archivedAt.Time)
			} else {
				row["archivedAt"] = nil
			}
			out = append(out, row)
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

func nullAny(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func create(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, tenant, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		body := decodeBody(r)
		nameRaw, _ := bodyAny(body, "name")
		descRaw, _ := bodyAny(body, "description")
		colorRaw, hasColor := bodyAny(body, "color")
		// F16:TS create 是 String(x ?? '') 强转(非 typeof 门),color 另有
		// JS truthy 前置(0/""/null→null,对象/数组恒真)。
		name := utf16Cap(strings.TrimSpace(httpx.JSStringOrNullish(nameRaw)), 80)
		description := utf16Cap(httpx.JSStringOrNullish(descRaw), 1000)
		var color any
		if hasColor && httpx.JSTruthy(colorRaw) {
			color = utf16Cap(httpx.JSToString(colorRaw), 200)
		}
		if name == "" {
			httpx.WriteError(w, http.StatusBadRequest, "name required")
			return
		}
		id := "p-" + randHex10()
		var colorArg any
		if s, ok := color.(string); ok {
			colorArg = s
		}
		if _, err := db.ExecContext(r.Context(),
			`INSERT INTO projects (id, company_id, name, description, color) VALUES ($1, $2, $3, $4, $5)`,
			id, tenant, name, description, colorArg); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{
			"id": id, "name": name, "description": description, "color": colorArg, "status": "active",
		})
	}
}

func update(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		var one int
		if err := db.QueryRowContext(r.Context(),
			`SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`, id, tenant).Scan(&one); err != nil {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		body := decodeBody(r)
		sets := []string{}
		params := []any{}
		add := func(v any, col string) {
			params = append(params, v)
			sets = append(sets, fmt.Sprintf("%s = $%d", col, len(params)))
		}
		// TS:键存在且为 string 才 trim/slice 更新;color 键存在为 null
		// 则显式清空。
		if v, has := bodyAny(body, "name"); has {
			if s, isStr := v.(string); isStr {
				add(utf16Cap(strings.TrimSpace(s), 80), "name")
			}
		}
		if v, has := bodyAny(body, "description"); has {
			if s, isStr := v.(string); isStr {
				add(utf16Cap(s, 1000), "description")
			}
		}
		if v, has := bodyAny(body, "color"); has {
			if s, isStr := v.(string); isStr {
				add(utf16Cap(s, 200), "color")
			} else if v == nil {
				add(nil, "color")
			}
		}
		if len(sets) == 0 {
			httpx.WriteError(w, http.StatusBadRequest, "nothing to update")
			return
		}
		params = append(params, id, tenant)
		if _, err := db.ExecContext(r.Context(),
			fmt.Sprintf(`UPDATE projects SET %s WHERE id = $%d AND company_id = $%d`,
				strings.Join(sets, ", "), len(params)-1, len(params)), params...); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func archive(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		archive := true
		if v, has := bodyAny(decodeBody(r), "archive"); has && v == false {
			archive = false
		}
		var stmt string
		if archive {
			stmt = `UPDATE projects SET status = 'archived', archived_at = NOW() WHERE id = $1 AND company_id = $2`
		} else {
			stmt = `UPDATE projects SET status = 'active', archived_at = NULL WHERE id = $1 AND company_id = $2`
		}
		if _, err := db.ExecContext(r.Context(), stmt, id, tenant); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		status := "active"
		if archive {
			status = "archived"
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
	}
}

func attach(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, tenant, ok := httpx.RequireCompany(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		body := decodeBody(r)
		// undefined(缺键/非串非 null)→ 400;null → 解绑;串 → 绑定。
		raw, has := bodyAny(body, "projectId")
		if !has {
			httpx.WriteError(w, http.StatusBadRequest, "projectId required (string or null to detach)")
			return
		}
		var projectID any
		switch v := raw.(type) {
		case nil:
			projectID = nil
		case string:
			s := strings.TrimSpace(v)
			if s == "" {
				httpx.WriteError(w, http.StatusBadRequest, "projectId required (string or null to detach)")
				return
			}
			projectID = s
		default:
			httpx.WriteError(w, http.StatusBadRequest, "projectId required (string or null to detach)")
			return
		}
		var membersJSON string
		err := db.QueryRowContext(r.Context(),
			`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2`, id, tenant).
			Scan(&membersJSON)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		var members []string
		_ = json.Unmarshal([]byte(membersJSON), &members)
		isMember := false
		for _, m := range members {
			if m == uid {
				isMember = true
				break
			}
		}
		if !isMember {
			httpx.WriteError(w, http.StatusForbidden, "only members can change the project")
			return
		}
		if s, isStr := projectID.(string); isStr {
			var one int
			if err := db.QueryRowContext(r.Context(),
				`SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`, s, tenant).Scan(&one); err != nil {
				httpx.WriteError(w, http.StatusBadRequest, "unknown project")
				return
			}
		}
		if _, err := db.ExecContext(r.Context(),
			`UPDATE conversations SET project_id = $2, updated_at = NOW() WHERE id = $1 AND company_id = $3`,
			id, projectID, tenant); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "projectId": projectID})
	}
}

func randHex10() string {
	b := make([]byte, 5)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}
