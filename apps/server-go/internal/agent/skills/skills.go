// agent 包 skills CLI 面 —— cli.ts cmdSkills(list/read/create/delete/
// search/install)+ skills.ts 的校验与 SkillHub HTTP。技能存在
// agent_workspace 的 skills/<name>/ 下,按 agent 隔离(渐进披露:唤醒
// 提示只带 name+description)。
package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	agent "github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
)

// Domain:域子包接收器——嵌入 agent.Service(内核),方法体与拆包前逐字
// 对齐(#140 刀法)。
type Domain struct {
	*agent.Service
}

// cliValidateSkillName:Agent Skills 规范校验;nil 表示通过。
func cliValidateSkillName(name string) string {
	if name == "" {
		return "name required"
	}
	if len(name) > 64 {
		return "name length must be 1–64 characters"
	}
	if !cliSkillNameRe.MatchString(name) {
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

var cliSkillNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

/* ───────────── SkillHub HTTP ───────────── */

type cliSkillHubHit struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Version     *string `json:"version,omitempty"`
	Author      *string `json:"author,omitempty"`
	InstallURL  *string `json:"install_url,omitempty"`
}

type cliSkillManifestFile struct {
	Path string `json:"path"`
	Body string `json:"body"`
}

type cliSkillManifest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Version     *string                `json:"version"`
	Author      *string                `json:"author"`
	Files       []cliSkillManifestFile `json:"files"`
}

const (
	skillHubTimeout  = 10 * time.Second
	skillMaxFiles    = 100
	skillMaxFileBody = 256 * 1024
)

var httpClientSkillHub = &http.Client{Timeout: skillHubTimeout}

func cliSkillHubBase() string { return strings.TrimRight(config.SkillHubURL(), "/") }

// cliSearchSkillHub:GET <hub>/search?q=;非数组响应报错。
func cliSearchSkillHub(query, hubURL string) ([]cliSkillHubHit, error) {
	if hubURL == "" {
		return nil, fmt.Errorf("SkillHub URL not configured — set SKILLHUB_URL on the server")
	}
	url := strings.TrimRight(hubURL, "/") + "/search?q=" + urlQueryEscape(query)
	resp, err := httpClientSkillHub.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub search failed: HTTP %d", resp.StatusCode)
	}
	var hits []cliSkillHubHit
	if err := json.Unmarshal(body, &hits); err != nil {
		return nil, fmt.Errorf("hub search returned non-array")
	}
	return hits, nil
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			for _, c := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		}
	}
	return b.String()
}

// cliFetchSkillManifest:id 或完整 URL → manifest。
func cliFetchSkillManifest(idOrURL, hubURL string) (cliSkillManifest, error) {
	var url string
	if agent.HTTPPrefixRe.MatchString(idOrURL) {
		url = idOrURL
	} else {
		if hubURL == "" {
			return cliSkillManifest{}, fmt.Errorf("SkillHub URL not configured — set SKILLHUB_URL or pass a full install URL")
		}
		url = strings.TrimRight(hubURL, "/") + "/skills/" + urlQueryEscape(idOrURL)
	}
	resp, err := httpClientSkillHub.Get(url)
	if err != nil {
		return cliSkillManifest{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return cliSkillManifest{}, fmt.Errorf("hub install failed: HTTP %d", resp.StatusCode)
	}
	var m cliSkillManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return cliSkillManifest{}, fmt.Errorf("manifest is not an object")
	}
	if m.Name == "" || m.Description == "" || m.Files == nil {
		return cliSkillManifest{}, fmt.Errorf("manifest missing required fields (name, description, files[])")
	}
	return m, nil
}

var skillUnsafePathRe = regexp.MustCompile(`[\\:*?<>"|]`)

