// domains/devtools —— 开发者/观察面(#77):agent workspace 文件读、run
// 事件流、纯 agent 会话偷看、admin 头像生成。门禁对齐 router.ts:
// requireDevtools(NODE_ENV≠production 恒开,否则 x-cumora-dev-mode 头 +
// owner/admin)、requireCompanyRole(avatar:owner/admin;peek:仅 owner)。
// #187 批次 5:本域同时是 observability tag 的宿主(run events/peek 三
// 路由原生,runs/triage/llm-spend 委托 obs.API);avatar/generate 属
// agents tag,经导出函数 AgentAvatarGenerate 供 agents 域委托。
package devtools

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/devtools"
	obscontract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/observability"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/obs"
)

const devtoolsHeader = "x-cumora-dev-mode"

// AvatarGen:头像生成钩子(main 注入 runtime.Service.cliGenerateAndPersistAvatar;
// boards 的 WakeMentioned 同款依赖倒置,域包不反向 import runtime)。
type AvatarGen func(ctx context.Context, agentID, tenant string) (string, error)

// Server:devtools tag(3 路由)+ observability tag(6 路由)的域实现。
type Server struct {
	DB  *sql.DB
	Obs *obs.API // observability 的 runs/triage/llm-spend 三面委托载体
}

var _ contract.ServerInterface = (*Server)(nil)
var _ obscontract.ServerInterface = (*Server)(nil)

// Mount:两 tag 均走契约生成物(#187 批次 5 收编 observability;agents
// tag 的 avatar/generate 由 agents 域 Mount 携钩子委托到本包导出函数,
// 本 Mount 不再需要头像钩子)。
func Mount(mux *http.ServeMux, db *sql.DB) {
	s := &Server{DB: db, Obs: &obs.API{DB: db}}
	_ = contract.HandlerFromMux(s, mux)
	_ = obscontract.HandlerFromMux(s, mux)
}

/* ───────── 门禁 ───────── */

func resolveRole(w http.ResponseWriter, r *http.Request, db *sql.DB) (uid, companyID, role string, ok bool) {
	uid, ok = httpx.RequireAuth(w, r)
	if !ok {
		return "", "", "", false
	}
	companyID, ok = httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return "", "", "", false
	}
	if err := db.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
		companyID, uid).Scan(&role); err != nil {
		role = "member"
	}
	return uid, companyID, role, true
}

func privileged(role string) bool { return role == "owner" || role == "admin" }

// requireDevtools:localDev(NODE_ENV≠production)恒开;生产要求 dev 头 +
// owner/admin。403 'developer tools are not enabled'。
func requireDevtools(w http.ResponseWriter, r *http.Request, db *sql.DB) (companyID string, ok bool) {
	_, cid, role, ok := resolveRole(w, r, db)
	if !ok {
		return "", false
	}
	localDev := !config.IsProduction()
	h := r.Header.Get(devtoolsHeader)
	requested := h == "1" || h == "true"
	if !(localDev || (requested && privileged(role))) {
		httpx.WriteError(w, http.StatusForbidden, "developer tools are not enabled")
		return "", false
	}
	return cid, true
}

// requireCompanyRole:role 不在白名单 → 403(TS 恒同一条文案)。
func requireCompanyRole(w http.ResponseWriter, r *http.Request, db *sql.DB, ownerOnly bool) (companyID string, ok bool) {
	_, cid, role, ok := resolveRole(w, r, db)
	if !ok {
		return "", false
	}
	allowed := privileged(role)
	if ownerOnly {
		allowed = role == "owner"
	}
	if !allowed {
		httpx.WriteError(w, http.StatusForbidden, "this action requires an owner or admin of the team")
		return "", false
	}
	return cid, true
}

/* ───────── GET /devtools/agent-workspace/file ───────── */

