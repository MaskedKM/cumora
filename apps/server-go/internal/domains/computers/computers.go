// domains/computers —— computers HTTP 面(#60):9 路由。用户面(公司/
// 角色门)与 daemon 面(设备令牌即凭据,无用户会话)。/computers/pair
// 兑换后做搁浅 agent 收养 + starter 团队种子 + 延迟的上线广播。
package computers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	reg "github.com/MaskedKM/cumora/apps/server-go/internal/computers"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
)

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/computers", list(db))
	mux.HandleFunc("POST /api/computers", create(db))
	mux.HandleFunc("POST /api/computers/{id}/repair", repair(db))
	mux.HandleFunc("DELETE /api/computers/{id}", revoke(db))
	mux.HandleFunc("POST /api/agents/{id}/computer", assign(db))
	mux.HandleFunc("POST /api/computers/pair", pair(db))
	mux.HandleFunc("GET /api/computers/me/agents", myAgents(db))
	mux.HandleFunc("POST /api/computers/heartbeat", heartbeat(db))
	mux.HandleFunc("POST /api/agents/{id}/runtime-token", runtimeToken(db))
}

func requireCompany(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", "", false
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return "", "", false
	}
	return uid, companyID, true
}

func requireRole(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", "", false
	}
	companyID, ok := httpx.ResolveCompanyRole(w, r, db, uid)
	if !ok {
		return "", "", false
	}
	return uid, companyID, true
}

// requireDevice 对齐 router.ts:Bearer 设备令牌即凭据,401 拒已吊销。
func requireDevice(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, string, bool) {
	auth := r.Header.Get("Authorization")
	token := ""
	if strings.HasPrefix(auth, "Bearer ") {
		token = strings.TrimSpace(auth[7:])
	}
	computerID, companyID, ok := reg.ResolveDevice(r.Context(), db, token)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "invalid or revoked device token")
		return "", "", false
	}
	return computerID, companyID, true
}

func list(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := requireCompany(w, r, db)
		if !ok {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, reg.ListComputers(r.Context(), db, companyID))
	}
}

func create(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		code, _, err := reg.IssuePairingCode(r.Context(), db, companyID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "query failed")
			return
		}
		_ = uid
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"code": code, "expiresInSeconds": nil})
	}
}

func repair(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		code, ok2 := reg.IssueRepairCode(r.Context(), db, companyID, r.PathValue("id"))
		if !ok2 {
			httpx.WriteError(w, http.StatusNotFound, "computer not found or not re-pairable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"code": code, "expiresInSeconds": nil})
	}
}

func revoke(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		if !reg.RevokeComputer(r.Context(), db, r.PathValue("id"), companyID) {
			httpx.WriteError(w, http.StatusNotFound, "computer not found or not revocable")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func assign(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, companyID, ok := requireRole(w, r, db)
		if !ok {
			return
		}
		var body struct {
			ComputerID string  `json:"computerId"`
			Engine     *string `json:"engine"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		computerID := strings.TrimSpace(body.ComputerID)
		if computerID == "" {
			httpx.WriteError(w, http.StatusBadRequest, "computerId required")
			return
		}
		var engine string
		if body.Engine != nil {
			engine = *body.Engine
		}
		out, ok2 := reg.AssignAgentToComputer(r.Context(), db, r.PathValue("id"), companyID, computerID, engine)
		if !ok2 {
			httpx.WriteError(w, http.StatusBadRequest, "invalid computer, agent, or engine for this company")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "kind": out["kind"], "engine": out["engine"]})
	}
}

func pair(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Code       string   `json:"code"`
			Engines    []string `json:"engines"`
			HostName   string   `json:"hostName"`
			Version    string   `json:"version"`
			Supervised *bool    `json:"supervised"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		code := strings.TrimSpace(body.Code)
		if code == "" {
			httpx.WriteError(w, http.StatusBadRequest, "code required")
			return
		}
		paired, err := reg.PairComputer(r.Context(), db, code, body.HostName, body.Engines, body.Version, body.Supervised, true)
		if err != nil || paired == nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid pairing token")
			return
		}
		// 搁浅收养:legacy/未分配 agent 落到新机;已住真机的 agent 不动
		// (保用户逐 agent 选的引擎)。engines[0] 为用户所选默认引擎。
		engine := "claude"
		if len(body.Engines) > 0 {
			switch body.Engines[0] {
			case "claude", "codex", "grok", "cursor", "zcode":
				engine = body.Engines[0]
			}
		}
		_, _ = db.ExecContext(r.Context(), `
			UPDATE participants p
			   SET computer_id = $1, engine = COALESCE(NULLIF(p.engine, 'managed'), $2)
			 WHERE p.company_id = $3 AND p.kind = 'agent'
			   AND NOT EXISTS (
			     SELECT 1 FROM computers c
			      WHERE c.id = p.computer_id AND c.kind <> 'cloud' AND c.revoked_at IS NULL)`,
			paired.ComputerID, engine, paired.CompanyID)
		onboard.OnboardStarterAgents(r.Context(), db, paired.CompanyID, &paired.ComputerID, &engine)
		// 名册就绪后再广播上线(desktop 的 onboarding 门会立即重载名册)。
		reg.AnnounceComputerOnline(r.Context(), paired.ComputerID, paired.CompanyID)
		httpx.WriteJSON(w, http.StatusOK, paired)
	}
}

func myAgents(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		computerID, _, ok := requireDevice(w, r, db)
		if !ok {
			return
		}
		httpx.WriteJSON(w, http.StatusOK, reg.ListAgentsForComputer(r.Context(), db, computerID))
	}
}

func heartbeat(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		computerID, _, ok := requireDevice(w, r, db)
		if !ok {
			return
		}
		var body struct {
			Version    string `json:"version"`
			Supervised *bool  `json:"supervised"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reg.HeartbeatComputer(r.Context(), db, computerID, body.Version, body.Supervised)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func runtimeToken(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		computerID, _, ok := requireDevice(w, r, db)
		if !ok {
			return
		}
		token, ttl, ok2 := reg.MintAgentRuntimeToken(r.Context(), db, computerID, r.PathValue("id"))
		if !ok2 {
			httpx.WriteError(w, http.StatusForbidden, "agent not assigned to this computer")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": token, "expiresInSeconds": ttl})
	}
}
