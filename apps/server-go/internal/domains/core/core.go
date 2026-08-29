// domains/core —— 认证/me/companies 域(#52):/api/auth/me、/api/auth/logout、
// /api/auth/ws-ticket、/api/me、/api/me/quota、/api/me/preferences、
// /api/me/account(DELETE)、/api/companies(list/create)。
// 行为对齐 server/src/api/router.ts 同名 handler;契约见 packages/contract。
package core

import (
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
	"github.com/redis/go-redis/v9"
)

func Mount(mux *http.ServeMux, db *sql.DB, rdb *redis.Client) {
	mux.HandleFunc("GET /api/auth/me", authMe(db))
	mux.HandleFunc("POST /api/auth/logout", logout(db))
	mux.HandleFunc("POST /api/auth/ws-ticket", wsTicket(db))
	// OAuth 登录流(#109):匿名路由(start 302 出去、callback 302 回来),
	// 不走 requireAuth;state 走 Redis。
	oauthDeps := oauthDeps{db: db, rdb: rdb}
	mux.HandleFunc("GET /api/auth/start/{provider}", oauthStart(oauthDeps))
	mux.HandleFunc("GET /api/auth/callback/{provider}", oauthCallback(oauthDeps))
	// iOS 原生 Sign in with Apple(#123):匿名路由,验 JWKS 后以 JSON
	// 回会话 token(无浏览器重定向)。
	mux.HandleFunc("POST /api/auth/apple/native", appleNative(oauthDeps))
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
			httpx.WriteInternalError(w, r, err)
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
		// baseline:仅认 Authorization 头,不校验会话有效性——挂起用户的
		// 令牌也能注销自己的行。
		auth := r.Header.Get("Authorization")
		if len(auth) > 7 && auth[:7] == "Bearer " {
			_, _ = db.ExecContext(r.Context(), `DELETE FROM sessions WHERE token_hash = $1`,
				authn.HashToken(strings.TrimSpace(auth[7:])))
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
			httpx.WriteInternalError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "expiresAt": expiresAt.UTC()})
	}
}

