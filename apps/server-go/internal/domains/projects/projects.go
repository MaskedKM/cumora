// domains/projects —— 项目域(#68 补齐):列表/创建/更新(owner/admin)/
// 会话挂接;#354(ADR 0008)起吸收工作区:项目=一摊工作的唯一容器
// (对话挂靠+文件夹),归档退役、删除成为唯一生命周期出口。
package projects

import (
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/projects"
	dbpkg "github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// Server:contract.projects ServerInterface 的域实现(#187 机械迁移,
// documents 范式)。方法体自原闭包工厂/直接 handler 原样搬运。
type Server struct{ DB *sql.DB }

// 编译期接口把关:规范改动 operation 而域未跟 = 构建红。
var _ contract.ServerInterface = (*Server)(nil)

// Mount:注册串来自契约生成物(pattern 即规范,#139)。
func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

// requireRole:owner/admin 门(TS requireCompanyRole;403 恒同文案)。
func requireRole(w http.ResponseWriter, r *http.Request, db *sql.DB) (string, bool) {
	uid, companyID, ok := httpx.RequireCompany(w, r, db)
	if !ok {
		return "", false
	}
	var role string
	if err := db.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
		companyID, uid).Scan(&role); err != nil {
		role = "member"
	}
	if role != "owner" && role != "admin" {
		httpx.WriteError(w, http.StatusForbidden, "this action requires an owner or admin of the team")
		return "", false
	}
	return companyID, true
}

func decodeBody(r *http.Request) map[string]json.RawMessage {
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body
}

