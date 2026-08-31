// /runtime/cli agent 私有记录命令组(#89):memory(list/note/pin/delete,
// 含 memory-scope 写入溯源)/ log(memory note 双写 agent_log)/ tasks
// (原 cli_private.go 拆出,函数体零改动)。
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

/* ───────── memory ───────── */

var memoryKinds = []string{"observation", "preference", "fact", "decision", "note"}

func normalizeMemoryKind(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	for _, k := range memoryKinds {
		if k == s {
			return s
		}
	}
	return "observation"
}

type memorySource struct {
	ConversationID *string `json:"conversationId"`
	ProjectID      *string `json:"projectId"`
}

func asMemorySource(raw any) *memorySource {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	src := &memorySource{}
	if v, ok := m["conversationId"].(string); ok {
		src.ConversationID = &v
	}
	if v, ok := m["projectId"].(string); ok {
		src.ProjectID = &v
	}
	return src
}

// cliGetThinkingConversations:agent-thinking-convos 反查(fail-open 空集)。
func (s *Service) cliGetThinkingConversations(agentID string) []string {
	rdb := s.redis()
	if rdb == nil {
		return nil
	}
	raw, err := rdb.Get(context.Background(), agentThinkingConvosKey(agentID)).Result()
	if err != nil || raw == "" {
		return nil
	}
	var ids []string
	if jsonUnmarshal([]byte(raw), &ids) != nil {
		return nil
	}
	return ids
}

type thinkingCtx struct {
	conversationID string
	projectID      *string
}

// pickWriteProvenance:memory-scope.ts 同名决策 —— 路径项目目录 > 显式旗
// 标 > 单一明确的 thinking 上下文;混合项目宁可 GLOBAL 不猜。
func pickWriteProvenance(explicitConvoRaw, explicitProjectRaw, path string, thinking []thinkingCtx) memorySource {
	emptyToPtr := func(s string) *string {
		v := strings.TrimSpace(s)
		if v == "" {
			return nil
		}
		return &v
	}
	var fromPath *string
	if path != "" {
		if p := ParseMemoryPath(path); p != nil {
			fromPath = p.ProjectID
		}
	}
	explicitConvo := emptyToPtr(explicitConvoRaw)
	explicitProject := emptyToPtr(explicitProjectRaw)

	conversationID := explicitConvo
	projectID := fromPath
	if projectID == nil {
		projectID = explicitProject
	}
	if conversationID == nil && len(thinking) == 1 {
		c := thinking[0].conversationID
		conversationID = &c
	}
	if projectID == nil {
		if explicitConvo != nil {
			for _, t := range thinking {
				if t.conversationID == *explicitConvo {
					projectID = t.projectID
					break
				}
			}
		} else {
			var pids []string
			seen := map[string]bool{}
			for _, t := range thinking {
				if t.projectID != nil && !seen[*t.projectID] {
					seen[*t.projectID] = true
					pids = append(pids, *t.projectID)
				}
			}
			if len(pids) == 1 {
				p := pids[0]
				projectID = &p
			}
		}
	}
	if conversationID == nil && projectID != nil && len(thinking) > 0 {
		var matches []string
		for _, t := range thinking {
			if t.projectID != nil && *t.projectID == *projectID {
				matches = append(matches, t.conversationID)
			}
		}
		if len(matches) == 1 {
			c := matches[0]
			conversationID = &c
		}
	}
	return memorySource{ConversationID: conversationID, ProjectID: projectID}
}

// cliResolveMemoryWriteSource:memory-write.ts —— 显式会话 > thinking 反查,
// 再把会话映射到项目;DB 打嗝 fail-open 成 GLOBAL。
func (s *Service) cliResolveMemoryWriteSource(ctx context.Context, agentID, conversationID string) memorySource {
	explicit := strings.TrimSpace(conversationID)
	ids := []string{}
	if explicit != "" {
		ids = []string{explicit}
	} else {
		ids = s.cliGetThinkingConversations(agentID)
	}
	var thinking []thinkingCtx
	if len(ids) > 0 {
		rows, err := s.DB.QueryContext(ctx,
			`SELECT id, project_id FROM conversations WHERE id = ANY($1::text[])`, ids)
		if err == nil {
			defer rows.Close()
			byID := map[string]*string{}
			for rows.Next() {
				var id string
				var pid *string
				if rows.Scan(&id, &pid) == nil {
					byID[id] = pid
				}
			}
			for _, id := range ids {
				thinking = append(thinking, thinkingCtx{id, byID[id]})
			}
		} else {
			for _, id := range ids {
				thinking = append(thinking, thinkingCtx{id, nil})
			}
		}
	}
	return pickWriteProvenance(explicit, "", "", thinking)
}

