// governance —— /api/admin 治理子面(#124):users 列表/详情/patch
// (tier/isAdmin/封禁)、waitlist 列表/审批/拒绝、stats 面板头。逐段对齐
// server/src/api/admin-router.ts(users/waitlist/stats 段)+ admin.ts
// (suspendUser/unsuspendUser/changeUserTier/listWaitlist/rejectWaitlist);
// approveWaitlist 的入伙机器在 core 包(与 oauth Path C 同构,机器复用)。
// 响应形状以前端 admin/api.ts 的真实消费为准(users/waitlist 是
// {items,total,limit,offset} 包裹,契约 schema 的裸 array 是滞后描述)。
package admin

import (
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/core"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

/* ───────────── users ───────────── */

type userRowDb struct {
	id               string
	email            string
	displayName      string
	avatarURL        sql.NullString
	tier             string
	isAdmin          bool
	sub2apiUserID    sql.NullInt64
	createdAt        string
	lastLoginAt      sql.NullString
	companyCount     int
	suspendedAt      sql.NullString
	suspensionReason sql.NullString
	suspendedBy      sql.NullString
}

const userColsSQL = `u.id, u.email, u.display_name, u.avatar_url, u.tier, u.is_admin, u.sub2api_user_id,
            u.created_at::text, u.last_login_at::text,
            u.suspended_at::text, u.suspension_reason, u.suspended_by,
            (SELECT COUNT(*)::int FROM company_members cm WHERE cm.user_id = u.id) AS company_count`

type rowScanner interface{ Scan(dest ...any) error }

func scanUserRow(scanner rowScanner) (userRowDb, error) {
	var r userRowDb
	err := scanner.Scan(&r.id, &r.email, &r.displayName, &r.avatarURL, &r.tier, &r.isAdmin,
		&r.sub2apiUserID, &r.createdAt, &r.lastLoginAt,
		&r.suspendedAt, &r.suspensionReason, &r.suspendedBy, &r.companyCount)
	return r, err
}

// gravatarURL:core/invitations 各有一份的同名件(MD5;#141 收口对象)。
func gravatarURL(email string) string {
	sum := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	return fmt.Sprintf("https://www.gravatar.com/avatar/%x?d=identicon&s=256", sum)
}

func nullStrAny(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}

func rowToUser(r userRowDb) map[string]any {
	avatar := any(nil)
	if r.avatarURL.Valid && r.avatarURL.String != "" {
		avatar = r.avatarURL.String
	} else {
		avatar = gravatarURL(r.email) // 行头像兜底(老用户/dev seed 为 NULL)
	}
	sub2api := any(nil)
	if r.sub2apiUserID.Valid {
		sub2api = r.sub2apiUserID.Int64
	}
	return map[string]any{
		"id": r.id, "email": r.email, "name": r.displayName, "avatarUrl": avatar,
		"tier": r.tier, "isAdmin": r.isAdmin, "sub2apiUserId": sub2api,
		"createdAt": r.createdAt, "lastLoginAt": nullStrAny(r.lastLoginAt),
		"companyCount": r.companyCount,
		"suspended":    r.suspendedAt.Valid, "suspendedAt": nullStrAny(r.suspendedAt),
		"suspensionReason": nullStrAny(r.suspensionReason), "suspendedBy": nullStrAny(r.suspendedBy),
	}
}

// jsNumOr:TS `Number(q.x) || def` —— 0/NaN/缺省 → def;负数真值保留,
// 由 clamp 兜(FE 只发整数字串,小数形态不值得复刻)。
func jsNumOr(s string, def int) int {
	if s == "" {
		return def // Number('')=0 → 0||def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def // NaN||def
	}
	if n == 0 {
		return def
	}
	return n
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func usersList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		tier := strings.TrimSpace(r.URL.Query().Get("tier"))
		limit := clampInt(jsNumOr(r.URL.Query().Get("limit"), 50), 1, 200)
		offset := max(0, jsNumOr(r.URL.Query().Get("offset"), 0))

		where := []string{}
		params := []any{}
		if q != "" {
			params = append(params, "%"+strings.ToLower(q)+"%")
			where = append(where, fmt.Sprintf("(LOWER(u.email) LIKE $%d OR LOWER(u.display_name) LIKE $%d)",
				len(params), len(params)))
		}
		if tier == "free" || tier == "pro" || tier == "max" {
			params = append(params, tier)
			where = append(where, fmt.Sprintf("u.tier = $%d", len(params)))
		}
		whereSql := ""
		if len(where) > 0 {
			whereSql = "WHERE " + strings.Join(where, " AND ")
		}
		filterParams := append([]any{}, params...)
		params = append(params, limit, offset)
		rows, err := db.QueryContext(r.Context(),
			`SELECT `+userColsSQL+` FROM users u `+whereSql+`
			 ORDER BY u.created_at DESC
			 LIMIT $`+strconv.Itoa(len(params)-1)+` OFFSET $`+strconv.Itoa(len(params)), params...)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			u, err := scanUserRow(rows)
			if err != nil {
				continue
			}
			items = append(items, rowToUser(u))
		}
		var totalStr string
		_ = db.QueryRowContext(r.Context(),
			`SELECT COUNT(*)::text FROM users u `+whereSql, filterParams...).Scan(&totalStr)
		total, _ := strconv.Atoi(totalStr)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"items": items, "total": total, "limit": limit, "offset": offset,
		})
	}
}

