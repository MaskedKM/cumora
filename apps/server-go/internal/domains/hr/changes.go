// changes —— #348 岗位层修改闭环:HR prompt 优化的应用、台账与回滚。
//
// 应用面:评估报告 payload 可携带 jobEdits([{agentId, field, value}]),
// cliReport 在收轮的同一事务里校验(目标属本司、字段白名单)并落库
// participants 岗位层字段 + 逐条记 hr_changes。作用域硬边界:只认
// system_prompt/bio/role 三字段(model/engine/computer/status 等一律拒),
// Private Area 永不可达(HR 域不触文件面)。变更全程无声 —— 直接 UPDATE,
// 不产生任何消息/通知;被改 agent 的 daemon 侧 runner 经 roster 同步
// (ConfigMatches 对比 SystemPrompt)自然换血。
//
// 回滚面:owner/admin POST /api/hr/changes/{id}/revert —— 把目标行的
// old_value 写回 participants,回滚本身也是一次变更(进历史,
// reverted_change_id 指向被回滚行);回滚的回滚自然成立。
package hr

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/contract"
	"github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// jobField:岗位层字段白名单(payload 驼峰名 → 列名)。
var jobFields = map[string]string{
	"systemPrompt": "system_prompt",
	"bio":          "bio",
	"role":         "role",
}

const jobValueCapUTF16 = 32_000

// jobEdit:报告 payload 的单条岗位层修改。
type jobEdit struct {
	AgentID string `json:"agentId"`
	Field   string `json:"field"`
	Value   string `json:"value"`
}

// applyJobEdits:在给定事务里校验并应用岗位层修改(报告收轮同事务)。
// 任一条不合法即整体拒绝(报告不落库,轮保持打开)—— 半套优化落库比
// 不落库更糟。返回真实应用数(无变化的 no-op 不入台账也不计数)。
func applyJobEdits(ctx context.Context, tx *sql.Tx, companyID, evaluationID string, edits []jobEdit) (int, error) {
	applied := 0
	for _, e := range edits {
		col, ok := jobFields[e.Field]
		if !ok {
			return 0, fmt.Errorf("jobEdits.field %q is not a job-level field (allowed: systemPrompt, bio, role)", e.Field)
		}
		if agent.UTF16Len(e.Value) > jobValueCapUTF16 {
			return 0, fmt.Errorf("jobEdits value for %s exceeds %d UTF-16 units", e.Field, jobValueCapUTF16)
		}
		var oldValue sql.NullString
		err := tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT %s FROM participants WHERE id = $1 AND company_id = $2 AND kind = 'agent' AND departed_at IS NULL LIMIT 1`,
			col), e.AgentID, companyID).Scan(&oldValue)
		if err != nil {
			return 0, fmt.Errorf("jobEdits target %q is not an active agent of this team", e.AgentID)
		}
		prev := ""
		if oldValue.Valid {
			prev = oldValue.String
		}
		if prev == e.Value {
			continue // 无变化不入台账(回执计数也不计)
		}
		// UPDATE 保留 SELECT 的守卫谓词(评审 P1:READ COMMITTED 下
		// SELECT→UPDATE 间他方提交 depart 的窗口真实存在)+ 零行即回滚。
		res, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE participants SET %s = $3 WHERE id = $1 AND company_id = $2 AND kind = 'agent' AND departed_at IS NULL`, col),
			e.AgentID, companyID, e.Value)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, fmt.Errorf("jobEdits target %q is no longer an active agent of this team", e.AgentID)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO hr_changes (id, company_id, agent_id, field, old_value, new_value, evaluation_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			"hrc-"+authn.NewToken()[:10], companyID, e.AgentID, col, prev, e.Value, evaluationID); err != nil {
			return 0, err
		}
		applied++
	}
	return applied, nil
}

