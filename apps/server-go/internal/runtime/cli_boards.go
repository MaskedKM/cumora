// /runtime/cli 看板组(#89):kanban(板/列 CRUD+mentions 游标)、card
// (卡 CRUD+原子 claim)、claim/unclaim(泛化声明已废,提示语平价)。
package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

/* ───────── @mention 解析(与 REST 路由同一契约)───────── */

type mentionTarget struct{ id, name string }

var mentionFallbackRe = regexp.MustCompile(`(?i)^@([a-z0-9][a-z0-9_-]{0,63})`)

func isWordChar(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isMentionIDChar(r rune) bool {
	return r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// cliParseMentionTargets:@id 与 @名字双轨解析;候选按 token 长度降序最
// 长优先;start 边界要求前一个字符不是 [\w@];end 边界要求下一个字符
// 不是 [a-z0-9_-];无命中回落裸 id 正则;@all 必须滤;去重保序。
func cliParseMentionTargets(text string, targets []mentionTarget) []string {
	if text == "" {
		return []string{}
	}
	type candidate struct{ id, token string }
	var candidates []candidate
	for _, p := range targets {
		candidates = append(candidates, candidate{p.id, p.id}, candidate{p.id, strings.TrimSpace(p.name)})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return len(candidates[i].token) > len(candidates[j].token)
	})
	lower := strings.ToLower(text)
	runes := []rune(text)
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(runes); i++ {
		if runes[i] != '@' {
			continue
		}
		// start 边界:前一字符不得是 [\w@]
		if i > 0 {
			prev := runes[i-1]
			if prev == '@' || isMentionIDChar(prev) {
				continue
			}
		}
		rest := lower[i+1:]
		var matchedID, matchedToken string
		for _, c := range candidates {
			if c.token == "" {
				continue
			}
			if strings.HasPrefix(rest, strings.ToLower(c.token)) {
				end := i + 1 + len([]rune(c.token))
				if end >= len(runes) || !isMentionIDChar(runes[end]) {
					matchedID, matchedToken = c.id, c.token
					break
				}
			}
		}
		advance := 0
		id := ""
		if matchedID != "" {
			id = strings.ToLower(matchedID)
			advance = len([]rune(matchedToken))
		} else {
			m := mentionFallbackRe.FindStringSubmatch(string(runes[i:]))
			if m != nil {
				id = strings.ToLower(m[1])
				advance = len([]rune(m[0])) - 1
			}
		}
		if id == "" || id == "all" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		i += advance
	}
	return out
}

func (s *Service) cliParseMentions(ctx context.Context, companyID, text string) []string {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name FROM participants WHERE company_id = $1 AND departed_at IS NULL`, companyID)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	var targets []mentionTarget
	for rows.Next() {
		var t mentionTarget
		if rows.Scan(&t.id, &t.name) == nil {
			targets = append(targets, t)
		}
	}
	return cliParseMentionTargets(text, targets)
}

func (s *Service) publishBoardCli(companyID, kind, boardID string, cardID, columnID, commentID *string, mentions []string, actorID string) {
	payload := map[string]any{
		"type":     "board.changed",
		"companyId": companyID,
		"kind":     kind,
		"boardId":  boardID,
		"actorId":  actorID,
	}
	if cardID != nil {
		payload["cardId"] = *cardID
	}
	if columnID != nil {
		payload["columnId"] = *columnID
	}
	if commentID != nil {
		payload["commentId"] = *commentID
	}
	if mentions != nil {
		payload["mentions"] = mentions
	}
	if b, err := jsonMarshalOrdered(payload); err == nil {
		_ = s.publishRaw("cumora:boards", b)
	}
}

// wakeMentionedAgentsCli:唤醒 mentions 里同租户的非 actor agent(best
// effort;CLI 命令已成功,唤醒是尽力而为的副作用,不得反噬进程)。
func (s *Service) wakeMentionedAgentsCli(companyID string, mentions []string, actorID string) {
	if len(mentions) == 0 {
		return
	}
	go func() {
		defer func() { _ = recover() }()
		var targets []string
		for _, id := range mentions {
			if id != actorID {
				targets = append(targets, id)
			}
		}
		if len(targets) == 0 {
			return
		}
		ctx := context.Background()
		rows, err := s.DB.QueryContext(ctx,
			`SELECT id FROM participants
			  WHERE kind = 'agent' AND company_id = $1 AND id = ANY($2::text[])
			    AND departed_at IS NULL`, companyID, targets)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				s.WakeAgent(id, "manual", nil)
			}
		}
	}()
}

// boardOwnedBy:板存在且属于本租户;否则 false。
func (s *Service) boardOwnedBy(ctx context.Context, boardID, companyID string) bool {
	var c string
	err := s.DB.QueryRowContext(ctx,
		`SELECT company_id FROM boards WHERE id = $1 LIMIT 1`, boardID).Scan(&c)
	return err == nil && c == companyID
}

/* ───────── kanban ───────── */

func (s *Service) cliCmdBoard(ctx context.Context, parsed cliParsed) cliResult {
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
	switch op {
	case "ls", "list":
		return s.cliBoardLs(ctx, parsed, companyID)
	case "show", "view":
		return s.cliBoardShow(ctx, parsed, companyID)
	case "create", "new":
		return s.cliBoardCreate(ctx, parsed, me, companyID)
	case "rename", "edit", "update":
		return s.cliBoardRename(ctx, parsed, op, me, companyID)
	case "columns", "cols":
		return s.cliBoardColumns(ctx, parsed, companyID)
	case "add-column", "add-col":
		return s.cliBoardAddColumn(ctx, parsed, me, companyID)
	case "edit-column", "rename-column", "update-column":
		return s.cliBoardEditColumn(ctx, parsed, op, me, companyID)
	case "delete-column", "rm-column":
		return s.cliBoardDeleteColumn(ctx, parsed, op, me, companyID)
	case "delete", "rm":
		return s.cliBoardDelete(ctx, parsed, me, companyID)
	case "mentions":
		return s.cliBoardMentions(ctx, parsed, me, companyID)
	}
	return cliErr("usage: kanban <ls|show|create|rename|columns|add-column|edit-column|delete-column|delete|mentions> [...]")
}

func (s *Service) cliBoardLs(ctx context.Context, parsed cliParsed, companyID string) cliResult {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, title, description, updated_at FROM boards
		  WHERE company_id = $1 ORDER BY updated_at DESC`, companyID)
	if err != nil {
		return cliErrThrow(err)
	}
	defer rows.Close()
	type row struct {
		ID          string     `json:"id"`
		Title       string     `json:"title"`
		Description *string    `json:"description"`
		UpdatedAt   cliISOTime `json:"updated_at"`
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Title, &r.Description, &r.UpdatedAt); err != nil {
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
		return cliOK("(no boards in this workspace)")
	}
	lines := []string{fmt.Sprintf("%d board(s):", len(all)), ""}
	for _, b := range all {
		lines = append(lines, "  "+utf16PadEnd(b.ID, 20)+" "+b.Title)
	}
	return cliOK(strings.Join(lines, "\n"))
}

