// domains/workspaces —— 工作区域(#56):建区/列表/详情(成员范围推导)/
// 显式成员/关联三件套/文件列读写/安全解绑。文件安全对齐
// 已退役 TS server 的 workspaces/core.ts 的双层防逃逸(resolve 归一 + realpath 复检,
// 新建文件回退父目录 realpath)。真目录 IO(非 mock)。
package workspaces

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/workspaces"
	dbpkg "github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxFileBytes = 2 * 1024 * 1024

// 对齐 express.json({limit:'34mb'}):超过读入上限直接 413,小于上限的
// 超限内容由 handler 的 maxFileBytes 检查接手。
const maxBodyBytes = 34 * 1024 * 1024

// shortID:ws-/wa- 前缀后的 10 位标识,对齐 TS randomUUID().slice(0,10)
// 的十六进制字母表(基线生成的 id 只含 [0-9a-f])。
func shortID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Server:workspaces tag 的域实现(#187 机械迁移,documents 范式)。
// 方法体自原闭包工厂原样搬运;文件面的查询参数宽容读法保留在
// handler(规范已如实化为可选,校验文案是契约的一部分)。
type Server struct{ DB *sql.DB }

var _ contract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

type wsRow struct {
	id         string
	companyID  string
	name       string
	folderPath string
	isDefault  bool
	createdAt  time.Time
	unboundAt  sql.NullTime
	unboundBy  sql.NullString
}

func loadWorkspace(ctx context.Context, db *sql.DB, companyID, id string) (wsRow, bool) {
	var w wsRow
	err := db.QueryRowContext(ctx, `
		SELECT id, company_id, name, folder_path, is_default, created_at, unbound_at, unbound_by
		  FROM workspaces WHERE id = $1 AND company_id = $2`, id, companyID).
		Scan(&w.id, &w.companyID, &w.name, &w.folderPath, &w.isDefault, &w.createdAt, &w.unboundAt, &w.unboundBy)
	if err != nil {
		return w, false
	}
	return w, true
}

// ensureDefault 惰性建默认区(自愈;产品管理目录 <uploads 根>/workspaces/
// <companyId>,uploads 根经 config.UploadsDir() 统一解析——#208 前硬编码
// server/uploads 相对 cwd,设 env 会被无视)。
// 必须 Abs 化再落库:folder_path 的唯一约束与双重绑定防御都以绝对路径为
// 不变量,CWD 变了也不能搬家。
func ensureDefault(ctx context.Context, db *sql.DB, companyID string) error {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM workspaces WHERE company_id = $1 AND is_default LIMIT 1`, companyID).Scan(&exists); err == nil && exists {
		return nil
	}
	folder := filepath.Join(config.UploadsDir(), "workspaces", companyID)
	if abs, err := filepath.Abs(folder); err == nil {
		folder = abs
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return fmt.Errorf("default workspace folder: %w", err)
	}
	real, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO workspaces (id, company_id, name, folder_path, is_default)
		VALUES ($1, $2, 'Team files', $3, TRUE) ON CONFLICT DO NOTHING`,
		"ws-default-"+companyID, companyID, real)
	if err != nil && !isUniqueViolation(err) {
		return err
	}
	return nil
}

