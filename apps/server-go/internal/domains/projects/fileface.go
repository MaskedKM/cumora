// projects 域文件面(#355 并入,#56 起源):详情(成员范围推导)/
// 显式成员/关联三件套/文件列读写/安全解绑。文件安全对齐
// 已退役 TS server 的 workspaces/core.ts 的双层防逃逸(resolve 归一 + realpath 复检,
// 新建文件回退父目录 realpath)。真目录 IO(非 mock)。
package projects

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
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/projects"
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
type projRow struct {
	id         string
	companyID  string
	name       string
	folderPath string
	isDefault  bool
	createdAt  time.Time
}

func loadProject(ctx context.Context, db *sql.DB, companyID, id string) (projRow, bool) {
	var w projRow
	var folder sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT id, company_id, name, folder_path, is_default, created_at
		  FROM projects WHERE id = $1 AND company_id = $2`, id, companyID).
		Scan(&w.id, &w.companyID, &w.name, &folder, &w.isDefault, &w.createdAt)
	if err != nil {
		return w, false
	}
	w.folderPath = folder.String
	return w, true
}

// ensureDefault 惰性建默认区(自愈;产品管理目录 <uploads 根>/workspaces/
// <companyId>,uploads 根经 config.UploadsDir() 统一解析——#208 前硬编码
// server/uploads 相对 cwd,设 env 会被无视)。
// 必须 Abs 化再落库:folder_path 的唯一约束与双重绑定防御都以绝对路径为
// 不变量,CWD 变了也不能搬家。
// EnsureDefault:默认区惰性自愈(导出供 runtime 挂载清单 #336 复用——
// daemon 同步周期拉可达清单,全新 team 不能因未开过人侧 UI 就漏掉默认区)。
// #354 起 workspace 即项目(ADR 0008):行落 projects 表,id 惯例
// ws-default-<companyId> 沿用(migration 0009 已把存量默认区按原 id 迁入)。
func EnsureDefault(ctx context.Context, db *sql.DB, companyID string) error {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM projects WHERE company_id = $1 AND is_default LIMIT 1`, companyID).Scan(&exists); err == nil && exists {
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
		INSERT INTO projects (id, company_id, name, description, folder_path, is_default)
		VALUES ($1, $2, 'Team files', '', $3, TRUE) ON CONFLICT DO NOTHING`,
		"ws-default-"+companyID, companyID, real)
	if err != nil && !isUniqueViolation(err) {
		return err
	}
	return nil
}

// EnsureProjectFolders:#354 存量无盘项目惰性补盘(ADR 0008 §3)。migration
// 是单事务纯 SQL(无 shell),建不了目录 —— 无盘项目落 folder_path NULL,
// 由读路径(人侧列表/agent 挂载清单/CLI 面)首次到达时收敛为受管目录
// <uploads 根>/projects/<id>。条件更新保证并发安全;评审 P0-2:不补盘则
// NULL 会打穿三处裸 string scan(挂载清单整列 500)。
func EnsureProjectFolders(ctx context.Context, db *sql.DB, companyID string) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM projects WHERE company_id = $1 AND folder_path IS NULL`, companyID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		folder := filepath.Join(config.UploadsDir(), "projects", id)
		if abs, err := filepath.Abs(folder); err == nil {
			folder = abs
		}
		if err := os.MkdirAll(folder, 0o755); err != nil {
			return fmt.Errorf("project folder: %w", err)
		}
		if real, err := filepath.EvalSymlinks(folder); err == nil {
			folder = real
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE projects SET folder_path = $2 WHERE id = $1 AND folder_path IS NULL`, id, folder); err != nil {
			return err
		}
	}
	return nil
}

// resolveAccess:默认区全员;否则显式成员 ∪ 挂靠会话成员 ∪ 关联目标活跃
// 参与者(board_card=assignee+mentions/document=creator+collaborators)。
// #354(ADR 0008 §5):project-kind 关联退役,"项目下会话成员"改为顶层
// 直推导(挂靠本项目的会话之成员);推导统一走 conversation_members 表
// —— 原 implicitMembers 的 legacy members jsonb 分叉随之消灭。
// departed 闸门有意只罩关联分支:挂靠推导以"还在会话里"为界(退群即出
// 域),离职者留在 conversation_members 是会话面的清理债,不在文件面拦
// (#343 评审确认的有意放宽)。
func resolveAccess(ctx context.Context, db *sql.DB, uid, companyID, wsID string) (projRow, int, string) {
	w, ok := loadProject(ctx, db, companyID, wsID)
	if !ok {
		return w, http.StatusNotFound, "project not found"
	}
	if w.isDefault {
		return w, 0, ""
	}
	var allowed bool
	err := db.QueryRowContext(ctx, `
		SELECT 1 FROM project_members WHERE project_id = $1 AND participant_id = $2
		UNION ALL
		SELECT 1 FROM conversations c
		 WHERE c.project_id = $1 AND c.company_id = $3
		   AND EXISTS (SELECT 1 FROM conversation_members cm
		                WHERE cm.conversation_id = c.id AND cm.participant_id = $2)
		UNION ALL
		SELECT 1 FROM project_associations a
		 WHERE a.project_id = $1 AND a.company_id = $3
		   AND EXISTS (SELECT 1 FROM participants p
		                WHERE p.id = $2 AND p.company_id = $3 AND p.departed_at IS NULL)
		   AND (
		     (a.target_kind = 'board_card' AND EXISTS (
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
		return w, http.StatusForbidden, "not a member of this project"
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

func (s *Server) GetProject(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	ws, ok := loadProject(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "project not found")
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
		  FROM project_members m JOIN participants p
		    ON p.id = m.participant_id AND p.company_id = $2
		 WHERE m.project_id = $1 ORDER BY m.created_at ASC`, ws.id, companyID)
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
		SELECT target_kind, target_id, created_at FROM project_associations
		 WHERE project_id = $1 ORDER BY created_at ASC`, ws.id)
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
		"createdAt": ws.createdAt.UTC(),
		"members":   append(explicit, implicit...), "associations": associations,
	}
	if privileged {
		resp["folderPath"] = ws.folderPath
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
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
		SELECT DISTINCT cm.participant_id FROM conversations c
		  JOIN conversation_members cm ON cm.conversation_id = c.id
		 WHERE c.project_id = $1 AND c.company_id = $2`, `
		SELECT DISTINCT x.pid FROM project_associations a,
		LATERAL (SELECT bc.assignee_id AS pid FROM board_cards bc JOIN boards b ON b.id = bc.board_id
		          WHERE bc.id = a.target_id AND b.company_id = $2
		         UNION ALL SELECT jsonb_array_elements_text(bc.mentions) FROM board_cards bc
		          JOIN boards b ON b.id = bc.board_id WHERE bc.id = a.target_id AND b.company_id = $2) x
		 WHERE a.project_id = $1 AND a.company_id = $2 AND a.target_kind = 'board_card'`, `
		SELECT DISTINCT x.pid FROM project_associations a,
		LATERAL (SELECT d.created_by AS pid FROM documents d WHERE d.id = a.target_id AND d.company_id = $2
		         UNION ALL SELECT jsonb_array_elements_text(d.collaborators) FROM documents d
		          WHERE d.id = a.target_id AND d.company_id = $2) x
		 WHERE a.project_id = $1 AND a.company_id = $2 AND a.target_kind = 'document'`}
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

func (s *Server) AddProjectMember(w http.ResponseWriter, r *http.Request, id string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompanyRole(w, r, s.DB, uid)
	if !ok {
		return
	}
	ws, ok := loadProject(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "project not found")
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
		INSERT INTO project_members (project_id, participant_id, added_by) VALUES ($1, $2, $3)
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

func (s *Server) RemoveProjectMember(w http.ResponseWriter, r *http.Request, id string, participantId string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompanyRole(w, r, s.DB, uid)
	if !ok {
		return
	}
	ws, ok := loadProject(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "project not found")
		return
	}
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM project_members WHERE project_id = $1 AND participant_id = $2`,
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

// #354(ADR 0008 §5):project-kind 关联退役 —— 项目自带盘,成员由"挂靠
// 会话成员"直推导;白名单只剩 board_card/document(存量行 migration 已清)。
var assocKinds = map[string]bool{"board_card": true, "document": true}

func (s *Server) AddProjectAssociation(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Kind     string `json:"kind"`
		TargetID string `json:"targetId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	kind := text(body.Kind, 20)
	targetID := text(body.TargetID, 100)
	if !assocKinds[kind] {
		httpx.WriteError(w, http.StatusBadRequest, "kind must be one of board_card, document")
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
	ws, ok := loadProject(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "project not found")
		return
	}
	if !targetExists(r.Context(), s.DB, companyID, kind, targetID) {
		httpx.WriteError(w, http.StatusNotFound, "associated "+kind+" not found in this company")
		return
	}
	assocID := "wa-" + shortID()
	res, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO project_associations (id, project_id, company_id, target_kind, target_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (project_id, target_kind, target_id) DO NOTHING`,
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
	case "board_card":
		q = `SELECT 1 FROM board_cards bc JOIN boards b ON b.id = bc.board_id WHERE bc.id = $1 AND b.company_id = $2 LIMIT 1`
	default:
		q = `SELECT 1 FROM documents WHERE id = $1 AND company_id = $2 LIMIT 1`
	}
	var exists bool
	_ = db.QueryRowContext(ctx, q, targetID, companyID).Scan(&exists)
	return exists
}

func (s *Server) RemoveProjectAssociation(w http.ResponseWriter, r *http.Request, id string, kind string, targetId string) {
	if !assocKinds[kind] {
		httpx.WriteError(w, http.StatusBadRequest, "kind must be one of board_card, document")
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
	ws, ok := loadProject(r.Context(), s.DB, companyID, id)
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "project not found")
		return
	}
	res, err := s.DB.ExecContext(r.Context(), `
		DELETE FROM project_associations WHERE project_id = $1 AND company_id = $2 AND target_kind = $3 AND target_id = $4`,
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

func requireMember(w http.ResponseWriter, r *http.Request, db *sql.DB) (projRow, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return projRow{}, false
	}
	companyID, ok := httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return projRow{}, false
	}
	ws, code, msg := resolveAccess(r.Context(), db, uid, companyID, r.PathValue("id"))
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return projRow{}, false
	}
	return ws, true
}

func (s *Server) ListProjectFiles(w http.ResponseWriter, r *http.Request, id string, params contract.ListProjectFilesParams) {
	ws, ok := requireMember(w, r, s.DB)
	if !ok {
		return
	}
	abs, rel, code, msg := resolveInside(ws.folderPath, r.URL.Query().Get("path"))
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return
	}
	if msg := RejectReserved(rel); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, msg)
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
		// .cumora 平台内部(版本快照/冲突副本)与 .git 仓库内部:文件树
		// 不展示(.git 同 RejectReserved 的排除语义,#265)。
		if rel == "" && (strings.EqualFold(e.Name(), reservedPrefix) || strings.EqualFold(e.Name(), ".git")) {
			continue
		}
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

func (s *Server) ReadProjectFile(w http.ResponseWriter, r *http.Request, id string, params contract.ReadProjectFileParams) {
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
	if msg := RejectReserved(rel); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, msg)
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
		"mtimeNanos": strconv.FormatInt(st.ModTime().UnixNano(), 10),
	})
}