func (s *Service) cliBoardShow(ctx context.Context, parsed cliParsed, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: kanban show <board_id>")
	}
	boardID := parsed.positional[1]
	var bID, bTitle string
	var bDesc *string
	var bCompany string
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, title, description, company_id FROM boards WHERE id = $1 LIMIT 1`, boardID,
	).Scan(&bID, &bTitle, &bDesc, &bCompany)
	if err == sql.ErrNoRows || (err == nil && bCompany != companyID) {
		return cliErr("board " + boardID + " not found")
	}
	if err != nil {
		return cliErrThrow(err)
	}
	cols, err := s.cliBoardColumnsList(ctx, boardID)
	if err != nil {
		return cliErrThrow(err)
	}
	cards, err := s.cliBoardCardsList(ctx, boardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		type boardObj struct {
			ID          string  `json:"id"`
			Title       string  `json:"title"`
			Description *string `json:"description"`
			CompanyID   string  `json:"company_id"`
		}
		type showObj struct {
			Board   boardObj      `json:"board"`
			Columns []cliColumn   `json:"columns"`
			Cards   []cliCardRow  `json:"cards"`
		}
		js, e := cliJSONStringify(showObj{
			Board:   boardObj{bID, bTitle, bDesc, bCompany},
			Columns: cols,
			Cards:   cards,
		})
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	byCol := map[string][]cliCardRow{}
	var colOrder []string
	for _, c := range cards {
		if _, ok := byCol[c.ColumnID]; !ok {
			colOrder = append(colOrder, c.ColumnID)
		}
		byCol[c.ColumnID] = append(byCol[c.ColumnID], c)
	}
	lines := []string{"# " + bTitle + "  (" + bID + ")"}
	if bDesc != nil {
		lines = append(lines, *bDesc)
	}
	for _, col := range cols {
		list := byCol[col.ID]
		lines = append(lines, "", fmt.Sprintf("## %s  (%s)  · %d card(s)", col.Title, col.ID, len(list)))
		for _, c := range list {
			who := "(unassigned)"
			if c.AssigneeID != nil {
				who = "@" + *c.AssigneeID
			}
			mentions := ""
			if len(c.Mentions) > 0 {
				parts := make([]string, len(c.Mentions))
				for i, m := range c.Mentions {
					parts[i] = "@" + m
				}
				mentions = "  · mentions: " + strings.Join(parts, " ")
			}
			lines = append(lines, "  - "+utf16PadEnd(c.ID, 20)+" "+utf16PadEnd(who, 16)+" "+c.Title+mentions)
		}
	}
	return cliOK(strings.Join(lines, "\n"))
}

type cliColumn struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Position int64  `json:"position"`
}

func (s *Service) cliBoardColumnsList(ctx context.Context, boardID string) ([]cliColumn, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, title, position FROM board_columns WHERE board_id = $1 ORDER BY position ASC`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cliColumn
	for rows.Next() {
		var c cliColumn
		if err := rows.Scan(&c.ID, &c.Title, &c.Position); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type cliCardRow struct {
	ID         string    `json:"id"`
	ColumnID   string    `json:"column_id"`
	Title      string    `json:"title"`
	AssigneeID *string   `json:"assignee_id"`
	Mentions   cliStrArr `json:"mentions"`
	Position   int64     `json:"position"`
}

func (s *Service) cliBoardCardsList(ctx context.Context, boardID string) ([]cliCardRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, column_id, title, assignee_id, mentions, position
		   FROM board_cards WHERE board_id = $1 ORDER BY column_id, position ASC`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cliCardRow
	for rows.Next() {
		var c cliCardRow
		if err := rows.Scan(&c.ID, &c.ColumnID, &c.Title, &c.AssigneeID, &c.Mentions, &c.Position); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) cliBoardCreate(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	title := strings.TrimSpace(strings.Join(positionalFrom(parsed, 1), " "))
	if title == "" {
		if t, ok := parsed.flagStr("title"); ok {
			title = t
		}
	}
	if title == "" {
		return cliErr(`usage: kanban create "<title>" [--description "..."]`)
	}
	var description any
	if d, ok := parsed.flagStr("description"); ok {
		v := utf16Slice(cliUnescapeChat(d), 4000)
		description = v
	}
	id := "board-" + uuidHex()[:12]
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return cliErrThrow(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO boards (id, company_id, title, description, created_by) VALUES ($1, $2, $3, $4, $5)`,
		id, companyID, utf16Slice(title, 200), description, me); err != nil {
		return cliErrThrow(err)
	}
	for i, seed := range []string{"Todo", "Doing", "Done"} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, $3, $4)`,
			"col-"+uuidHex()[:12], id, seed, (i+1)*1000); err != nil {
			return cliErrThrow(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "board.created", id, nil, nil, nil, nil, me)
	return cliOK("created board "+id+": "+title, cliSideEffect{
		"event":         "kanban.board_created",
		"command":       "kanban create",
		"boardId":       id,
		"actorId":       me,
		"companyId":     companyID,
		"title":         title,
		"visibleToUser": true,
	})
}

func (s *Service) cliBoardRename(ctx context.Context, parsed cliParsed, op, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr(`usage: kanban ` + op + ` <board_id> --title "..." [--description "..."]`)
	}
	boardID := parsed.positional[1]
	if !s.boardOwnedBy(ctx, boardID, companyID) {
		return cliErr("board " + boardID + " not found")
	}
	var sets []string
	var params []any
	titleFlag, hasTitleFlag := parsed.flagStr("title")
	if hasTitleFlag || len(parsed.positional) > 2 {
		nextTitle := strings.Join(positionalFrom(parsed, 2), " ")
		if hasTitleFlag {
			nextTitle = cliUnescapeChat(titleFlag)
		}
		nextTitle = utf16Slice(strings.TrimSpace(nextTitle), 200)
		if nextTitle == "" {
			return cliErr("--title cannot be empty")
		}
		params = append(params, nextTitle)
		sets = append(sets, fmt.Sprintf("title = $%d", len(params)))
	}
	var nextDescription any
	if d, ok := parsed.flagStr("description"); ok {
		v := utf16Slice(strings.TrimSpace(cliUnescapeChat(d)), 4000)
		if v == "" {
			nextDescription = nil
		} else {
			nextDescription = v
		}
		params = append(params, nextDescription)
		sets = append(sets, fmt.Sprintf("description = $%d", len(params)))
	}
	if len(sets) == 0 {
		return cliErr("nothing to update — pass --title or --description")
	}
	params = append(params, boardID, companyID)
	var rowTitle string
	var rowDesc *string
	err := s.DB.QueryRowContext(ctx,
		`UPDATE boards SET `+strings.Join(sets, ", ")+`, updated_at = NOW()
		  WHERE id = $`+fmt.Sprint(len(params)-1)+` AND company_id = $`+fmt.Sprint(len(params))+
			` RETURNING title, description`, params...,
	).Scan(&rowTitle, &rowDesc)
	if err == sql.ErrNoRows {
		return cliErr("board " + boardID + " not found")
	}
	if err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "board.updated", boardID, nil, nil, nil, nil, me)
	effect := cliSideEffect{
		"event":         "kanban.board_updated",
		"command":       "kanban " + op,
		"boardId":       boardID,
		"actorId":       me,
		"companyId":     companyID,
		"title":         rowTitle,
		"description":   nilIfNilString(rowDesc),
		"visibleToUser": true,
	}
	return cliOK("updated board "+boardID+": "+rowTitle, effect)
}

func nilIfNilString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func (s *Service) cliBoardColumns(ctx context.Context, parsed cliParsed, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: kanban columns <board_id>")
	}
	boardID := parsed.positional[1]
	if !s.boardOwnedBy(ctx, boardID, companyID) {
		return cliErr("board " + boardID + " not found")
	}
	cols, err := s.cliBoardColumnsList(ctx, boardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONList(cols)
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	var lines []string
	for _, c := range cols {
		lines = append(lines, "  "+utf16PadEnd(c.ID, 20)+" "+c.Title)
	}
	if len(lines) == 0 {
		return cliOK("(no columns)")
	}
	return cliOK(strings.Join(lines, "\n"))
}

func (s *Service) cliBoardAddColumn(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr(`usage: kanban add-column <board_id> "<title>"`)
	}
	boardID := parsed.positional[1]
	title := strings.TrimSpace(strings.Join(positionalFrom(parsed, 2), " "))
	if title == "" {
		return cliErr(`usage: kanban add-column <board_id> "<title>"`)
	}
	if !s.boardOwnedBy(ctx, boardID, companyID) {
		return cliErr("board " + boardID + " not found")
	}
	var maxPos sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT MAX(position) AS max FROM board_columns WHERE board_id = $1`, boardID).Scan(&maxPos); err != nil {
		return cliErrThrow(err)
	}
	position := maxPos.Int64 + 1000
	id := "col-" + uuidHex()[:12]
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, $3, $4)`,
		id, boardID, utf16Slice(title, 100), position); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "column.created", boardID, nil, &id, nil, nil, me)
	return cliOK("added column "+id+": "+title, cliSideEffect{
		"event":         "kanban.column_created",
		"command":       "kanban add-column",
		"boardId":       boardID,
		"columnId":      id,
		"actorId":       me,
		"companyId":     companyID,
		"title":         title,
		"visibleToUser": true,
	})
}

func (s *Service) cliBoardEditColumn(ctx context.Context, parsed cliParsed, op, me, companyID string) cliResult {
	if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
		return cliErr(`usage: kanban ` + op + ` <board_id> <column_id> [--title "..."] [--position N]`)
	}
	boardID, columnID := parsed.positional[1], parsed.positional[2]
	if !s.boardOwnedBy(ctx, boardID, companyID) {
		return cliErr("board " + boardID + " not found")
	}
	var sets []string
	var params []any
	titleFlag, hasTitleFlag := parsed.flagStr("title")
	if hasTitleFlag || len(parsed.positional) > 3 {
		title := strings.Join(positionalFrom(parsed, 3), " ")
		if hasTitleFlag {
			title = cliUnescapeChat(titleFlag)
		}
		title = utf16Slice(strings.TrimSpace(title), 100)
		if title == "" {
			return cliErr("--title cannot be empty")
		}
		params = append(params, title)
		sets = append(sets, fmt.Sprintf("title = $%d", len(params)))
	}
	if v, ok := parsed.flags["position"]; ok {
		n, valid := jsFloorNumber(v)
		if !valid {
			return cliErr("invalid --position: " + fmt.Sprint(v))
		}
		params = append(params, n)
		sets = append(sets, fmt.Sprintf("position = $%d", len(params)))
	}
	if len(sets) == 0 {
		return cliErr("nothing to update — pass --title or --position")
	}
	params = append(params, columnID, boardID)
	var rowTitle string
	var rowPos int64
	err := s.DB.QueryRowContext(ctx,
		`UPDATE board_columns SET `+strings.Join(sets, ", ")+
			` WHERE id = $`+fmt.Sprint(len(params)-1)+` AND board_id = $`+fmt.Sprint(len(params))+
			` RETURNING title, position`, params...,
	).Scan(&rowTitle, &rowPos)
	if err == sql.ErrNoRows {
		return cliErr("column " + columnID + " not in board " + boardID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "column.updated", boardID, nil, &columnID, nil, nil, me)
	return cliOK("updated column "+columnID+": "+rowTitle, cliSideEffect{
		"event":         "kanban.column_updated",
		"command":       "kanban " + op,
		"boardId":       boardID,
		"columnId":      columnID,
		"actorId":       me,
		"companyId":     companyID,
		"title":         rowTitle,
		"position":      rowPos,
		"visibleToUser": true,
	})
}

func (s *Service) cliBoardDeleteColumn(ctx context.Context, parsed cliParsed, op, me, companyID string) cliResult {
	if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
		return cliErr(`usage: kanban ` + op + ` <board_id> <column_id>`)
	}
	boardID, columnID := parsed.positional[1], parsed.positional[2]
	if !s.boardOwnedBy(ctx, boardID, companyID) {
		return cliErr("board " + boardID + " not found")
	}
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM board_columns WHERE id = $1 AND board_id = $2`, columnID, boardID)
	if err != nil {
		return cliErrThrow(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cliErr("column " + columnID + " not in board " + boardID)
	}
	s.publishBoardCli(companyID, "column.deleted", boardID, nil, &columnID, nil, nil, me)
	return cliOK("deleted column "+columnID, cliSideEffect{
		"event":         "kanban.column_deleted",
		"command":       "kanban " + op,
		"boardId":       boardID,
		"columnId":      columnID,
		"actorId":       me,
		"companyId":     companyID,
		"visibleToUser": true,
	})
}

