// /runtime/cli 文档组(#89):doc ls/create/read/append/prepend/image/
// image-delete/replace/replace-block/rename/delete。编辑走 docrelay 的
// ApplyAgentEdit(与 sidecar 同源),文本快照走 ReadDocumentText。
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type cliISOTimeAlias = cliISOTime

func nowMS() int64 { return time.Now().UnixMilli() }

func randHex16() string {
	// TS: randomUUID().replace(/-/g, '').slice(0, 16) —— 先去杠再切。
	return strings.ReplaceAll(uuidHex(), "-", "")[:16]
}

func (s *Service) publishDocChanged(companyID, documentID, kind, actorID string) {
	payload, err := jsonMarshalOrdered(map[string]any{
		"type":       "doc.changed",
		"kind":       kind,
		"companyId":  companyID,
		"documentId": documentID,
		"actorId":    actorID,
	})
	if err == nil {
		_ = s.publishRaw("cumora:docs", payload)
	}
}

// docOwnedBy:文档存在且属于本租户(返回 title 供 read 用;wantTitle=false 时 title 为空)。
func (s *Service) docOwnedBy(ctx context.Context, docID, companyID string, wantTitle bool) (string, bool, error) {
	q := `SELECT company_id FROM documents WHERE id = $1 LIMIT 1`
	if wantTitle {
		q = `SELECT company_id, title FROM documents WHERE id = $1 LIMIT 1`
	}
	var docCompany string
	var title sql.NullString
	row := s.DB.QueryRowContext(ctx, q, docID)
	var err error
	if wantTitle {
		err = row.Scan(&docCompany, &title)
	} else {
		err = row.Scan(&docCompany)
	}
	if err == sql.ErrNoRows || (err == nil && docCompany != companyID) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !wantTitle {
		return "", true, nil
	}
	return title.String, true, nil
}