func (s *Server) WriteProjectFile(w http.ResponseWriter, r *http.Request, id string, params contract.WriteProjectFileParams) {
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
	if msg := RejectReserved(rel); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	// #337 CAS:可选 expectedMtimeNanos(query)。失配 = 别人在你 stat 之后
	// 写过 —— 412 + 盘上最新 mtime + 挑战者内容留 .conflict 副本(永不
	// 静默丢),调用方重读重判后再试(与消息面 HELD 同构)。
	var expected *int64
	if rawExp := strings.TrimSpace(r.URL.Query().Get("expectedMtimeNanos")); rawExp != "" {
		v, err := strconv.ParseInt(rawExp, 10, 64)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "expectedMtimeNanos must be an integer")
			return
		}
		expected = &v
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
	// 悬空 symlink 写穿(#342 评审 P2,纵深):EvalSymlinks 对悬空链接
	// 回退父目录而 Stat 跟随失败,WriteFile 会沿链接写到根外 —— 落盘
	// 前拒一切非常规文件(不存在 = 新建,放行)。
	if fi, err := os.Lstat(abs); err == nil && !fi.Mode().IsRegular() {
		httpx.WriteError(w, http.StatusBadRequest, "path is not a regular file")
		return
	}
	// CAS 检查(在 body 解码后、落盘前):失配 → 挑战者内容留副本 + 412。
	if expected != nil {
		cur := int64(0)
		if st, err := os.Stat(abs); err == nil && !st.IsDir() {
			cur = st.ModTime().UnixNano()
		}
		if cur != *expected {
			uid, _ := httpx.RequireAuth(w, r)
			conflict := SaveConflictCopy(ws.folderPath, rel, uid, content)
			httpx.WriteJSON(w, http.StatusPreconditionFailed, map[string]any{
				"error": "stale write", "currentMtimeNanos": strconv.FormatInt(cur, 10), "conflictPath": conflict,
			})
			return
		}
	}
	// 写前快照:覆盖/新建路径上的旧内容留档(10 版帽,#337)。
	SnapshotVersion(ws.folderPath, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "path": rel,
		"mtimeNanos": strconv.FormatInt(fileMtimeNanos(abs), 10)})
}