func (s *Server) ReadAgentWorkspaceFile(w http.ResponseWriter, r *http.Request, params contract.ReadAgentWorkspaceFileParams) {
	tenant, ok := requireDevtools(w, r, s.DB)
	if !ok {
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agentId"))
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if agentID == "" || path == "" {
		httpx.WriteError(w, http.StatusBadRequest, "agentId and path required")
		return
	}
	var body string
	var size, lineCount int
	var updatedAt time.Time
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT
		    body,
		    LENGTH(body)::int,
		    (LENGTH(body) - LENGTH(REPLACE(body, E'\n', '')) + 1)::int,
		    updated_at
		  FROM agent_workspace
		 WHERE agent_id = $1 AND path = $2 AND company_id = $3
		 LIMIT 1`, agentID, path, tenant).Scan(&body, &size, &lineCount, &updatedAt)
	if err == sql.ErrNoRows {
		httpx.WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"path": path, "body": body, "size": size,
		"lineCount": lineCount, "updatedAt": httpx.ISOms(updatedAt),
	})
}

/* ───────── GET /agents/observability/runs/{id}/events ───────── */

func (s *Server) GetAgentRunEvents(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireDevtools(w, r, s.DB)
	if !ok {
		return
	}
	runID := id
	var one int
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM agent_runs WHERE id = $1 AND company_id = $2 LIMIT 1`,
		runID, tenant).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			httpx.WriteError(w, http.StatusNotFound, "not found")
		} else {
			// 门禁查询失败 ≠ 不存在(TS throw → 500,#107 评审 NIT5)。
			httpx.WriteInternalError(w, r, err)
		}
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, run_id, agent_id, kind, level, title, data, created_at
		  FROM agent_events
		 WHERE run_id = $1 AND company_id = $2
		 ORDER BY created_at ASC, id ASC`, runID, tenant)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, runID2, agentID, kind, level, title string
		var data []byte
		var createdAt time.Time
		if err := rows.Scan(&id, &runID2, &agentID, &kind, &level, &title, &data, &createdAt); err != nil {
			continue
		}
		var dataAny any
		_ = json.Unmarshal(data, &dataAny)
		out = append(out, map[string]any{
			"id": id, "runId": runID2, "agentId": agentID, "kind": kind,
			"level": level, "title": title, "data": dataAny, "createdAt": httpx.ISOms(createdAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// GetAgentRunTranscript:#260 工具级转录回放(sinceSeq 游标分页,限幅)。
func (s *Server) GetAgentRunTranscript(w http.ResponseWriter, r *http.Request, runId string, params obscontract.GetAgentRunTranscriptParams) {
	tenant, ok := requireDevtools(w, r, s.DB)
	if !ok {
		return
	}
	var one int
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM agent_runs WHERE id = $1 AND company_id = $2 LIMIT 1`,
		runId, tenant).Scan(&one); err != nil {
		if err == sql.ErrNoRows {
			httpx.WriteError(w, http.StatusNotFound, "not found")
		} else {
			httpx.WriteInternalError(w, r, err)
		}
		return
	}
	limit := 200
	if params.Limit != nil && *params.Limit > 0 && *params.Limit <= 1000 {
		limit = *params.Limit
	}
	sinceSeq := 0
	if params.SinceSeq != nil && *params.SinceSeq > 0 {
		sinceSeq = *params.SinceSeq
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT seq, type, tool, content, input, created_at
		  FROM agent_transcript
		 WHERE run_id = $1 AND seq > $2
		 ORDER BY seq ASC
		 LIMIT $3`, runId, sinceSeq, limit)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var seq int
		var typ string
		var tool sql.NullString
		var content sql.NullString
		var input []byte
		var createdAt time.Time
		if err := rows.Scan(&seq, &typ, &tool, &content, &input, &createdAt); err != nil {
			continue
		}
		var inputAny any
		_ = json.Unmarshal(input, &inputAny)
		out = append(out, map[string]any{
			"seq": seq, "type": typ, "tool": nullStrOf(tool), "content": nullStrOf(content),
			"input": inputAny, "createdAt": httpx.ISOms(createdAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

/* ───────── GET /peek/agent-chats/{id}/messages ───────── */

// GetWhisperMessages:owner-only;绕过"必须是成员"规则以允许人类旁听纯
// agent 房,但必须验证会话确为全员 agent(否则就是读任意会话的后门)。
func (s *Server) GetWhisperMessages(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireCompanyRole(w, r, s.DB, true)
	if !ok {
		return
	}
	var isAgentOnly bool
	if err := s.DB.QueryRowContext(r.Context(), `
		SELECT (
		    jsonb_array_length(c.members) >= 1
		    AND NOT EXISTS (
		      SELECT 1 FROM jsonb_array_elements_text(c.members) m
		        LEFT JOIN participants p ON p.id = m AND p.company_id = c.company_id
		       WHERE p.kind IS DISTINCT FROM 'agent'
		    )
		 ) FROM conversations c
		WHERE c.id = $1 AND c.company_id = $2
		LIMIT 1`, id, tenant).Scan(&isAgentOnly); err != nil {
		if err == sql.ErrNoRows {
			httpx.WriteError(w, http.StatusNotFound, "not found")
		} else {
			httpx.WriteInternalError(w, r, err)
		}
		return
	}
	if !isAgentOnly {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, conversation_id, author_id, kind, body, sequence, tool, created_at
		  FROM messages
		 WHERE conversation_id = $1
		 ORDER BY sequence ASC`, id)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var msgID, convoID, authorID, kind, body string
		var sequence int64
		var tool []byte
		var createdAt time.Time
		if err := rows.Scan(&msgID, &convoID, &authorID, &kind, &body, &sequence, &tool, &createdAt); err != nil {
			continue
		}
		var toolAny any
		_ = json.Unmarshal(tool, &toolAny)
		out = append(out, map[string]any{
			"id": msgID, "conversationId": convoID, "authorId": authorID,
			"kind": kind, "body": body, "sequence": sequence, "tool": toolAny,
			"createdAt": httpx.ISOms(createdAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

/* ───────── POST /agents/{id}/avatar/generate ───────── */

// AgentAvatarGenerate:agents tag 的 {id}/avatar/generate(agents 域
// Server 委托到此;体为原 avatarGenerate 闭包逐字上移)。owner/admin
// (烧真钱的图像 API);404 'not found' / 400 'avatar generation is
// only for agents' 透传,其余 502 'image generation failed: <msg>'。
func AgentAvatarGenerate(db *sql.DB, gen AvatarGen, w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireCompanyRole(w, r, db, false)
	if !ok {
		return
	}
	url, err := gen(r.Context(), id, tenant)
	if err != nil {
		switch err.Error() {
		case "not found":
			httpx.WriteError(w, http.StatusNotFound, "not found")
		case "avatar generation is only for agents":
			httpx.WriteError(w, http.StatusBadRequest, "avatar generation is only for agents")
		default:
			slog.Warn("[agents] avatar generate failed", "err", err)
			// TS baseline 无条件透传(`image generation failed: ${msg}`),
			// 非 errorHandler 面 —— 不进 WriteInternalError。
			httpx.WriteError(w, http.StatusBadGateway, "image generation failed: "+err.Error())
		}
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"url": url})
}

/* ───────── observability 委托面(obs.API 三路由) ───────── */

// runs/triage/llm-spend 的体在 obs 包(#140 自 runtime 拆出);生成器不
// 摘 query,obs 侧继续读 r.URL.Query(),params 形参落地不消费。
func (s *Server) GetAgentRuns(w http.ResponseWriter, r *http.Request, params obscontract.GetAgentRunsParams) {
	s.Obs.HandleObsRuns(w, r)
}

func (s *Server) GetTriageEconomics(w http.ResponseWriter, r *http.Request, params obscontract.GetTriageEconomicsParams) {
	s.Obs.HandleObsTriage(w, r)
}

func (s *Server) GetLlmSpend(w http.ResponseWriter, r *http.Request) {
	s.Obs.HandleObsSpend(w, r)
}

/* ───────── capabilities + workspace 索引 + peek 列表(#68 补齐) ───────── */

// capabilities:getDevtoolsState 的客户端通告(不 403——探测端点)。
func (s *Server) GetDevtoolsCapabilities(w http.ResponseWriter, r *http.Request) {
	_, cid, role, ok := resolveRole(w, r, s.DB)
	if !ok {
		return
	}
	_ = cid
	localDev := !config.IsProduction()
	h := r.Header.Get(devtoolsHeader)
	requested := h == "1" || h == "true"
	priv := privileged(role)
	canEnable := localDev || priv
	enabled := localDev || (requested && priv)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"enabled": enabled, "canEnable": canEnable, "localDev": localDev,
		"productionDevMode": !localDev && requested && enabled, "role": role,
	})
}