func bodyAny(body map[string]json.RawMessage, key string) (any, bool) {
	raw, ok := body[key]
	if !ok {
		return nil, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return v, true
}

func (s *Server) ListProjects(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, name, description, color, status,
		       created_at, archived_at, folder_path, is_default,
		       (SELECT COUNT(*)::int FROM conversations WHERE project_id = projects.id)
		  FROM projects
		 WHERE company_id = $1
		 ORDER BY is_default DESC, created_at DESC`, tenant)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, status string
		var description, color, folder sql.NullString
		var createdAt time.Time
		var archivedAt sql.NullTime
		var isDefault bool
		var convoCount int
		if err := rows.Scan(&id, &name, &description, &color, &status, &createdAt, &archivedAt, &folder, &isDefault, &convoCount); err != nil {
			continue
		}
		row := map[string]any{
			"id": id, "name": name,
			"description": nullAny(description), "color": nullAny(color),
			"status": status, "createdAt": httpx.ISOms(createdAt),
			"folderPath": nullAny(folder), "isDefault": isDefault,
			"conversationCount": convoCount,
		}
		if archivedAt.Valid {
			row["archivedAt"] = httpx.ISOms(archivedAt.Time)
		} else {
			row["archivedAt"] = nil
		}
		out = append(out, row)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func nullAny(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func (s *Server) CreateProject(w http.ResponseWriter, r *http.Request) {
	_, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	body := decodeBody(r)
	nameRaw, _ := bodyAny(body, "name")
	descRaw, _ := bodyAny(body, "description")
	colorRaw, hasColor := bodyAny(body, "color")
	// F16:TS create 是 String(x ?? '') 强转(非 typeof 门),color 另有
	// JS truthy 前置(0/""/null→null,对象/数组恒真)。
	name := httpx.UTF16Cap(strings.TrimSpace(httpx.JSStringOrNullish(nameRaw)), 80)
	description := httpx.UTF16Cap(httpx.JSStringOrNullish(descRaw), 1000)
	var color any
	if hasColor && httpx.JSTruthy(colorRaw) {
		color = httpx.UTF16Cap(httpx.JSToString(colorRaw), 200)
	}
	if name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "name required")
		return
	}
	// #354(ADR 0008 §3)盘强制必绑:默认在受管目录自动建空盘(零额外输入);
	// body.folderPath 可自填已有路径(绑代码 repo 场景)——校验语义与建工作区
	// 同款(存在目录 + realpath + 一文件夹至多一项目)。
	folder := ""
	if fpRaw, has := bodyAny(body, "folderPath"); has {
		folder = strings.TrimSpace(httpx.JSStringOrNullish(fpRaw))
	}
	if folder != "" {
		real, err := filepath.EvalSymlinks(folder)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "folder not found")
			return
		}
		if st, serr := os.Stat(real); serr != nil || !st.IsDir() {
			httpx.WriteError(w, http.StatusBadRequest, "folderPath must be a directory")
			return
		}
		var bound string
		_ = s.DB.QueryRowContext(r.Context(),
			`SELECT id FROM projects WHERE folder_path = $1 LIMIT 1`, real).Scan(&bound)
		if bound != "" {
			httpx.WriteError(w, http.StatusConflict, "folder already bound to a project")
			return
		}
		folder = real
	}
	id := "p-" + randHex10()
	if folder == "" {
		auto := filepath.Join(config.UploadsDir(), "projects", id)
		if abs, err := filepath.Abs(auto); err == nil {
			auto = abs
		}
		if err := os.MkdirAll(auto, 0o755); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
		if real, err := filepath.EvalSymlinks(auto); err == nil {
			folder = real
		} else {
			folder = auto
		}
	}
	var colorArg any
	if s, ok := color.(string); ok {
		colorArg = s
	}
	if _, err := s.DB.ExecContext(r.Context(),
		`INSERT INTO projects (id, company_id, name, description, color, folder_path, is_default)
		 VALUES ($1, $2, $3, $4, $5, $6, FALSE)`,
		id, tenant, name, description, colorArg, folder); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": name, "description": description, "color": colorArg, "status": "active",
		"folderPath": folder, "isDefault": false,
	})
}

func (s *Server) UpdateProject(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	var one int
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`, id, tenant).Scan(&one); err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	body := decodeBody(r)
	sets := []string{}
	params := []any{}
	add := func(v any, col string) {
		params = append(params, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(params)))
	}
	// TS:键存在且为 string 才 trim/slice 更新;color 键存在为 null
	// 则显式清空。
	if v, has := bodyAny(body, "name"); has {
		if s, isStr := v.(string); isStr {
			add(httpx.UTF16Cap(strings.TrimSpace(s), 80), "name")
		}
	}
	if v, has := bodyAny(body, "description"); has {
		if s, isStr := v.(string); isStr {
			add(httpx.UTF16Cap(s, 1000), "description")
		}
	}
	if v, has := bodyAny(body, "color"); has {
		if s, isStr := v.(string); isStr {
			add(httpx.UTF16Cap(s, 200), "color")
		} else if v == nil {
			add(nil, "color")
		}
	}
	if len(sets) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	params = append(params, id, tenant)
	if _, err := s.DB.ExecContext(r.Context(),
		fmt.Sprintf(`UPDATE projects SET %s WHERE id = $%d AND company_id = $%d`,
			strings.Join(sets, ", "), len(params)-1, len(params)), params...); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) ArchiveProject(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	archive := true
	if v, has := bodyAny(decodeBody(r), "archive"); has && v == false {
		archive = false
	}
	var stmt string
	if archive {
		stmt = `UPDATE projects SET status = 'archived', archived_at = NOW() WHERE id = $1 AND company_id = $2`
	} else {
		stmt = `UPDATE projects SET status = 'active', archived_at = NULL WHERE id = $1 AND company_id = $2`
	}
	if _, err := s.DB.ExecContext(r.Context(), stmt, id, tenant); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	status := "active"
	if archive {
		status = "archived"
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func (s *Server) AttachProject(w http.ResponseWriter, r *http.Request, id string) {
	uid, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	body := decodeBody(r)
	// undefined(缺键/非串非 null)→ 400;null → 解绑;串 → 绑定。
	raw, has := bodyAny(body, "projectId")
	if !has {
		httpx.WriteError(w, http.StatusBadRequest, "projectId required (string or null to detach)")
		return
	}
	var projectID any
	switch v := raw.(type) {
	case nil:
		projectID = nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			httpx.WriteError(w, http.StatusBadRequest, "projectId required (string or null to detach)")
			return
		}
		projectID = s
	default:
		httpx.WriteError(w, http.StatusBadRequest, "projectId required (string or null to detach)")
		return
	}
	var membersJSON string
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2`, id, tenant).
		Scan(&membersJSON)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	isMember := false
	for _, m := range members {
		if m == uid {
			isMember = true
			break
		}
	}
	if !isMember {
		httpx.WriteError(w, http.StatusForbidden, "only members can change the project")
		return
	}
	if pid, isStr := projectID.(string); isStr {
		var one int
		if err := s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`, pid, tenant).Scan(&one); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "unknown project")
			return
		}
	}
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE conversations SET project_id = $2, updated_at = NOW() WHERE id = $1 AND company_id = $3`,
		id, projectID, tenant); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "projectId": projectID})
}

// DeleteProject:#354(ADR 0008 §6)项目删除 —— 生命周期的唯一出口
// (归档/解绑已退役)。级联语义:对话经 FK ON DELETE SET NULL 保留、交付
// 台账经 FK SET NULL 随卡片存活(0007 可追溯意图的延续)、成员/关联行
// 清理;盘文件原地保留(平台不代删真实文件,受管目录可手动清理)。
// is_default 项目(团队文件公共盘)不可删。
func (s *Server) DeleteProject(w http.ResponseWriter, r *http.Request, id string) {
	tenant, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	var isDefault bool
	var folder sql.NullString
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT is_default, folder_path FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`,
		id, tenant).Scan(&isDefault, &folder)
	if err == sql.ErrNoRows {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if isDefault {
		httpx.WriteError(w, http.StatusForbidden, "the default project (team files) cannot be deleted")
		return
	}
	if err := dbpkg.WithTx(r.Context(), s.DB, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM workspace_members WHERE workspace_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(),
			`DELETE FROM workspace_associations WHERE workspace_id = $1`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(r.Context(),
			`DELETE FROM projects WHERE id = $1 AND company_id = $2`, id, tenant)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return sql.ErrNoRows
		}
		return nil
	}); err != nil {
		if err == sql.ErrNoRows {
			httpx.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		httpx.WriteInternalError(w, r, err)
		return
	}
	// conversations.project_id / card_deliveries.workspace_id 由 FK
	// ON DELETE SET NULL 自动置空;盘目录未删。
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "folderKept": nullAny(folder),
	})
}

func randHex10() string {
	b := make([]byte, 5)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}
