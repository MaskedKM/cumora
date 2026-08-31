// /runtime/cli 文件区命令组(#89):workspace(团队真实文件夹,resolve 归一
// + realpath 双层防逃逸)/ ws(agent 私有区)(原 cli_private.go 拆出,
// 函数体零改动)。
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

/* ───────── workspace(团队真实文件夹)───────── */

// cliWorkspaceResolve:core.ts resolveWorkspaceAccess 的 CLI 面 —— 默认区
// 全员;显式成员/关联;错误文案与 TS WorkspaceError 逐字对齐。
func (s *Service) cliWorkspaceResolve(ctx context.Context, tenant, me, wsID string) (folderPath, name, id string, errMsg string) {
	var fp, n string
	var unboundAt sql.NullTime
	var isDefault bool
	err := s.DB.QueryRowContext(ctx,
		`SELECT folder_path, name, is_default, unbound_at FROM workspaces
		  WHERE company_id = $1 AND id = $2`, tenant, wsID,
	).Scan(&fp, &n, &isDefault, &unboundAt)
	if err == sql.ErrNoRows {
		return "", "", "", "workspace not found"
	}
	if err != nil {
		return "", "", "", "workspace lookup failed"
	}
	if unboundAt.Valid {
		return "", "", "", "workspace is unbound"
	}
	if isDefault {
		return fp, n, wsID, ""
	}
	var allowed bool
	err = s.DB.QueryRowContext(ctx, `
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
		 LIMIT 1`, wsID, me, tenant).Scan(&allowed)
	if err != nil && err != sql.ErrNoRows {
		return "", "", "", "membership query failed"
	}
	if err == sql.ErrNoRows || !allowed {
		return "", "", "", "not a member of this workspace"
	}
	return fp, n, wsID, ""
}

const cliMaxFileBytes = 2 * 1024 * 1024

// cliResolveInside:双层防逃逸(resolve 归一 + realpath 复检),错误文案
// 对齐 core.ts assertInside/resolveInside。
func cliResolveInside(root, raw string) (abs, rel string, errMsg string) {
	r := strings.TrimSpace(raw)
	if strings.ContainsRune(r, 0) {
		return "", "", "invalid path"
	}
	absPath := filepath.Join(root, r)
	if filepath.IsAbs(r) {
		// node resolve(root, 绝对rel) 以绝对 rel 为准 → 必然逃逸
		absPath = filepath.Clean(r)
	}
	if msg := cliAssertInside(root, absPath); msg != "" {
		return "", "", msg
	}
	real := absPath
	if rp, err := filepath.EvalSymlinks(absPath); err == nil {
		real = rp
	} else if pd, err := filepath.EvalSymlinks(filepath.Dir(absPath)); err == nil {
		real = filepath.Join(pd, filepath.Base(absPath))
	}
	if msg := cliAssertInside(root, real); msg != "" {
		return "", "", msg
	}
	relP, _ := filepath.Rel(root, real)
	return real, relP, ""
}

func cliAssertInside(root, abs string) string {
	fromRoot, err := filepath.Rel(root, abs)
	if err != nil {
		return "path escapes the workspace folder"
	}
	if fromRoot != "" && (fromRoot == ".." || strings.HasPrefix(fromRoot, ".."+string(filepath.Separator)) || filepath.IsAbs(fromRoot)) {
		return "path escapes the workspace folder"
	}
	return ""
}

// cliEnsureDefaultWorkspace:默认区惰性自愈(<uploads 根>/workspaces/<cid>,
// 根经 config.UploadsDir() 统一解析)。
func (s *Service) cliEnsureDefaultWorkspace(ctx context.Context, tenant string) error {
	var one int
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM workspaces WHERE company_id = $1 AND is_default LIMIT 1`, tenant).Scan(&one)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	folder := filepath.Join(uploadDir(), "workspaces", tenant)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return err
	}
	folderReal, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO workspaces (id, company_id, name, folder_path, is_default)
		 VALUES ($1, $2, $3, $4, TRUE) ON CONFLICT DO NOTHING`,
		"ws-default-"+tenant, tenant, "Team files", folderReal)
	return err
}

