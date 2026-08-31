// httpx/authn —— 请求级认证与租户解析中间件(#52),对齐 router.ts 的
// authMiddleware + requireCompany + requireCompanyRole。
package httpx

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
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
// 已退役 TS server 的 __integration__/_helpers.ts 的 buildApiTestApp),使
// MIRROR_BASE 双跑能测认证面。仅限本地双跑,生产不得开。
func Authn(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if config.FakeAuth() == "1" {
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

// Recover:/api 链 panic 兜底(#214)。此前域 handler panic 走 net/http
// 默认路径——每连接单独 goroutine 里未被捕获的 panic 会让连接被服务端
// 直接切断,客户端拿到的是连接重置而非可辨识的 500。对齐 runtime 侧
// withAgent 的既有 recover 先例(runtime/routes.go 的 auth 中间件):
// slog.Error 带 method/path/panic + 500 JSON {"error":"internal error"}
// (纯逻辑分支,无 error 对象——与该先例同款豁免)。放链最外层,随后
// 续 WriteDeadline → Authn;panic 响应已完成 handler 生命期,不与写期限
// 交互。header 已写出的中途 panic 下 WriteHeader 会打 superfluous 告警,
// 与 runtime 先例同样接受(TS 侧由 errorHandler 兜 try/catch 面,等价)。
func Recover() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("[api] handler panicked", "method", r.Method, "path", r.URL.Path, "panic", rec)
					// 500 豁免(#214):panic recover 面无 error 对象;固定
					// 文案对齐 runtime 先例与 TS withAgent 的 catch 形状。
					WriteError(w, http.StatusInternalServerError, "internal error")
				}
			}()
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