func userGet(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		id := r.PathValue("id")
		u, err := scanUserRow(db.QueryRowContext(r.Context(),
			`SELECT `+userColsSQL+` FROM users u WHERE u.id = $1`, id))
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		cRows, err := db.QueryContext(r.Context(),
			`SELECT c.id, c.name, c.slug, cm.role, c.created_at::text,
			        (SELECT COUNT(*)::int FROM participants p
			          WHERE p.company_id = c.id AND p.kind = 'agent' AND p.departed_at IS NULL) AS agent_count
			   FROM company_members cm
			   JOIN companies c ON c.id = cm.company_id
			  WHERE cm.user_id = $1
			  ORDER BY cm.joined_at ASC`, id)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		defer cRows.Close()
		companies := []map[string]any{}
		for cRows.Next() {
			var cid, name, slug, createdAt string
			var role sql.NullString
			var agentCount int
			if cRows.Scan(&cid, &name, &slug, &role, &createdAt, &agentCount) != nil {
				continue
			}
			companies = append(companies, map[string]any{
				"id": cid, "name": name, "slug": slug, "role": nullStrAny(role),
				"createdAt": createdAt, "agentCount": agentCount,
			})
		}
		out := rowToUser(u)
		out["companies"] = companies
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// auditAsync:fire-and-forget 审计行(TS audit();写挂不倒滚安全动作)。
func auditAsync(db *sql.DB, kind, actorID, detailJSON string) {
	go func() {
		_, _ = db.Exec(
			`INSERT INTO audit_events (user_id, company_id, kind, detail)
			   VALUES ($1, NULL, $2, $3::jsonb)`, actorID, kind, detailJSON)
	}()
}

// suspendUser:置位 + 同事务清全部 session(无"已置位但旧 token 仍活"窗
// 口)+ 事后审计。已封 → 409,不存在 → 404(TS 用零行后回读区分)。
func suspendUser(w http.ResponseWriter, r *http.Request, db *sql.DB, userID, adminID string, reason any) bool {
	if userID == adminID {
		httpx.WriteError(w, http.StatusConflict, "cannot suspend yourself")
		return false
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return false
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(),
		`UPDATE users SET suspended_at = NOW(), suspension_reason = $2, suspended_by = $3
		  WHERE id = $1 AND suspended_at IS NULL`, userID, reason, adminID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		_ = tx.QueryRowContext(r.Context(), `SELECT 1 FROM users WHERE id = $1`, userID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
			return false
		}
		httpx.WriteError(w, http.StatusConflict, "user is already suspended")
		return false
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return false
	}
	if err := tx.Commit(); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return false
	}
	detail := `{"targetUserId":` + mustJSON(userID)
	if s, isStr := reason.(string); isStr && s != "" {
		detail += `,"reason":` + mustJSON(s)
	}
	detail += `}`
	auditAsync(db, "user_suspend", adminID, detail)
	return true
}

// unsuspendUser:复位;未封禁的活跃号是 no-op(不写审计)。
func unsuspendUser(w http.ResponseWriter, r *http.Request, db *sql.DB, userID, adminID string) bool {
	res, err := db.ExecContext(r.Context(),
		`UPDATE users SET suspended_at = NULL, suspension_reason = NULL, suspended_by = NULL
		  WHERE id = $1 AND suspended_at IS NOT NULL`, userID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		_ = db.QueryRowContext(r.Context(), `SELECT 1 FROM users WHERE id = $1`, userID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
			return false
		}
		return true
	}
	auditAsync(db, "user_unsuspend", adminID, `{"targetUserId":`+mustJSON(userID)+`}`)
	return true
}

