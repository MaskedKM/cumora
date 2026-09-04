// /runtime/cli 文件区命令组(#89):workspace(团队真实文件夹,resolve 归一
// + realpath 双层防逃逸)/ ws(agent 私有区)(原 cli_private.go 拆出,
// 函数体零改动)。
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/workspaces"
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

// cliRejectReserved:#336 起预留平台内部目录(.cumora/ —— 刀 2 的写前
// 快照与冲突副本落此)。CLI 面读写删移全拒;挂载直写挡不住(引擎全权,
// ADR 0006 信任域边界),这是 CLI 层的最大努力防护。大小写不敏感
// (#339 评审余量:macOS 大小写不敏感盘 .CUMORA 绕纯前缀检查)。
func cliRejectReserved(rel string) string {
	return workspaces.RejectReserved(rel)
}

// cliCASCheck:#337 团队区写命令的可选 --expected <mtimeNanos> —— 失配
// = 别人在你 stat 之后写过:挑战者内容留 .conflict 副本(永不静默丢),
// 返回失败文案让 agent 重读重判(与消息面 HELD 同构)。
func cliCASCheck(folder, rel, expected, principal, challenger string) string {
	v, err := strconv.ParseInt(strings.TrimSpace(expected), 10, 64)
	if err != nil {
		return "--expected must be an integer (unix nanos, from `workspace stat --json`)"
	}
	cur := int64(0)
	if st, serr := os.Stat(filepath.Join(folder, filepath.FromSlash(rel))); serr == nil && !st.IsDir() {
		cur = st.ModTime().UnixNano()
	}
	if cur != v {
		msg := fmt.Sprintf("stale write — current mtime %d ns ≠ expected %d ns; re-read and retry with --expected %d", cur, v, cur)
		if conflict := workspaces.SaveConflictCopy(folder, rel, principal, challenger); conflict != "" {
			msg += "; your content saved to " + conflict
		}
		return msg
	}
	return ""
}

// cliRejectRoot:rel "." = workspace 根自身(filepath.Rel(root,root))。
// delete/mv/stat 不得作用于根(评审 #339 P2;read/write 家族会被既有
// "path is a directory" 检查拦)。
func cliRejectRoot(rel string) string {
	if rel == "." {
		return "cannot operate on the workspace root"
	}
	return ""
}