// buildMemoryMeta:新写入 meta(键序手拼对齐 TS 字面量序)。
func buildMemoryMeta(path, kind string, about *string, pinned bool, src memorySource) string {
	parsed := ParseMemoryPath(path)
	k := kind
	if k == "" {
		if parsed != nil && parsed.Kind != "" {
			k = parsed.Kind
		} else if segs := strings.Split(path, "/"); len(segs) > 1 {
			k = segs[1]
		} else {
			k = "note"
		}
	}
	srcJSON := compactJSON(src)
	aboutJSON := "null"
	if about != nil {
		aboutJSON = string(cliJSONString(*about))
	}
	pinStr := "false"
	if pinned {
		pinStr = "true"
	}
	return `{"type":"memory","kind":` + string(cliJSONString(k)) +
		`,"about":` + aboutJSON +
		`,"pinned":` + pinStr +
		`,"source":` + srcJSON +
		`,"createdAt":"` + isoNowMs() + `"}`
}

func memoryWritePath(kind, id string, projectID *string) string {
	k := kind
	if k == "" {
		k = "note"
	}
	file := id + ".md"
	if projectID != nil && *projectID != "" {
		return "memory/projects/" + *projectID + "/" + k + "/" + file
	}
	return "memory/" + k + "/" + file
}