func (s *Service) cliCmdTeamWorkspace(ctx context.Context, parsed cliParsed) cliResult {
	op := ""
	if len(parsed.positional) > 0 {
		op = parsed.positional[0]
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	tenant, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErrThrow(err)
	}
	if tenant == "" {
		return cliErr("no company for " + me + " — team workspaces need a team")
	}
	usage := "usage: workspace ls | workspace read <id> <path> | workspace write <id> <path> <body> [--as id]"
	switch op {
	case "ls":
		if err := s.cliEnsureDefaultWorkspace(ctx, tenant); err != nil {
			return cliErrThrow(err)
		}
		rows, err := s.DB.QueryContext(ctx,
			`SELECT id, name, is_default, created_at FROM workspaces
			  WHERE company_id = $1 AND unbound_at IS NULL ORDER BY created_at ASC`, tenant)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type row struct {
			ID        string     `json:"id"`
			Name      string     `json:"name"`
			IsDefault bool       `json:"is_default"`
			CreatedAt cliISOTime `json:"created_at"`
		}
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.ID, &r.Name, &r.IsDefault, &r.CreatedAt); err != nil {
				return cliErrThrow(err)
			}
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONList(all)
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		lines := []string{fmt.Sprintf("%d team workspace(s):", len(all)), ""}
		for _, r := range all {
			def := ""
			if r.IsDefault {
				def = "[default] "
			}
			lines = append(lines, "  "+utf16PadEnd(r.ID, 50)+" "+def+r.Name)
		}
		return cliOK(strings.Join(lines, "\n"))
	case "read":
		if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
			return cliErr("usage: workspace read <id> <path> [--as id]")
		}
		wsID, path := parsed.positional[1], parsed.positional[2]
		folder, _, _, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		body, errMsg := cliReadWorkspaceFile(folder, path)
		if errMsg != "" {
			return cliErr(errMsg)
		}
		return cliOK(body)
	case "write":
		if len(parsed.positional) < 4 || parsed.positional[2] == "" {
			return cliErr("usage: workspace write <id> <path> <body> [--as id]")
		}
		wsID, path := parsed.positional[1], parsed.positional[2]
		body := strings.Join(positionalFrom(parsed, 3), " ")
		if body == "" {
			return cliErr("usage: workspace write <id> <path> <body> [--as id]")
		}
		folder, wsName, wsResolvedID, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		if errMsg := cliWriteWorkspaceFile(folder, path, body); errMsg != "" {
			return cliErr(errMsg)
		}
		return cliOK(fmt.Sprintf("wrote %s in %s (%d chars)", path, wsName, len(body)), CliSideEffect{
			"event":       "team_workspace.file_written",
			"command":     "workspace write",
			"agentId":     me,
			"companyId":   tenant,
			"workspaceId": wsResolvedID,
			"path":        path,
			"bodyLength":  len(body),
		})
	}
	return cliErr(usage)
}

func cliReadWorkspaceFile(root, rawPath string) (string, string) {
	abs, rel, msg := cliResolveInside(root, rawPath)
	if msg != "" {
		return "", msg
	}
	if rel == "" {
		return "", "path required"
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", "file not found"
	}
	if st.IsDir() {
		return "", "path is a directory"
	}
	if st.Size() > cliMaxFileBytes {
		return "", "file too large"
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		return "", "file not found"
	}
	return string(body), ""
}

func cliWriteWorkspaceFile(root, rawPath, content string) string {
	abs, rel, msg := cliResolveInside(root, rawPath)
	if msg != "" {
		return msg
	}
	if rel == "" {
		return "path required"
	}
	if len(content) > cliMaxFileBytes {
		return "file too large"
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return "path is a directory"
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err.Error()
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return err.Error()
	}
	return ""
}

/* ───────── ws(私有区)───────── */

