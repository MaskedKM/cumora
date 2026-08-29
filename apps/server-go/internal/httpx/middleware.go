// httpx/authn —— 请求级认证与租户解析中间件(#52),对齐 router.ts 的
// authMiddleware + requireCompany + requireCompanyRole。
package httpx

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
)

type ctxKey int

const (
	ctxUserID ctxKey = iota
	ctxCompanyID
)

// Authn 中间件:有令牌就解析并注入 uid(不拒绝——由各 handler 决定,
// 与 baseline 的 opt-in requireAuth 语义一致)。
//
// 验收镜像盖章(#55 起):CUMORA_GO_FAKE_AUTH=1 时信任 x-test-user 头
// 直接注入 uid——对齐 TS 镜像脚手架的伪造 auth 盖章(见
// server/src/__integration__/_helpers.ts 的 buildApiTestApp),使
// MIRROR_BASE 双跑能测认证面。仅限本地双跑,生产不得开。
func Authn(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if os.Getenv("CUMORA_GO_FAKE_AUTH") == "1" {
				if uid := r.Header.Get("x-test-user"); uid != "" {
					r = r.WithContext(context.WithValue(r.Context(), ctxUserID, uid))
					next.ServeHTTP(w, r)
					return
				}
			}
			token := authn.Bearer(r.Header.Get("Authorization"), r.Header.Get("x-session-token"))
			if token != "" {
				if uid, ok := authn.ResolveSession(r.Context(), db, token); ok {
					r = r.WithContext(context.WithValue(r.Context(), ctxUserID, uid))
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteDeadline:#136 非流式面的写期限兜底——对响应设绝对写期限,
// 挂起客户端(TCP 零窗口)不再无限占用 handler。只挂 /api 链:SSE
// (/runtime/wake-stream)与 WS(/ws)是长响应,绝不进本链;全局
// http.Server.WriteTimeout 同理保持 0。handler 可用
// http.NewResponseController 再顺延;底层不支持时静默降级。
func WriteDeadline(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
			next.ServeHTTP(w, r)
		})
	}
}

// UserID 取认证 uid;ok=false 即未认证(handler 自行 401)。
func UserID(r *http.Request) (string, bool) {
	uid, ok := r.Context().Value(ctxUserID).(string)
	return uid, ok
}

// RequireAuth 未认证即 401 {error}。
func RequireAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, ok := UserID(r)
	if !ok {
		WriteError(w, http.StatusUnauthorized, "authentication required")
		return "", false
	}
	return uid, true
}

// ResolveCompany 解析租户:显式 x-company-id(校验成员资格)优先,
// 否则取最早加入的成员公司。对齐 requireCompany 语义与错误码。
func ResolveCompany(w http.ResponseWriter, r *http.Request, db *sql.DB, uid string) (string, bool) {
	requested := r.Header.Get("x-company-id")
	if requested != "" {
		var exists bool
		if err := db.QueryRowContext(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM company_members WHERE company_id = $1 AND user_id = $2)`,
			requested, uid).Scan(&exists); err == nil && exists {
			return requested, true
		}
		WriteError(w, http.StatusForbidden, "not a member of this company")
		return "", false
	}
	var companyID string
	err := db.QueryRowContext(r.Context(),
		`SELECT company_id FROM company_members WHERE user_id = $1 ORDER BY joined_at ASC LIMIT 1`,
		uid).Scan(&companyID)
	if err != nil {
		WriteError(w, http.StatusNotFound, "no company memberships — create or join one")
		return "", false
	}
	return companyID, true
}

// RequireCompany 认证 + 租户解析一步到位(#141 合一前是 9 个域包的
// 逐字节相同拷贝 + uploads 单返回变体;失败路径已写响应,调用方直接 return)。
func RequireCompany(w http.ResponseWriter, r *http.Request, db *sql.DB) (uid, companyID string, ok bool) {
	uid, ok = RequireAuth(w, r)
	if !ok {
		return "", "", false
	}
	companyID, ok = ResolveCompany(w, r, db, uid)
	if !ok {
		return "", "", false
	}
	return uid, companyID, true
}

// RequireCompanyID 同 RequireCompany,但调用方只需要租户(uploads 形态)。
func RequireCompanyID(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	_, companyID, ok := RequireCompany(w, r, db)
	return companyID, ok
}

// ResolveCompanyRole 在 ResolveCompany 之上再校验 owner/admin。
func ResolveCompanyRole(w http.ResponseWriter, r *http.Request, db *sql.DB, uid string) (companyID string, ok bool) {
	companyID, ok = ResolveCompany(w, r, db, uid)
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
		WriteError(w, http.StatusForbidden, "this action requires an owner or admin of the team")
		return "", false
	}
	return companyID, true
}

// CompanyID 取已解析注入的租户(域 handler 内用)。
func CompanyID(r *http.Request) (string, bool) {
	cid, ok := r.Context().Value(ctxCompanyID).(string)
	return cid, ok
}

func WithCompanyID(r *http.Request, companyID string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxCompanyID, companyID))
}
