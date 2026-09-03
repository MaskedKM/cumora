// domains/skills —— #261 公司级 Skills 库(公司 SOP 手册):CRUD 面。
// 分发半边在 computers 域(/api/computers/me/skills*:daemon 按内容哈希
// 拉清单与整包,物化到各引擎原生 skills 目录)。写面 privileged
// (owner/admin):手册是公司级资产;读面全员。body 便捷位:省略 files
// 时服务端组装单文件 SKILL.md(frontmatter name/description + body),
// 管理页与 CLI 的最常用路径一键成形。
package skills

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/skills"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// SkillFile:包内一个文件(path 相对技能根,SKILL.md 必在根)。
type SkillFile struct {
	Path string `json:"path"`
	Body string `json:"body"`
}

// Server:contract.ServerInterface(skills tag)的域实现。
type Server struct{ DB *sql.DB }

var _ contract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

const (
	skillMaxFiles    = 100
	skillMaxFileBody = 256 * 1024
)

var skillNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)
var skillUnsafePathRe = regexp.MustCompile(`[\\:*?<>"|]`)

// validateSkillName:Agent Skills 规范(与 agent 私有 skills 同规则,
// 两个命名空间共用一套约束,跨面搬运零转换)。
func validateSkillName(name string) string {
	if name == "" {
		return "name required"
	}
	if len(name) > 64 {
		return "name length must be 1–64 characters"
	}
	if !skillNameRe.MatchString(name) {
		return "name may only contain lowercase a-z, 0-9, and hyphens"
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return "name may not start or end with a hyphen"
	}
	if strings.Contains(name, "--") {
		return "name may not contain consecutive hyphens"
	}
	return ""
}

// validateSkillFiles:文件集校验(SKILL.md 必在根;路径穿越/危险字符/
// 条数/单文件大小上限)。返回排序后的副本(hash 与落库共用同一序)。
func validateSkillFiles(files []SkillFile) ([]SkillFile, string) {
	if len(files) == 0 {
		return nil, "files must not be empty"
	}
	if len(files) > skillMaxFiles {
		return nil, fmt.Sprintf("too many files: %d > %d", len(files), skillMaxFiles)
	}
	hasSkillMd := false
	for _, f := range files {
		if len(f.Path) == 0 || len(f.Path) > 200 {
			return nil, fmt.Sprintf("bad path length: %s", f.Path)
		}
		if strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") || skillUnsafePathRe.MatchString(f.Path) {
			return nil, fmt.Sprintf("unsafe path: %s", f.Path)
		}
		if len(f.Body) > skillMaxFileBody {
			return nil, fmt.Sprintf("file %s > %d bytes", f.Path, skillMaxFileBody)
		}
		if f.Path == "SKILL.md" {
			hasSkillMd = true
		}
	}
	if !hasSkillMd {
		return nil, "files must include SKILL.md at the skill root"
	}
	out := append([]SkillFile(nil), files...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, ""
}

// bundleHash:内容寻址键——文件按 path 排序后长度前缀拼接体做 sha256。
// 路径与正文都参与;同内容必同键(公司间重复包共享 daemon 缓存),改一
// 字节即换键(整包重物化)。键不含 id/时间戳——daemon 只认内容。
func bundleHash(files []SkillFile) string {
	sorted := append([]SkillFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%d:%s%d:%s", len(f.Path), f.Path, len(f.Body), f.Body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// composeSkillMd:body 便捷位的组装形——frontmatter name/description
// 按引擎原生加载器的渐进披露约定给足。
func composeSkillMd(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body
}

// privileged:owner/admin(documents 域同款治理规则)。
func privileged(db *sql.DB, companyID, userID string) bool {
	var role string
	if err := db.QueryRow(
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2`,
		companyID, userID).Scan(&role); err != nil {
		return false
	}
	return role == "owner" || role == "admin"
}

func (s *Server) ListCompanySkills(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, name, description, bundle_hash, files, created_by, created_at, updated_at
		  FROM company_skills WHERE company_id = $1
		 ORDER BY name ASC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	type skillOut struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		BundleHash  string `json:"bundleHash"`
		FileCount   int    `json:"fileCount"`
		CreatedBy   any    `json:"createdBy"`
		CreatedAt   string `json:"createdAt"`
		UpdatedAt   string `json:"updatedAt"`
	}
	out := []skillOut{}
	for rows.Next() {
		var sk skillOut
		var files json.RawMessage
		var createdBy sql.NullString
		var cAt, uAt sql.NullTime
		if rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.BundleHash, &files, &createdBy, &cAt, &uAt) != nil {
			continue
		}
		var fl []SkillFile
		_ = json.Unmarshal(files, &fl)
		sk.FileCount = len(fl)
		if createdBy.Valid {
			sk.CreatedBy = createdBy.String
		}
		sk.CreatedAt = httpx.ISOms(cAt.Time)
		sk.UpdatedAt = httpx.ISOms(uAt.Time)
		out = append(out, sk)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"skills": out})
}

func (s *Server) GetCompanySkill(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var name, description, hash string
	var filesRaw json.RawMessage
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT name, description, bundle_hash, files FROM company_skills
		 WHERE id = $1 AND company_id = $2`, id, companyID).
		Scan(&name, &description, &hash, &filesRaw)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "skill not found")
		return
	}
	var files []SkillFile
	_ = json.Unmarshal(filesRaw, &files)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "description": description, "bundleHash": hash, "files": files,
	})
}

