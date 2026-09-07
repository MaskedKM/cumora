// proposals —— #349 招人/淘汰提案审批:花名册进出的人闸。
//
// 落库面:评估报告 payload 可携带 proposals(hire=完整档案草稿+依据;
// offboard=目标 agent+依据),cliReport 收轮时校验并写 hr_proposals
// (status open)—— 只落库不执行,执行权在 owner。批准 hire 走 agents
// 域 createAgentCore 同源路径(入职副作用齐全:#all-hands 入组、
// IDENTITY/SOUL、owner 自动 DM、头像);批准 offboard 走
// OffboardAgentCore(软删、Former 区可 rehire)。未批准的提案不存在
// 任何执行路径(执行仅此一处)。
package hr

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/contract"
	"github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/agents"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// 哨兵错误(approve claim 事务内分类)。errNotFound 与 changes.go 共用。
var (
	errAlreadyDecided   = errors.New("proposal already decided")
	errMalformedProfile = errors.New("proposal profile is malformed")
	errAgentsUnwired    = errors.New("agents domain not wired")
)

// reportProposal:报告 payload 的单条提案。
type reportProposal struct {
	Kind    string         `json:"kind"`
	AgentID string         `json:"agentId"`
	Profile map[string]any `json:"profile"`
	Reason  string         `json:"reason"`
}

// hireProfile:hire 提案的档案草稿(宽松解析,执行时再经 createAgentCore
// 的同一套校验)。headcountNote 不参与执行,仅随档留痕。
type hireProfile struct {
	Name          string `json:"name"`
	Role          string `json:"role"`
	Bio           string `json:"bio"`
	SystemPrompt  string `json:"systemPrompt"`
	Model         string `json:"model"`
	FastModel     string `json:"fastModel"`
	HeadcountNote string `json:"headcountNote"`
}