// fileMtimeNanos:写后回读 mtime(失败 0)—— CAS 回执给调用方下一轮
// expected 用。
func fileMtimeNanos(abs string) int64 {
	if st, err := os.Stat(abs); err == nil {
		return st.ModTime().UnixNano()
	}
	return 0
}

// maxBinaryBytes:#338 multipart 二进制帽(对齐 uploads 域 25MB;文本面
// 维持 maxFileBytes 2MB 不变)。
const maxBinaryBytes = 25 * 1024 * 1024

// UploadProjectFile:#338 multipart 上传 —— 人侧 UI 通道(agent 走挂载
// 盘原生写入)。流式读 file part(LimitReader 25MB+1),复用全套写路径
// 防护:requireMember → resolveInside 防逃逸 → RejectReserved/RejectRoot
// → 写前快照 → 落盘。字段序宽容:path 与 file 任意先后。
func (s *Server) UploadProjectFile(w http.ResponseWriter, r *http.Request, id string) {
	ws, ok := requireMember(w, r, s.DB)
	if !ok {
		return
	}
	// 总请求体帽(#342 评审 P1):NextPart/Close 会排空剩余 part 字节,
	// 只限单 file part 时带宽/CPU 无界 —— 对齐文本面 34MB 姿态,
	// 25MB 文件 + path part + 边界开销取 26MB 总帽。
	r.Body = http.MaxBytesReader(w, r.Body, maxBinaryBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "multipart form required")
		return
	}
	var rel, abs string
	var havePath bool
	var content []byte
	var haveFile bool
	parts := 0
	for {
		part, perr := reader.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid multipart body")
			return
		}
		parts++
		if parts > 8 {
			httpx.WriteError(w, http.StatusBadRequest, "too many parts")
			return
		}
		switch part.FormName() {
		case "path":
			if !havePath {
				b, perr2 := io.ReadAll(io.LimitReader(part, 4097))
				if perr2 != nil || len(b) > 4096 {
					httpx.WriteError(w, http.StatusBadRequest, "path too long")
					return
				}
				rel = strings.TrimSpace(string(b))
				havePath = true
			}
		case "file":
			if !haveFile {
				b, ferr := io.ReadAll(io.LimitReader(part, maxBinaryBytes+1))
				if ferr != nil {
					httpx.WriteError(w, http.StatusBadRequest, "invalid file part")
					return
				}
				if len(b) > maxBinaryBytes {
					httpx.WriteError(w, http.StatusRequestEntityTooLarge, "file too large (25MB limit)")
					return
				}
				content = b
				haveFile = true
			}
		}
		_ = part.Close()
	}
	if !havePath || !haveFile || rel == "" {
		httpx.WriteError(w, http.StatusBadRequest, "path and file required")
		return
	}
	abs, relResolved, code, msg := resolveInside(ws.folderPath, rel)
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return
	}
	if relResolved == "" {
		httpx.WriteError(w, http.StatusBadRequest, "path required")
		return
	}
	if msg := RejectReserved(relResolved); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		httpx.WriteError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	if fi, err := os.Lstat(abs); err == nil && !fi.Mode().IsRegular() {
		httpx.WriteError(w, http.StatusBadRequest, "path is not a regular file")
		return
	}
	SnapshotVersion(ws.folderPath, relResolved)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "path": relResolved, "size": len(content),
		"mtimeNanos": strconv.FormatInt(fileMtimeNanos(abs), 10),
	})
}

// rawContentTypes:图片扩展名 → Content-Type(预览需要;其余一律
// application/octet-stream —— 不猜更多,浏览器按下载处理)。
var rawContentTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml",
}

// ReadProjectFileRaw:#338 原始字节读 —— 图片预览/下载通道。二进制读
// 帽 25MB(与上传面对齐;文本 JSON 面维持 2MB)。
func (s *Server) ReadProjectFileRaw(w http.ResponseWriter, r *http.Request, id string, params contract.ReadProjectFileRawParams) {
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
	if msg := RejectReserved(rel); msg != "" {
		httpx.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() || !st.Mode().IsRegular() {
		httpx.WriteError(w, http.StatusNotFound, "file not found")
		return
	}
	if st.Size() > maxBinaryBytes {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "file too large (25MB limit)")
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer f.Close()
	ct := rawContentTypes[strings.ToLower(filepath.Ext(rel))]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
