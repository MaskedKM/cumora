// runtime 包 memory —— inproc-client.ts loadMemory + memory-scope.ts +
// embeddings.ts:混合检索(pinned + 语义 + 近期)与 pgvector 探测、
// OpenAI text-embedding-3-small 嵌入(尽力而为,失败退化为仅按近期)。
package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

/* ───────── memory 路径/作用域(memory-scope.ts 的读侧) ───────── */

// ParsedMemoryPath:memory/<kind>/<id>.md 或 memory/projects/<pid>/...
type ParsedMemoryPath struct {
	Kind      string
	ID        string
	ProjectID *string
}

// ParseMemoryPath:解析 memory 路径;非 memory/ 前缀返回 nil。
func ParseMemoryPath(path string) *ParsedMemoryPath {
	if !strings.HasPrefix(path, "memory/") {
		return nil
	}
	segs := strings.Split(path, "/")
	var projectID *string
	rest := segs[1:]
	if len(segs) >= 3 && segs[1] == "projects" && segs[2] != "" {
		p := segs[2]
		projectID = &p
		rest = segs[3:]
	}
	if len(rest) == 0 {
		return &ParsedMemoryPath{Kind: "index", ID: "", ProjectID: projectID}
	}
	if len(rest) == 1 {
		stem := strings.TrimSuffix(rest[0], ".md")
		kind := "note"
		if strings.ToUpper(stem) == "MEMORY" {
			kind = "index"
		}
		return &ParsedMemoryPath{Kind: kind, ID: stem, ProjectID: projectID}
	}
	kind := rest[0]
	if kind == "" {
		kind = "note"
	}
	id := strings.TrimSuffix(rest[len(rest)-1], ".md")
	return &ParsedMemoryPath{Kind: kind, ID: id, ProjectID: projectID}
}

// memoryScopeSQL 片段:pinned 或无项目或项目在 $n。projectId 取
// meta.source.projectId,空则回退路径推断(memory/projects/<id>/)。
func memoryScopeSQL(metaExpr, pathExpr, param string) string {
	return fmt.Sprintf(`(
    COALESCE((%s->>'pinned')::boolean, false) = true
    OR COALESCE(NULLIF(%s#>>'{source,projectId}', ''), substring(%s from '^memory/projects/([^/]+)/')) IS NULL
    OR COALESCE(NULLIF(%s#>>'{source,projectId}', ''), substring(%s from '^memory/projects/([^/]+)/')) = ANY(%s::text[])
  )`, metaExpr, metaExpr, pathExpr, metaExpr, pathExpr, param)
}

// RowProjectID:meta.source.projectId 优先,路径次之。
func rowProjectID(meta map[string]any, path string) *string {
	if src, ok := meta["source"].(map[string]any); ok {
		if pid, ok := src["projectId"].(string); ok && pid != "" {
			return &pid
		}
	}
	if p := ParseMemoryPath(path); p != nil && p.ProjectID != nil {
		return p.ProjectID
	}
	return nil
}

// MemoryVisibleInScope:pinned=身份恒可见;无项目(遗留/全局)恒可见;
// 项目记忆仅当项目在当前 scope。无项目会话只见全局。
func MemoryVisibleInScope(meta map[string]any, path string, projectIDs []string) bool {
	if pinned, _ := meta["pinned"].(bool); pinned {
		return true
	}
	pid := rowProjectID(meta, path)
	if pid == nil {
		return true
	}
	for _, id := range projectIDs {
		if id == *pid {
			return true
		}
	}
	return false
}

