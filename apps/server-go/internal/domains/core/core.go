// domains/core —— 认证/me/companies 域(#52):/api/auth/me、/api/auth/logout、
// /api/auth/ws-ticket、/api/me、/api/me/quota、/api/me/preferences、
// /api/me/account(DELETE)、/api/companies(list/create)。
// 行为对齐 server/src/api/router.ts 同名 handler;契约见 packages/contract。
package core

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/auth/me", authMe(db))
	mux.HandleFunc("POST /api/auth/logout", logout(db))
	mux.HandleFunc("POST /api/auth/ws-ticket", wsTicket(db))
	mux.HandleFunc("GET /api/me", me(db))
	mux.HandleFunc("GET /api/me/quota", quota())
	mux.HandleFunc("GET /api/me/preferences", preferencesGet(db))
	mux.HandleFunc("PUT /api/me/preferences", preferencesPut(db))
	mux.HandleFunc("DELETE /api/me/account", accountDelete(db))
	mux.HandleFunc("GET /api/companies", companiesList(db))
	mux.HandleFunc("POST /api/companies", companiesCreate(db))
}

func authMe(db *sql.DB) http.HandlerFunc {
	type companyRow struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
		Role string `json:"role"`
		Tier string `json:"tier"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		var id, email, displayName string
		var emailVerifiedAt sql.NullString
		var isAdmin bool
		err := db.QueryRowContext(r.Context(),
			`SELECT id, email, display_name, email_verified_at, is_admin FROM users WHERE id = $1`, uid).
			Scan(&id, &email, &displayName, &emailVerifiedAt, &isAdmin)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "session points to missing user")
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT c.id, c.name, c.slug, cm.role, COALESCE(o.tier, 'free')
			  FROM company_members cm
			  JOIN companies c ON c.id = cm.company_id
			  LEFT JOIN users o ON o.id = c.owner_user_id
			 WHERE cm.user_id = $1 ORDER BY cm.joined_at ASC`, uid)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		companies := []companyRow{}
		for rows.Next() {
			var c companyRow
			if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.Role, &c.Tier); err != nil {
				continue
			}
			companies = append(companies, c)
		}
		identRows, _ := db.QueryContext(r.Context(), `SELECT provider FROM user_identities WHERE user_id = $1`, uid)
		providers := []string{}
		if identRows != nil {
			defer identRows.Close()
			for identRows.Next() {
				var p string
				_ = identRows.Scan(&p)
				providers = append(providers, p)
			}
		}
		var activeCompany any
		if len(companies) > 0 {
			activeCompany = companies[0].ID
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"user": map[string]any{
				"id":            id,
				"email":         email,
				"name":          displayName,
				"emailVerified": emailVerifiedAt.Valid,
				"isAdmin":       isAdmin,
				"providers":     providers,
			},
			"companies":       companies,
			"activeCompanyId": activeCompany,
			"serverCapabilities": map[string]any{
				"invitationEmail": os.Getenv("EMAIL_DOMAIN") != "",
			},
		})
	}
}

func logout(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := httpx.UserID(r); ok {
			token := authn.Bearer(r.Header.Get("Authorization"), r.Header.Get("x-session-token"))
			if token != "" {
				_, _ = db.ExecContext(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`, authn.HashToken(token))
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func wsTicket(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		ticket, expiresAt, err := authn.CreateWsTicket(r.Context(), db, uid)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "expiresAt": expiresAt})
	}
}

func me(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		var id, displayName string
		var kind string
		err := db.QueryRowContext(r.Context(), `
			SELECT p.id, p.name, p.kind FROM participants p
			 WHERE p.id = (SELECT id FROM users WHERE id = $1) LIMIT 1`, uid).Scan(&id, &displayName, &kind)
		if err != nil {
			// baseline:无 participants 行时回 users 表形态
			if err2 := db.QueryRowContext(r.Context(),
				`SELECT id, display_name, 'human' FROM users WHERE id = $1`, uid).
				Scan(&id, &displayName, &kind); err2 != nil {
				httpx.WriteError(w, http.StatusNotFound, "not found")
				return
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "name": displayName, "kind": kind})
	}
}

func quota() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := httpx.RequireAuth(w, r); !ok {
			return
		}
		// sub2api 未配置的自托管形态(baseline:configured=false, snapshot=null)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"configured": false, "snapshot": nil})
	}
}

func preferencesGet(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		var prefs json.RawMessage
		err := db.QueryRowContext(r.Context(),
			`SELECT prefs FROM user_preferences WHERE user_id = $1`, uid).Scan(&prefs)
		if err != nil || prefs == nil {
			prefs = json.RawMessage("{}")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(prefs)
	}
}

func preferencesPut(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		var prefs json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&prefs); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		_, err := db.ExecContext(r.Context(), `
			INSERT INTO user_preferences (user_id, prefs, updated_at) VALUES ($1, $2, NOW())
			ON CONFLICT (user_id) DO UPDATE SET prefs = $2, updated_at = NOW()`, uid, prefs)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func accountDelete(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		// 软删 + PII 清空 + 会话全灭(baseline 语义;审计写失败不阻断)
		_, _ = db.ExecContext(r.Context(), `
			UPDATE users SET deleted_at = NOW(),
			  email = 'deleted+' || id || '@deleted.local',
			  display_name = 'Deleted user', email_verified_at = NULL
			WHERE id = $1`, uid)
		_, _ = db.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id = $1`, uid)
		_, _ = db.ExecContext(r.Context(), `DELETE FROM user_identities WHERE user_id = $1`, uid)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func companiesList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT c.id, c.name, c.slug, c.created_at, cm.role
			  FROM company_members cm JOIN companies c ON c.id = cm.company_id
			 WHERE cm.user_id = $1 ORDER BY cm.joined_at ASC`, uid)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		type row struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Slug      string `json:"slug"`
			CreatedAt string `json:"createdAt"`
			Role      string `json:"role"`
		}
		out := []row{}
		for rows.Next() {
			var x row
			if rows.Scan(&x.ID, &x.Name, &x.Slug, &x.CreatedAt, &x.Role) == nil {
				out = append(out, x)
			}
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

func companiesCreate(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "" {
			httpx.WriteError(w, http.StatusBadRequest, "name required")
			return
		}
		slug := slugify(body.Name)
		// 唯一 slug(冲突追加随机后缀,与 baseline 撞名重试语义一致)
		var count int
		_ = db.QueryRowContext(r.Context(), `SELECT count(*) FROM companies WHERE slug = $1 OR slug LIKE $1 || '-%'`, slug).Scan(&count)
		if count > 0 {
			slug = slug + "-" + authn.NewToken()[:6]
		}
		id := "co-" + authn.NewToken()[:10]
		_, err := db.ExecContext(r.Context(), `
			INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
			id, body.Name, slug, uid)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		_, _ = db.ExecContext(r.Context(),
			`INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, 'owner')`, id, uid)
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "name": body.Name, "slug": slug, "role": "owner"})
	}
}

func slugify(s string) string {
	out := make([]rune, 0, len(s))
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case c == ' ', c == '-', c == '_':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "team"
	}
	return string(out)
}