func (s *Service) cliBoardDelete(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: kanban delete <board_id>")
	}
	boardID := parsed.positional[1]
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM boards WHERE id = $1 AND company_id = $2`, boardID, companyID)
	if err != nil {
		return cliErrThrow(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cliErr("board " + boardID + " not found")
	}
	s.publishBoardCli(companyID, "board.deleted", boardID, nil, nil, nil, nil, me)
	return cliOK("deleted board "+boardID, cliSideEffect{
		"event":         "kanban.board_deleted",
		"command":       "kanban delete",
		"boardId":       boardID,
		"actorId":       me,
		"companyId":     companyID,
		"visibleToUser": true,
	})
}

func (s *Service) cliBoardMentions(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	// 自上次检查以来谁 @ 了我(卡+评论):读游标 → 返回未读集 →(除非
	// --peek)把游标推到 NOW,下次只给真正的新东西。
	peek := parsed.flagTruey("peek")
	since := "1970-01-01T00:00:00Z"
	var sinceAt sql.NullTime
	err := s.DB.QueryRowContext(ctx,
		`SELECT last_read_at FROM board_mention_reads WHERE user_id = $1 LIMIT 1`, me).Scan(&sinceAt)
	if err != nil && err != sql.ErrNoRows {
		return cliErrThrow(err)
	}
	if err == nil && sinceAt.Valid {
		since = isoMilli(sinceAt.Time)
	}
	type cardMention struct {
		ID         string     `json:"id"`
		BoardID    string     `json:"board_id"`
		ColumnID   string     `json:"column_id"`
		Title      string     `json:"title"`
		UpdatedAt  cliISOTime `json:"updated_at"`
		CreatedBy  string     `json:"created_by"`
		BoardTitle string     `json:"board_title"`
	}
	type commentMention struct {
		ID         string     `json:"id"`
		CardID     string     `json:"card_id"`
		Body       string     `json:"body"`
		AuthorID   string     `json:"author_id"`
		CreatedAt  cliISOTime `json:"created_at"`
		BoardID    string     `json:"board_id"`
		CardTitle  string     `json:"card_title"`
		BoardTitle string     `json:"board_title"`
	}
	cardsR, err := s.DB.QueryContext(ctx,
		`SELECT c.id, c.board_id, c.column_id, c.title, c.updated_at, c.created_by,
		        b.title AS board_title
		   FROM board_cards c
		   JOIN boards b ON b.id = c.board_id
		  WHERE b.company_id = $1
		    AND c.updated_at > $2
		    AND c.mentions @> to_jsonb($3::text)
		  ORDER BY c.updated_at DESC
		  LIMIT 50`, companyID, since, me)
	if err != nil {
		return cliErrThrow(err)
	}
	var cards []cardMention
	for cardsR.Next() {
		var c cardMention
		if err := cardsR.Scan(&c.ID, &c.BoardID, &c.ColumnID, &c.Title, &c.UpdatedAt, &c.CreatedBy, &c.BoardTitle); err != nil {
			cardsR.Close()
			return cliErrThrow(err)
		}
		cards = append(cards, c)
	}
	cardsR.Close()
	if err := cardsR.Err(); err != nil {
		return cliErrThrow(err)
	}
	commentsR, err := s.DB.QueryContext(ctx,
		`SELECT cm.id, cm.card_id, cm.body, cm.author_id, cm.created_at,
		        c.board_id, c.title AS card_title, b.title AS board_title
		   FROM board_card_comments cm
		   JOIN board_cards c ON c.id = cm.card_id
		   JOIN boards b ON b.id = c.board_id
		  WHERE b.company_id = $1
		    AND cm.created_at > $2
		    AND cm.mentions @> to_jsonb($3::text)
		  ORDER BY cm.created_at DESC
		  LIMIT 50`, companyID, since, me)
	if err != nil {
		return cliErrThrow(err)
	}
	var comments []commentMention
	for commentsR.Next() {
		var c commentMention
		if err := commentsR.Scan(&c.ID, &c.CardID, &c.Body, &c.AuthorID, &c.CreatedAt, &c.BoardID, &c.CardTitle, &c.BoardTitle); err != nil {
			commentsR.Close()
			return cliErrThrow(err)
		}
		comments = append(comments, c)
	}
	commentsR.Close()
	if err := commentsR.Err(); err != nil {
		return cliErrThrow(err)
	}
	if !peek {
		if _, err := s.DB.ExecContext(ctx,
			`INSERT INTO board_mention_reads (user_id, last_read_at)
			 VALUES ($1, NOW())
			 ON CONFLICT (user_id) DO UPDATE SET last_read_at = NOW()`, me); err != nil {
			return cliErrThrow(err)
		}
	}
	if parsed.flagTruey("json") {
		js, e := cliJSONStringify(map[string]any{
			"since":    since,
			"cards":    cards,
			"comments": comments,
		})
		if e != nil {
			return cliErrThrow(e)
		}
		return cliOK(js)
	}
	if len(cards) == 0 && len(comments) == 0 {
		return cliOK(fmt.Sprintf("(no new kanban @-mentions for %s since %s)", me, since))
	}
	lines := []string{fmt.Sprintf("%d new kanban @-mention(s) for %s:", len(cards)+len(comments), me)}
	if len(cards) > 0 {
		lines = append(lines, "", "--- cards ---")
		for _, c := range cards {
			lines = append(lines, fmt.Sprintf("  %s  [%s / %s]  %s  · by %s at %s", c.ID, c.BoardTitle, c.ColumnID, c.Title, c.CreatedBy, isoMilli(timeOf(c.UpdatedAt))))
		}
	}
	if len(comments) > 0 {
		lines = append(lines, "", "--- comments ---")
		for _, cm := range comments {
			lines = append(lines, fmt.Sprintf("  %s  on card %s [%s]  · by %s at %s", cm.ID, cm.CardID, cm.BoardTitle, cm.AuthorID, isoMilli(timeOf(cm.CreatedAt))))
			lines = append(lines, "    \""+utf16Slice(strings.ReplaceAll(cm.Body, "\n", " "), 200)+"\"")
		}
	}
	if !peek {
		lines = append(lines, "", "(read cursor advanced — next call shows only newer mentions; use --peek to keep it)")
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ───────── claim / unclaim(泛化声明已废)───────── */

func (s *Service) cliCmdClaim(ctx context.Context, parsed cliParsed, mode string) cliResult {
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
	key := ""
	if len(parsed.positional) > 0 {
		key = strings.TrimSpace(parsed.positional[0])
	}
	if key == "" {
		usage := `usage: ` + mode + ` "<what you're claiming>" [--in <conversation_id>]`
		if mode == "claim" {
			usage += ` [--ttl <seconds>]`
		}
		return cliErr(usage)
	}
	// 泛化字符串锁已废:真正存在的 claim 只有板卡上的任务声明
	// (cumora card claim);回合/槽位声明一律引导为"直接发真内容,
	// 服务端 HOLD 兜底"。
	if mode == "unclaim" {
		return cliOK("ok — nothing to release. Cumora no longer uses generic claims; just post, the server settles races.")
	}
	return cliErr(
		"Claiming a turn / game slot / activity is not a thing anymore. " +
			"Do NOT reserve a position and wait for it. Read the latest posts and send the REAL next item (`cumora reply`); " +
			"if a peer moved the room while you composed, the reply comes back HELD with the newer messages — re-read and resend. " +
			"That IS the coordination. The only claim that exists is for a genuine shared DELIVERABLE on the board: `cumora card claim <cardId>`.")
}

/* ───────── card ───────── */

func (s *Service) cliCmdCard(ctx context.Context, parsed cliParsed) cliResult {
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
	resolveCardBoard := func(cardID string) (*struct{ boardID, columnID string }, error) {
		var boardID, columnID, cardCompany string
		err := s.DB.QueryRowContext(ctx,
			`SELECT c.board_id, c.column_id, b.company_id
			   FROM board_cards c JOIN boards b ON b.id = c.board_id
			  WHERE c.id = $1 LIMIT 1`, cardID).Scan(&boardID, &columnID, &cardCompany)
		if err == sql.ErrNoRows || (err == nil && cardCompany != companyID) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &struct{ boardID, columnID string }{boardID, columnID}, nil
	}
	switch op {
	case "ls", "list":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: card ls <board_id>")
		}
		boardID := parsed.positional[1]
		if !s.boardOwnedBy(ctx, boardID, companyID) {
			return cliErr("board " + boardID + " not found")
		}
		cards, err := s.cliBoardCardsList(ctx, boardID)
		if err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONList(cards)
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		if len(cards) == 0 {
			return cliOK("(no cards)")
		}
		var lines []string
		for _, c := range cards {
			who := "(unassigned)"
			if c.AssigneeID != nil {
				who = "@" + *c.AssigneeID
			}
			lines = append(lines, "  "+utf16PadEnd(c.ID, 20)+" ["+utf16PadEnd(utf16Slice(c.ColumnID, 16), 16)+"] "+utf16PadEnd(who, 16)+" "+c.Title)
		}
		return cliOK(strings.Join(lines, "\n"))
	case "show":
		if len(parsed.positional) < 2 || parsed.positional[1] == "" {
			return cliErr("usage: card show <card_id>")
		}
		cardID := parsed.positional[1]
		type cardDetail struct {
			ID          string     `json:"id"`
			BoardID     string     `json:"board_id"`
			ColumnID    string     `json:"column_id"`
			Title       string     `json:"title"`
			Description *string    `json:"description"`
			AssigneeID  *string    `json:"assignee_id"`
			Mentions    cliStrArr  `json:"mentions"`
			CreatedBy   string     `json:"created_by"`
			CreatedAt   cliISOTime `json:"created_at"`
			UpdatedAt   cliISOTime `json:"updated_at"`
			CompanyID   string     `json:"company_id"`
		}
		var c cardDetail
		err := s.DB.QueryRowContext(ctx,
			`SELECT c.id, c.board_id, c.column_id, c.title, c.description,
			        c.assignee_id, c.mentions, c.created_by, c.created_at, c.updated_at,
			        b.company_id
			   FROM board_cards c JOIN boards b ON b.id = c.board_id
			  WHERE c.id = $1 LIMIT 1`, cardID,
		).Scan(&c.ID, &c.BoardID, &c.ColumnID, &c.Title, &c.Description, &c.AssigneeID,
			&c.Mentions, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.CompanyID)
		if err == sql.ErrNoRows || (err == nil && c.CompanyID != companyID) {
			return cliErr("card " + cardID + " not found")
		}
		if err != nil {
			return cliErrThrow(err)
		}
		type commentRow struct {
			ID        string     `json:"id"`
			AuthorID  string     `json:"author_id"`
			Body      string     `json:"body"`
			CreatedAt cliISOTime `json:"created_at"`
		}
		rows, err := s.DB.QueryContext(ctx,
			`SELECT id, author_id, body, created_at
			   FROM board_card_comments WHERE card_id = $1 ORDER BY created_at ASC`, cardID)
		if err != nil {
			return cliErrThrow(err)
		}
		defer rows.Close()
		comments := []commentRow{}
		for rows.Next() {
			var cm commentRow
			if err := rows.Scan(&cm.ID, &cm.AuthorID, &cm.Body, &cm.CreatedAt); err != nil {
				return cliErrThrow(err)
			}
			comments = append(comments, cm)
		}
		if err := rows.Err(); err != nil {
			return cliErrThrow(err)
		}
		if parsed.flagTruey("json") {
			js, e := cliJSONStringify(map[string]any{"card": c, "comments": comments})
			if e != nil {
				return cliErrThrow(e)
			}
			return cliOK(js)
		}
		assignee := "(unassigned)"
		if c.AssigneeID != nil {
			assignee = *c.AssigneeID
		}
		lines := []string{
			"# " + c.Title + "  (" + c.ID + ")",
			"  board:    " + c.BoardID,
			"  column:   " + c.ColumnID,
			"  assignee: " + assignee,
			"  created:  " + nodeDateToString(timeOf(c.CreatedAt)) + "  by " + c.CreatedBy,
		}
		if len(c.Mentions) > 0 {
			parts := make([]string, len(c.Mentions))
			for i, m := range c.Mentions {
				parts[i] = "@" + m
			}
			lines = append(lines, "  mentions: "+strings.Join(parts, " "))
		}
		if c.Description != nil {
			lines = append(lines, "", *c.Description)
		}
		if len(comments) > 0 {
			lines = append(lines, "", fmt.Sprintf("--- %d comment(s) ---", len(comments)))
			for _, cm := range comments {
				lines = append(lines, "  "+isoMilli(timeOf(cm.CreatedAt))+"  "+cm.AuthorID+": "+cm.Body)
			}
		}
		return cliOK(strings.Join(lines, "\n"))
	case "add", "create":
		return s.cliCardAdd(ctx, parsed, me, companyID)
	case "move":
		return s.cliCardMove(ctx, parsed, me, companyID, resolveCardBoard)
	case "assign":
		return s.cliCardAssign(ctx, parsed, me, companyID, resolveCardBoard)
	case "claim":
		return s.cliCardClaim(ctx, parsed, me, companyID, resolveCardBoard)
	case "rename", "edit":
		return s.cliCardRename(ctx, parsed, op, me, companyID, resolveCardBoard)
	case "comment":
		return s.cliCardComment(ctx, parsed, me, companyID, resolveCardBoard)
	case "delete-comment", "rm-comment":
		return s.cliCardDeleteComment(ctx, parsed, op, me, companyID, resolveCardBoard)
	case "delete", "rm":
		return s.cliCardDelete(ctx, parsed, me, companyID, resolveCardBoard)
	}
	return cliErr("usage: card <ls|show|add|move|assign|rename|comment|delete-comment|delete> [...]")
}

func timeOf(t cliISOTime) time.Time { return time.Time(t) }

func mentionsNote(mentions []string) string {
	if len(mentions) == 0 {
		return ""
	}
	parts := make([]string, len(mentions))
	for i, m := range mentions {
		parts[i] = "@" + m
	}
	return "  · mentions: " + strings.Join(parts, " ")
}

func (s *Service) cliCardAdd(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr(`usage: card add <board_id> "<title>" --column <col_id> [--description "..."] [--assign <id>]`)
	}
	boardID := parsed.positional[1]
	title := strings.TrimSpace(strings.Join(positionalFrom(parsed, 2), " "))
	if title == "" {
		if t, ok := parsed.flagStr("title"); ok {
			title = t
		}
	}
	if title == "" {
		return cliErr(`usage: card add <board_id> "<title>" --column <col_id> [--description "..."] [--assign <id>]`)
	}
	columnID := strings.TrimSpace(parsed.flagStrOr("column", ""))
	if columnID == "" {
		columnID = strings.TrimSpace(parsed.flagStrOr("col", ""))
	}
	if columnID == "" {
		return cliErr("--column <col_id> required (run `cumora kanban columns <board_id>` to list)")
	}
	if !s.boardOwnedBy(ctx, boardID, companyID) {
		return cliErr("board " + boardID + " not found")
	}
	var one int
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM board_columns WHERE id = $1 AND board_id = $2 LIMIT 1`, columnID, boardID).Scan(&one)
	if err == sql.ErrNoRows {
		return cliErr("column " + columnID + " not in board " + boardID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	var description any
	if d, ok := parsed.flagStr("description"); ok {
		description = utf16Slice(cliUnescapeChat(d), 8000)
	}
	var assignee any
	if a, ok := parsed.flagStr("assign"); ok {
		assignee = strings.TrimSpace(a)
	}
	var maxPos sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT MAX(position) AS max FROM board_cards WHERE column_id = $1`, columnID).Scan(&maxPos); err != nil {
		return cliErrThrow(err)
	}
	position := maxPos.Int64 + 1000
	descText := ""
	if d, ok := description.(string); ok {
		descText = d
	}
	mentions := s.cliParseMentions(ctx, companyID, title+"\n"+descText)
	id := "card-" + uuidHex()[:12]
	mentionsJSON, _ := jsonMarshalStrings(mentions)
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO board_cards
		   (id, board_id, column_id, title, description, position, assignee_id, mentions, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)`,
		id, boardID, columnID, utf16Slice(title, 200), description, position, assignee, mentionsJSON, me); err != nil {
		return cliErrThrow(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE boards SET updated_at = NOW() WHERE id = $1`, boardID); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "card.created", boardID, &id, &columnID, nil, mentions, me)
	s.wakeMentionedAgentsCli(companyID, mentions, me)
	if a, ok := assignee.(string); ok && a != "" && a != me {
		s.wakeMentionedAgentsCli(companyID, []string{a}, me)
	}
	effect := cliSideEffect{
		"event":         "kanban.card_created",
		"command":       "card add",
		"boardId":       boardID,
		"cardId":        id,
		"columnId":      columnID,
		"actorId":       me,
		"companyId":     companyID,
		"assigneeId":    assignee,
		"mentions":      mentions,
		"title":         title,
		"visibleToUser": true,
	}
	return cliOK("added card "+id+": "+title+mentionsNote(mentions), effect)
}

type cardBoardResolver = func(string) (*struct{ boardID, columnID string }, error)

func (s *Service) cliCardMove(ctx context.Context, parsed cliParsed, me, companyID string, resolveCardBoard cardBoardResolver) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: card move <card_id> --to <column_id>")
	}
	cardID := parsed.positional[1]
	toCol := strings.TrimSpace(parsed.flagStrOr("to", ""))
	if toCol == "" {
		toCol = strings.TrimSpace(parsed.flagStrOr("column", ""))
	}
	if toCol == "" {
		toCol = strings.TrimSpace(parsed.flagStrOr("col", ""))
	}
	if toCol == "" {
		return cliErr("usage: card move <card_id> --to <column_id>")
	}
	home, err := resolveCardBoard(cardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if home == nil {
		return cliErr("card " + cardID + " not found")
	}
	var one int
	err = s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM board_columns WHERE id = $1 AND board_id = $2 LIMIT 1`, toCol, home.boardID).Scan(&one)
	if err == sql.ErrNoRows {
		return cliErr("column " + toCol + " not in board " + home.boardID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	var maxPos sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT MAX(position) AS max FROM board_cards WHERE column_id = $1`, toCol).Scan(&maxPos); err != nil {
		return cliErrThrow(err)
	}
	position := maxPos.Int64 + 1000
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE board_cards SET column_id = $1, position = $2, updated_at = NOW() WHERE id = $3`,
		toCol, position, cardID); err != nil {
		return cliErrThrow(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE boards SET updated_at = NOW() WHERE id = $1`, home.boardID); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "card.moved", home.boardID, &cardID, &toCol, nil, nil, me)
	return cliOK("moved card "+cardID+" → "+toCol, cliSideEffect{
		"event":         "kanban.card_moved",
		"command":       "card move",
		"boardId":       home.boardID,
		"cardId":        cardID,
		"fromColumnId":  home.columnID,
		"columnId":      toCol,
		"actorId":       me,
		"companyId":     companyID,
		"visibleToUser": true,
	})
}

func (s *Service) cliCardAssign(ctx context.Context, parsed cliParsed, me, companyID string, resolveCardBoard cardBoardResolver) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: card assign <card_id> <participant_id|null>")
	}
	cardID := parsed.positional[1]
	who := ""
	if len(parsed.positional) > 2 {
		who = parsed.positional[2]
	}
	home, err := resolveCardBoard(cardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if home == nil {
		return cliErr("card " + cardID + " not found")
	}
	var assignee any
	if who != "" && !strings.EqualFold(who, "null") && who != "-" {
		assignee = strings.TrimSpace(who)
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE board_cards SET assignee_id = $1, updated_at = NOW() WHERE id = $2`, assignee, cardID); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "card.updated", home.boardID, &cardID, nil, nil, nil, me)
	if a, ok := assignee.(string); ok && a != "" && a != me {
		s.wakeMentionedAgentsCli(companyID, []string{a}, me)
	}
	if a, ok := assignee.(string); ok && a != "" {
		return cliOK("assigned card "+cardID+" → @"+a, cliSideEffect{
			"event":         "kanban.card_assigned",
			"command":       "card assign",
			"boardId":       home.boardID,
			"cardId":        cardID,
			"actorId":       me,
			"companyId":     companyID,
			"assigneeId":    assignee,
			"visibleToUser": true,
		})
	}
	return cliOK("unassigned card "+cardID, cliSideEffect{
		"event":         "kanban.card_assigned",
		"command":       "card assign",
		"boardId":       home.boardID,
		"cardId":        cardID,
		"actorId":       me,
		"companyId":     companyID,
		"assigneeId":    nil,
		"visibleToUser": true,
	})
}

func (s *Service) cliCardClaim(ctx context.Context, parsed cliParsed, me, companyID string, resolveCardBoard cardBoardResolver) cliResult {
	// 原子独占声明(防发散协作的第一原语):只有无人认领、已是自己、
	// 或声明已陈旧(≥20 分钟未动)才能赢;WHERE 守卫即闸门,rowCount
	// 是唯一真相 —— 两个 agent 竞同一张卡不可能双赢。
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: card claim <card_id>")
	}
	cardID := parsed.positional[1]
	home, err := resolveCardBoard(cardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if home == nil {
		return cliErr("card " + cardID + " not found")
	}
	var claimed string
	err = s.DB.QueryRowContext(ctx,
		`UPDATE board_cards SET assignee_id = $1, updated_at = NOW()
		   WHERE id = $2
		     AND (assignee_id IS NULL OR assignee_id = $1
		          OR updated_at < NOW() - INTERVAL '20 minutes')
		   RETURNING id`, me, cardID).Scan(&claimed)
	if err == sql.ErrNoRows {
		var holder sql.NullString
		_ = s.DB.QueryRowContext(ctx,
			`SELECT assignee_id FROM board_cards WHERE id = $1 LIMIT 1`, cardID).Scan(&holder)
		h := "?"
		if holder.Valid {
			h = holder.String
		}
		return cliErr("claim failed: card " + cardID + " is already being worked by @" + h + " — move on to another task")
	}
	if err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "card.updated", home.boardID, &cardID, nil, nil, nil, me)
	return cliOK("claimed card "+cardID+" — it's yours. Do the work, post progress with `card comment`, move it with `card move`, and release with `card assign "+cardID+" null` (or move to a done column) when finished.", cliSideEffect{
		"event":         "kanban.card_claimed",
		"command":       "card claim",
		"boardId":       home.boardID,
		"cardId":        cardID,
		"actorId":       me,
		"companyId":     companyID,
		"assigneeId":    me,
		"visibleToUser": true,
	})
}

func (s *Service) cliCardRename(ctx context.Context, parsed cliParsed, op, me, companyID string, resolveCardBoard cardBoardResolver) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr(`usage: card rename <card_id> --title "..." [--description "..."]`)
	}
	cardID := parsed.positional[1]
	home, err := resolveCardBoard(cardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if home == nil {
		return cliErr("card " + cardID + " not found")
	}
	var curTitle string
	var curDesc sql.NullString
	if err := s.DB.QueryRowContext(ctx,
		`SELECT title, description FROM board_cards WHERE id = $1`, cardID).Scan(&curTitle, &curDesc); err != nil {
		return cliErrThrow(err)
	}
	nextTitle := curTitle
	var nextDesc any
	if curDesc.Valid {
		nextDesc = curDesc.String
	}
	var sets []string
	var params []any
	if t, ok := parsed.flagStr("title"); ok {
		nextTitle = utf16Slice(cliUnescapeChat(t), 200)
		params = append(params, nextTitle)
		sets = append(sets, fmt.Sprintf("title = $%d", len(params)))
	}
	if d, ok := parsed.flagStr("description"); ok {
		v := utf16Slice(cliUnescapeChat(d), 8000)
		if v == "" {
			nextDesc = nil
		} else {
			nextDesc = v
		}
		params = append(params, nextDesc)
		sets = append(sets, fmt.Sprintf("description = $%d", len(params)))
	}
	if len(sets) == 0 {
		return cliErr("nothing to update — pass --title or --description")
	}
	nextDescText := ""
	if v, ok := nextDesc.(string); ok {
		nextDescText = v
	}
	mentions := s.cliParseMentions(ctx, companyID, nextTitle+"\n"+nextDescText)
	mentionsJSON, _ := jsonMarshalStrings(mentions)
	params = append(params, mentionsJSON)
	sets = append(sets, fmt.Sprintf("mentions = $%d::jsonb", len(params)))
	params = append(params, cardID)
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE board_cards SET `+strings.Join(sets, ", ")+`, updated_at = NOW() WHERE id = $`+fmt.Sprint(len(params)),
		params...); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "card.updated", home.boardID, &cardID, nil, nil, mentions, me)
	s.wakeMentionedAgentsCli(companyID, mentions, me)
	command := "card rename"
	if op != "rename" {
		command = "card edit"
	}
	return cliOK("updated card "+cardID+mentionsNote(mentions), cliSideEffect{
		"event":         "kanban.card_updated",
		"command":       command,
		"boardId":       home.boardID,
		"cardId":        cardID,
		"actorId":       me,
		"companyId":     companyID,
		"mentions":      mentions,
		"title":         nextTitle,
		"visibleToUser": true,
	})
}

func (s *Service) cliCardComment(ctx context.Context, parsed cliParsed, me, companyID string, resolveCardBoard cardBoardResolver) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr(`usage: card comment <card_id> "<body>"`)
	}
	cardID := parsed.positional[1]
	body := strings.TrimSpace(strings.Join(positionalFrom(parsed, 2), " "))
	if body == "" {
		if b, ok := parsed.flagStr("body"); ok {
			body = cliUnescapeChat(b)
		}
	}
	if body == "" {
		return cliErr(`usage: card comment <card_id> "<body>"`)
	}
	home, err := resolveCardBoard(cardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if home == nil {
		return cliErr("card " + cardID + " not found")
	}
	mentions := s.cliParseMentions(ctx, companyID, body)
	id := "cmt-" + uuidHex()[:12]
	mentionsJSON, _ := jsonMarshalStrings(mentions)
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO board_card_comments (id, card_id, author_id, body, mentions)
		 VALUES ($1, $2, $3, $4, $5::jsonb)`,
		id, cardID, me, utf16Slice(body, 8000), mentionsJSON); err != nil {
		return cliErrThrow(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE board_cards SET updated_at = NOW() WHERE id = $1`, cardID); err != nil {
		return cliErrThrow(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE boards SET updated_at = NOW() WHERE id = $1`, home.boardID); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "comment.created", home.boardID, &cardID, nil, &id, mentions, me)
	s.wakeMentionedAgentsCli(companyID, mentions, me)
	return cliOK("commented on "+cardID+mentionsNote(mentions), cliSideEffect{
		"event":         "kanban.comment_created",
		"command":       "card comment",
		"boardId":       home.boardID,
		"cardId":        cardID,
		"commentId":     id,
		"actorId":       me,
		"companyId":     companyID,
		"mentions":      mentions,
		"visibleToUser": true,
	})
}