// userPatch:字段各自独立可选(TS patch handler 顺序:tier → isAdmin →
// suspended),全走完回读单行。sub2api 镜像(#109 延后)no-op。
func userPatch(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		body := map[string]json.RawMessage{}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
			httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		strOf := func(k string) (string, bool) {
			raw, present := body[k]
			if !present || string(raw) == "null" {
				return "", false
			}
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return "", false
			}
			return s, true
		}
		boolOf := func(k string) (bool, bool) {
			raw, present := body[k]
			if !present || string(raw) == "null" {
				return false, false
			}
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				return false, false
			}
			return b, true
		}

		if t, has := strOf("tier"); has {
			if t != "free" && t != "pro" && t != "max" {
				httpx.WriteError(w, http.StatusBadRequest, "invalid tier")
				return
			}
			// 手动改级覆盖移动端试用戳(pro_trial_expires_at 清掉,防
			// 试用清扫工把真升级又降回去——TS changeUserTier 同款)。
			res, err := db.ExecContext(r.Context(),
				`UPDATE users SET tier = $2, pro_trial_expires_at = NULL WHERE id = $1`, id, t)
			if err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				httpx.WriteError(w, http.StatusNotFound, "user not found")
				return
			}
		}

		if b, has := boolOf("isAdmin"); has {
			if id == adminID && !b {
				httpx.WriteError(w, http.StatusConflict, "cannot demote yourself")
				return
			}
			res, err := db.ExecContext(r.Context(),
				`UPDATE users SET is_admin = $2 WHERE id = $1`, id, b)
			if err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				httpx.WriteError(w, http.StatusNotFound, "user not found")
				return
			}
		}

		if b, has := boolOf("suspended"); has {
			var reason any
			if b {
				if s, isStr := strOf("suspensionReason"); isStr {
					s = httpx.UTF16Cap(strings.TrimSpace(s), 500)
					if s != "" {
						reason = s
					}
				}
				if !suspendUser(w, r, db, id, adminID, reason) {
					return
				}
			} else {
				if !unsuspendUser(w, r, db, id, adminID) {
					return
				}
			}
		}

		u, err := scanUserRow(db.QueryRowContext(r.Context(),
			`SELECT `+userColsSQL+` FROM users u WHERE u.id = $1`, id))
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, rowToUser(u))
	}
}

/* ───────────── waitlist ───────────── */

type waitlistEntryDb struct {
	id          string
	provider    string
	providerID  string
	email       string
	displayName string
	avatarURL   sql.NullString
	status      string
	note        sql.NullString
	requestedAt string
	decidedAt   sql.NullString
	decidedBy   sql.NullString
}

func rowToWaitlist(e waitlistEntryDb) map[string]any {
	avatar := any(nil)
	if e.avatarURL.Valid && e.avatarURL.String != "" {
		avatar = e.avatarURL.String
	} else {
		avatar = gravatarURL(e.email) // 队列行总有东西可渲染
	}
	return map[string]any{
		"id": e.id, "provider": e.provider, "providerId": e.providerID,
		"email": e.email, "displayName": e.displayName, "avatarUrl": avatar,
		"status": e.status, "note": nullStrAny(e.note), "requestedAt": e.requestedAt,
		"decidedAt": nullStrAny(e.decidedAt), "decidedBy": nullStrAny(e.decidedBy),
	}
}

