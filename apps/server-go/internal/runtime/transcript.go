// runtime 包 transcript —— #260 工具级执行转录的批量上报面。
// daemon 侧 200ms/finish 冲刷(单 run 2000 条帽在 daemon),此处承接:
// 归属校验(run 属本 agent)+ 服务端截断 + (run_id,seq) 冲突幂等跳过。
// 读面在 devtools(GET /api/devtools/runs/{runId}/transcript)。
package runtime

import (
	"encoding/json"
	"net/http"
	"unicode/utf8"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

const (
	transcriptBatchMax     = 100
	transcriptContentCapB  = 8 * 1024 // content 截断(字节,rune 安全由调用方保证为 UTF-8)
	transcriptInputJSONCap = 8 * 1024
)

// handleTranscriptBatch:POST /runtime/transcript-batch。
func (s *Service) handleTranscriptBatch(w http.ResponseWriter, r *http.Request, agentID string, companyID *string) {
	body, ok := readJSON(w, r)
	if !ok {
		return
	}
	runID := bodyStr(body, "runId")
	if runID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "runId required")
		return
	}
	rawEntries, _ := body["entries"].([]any)
	if len(rawEntries) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "entries required")
		return
	}
	if len(rawEntries) > transcriptBatchMax {
		httpx.WriteError(w, http.StatusBadRequest, "entries exceed batch cap (100)")
		return
	}
	// 归属:run 必须属于本 agent(agentId 恒取 JWT,被 compromise 的
	// daemon 写不了别人的转录)。
	var runAgent string
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT agent_id FROM agent_runs WHERE id = $1`, runID).Scan(&runAgent); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "run not found")
		return
	}
	if runAgent != agentID {
		httpx.WriteError(w, http.StatusForbidden, "run belongs to another agent")
		return
	}

	stored := 0
	for _, re := range rawEntries {
		e, _ := re.(map[string]any)
		if e == nil {
			continue
		}
		seqF, _ := bodyFloat(e, "seq")
		typ := bodyStr(e, "type")
		// 整数 + int32 界内(评审 #326 P2-8:1.5 静默截断会撞幂等键,
		// 超 int32 会让 PG 报 500)。
		if seqF <= 0 || seqF != float64(int64(seqF)) || seqF > 2147483647 || typ == "" {
			continue
		}
		content := bodyStr(e, "content")
		if len(content) > transcriptContentCapB {
			content = trimRunesSafe(content, transcriptContentCapB)
		}
		var input any
		if raw, has := e["input"]; has && raw != nil {
			input = clampTranscriptJSON(raw)
		}
		var tool any
		if t := bodyStr(e, "tool"); t != "" {
			tool = t
		}
		res, err := s.DB.ExecContext(r.Context(), `
			INSERT INTO agent_transcript (id, run_id, agent_id, company_id, seq, type, tool, content, input)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (run_id, seq) DO NOTHING`,
			httpx.UUIDHex(), runID, agentID, companyID, int(seqF), typ, tool, content, input)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			stored++
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "stored": stored})
}

// clampTranscriptJSON:input 序列化超帽时降级为截断提示(保形状不保全文)。
func clampTranscriptJSON(v any) any {
	b, ok := mustJSONBytes(v)
	if ok && len(b) <= transcriptInputJSONCap {
		return v
	}
	if ok {
		return map[string]any{"truncated": true, "prefix": trimRunesSafe(string(b), transcriptInputJSONCap)}
	}
	return nil
}

func mustJSONBytes(v any) ([]byte, bool) {
	b, err := json.Marshal(v)
	return b, err == nil
}

// trimRunesSafe:字节帽内按 rune 边界截断。
func trimRunesSafe(s string, capB int) string {
	if len(s) <= capB {
		return s
	}
	cut := capB
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