func (s *Service) cliCmdDoc(ctx context.Context, parsed cliParsed) cliResult {
	op := "ls"
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
	if companyID == "" {
		return cliErr("unknown agent " + me + " (no company)")
	}
	if s.Relay == nil {
		return cliErrThrow(fmt.Errorf("doc collaboration unavailable (sidecar not configured)"))
	}
	switch op {
	case "ls", "list":
		rows, err := s.DB.QueryContext(ctx,
			`SELECT id, title, created_by, updated_at FROM documents
			  WHERE company_id = $1 ORDER BY updated_at DESC LIMIT 200`, companyID)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type row struct {
			ID        string     `json:"id"`
			Title     string     `json:"title"`
			CreatedBy string     `json:"created_by"`
			UpdatedAt cliISOTime `json:"updated_at"`
		}
		all := []row{}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.ID, &r.Title, &r.CreatedBy, &r.UpdatedAt); err != nil {
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
			return cliOK("(no documents in this workspace)")
		}
		lines := []string{fmt.Sprintf("%d document(s):", len(all)), ""}
		for _, d := range all {
			lines = append(lines, "  "+utf16PadEnd(d.ID, 24)+" "+d.Title)
		}
		return cliOK(strings.Join(lines, "\n"))
	case "create", "new":
		return s.cliDocCreate(ctx, parsed, me, companyID)
	case "read", "show":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: doc read <document_id>")
		}
		docID := parsed.positional[1]
		title, ok, err := s.docOwnedBy(ctx, docID, companyID, true)
		if err != nil {
			return cliErrThrow(err)
		}
		if !ok {
			return cliErr("document " + docID + " not found")
		}
		body, err := s.Relay.ReadDocumentText(ctx, docID, companyID)
		if err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONStringify(map[string]any{"id": docID, "title": title, "body": body})
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		bodyText := body
		if bodyText == "" {
			bodyText = "(empty)"
		}
		return cliOK(strings.Join([]string{"# " + title + "  (" + docID + ")", "", bodyText}, "\n"))
	case "append", "prepend":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr(`usage: doc ` + op + ` <document_id> "<text>"`)
		}
		docID := parsed.positional[1]
		text := strings.TrimSpace(strings.Join(positionalFrom(parsed, 2), " "))
		if text == "" {
			if t, ok := parsed.flagStr("text"); ok {
				text = cliUnescapeChat(t)
			}
		}
		if text == "" {
			return cliErr(`usage: doc ` + op + ` <document_id> "<text>"`)
		}
		if _, ok, err := s.docOwnedBy(ctx, docID, companyID, false); err != nil {
			return cliErrThrow(err)
		} else if !ok {
			return cliErr("document " + docID + " not found")
		}
		editKind := "append"
		opMap := map[string]any{"kind": "append", "text": text}
		if op == "prepend" {
			editKind = "prepend"
			opMap = map[string]any{"kind": "insertParagraph", "at": "start", "text": text}
		}
		if _, err := s.Relay.ApplyAgentEdit(ctx, docID, companyID, me, []map[string]any{opMap}); err != nil {
			return cliErrThrow(err)
		}
		s.publishDocChanged(companyID, docID, "document.updated", me)
		verb := "appended"
		if op == "prepend" {
			verb = "prepended"
		}
		return cliOK(fmt.Sprintf("%s %d chars to %s", verb, len(text), docID), CliSideEffect{
			"event":         "document.updated",
			"command":       "doc " + op,
			"documentId":    docID,
			"actorId":       me,
			"companyId":     companyID,
			"editKind":      editKind,
			"bodyLength":    len(text),
			"visibleToUser": true,
		})
	case "image":
		return s.cliDocImage(ctx, parsed, me, companyID)
	case "image-delete":
		return s.cliDocImageDelete(ctx, parsed, me, companyID)
	case "replace":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr(`usage: doc replace <document_id> --find "..." --replace "..."`)
		}
		docID := parsed.positional[1]
		find := parsed.flagStrOr("find", "")
		if v, ok := parsed.flagStr("find"); ok {
			find = cliUnescapeChat(v)
		}
		replace := ""
		if v, ok := parsed.flagStr("replace"); ok {
			replace = cliUnescapeChat(v)
		}
		if docID == "" || find == "" {
			return cliErr(`usage: doc replace <document_id> --find "..." --replace "..."`)
		}
		if _, ok, err := s.docOwnedBy(ctx, docID, companyID, false); err != nil {
			return cliErrThrow(err)
		} else if !ok {
			return cliErr("document " + docID + " not found")
		}
		r, err := s.Relay.ApplyAgentEdit(ctx, docID, companyID, me, []map[string]any{
			{"kind": "replace", "find": find, "replace": replace},
		})
		if err != nil {
			return cliErrThrow(err)
		}
		if r.Replaced == 0 {
			return cliErr("text not found in " + docID + ": " + utf16Slice(compactJSON(find), 80))
		}
		s.publishDocChanged(companyID, docID, "document.updated", me)
		return cliOK(fmt.Sprintf("replaced %d occurrence in %s", r.Replaced, docID), CliSideEffect{
			"event":         "document.updated",
			"command":       "doc replace",
			"documentId":    docID,
			"actorId":       me,
			"companyId":     companyID,
			"editKind":      "replace",
			"replaced":      r.Replaced,
			"visibleToUser": true,
		})
	case "replace-block":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr(`usage: doc replace-block <document_id> --anchor "<snippet in the block>" "<replacement markdown>"`)
		}
		docID := parsed.positional[1]
		anchor := ""
		if v, ok := parsed.flagStr("anchor"); ok {
			anchor = cliUnescapeChat(v)
		}
		text := strings.TrimSpace(strings.Join(positionalFrom(parsed, 2), " "))
		if text == "" {
			if t, ok := parsed.flagStr("text"); ok {
				text = cliUnescapeChat(t)
			}
		}
		if docID == "" || anchor == "" || text == "" {
			return cliErr(`usage: doc replace-block <document_id> --anchor "<snippet in the block>" "<replacement markdown>"`)
		}
		if _, ok, err := s.docOwnedBy(ctx, docID, companyID, false); err != nil {
			return cliErrThrow(err)
		} else if !ok {
			return cliErr("document " + docID + " not found")
		}
		r, err := s.Relay.ApplyAgentEdit(ctx, docID, companyID, me, []map[string]any{
			{"kind": "replaceBlock", "anchorText": anchor, "text": text},
		})
		if err != nil {
			return cliErrThrow(err)
		}
		if r.BlocksReplaced == 0 {
			return cliErr("no block containing " + utf16Slice(compactJSON(anchor), 80) + " in " + docID)
		}
		s.publishDocChanged(companyID, docID, "document.updated", me)
		return cliOK("replaced 1 block in "+docID, CliSideEffect{
			"event":         "document.updated",
			"command":       "doc replace-block",
			"documentId":    docID,
			"actorId":       me,
			"companyId":     companyID,
			"editKind":      "replace-block",
			"visibleToUser": true,
		})
	case "rename":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr(`usage: doc rename <document_id> "<title>"`)
		}
		docID := parsed.positional[1]
		title := strings.TrimSpace(strings.Join(positionalFrom(parsed, 2), " "))
		if title == "" {
			if t, ok := parsed.flagStr("title"); ok {
				title = t
			}
		}
		if title == "" {
			return cliErr(`usage: doc rename <document_id> "<title>"`)
		}
		res, err := s.DB.ExecContext(ctx,
			`UPDATE documents SET title = $1, updated_at = NOW() WHERE id = $2 AND company_id = $3`,
			utf16Slice(title, 200), docID, companyID)
		if err != nil {
			return cliErrThrow(err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return cliErr("document " + docID + " not found")
		}
		s.publishDocChanged(companyID, docID, "document.updated", me)
		return cliOK(fmt.Sprintf("renamed %s to %q", docID, title), CliSideEffect{
			"event":         "document.updated",
			"command":       "doc rename",
			"documentId":    docID,
			"actorId":       me,
			"companyId":     companyID,
			"editKind":      "rename",
			"title":         title,
			"visibleToUser": true,
		})
	case "delete", "rm":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: doc delete <document_id>")
		}
		docID := parsed.positional[1]
		var createdBy string
		err := s.DB.QueryRowContext(ctx,
			`SELECT created_by FROM documents WHERE id = $1 AND company_id = $2 LIMIT 1`,
			docID, companyID).Scan(&createdBy)
		if err == sql.ErrNoRows {
			return cliErr("document " + docID + " not found")
		}
		if err != nil {
			return cliErrThrow(err)
		}
		if createdBy != me {
			return cliErr("only the creator can delete document " + docID)
		}
		if _, err := s.DB.ExecContext(ctx, `DELETE FROM documents WHERE id = $1`, docID); err != nil {
			return cliErrThrow(err)
		}
		s.publishDocChanged(companyID, docID, "document.deleted", me)
		return cliOK("deleted document "+docID, CliSideEffect{
			"event":         "document.deleted",
			"command":       "doc delete",
			"documentId":    docID,
			"actorId":       me,
			"companyId":     companyID,
			"visibleToUser": true,
		})
	}
	return cliErr("unknown doc op: " + op + "\nusage: doc {ls|create|read|append|prepend|image|image-delete|replace|rename|delete} ...")
}