func me(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		var id, displayName string
		err := db.QueryRowContext(r.Context(),
			`SELECT id, display_name FROM users WHERE id = $1`, uid).Scan(&id, &displayName)
		if err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "session points to missing user")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "name": displayName, "kind": "human"})
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
			httpx.WriteInternalError(w, r, err)
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
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		defer tx.Rollback()

		var email sql.NullString
		err = tx.QueryRowContext(r.Context(),
			`SELECT email FROM users WHERE id = $1 AND deleted_at IS NULL`, uid).Scan(&email)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "account already deleted or not found")
			return
		}
		// 软删 + PII 全清(哨兵邮箱保 UNIQUE + 审计线索;对齐 baseline 字段集)
		sentinel := "deleted+" + uid + "@cumora.invalid"
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE users SET deleted_at = NOW(), email = $2, display_name = 'Deleted user',
			  password_hash = NULL, avatar_url = NULL, email_verified_at = NULL
			WHERE id = $1`, uid, sentinel); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		for _, q := range []string{
			`DELETE FROM sessions WHERE user_id = $1`,
			`DELETE FROM ws_tickets WHERE user_id = $1`,
			`DELETE FROM user_identities WHERE user_id = $1`,
			`UPDATE participants SET departed_at = NOW() WHERE id = $1 AND kind = 'human' AND departed_at IS NULL`,
		} {
			if _, err := tx.ExecContext(r.Context(), q, uid); err != nil {
				httpx.WriteInternalError(w, r, err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		// 审计失败不阻断(fire-and-forget,与 baseline 一致)
		auditIP2, auditUA2 := r.RemoteAddr, r.UserAgent()
		auditEmail := email.String
		go db.Exec(`INSERT INTO audit_events (user_id, kind, ip, user_agent, detail)
			VALUES ($1, 'account_deleted', NULLIF($2,''), NULLIF($3,''), $4::jsonb)`,
			uid, auditIP2, auditUA2, `{"email":`+jsonString(auditEmail)+`}`)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
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
			httpx.WriteInternalError(w, r, err)
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
			var ca time.Time
			if rows.Scan(&x.ID, &x.Name, &x.Slug, &ca, &x.Role) == nil {
				x.CreatedAt = ca.UTC().Format("2006-01-02T15:04:05.000Z07:00")
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
		// TS router.ts:962 `String(req.body?.name ?? '').trim().slice(0, 80)`
		// —— 逐键解码 + JSStringOrNullish 强转(#118:struct 解码会把
		// 非串值整包丢弃,name:123 曾直接落 400);slice 按 UTF-16 码元。
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		var nameRaw any
		if v, ok := raw["name"]; ok {
			_ = json.Unmarshal(v, &nameRaw)
		}
		name := httpx.UTF16Cap(strings.TrimSpace(httpx.JSStringOrNullish(nameRaw)), 80)
		if name == "" {
			httpx.WriteError(w, http.StatusBadRequest, "name required")
			return
		}
		// tier 限(对齐 assertUserCompanyLimit / TIER_LIMITS)
		var tier sql.NullString
		var companyCount int
		if err := db.QueryRowContext(r.Context(), `
			SELECT u.tier, COUNT(cm.company_id)::int FROM users u
			  LEFT JOIN company_members cm ON cm.user_id = u.id
			 WHERE u.id = $1 GROUP BY u.tier`, uid).Scan(&tier, &companyCount); err != nil {
			httpx.WriteError(w, http.StatusUnauthorized, "session points to missing user")
			return
		}
		t := "free"
		if tier.Valid {
			t = strings.ToLower(tier.String)
		}
		limit := tierCompanies(t)
		if companyCount >= limit {
			httpx.WriteError(w, http.StatusForbidden, fmt.Sprintf("%s tier users can belong to at most %d companies", t, limit))
			return
		}
		baseSlug := slugify(name)
		slug := baseSlug
		id := "co-" + authn.NewToken()[:10]
		for attempt := 0; attempt < 3; attempt++ {
			_, err := db.ExecContext(r.Context(), `
				INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
				id, name, slug, uid)
			if err != nil {
				if strings.Contains(err.Error(), "duplicate key") {
					slug = baseSlug + "-" + authn.NewToken()[:4]
					id = "co-" + authn.NewToken()[:10]
					continue
				}
				httpx.WriteInternalError(w, r, err)
				return
			}
			_, _ = db.ExecContext(r.Context(),
				`INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, 'owner')`, id, uid)
			// 镜像 signup 的"人也是参与者"(幂等;同人多司时参与者行归首司)
			var displayName, email sql.NullString
			_ = db.QueryRowContext(r.Context(),
				`SELECT display_name, email FROM users WHERE id = $1`, uid).Scan(&displayName, &email)
			dn := uid
			if displayName.Valid {
				dn = displayName.String
			}
			var avatar any
			if email.Valid {
				avatar = gravatarURL(email.String)
			}
			initial := "U"
			if dn != "" {
				r, _ := utf8.DecodeRuneInString(dn)
				initial = strings.ToUpper(string(r))
			}
			_, _ = db.ExecContext(r.Context(), `
				INSERT INTO participants (id, kind, name, initial, avatar_bg, avatar_url, status, company_id)
				VALUES ($1, 'human', $2, $3, '#FF8870', $4, 'avail', $5)
				ON CONFLICT (id, company_id) DO NOTHING`, uid, dn, initial, avatar, id)
			onboard.JoinAllHands(r.Context(), db, id, uid)
			auditIP, auditUA := r.RemoteAddr, r.UserAgent()
			auditDetail := fmt.Sprintf(`{"name":%s,"slug":%s}`, jsonString(name), jsonString(slug))
			go db.Exec(`INSERT INTO audit_events (user_id, company_id, kind, ip, user_agent, detail)
				VALUES ($1, $2, 'company_create', NULLIF($3,''), NULLIF($4,''), $5::jsonb)`,
				uid, id, auditIP, auditUA, auditDetail)
			httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "name": name, "slug": slug, "role": "owner"})
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "failed to create company after retries")
	}
}

// tierCompanies 对齐 TIER_LIMITS(free=3/pro=10/max=25)。
func tierCompanies(t string) int {
	switch t {
	case "pro":
		return 10
	case "max":
		return 25
	default:
		return 3
	}
}

// gravatarURL 对齐 gravatarUrlForEmail(lower-trim-md5,d=identicon,s=256)。
func gravatarURL(email string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("https://www.gravatar.com/avatar/%x?d=identicon&s=256", sum)
}

// slugify 对齐 baseline:[^a-z0-9]+ 运行折叠为单个 -,去首尾 -,截 40,
// 空则 'company'。
func slugify(s string) string {
	low := strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, c := range low {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		return "company"
	}
	return out
}