// cliNormalizeShortI:grep 家族的 -i 短旗归一 —— cliParseArgs 只认 --
// 前缀,单横线会落进 positional 变死旗(help/技能目录宣传的是 -i 形)。
// 只在 grep 分支局部调用,不做全局短旗支持:reply 等命令的正文
// positional 可以合法地以 - 开头(评审 #339 P1)。
func cliNormalizeShortI(p cliParsed) cliParsed {
	out := make([]string, 0, len(p.positional))
	for _, v := range p.positional {
		if v == "-i" {
			p.flags["i"] = true
			continue
		}
		out = append(out, v)
	}
	p.positional = out
	return p
}

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
	usage := "usage: workspace ls | workspace read <id> <path> | workspace write <id> <path> <body> | workspace append <id> <path> <body>\n" +
		"       workspace edit <id> <path> <old> <new> [--all] | workspace delete <id> <path> | workspace mv <id> <src> <dst>\n" +
		"       workspace stat <id> <path> | workspace grep <id> <pattern> [-i] [--json] [--as id]"
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
			return cliErr("usage: workspace write <id> <path> <body> [--expected <nanos>] [--as id]")
		}
		folder, wsName, wsResolvedID, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		// 评审 #341 P0:逃逸/保留路径校验必须先于 CAS 与快照 —— 否则
		// --expected 路径的 SaveConflictCopy 会在校验前把挑战者内容写
		// 到 Join(folder, 原始path) 的越界位置(照抄 append 的次序)。
		_, relW, errMsgW := cliResolveInside(folder, path)
		if errMsgW != "" {
			return cliErr(errMsgW)
		}
		if relW == "" {
			return cliErr("path required")
		}
		if msg := cliRejectRoot(relW); msg != "" {
			return cliErr(msg)
		}
		if msg := cliRejectReserved(relW); msg != "" {
			return cliErr(msg)
		}
		if exp, has := parsed.flagStr("expected"); has {
			if fail := cliCASCheck(folder, path, exp, me, body); fail != "" {
				return cliErr(fail)
			}
		}
		workspaces.SnapshotVersion(folder, path)
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
	case "append":
		if len(parsed.positional) < 4 || parsed.positional[2] == "" {
			return cliErr("usage: workspace append <id> <path> <body> [--expected <nanos>] [--as id]")
		}
		wsID, path := parsed.positional[1], parsed.positional[2]
		body := strings.Join(positionalFrom(parsed, 3), " ")
		if body == "" {
			return cliErr("usage: workspace append <id> <path> <body> [--expected <nanos>] [--as id]")
		}
		folder, wsName, wsResolvedID, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		abs, rel, errMsg := cliResolveInside(folder, path)
		if errMsg != "" {
			return cliErr(errMsg)
		}
		if rel == "" {
			return cliErr("path required")
		}
		if msg := cliRejectReserved(rel); msg != "" {
			return cliErr(msg)
		}
		var existing []byte
		if st, err := os.Stat(abs); err == nil {
			if st.IsDir() {
				return cliErr("path is a directory")
			}
			if st.Size() > cliMaxFileBytes {
				return cliErr("file too large")
			}
			if existing, err = os.ReadFile(abs); err != nil {
				return cliErrThrow(err)
			}
		}
		if len(existing)+len(body) > cliMaxFileBytes {
			return cliErr("file too large")
		}
		if exp, has := parsed.flagStr("expected"); has {
			if fail := cliCASCheck(folder, path, exp, me, string(existing)+body); fail != "" {
				return cliErr(fail)
			}
		}
		workspaces.SnapshotVersion(folder, path)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return cliErrThrow(err)
		}
		if err := os.WriteFile(abs, append(existing, []byte(body)...), 0o644); err != nil {
			return cliErrThrow(err)
		}
		return cliOK(fmt.Sprintf("appended %d chars to %s in %s", len(body), path, wsName), CliSideEffect{
			"event":       "team_workspace.file_written",
			"command":     "workspace append",
			"agentId":     me,
			"companyId":   tenant,
			"workspaceId": wsResolvedID,
			"path":        path,
			"bodyLength":  len(body),
		})
	case "edit":
		if len(parsed.positional) < 4 || parsed.positional[1] == "" || parsed.positional[2] == "" {
			return cliErr("usage: workspace edit <id> <path> <old> <new> [--all] [--as id]")
		}
		wsID, path := parsed.positional[1], parsed.positional[2]
		oldStr := parsed.positional[3]
		newStr := ""
		if len(parsed.positional) > 4 {
			newStr = parsed.positional[4]
		}
		folder, wsName, wsResolvedID, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		body, errMsg := cliReadWorkspaceFile(folder, path)
		if errMsg != "" {
			return cliErr(errMsg)
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
		if exp, has := parsed.flagStr("expected"); has {
			if fail := cliCASCheck(folder, path, exp, me, next); fail != "" {
				return cliErr(fail)
			}
		}
		workspaces.SnapshotVersion(folder, path)
		if errMsg := cliWriteWorkspaceFile(folder, path, next); errMsg != "" {
			return cliErr(errMsg)
		}
		plural := "s"
		if occurrences == 1 {
			plural = ""
		}
		return cliOK(fmt.Sprintf("edited %s in %s (%d replacement%s)", path, wsName, occurrences, plural), CliSideEffect{
			"event":        "team_workspace.file_updated",
			"command":      "workspace edit",
			"agentId":      me,
			"companyId":    tenant,
			"workspaceId":  wsResolvedID,
			"path":         path,
			"replacements": occurrences,
			"bodyLength":   len(next),
		})
	case "delete":
		if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
			return cliErr("usage: workspace delete <id> <path> [--as id]")
		}
		wsID, path := parsed.positional[1], parsed.positional[2]
		folder, _, wsResolvedID, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		abs, rel, errMsg := cliResolveInside(folder, path)
		if errMsg != "" {
			return cliErr(errMsg)
		}
		if rel == "" {
			return cliErr("path required")
		}
		if msg := cliRejectRoot(rel); msg != "" {
			return cliErr(msg)
		}
		if msg := cliRejectReserved(rel); msg != "" {
			return cliErr(msg)
		}
		st, err := os.Lstat(abs)
		if err != nil {
			return cliErr("file not found")
		}
		// 删前留档:误删可从 .cumora/versions/ 恢复(#337)。
		workspaces.SnapshotVersion(folder, path)
		if st.IsDir() {
			// 空目录才删(os.Remove 语义):非空目录保守拒绝,清空后再删。
			if err := os.Remove(abs); err != nil {
				return cliErr("directory not empty — remove its files first")
			}
		} else if err := os.Remove(abs); err != nil {
			return cliErrThrow(err)
		}
		return cliOK("deleted "+rel, CliSideEffect{
			"event":       "team_workspace.file_deleted",
			"command":     "workspace delete",
			"agentId":     me,
			"companyId":   tenant,
			"workspaceId": wsResolvedID,
			"path":        rel,
		})
	case "mv":
		if len(parsed.positional) < 4 || parsed.positional[1] == "" || parsed.positional[2] == "" || parsed.positional[3] == "" {
			return cliErr("usage: workspace mv <id> <src> <dst> [--as id]")
		}
		wsID, src, dst := parsed.positional[1], parsed.positional[2], parsed.positional[3]
		folder, wsName, wsResolvedID, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		absSrc, relSrc, errMsg := cliResolveInside(folder, src)
		if errMsg != "" {
			return cliErr(errMsg)
		}
		absDst, relDst, errMsg := cliResolveInside(folder, dst)
		if errMsg != "" {
			return cliErr(errMsg)
		}
		if relSrc == "" || relDst == "" {
			return cliErr("path required")
		}
		if msg := cliRejectRoot(relSrc); msg != "" {
			return cliErr(msg)
		}
		if msg := cliRejectRoot(relDst); msg != "" {
			return cliErr(msg)
		}
		if msg := cliRejectReserved(relSrc); msg != "" {
			return cliErr(msg)
		}
		if msg := cliRejectReserved(relDst); msg != "" {
			return cliErr(msg)
		}
		if _, err := os.Lstat(absSrc); err != nil {
			return cliErr("file not found")
		}
		if st, err := os.Lstat(absDst); err == nil && st.IsDir() {
			// 对齐 mv 直觉:目标是目录 → 移入其中(保留原名)。
			absDst = filepath.Join(absDst, filepath.Base(absSrc))
			if msg := cliAssertInside(folder, absDst); msg != "" {
				return cliErr(msg)
			}
			relDst, _ = filepath.Rel(folder, absDst)
		}
		if err := os.MkdirAll(filepath.Dir(absDst), 0o755); err != nil {
			return cliErrThrow(err)
		}
		// 移前留档:源位置的旧内容可恢复(#337)。
		workspaces.SnapshotVersion(folder, relSrc)
		if err := os.Rename(absSrc, absDst); err != nil {
			return cliErrThrow(err)
		}
		return cliOK(fmt.Sprintf("moved %s → %s in %s", relSrc, relDst, wsName), CliSideEffect{
			"event":       "team_workspace.file_moved",
			"command":     "workspace mv",
			"agentId":     me,
			"companyId":   tenant,
			"workspaceId": wsResolvedID,
			"from":        relSrc,
			"to":          relDst,
		})
	case "stat":
		if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
			return cliErr("usage: workspace stat <id> <path> [--json] [--as id]")
		}
		wsID, path := parsed.positional[1], parsed.positional[2]
		folder, _, _, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		abs, rel, errMsg := cliResolveInside(folder, path)
		if errMsg != "" {
			return cliErr(errMsg)
		}
		if rel == "" {
			return cliErr("path required")
		}
		if msg := cliRejectRoot(rel); msg != "" {
			return cliErr(msg)
		}
		if msg := cliRejectReserved(rel); msg != "" {
			return cliErr(msg)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return cliErr("file not found")
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONStringify(map[string]any{
				"path": rel, "size": st.Size(), "isDir": st.IsDir(),
				"modifiedAt": cliISOTime(st.ModTime()),
				// 纳秒 int64 超 JS 安全整数:字符串化,LLM 抄写不丢精度。
				"mtimeNanos": strconv.FormatInt(st.ModTime().UnixNano(), 10),
			})
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		kindF := "file"
		if st.IsDir() {
			kindF = "dir"
		}
		return cliOK(fmt.Sprintf("%s — %s · %d bytes · modified %s", rel, kindF, st.Size(), st.ModTime().UTC().Format(time.RFC3339)))
	case "grep":
		if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
			return cliErr("usage: workspace grep <id> <pattern> [-i] [--json] [--as id]")
		}
		parsed = cliNormalizeShortI(parsed)
		wsID, pattern := parsed.positional[1], parsed.positional[2]
		folder, wsName, _, msg := s.cliWorkspaceResolve(ctx, tenant, me, wsID)
		if msg != "" {
			return cliErr(msg)
		}
		re, reErr := regexp.Compile(pattern)
		if reErr == nil && parsed.flagTruey("i") {
			re, reErr = regexp.Compile("(?i)" + pattern)
		}
		if reErr != nil {
			return cliErr("bad regex: " + pattern)
		}
		hits, truncated := cliGrepWorkspaceFolder(folder, re)
		if parsed.flagTruey("json") {
			js, e := cliJSONStringify(hits)
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		if len(hits) == 0 {
			return cliOK(fmt.Sprintf("(no matches for /%s/ in %s)", pattern, wsName))
		}
		lines := append([]string{fmt.Sprintf("%d match(es) in %s:", len(hits), wsName), ""}, hits...)
		if truncated {
			lines = append(lines, "", "  (output truncated)")
		}
		return cliOK(strings.Join(lines, "\n"))
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
	if msg := cliRejectReserved(rel); msg != "" {
		return "", msg
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
	if msg := cliRejectReserved(rel); msg != "" {
		return msg
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

// cliGrepWorkspaceFolder:真实文件夹递归文本检索(#336)。跳过 .cumora/
// (平台内部)与 .git/(对象库);单文件 >2MB 跳过(对齐读面上限);
// 命中 200 条封顶(大 repo 防打爆,CLI 输出不是 grep 的主场——挂点
// 直达后 agent 有 native grep)。
const cliGrepMaxHits = 200

func cliGrepWorkspaceFolder(root string, re *regexp.Regexp) (hits []string, truncated bool) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单点读失败(权限/并发删除)跳过,不中断整轮
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (name == ".cumora" || name == ".git") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ie := d.Info()
		if ie != nil || info.Size() > cliMaxFileBytes {
			return nil
		}
		body, reErr := os.ReadFile(p)
		if reErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		for i, line := range strings.Split(string(body), "\n") {
			if re.MatchString(line) {
				if len(hits) >= cliGrepMaxHits {
					truncated = true
					return fs.SkipAll // 触帽即收束,不再白读剩余整棵树
				}
				hits = append(hits, "  "+rel+":"+strconv.Itoa(i+1)+": "+utf16Slice(line, 200))
			}
		}
		return nil
	})
	return hits, truncated
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
			return cliErr("usage: ws grep <pattern> [-i] [--as id]")
		}
		parsed = cliNormalizeShortI(parsed)
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
