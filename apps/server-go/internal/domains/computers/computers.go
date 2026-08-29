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
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/computers"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
)

// Server:computers tag 的域实现(#187;方法体原样搬运)。
type Server struct{ DB *sql.DB }

var _ contract.ServerInterface = (*Server)(nil)

// Mount:computers tag 7 路由走契约生成物;assign/runtimeToken 属
// agents tag(本包代挂),待 agents 批次收编后并入。
func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
	mux.HandleFunc("POST /api/agents/{id}/computer", assign(db))
	mux.HandleFunc("POST /api/agents/{id}/runtime-token", runtimeToken(db))
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

func (s *Server) GetComputers(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, reg.ListComputers(r.Context(), s.DB, companyID))
}

func (s *Server) RequestPairingCode(w http.ResponseWriter, r *http.Request) {
	uid, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	code, _, err := reg.IssuePairingCode(r.Context(), s.DB, companyID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "query failed")
		return
	}
	_ = uid
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"code": code, "expiresInSeconds": nil})
}

func (s *Server) RepairComputer(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	code, ok2 := reg.IssueRepairCode(r.Context(), s.DB, companyID, id)
	if !ok2 {
		httpx.WriteError(w, http.StatusNotFound, "computer not found or not re-pairable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"code": code, "expiresInSeconds": nil})
}

func (s *Server) DeleteComputer(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	if !reg.RevokeComputer(r.Context(), s.DB, id, companyID) {
		httpx.WriteError(w, http.StatusNotFound, "computer not found or not revocable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
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

func (s *Server) PairComputer(w http.ResponseWriter, r *http.Request) {
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
	paired, err := reg.PairComputer(r.Context(), s.DB, code, body.HostName, body.Engines, body.Version, body.Supervised, true)
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
	_, _ = s.DB.ExecContext(r.Context(), `
		UPDATE participants p
		   SET computer_id = $1, engine = COALESCE(NULLIF(p.engine, 'managed'), $2)
		 WHERE p.company_id = $3 AND p.kind = 'agent'
		   AND NOT EXISTS (
		     SELECT 1 FROM computers c
		      WHERE c.id = p.computer_id AND c.kind <> 'cloud' AND c.revoked_at IS NULL)`,
		paired.ComputerID, engine, paired.CompanyID)
	onboard.OnboardStarterAgents(r.Context(), s.DB, paired.CompanyID, &paired.ComputerID, &engine)
	// 名册就绪后再广播上线(desktop 的 onboarding 门会立即重载名册)。
	reg.AnnounceComputerOnline(r.Context(), paired.ComputerID, paired.CompanyID)
	httpx.WriteJSON(w, http.StatusOK, paired)
}

func (s *Server) ListAgentsForComputer(w http.ResponseWriter, r *http.Request) {
	computerID, _, ok := requireDevice(w, r, s.DB)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, reg.ListAgentsForComputer(r.Context(), s.DB, computerID))
}

func (s *Server) HeartbeatComputer(w http.ResponseWriter, r *http.Request) {
	computerID, _, ok := requireDevice(w, r, s.DB)
	if !ok {
		return
	}
	var body struct {
		Version    string `json:"version"`
		Supervised *bool  `json:"supervised"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	reg.HeartbeatComputer(r.Context(), s.DB, computerID, body.Version, body.Supervised)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
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