// workspaceIndex:agent 工作区文件索引(path/size/lineCount/updatedAt)。
func (s *Server) ListAgentWorkspace(w http.ResponseWriter, r *http.Request, params contract.ListAgentWorkspaceParams) {
	tenant, ok := requireDevtools(w, r, s.DB)
	if !ok {
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agentId"))
	if agentID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "agentId required")
		return
	}
	var one int
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 AND kind = 'agent' LIMIT 1`,
		agentID, tenant).Scan(&one); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "agent not found")
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT path, LENGTH(body)::int,
		       (LENGTH(body) - LENGTH(REPLACE(body, E'\n', '')) + 1)::int,
		       updated_at
		  FROM agent_workspace
		 WHERE agent_id = $1 AND company_id = $2
		 ORDER BY path`, agentID, tenant)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var path string
		var size, lineCount int
		var updatedAt time.Time
		if rows.Scan(&path, &size, &lineCount, &updatedAt) == nil {
			out = append(out, map[string]any{
				"path": path, "size": size, "lineCount": lineCount, "updatedAt": httpx.ISOms(updatedAt),
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// GetWhispers:owner-only 的 agent↔agent 会话观察列表。
func (s *Server) GetWhispers(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireCompanyRole(w, r, s.DB, true)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT c.id, c.kind, c.title, c.members,
		       (c.members->>0), (c.members->>1),
		       c.topic, c.created_at, c.updated_at,
		       (SELECT COUNT(*)::int FROM messages WHERE conversation_id = c.id)
		  FROM conversations c
		 WHERE c.company_id = $1
		   AND jsonb_array_length(c.members) >= 2
		   AND NOT EXISTS (
		     SELECT 1 FROM jsonb_array_elements_text(c.members) m
		       LEFT JOIN participants p ON p.id = m AND p.company_id = c.company_id
		      WHERE p.kind IS DISTINCT FROM 'agent'
		   )
		 ORDER BY c.updated_at DESC
		 LIMIT 50`, tenant)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, kind, title, agentA, agentB string
		var members []byte
		var topic sql.NullString
		var createdAt, updatedAt time.Time
		var msgCount int
		if rows.Scan(&id, &kind, &title, &members, &agentA, &agentB, &topic, &createdAt, &updatedAt, &msgCount) == nil {
			var membersAny any
			_ = json.Unmarshal(members, &membersAny)
			out = append(out, map[string]any{
				"id": id, "kind": kind, "title": title, "members": membersAny,
				"agentA": agentA, "agentB": agentB, "about": nullStrOf(topic),
				"createdAt": httpx.ISOms(createdAt), "updatedAt": httpx.ISOms(updatedAt),
				"msgCount": msgCount,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func nullStrOf(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}