// ResolveMemoryScope:projectIds 直用;否则由会话反查项目;空 = 仅全局。
func (s *Service) ResolveMemoryScope(ctx context.Context, projectIDs, conversationIDs []string) ([]string, error) {
	if len(projectIDs) > 0 {
		return projectIDs, nil
	}
	if len(conversationIDs) == 0 {
		return []string{}, nil
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DISTINCT project_id FROM conversations
		 WHERE id = ANY($1::text[]) AND project_id IS NOT NULL`, pqArray(conversationIDs))
	if err != nil {
		return []string{}, nil // 对齐 TS:查询失败按空 scope(全局)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return out, nil
		}
		out = append(out, pid)
	}
	return out, rows.Err()
}

/* ───────── embeddings(embeddings.ts) ───────── */

const (
	embedModel         = "text-embedding-3-small"
	embedDim           = 1536
	embedMaxInputRunes = 8000 // ~2K token,单条记忆/几条近期消息绰绰有余
)

var (
	pgvectorAvailable *bool
	embedHTTP         = &http.Client{Timeout: 15 * time.Second}
)

func openAIBaseURL() string {
	if u := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "https://api.openai.com/v1"
}

// HasPgVector:pg_extension 探测,结果缓存(loadMemory 不必每次唤醒都探)。
func (s *Service) HasPgVector(ctx context.Context) bool {
	if pgvectorAvailable != nil {
		return *pgvectorAvailable
	}
	var exists bool
	ok := s.DB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector') AS exists`).Scan(&exists) == nil
	if !ok {
		exists = false
	}
	pgvectorAvailable = &exists
	return exists
}

// EmbedText:文本 → pgvector 字面量([0.1,0.2,…],以 $N::vector 绑定)。
// 空/超限/OpenAI 打嗝 → nil,调用方退化为仅按近期检索。测试经
// CUMORA_TEST_EMBED_OVERRIDE(固定 JSON 向量)确定性化,不花嵌入额度。
func (s *Service) EmbedText(ctx context.Context, text string) *string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if override := strings.TrimSpace(os.Getenv("CUMORA_TEST_EMBED_OVERRIDE")); override != "" {
		return &override
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return nil
	}
	if len([]rune(trimmed)) > embedMaxInputRunes {
		trimmed = string([]rune(trimmed)[:embedMaxInputRunes])
	}
	body, _ := json.Marshal(map[string]any{"model": embedModel, "input": trimmed})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		openAIBaseURL()+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := embedHTTP.Do(req)
	if err != nil {
		slog.Warn("[embed] failed", "err", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		slog.Warn("[embed] failed", "status", resp.StatusCode, "body", string(b))
		return nil
	}
	var parsed struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&parsed) != nil || len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) != embedDim {
		return nil
	}
	vec := parsed.Data[0].Embedding
	parts := make([]string, len(vec))
	for i, f := range vec {
		parts[i] = formatFloat(f)
	}
	lit := "[" + strings.Join(parts, ",") + "]"
	return &lit
}