// resolveAccess 对齐 core.resolveWorkspaceAccess:默认区全员;否则
// 显式行 ∪ 关联目标活跃参与者(project=会话成员/board_card=assignee+
// mentions/document=creator+collaborators)。
func resolveAccess(ctx context.Context, db *sql.DB, uid, companyID, wsID string) (wsRow, int, string) {
	w, ok := loadWorkspace(ctx, db, companyID, wsID)
	if !ok {
		return w, http.StatusNotFound, "workspace not found"
	}
	if w.unboundAt.Valid {
		return w, http.StatusGone, "workspace is unbound"
	}
	if w.isDefault {
		return w, 0, ""
	}
	var allowed bool
	err := db.QueryRowContext(ctx, `
		SELECT 1 FROM workspace_members WHERE workspace_id = $1 AND participant_id = $2
		UNION ALL
		SELECT 1 FROM workspace_associations a
		 WHERE a.workspace_id = $1 AND a.company_id = $3
		   AND EXISTS (SELECT 1 FROM participants p
		                WHERE p.id = $2 AND p.company_id = $3 AND p.departed_at IS NULL)
		   AND (
		     (a.target_kind = 'project' AND EXISTS (
		        SELECT 1 FROM conversations c
		         WHERE c.project_id = a.target_id AND c.company_id = $3
		           AND EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id = c.id AND cm.participant_id = $2)))
		     OR (a.target_kind = 'board_card' AND EXISTS (
		        SELECT 1 FROM board_cards bc JOIN boards b ON b.id = bc.board_id
		         WHERE bc.id = a.target_id AND b.company_id = $3
		           AND (bc.assignee_id = $2 OR bc.mentions @> to_jsonb($2::text))))
		     OR (a.target_kind = 'document' AND EXISTS (
		        SELECT 1 FROM documents d
		         WHERE d.id = a.target_id AND d.company_id = $3
		           AND (d.created_by = $2 OR d.collaborators @> to_jsonb($2::text))))
		   )
		 LIMIT 1`, wsID, uid, companyID).Scan(&allowed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return w, http.StatusInternalServerError, "membership query failed"
	}
	if err != nil || !allowed {
		return w, http.StatusForbidden, "not a member of this workspace"
	}
	return w, 0, ""
}

// resolveInside 双层防逃逸:filepath.Clean 归一 + EvalSymlinks 复检
// (新建文件回退父目录)。root 已在建区时 realpath 化。
func resolveInside(root, raw string) (abs, rel string, code int, msg string) {
	rel = strings.TrimSpace(raw)
	if strings.ContainsRune(rel, 0) {
		return "", "", http.StatusBadRequest, "invalid path"
	}
	// 绝对路径必须当逃逸拒绝:node 的 resolve(root, rel) 会以绝对 rel 为准
	// 再被 assertInside 打回,而 filepath.Join 会把绝对 rel 拼到 root 下。
	if filepath.IsAbs(rel) {
		return "", "", http.StatusBadRequest, "path escapes the workspace folder"
	}
	abs = filepath.Join(root, rel)
	if !insideRoot(root, abs) {
		return "", "", http.StatusBadRequest, "path escapes the workspace folder"
	}
	real := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		real = resolved
	} else if parent, perr := filepath.EvalSymlinks(filepath.Dir(abs)); perr == nil {
		real = filepath.Join(parent, filepath.Base(abs))
	}
	if !insideRoot(root, real) {
		return "", "", http.StatusBadRequest, "path escapes the workspace folder"
	}
	r, err := filepath.Rel(root, real)
	if err != nil {
		return "", "", http.StatusBadRequest, "invalid path"
	}
	// 根目录的相对路径是 ".";TS 的 relative(root, real) 给 ""(root 就是 root)
	if r == "." {
		r = ""
	}
	return real, r, 0, ""
}

