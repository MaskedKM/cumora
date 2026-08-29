// domains/admin/stats —— #124(#117-e):面板头四个快速计数(用户分层
// /等待名单三态/区数/在编 agent 数)。对齐 api/admin-router.ts 365–405。
package admin

import (
	"net/http"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func (s *Server) AdminStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r, s.DB); !ok {
		return
	}
	var uTotal, uAdmins, uFree, uPro, uMax int
	_ = s.DB.QueryRowContext(r.Context(), `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE is_admin),
		       COUNT(*) FILTER (WHERE tier = 'free'),
		       COUNT(*) FILTER (WHERE tier = 'pro'),
		       COUNT(*) FILTER (WHERE tier = 'max')
		  FROM users`).Scan(&uTotal, &uAdmins, &uFree, &uPro, &uMax)
	var wPending, wApproved, wRejected int
	_ = s.DB.QueryRowContext(r.Context(), `
		SELECT COUNT(*) FILTER (WHERE status = 'pending'),
		       COUNT(*) FILTER (WHERE status = 'approved'),
		       COUNT(*) FILTER (WHERE status = 'rejected')
		  FROM waitlist`).Scan(&wPending, &wApproved, &wRejected)
	var companies int
	_ = s.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM companies`).Scan(&companies)
	var agents int
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM participants WHERE kind = 'agent' AND departed_at IS NULL`).Scan(&agents)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"users": map[string]any{
			"total":  uTotal,
			"admins": uAdmins,
			"tiers":  map[string]int{"free": uFree, "pro": uPro, "max": uMax},
		},
		"waitlist": map[string]int{
			"pending": wPending, "approved": wApproved, "rejected": wRejected,
		},
		"companies": companies,
		"agents":    agents,
	})
}