// skillBody:create/update 请求体(files 与 body 二选一;files 优先)。
type skillBody struct {
	Name        *string      `json:"name"`
	Description *string      `json:"description"`
	Body        *string      `json:"body"`
	Files       *[]SkillFile `json:"files"`
}

func (s *Server) CreateCompanySkill(w http.ResponseWriter, r *http.Request) {
	userID, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	if !privileged(s.DB, companyID, userID) {
		httpx.WriteError(w, http.StatusForbidden, "owner or admin required")
		return
	}
	var in skillBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := ""
	if in.Name != nil {
		name = strings.TrimSpace(*in.Name)
	}
	description := ""
	if in.Description != nil {
		description = strings.TrimSpace(*in.Description)
	}
	if nameErr := validateSkillName(name); nameErr != "" {
		httpx.WriteError(w, http.StatusBadRequest, nameErr)
		return
	}
	if description == "" || len(description) > 1024 {
		httpx.WriteError(w, http.StatusBadRequest, "description must be 1–1024 chars")
		return
	}
	files := resolveFiles(name, description, in)
	files, filesErr := validateSkillFiles(files)
	if filesErr != "" {
		httpx.WriteError(w, http.StatusBadRequest, filesErr)
		return
	}
	flJSON, _ := json.Marshal(files)
	hash := bundleHash(files)
	id := httpx.UUIDHex()
	var clash string
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT name FROM company_skills WHERE company_id = $1 AND name = $2`,
		companyID, name).Scan(&clash); err == nil {
		httpx.WriteError(w, http.StatusConflict, fmt.Sprintf("skill %q already exists", name))
		return
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO company_skills (id, company_id, name, description, files, bundle_hash, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		id, companyID, name, description, flJSON, hash, userID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "bundleHash": hash})
}

func (s *Server) UpdateCompanySkill(w http.ResponseWriter, r *http.Request, id string) {
	userID, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	if !privileged(s.DB, companyID, userID) {
		httpx.WriteError(w, http.StatusForbidden, "owner or admin required")
		return
	}
	var existing struct {
		name        string
		description string
		files       []SkillFile
	}
	var filesRaw json.RawMessage
	err := s.DB.QueryRowContext(r.Context(),
		`SELECT name, description, files FROM company_skills WHERE id = $1 AND company_id = $2`,
		id, companyID).Scan(&existing.name, &existing.description, &filesRaw)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "skill not found")
		return
	}
	_ = json.Unmarshal(filesRaw, &existing.files)

	var in skillBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	description := existing.description
	if in.Description != nil {
		description = strings.TrimSpace(*in.Description)
	}
	if description == "" || len(description) > 1024 {
		httpx.WriteError(w, http.StatusBadRequest, "description must be 1–1024 chars")
		return
	}
	if in.Description == nil && in.Body == nil && in.Files == nil {
		httpx.WriteError(w, http.StatusBadRequest, "nothing to update (description, body or files)")
		return
	}
	// description-only 变更不碰文件(既有集原样;SKILL.md frontmatter 仅
	// 在 body/files 提供时随包重写)。
	files := existing.files
	if in.Files != nil || in.Body != nil {
		files = resolveFiles(existing.name, description, in)
	}
	files, filesErr := validateSkillFiles(files)
	if filesErr != "" {
		httpx.WriteError(w, http.StatusBadRequest, filesErr)
		return
	}
	flJSON, _ := json.Marshal(files)
	hash := bundleHash(files)
	if _, err := s.DB.ExecContext(r.Context(), `
		UPDATE company_skills SET description = $3, files = $4, bundle_hash = $5, updated_at = now()
		 WHERE id = $1 AND company_id = $2`,
		id, companyID, description, flJSON, hash); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "bundleHash": hash})
}

func (s *Server) DeleteCompanySkill(w http.ResponseWriter, r *http.Request, id string) {
	userID, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	if !privileged(s.DB, companyID, userID) {
		httpx.WriteError(w, http.StatusForbidden, "owner or admin required")
		return
	}
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM company_skills WHERE id = $1 AND company_id = $2`, id, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "skill not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resolveFiles:请求体 → 文件集。files 显式给则原样(调用方校验);否则
// body 便捷位组装单文件 SKILL.md;两者皆缺 → 既有文件集(update 路径,
// description-only 变更不碰文件)。
func resolveFiles(name, description string, in skillBody) []SkillFile {
	if in.Files != nil {
		return *in.Files
	}
	if in.Body != nil {
		return []SkillFile{{Path: "SKILL.md", Body: composeSkillMd(name, description, *in.Body)}}
	}
	return nil
}