func insideRoot(root, p string) bool {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

// text:TS `.trim().slice(0, N)` —— UTF-16 码元截断(#141 rider:
// rune 截断在代理对边界漂移,长 emoji 标题会差 1 字)。
func text(v string, max int) string {
	return httpx.UTF16Cap(strings.TrimSpace(v), max)
}

/* handlers */

func (s *Server) CreateWorkspace(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompanyRole(w, r, s.DB, uid)
	if !ok {
		return
	}
	var body struct {
		Name       string `json:"name"`
		FolderPath string `json:"folderPath"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	name := text(body.Name, 80)
	if name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "name required")
		return
	}
	rawPath := text(body.FolderPath, 4096)
	if rawPath == "" || !filepath.IsAbs(rawPath) {
		httpx.WriteError(w, http.StatusBadRequest, "folderPath must be an absolute path")
		return
	}
	folder, err := filepath.EvalSymlinks(rawPath)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "folder not found")
		return
	}
	if st, serr := os.Stat(folder); serr != nil || !st.IsDir() {
		httpx.WriteError(w, http.StatusBadRequest, "folderPath must be a directory")
		return
	}
	var bound string
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT id FROM workspaces WHERE folder_path = $1 LIMIT 1`, folder).Scan(&bound)
	if bound != "" {
		httpx.WriteError(w, http.StatusConflict, "folder already bound to a workspace")
		return
	}
	id := "ws-" + shortID()
	// #235 收编 db.WithTx:#214 后各步 500 均为 WriteInternalError(err)
	// 同构映射,#213 豁免的"文案各异"半理由消失;剩余 unique-violation
	// 409 分支由外层 isUniqueViolation(errors.As pg 23505)二分表达——
	// WithTx 将 fn 错误原样回传,23505 只可能来自 workspaces INSERT
	// (member INSERT 撞的是刚建区的全新 id;schema 无 DEFERRABLE 约束,
	// Commit 阶段不会补出 23505),回滚路径与手写版一致,响应字节不变。
	var createdAt time.Time
	if err := dbpkg.WithTx(r.Context(), s.DB, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(r.Context(), `
			INSERT INTO workspaces (id, company_id, name, folder_path)
			VALUES ($1, $2, $3, $4) RETURNING created_at`, id, companyID, name, folder).Scan(&createdAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO workspace_members (workspace_id, participant_id, added_by) VALUES ($1, $2, $2)`,
			id, uid); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if isUniqueViolation(err) {
			httpx.WriteError(w, http.StatusConflict, "folder already bound to a workspace")
			return
		}
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": name, "folderPath": folder, "isDefault": false, "createdAt": createdAt.UTC(),
	})
}

func (s *Server) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	if err := ensureDefault(r.Context(), s.DB, companyID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT w.id, w.name, w.is_default, w.created_at, count(m.participant_id)::int
		  FROM workspaces w LEFT JOIN workspace_members m ON m.workspace_id = w.id
		 WHERE w.company_id = $1 AND w.unbound_at IS NULL
		 GROUP BY w.id ORDER BY w.created_at ASC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name string
		var isDefault bool
		var createdAt time.Time
		var count int
		if rows.Scan(&id, &name, &isDefault, &createdAt, &count) == nil {
			out = append(out, map[string]any{
				"id": id, "name": name, "isDefault": isDefault,
				"createdAt": createdAt.UTC(), "explicitMemberCount": count,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) GetWorkspace(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	ws, ok := loadWorkspace(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "workspace not found")
		return
	}
	// 显式成员。slice 必须 make:删光成员后 nil 会序列化成 null,
	// 而 TS 的 spread 永远给数组(契约 members: type array required)。
	type member struct {
		ParticipantID string `json:"participantId"`
		Name          string `json:"name"`
		Kind          string `json:"kind"`
		AddedAt       any    `json:"addedAt"`
		Source        string `json:"source"`
	}
	explicit := make([]member, 0)
	explicitSet := map[string]bool{}
	explicitRows, err := s.DB.QueryContext(r.Context(), `
		SELECT m.participant_id, p.name, p.kind, m.created_at
		  FROM workspace_members m JOIN participants p
		    ON p.id = m.participant_id AND p.company_id = $2
		 WHERE m.workspace_id = $1 ORDER BY m.created_at ASC`, ws.id, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer explicitRows.Close()
	for explicitRows.Next() {
		var m member
		var added sql.NullTime
		if explicitRows.Scan(&m.ParticipantID, &m.Name, &m.Kind, &added) == nil {
			m.Source = "explicit"
			if added.Valid {
				m.AddedAt = added.Time.UTC()
			} else {
				m.AddedAt = nil
			}
			explicit = append(explicit, m)
			explicitSet[m.ParticipantID] = true
		}
	}
	// 隐式成员(关联推导;默认区并全员)
	implicitSet := implicitMembers(r.Context(), s.DB, ws.id, companyID)
	if ws.isDefault {
		allRows, err := s.DB.QueryContext(r.Context(),
			`SELECT id FROM participants WHERE company_id = $1 AND departed_at IS NULL`, companyID)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		defer allRows.Close()
		for allRows.Next() {
			var pid string
			if allRows.Scan(&pid) == nil {
				implicitSet[pid] = true
			}
		}
	}
	var derivedOnly []string
	for pid := range implicitSet {
		if !explicitSet[pid] {
			derivedOnly = append(derivedOnly, pid)
		}
	}
	implicit := make([]member, 0)
	if len(derivedOnly) > 0 {
		// 单条查询(参数数组)
		args := make([]string, len(derivedOnly))
		placeholders := make([]string, len(derivedOnly))
		for i, pid := range derivedOnly {
			args[i] = pid
			placeholders[i] = fmt.Sprintf("$%d", i+2)
		}
		rows, err := s.DB.QueryContext(r.Context(), fmt.Sprintf(`
			SELECT p.id, p.name, p.kind FROM participants p
			 WHERE p.company_id = $1 AND p.id = ANY(%s) AND p.departed_at IS NULL`,
			"ARRAY["+strings.Join(placeholders, ",")+"]::text[]"),
			append([]any{companyID}, toAny(derivedOnly)...)...)
		if err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var m member
			if rows.Scan(&m.ParticipantID, &m.Name, &m.Kind) == nil {
				m.Source = "implicit"
				m.AddedAt = nil
				implicit = append(implicit, m)
			}
		}
	}
	// 关联
	type assoc struct {
		Kind      string `json:"kind"`
		TargetID  string `json:"targetId"`
		CreatedAt any    `json:"createdAt"`
	}
	associations := []assoc{}
	assocRows, err := s.DB.QueryContext(r.Context(), `
		SELECT target_kind, target_id, created_at FROM workspace_associations
		 WHERE workspace_id = $1 ORDER BY created_at ASC`, ws.id)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer assocRows.Close()
	for assocRows.Next() {
		var a assoc
		var ca sql.NullTime
		if assocRows.Scan(&a.Kind, &a.TargetID, &ca) == nil {
			if ca.Valid {
				a.CreatedAt = ca.Time.UTC()
			}
			associations = append(associations, a)
		}
	}
	// folderPath 仅特权成员
	var role string
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
		companyID, uid).Scan(&role)
	privileged := role == "owner" || role == "admin"
	resp := map[string]any{
		"id": ws.id, "name": ws.name, "isDefault": ws.isDefault,
		"createdAt": ws.createdAt.UTC(), "unboundAt": nullTime(ws.unboundAt), "unboundBy": nullStr(ws.unboundBy),
		"members": append(explicit, implicit...), "associations": associations,
	}
	if privileged {
		resp["folderPath"] = ws.folderPath
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func nullTime(nt sql.NullTime) any {
	if nt.Valid {
		return nt.Time.UTC()
	}
	return nil
}

func nullStr(ns sql.NullString) any {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func toAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// implicitMembers 对齐 core.implicitMembers 的三分支参与者模型。
func implicitMembers(ctx context.Context, db *sql.DB, wsID, companyID string) map[string]bool {
	out := map[string]bool{}
	queries := []string{`
		SELECT DISTINCT x.pid FROM workspace_associations a,
		LATERAL (SELECT jsonb_array_elements_text(c.members) AS pid FROM conversations c
		          WHERE c.project_id = a.target_id AND c.company_id = $2) x
		 WHERE a.workspace_id = $1 AND a.company_id = $2 AND a.target_kind = 'project'`, `
		SELECT DISTINCT x.pid FROM workspace_associations a,
		LATERAL (SELECT bc.assignee_id AS pid FROM board_cards bc JOIN boards b ON b.id = bc.board_id
		          WHERE bc.id = a.target_id AND b.company_id = $2
		         UNION ALL SELECT jsonb_array_elements_text(bc.mentions) FROM board_cards bc
		          JOIN boards b ON b.id = bc.board_id WHERE bc.id = a.target_id AND b.company_id = $2) x
		 WHERE a.workspace_id = $1 AND a.company_id = $2 AND a.target_kind = 'board_card'`, `
		SELECT DISTINCT x.pid FROM workspace_associations a,
		LATERAL (SELECT d.created_by AS pid FROM documents d WHERE d.id = a.target_id AND d.company_id = $2
		         UNION ALL SELECT jsonb_array_elements_text(d.collaborators) FROM documents d
		          WHERE d.id = a.target_id AND d.company_id = $2) x
		 WHERE a.workspace_id = $1 AND a.company_id = $2 AND a.target_kind = 'document'`}
	for _, q := range queries {
		rows, err := db.QueryContext(ctx, q, wsID, companyID)
		if err != nil {
			continue
		}
		for rows.Next() {
			var pid sql.NullString
			if rows.Scan(&pid) == nil && pid.Valid && pid.String != "" {
				out[pid.String] = true
			}
		}
		rows.Close()
	}
	return out
}

func (s *Server) AddWorkspaceMember(w http.ResponseWriter, r *http.Request, id string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompanyRole(w, r, s.DB, uid)
	if !ok {
		return
	}
	ws, ok := loadWorkspace(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if ws.unboundAt.Valid {
		httpx.WriteError(w, http.StatusGone, "workspace is unbound")
		return
	}
	var body struct {
		ParticipantID string `json:"participantId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	pid := text(body.ParticipantID, 100)
	if pid == "" {
		httpx.WriteError(w, http.StatusBadRequest, "participantId required")
		return
	}
	var exists bool
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 AND departed_at IS NULL LIMIT 1`,
		pid, companyID).Scan(&exists)
	if !exists {
		httpx.WriteError(w, http.StatusNotFound, "participant not found in this company")
		return
	}
	res, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO workspace_members (workspace_id, participant_id, added_by) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, ws.id, pid, uid)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusConflict, "already a member of this workspace")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *Server) RemoveWorkspaceMember(w http.ResponseWriter, r *http.Request, id string, participantId string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompanyRole(w, r, s.DB, uid)
	if !ok {
		return
	}
	ws, ok := loadWorkspace(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if ws.unboundAt.Valid {
		httpx.WriteError(w, http.StatusGone, "workspace is unbound")
		return
	}
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM workspace_members WHERE workspace_id = $1 AND participant_id = $2`,
		ws.id, participantId)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "not an explicit member of this workspace")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

var assocKinds = map[string]bool{"project": true, "board_card": true, "document": true}

func (s *Server) AddWorkspaceAssociation(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Kind     string `json:"kind"`
		TargetID string `json:"targetId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	kind := text(body.Kind, 20)
	targetID := text(body.TargetID, 100)
	if !assocKinds[kind] {
		httpx.WriteError(w, http.StatusBadRequest, "kind must be one of project, board_card, document")
		return
	}
	if targetID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "targetId required")
		return
	}
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	var companyID string
	if kind == "document" {
		companyID, ok = httpx.ResolveCompany(w, r, s.DB, uid)
	} else {
		companyID, ok = httpx.ResolveCompanyRole(w, r, s.DB, uid)
	}
	if !ok {
		return
	}
	ws, ok := loadWorkspace(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if ws.unboundAt.Valid {
		httpx.WriteError(w, http.StatusGone, "workspace is unbound")
		return
	}
	if !targetExists(r.Context(), s.DB, companyID, kind, targetID) {
		httpx.WriteError(w, http.StatusNotFound, "associated "+kind+" not found in this company")
		return
	}
	assocID := "wa-" + shortID()
	res, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO workspace_associations (id, workspace_id, company_id, target_kind, target_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (workspace_id, target_kind, target_id) DO NOTHING`,
		assocID, ws.id, companyID, kind, targetID, uid)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusConflict, "already associated with this workspace")
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true, "kind": kind, "targetId": targetID})
}

