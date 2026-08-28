// domains/admin/users —— #124(#117-e):用户管理子面。GET 列表(分页
// +搜索 +tier 过滤)/GET 详情(带区清单与各区 agent 数)/PATCH(逐字段
// 改 tier / admin 位 / 停用态)。逐段对齐 api/admin-router.ts 150–328
// 与 admin.ts 636–772(suspend/unsuspend/changeUserTier)。
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

type adminUserRow struct {
	id               string
	email            string
	displayName      string
	avatarURL        sql.NullString
	tier             string
	isAdmin          bool
	sub2apiUserID    sql.NullInt64
	createdAt        time.Time
	lastLoginAt      sql.NullTime
	companyCount     int
	suspendedAt      sql.NullTime
	suspensionReason sql.NullString
	suspendedBy      sql.NullString
}

const adminUserCols = `u.id, u.email, u.display_name, u.avatar_url, u.tier, u.is_admin, u.sub2api_user_id,
            u.created_at, u.last_login_at,
            u.suspended_at, u.suspension_reason, u.suspended_by,
            (SELECT COUNT(*)::int FROM company_members cm WHERE cm.user_id = u.id) AS company_count`

func scanAdminUserRow(scanner interface{ Scan(...any) error }) (*adminUserRow, error) {
	var r adminUserRow
	err := scanner.Scan(&r.id, &r.email, &r.displayName, &r.avatarURL, &r.tier, &r.isAdmin, &r.sub2apiUserID,
		&r.createdAt, &r.lastLoginAt,
		&r.suspendedAt, &r.suspensionReason, &r.suspendedBy, &r.companyCount)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// toWireUser:rowToUser —— gravatar 兜底让管理行恒有头像;停用四件套
// (派生布尔 + 时间戳/理由/操作者)供详情抽屉。
func (r *adminUserRow) toWire() map[string]any {
	var sub2api any
	if r.sub2apiUserID.Valid {
		sub2api = r.sub2apiUserID.Int64
	}
	return map[string]any{
		"id": r.id, "email": r.email, "name": r.displayName,
		"avatarUrl":         avatarOrGravatar(r.avatarURL, r.email),
		"tier":              r.tier,
		"isAdmin":           r.isAdmin,
		"sub2apiUserId":     sub2api,
		"createdAt":         isoTime(r.createdAt),
		"lastLoginAt":       nullTimeAny(r.lastLoginAt),
		"companyCount":      r.companyCount,
		"suspended":         r.suspendedAt.Valid,
		"suspendedAt":       nullTimeAny(r.suspendedAt),
		"suspensionReason":  nullStrAny(r.suspensionReason),
		"suspendedBy":       nullStrAny(r.suspendedBy),
	}
}

// numOrDefault:TS Number(x) || d —— 空/非数/NaN/0 均落默认(0 falsy)。
func numOrDefault(raw string, def float64) float64 {
	if raw == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%g", &f); err != nil || math.IsNaN(f) || f == 0 {
		return def
	}
	return f
}

