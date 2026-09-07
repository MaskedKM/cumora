// ratings —— #347 评估输入补全的 owner 主观评分半边。
//
// 打分(1..5)+评语,按 agent upsert(每司每 agent 恰一行,"当前评分"语
// 义,编辑=替换;历史轨迹由评估轮 input_snapshot 天然留存)。仅
// owner/admin 可写;评分进入下一轮评估装配(assembleInputs ratings 路)。
package hr

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func (s *Server) ListHrRatings(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT agent_id, score, comment, updated_at
		  FROM hr_ratings WHERE company_id = $1 ORDER BY agent_id ASC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var agentID string
		var score int
		var comment string
		var updatedAt time.Time
		if rows.Scan(&agentID, &score, &comment, &updatedAt) != nil {
			continue
		}
		out = append(out, map[string]any{
			"agentId": agentID, "score": score, "comment": comment, "updatedAt": updatedAt.UTC(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": out})
}

func (s *Server) PutHrRating(w http.ResponseWriter, r *http.Request, agentID string) {
	uid, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	agentID = strings.TrimSpace(agentID)
	var body struct {
		Score   *int    `json:"score"`
		Comment *string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Score == nil || *body.Score < 1 || *body.Score > 5 {
		httpx.WriteError(w, http.StatusBadRequest, "score must be an integer between 1 and 5")
		return
	}
	comment := ""
	if body.Comment != nil {
		comment = httpx.UTF16Cap(strings.TrimSpace(*body.Comment), 2000)
	}
	var exists bool
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 AND kind = 'agent' AND departed_at IS NULL LIMIT 1`,
		agentID, companyID).Scan(&exists)
	if !exists {
		httpx.WriteError(w, http.StatusBadRequest, "unknown target agent")
		return
	}
	var updatedAt time.Time
	err := s.DB.QueryRowContext(r.Context(), `
		INSERT INTO hr_ratings (company_id, agent_id, score, comment, updated_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (company_id, agent_id)
		DO UPDATE SET score = EXCLUDED.score, comment = EXCLUDED.comment,
		              updated_by = EXCLUDED.updated_by, updated_at = NOW()
		RETURNING updated_at`, companyID, agentID, *body.Score, comment, uid).Scan(&updatedAt)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"agentId": agentID, "score": *body.Score, "comment": comment, "updatedAt": updatedAt.UTC(),
	})
}