func targetExists(ctx context.Context, db *sql.DB, companyID, kind, targetID string) bool {
	var q string
	switch kind {
	case "project":
		q = `SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`
	case "board_card":
		q = `SELECT 1 FROM board_cards bc JOIN boards b ON b.id = bc.board_id WHERE bc.id = $1 AND b.company_id = $2 LIMIT 1`
	default:
		q = `SELECT 1 FROM documents WHERE id = $1 AND company_id = $2 LIMIT 1`
	}
	var exists bool
	_ = db.QueryRowContext(ctx, q, targetID, companyID).Scan(&exists)
	return exists
}

func (s *Server) RemoveWorkspaceAssociation(w http.ResponseWriter, r *http.Request, id string, kind string, targetId string) {
	if !assocKinds[kind] {
		httpx.WriteError(w, http.StatusBadRequest, "kind must be one of project, board_card, document")
		return
	}
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	var companyID string
	if kind == "document" {
		companyID, ok = httpx.ResolveCompany(w, r, s.DB, uid)
	} else {
		companyID, ok = httpx.ResolveCompanyRole(w, r, s.DB, uid)
	}
	if !ok {
		return
	}
	ws, ok := loadWorkspace(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if ws.unboundAt.Valid {
		httpx.WriteError(w, http.StatusGone, "workspace is unbound")
		return
	}
	res, err := s.DB.ExecContext(r.Context(), `
		DELETE FROM workspace_associations WHERE workspace_id = $1 AND company_id = $2 AND target_kind = $3 AND target_id = $4`,
		ws.id, companyID, kind, targetId)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "no such association on this workspace")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func requireMember(w http.ResponseWriter, r *http.Request, db *sql.DB) (wsRow, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return wsRow{}, false
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return wsRow{}, false
	}
	ws, code, msg := resolveAccess(r.Context(), db, uid, companyID, r.PathValue("id"))
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return wsRow{}, false
	}
	return ws, true
}