// recordProposals:收轮事务里校验并落库提案(报告不合法整体拒绝,轮保持
// 打开 —— 与 jobEdits 同语义)。
func recordProposals(ctx context.Context, tx *sql.Tx, companyID, evaluationID string, raw any) error {
	if raw == nil {
		return nil
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return errors.New("proposals must be an array")
	}
	var props []reportProposal
	if err := json.Unmarshal(blob, &props); err != nil {
		return errors.New("proposals must be an array of {kind, agentId|profile, reason}")
	}
	for _, p := range props {
		if p.Reason != "" && agent.UTF16Len(p.Reason) > 2000 {
			return errors.New("proposals[].reason exceeds 2000 UTF-16 units")
		}
		// 校验与 CreateAgentCore 同步(trim 语义对齐,评审 P1:否则
		// 空白档案"落库合格、执行时 400",提案永久卡 open)
		switch p.Kind {
		case "hire":
			b, _ := json.Marshal(p.Profile)
			var hp hireProfile
			if json.Unmarshal(b, &hp) != nil ||
				strings.TrimSpace(hp.Name) == "" ||
				len(strings.TrimSpace(hp.SystemPrompt)) < 10 {
				return errors.New("hire proposal requires profile.name and profile.systemPrompt (>= 10 chars)")
			}
		case "offboard":
			if p.AgentID == "" {
				return errors.New("offboard proposal requires agentId")
			}
			var exists bool
			err := tx.QueryRowContext(ctx,
				`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 AND kind = 'agent' AND departed_at IS NULL LIMIT 1`,
				p.AgentID, companyID).Scan(&exists)
			if err != nil || !exists {
				return errors.New("offboard proposal target is not an active agent of this team")
			}
		default:
			return errors.New("proposals[].kind must be hire or offboard")
		}
		var profileArg any
		if p.Kind == "hire" && p.Profile != nil {
			profileArg = p.Profile
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO hr_proposals (id, company_id, kind, agent_id, profile, reason, evaluation_id)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5::jsonb, $6, $7)`,
			"hrp-"+authn.NewToken()[:10], companyID, p.Kind, p.AgentID, profileArg, p.Reason, evaluationID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ListHrProposals(w http.ResponseWriter, r *http.Request, params contract.ListHrProposalsParams) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	statusFilter := ""
	if params.Status != nil {
		statusFilter = string(*params.Status)
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, kind, agent_id, profile, reason, evaluation_id, status, decided_by, decided_at, result_agent_id, created_at
		  FROM hr_proposals
		 WHERE company_id = $1 AND ($2 = '' OR status = $2)
		 ORDER BY created_at DESC LIMIT 100`, companyID, statusFilter)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		row := scanProposalRow(rows)
		if row == nil {
			continue
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": out})
}

func (s *Server) ApproveHrProposal(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	// 行锁 claim(评审 P0):并发双 approve / approve·reject 交错的执行面
	// 必须互斥 —— SELECT ... FOR UPDATE 持锁到处置 UPDATE 落库,第二个
	// 进来时状态已非 open,不会再执行。执行(Core 走自己的连接)在锁内
	// 发生,锁保证同一提案的执行至多一次。
	var kind, agentID string
	var profile []byte
	var createdAt time.Time
	claim := func() error {
		return db.WithTx(r.Context(), s.DB, func(tx *sql.Tx) error {
			err := tx.QueryRowContext(r.Context(), `
				SELECT kind, COALESCE(agent_id, ''), profile, created_at FROM hr_proposals
				 WHERE id = $1 AND company_id = $2 AND status = 'open' LIMIT 1 FOR UPDATE`, id, companyID).
				Scan(&kind, &agentID, &profile, &createdAt)
			if err == sql.ErrNoRows {
				return errNotFound
			}
			if err != nil {
				return err
			}
			var resultAgent string
			var execErr error
			if kind == "hire" {
				var hp hireProfile
				if json.Unmarshal(profile, &hp) != nil {
					return errMalformedProfile
				}
				if s.Agents == nil {
					return errAgentsUnwired
				}
				resultAgent, execErr = s.Agents.CreateAgentCore(r.Context(), uid, companyID, agents.AgentCreateInput{
					Name: hp.Name, Role: hp.Role, Bio: hp.Bio, SystemPrompt: hp.SystemPrompt,
					Model: hp.Model, FastModel: hp.FastModel,
				})
			} else {
				if s.Agents == nil {
					return errAgentsUnwired
				}
				execErr = s.Agents.OffboardAgentCore(r.Context(), agentID, companyID)
			}
			if execErr != nil {
				return execErr // 锁回滚,提案保持 open 可重试
			}
			res, err := tx.ExecContext(r.Context(), `
				UPDATE hr_proposals SET status = 'approved', decided_by = $3, decided_at = NOW(),
				       result_agent_id = NULLIF($4, '')
				 WHERE id = $1 AND company_id = $2 AND status = 'open'`, id, companyID, uid, resultAgent)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errAlreadyDecided
			}
			return nil
		})
	}
	err := claim()
	var badInput agents.ErrAgentInput
	var conflict agents.ErrAgentConflict
	switch {
	case err == nil:
	case err == errNotFound:
		// 区分"不存在/异司"与"已处置"
		var exists bool
		_ = s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM hr_proposals WHERE id = $1 AND company_id = $2`, id, companyID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusNotFound, "no such proposal")
			return
		}
		httpx.WriteError(w, http.StatusConflict, "proposal already decided")
		return
	case err == errAlreadyDecided:
		httpx.WriteError(w, http.StatusConflict, "proposal already decided")
		return
	case err == errMalformedProfile:
		httpx.WriteError(w, http.StatusBadRequest, "proposal profile is malformed")
		return
	case err == errAgentsUnwired:
		httpx.WriteError(w, http.StatusInternalServerError, "agents domain not wired")
		return
	case errors.As(err, &badInput):
		httpx.WriteError(w, http.StatusBadRequest, badInput.Msg)
		return
	case errors.As(err, &conflict):
		httpx.WriteError(w, http.StatusConflict, conflict.Msg)
		return
	case errors.Is(err, agents.ErrAgentAlreadyDeparted),
		errors.Is(err, agents.ErrAgentNotFound),
		errors.Is(err, agents.ErrAgentNotAgent):
		// offboard 目标已不在(离职/消失)= 已无需执行,409 语义
		httpx.WriteError(w, http.StatusConflict, "offboard target is already gone")
		return
	default:
		httpx.WriteInternalError(w, r, err)
		return
	}

	// 处置后回读整行返回(评审 P2:响应体须与 HrProposal 契约对齐,
	// createdAt 是提案创建时间而非处置时间)
	var reason string
	var evaluationID sql.NullString
	var decidedAt sql.NullTime
	var resultAgent sql.NullString
	err = s.DB.QueryRowContext(r.Context(), `
		SELECT reason, evaluation_id, decided_at, result_agent_id FROM hr_proposals
		 WHERE id = $1 AND company_id = $2`, id, companyID).
		Scan(&reason, &evaluationID, &decidedAt, &resultAgent)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": id, "kind": kind, "status": "approved",
		"agentId": nilIfEmpty(agentID), "profile": rawJSON(profile), "reason": reason,
		"evaluationId":  nullStr(evaluationID),
		"decidedBy":     uid,
		"decidedAt":     nullTime(decidedAt),
		"resultAgentId": nullStr(resultAgent),
		"createdAt":     createdAt.UTC(),
	})
}

func (s *Server) RejectHrProposal(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	res, err := s.DB.ExecContext(r.Context(), `
		UPDATE hr_proposals SET status = 'rejected', decided_by = $3, decided_at = NOW()
		 WHERE id = $1 AND company_id = $2 AND status = 'open'`, id, companyID, uid)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		_ = s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM hr_proposals WHERE id = $1 AND company_id = $2`, id, companyID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusNotFound, "no such proposal")
			return
		}
		httpx.WriteError(w, http.StatusConflict, "proposal already decided")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": "rejected", "decidedBy": uid, "decidedAt": time.Now().UTC(),
	})
}

func scanProposalRow(rows *sql.Rows) map[string]any {
	var id, kind, status string
	var agentID, decidedBy sql.NullString
	var profile []byte
	var reason string
	var evaluationID sql.NullString
	var decidedAt sql.NullTime
	var resultAgent sql.NullString
	var createdAt time.Time
	if rows.Scan(&id, &kind, &agentID, &profile, &reason, &evaluationID, &status, &decidedBy, &decidedAt, &resultAgent, &createdAt) != nil {
		return nil
	}
	return map[string]any{
		"id": id, "kind": kind, "status": status,
		"agentId": nullStr(agentID), "profile": rawJSON(profile), "reason": reason,
		"evaluationId":  nullStr(evaluationID),
		"decidedBy":     nullStr(decidedBy),
		"decidedAt":     nullTime(decidedAt),
		"resultAgentId": nullStr(resultAgent),
		"createdAt":     createdAt.UTC(),
	}
}