func (s *Service) cliCardDeleteComment(ctx context.Context, parsed cliParsed, op, me, companyID string, resolveCardBoard cardBoardResolver) cliResult {
	if len(parsed.positional) < 3 || parsed.positional[1] == "" || parsed.positional[2] == "" {
		return cliErr(`usage: card ` + op + ` <card_id> <comment_id>`)
	}
	cardID, commentID := parsed.positional[1], parsed.positional[2]
	home, err := resolveCardBoard(cardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if home == nil {
		return cliErr("card " + cardID + " not found")
	}
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM board_card_comments WHERE id = $1 AND card_id = $2 AND author_id = $3`,
		commentID, cardID, me)
	if err != nil {
		return cliErrThrow(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cliErr("comment " + commentID + " not found or not authored by " + me)
	}
	s.publishBoardCli(companyID, "comment.deleted", home.boardID, &cardID, nil, &commentID, nil, me)
	return cliOK("deleted comment "+commentID, cliSideEffect{
		"event":         "kanban.comment_deleted",
		"command":       "card " + op,
		"boardId":       home.boardID,
		"cardId":        cardID,
		"commentId":     commentID,
		"actorId":       me,
		"companyId":     companyID,
		"visibleToUser": true,
	})
}

func (s *Service) cliCardDelete(ctx context.Context, parsed cliParsed, me, companyID string, resolveCardBoard cardBoardResolver) cliResult {
	if len(parsed.positional) < 2 || parsed.positional[1] == "" {
		return cliErr("usage: card delete <card_id>")
	}
	cardID := parsed.positional[1]
	home, err := resolveCardBoard(cardID)
	if err != nil {
		return cliErrThrow(err)
	}
	if home == nil {
		return cliErr("card " + cardID + " not found")
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM board_cards WHERE id = $1`, cardID); err != nil {
		return cliErrThrow(err)
	}
	s.publishBoardCli(companyID, "card.deleted", home.boardID, &cardID, nil, nil, nil, me)
	return cliOK("deleted card "+cardID, cliSideEffect{
		"event":         "kanban.card_deleted",
		"command":       "card delete",
		"boardId":       home.boardID,
		"cardId":        cardID,
		"actorId":       me,
		"companyId":     companyID,
		"visibleToUser": true,
	})
}