func (s *Server) ListWorkspaceFiles(w http.ResponseWriter, r *http.Request, id string, params contract.ListWorkspaceFilesParams) {
	ws, ok := requireMember(w, r, s.DB)
	if !ok {
		return
	}
	abs, rel, code, msg := resolveInside(ws.folderPath, r.URL.Query().Get("path"))
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		httpx.WriteError(w, http.StatusBadRequest, "path is not a directory inside the workspace folder")
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if len(entries) > 500 {
		entries = entries[:500]
	}
	out := []map[string]any{}
	for _, e := range entries {
		var size any
		var modAt any
		if s, serr := os.Stat(filepath.Join(abs, e.Name())); serr == nil {
			size = s.Size()
			modAt = s.ModTime().UTC()
		} else {
			size = nil
			modAt = nil
		}
		out = append(out, map[string]any{
			"name": e.Name(), "dir": e.IsDir(), "size": size, "modifiedAt": modAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"path": rel, "entries": out})
}

func (s *Server) ReadWorkspaceFile(w http.ResponseWriter, r *http.Request, id string, params contract.ReadWorkspaceFileParams) {
	ws, ok := requireMember(w, r, s.DB)
	if !ok {
		return
	}
	abs, rel, code, msg := resolveInside(ws.folderPath, r.URL.Query().Get("path"))
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return
	}
	if rel == "" {
		httpx.WriteError(w, http.StatusBadRequest, "path required")
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	if st.IsDir() {
		httpx.WriteError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	if st.Size() > maxFileBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"path": rel, "body": string(content), "size": st.Size(), "modifiedAt": st.ModTime().UTC(),
	})
}