func (s *Server) ListHrChanges(w http.ResponseWriter, r *http.Request, params contract.ListHrChangesParams) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, agent_id, field, old_value, new_value, evaluation_id, reverted_change_id, created_at
		  FROM hr_changes
		 WHERE company_id = $1 AND ($2 = '' OR agent_id = $2)
		 ORDER BY created_at DESC LIMIT 100`, companyID, derefStr(params.AgentId))
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		row := scanChangeRow(rows)
		if row == nil {
			continue // 扫描失败跳过该行,不产 null 违反契约(评审 P2)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": out})
}

// revertFieldCol:回滚列名反查(应用层白名单;DB CHECK 为第二道保险)。
func revertFieldCol(field string) (string, bool) {
	switch field {
	case "system_prompt", "bio", "role":
		return field, true
	}
	return "", false
}

func (s *Server) RevertHrChange(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	// 单事务双写(participants 写回 + 台账行)原子化,FOR UPDATE 串行化
	// 并发回滚(评审 P0:吞错+非原子会静默断裂审计链)。
	newID := "hrc-" + authn.NewToken()[:10]
	var out struct {
		agentID, field, oldValue, newValue string
	}
	err := db.WithTx(r.Context(), s.DB, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(r.Context(), `
			SELECT agent_id, field, old_value, new_value FROM hr_changes
			 WHERE id = $1 AND company_id = $2 LIMIT 1 FOR UPDATE`, id, companyID).
			Scan(&out.agentID, &out.field, &out.oldValue, &out.newValue)
		if err == sql.ErrNoRows {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if _, ok := revertFieldCol(out.field); !ok {
			return fmt.Errorf("change row carries non job-level field %q", out.field)
		}
		res, err := tx.ExecContext(r.Context(), `
			UPDATE participants SET `+out.field+` = $3
		 WHERE id = $1 AND company_id = $2 AND kind = 'agent' AND departed_at IS NULL`,
			out.agentID, companyID, out.oldValue)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// 行没了(硬删)或已离职:不落假台账行
			return errNotFound
		}
		_, err = tx.ExecContext(r.Context(), `
			INSERT INTO hr_changes (id, company_id, agent_id, field, old_value, new_value, reverted_change_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			newID, companyID, out.agentID, out.field, out.newValue, out.oldValue, id)
		return err
	})
	if err == errNotFound {
		httpx.WriteError(w, http.StatusNotFound, "no such change (or its agent is gone)")
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": newID, "agentId": out.agentID, "field": fieldCamel(out.field),
		"oldValue": out.newValue, "newValue": out.oldValue,
		"revertedChangeId": id, "createdAt": time.Now().UTC(),
	})
}

// errNotFound:行缺失哨兵(区别于其它 SQL 错误)。
var errNotFound = errors.New("not found")

// scanChangeRow:列表/详情共用行扫描(payload 键名与 HrChange 契约一致,
// field 回驼峰)。
func scanChangeRow(rows *sql.Rows) map[string]any {
	var id, agentID, field string
	var oldValue, newValue string
	var evaluationID, revertedID sql.NullString
	var createdAt time.Time
	if rows.Scan(&id, &agentID, &field, &oldValue, &newValue, &evaluationID, &revertedID, &createdAt) != nil {
		return nil
	}
	return map[string]any{
		"id": id, "agentId": agentID, "field": fieldCamel(field),
		"oldValue": oldValue, "newValue": newValue,
		"evaluationId":     nullStr(evaluationID),
		"revertedChangeId": nullStr(revertedID),
		"createdAt":        createdAt.UTC(),
	}
}

func fieldCamel(col string) string {
	if col == "system_prompt" {
		return "systemPrompt"
	}
	return col
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// jobEditsFrom:报告 payload 取 jobEdits(缺键=空)。
func jobEditsFrom(payload map[string]any) ([]jobEdit, error) {
	raw, ok := payload["jobEdits"]
	if !ok || raw == nil {
		return nil, nil
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("jobEdits must be an array")
	}
	var edits []jobEdit
	if err := json.Unmarshal(blob, &edits); err != nil {
		return nil, fmt.Errorf("jobEdits must be an array of {agentId, field, value}")
	}
	return edits, nil
}