func (s *Service) cliCmdWorkspace(ctx context.Context, parsed cliParsed) cliResult {
	op := ""
	if len(parsed.positional) > 0 {
		op = parsed.positional[0]
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	// 写入要带 company_id(Observability 按租户过滤);读取按 agent 全局
	// 唯一 id 即可。
	tenant, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErrThrow(err)
	}
	switch op {
	case "ls":
		rows, err := s.DB.QueryContext(ctx,
			`SELECT path, updated_at FROM agent_workspace WHERE agent_id = $1 ORDER BY path ASC`, me)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type row struct {
			Path      string     `json:"path"`
			UpdatedAt cliISOTime `json:"updated_at"`
		}
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.Path, &r.UpdatedAt); err != nil {
				return cliErrThrow(err)
			}
			all = append(all, r)
		}
		if err := rows.Err(); err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONList(all)
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		if len(all) == 0 {
			return cliOK(fmt.Sprintf("(%s's Private Area is empty)", me))
		}
		lines := []string{fmt.Sprintf("%d file(s) in %s's Private Area:", len(all), me), ""}
		for _, r := range all {
			lines = append(lines, "  "+utf16PadEnd(r.Path, 40)+" "+nodeLocaleString(time.Time(r.UpdatedAt)))
		}
		return cliOK(strings.Join(lines, "\n"))
	case "read":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: ws read <path> [--as id]")
		}
		path := parsed.positional[1]
		var body string
		err := s.DB.QueryRowContext(ctx,
			`SELECT body FROM agent_workspace WHERE agent_id = $1 AND path = $2`, me, path).Scan(&body)
		if err == sql.ErrNoRows {
			return cliErr("no file at " + path + " in " + me + "'s Private Area")
		}
		if err != nil {
			return cliErrThrow(err)
		}
		return cliOK(body)
	case "write":
		if len(parsed.positional) < 3 || parsed.positional[1] == "" {
			return cliErr("usage: ws write <path> <body> [--as id]")
		}
		path := parsed.positional[1]
		body := strings.Join(positionalFrom(parsed, 2), " ")
		if body == "" {
			return cliErr("usage: ws write <path> <body> [--as id]")
		}
		var metaArg any
		if strings.HasPrefix(path, "memory/") {
			src := s.cliResolveMemoryWriteSource(ctx, me, "")
			metaArg = buildMemoryMeta(path, "", nil, false, src)
		}
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO agent_workspace (agent_id, path, body, meta, company_id, updated_at)
			   VALUES ($1, $2, $3, $4::jsonb, $5, NOW())
			 ON CONFLICT (agent_id, path) DO UPDATE
			   SET body = EXCLUDED.body,
			       company_id = EXCLUDED.company_id,
			       meta = COALESCE(agent_workspace.meta, EXCLUDED.meta),
			       updated_at = NOW()`,
			me, path, body, metaArg, tenant); err != nil {
			return cliErrThrow(err)
		}
		effect := CliSideEffect{
			"event":      "workspace.file_written",
			"command":    "workspace write",
			"agentId":    me,
			"path":       path,
			"bodyLength": len(body),
		}
		if tenant != "" {
			effect["companyId"] = tenant
		}
		return cliOK(fmt.Sprintf("wrote %s (%d chars)", path, len(body)), effect)
	case "delete":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: ws delete <path> [--as id]")
		}
		path := parsed.positional[1]
		res, err := s.DB.ExecContext(ctx,
			`DELETE FROM agent_workspace WHERE agent_id = $1 AND path = $2`, me, path)
		if err != nil {
			return cliErrThrow(err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return cliErr("no file at " + path)
		}
		effect := CliSideEffect{
			"event":   "workspace.file_deleted",
			"command": "workspace delete",
			"agentId": me,
			"path":    path,
		}
		if tenant != "" {
			effect["companyId"] = tenant
		}
		return cliOK("deleted "+path, effect)
	case "edit":
		if len(parsed.positional) < 3 || parsed.positional[1] == "" {
			return cliErr("usage: ws edit <path> <old> <new> [--all] [--as id]")
		}
		path := parsed.positional[1]
		oldStr := parsed.positional[2]
		newStr := ""
		if len(parsed.positional) > 3 {
			newStr = parsed.positional[3]
		}
		var body string
		err := s.DB.QueryRowContext(ctx,
			`SELECT body FROM agent_workspace WHERE agent_id = $1 AND path = $2`, me, path).Scan(&body)
		if err == sql.ErrNoRows {
			return cliErr("no file at " + path)
		}
		if err != nil {
			return cliErrThrow(err)
		}
		occurrences := strings.Count(body, oldStr)
		if occurrences == 0 {
			return cliErr("old string not found in " + path)
		}
		if occurrences > 1 && !parsed.flagTruey("all") {
			return cliErr(fmt.Sprintf("old string appears %d times in %s — pass --all or include more context to make it unique", occurrences, path))
		}
		next := strings.Replace(body, oldStr, newStr, 1)
		if parsed.flagTruey("all") {
			next = strings.ReplaceAll(body, oldStr, newStr)
		}
		if _, err := s.DB.ExecContext(ctx,
			`UPDATE agent_workspace SET body = $3, updated_at = NOW() WHERE agent_id = $1 AND path = $2`,
			me, path, next); err != nil {
			return cliErrThrow(err)
		}
		plural := "s"
		if occurrences == 1 {
			plural = ""
		}
		effect := CliSideEffect{
			"event":        "workspace.file_updated",
			"command":      "workspace edit",
			"agentId":      me,
			"path":         path,
			"replacements": occurrences,
			"bodyLength":   len(next),
		}
		if tenant != "" {
			effect["companyId"] = tenant
		}
		return cliOK(fmt.Sprintf("edited %s (%d replacement%s)", path, occurrences, plural), effect)
	case "grep":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: ws grep <pattern> [--as id]")
		}
		pattern := parsed.positional[1]
		flags := ""
		if parsed.flagTruey("i") {
			flags = "i"
		}
		re, reErr := regexp.Compile(pattern)
		if reErr == nil && flags == "i" {
			re, reErr = regexp.Compile("(?i)" + pattern)
		}
		if reErr != nil {
			return cliErr("bad regex: " + pattern)
		}
		rows, err := s.DB.QueryContext(ctx,
			`SELECT path, body FROM agent_workspace WHERE agent_id = $1 ORDER BY path ASC`, me)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		var hits []string
		for rows.Next() {
			var path, body string
			if err := rows.Scan(&path, &body); err != nil {
				return cliErrThrow(err)
			}
			for i, line := range strings.Split(body, "\n") {
				if re.MatchString(line) {
					hits = append(hits, "  "+path+":"+strconv.Itoa(i+1)+": "+utf16Slice(line, 200))
				}
			}
		}
		if err := rows.Err(); err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONStringify(hits)
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		if len(hits) == 0 {
			return cliOK(fmt.Sprintf("(no matches for /%s/ in %s's Private Area)", pattern, me))
		}
		lines := append([]string{fmt.Sprintf("%d match(es):", len(hits)), ""}, hits...)
		return cliOK(strings.Join(lines, "\n"))
	}
	return cliErr("usage: ws <ls|read|write|edit|grep|delete> [...]")
}