func (s *Service) cliCmdMemory(ctx context.Context, parsed cliParsed) cliResult {
	op := ""
	if len(parsed.positional) > 0 {
		op = parsed.positional[0]
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	explicitConvo := parsed.flagStrOr("in", "")
	if explicitConvo == "" {
		explicitConvo = parsed.flagStrOr("conversation", "")
	}
	switch op {
	case "list":
		return s.cliMemoryList(ctx, parsed, me, explicitConvo)
	case "note":
		return s.cliMemoryNote(ctx, parsed, me, explicitConvo)
	case "pin":
		return s.cliMemoryPin(ctx, parsed, me)
	case "delete":
		return s.cliMemoryDelete(ctx, parsed, me)
	}
	return cliErr("usage: memory <list|note|pin|delete> [...]")
}

type cliMemRow struct {
	path      string
	meta      map[string]any
	ID        string        `json:"id"`
	Kind      string        `json:"kind"`
	About     *string       `json:"about"`
	Body      string        `json:"body"`
	Pinned    bool          `json:"pinned"`
	CreatedAt cliISOTime    `json:"created_at"`
	ProjectID *string       `json:"projectId"`
	Source    *memorySource `json:"source"`
}

func (s *Service) cliMemoryList(ctx context.Context, parsed cliParsed, me, explicitConvo string) cliResult {
	args := []any{me}
	where := `agent_id = $1 AND path LIKE 'memory/%'`
	if v, ok := parsed.flags["about"]; ok {
		args = append(args, fmt.Sprint(v))
		where += fmt.Sprintf(` AND meta->>'about' = $%d`, len(args))
	}
	if v, ok := parsed.flags["kind"]; ok {
		k := normalizeMemoryKind(fmt.Sprint(v))
		args = append(args, "memory/"+k+"/%", "memory/projects/%/"+k+"/%")
		where += fmt.Sprintf(` AND (path LIKE $%d OR path LIKE $%d)`, len(args)-1, len(args))
	}
	limit, err := cliMsgFlagNum(parsed, "limit", 20, 100)
	if err != nil {
		return cliErrThrow(err)
	}
	// 多取一些,内存项目过滤后仍能填满 limit;--all 是唯一跳过隔离的路径。
	fetchLimit := limit
	if !parsed.flagTruey("all") {
		fetchLimit = limit * 10
		if fetchLimit < 100 {
			fetchLimit = 100
		}
		if fetchLimit > 500 {
			fetchLimit = 500
		}
	}
	args = append(args, fetchLimit)
	rows, err := s.DB.QueryContext(ctx,
		`SELECT path, body, meta, updated_at
		   FROM agent_workspace WHERE `+where+`
		   ORDER BY COALESCE((meta->>'pinned')::boolean, false) DESC, updated_at DESC
		   LIMIT `+fmt.Sprintf("$%d", len(args)), args...)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	var parsedRows []cliMemRow
	for rows.Next() {
		var r cliMemRow
		var updatedAt time.Time
		if err := rows.Scan(&r.path, &r.Body, jsonMapScan{&r.meta}, &updatedAt); err != nil {
			return cliErrThrow(err)
		}
		pp := ParseMemoryPath(r.path)
		r.Kind, r.ID = "note", ""
		if pp != nil {
			r.Kind, r.ID = pp.Kind, pp.ID
		}
		if a, ok := r.meta["about"].(string); ok {
			r.About = &a
		}
		r.Pinned, _ = r.meta["pinned"].(bool)
		r.Source = asMemorySource(r.meta["source"])
		if pp != nil && pp.ProjectID != nil {
			r.ProjectID = pp.ProjectID
		} else if r.Source != nil {
			r.ProjectID = r.Source.ProjectID
		}
		r.CreatedAt = cliISOTime(updatedAt)
		parsedRows = append(parsedRows, r)
	}
	if err := rows.Err(); err != nil {
		return cliErrThrow(err)
	}
	scoped := parsedRows
	if !parsed.flagTruey("all") {
		src := s.cliResolveMemoryWriteSource(ctx, me, explicitConvo)
		var projectIDs []string
		if src.ProjectID != nil {
			projectIDs = []string{*src.ProjectID}
		}
		var filtered []cliMemRow
		for _, r := range parsedRows {
			// 谓词同 memory-scope:置顶恒可见;无项目=全局恒可见;有项目
			// 只在该项目的唤醒里可见。输入用的是 (pinned, source) + 真 path。
			scopeMeta := map[string]any{}
			if r.Pinned {
				scopeMeta["pinned"] = true
			}
			if r.Source != nil {
				scopeMeta["source"] = map[string]any{
					"conversationId": nilIfEmpty(derefStr(r.Source.ConversationID)),
					"projectId":      nilIfEmpty(derefStr(r.Source.ProjectID)),
				}
			}
			if MemoryVisibleInScope(scopeMeta, r.path, projectIDs) {
				filtered = append(filtered, r)
			}
		}
		if len(filtered) > limit {
			filtered = filtered[:limit]
		}
		scoped = filtered
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(scoped)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	if len(scoped) == 0 {
		return cliOK(fmt.Sprintf("(%s has no memory yet)", me))
	}
	lines := []string{fmt.Sprintf("%d memory record(s) for %s:", len(scoped), me), ""}
	for _, m := range scoped {
		t := nodeLocaleDate(time.Time(m.CreatedAt))
		pin := "  "
		if m.Pinned {
			pin = "★ "
		}
		about := "-"
		if m.About != nil {
			about = *m.About
		}
		proj := " global"
		if m.ProjectID != nil {
			proj = " proj:" + *m.ProjectID
		}
		lines = append(lines, "  "+pin+"["+utf16Slice(m.ID, 10)+"] "+utf16PadEnd(m.Kind, 11)+" "+
			utf16PadEnd(about, 10)+" "+t+proj+"\n      "+
			strings.ReplaceAll(utf16Slice(m.Body, 280), "\n", " \\n "))
	}
	return cliOK(strings.Join(lines, "\n"))
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (s *Service) cliMemoryNote(ctx context.Context, parsed cliParsed, me, explicitConvo string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: memory note <body> [--about subject] [--kind kind] [--in convo] [--as id]")
	}
	body := parsed.positional[1]
	kind := "observation"
	if v, ok := parsed.flags["kind"]; ok {
		kind = normalizeMemoryKind(fmt.Sprint(v))
	}
	var about *string
	if v, ok := parsed.flagStr("about"); ok {
		about = &v
	}
	id := "mem-" + uuidHex()[:12]
	src := s.cliResolveMemoryWriteSource(context.Background(), me, explicitConvo)
	path := memoryWritePath(kind, id, src.ProjectID)
	tenant, err := s.cliAgentCompany(context.Background(), me)
	if err != nil {
		return cliErrThrow(err)
	}
	meta := buildMemoryMeta(path, kind, about, false, src)
	// 嵌入先算好再 INSERT,body+vector 一次落行;失败仍写行(记忆不丢,
	// 后台回填补向量)。
	embedding := s.EmbedText(context.Background(), body)
	var execErr error
	if embedding != nil {
		_, execErr = s.DB.ExecContext(context.Background(),
			`INSERT INTO agent_workspace (agent_id, path, body, meta, embedding, company_id, updated_at)
			 VALUES ($1, $2, $3, $4::jsonb, $5::vector, $6, NOW())`,
			me, path, body, meta, *embedding, tenant)
	} else {
		_, execErr = s.DB.ExecContext(context.Background(),
			`INSERT INTO agent_workspace (agent_id, path, body, meta, company_id, updated_at)
			 VALUES ($1, $2, $3, $4::jsonb, $5, NOW())`,
			me, path, body, meta, tenant)
	}
	if execErr != nil {
		return cliErrThrow(execErr)
	}
	if _, err := s.DB.ExecContext(context.Background(),
		`INSERT INTO agent_log (id, agent_id, kind, body, ref) VALUES ($1, $2, 'note', $3, $4::jsonb)`,
		"log-"+uuidHex()[:12], me, "noted: "+utf16Slice(body, 120),
		`{"memoryId":"`+id+`","path":"`+path+`"}`); err != nil {
		return cliErrThrow(err)
	}
	return cliOK("saved memory "+id, CliSideEffect{
		"event":    "memory.written",
		"command":  "memory note",
		"memoryId": id,
		"path":     path,
		"agentId":  me,
		"kind":     kind,
		"about":    about,
	})
}

func (s *Service) cliMemoryPin(ctx context.Context, parsed cliParsed, me string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: memory pin <id>")
	}
	id := parsed.positional[1]
	var meta map[string]any
	err := s.DB.QueryRowContext(ctx,
		`UPDATE agent_workspace
		    SET meta = COALESCE(meta, '{}'::jsonb)
		             || jsonb_build_object('pinned', NOT COALESCE((meta->>'pinned')::boolean, false))
		  WHERE agent_id = $1 AND path LIKE $2
		  RETURNING meta`, me, "memory/%/"+id+".md",
	).Scan(jsonMapScan{&meta})
	if err == sql.ErrNoRows {
		return cliErr("no memory " + id + " for " + me)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	pinned, _ := meta["pinned"].(bool)
	return cliOK("pinned: "+fmt.Sprint(pinned), CliSideEffect{
		"event":    "memory.pinned",
		"command":  "memory pin",
		"memoryId": id,
		"agentId":  me,
		"pinned":   pinned,
	})
}

func (s *Service) cliMemoryDelete(ctx context.Context, parsed cliParsed, me string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: memory delete <id>")
	}
	id := parsed.positional[1]
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM agent_workspace WHERE agent_id = $1 AND path LIKE $2`, me, "memory/%/"+id+".md")
	if err != nil {
		return cliErrThrow(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cliErr("no memory " + id + " for " + me)
	}
	return cliOK("deleted "+id, CliSideEffect{
		"event":    "memory.deleted",
		"command":  "memory delete",
		"memoryId": id,
		"agentId":  me,
	})
}

/* ───────── log ───────── */

func (s *Service) cliCmdLog(ctx context.Context, parsed cliParsed) cliResult {
	op := "list"
	if len(parsed.positional) > 0 && parsed.positional[0] != "" {
		op = parsed.positional[0]
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if op == "note" {
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: log note <body> [--as id]")
		}
		id := "log-" + uuidHex()[:12]
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO agent_log (id, agent_id, kind, body) VALUES ($1, $2, 'note', $3)`,
			id, me, parsed.positional[1]); err != nil {
			return cliErrThrow(err)
		}
		return cliOK("logged " + id)
	}
	limit, err := cliMsgFlagNum(parsed, "limit", 30, 100)
	if err != nil {
		return cliErrThrow(err)
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, kind, body, ref, created_at
		   FROM agent_log WHERE agent_id = $1
		   ORDER BY created_at DESC LIMIT $2`, me, limit)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID        string     `json:"id"`
		Kind      string     `json:"kind"`
		Body      string     `json:"body"`
		Ref       cliRawJSON `json:"ref"`
		CreatedAt cliISOTime `json:"created_at"`
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Kind, &r.Body, &r.Ref, &r.CreatedAt); err != nil {
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
		return cliOK(fmt.Sprintf("(no log entries for %s)", me))
	}
	lines := []string{fmt.Sprintf("last %d log entries for %s:", len(all), me), ""}
	for _, r := range all {
		t := nodeLocaleString(time.Time(r.CreatedAt))
		lines = append(lines, "  ["+t+"] "+utf16PadEnd(r.Kind, 10)+" "+utf16Slice(r.Body, 200))
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ───────── tasks ───────── */

func (s *Service) cliCmdTasks(ctx context.Context, parsed cliParsed) cliResult {
	op := "list"
	if len(parsed.positional) > 0 && parsed.positional[0] != "" {
		op = parsed.positional[0]
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErrThrow(err)
	}
	switch op {
	case "list":
		args := []any{me}
		where := `agent_id = $1`
		if v, ok := parsed.flags["status"]; ok {
			args = append(args, fmt.Sprint(v))
			where += fmt.Sprintf(` AND status = $%d`, len(args))
		}
		rows, err := s.DB.QueryContext(ctx,
			`SELECT id, title, status, due_at, created_at, updated_at
			   FROM agent_tasks WHERE `+where+` ORDER BY status ASC, updated_at DESC`, args...)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type row struct {
			ID        string     `json:"id"`
			Title     string     `json:"title"`
			Status    string     `json:"status"`
			DueAt     *time.Time `json:"due_at"`
			CreatedAt cliISOTime `json:"created_at"`
			UpdatedAt cliISOTime `json:"updated_at"`
		}
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.ID, &r.Title, &r.Status, &r.DueAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
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
			return cliOK(fmt.Sprintf("(no tasks for %s)", me))
		}
		lines := []string{fmt.Sprintf("%d task(s) for %s:", len(all), me), ""}
		for _, t := range all {
			lines = append(lines, "  ["+utf16PadEnd(t.Status, 7)+"] "+utf16PadEnd(utf16Slice(t.ID, 12), 13)+" "+t.Title)
		}
		return cliOK(strings.Join(lines, "\n"))
	case "add":
		title := strings.Join(positionalFrom(parsed, 1), " ")
		if title == "" {
			return cliErr("usage: tasks add <title> [--as id]")
		}
		id := "task-" + uuidHex()[:12]
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO agent_tasks (id, agent_id, title) VALUES ($1, $2, $3)`, id, me, title); err != nil {
			return cliErrThrow(err)
		}
		effect := CliSideEffect{
			"event":         "task.created",
			"command":       "tasks add",
			"taskId":        id,
			"agentId":       me,
			"title":         title,
			"status":        "open",
			"visibleToUser": true,
		}
		if companyID != "" {
			effect["companyId"] = companyID
		}
		return cliOK("added task "+id+": "+title, effect)
	case "set":
		if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
			return cliErr("usage: tasks set <task_id> <status>")
		}
		id, status := parsed.positional[1], parsed.positional[2]
		switch status {
		case "open", "doing", "done", "dropped":
		default:
			return cliErr("bad status: " + status)
		}
		res, err := s.DB.ExecContext(ctx,
			`UPDATE agent_tasks SET status = $3, updated_at = NOW() WHERE id = $1 AND agent_id = $2`,
			id, me, status)
		if err != nil {
			return cliErrThrow(err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return cliErr("no task " + id + " for " + me)
		}
		effect := CliSideEffect{
			"event":         "task.status_changed",
			"command":       "tasks set",
			"taskId":        id,
			"agentId":       me,
			"status":        status,
			"visibleToUser": true,
		}
		if companyID != "" {
			effect["companyId"] = companyID
		}
		return cliOK("task "+id+" → "+status, effect)
	}
	return cliErr("usage: tasks <list|add|set> [...]")
}
