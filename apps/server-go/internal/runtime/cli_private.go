// /runtime/cli 私有状态组(#89):topic·topic-set·rename / memory /
// climate / log / workspace(团队)/ ws(私有区)/ tasks。
package runtime

import (
	"bytes"
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

// nodeLocaleDate:en-US toLocaleDateString = "8/27/2026"。
func nodeLocaleDate(t time.Time) string {
	return fmt.Sprintf("%d/%d/%d", t.Month(), t.Day(), t.Year())
}

/* ───────── topic / topic-set / rename ───────── */

func (s *Service) cliCmdTopicRead(ctx context.Context, parsed cliParsed) cliResult {
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr("usage: topic <conversation_id>")
	}
	convoID := parsed.positional[0]
	var topic sql.NullString
	var title string
	err := s.DB.QueryRowContext(ctx,
		`SELECT topic, title FROM conversations WHERE id = $1`, convoID).Scan(&topic, &title)
	if err == sql.ErrNoRows {
		return cliErr("unknown conversation " + convoID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if !topic.Valid || topic.String == "" {
		return cliOK(fmt.Sprintf("(no topic set on %q)", title))
	}
	return cliOK(topic.String)
}

func (s *Service) publishConvoUpdated(conversationID, companyID string, patch map[string]any) {
	payload, err := jsonMarshalOrdered(map[string]any{
		"type":           "conversation.updated",
		"conversationId": conversationID,
		"companyId":      companyID,
		"patch":          patch,
	})
	if err == nil {
		_ = s.publishRaw("cumora:convo.updated", payload)
	}
}

func (s *Service) publishRaw(channel string, payload []byte) error {
	if s.RDB == nil {
		return nil
	}
	return s.RDB.Publish(context.Background(), channel, payload).Err()
}

func jsonMarshalOrdered(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := newJSONEncoderNoEscape(&buf).Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

func (s *Service) cliCmdTopicSet(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr(`usage: topic-set <conversation_id> "<text>"  (empty body clears the topic)`)
	}
	convoID := parsed.positional[0]
	raw := strings.TrimSpace(cliUnescapeChat(strings.Join(parsed.positional[1:], " ")))
	var topic any
	if len(raw) > 0 {
		topic = utf16Slice(raw, 200)
	}
	var members cliStrArr
	var companyID string
	err = s.DB.QueryRowContext(ctx,
		`SELECT members, company_id FROM conversations WHERE id = $1`, convoID,
	).Scan(&members, &companyID)
	if err == sql.ErrNoRows {
		return cliErr("unknown conversation " + convoID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if !containsString(members, me) {
		return cliErr(me + " is not a member of " + convoID)
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE conversations SET topic = $2, updated_at = NOW() WHERE id = $1`, convoID, topic); err != nil {
		return cliErrThrow(err)
	}
	s.publishConvoUpdated(convoID, companyID, map[string]any{"topic": topic})
	effect := cliSideEffect{
		"event":          "conversation.topic_updated",
		"command":        "topic-set",
		"conversationId": convoID,
		"actorId":        me,
		"companyId":      companyID,
		"topic":          topic,
		"visibleToUser":  true,
	}
	if topic != nil {
		return cliOK(fmt.Sprintf("topic set: %q", topic.(string)), effect)
	}
	return cliOK("(topic cleared)", effect)
}

func (s *Service) cliCmdRename(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	if len(parsed.positional) == 0 || parsed.positional[0] == "" {
		return cliErr(`usage: rename <conversation_id> "<new title>"`)
	}
	convoID := parsed.positional[0]
	title := utf16Slice(strings.TrimSpace(cliUnescapeChat(strings.Join(parsed.positional[1:], " "))), 80)
	if title == "" {
		return cliErr("rename requires a non-empty title")
	}
	var members cliStrArr
	var kind, companyID, currentTitle string
	err = s.DB.QueryRowContext(ctx,
		`SELECT members, kind, company_id, title FROM conversations WHERE id = $1`, convoID,
	).Scan(&members, &kind, &companyID, &currentTitle)
	if err == sql.ErrNoRows {
		return cliErr("unknown conversation " + convoID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if kind != "group" {
		return cliErr(fmt.Sprintf("only group chats can be renamed (%s is a %s)", convoID, kind))
	}
	if !containsString(members, me) {
		return cliErr(me + " is not a member of " + convoID)
	}
	// 乐观并发:--if-equals 声明调用方相信的当前标题;不符即拒绝重读。
	if ifEqualsRaw, ok := parsed.flagStr("if-equals"); ok {
		ifEquals := utf16Slice(strings.TrimSpace(cliUnescapeChat(ifEqualsRaw)), 80)
		if currentTitle != ifEquals {
			return cliErr(fmt.Sprintf("stale: current title is %q, you passed --if-equals %q. Re-read with `cumora conversations` and decide if you still want to rename.", currentTitle, ifEquals))
		}
	}
	// 幂等 no-op:标题已是目标值即返回成功,不发事件不广播 —— 压掉 N 个
	// agent 同瞬间改同名的噪声;真正的变化才写穿。
	if currentTitle == title {
		return cliOK(fmt.Sprintf("(no-op — title was already %q)", title))
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE conversations SET title = $2, updated_at = NOW() WHERE id = $1`, convoID, title); err != nil {
		return cliErrThrow(err)
	}
	s.publishConvoUpdated(convoID, companyID, map[string]any{"title": title})
	return cliOK(fmt.Sprintf("renamed to %q (%s)", title, convoID), cliSideEffect{
		"event":          "conversation.renamed",
		"command":        "rename",
		"conversationId": convoID,
		"actorId":        me,
		"companyId":      companyID,
		"title":          title,
		"visibleToUser":  true,
	})
}

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
	return cliOK("saved memory "+id, cliSideEffect{
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
	return cliOK("pinned: "+fmt.Sprint(pinned), cliSideEffect{
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
	return cliOK("deleted "+id, cliSideEffect{
		"event":    "memory.deleted",
		"command":  "memory delete",
		"memoryId": id,
		"agentId":  me,
	})
}

/* ───────── climate(情感系统)───────── */

// clamp11:夹到 [-1,1];垃圾输入归 0。
func clamp11(v any) float64 {
	var n float64
	switch t := v.(type) {
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0
		}
		n = f
	case bool:
		if t {
			return 1 // TS Number(true)=1
		}
		return 0
	case float64:
		n = t
	default:
		return 0
	}
	if n > 1 {
		return 1
	}
	if n < -1 {
		return -1
	}
	return n
}

func fmtSigned2(n float64) string {
	if n >= 0 {
		return fmt.Sprintf("+%.2f", n)
	}
	return fmt.Sprintf("%.2f", n)
}

func (s *Service) cliCmdClimate(ctx context.Context, parsed cliParsed) cliResult {
	op := "read"
	if len(parsed.positional) > 0 && parsed.positional[0] != "" {
		op = parsed.positional[0]
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	switch op {
	case "read":
		about := ""
		if len(parsed.positional) > 1 {
			about = parsed.positional[1]
		}
		args := []any{me}
		where := `agent_id = $1`
		if about != "" {
			args = append(args, about)
			where += fmt.Sprintf(` AND about_id = $%d`, len(args))
		}
		// affinity/trust 是 float4 —— 二进制解码经 float32 中转丢精度,
		// ::text + ParseFloat 对齐 node-pg 文本解析(#60 坑)。
		rows, err := s.DB.QueryContext(ctx,
			`SELECT about_id, affinity::text, trust::text, last_note, updated_at
			   FROM agent_climate WHERE `+where+`
			   ORDER BY updated_at DESC LIMIT 50`, args...)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type row struct {
			AboutID   string     `json:"about_id"`
			Affinity  float64    `json:"affinity"`
			Trust     float64    `json:"trust"`
			LastNote  string     `json:"last_note"`
			UpdatedAt cliISOTime `json:"updated_at"`
		}
		var all []row
		for rows.Next() {
			var r row
			var affS, truS string
			if err := rows.Scan(&r.AboutID, &affS, &truS, &r.LastNote, &r.UpdatedAt); err != nil {
				return cliErrThrow(err)
			}
			r.Affinity, _ = strconv.ParseFloat(affS, 64)
			r.Trust, _ = strconv.ParseFloat(truS, 64)
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
			if about != "" {
				return cliOK(fmt.Sprintf("(no climate noted for %s → %s)", me, about))
			}
			return cliOK(fmt.Sprintf("(no climate notes saved yet for %s)", me))
		}
		plural := "s"
		if len(all) == 1 {
			plural = ""
		}
		lines := []string{fmt.Sprintf("Climate around %s (%d relationship%s):", me, len(all), plural), ""}
		for _, r := range all {
			t := nodeLocaleDate(time.Time(r.UpdatedAt))
			lines = append(lines, "  "+utf16PadEnd(r.AboutID, 10)+"  affinity="+fmtSigned2(r.Affinity)+
				"  trust="+fmtSigned2(r.Trust)+"  "+t+"\n      "+
				strings.ReplaceAll(utf16Slice(r.LastNote, 240), "\n", " \\n "))
		}
		return cliOK(strings.Join(lines, "\n"))
	case "note":
		return s.cliClimateNote(ctx, parsed, me)
	case "forget":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: climate forget <about_id>")
		}
		aboutID := parsed.positional[1]
		res, err := s.DB.ExecContext(ctx,
			`DELETE FROM agent_climate WHERE agent_id = $1 AND about_id = $2`, me, aboutID)
		if err != nil {
			return cliErrThrow(err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return cliErr("no climate to forget for " + me + " → " + aboutID)
		}
		return cliOK("forgot climate "+me+" → "+aboutID, cliSideEffect{
			"event":   "climate.deleted",
			"command": "climate forget",
			"agentId": me,
			"aboutId": aboutID,
		})
	}
	return cliErr("usage: climate <read|note|forget> [...]")
}

func (s *Service) cliClimateNote(ctx context.Context, parsed cliParsed, me string) cliResult {
	aboutID := ""
	if len(parsed.positional) > 1 {
		aboutID = parsed.positional[1]
	}
	note := strings.TrimSpace(cliUnescapeChat(strings.Join(positionalFrom(parsed, 2), " ")))
	if aboutID == "" || note == "" {
		return cliErr(`usage: climate note <about_id> "<note>" [--affinity -1..1] [--trust -1..1]`)
	}
	affinityFlag, hasAffinity := parsed.flags["affinity"]
	trustFlag, hasTrust := parsed.flags["trust"]
	var prevAffinity, prevTrust float64
	var prevHistory cliRawJSON
	err := s.DB.QueryRowContext(ctx,
		`SELECT affinity::text, trust::text, history FROM agent_climate WHERE agent_id = $1 AND about_id = $2`,
		me, aboutID).Scan(&affinityTextScan{&prevAffinity}, &affinityTextScan{&prevTrust}, &prevHistory)
	if err != nil && err != sql.ErrNoRows {
		return cliErrThrow(err)
	}
	nextAffinity := prevAffinity
	if hasAffinity {
		nextAffinity = clamp11(affinityFlag)
	}
	nextTrust := prevTrust
	if hasTrust {
		nextTrust = clamp11(trustFlag)
	}
	// 追加历史并截到最近 20 条。
	var prevList []any
	if prevHistory != nil && string(prevHistory) != "null" {
		_ = jsonUnmarshal(prevHistory, &prevList)
	}
	if len(prevList) > 19 {
		prevList = prevList[len(prevList)-19:]
	}
	newHistory := append(prevList, map[string]any{
		"at":       isoNowMs(),
		"affinity": nextAffinity,
		"trust":    nextTrust,
		"note":     utf16Slice(note, 400),
	})
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO agent_climate (agent_id, about_id, affinity, trust, last_note, history, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, NOW())
		 ON CONFLICT (agent_id, about_id) DO UPDATE
		   SET affinity = EXCLUDED.affinity,
		       trust    = EXCLUDED.trust,
		       last_note = EXCLUDED.last_note,
		       history   = EXCLUDED.history,
		       updated_at = NOW()`,
		me, aboutID, nextAffinity, nextTrust, utf16Slice(note, 400), compactJSON(newHistory)); err != nil {
		return cliErrThrow(err)
	}
	return cliOK(fmt.Sprintf("climate updated: %s → %s  affinity=%.2f  trust=%.2f", me, aboutID, nextAffinity, nextTrust), cliSideEffect{
		"event":    "climate.updated",
		"command":  "climate note",
		"agentId":  me,
		"aboutId":  aboutID,
		"affinity": nextAffinity,
		"trust":    nextTrust,
	})
}

type affinityTextScan struct{ dst *float64 }

func (a affinityTextScan) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		return nil
	case []byte:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil {
			return err
		}
		*a.dst = f
		return nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return err
		}
		*a.dst = f
		return nil
	default:
		return fmt.Errorf("affinityTextScan: unsupported %T", src)
	}
}

// positionalFrom:切片越界安全。
func positionalFrom(p cliParsed, from int) []string {
	if from >= len(p.positional) {
		return nil
	}
	return p.positional[from:]
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

// cliEnsureDefaultWorkspace:默认区惰性自愈(server/uploads/workspaces/<cid>)。
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
		return cliOK(fmt.Sprintf("wrote %s in %s (%d chars)", path, wsName, len(body)), cliSideEffect{
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
		effect := cliSideEffect{
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
		effect := cliSideEffect{
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
		effect := cliSideEffect{
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
		effect := cliSideEffect{
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
		effect := cliSideEffect{
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
