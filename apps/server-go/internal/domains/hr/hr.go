// domains/hr —— HR Agent(编外隐形人事代理,ADR 0007)配置面。
//
// 每公司恰一行 hr_agents(独立存储,非 participants 行):不在花名册、
// 对其他 agent 不可见不可召唤、不可 offboard;prompt/computer/engine 仅
// owner/admin 可配。观测归因约定:HR 的 runs/llm 台账按
// agent_id = "hr-<companyId>" 落账(agent_runs/llm_calls 的 agent_id 为
// 无 FK 纯文本,零新表即单独归因);花名册/成员枚举/配额计数因它不在
// participants 表而天然不见它。#345 骨架:仅配置面,评估执行在 #346。
package hr

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/contract"
	hrcontract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/hr"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// Server:hr tag 的域实现(路由 2:getHrAgent/putHrAgentConfig)。
type Server struct {
	DB *sql.DB
}

var _ hrcontract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = hrcontract.HandlerFromMux(&Server{DB: db}, mux)
}

// requireRole:owner/admin 闸(httpx.ResolveCompanyRole 薄包装,computers 域同款)。
func requireRole(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", false
	}
	return httpx.ResolveCompanyRole(w, r, db, uid)
}

// EnsureProvisioned:幂等置备 —— CreateCompany 钩子与 GET 兜底共用;
// 存量公司由 0008 迁移回填。prompt 默认值单源于 SQL DEFAULT。
func EnsureProvisioned(ctx context.Context, db *sql.DB, companyID string) {
	_, _ = db.ExecContext(ctx,
		`INSERT INTO hr_agents (company_id) VALUES ($1) ON CONFLICT (company_id) DO NOTHING`,
		companyID)
}

type hrRow struct {
	systemPrompt string
	computerID   sql.NullString
	engine       sql.NullString
	updatedAt    time.Time
}

func loadHrAgent(ctx context.Context, db *sql.DB, companyID string) (hrRow, bool) {
	var row hrRow
	err := db.QueryRowContext(ctx, `
		SELECT system_prompt, computer_id, engine, updated_at
		  FROM hr_agents WHERE company_id = $1`, companyID).
		Scan(&row.systemPrompt, &row.computerID, &row.engine, &row.updatedAt)
	return row, err == nil
}

// loadOrProvision:读为主,缺失才置备(迁移回填 + CreateCompany 钩子两路
// 常态已覆盖;此处兜底早于它们的路径,幂等)。置备后仍缺 = 内部错。
func loadOrProvision(ctx context.Context, db *sql.DB, companyID string) (hrRow, bool) {
	if row, ok := loadHrAgent(ctx, db, companyID); ok {
		return row, true
	}
	EnsureProvisioned(ctx, db, companyID)
	row, ok := loadHrAgent(ctx, db, companyID)
	return row, ok
}

// payload:HrAgent 契约形。agentId 是观测归因键(runs/llm-spend 按它落账)。
func (row hrRow) payload(companyID string) map[string]any {
	return map[string]any{
		"agentId":      "hr-" + companyID,
		"systemPrompt": row.systemPrompt,
		"computerId":   nullStr(row.computerID),
		"engine":       nullStr(row.engine),
		"updatedAt":    row.updatedAt.UTC(),
	}
}

func nullStr(ns sql.NullString) any {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func (s *Server) GetHrAgent(w http.ResponseWriter, r *http.Request) {
	companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	// 兜底:早于钩子/迁移路径存在的公司(幂等,行缺失即补)
	row, ok := loadOrProvision(r.Context(), s.DB, companyID)
	if !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("hr_agents row missing for company %s", companyID))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row.payload(companyID))
}

func (s *Server) PutHrAgentConfig(w http.ResponseWriter, r *http.Request) {
	companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	if _, ok := loadOrProvision(r.Context(), s.DB, companyID); !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("hr_agents row missing for company %s", companyID))
		return
	}
	var body contract.HrAgentConfigInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// 部分更新:缺键不动;systemPrompt 空串拒收;computerId 空串 =
	// 清空指派(computer+engine 一并清),非空则校验属本司并解析引擎。
	var sets []string
	var args []any
	engineOnly := false
	if body.SystemPrompt != nil {
		p := strings.TrimSpace(*body.SystemPrompt)
		if p == "" {
			httpx.WriteError(w, http.StatusBadRequest, "systemPrompt must not be empty")
			return
		}
		args = append(args, p)
		sets = append(sets, fmt.Sprintf("system_prompt = $%d", len(args)))
	}
	if body.ComputerId != nil {
		cid := strings.TrimSpace(*body.ComputerId)
		if cid == "" {
			sets = append(sets, "computer_id = NULL", "engine = NULL")
		} else {
			requested := ""
			if body.Engine != nil {
				requested = strings.TrimSpace(*body.Engine)
			}
			pick, ok := resolveEngine(r.Context(), s.DB, companyID, cid, requested)
			if !ok {
				httpx.WriteError(w, http.StatusBadRequest, "invalid computer or engine for this company")
				return
			}
			args = append(args, cid, pick)
			sets = append(sets,
				fmt.Sprintf("computer_id = $%d", len(args)-1),
				fmt.Sprintf("engine = $%d", len(args)))
		}
	} else if body.Engine != nil && strings.TrimSpace(*body.Engine) != "" {
		// 只换引擎:对现行 computer 校验解析。WHERE 带 computer_id 非空守卫
		// ——并发"清空指派"交错时不复活孤儿 engine(0 行 = 竞态败者,400)。
		row, ok := loadHrAgent(r.Context(), s.DB, companyID)
		if !ok || !row.computerID.Valid {
			httpx.WriteError(w, http.StatusBadRequest, "assign a computer before choosing an engine")
			return
		}
		pick, ok2 := resolveEngine(r.Context(), s.DB, companyID, row.computerID.String, strings.TrimSpace(*body.Engine))
		if !ok2 {
			httpx.WriteError(w, http.StatusBadRequest, "invalid engine for the current computer")
			return
		}
		args = append(args, pick)
		sets = append(sets, fmt.Sprintf("engine = $%d", len(args)))
		engineOnly = true
	}
	if len(sets) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	args = append(args, companyID)
	where := fmt.Sprintf(` WHERE company_id = $%d`, len(args))
	if engineOnly {
		where += ` AND computer_id IS NOT NULL`
	}
	res, err := s.DB.ExecContext(r.Context(),
		`UPDATE hr_agents SET `+strings.Join(sets, ", ")+`, updated_at = NOW()`+where, args...)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "assign a computer before choosing an engine")
		return
	}
	row, ok := loadHrAgent(r.Context(), s.DB, companyID)
	if !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("hr_agents row missing for company %s after update", companyID))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row.payload(companyID))
}

// resolveEngine:computer 属本司且未吊销时解析落库引擎 —— 请求值在
// advertised 里则用之,否则回退首项(语义对齐 computers.AssignAgentToComputer)。
func resolveEngine(ctx context.Context, db *sql.DB, companyID, computerID, requested string) (string, bool) {
	var enginesJSON []byte
	err := db.QueryRowContext(ctx, `
		SELECT available_engines FROM computers
		 WHERE id = $1 AND company_id = $2 AND kind <> 'cloud' AND revoked_at IS NULL LIMIT 1`,
		computerID, companyID).Scan(&enginesJSON)
	if err != nil {
		return "", false
	}
	var advertised []string
	_ = json.Unmarshal(enginesJSON, &advertised)
	for _, a := range advertised {
		if requested != "" && a == requested {
			return a, true
		}
	}
	if len(advertised) > 0 {
		return advertised[0], true
	}
	return "", false
}