func usersList(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		tier := strings.TrimSpace(r.URL.Query().Get("tier"))
		limit := int(math.Min(200, math.Max(1, numOrDefault(r.URL.Query().Get("limit"), 50))))
		offset := int(math.Max(0, numOrDefault(r.URL.Query().Get("offset"), 0)))

		where := []string{}
		var params []any
		if q != "" {
			params = append(params, "%"+strings.ToLower(q)+"%")
			where = append(where, fmt.Sprintf(`(LOWER(u.email) LIKE $%d OR LOWER(u.display_name) LIKE $%d)`, len(params), len(params)))
		}
		if tier == "free" || tier == "pro" || tier == "max" {
			params = append(params, tier)
			where = append(where, fmt.Sprintf(`u.tier = $%d`, len(params)))
		}
		whereSql := ""
		if len(where) > 0 {
			whereSql = "WHERE " + strings.Join(where, " AND ")
		}
		params = append(params, limit, offset)
		rows, err := db.QueryContext(r.Context(), fmt.Sprintf(`
			SELECT %s FROM users u %s ORDER BY u.created_at DESC LIMIT $%d OFFSET $%d`,
			adminUserCols, whereSql, len(params)-1, len(params)), params...)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			u, err := scanAdminUserRow(rows)
			if err != nil {
				continue
			}
			items = append(items, u.toWire())
		}
		var total int
		countParams := params[:len(params)-2]
		_ = db.QueryRowContext(r.Context(),
			fmt.Sprintf(`SELECT COUNT(*)::int FROM users u %s`, whereSql), countParams...).Scan(&total)
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
		u, err := scanAdminUserRow(db.QueryRowContext(r.Context(),
			fmt.Sprintf(`SELECT %s FROM users u WHERE u.id = $1`, adminUserCols), id))
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
			return
		}
		rows, err := db.QueryContext(r.Context(), `
			SELECT c.id, c.name, c.slug, cm.role, c.created_at,
			       (SELECT COUNT(*)::int FROM participants p
			         WHERE p.company_id = c.id AND p.kind = 'agent' AND p.departed_at IS NULL) AS agent_count
			  FROM company_members cm JOIN companies c ON c.id = cm.company_id
			 WHERE cm.user_id = $1 ORDER BY cm.joined_at ASC`, id)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		companies := []map[string]any{}
		for rows.Next() {
			var cid, name, slug, role string
			var createdAt time.Time
			var agentCount int
			if rows.Scan(&cid, &name, &slug, &role, &createdAt, &agentCount) == nil {
				companies = append(companies, map[string]any{
					"id": cid, "name": name, "slug": slug, "role": role,
					"createdAt": isoTime(createdAt), "agentCount": agentCount,
				})
			}
		}
		out := u.toWire()
		out["companies"] = companies
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// userPatch:三个字段独立可选,逐字段生效(管理 UI 每个开关单独发)。
func userPatch(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := requireAdmin(w, r, db)
		if !ok {
			return
		}
		id := r.PathValue("id")
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)

		// TS typeof === 'string' 门:字段是字符串就必校验(空串也 400)。
		if raw, has := body["tier"]; has {
			var tier string
			if json.Unmarshal(raw, &tier) == nil {
				if tier != "free" && tier != "pro" && tier != "max" {
					httpx.WriteError(w, http.StatusBadRequest, "invalid tier")
					return
				}
				if !adminChangeTier(r.Context(), db, id, tier) {
					httpx.WriteError(w, http.StatusNotFound, "user not found")
					return
				}
			}
		}
		if raw, has := body["isAdmin"]; has {
			var isAdmin bool
			if json.Unmarshal(raw, &isAdmin) == nil {
				// 拒绝自降 —— 唯一操作员把自己锁在面板外。
				if id == adminID && !isAdmin {
					httpx.WriteError(w, http.StatusConflict, "cannot demote yourself")
					return
				}
				res, err := db.ExecContext(r.Context(),
					`UPDATE users SET is_admin = $2 WHERE id = $1`, id, isAdmin)
				if err != nil {
					httpx.WriteError(w, http.StatusInternalServerError, err.Error())
					return
				}
				if n, _ := res.RowsAffected(); n == 0 {
					httpx.WriteError(w, http.StatusNotFound, "user not found")
					return
				}
			}
		}
		if raw, has := body["suspended"]; has {
			var suspended bool
			if json.Unmarshal(raw, &suspended) == nil {
				if suspended {
					// suspensionReason:trim + 截 500,空串归 null(TS 同款)。
					var reason sql.NullString
					var rawReason string
					if rr, hasR := body["suspensionReason"]; hasR && json.Unmarshal(rr, &rawReason) == nil {
						if t := strings.TrimSpace(rawReason); t != "" {
							if len(t) > 500 {
								t = t[:500]
							}
							reason = sql.NullString{String: t, Valid: true}
						}
					}
					if !adminSuspendUser(w, r, db, id, adminID, reason) {
						return // 细分错误已写响应
					}
				} else {
					if !adminUnsuspendUser(w, r, db, id, adminID) {
						return
					}
				}
			}
		}

		u, err := scanAdminUserRow(db.QueryRowContext(r.Context(),
			fmt.Sprintf(`SELECT %s FROM users u WHERE u.id = $1`, adminUserCols), id))
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, u.toWire())
	}
}

// adminChangeTier:changeUserTier —— sub2api 未配置(部署实态,#109 延后
// 同款 no-op 门),只落库;手动改级覆盖移动端试用戳,防到期自动降级
// 误伤真升级。
func adminChangeTier(ctx context.Context, db *sql.DB, userID, tier string) bool {
	res, err := db.ExecContext(ctx,
		`UPDATE users SET tier = $2, pro_trial_expires_at = NULL WHERE id = $1`, userID, tier)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// adminSuspendUser:suspendUser —— 盖停用戳 + 同事务清 session(无
// "已标停用但旧 token 仍活"窗口);幂等冲突 409;审计行事务外补记。
// 返回 false 时已把细分错误(404/409)写进响应。
func adminSuspendUser(w http.ResponseWriter, r *http.Request, db *sql.DB, userID, adminID string, reason sql.NullString) bool {
	if userID == adminID {
		httpx.WriteError(w, http.StatusConflict, "cannot suspend yourself")
		return false
	}
	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(), `
		UPDATE users SET suspended_at = NOW(), suspension_reason = $2, suspended_by = $3
		 WHERE id = $1 AND suspended_at IS NULL`, userID, reason, adminID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 用户不存在 vs 已停用:补一刀读区分报错。
		var one int
		exists := tx.QueryRowContext(r.Context(), `SELECT 1 FROM users WHERE id = $1`, userID).Scan(&one) == nil
		_ = tx.Rollback()
		if !exists {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
		} else {
			httpx.WriteError(w, http.StatusConflict, "user is already suspended")
		}
		return false
	}
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if err := tx.Commit(); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	adminAudit(db, adminID, "user_suspend", map[string]any{"targetUserId": userID, "reason": nullStrAny(reason)})
	return true
}

// adminUnsuspendUser:unsuspendUser —— 反向;未停用视为 no-op(不审计)。
func adminUnsuspendUser(w http.ResponseWriter, r *http.Request, db *sql.DB, userID, adminID string) bool {
	res, err := db.ExecContext(r.Context(), `
		UPDATE users SET suspended_at = NULL, suspension_reason = NULL, suspended_by = NULL
		 WHERE id = $1 AND suspended_at IS NOT NULL`, userID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if db.QueryRowContext(r.Context(), `SELECT 1 FROM users WHERE id = $1`, userID).Scan(&one) != nil {
			httpx.WriteError(w, http.StatusNotFound, "user not found")
			return false
		}
		return true // 未停用:幂等 no-op
	}
	adminAudit(db, adminID, "user_unsuspend", map[string]any{"targetUserId": userID})
	return true
}
