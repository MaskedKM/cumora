// domains/computers —— computers HTTP 面(#60):9 路由。用户面(公司/
// 角色门)与 daemon 面(设备令牌即凭据,无用户会话)。/computers/pair
// 兑换后做搁浅 agent 收养 + starter 团队种子 + 延迟的上线广播。
package computers

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"

	reg "github.com/MaskedKM/cumora/apps/server-go/internal/computers"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/computers"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/onboard"
	"github.com/MaskedKM/cumora/apps/server-go/internal/sched"
)

// Server:computers tag 的域实现(#187;方法体原样搬运)。RDB:#337
// workspace-report 对账文件已知态用(nil = 无缓存降级:只快照不判重)。
type Server struct {
	DB  *sql.DB
	RDB redis.UniversalClient
}

var _ contract.ServerInterface = (*Server)(nil)

// Mount:computers tag 7 路由走契约生成物;agents tag 的 assign/
// runtimeToken 已由 agents 域经 Server 方法委托收编(#187 批次 5)。
func Mount(mux *http.ServeMux, db *sql.DB, rdb redis.UniversalClient) {
	_ = contract.HandlerFromMux(&Server{DB: db, RDB: rdb}, mux)
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
		httpx.WriteInternalError(w, r, err)
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

// AssignAgentComputer:agents tag 跨包路由(agents 域 Server 一行委托
// 到此)—— 体为原 assign 闭包逐字上移,#187 批次 5。
func (s *Server) AssignAgentComputer(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := requireRole(w, r, s.DB)
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
	out, ok2 := reg.AssignAgentToComputer(r.Context(), s.DB, id, companyID, computerID, engine)
	if !ok2 {
		httpx.WriteError(w, http.StatusBadRequest, "invalid computer, agent, or engine for this company")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "kind": out["kind"], "engine": out["engine"]})
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

// MintAgentRuntimeToken:agents tag 跨包路由(agents 域委托)—— 体为
// 原 runtimeToken 闭包逐字上移,#187 批次 5。
func (s *Server) MintAgentRuntimeToken(w http.ResponseWriter, r *http.Request, id string) {
	computerID, _, ok := requireDevice(w, r, s.DB)
	if !ok {
		return
	}
	token, ttl, ok2 := reg.MintAgentRuntimeToken(r.Context(), s.DB, computerID, id)
	if !ok2 {
		httpx.WriteError(w, http.StatusForbidden, "agent not assigned to this computer")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": token, "expiresInSeconds": ttl})
}

// ListComputerSkills:#261 公司 Skills 分发清单——daemon 每同步周期拉一次,
// 以 bundle_hash 为增量键(哈希不变不重拉整包)。公司集合 = 本机托管
// agent 的公司去重; departed/换机的 agent 不再带出其公司(除非同公司
// 还有别的在管 agent)。
func (s *Server) ListComputerSkills(w http.ResponseWriter, r *http.Request) {
	computerID, _, ok := requireDevice(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT DISTINCT cs.company_id, cs.name, cs.description, cs.bundle_hash
		  FROM company_skills cs
		  JOIN participants p ON p.company_id = cs.company_id
		 WHERE p.computer_id = $1 AND p.kind = 'agent' AND p.departed_at IS NULL
		 ORDER BY cs.company_id, cs.name`, computerID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	type skillRef struct {
		CompanyID   string `json:"companyId"`
		Name        string `json:"name"`
		Description string `json:"description"`
		BundleHash  string `json:"bundleHash"`
	}
	out := []skillRef{}
	for rows.Next() {
		var ref skillRef
		if rows.Scan(&ref.CompanyID, &ref.Name, &ref.Description, &ref.BundleHash) == nil {
			out = append(out, ref)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// GetComputerSkillBundle:#261 按内容哈希取整包。哈希查询 + 本机公司域
// 存在性约束(设备只能拉它托管公司的包);同内容跨公司重复时任取一份
// ——内容寻址键保证两者字节相同。
func (s *Server) GetComputerSkillBundle(w http.ResponseWriter, r *http.Request, hash string) {
	computerID, _, ok := requireDevice(w, r, s.DB)
	if !ok {
		return
	}
	var name string
	var filesRaw json.RawMessage
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT cs.name, cs.files FROM company_skills cs
		 WHERE cs.bundle_hash = $1 AND EXISTS (
		     SELECT 1 FROM participants p
		      WHERE p.computer_id = $2 AND p.company_id = cs.company_id
		        AND p.kind = 'agent' AND p.departed_at IS NULL)
		 LIMIT 1`, hash, computerID).Scan(&name, &filesRaw)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "skill bundle not found")
		return
	}
	var files []struct {
		Path string `json:"path"`
		Body string `json:"body"`
	}
	_ = json.Unmarshal(filesRaw, &files)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"name": name, "files": files})
}

// ReportWorkspaceChanges:#337 daemon watcher 上报面 —— 挂载工作区文件
// 变更(去抖批量)。device token 即凭据;工作区必须属于本机同公司
// (跨租户上报 404)。处理走 sched.SyncWorkspaceFileState:对账已知态 →
// 变化项快照 → 广播 workspace.files_changed。
func (s *Server) ReportWorkspaceChanges(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := requireDevice(w, r, s.DB)
	if !ok {
		return
	}
	var payload struct {
		Items []struct {
			WorkspaceID string `json:"workspaceId"`
			Path        string `json:"path"`
		} `json:"items"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Items) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "items required")
		return
	}
	if len(payload.Items) > 500 {
		httpx.WriteError(w, http.StatusBadRequest, "too many items (max 500)")
		return
	}
	// 按 workspace 聚合(清单行可能跨区),区信息一次查全。
	grouped := map[string][]string{}
	for _, it := range payload.Items {
		if it.WorkspaceID != "" && it.Path != "" {
			grouped[it.WorkspaceID] = append(grouped[it.WorkspaceID], it.Path)
		}
	}
	changed := 0
	for wsID, paths := range grouped {
		var folder string
		err := s.DB.QueryRowContext(r.Context(),
			`SELECT folder_path FROM workspaces
			  WHERE id = $1 AND company_id = $2 AND unbound_at IS NULL`, wsID, companyID,
		).Scan(&folder)
		if err == sql.ErrNoRows {
			// 不属于本公司的区:静默跳过(不确认存在性,也不计变更)。
			continue
		}
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		changed += len(sched.SyncWorkspaceFileState(r.Context(), wsID, companyID, folder, paths, s.RDB))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"changed": changed})
}