func (s *Server) WriteWorkspaceFile(w http.ResponseWriter, r *http.Request, id string, params contract.WriteWorkspaceFileParams) {
	ws, ok := requireMember(w, r, s.DB)
	if !ok {
		return
	}
	abs, rel, code, msg := resolveInside(ws.folderPath, r.URL.Query().Get("path"))
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return
	}
	if rel == "" {
		httpx.WriteError(w, http.StatusBadRequest, "path required")
		return
	}
	// 严格解码:baseline 只接受字符串 body(express.json 解析失败即 400,
	// typeof body !== 'string' 即 400)。用 RawMessage 逐键判型,杜绝
	// 「非字符串 body → 空文件 200」静默截断既有文件。
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(raw) > maxBodyBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	var payload map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	token, hasKey := payload["body"]
	if !hasKey || len(token) == 0 || token[0] != '"' {
		httpx.WriteError(w, http.StatusBadRequest, "body required (string)")
		return
	}
	var content string
	if err := json.Unmarshal(token, &content); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(content) > maxFileBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "file too large")
		return
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		httpx.WriteError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "path": rel})
}

func (s *Server) UnbindWorkspace(w http.ResponseWriter, r *http.Request, id string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompanyRole(w, r, s.DB, uid)
	if !ok {
		return
	}
	ws, ok := loadWorkspace(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "workspace not found")
		return
	}
	if ws.isDefault {
		httpx.WriteError(w, http.StatusForbidden, "the default workspace cannot be unbound")
		return
	}
	var unboundAt time.Time
	err := s.DB.QueryRowContext(r.Context(), `
		UPDATE workspaces SET unbound_at = NOW(), unbound_by = $2
		 WHERE id = $1 AND unbound_at IS NULL RETURNING unbound_at`, ws.id, uid).Scan(&unboundAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteError(w, http.StatusConflict, "workspace is already unbound")
			return
		}
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "unboundAt": unboundAt.UTC()})
}