// cliInstallSkillFromManifest:校验(名称/大小/路径穿越/SKILL.md 必在根)
// + 逐文件 upsert 到 agent_workspace;拒绝覆盖同名技能。
func (s *Domain) cliInstallSkillFromManifest(ctx context.Context, agentID string, manifest cliSkillManifest) (string, int, error) {
	if nameErr := cliValidateSkillName(manifest.Name); nameErr != "" {
		return "", 0, fmt.Errorf("skill name invalid: %s", nameErr)
	}
	if manifest.Description == "" || len(manifest.Description) > 1024 {
		return "", 0, fmt.Errorf("description must be 1–1024 chars")
	}
	if len(manifest.Files) == 0 {
		return "", 0, fmt.Errorf("manifest has no files")
	}
	if len(manifest.Files) > skillMaxFiles {
		return "", 0, fmt.Errorf("too many files: %d > %d", len(manifest.Files), skillMaxFiles)
	}
	hasSkillMd := false
	for _, f := range manifest.Files {
		if f.Path == "SKILL.md" {
			hasSkillMd = true
		}
	}
	if !hasSkillMd {
		return "", 0, fmt.Errorf("manifest must include SKILL.md at the skill root")
	}
	for _, f := range manifest.Files {
		if len(f.Path) == 0 || len(f.Path) > 200 {
			return "", 0, fmt.Errorf("bad path length: %s", f.Path)
		}
		if strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") || skillUnsafePathRe.MatchString(f.Path) {
			return "", 0, fmt.Errorf("unsafe path: %s", f.Path)
		}
		if len(f.Body) > skillMaxFileBody {
			return "", 0, fmt.Errorf("file %s > %d bytes", f.Path, skillMaxFileBody)
		}
	}
	var existing string
	err := s.DB.QueryRowContext(ctx,
		`SELECT path FROM agent_workspace WHERE agent_id = $1 AND path = $2 LIMIT 1`,
		agentID, "skills/"+manifest.Name+"/SKILL.md").Scan(&existing)
	if err == nil {
		return "", 0, fmt.Errorf("skill %q already installed — `cumora skills delete %s` first if you want to reinstall", manifest.Name, manifest.Name)
	}
	var tenant sql.NullString
	_ = s.DB.QueryRowContext(ctx, `SELECT company_id FROM participants WHERE id = $1 LIMIT 1`, agentID).Scan(&tenant)
	written := 0
	for _, f := range manifest.Files {
		path := "skills/" + manifest.Name + "/" + f.Path
		_, _ = s.DB.ExecContext(ctx, `
			INSERT INTO agent_workspace (agent_id, path, body, company_id, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (agent_id, path) DO UPDATE SET body = EXCLUDED.body, company_id = EXCLUDED.company_id, updated_at = NOW()`,
			agentID, path, f.Body, tenant)
		written++
	}
	return manifest.Name, written, nil
}

/* ───────────── 命令面 ───────────── */

// cliSkillIndexEntry:--json 键序 name, description, path。
type cliSkillIndexEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
}