func formatFloat(f float64) string {
	// JS 的 Number→String 最短往返表示;Go strconv 'g' 同为最短往返,
	// 但会输出 1e-07 形态——pgvector 不吃指数记法,统一十进制。
	s := fmt.Sprintf("%g", f)
	if strings.ContainsAny(s, "eE") {
		s = fmt.Sprintf("%.17f", f)
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

/* ───────── loadMemory(inproc-client.ts) ───────── */

type memoryQueryRow struct {
	Path      string
	Body      string
	Meta      map[string]any
	UpdatedAt time.Time
}

// LoadMemory:①pinned 恒在(核心身份);②与当前上下文语义最相关的 Top-N
// (pgvector 余弦距离);③最近 Top-M 兜底(嵌入未跟上时新上下文不丢)。
// 无查询向量时退化为 pinned+近期。agent_workspace 存 memory/<kind>/<id>.md,
// 结构字段在 meta JSONB,稠度向量在 embedding vector(1536)。
func (s *Service) LoadMemory(ctx context.Context, agentID, queryText string,
	semanticLimit, recentLimit, totalLimit int, projectIDs, convoScope []string) ([]map[string]any, error) {
	if semanticLimit <= 0 {
		semanticLimit = 20
	}
	if recentLimit <= 0 {
		recentLimit = 10
	}
	if totalLimit <= 0 {
		totalLimit = 40
	}
	scope, err := s.ResolveMemoryScope(ctx, projectIDs, convoScope)
	if err != nil {
		return nil, err
	}

	useSemantic := strings.TrimSpace(queryText) != "" && s.HasPgVector(ctx)
	var queryVec *string
	if useSemantic {
		queryVec = s.EmbedText(ctx, queryText)
	}

	if queryVec == nil {
		rows, err := s.DB.QueryContext(ctx, `
			SELECT path, body, meta, updated_at
			  FROM agent_workspace
			 WHERE agent_id = $1 AND path LIKE 'memory/%'
			   AND `+memoryScopeSQL("meta", "path", "$3")+`
			 ORDER BY COALESCE((meta->>'pinned')::boolean, false) DESC, updated_at DESC
			 LIMIT $2`, agentID, totalLimit, pqArray(scope))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		qr, err := scanMemoryRows(rows)
		if err != nil {
			return nil, err
		}
		return memoryRowsToOut(qr, scope), nil
	}

	// 三 CTE 并集 + ROW_NUMBER 按 path 去重(保留最低来源位:pinned >
	// semantic > recent),终序保来源位——身份先,相关次,近期末。
	rows, err := s.DB.QueryContext(ctx, `
		WITH
		  pinned AS (
		    SELECT path, body, meta, updated_at, 0 AS source_rank
		      FROM agent_workspace
		     WHERE agent_id = $1 AND path LIKE 'memory/%'
		       AND COALESCE((meta->>'pinned')::boolean, false) = true
		  ),
		  relevant AS (
		    SELECT path, body, meta, updated_at, 1 AS source_rank
		      FROM agent_workspace
		     WHERE agent_id = $1 AND path LIKE 'memory/%'
		       AND embedding IS NOT NULL
		       AND COALESCE((meta->>'pinned')::boolean, false) = false
		       AND `+memoryScopeSQL("meta", "path", "$6")+`
		     ORDER BY embedding <=> $2::vector ASC
		     LIMIT $3
		  ),
		  recent AS (
		    SELECT path, body, meta, updated_at, 2 AS source_rank
		      FROM agent_workspace
		     WHERE agent_id = $1 AND path LIKE 'memory/%'
		       AND COALESCE((meta->>'pinned')::boolean, false) = false
		       AND `+memoryScopeSQL("meta", "path", "$6")+`
		     ORDER BY updated_at DESC
		     LIMIT $4
		  ),
		  unioned AS (
		    SELECT * FROM pinned
		    UNION ALL SELECT * FROM relevant
		    UNION ALL SELECT * FROM recent
		  ),
		  deduped AS (
		    SELECT *, ROW_NUMBER() OVER (
		      PARTITION BY path ORDER BY source_rank ASC
		    ) AS rn FROM unioned
		  )
		SELECT path, body, meta, updated_at
		  FROM deduped
		 WHERE rn = 1
		 ORDER BY source_rank ASC, updated_at DESC
		 LIMIT $5`,
		agentID, *queryVec, semanticLimit, recentLimit, totalLimit, pqArray(scope))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	qr, err := scanMemoryRows(rows)
	if err != nil {
		return nil, err
	}
	return memoryRowsToOut(qr, scope), nil
}

func scanMemoryRows(rows *sql.Rows) ([]memoryQueryRow, error) {
	var out []memoryQueryRow
	for rows.Next() {
		var r memoryQueryRow
		var meta []byte
		if err := rows.Scan(&r.Path, &r.Body, &meta, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if meta != nil {
			var m map[string]any
			if json.Unmarshal(meta, &m) == nil {
				r.Meta = m
			}
		}
		if r.Meta == nil {
			r.Meta = map[string]any{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// memoryRowsToOut:过滤可见性后转 MemoryRow 形(id/kind/about/body/
// pinned/created_at——created_at 即行的 updated_at,对齐 toMemoryRow)。
func memoryRowsToOut(rows []memoryQueryRow, scope []string) []map[string]any {
	out := []map[string]any{}
	for _, r := range rows {
		if !MemoryVisibleInScope(r.Meta, r.Path, scope) {
			continue
		}
		segs := strings.Split(r.Path, "/")
		id := ""
		if len(segs) > 2 {
			id = strings.TrimSuffix(segs[2], ".md")
		}
		kind := "note"
		if k, ok := r.Meta["kind"].(string); ok && k != "" {
			kind = k
		} else if len(segs) > 1 && segs[1] != "" {
			kind = segs[1]
		}
		var about any
		if a, ok := r.Meta["about"].(string); ok {
			about = a
		}
		pinned, _ := r.Meta["pinned"].(bool)
		out = append(out, map[string]any{
			"id":         id,
			"kind":       kind,
			"about":      about,
			"body":       r.Body,
			"pinned":     pinned,
			"created_at": httpx.ISOms(r.UpdatedAt),
		})
	}
	return out
}