func waitlistList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		q := r.URL.Query()
		status := q.Get("status")
		switch status {
		case "pending", "approved", "rejected":
		default:
			status = ""
		}
		search := strings.ToLower(strings.TrimSpace(q.Get("q")))
		limit := clampInt(jsNumOr(q.Get("limit"), 50), 1, 200)
		offset := max(0, jsNumOr(q.Get("offset"), 0))

		where := []string{}
		params := []any{}
		if status != "" {
			params = append(params, status)
			where = append(where, fmt.Sprintf("status = $%d", len(params)))
		}
		if search != "" {
			// TS listWaitlist:email/display_name/provider/provider_id/note
			// 五列 LOWER LIKE(参数同一占位)。
			params = append(params, "%"+search+"%")
			n := len(params)
			where = append(where, fmt.Sprintf(`(
		      LOWER(email) LIKE $%d
		      OR LOWER(display_name) LIKE $%d
		      OR LOWER(provider) LIKE $%d
		      OR LOWER(provider_id) LIKE $%d
		      OR LOWER(COALESCE(note, '')) LIKE $%d
		    )`, n, n, n, n, n))
		}
		whereSql := ""
		if len(where) > 0 {
			whereSql = "WHERE " + strings.Join(where, " AND ")
		}
		filterParams := append([]any{}, params...)
		var totalStr string
		_ = db.QueryRowContext(r.Context(),
			`SELECT COUNT(*)::text FROM waitlist `+whereSql, filterParams...).Scan(&totalStr)
		params = append(params, limit, offset)
		rows, err := db.QueryContext(r.Context(),
			`SELECT id, provider, provider_id, email, display_name, avatar_url, status, note,
			        requested_at::text, decided_at::text, decided_by
			   FROM waitlist `+whereSql+`
			  ORDER BY requested_at DESC
			  LIMIT $`+strconv.Itoa(len(params)-1)+` OFFSET $`+strconv.Itoa(len(params)), params...)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var e waitlistEntryDb
			if rows.Scan(&e.id, &e.provider, &e.providerID, &e.email, &e.displayName,
				&e.avatarURL, &e.status, &e.note, &e.requestedAt, &e.decidedAt, &e.decidedBy) != nil {
				continue
			}
			items = append(items, rowToWaitlist(e))
		}
		total, _ := strconv.Atoi(totalStr)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"items": items, "total": total, "limit": limit, "offset": offset,
		})
	}
}

func waitlistApprove(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		userID, companyID, err := core.ApproveWaitlist(r.Context(), db, r.PathValue("id"), adminID)
		if err != nil {
			var conflict *core.WaitlistConflictError
			var emailExists *core.WaitlistEmailExistsError
			switch {
			case errors.Is(err, core.ErrWaitlistNotFound):
				httpx.WriteError(w, http.StatusNotFound, "waitlist entry not found")
			case errors.As(err, &conflict):
				httpx.WriteError(w, http.StatusConflict, conflict.Error())
			case errors.As(err, &emailExists):
				httpx.WriteError(w, http.StatusConflict, emailExists.Error())
			default:
				httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			}
			return
		}
		var companyAny any
		if companyID != nil {
			companyAny = *companyID
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"userId": userID, "companyId": companyAny})
	}
}

func waitlistReject(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		body := map[string]json.RawMessage{}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && err != io.EOF {
			httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		var note any
		if raw, present := body["note"]; present && string(raw) != "null" {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				note = s
			}
		}
		res, err := db.ExecContext(r.Context(),
			`UPDATE waitlist SET status = 'rejected', decided_at = NOW(), decided_by = $2, note = $3
			  WHERE id = $1 AND status = 'pending'`, r.PathValue("id"), adminID, note)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.WriteError(w, http.StatusNotFound, "no pending waitlist entry")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

/* ───────────── stats ───────────── */

func stats(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		var usersTotal, admins, free, pro, maxTier int
		if err := db.QueryRowContext(r.Context(), `
			SELECT COUNT(*)::int,
			       COUNT(*) FILTER (WHERE is_admin)::int,
			       COUNT(*) FILTER (WHERE tier = 'free')::int,
			       COUNT(*) FILTER (WHERE tier = 'pro')::int,
			       COUNT(*) FILTER (WHERE tier = 'max')::int
			  FROM users`).
			Scan(&usersTotal, &admins, &free, &pro, &maxTier); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		var wlPending, wlApproved, wlRejected int
		if err := db.QueryRowContext(r.Context(), `
			SELECT COUNT(*) FILTER (WHERE status = 'pending')::int,
			       COUNT(*) FILTER (WHERE status = 'approved')::int,
			       COUNT(*) FILTER (WHERE status = 'rejected')::int
			  FROM waitlist`).
			Scan(&wlPending, &wlApproved, &wlRejected); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		var companies int
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(*)::int FROM companies`).Scan(&companies); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		var agents int
		if err := db.QueryRowContext(r.Context(),
			`SELECT COUNT(*)::int FROM participants WHERE kind = 'agent' AND departed_at IS NULL`).Scan(&agents); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"users": map[string]any{
				"total":  usersTotal,
				"admins": admins,
				"tiers":  map[string]any{"free": free, "pro": pro, "max": maxTier},
			},
			"waitlist": map[string]any{
				"pending": wlPending, "approved": wlApproved, "rejected": wlRejected,
			},
			"companies": companies,
			"agents":    agents,
		})
	}
}