func (s *Domain) CmdSkills(ctx context.Context, parsed agent.Parsed) agent.Result {
	me, err := agent.ResolveAs(parsed)
	if err != nil {
		return agent.Err(err.Error())
	}
	op := "list"
	if len(parsed.Positional()) > 0 && parsed.Positional()[0] != "" {
		op = parsed.Positional()[0]
	}

	switch op {
	case "list":
		rows, err := s.DB.QueryContext(ctx, `
			SELECT path, body FROM agent_workspace
			 WHERE agent_id = $1 AND path LIKE 'skills/%/SKILL.md'
			 ORDER BY path ASC`, me)
		if err != nil {
			return agent.ErrCode(fmt.Sprintf("error: %v", err), 2)
		}
		defer rows.Close()
		skills := []cliSkillIndexEntry{}
		for rows.Next() {
			var path, body string
			if rows.Scan(&path, &body) != nil {
				continue
			}
			fm, _ := agent.ParseSkillMd(body)
			if fm == nil {
				continue
			}
			skills = append(skills, cliSkillIndexEntry{Name: fm.Name, Description: fm.Description, Path: path})
		}
		if parsed.FlagTruey("json") {
			txt, jerr := agent.JSONList(skills)
			if jerr != nil {
				return agent.ErrCode(fmt.Sprintf("error: %v", jerr), 2)
			}
			return agent.OK(txt)
		}
		if len(skills) == 0 {
			return agent.OK("(no skills installed — use `cumora skills create <name> \"<description>\"` to scaffold one)")
		}
		lines := make([]string, 0, len(skills))
		for _, sk := range skills {
			lines = append(lines, fmt.Sprintf("  %s\n    %s\n    → cumora skills read %s", sk.Name, sk.Description, sk.Name))
		}
		return agent.OK(strings.Join(lines, "\n\n"))

	case "read":
		name := ""
		if len(parsed.Positional()) > 1 {
			name = parsed.Positional()[1]
		}
		if name == "" {
			return agent.Err("usage: skills read <name> [<sub-path>]")
		}
		subPath := ""
		if len(parsed.Positional()) > 2 {
			subPath = parsed.Positional()[2]
		}
		fullPath := "skills/" + name + "/SKILL.md"
		if subPath != "" {
			fullPath = "skills/" + name + "/" + subPath
		}
		var body string
		err := s.DB.QueryRowContext(ctx,
			`SELECT body FROM agent_workspace WHERE agent_id = $1 AND path = $2 LIMIT 1`,
			me, fullPath).Scan(&body)
		if err != nil {
			return agent.Err(fmt.Sprintf("no such file: %s", fullPath))
		}
		return agent.OK(body)

	case "create":
		name, description := "", ""
		if len(parsed.Positional()) > 1 {
			name = parsed.Positional()[1]
		}
		if len(parsed.Positional()) > 2 {
			description = parsed.Positional()[2]
		}
		if name == "" || description == "" {
			return agent.Err(`usage: skills create <name> "<description>"  (name: lowercase a-z, 0-9, hyphens; description: ≤1024 chars)`)
		}
		if nameErr := cliValidateSkillName(name); nameErr != "" {
			return agent.Err(nameErr)
		}
		if len(description) > 1024 {
			return agent.Err("description must be ≤ 1024 characters")
		}
		path := "skills/" + name + "/SKILL.md"
		var existing string
		if s.DB.QueryRowContext(ctx,
			`SELECT path FROM agent_workspace WHERE agent_id = $1 AND path = $2 LIMIT 1`,
			me, path).Scan(&existing) == nil {
			return agent.Err(fmt.Sprintf("skill %q already exists — use `cumora ws edit %s` to modify it, or `cumora skills delete %s` first", name, path, name))
		}
		body := fmt.Sprintf(`---
name: %s
description: %s
---

# %s

_Write the skill instructions here. Recommended sections: overview,
step-by-step, examples, edge cases. Keep this file under ~500 lines —
move long reference material into `+"`references/`"+` files and load them
on demand via `+"`cumora skills read %s references/<file>`"+`._
`, name, description, name, name)
		companyID, _ := s.AgentCompany(ctx, me)
		var companyArg any
		if companyID != "" {
			companyArg = companyID
		}
		if _, err := s.DB.ExecContext(ctx, `
			INSERT INTO agent_workspace (agent_id, path, body, company_id, updated_at)
			VALUES ($1, $2, $3, $4, NOW())`, me, path, body, companyArg); err != nil {
			return agent.ErrCode(fmt.Sprintf("error: %v", err), 2)
		}
		return agent.OK(fmt.Sprintf(
			"created skill %q at %s\n\nflesh it out: cumora ws edit %s \"<old>\" \"<new>\"\nadd scripts:  cumora ws write skills/%s/scripts/<file>.py \"<body>\"\nread it back: cumora skills read %s",
			name, path, path, name, name), agent.CliSideEffect{
			"event":     "skill.created",
			"command":   "skills create",
			"agentId":   me,
			"skillName": name,
			"path":      path,
		})

	case "delete":
		name := ""
		if len(parsed.Positional()) > 1 {
			name = parsed.Positional()[1]
		}
		if name == "" {
			return agent.Err("usage: skills delete <name>")
		}
		res, err := s.DB.ExecContext(ctx, `
			DELETE FROM agent_workspace
			 WHERE agent_id = $1 AND (path = $2 OR path LIKE $3)`,
			me, "skills/"+name+"/SKILL.md", "skills/"+name+"/%")
		if err != nil {
			return agent.ErrCode(fmt.Sprintf("error: %v", err), 2)
		}
		removed := 0
		if n, err := res.RowsAffected(); err == nil {
			removed = int(n)
		}
		if removed == 0 {
			return agent.Err(fmt.Sprintf("no such skill: %s", name))
		}
		return agent.OK(fmt.Sprintf("deleted skill %q (%d files removed)", name, removed), agent.CliSideEffect{
			"event":     "skill.deleted",
			"command":   "skills delete",
			"agentId":   me,
			"skillName": name,
			"fileCount": removed,
		})

	case "search":
		query := strings.TrimSpace(strings.Join(parsed.Positional()[1:], " "))
		if query == "" {
			return agent.Err("usage: skills search <query>")
		}
		hub := config.SkillHubURL()
		if hub == "" {
			return agent.Err("SkillHub URL not configured — set SKILLHUB_URL on the server")
		}
		hits, err := cliSearchSkillHub(query, hub)
		if err != nil {
			return agent.Err(fmt.Sprintf("skills search failed: %s", errText(err)))
		}
		if parsed.FlagTruey("json") {
			txt, jerr := agent.JSONList(hits)
			if jerr != nil {
				return agent.ErrCode(fmt.Sprintf("error: %v", jerr), 2)
			}
			return agent.OK(txt)
		}
		if len(hits) == 0 {
			return agent.OK(fmt.Sprintf("(no skills found matching %q)", query))
		}
		blocks := make([]string, 0, len(hits))
		for _, h := range hits {
			var meta []string
			if h.Version != nil && *h.Version != "" {
				meta = append(meta, "v"+*h.Version)
			}
			if h.Author != nil && *h.Author != "" {
				meta = append(meta, "by "+*h.Author)
			}
			metaTxt := strings.Join(meta, " · ")
			tag := h.Name
			if h.InstallURL != nil && *h.InstallURL != "" {
				tag = *h.InstallURL
			}
			nameLine := h.Name
			if metaTxt != "" {
				nameLine += "  (" + metaTxt + ")"
			}
			blocks = append(blocks, fmt.Sprintf("  %s\n    %s\n    → cumora skills install %s", nameLine, h.Description, tag))
		}
		return agent.OK(strings.Join(blocks, "\n\n"))

	case "company":
		// #261 公司 Skills 库(SOP 手册)只读索引:内容已由 daemon 物化到
		// 引擎原生 skills 目录(引擎加载器直接可用),这里给人/agent 一个
		// 可发现的清单面。
		companyID, _ := s.AgentCompany(ctx, me)
		if companyID == "" {
			return agent.Err("no company")
		}
		rows, err := s.DB.QueryContext(ctx, `
			SELECT name, description FROM company_skills
			 WHERE company_id = $1 ORDER BY name ASC`, companyID)
		if err != nil {
			return agent.ErrCode(fmt.Sprintf("error: %v", err), 2)
		}
		defer rows.Close()
		type coSkill struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		skills := []coSkill{}
		for rows.Next() {
			var sk coSkill
			if rows.Scan(&sk.Name, &sk.Description) == nil {
				skills = append(skills, sk)
			}
		}
		if parsed.FlagTruey("json") {
			txt, jerr := agent.JSONList(skills)
			if jerr != nil {
				return agent.ErrCode(fmt.Sprintf("error: %v", jerr), 2)
			}
			return agent.OK(txt)
		}
		if len(skills) == 0 {
			return agent.OK("(no company skills — ask a human to add them in the desktop app)")
		}
		lines := make([]string, 0, len(skills))
		for _, sk := range skills {
			lines = append(lines, fmt.Sprintf("  %s\n    %s", sk.Name, sk.Description))
		}
		return agent.OK("Company skills (the team playbook — already materialized in your skills directory):\n" + strings.Join(lines, "\n"))

	case "install":
		idOrURL := ""
		if len(parsed.Positional()) > 1 {
			idOrURL = parsed.Positional()[1]
		}
		if idOrURL == "" {
			return agent.Err("usage: skills install <skill_id_or_install_url>")
		}
		hub := config.SkillHubURL()
		manifest, err := cliFetchSkillManifest(idOrURL, hub)
		if err == nil {
			var name string
			var files int
			name, files, err = s.cliInstallSkillFromManifest(ctx, me, manifest)
			if err == nil {
				plural := "s"
				if files == 1 {
					plural = ""
				}
				return agent.OK(fmt.Sprintf("installed skill %q (%d file%s)\nread it with: cumora skills read %s", name, files, plural, name), agent.CliSideEffect{
					"event":     "skill.installed",
					"command":   "skills install",
					"agentId":   me,
					"skillName": name,
					"fileCount": files,
					"source":    idOrURL,
				})
			}
		}
		return agent.Err(fmt.Sprintf("skills install failed: %s", errText(err)))
	}

	return agent.Err(strings.Join([]string{
		"usage:",
		"  skills list                                 list installed skills (name + description only)",
		"  skills read <name> [<sub-path>]             load full SKILL.md (or a bundled file)",
		"  skills create <name> \"<description>\"        scaffold a new skill",
		"  skills company                              list the company skill library (shared playbook)",
		"  skills search <query>                       search the configured SkillHub",
		"  skills install <id_or_url>                  install a skill from SkillHub (or any compatible URL)",
		"  skills delete <name>                        remove a skill and all its files",
	}, "\n"))
}

// errText:Go error 的 message 面(与 TS e instanceof Error ? e.message 一致)。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