func (s *Service) cliDocCreate(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	title := strings.TrimSpace(strings.Join(positionalFrom(parsed, 1), " "))
	if title == "" {
		if t, ok := parsed.flagStr("title"); ok {
			title = t
		}
	}
	if title == "" {
		title = "Untitled"
	}
	// 租户级 claim 防并发同题;claim 窗口内再查权威表防顺序重复
	// (nova :17 建+释放、saga :22 直通的双猫事故)。
	if blocked := s.cliTryClaimTenantWork(companyID, me, "doc-create", title); blocked != nil {
		return *blocked
	}
	defer s.ReleaseWork("tenant:"+companyID, me, "doc-create", title)
	normTitle := NormalizeWorkSubject(title)
	docHoldScope := "doc-create:" + normTitle
	forceArmed := false
	if parsed.flagTruey("force") {
		forceArmed = s.ConsumeHold(me, docHoldScope).Armed
	}
	if !forceArmed {
		rows, err := s.DB.QueryContext(ctx,
			`SELECT id, title, created_by, created_at FROM documents
			  WHERE company_id = $1 AND created_by <> $2
			    AND created_at > NOW() - INTERVAL '15 minutes'
			  ORDER BY created_at DESC LIMIT 50`, companyID, me)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		type dupRow struct {
			id, title, createdBy string
			createdAt            cliISOTimeAlias
		}
		var dups []dupRow
		for rows.Next() {
			var d dupRow
			if err := rows.Scan(&d.id, &d.title, &d.createdBy, &d.createdAt); err != nil {
				return cliErrThrow(err)
			}
			dups = append(dups, d)
		}
		if err := rows.Err(); err != nil {
			return cliErrThrow(err)
		}
		for _, d := range dups {
			if NormalizeWorkSubject(d.title) == normTitle {
				s.RecordHold(me, docHoldScope, nil)
				ageSec := (nowMS() - timeOf(d.createdAt).UnixMilli() + 500) / 1000
				if ageSec < 1 {
					ageSec = 1
				}
				return cliErrCode(
					fmt.Sprintf("HELD — document NOT created. %s already created %q (%s) %ds ago — ", d.createdBy, d.title, d.id, ageSec)+
						`this work is DONE; a second copy is duplicate clutter. `+
						"Build on theirs instead: `cumora doc read "+d.id+"` / `cumora doc append "+d.id+" \"...\"`. "+
						`If you GENUINELY need a separate doc with this same title, rerun with --force `+
						`(--force only works after you've been shown this hold — passing it preemptively does nothing).`, 2)
			}
		}
	}
	id := "doc_" + randHex16()
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO documents (id, company_id, title, created_by) VALUES ($1, $2, $3, $4)`,
		id, companyID, utf16Slice(title, 200), me); err != nil {
		return cliErrThrow(err)
	}
	// --body 落成段落:换行切分,多行正文成为正确的块结构。
	body := ""
	if b, ok := parsed.flagStr("body"); ok {
		body = cliUnescapeChat(b)
	}
	if body != "" {
		if _, err := s.Relay.ApplyAgentEdit(ctx, id, companyID, me, []map[string]any{
			{"kind": "append", "text": body},
		}); err != nil {
			return cliErrThrow(err)
		}
	}
	s.publishDocChanged(companyID, id, "document.created", me)
	return cliOK("created document "+id+": "+title, CliSideEffect{
		"event":         "document.created",
		"command":       "doc create",
		"documentId":    id,
		"actorId":       me,
		"companyId":     companyID,
		"title":         title,
		"bodyLength":    len(body),
		"visibleToUser": true,
	})
}

func (s *Service) cliDocImage(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr(`usage: doc image <document_id> <url> [--alt "..."] [--at end|start | --replace "..." | --after "..." | --before "..."]`)
	}
	docID := parsed.positional[1]
	src := ""
	if len(parsed.positional) > 2 {
		src = parsed.positional[2]
	}
	if src == "" {
		if v, ok := parsed.flagStr("src"); ok {
			src = cliUnescapeChat(v)
		}
	}
	alt := ""
	if v, ok := parsed.flagStr("alt"); ok {
		alt = strings.TrimSpace(cliUnescapeChat(v))
	}
	replaceAnchor := ""
	if v, ok := parsed.flagStr("replace"); ok {
		replaceAnchor = cliUnescapeChat(v)
	}
	afterAnchor := ""
	if v, ok := parsed.flagStr("after"); ok {
		afterAnchor = cliUnescapeChat(v)
	}
	beforeAnchor := ""
	if v, ok := parsed.flagStr("before"); ok {
		beforeAnchor = cliUnescapeChat(v)
	}
	atRaw := strings.ToLower(strings.TrimSpace(parsed.flagStrOr("at", "end")))
	if docID == "" || src == "" {
		return cliErr(`usage: doc image <document_id> <url> [--alt "..."] [--at end|start | --replace "..." | --after "..." | --before "..."]`)
	}
	if !strings.HasPrefix(strings.ToLower(src), "http://") && !strings.HasPrefix(strings.ToLower(src), "https://") {
		return cliErr("image url must be http(s)://")
	}
	// 锚定旗标优先于 --at;多锚并给即报错(意图必须无歧义)。
	type anchorPair struct {
		mode, text string
	}
	var anchors []anchorPair
	if replaceAnchor != "" {
		anchors = append(anchors, anchorPair{"replace", replaceAnchor})
	}
	if afterAnchor != "" {
		anchors = append(anchors, anchorPair{"after", afterAnchor})
	}
	if beforeAnchor != "" {
		anchors = append(anchors, anchorPair{"before", beforeAnchor})
	}
	if len(anchors) > 1 {
		return cliErr(fmt.Sprintf("pass only one of --replace / --after / --before (got %d)", len(anchors)))
	}
	anchored := len(anchors) == 1
	mode := "end"
	anchorText := ""
	if anchored {
		mode = anchors[0].mode
		anchorText = anchors[0].text
	} else if atRaw == "start" {
		mode = "start"
	}
	if _, ok, err := s.docOwnedBy(ctx, docID, companyID, false); err != nil {
		return cliErrThrow(err)
	} else if !ok {
		return cliErr("document " + docID + " not found")
	}
	opMap := map[string]any{"kind": "image", "src": src}
	if alt != "" {
		opMap["alt"] = alt
	} else {
		opMap["alt"] = nil
	}
	if anchored {
		opMap["placement"] = map[string]any{"mode": mode, "anchorText": anchorText}
	} else {
		opMap["placement"] = map[string]any{"mode": mode}
	}
	result, err := s.Relay.ApplyAgentEdit(ctx, docID, companyID, me, []map[string]any{opMap})
	if err != nil {
		return cliErrThrow(err)
	}
	// 锚 miss 是硬错误:miss 回退 end-of-doc 正是文档堆积重复惰性图
	// 像的来源。不发 change 事件,bash 非零退出让 agent 知道换锚重试。
	if anchored && result.ImagePlaced != nil && *result.ImagePlaced == "anchor-missed" {
		return cliErr(fmt.Sprintf("anchor not found in %s: %q. Re-read the doc and pick a snippet that uniquely identifies the target block — no image was inserted.", docID, utf16Slice(anchorText, 60)))
	}
	s.publishDocChanged(companyID, docID, "document.updated", me)
	where := ""
	if anchored {
		where = mode + " block containing \"" + utf16Slice(anchorText, 60) + "\""
	} else if mode == "start" {
		where = "at start"
	} else {
		where = "at end"
	}
	return cliOK("inserted image into "+docID+" "+where, CliSideEffect{
		"event":         "document.updated",
		"command":       "doc image",
		"documentId":    docID,
		"actorId":       me,
		"companyId":     companyID,
		"editKind":      "image",
		"visibleToUser": true,
	})
}

func (s *Service) cliDocImageDelete(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr(`usage: doc image-delete <document_id> [--src <exact_url> | --src-contains <substr> | --alt <text>]`)
	}
	docID := parsed.positional[1]
	srcExact := ""
	if v, ok := parsed.flagStr("src"); ok {
		srcExact = cliUnescapeChat(v)
	}
	srcContains := ""
	if v, ok := parsed.flagStr("src-contains"); ok {
		srcContains = cliUnescapeChat(v)
	}
	altMatch := ""
	if v, ok := parsed.flagStr("alt"); ok {
		altMatch = cliUnescapeChat(v)
	}
	provided := 0
	for _, v := range []string{srcExact, srcContains, altMatch} {
		if v != "" {
			provided++
		}
	}
	if provided == 0 {
		return cliErr(`usage: doc image-delete <document_id> [--src <exact_url> | --src-contains <substr> | --alt <text>]`)
	}
	if provided > 1 {
		return cliErr("pass only one of --src / --src-contains / --alt")
	}
	if _, ok, err := s.docOwnedBy(ctx, docID, companyID, false); err != nil {
		return cliErrThrow(err)
	} else if !ok {
		return cliErr("document " + docID + " not found")
	}
	var match map[string]any
	if srcExact != "" {
		match = map[string]any{"by": "src", "src": srcExact}
	} else if srcContains != "" {
		match = map[string]any{"by": "src-contains", "substring": srcContains}
	} else {
		match = map[string]any{"by": "alt", "alt": altMatch}
	}
	result, err := s.Relay.ApplyAgentEdit(ctx, docID, companyID, me, []map[string]any{
		{"kind": "imageDelete", "match": match},
	})
	if err != nil {
		return cliErrThrow(err)
	}
	if result.ImagesDeleted == 0 {
		return cliErr("no images in " + docID + " matched the criterion")
	}
	plural := "s"
	if result.ImagesDeleted == 1 {
		plural = ""
	}
	s.publishDocChanged(companyID, docID, "document.updated", me)
	return cliOK(fmt.Sprintf("deleted %d image%s from %s", result.ImagesDeleted, plural, docID), CliSideEffect{
		"event":         "document.updated",
		"command":       "doc image-delete",
		"documentId":    docID,
		"actorId":       me,
		"companyId":     companyID,
		"editKind":      "image-delete",
		"imagesDeleted": result.ImagesDeleted,
		"visibleToUser": true,
	})
}
