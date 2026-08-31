// domains/boards —— 看板域(#54):板 CRUD、列 CRUD、卡 CRUD、评论 CRUD、
// 跨板卡反查。行为对齐 router.ts 4900–5400;事件经 events 包广播
// CH_BOARDS(board.changed 形状);@提及解析对齐 parseMentionTargets。
package boards

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/boards"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// WakeMentioned:看板 @提及/指派唤醒钩子(#82,对齐 router.ts
// wakeMentionedAgents 的 5 个调用点)。nil = no-op(无 Redis 的降级/
// 单测形态);生产由 main.go 注入 runtime.Service.WakeMentionedAgents。
type WakeMentioned func(companyID string, mentions []string, actorID string)

func noopWake(string, []string, string) {}

// Server:boards tag(15 路由)的域实现(#187 机械迁移);Wake 为
// @ 提及唤醒钩子(main 注入,nil 落 noopWake —— 与原 Mount 同语义)。
type Server struct {
	DB   *sql.DB
	Wake WakeMentioned
}

var _ contract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB, wake WakeMentioned) {
	if wake == nil {
		wake = noopWake
	}
	_ = contract.HandlerFromMux(&Server{DB: db, Wake: wake}, mux)
}

/* access gate + mentions + events */

// boardAccess 对齐 requireBoardAccess:跨租户 404 不透明。
func boardAccess(w http.ResponseWriter, r *http.Request, db *sql.DB, boardID string) (uid, companyID string, ok bool) {
	uid, ok = httpx.RequireAuth(w, r)
	if !ok {
		return "", "", false
	}
	companyID, ok = httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return "", "", false
	}
	var owner string
	err := db.QueryRowContext(r.Context(),
		`SELECT company_id FROM boards WHERE id = $1 LIMIT 1`, boardID).Scan(&owner)
	if err != nil || owner != companyID {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return "", "", false
	}
	return uid, companyID, true
}

type mentionTarget struct {
	ID   string
	Name string
}

// parseMentions 对齐 parseMentionTargets:@token 形式匹配参与者 id 或
// 名字(最长优先),未命中回退 @id 正则;返回去重 id 列表。
func parseMentions(ctx context.Context, db *sql.DB, companyID, text string) []string {
	if text == "" {
		return []string{}
	}
	rows, err := db.QueryContext(ctx,
		`SELECT id, name FROM participants WHERE company_id = $1 AND departed_at IS NULL`, companyID)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	var targets []mentionTarget
	for rows.Next() {
		var t mentionTarget
		if rows.Scan(&t.ID, &t.Name) == nil {
			targets = append(targets, t)
		}
	}
	type candidate struct{ id, token string }
	var cands []candidate
	for _, t := range targets {
		if t.ID != "" {
			cands = append(cands, candidate{t.ID, t.ID})
		}
		if n := strings.TrimSpace(t.Name); n != "" {
			cands = append(cands, candidate{t.ID, n})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return len(cands[i].token) > len(cands[j].token) })
	lower := strings.ToLower(text)
	seen := map[string]bool{}
	out := []string{}
	fallback := regexp.MustCompile(`(?i)^@([a-z0-9][a-z0-9_-]{0,63})`)
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		// start boundary:前字符为词字符**或 @** 均不算提及开头(@@id 非提及)
		if i > 0 && (isWordByte(text[i-1]) || text[i-1] == '@') {
			continue
		}
		rest := lower[i+1:]
		var id string
		for _, c := range cands {
			ct := strings.ToLower(c.token)
			if strings.HasPrefix(rest, ct) && !endBoundary(text, i+1+len(c.token)) {
				id = c.id
				break
			}
		}
		if id == "" {
			if m := fallback.FindStringSubmatch(text[i:]); m != nil {
				id = strings.ToLower(m[1])
			}
		}
		if id == "all" {
			continue
		}
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func endBoundary(text string, idx int) bool {
	if idx >= len(text) {
		return false // 字符串终点=boundary OK(boundary 检查返回 false=可接受)
	}
	c := text[idx]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// boardEvent 广播 board.changed(形状对齐 publishBoardEvent → WS fanout)。
func boardEvent(ctx context.Context, companyID, kind, boardID string, extra map[string]any) {
	payload := map[string]any{
		"type": "board.changed", "kind": kind, "boardId": boardID, "companyId": companyID,
	}
	for k, v := range extra {
		if v == nil {
			continue // baseline:undefined 字段 JSON 序列化即省略
		}
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	_ = events.PublishRaw(ctx, "cumora:boards", b)
}

/* handlers */

func (s *Server) ListBoards(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, title, description, created_by, created_at, updated_at
		  FROM boards WHERE company_id = $1 ORDER BY updated_at DESC`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, createdBy string
		var description sql.NullString
		var createdAt, updatedAt sql.NullTime
		if rows.Scan(&id, &title, &description, &createdBy, &createdAt, &updatedAt) == nil {
			out = append(out, map[string]any{
				"id": id, "title": title, "description": nullOr(description),
				"createdBy": createdBy, "createdAt": createdAt.Time.UTC(), "updatedAt": updatedAt.Time.UTC(),
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) CreateBoard(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	// F16:TS create 是 String(x ?? '') 强转,struct 解码丢非串体。
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	var titleRaw, descRaw any
	_ = json.Unmarshal(raw["title"], &titleRaw)
	_ = json.Unmarshal(raw["description"], &descRaw)
	title := strings.TrimSpace(httpx.JSStringOrNullish(titleRaw))
	title = httpx.UTF16Cap(title, 200)
	if title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "title required")
		return
	}
	description := strings.TrimSpace(httpx.JSStringOrNullish(descRaw))
	description = httpx.UTF16Cap(description, 4000)
	boardID := "board-" + authn.NewToken()[:12]
	// 事务豁免(#213):各步失败的 500 文案各异(tx failed/insert failed/
	// columns seed failed/commit failed),WithTx 单一错误通道抹平
	// BeginTx/Commit 区分,响应字节无法等价。
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO boards (id, company_id, title, description, created_by)
		VALUES ($1, $2, $3, NULLIF($4,''), $5)`,
		boardID, companyID, title, description, uid); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	// 自动种 Todo/Doing/Done 三列(baseline 语义)
	for i, col := range []string{"Todo", "Doing", "Done"} {
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, $3, $4)`,
			"col-"+authn.NewToken()[:12], boardID, col, (i+1)*1000); err != nil {
			httpx.WriteInternalError(w, r, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	boardEvent(r.Context(), companyID, "board.created", boardID, map[string]any{"actorId": uid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": boardID})
}

func (s *Server) GetBoard(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := boardAccess(w, r, s.DB, id)
	if !ok {
		return
	}
	boardID := id
	var rowID, title, createdBy string
	var description sql.NullString
	var createdAt, updatedAt sql.NullTime
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT id, title, description, created_by, created_at, updated_at
		  FROM boards WHERE id = $1 LIMIT 1`, boardID).
		Scan(&rowID, &title, &description, &createdBy, &createdAt, &updatedAt)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	colRows, _ := s.DB.QueryContext(r.Context(), `
		SELECT id, title, position, created_at FROM board_columns
		 WHERE board_id = $1 ORDER BY position ASC`, boardID)
	type colRow struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Position  int    `json:"position"`
		CreatedAt string `json:"createdAt"`
	}
	columns := []colRow{}
	if colRows != nil {
		defer colRows.Close()
		for colRows.Next() {
			var c colRow
			var ca sql.NullTime
			if colRows.Scan(&c.ID, &c.Title, &c.Position, &ca) == nil {
				c.CreatedAt = ca.Time.UTC().Format("2006-01-02T15:04:05.000Z")
				columns = append(columns, c)
			}
		}
	}
	cardRows, _ := s.DB.QueryContext(r.Context(), `
		SELECT c.id, c.column_id, c.title, c.description, c.position,
		       c.assignee_id, c.mentions::text, c.created_by, c.created_at, c.updated_at,
		       (SELECT COUNT(*)::int FROM board_card_comments cc WHERE cc.card_id = c.id)
		  FROM board_cards c WHERE c.board_id = $1
		 ORDER BY c.column_id, c.position ASC`, boardID)
	cards := []map[string]any{}
	if cardRows != nil {
		defer cardRows.Close()
		for cardRows.Next() {
			var cid, colID, ctitle, ccreatedBy string
			var cdesc, cassignee, cmentions sql.NullString
			var cpos int
			var cca, ccu sql.NullTime
			var ccount int
			if cardRows.Scan(&cid, &colID, &ctitle, &cdesc, &cpos, &cassignee, &cmentions,
				&ccreatedBy, &cca, &ccu, &ccount) == nil {
				var mentions []string
				_ = json.Unmarshal([]byte(cmentions.String), &mentions)
				if mentions == nil {
					mentions = []string{}
				}
				cards = append(cards, map[string]any{
					"id": cid, "boardId": boardID, "columnId": colID,
					"title": ctitle, "description": nullOr(cdesc),
					"position": cpos, "assigneeId": nullOr(cassignee),
					"mentions": mentions, "commentCount": ccount,
					"createdBy": ccreatedBy, "createdAt": cca.Time.UTC(), "updatedAt": ccu.Time.UTC(),
				})
			}
		}
	}
	_ = companyID
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": rowID, "title": title, "description": nullOr(description),
		"createdBy": createdBy, "createdAt": createdAt.Time.UTC(), "updatedAt": updatedAt.Time.UTC(),
		"columns": columns, "cards": cards,
	})
}

func (s *Server) UpdateBoard(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, id)
	if !ok {
		return
	}
	boardID := id
	var body struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	sets := []string{}
	args := []any{}
	if body.Title != nil {
		t := strings.TrimSpace(*body.Title)
		t = httpx.UTF16Cap(t, 200)
		args = append(args, t)
		sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
	}
	if body.Description != nil {
		d := strings.TrimSpace(*body.Description)
		d = httpx.UTF16Cap(d, 4000)
		var dv any
		if d != "" {
			dv = d
		}
		args = append(args, dv)
		sets = append(sets, fmt.Sprintf("description = $%d", len(args)))
	}
	if len(sets) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	args = append(args, boardID)
	if _, err := s.DB.ExecContext(r.Context(),
		fmt.Sprintf("UPDATE boards SET %s, updated_at = NOW() WHERE id = $%d", strings.Join(sets, ", "), len(args)), args...); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	boardEvent(r.Context(), companyID, "board.updated", boardID, map[string]any{"actorId": uid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) DeleteBoard(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, id)
	if !ok {
		return
	}
	boardID := id
	if _, err := s.DB.ExecContext(r.Context(), `DELETE FROM boards WHERE id = $1`, boardID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	boardEvent(r.Context(), companyID, "board.deleted", boardID, map[string]any{"actorId": uid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func nullOr(ns sql.NullString) any {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func (s *Server) GetBoardCard(w http.ResponseWriter, r *http.Request, id string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	var bid, bTitle string
	var bDesc sql.NullString
	var bCreatedBy string
	var bCa, bUa sql.NullTime
	var cid, colID, cTitle string
	var cDesc, cAssignee sql.NullString
	var cPos int
	var cMentions sql.NullString
	var cCreatedBy string
	var cCa, cCu sql.NullTime
	var cCount int
	var colTitle string
	var colPos int
	var colCa sql.NullTime
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT c.id, c.board_id, c.column_id, c.title, c.description, c.position,
		       c.assignee_id, c.mentions, c.created_by, c.created_at, c.updated_at,
		       b.title, b.description, b.created_by, b.created_at, b.updated_at,
		       col.title, col.position, col.created_at,
		       (SELECT COUNT(*)::int FROM board_card_comments cc WHERE cc.card_id = c.id)
		  FROM board_cards c
		  JOIN boards b ON b.id = c.board_id
		  JOIN board_columns col ON col.id = c.column_id
		 WHERE c.id = $1 AND b.company_id = $2 LIMIT 1`, id, companyID).
		Scan(&cid, &bid, &colID, &cTitle, &cDesc, &cPos, &cAssignee, &cMentions,
			&cCreatedBy, &cCa, &cCu, &bTitle, &bDesc, &bCreatedBy, &bCa, &bUa,
			&colTitle, &colPos, &colCa, &cCount)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	var mentions []string
	_ = json.Unmarshal([]byte(cMentions.String), &mentions)
	if mentions == nil {
		mentions = []string{}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"board": map[string]any{
			"id": bid, "title": bTitle, "description": nullOr(bDesc),
			"createdBy": bCreatedBy, "createdAt": bCa.Time.UTC(), "updatedAt": bUa.Time.UTC(),
		},
		"column": map[string]any{
			"id": colID, "title": colTitle, "position": colPos, "createdAt": colCa.Time.UTC(),
		},
		"card": map[string]any{
			"id": cid, "boardId": bid, "columnId": colID,
			"title": cTitle, "description": nullOr(cDesc),
			"position": cPos, "assigneeId": nullOr(cAssignee),
			"mentions": mentions, "commentCount": cCount,
			"createdBy": cCreatedBy, "createdAt": cCa.Time.UTC(), "updatedAt": cCu.Time.UTC(),
		},
	})
}

func (s *Server) AddBoardColumn(w http.ResponseWriter, r *http.Request, bid string) {
	// 规范/生成 pattern 的通配符名是 {bid}(原手写是 {id},reader 读名
	// 必须跟注册名走 —— #187 批次 4 抓到的名字错位,注入参数即正解)。
	uid, companyID, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID := bid
	// F16:TS addColumn 是 String(x ?? '') 强转。
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	var titleRaw any
	_ = json.Unmarshal(raw["title"], &titleRaw)
	title := strings.TrimSpace(httpx.JSStringOrNullish(titleRaw))
	title = httpx.UTF16Cap(title, 100)
	if title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "title required")
		return
	}
	var maxPos sql.NullFloat64
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT MAX(position) FROM board_columns WHERE board_id = $1`, boardID).Scan(&maxPos)
	pos := 1000.0
	if maxPos.Valid {
		pos = maxPos.Float64 + 1000
	}
	colID := "col-" + authn.NewToken()[:12]
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, $3, $4)`,
		colID, boardID, title, pos); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	boardEvent(r.Context(), companyID, "column.created", boardID, map[string]any{"columnId": colID, "actorId": uid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": colID, "position": pos})
}

func (s *Server) UpdateBoardColumn(w http.ResponseWriter, r *http.Request, bid string, cid string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID, colID := bid, cid
	var body struct {
		Title    *string `json:"title"`
		Position *int    `json:"position"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	sets := []string{}
	args := []any{}
	if body.Title != nil {
		t := strings.TrimSpace(*body.Title)
		t = httpx.UTF16Cap(t, 100)
		args = append(args, t)
		sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
	}
	if body.Position != nil {
		args = append(args, *body.Position)
		sets = append(sets, fmt.Sprintf("position = $%d", len(args)))
	}
	if len(sets) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	args = append(args, colID, boardID)
	res, err := s.DB.ExecContext(r.Context(),
		fmt.Sprintf("UPDATE board_columns SET %s WHERE id = $%d AND board_id = $%d",
			strings.Join(sets, ", "), len(args)-1, len(args)), args...)
	if err != nil || res == nil {
		// res==nil 分支按 database/sql 契约不可达(err==nil ⇒ res 非 nil),
		// 兜底造错防 WriteInternalError 解引用 nil。
		if err == nil {
			err = fmt.Errorf("column update returned nil result")
		}
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	boardEvent(r.Context(), companyID, "column.updated", boardID, map[string]any{"columnId": colID, "actorId": uid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) DeleteBoardColumn(w http.ResponseWriter, r *http.Request, bid string, cid string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID, colID := bid, cid
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM board_columns WHERE id = $1 AND board_id = $2`, colID, boardID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	boardEvent(r.Context(), companyID, "column.deleted", boardID, map[string]any{"columnId": colID, "actorId": uid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) CreateCard(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, id)
	if !ok {
		return
	}
	boardID := id
	// F16:TS createCard 各键强转(title/description/columnId 走
	// String(x ?? ''),assigneeId 走 truthy 门)。
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	keyAny := func(k string) any {
		var a any
		_ = json.Unmarshal(raw[k], &a)
		return a
	}
	titleRaw, descRaw, colRaw, assigneeRaw := keyAny("title"), keyAny("description"), keyAny("columnId"), keyAny("assigneeId")
	title := strings.TrimSpace(httpx.JSStringOrNullish(titleRaw))
	title = httpx.UTF16Cap(title, 200)
	if title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "title required")
		return
	}
	columnID := strings.TrimSpace(httpx.JSStringOrNullish(colRaw))
	if columnID == "" {
		httpx.WriteError(w, http.StatusBadRequest, "columnId required")
		return
	}
	description := strings.TrimSpace(httpx.JSStringOrNullish(descRaw))
	description = httpx.UTF16Cap(description, 8000)
	var colExists bool
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM board_columns WHERE id = $1 AND board_id = $2 LIMIT 1`, columnID, boardID).Scan(&colExists)
	if !colExists {
		httpx.WriteError(w, http.StatusNotFound, "column not found")
		return
	}
	var maxPos sql.NullFloat64
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT MAX(position) FROM board_cards WHERE column_id = $1`, columnID).Scan(&maxPos)
	position := 1000.0
	if maxPos.Valid {
		position = maxPos.Float64 + 1000
	}
	var assignee any
	if httpx.JSTruthy(assigneeRaw) {
		assignee = strings.TrimSpace(httpx.JSToString(assigneeRaw))
	}
	mentions := parseMentions(r.Context(), s.DB, companyID, title+"\n"+description)
	cardID := "card-" + authn.NewToken()[:12]
	mj, _ := json.Marshal(mentions)
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO board_cards (id, board_id, column_id, title, description, position, assignee_id, mentions, created_by)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), $6, $7, $8::jsonb, $9)`,
		cardID, boardID, columnID, title, description, position, assignee, mj, uid); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_, _ = s.DB.ExecContext(r.Context(), `UPDATE boards SET updated_at = NOW() WHERE id = $1`, boardID)
	boardEvent(r.Context(), companyID, "card.created", boardID, map[string]any{
		"cardId": cardID, "columnId": columnID, "mentions": mentions, "actorId": uid,
	})
	s.Wake(companyID, mentions, uid)
	// 指派即提及:assignee_id=someone 时,那个 someone 也该知道——
	// 哪怕正文里没有 @token。
	if a, ok := assignee.(string); ok && a != "" && a != uid {
		s.Wake(companyID, []string{a}, uid)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": cardID, "position": position, "mentions": mentions})
}

func (s *Server) UpdateCard(w http.ResponseWriter, r *http.Request, bid string, cid string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID, cardID := bid, cid
	// 现值载入(决定 moved vs updated + mentions 重解析的合并源)
	var curTitle string
	var curDesc sql.NullString
	var curColID string
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT title, description, column_id FROM board_cards WHERE id = $1 AND board_id = $2 LIMIT 1`,
		cardID, boardID).Scan(&curTitle, &curDesc, &curColID)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	sets := []string{}
	args := []any{}
	nextTitle, nextDesc := curTitle, curDesc.String
	columnChanged := false
	if v, has := raw["title"]; has && string(v) != "null" {
		var t string
		_ = json.Unmarshal(v, &t)
		nextTitle = strings.TrimSpace(t)
		nextTitle = httpx.UTF16Cap(nextTitle, 200)
		args = append(args, nextTitle)
		sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
	}
	if v, has := raw["description"]; has && string(v) != "null" {
		var d string
		_ = json.Unmarshal(v, &d)
		nextDesc = strings.TrimSpace(d)
		nextDesc = httpx.UTF16Cap(nextDesc, 8000)
		args = append(args, nextDesc)
		sets = append(sets, fmt.Sprintf("description = $%d", len(args)))
	}
	if v, has := raw["position"]; has {
		var p float64
		_ = json.Unmarshal(v, &p)
		args = append(args, p)
		sets = append(sets, fmt.Sprintf("position = $%d", len(args)))
	}
	if v, has := raw["assigneeId"]; has {
		var av any
		if string(v) != "null" {
			var a string
			_ = json.Unmarshal(v, &a)
			a = strings.TrimSpace(a)
			if a != "" {
				av = a
			}
		}
		args = append(args, av)
		sets = append(sets, fmt.Sprintf("assignee_id = $%d", len(args)))
	}
	if v, has := raw["columnId"]; has {
		var newCol string
		_ = json.Unmarshal(v, &newCol)
		newCol = strings.TrimSpace(newCol)
		if newCol != curColID {
			var colExists bool
			_ = s.DB.QueryRowContext(r.Context(),
				`SELECT 1 FROM board_columns WHERE id = $1 AND board_id = $2 LIMIT 1`, newCol, boardID).Scan(&colExists)
			if !colExists {
				httpx.WriteError(w, http.StatusNotFound, "column not found")
				return
			}
			args = append(args, newCol)
			sets = append(sets, fmt.Sprintf("column_id = $%d", len(args)))
			columnChanged = true
		}
	}
	proseChanged := raw["title"] != nil || raw["description"] != nil
	var mentions []string
	if proseChanged {
		mentions = parseMentions(r.Context(), s.DB, companyID, nextTitle+"\n"+nextDesc)
		mj, _ := json.Marshal(mentions)
		args = append(args, string(mj))
		sets = append(sets, fmt.Sprintf("mentions = $%d::jsonb", len(args)))
	}
	if len(sets) == 0 {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	args = append(args, cardID, boardID)
	if _, err := s.DB.ExecContext(r.Context(),
		fmt.Sprintf("UPDATE board_cards SET %s, updated_at = NOW() WHERE id = $%d AND board_id = $%d",
			strings.Join(sets, ", "), len(args)-1, len(args)), args...); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_, _ = s.DB.ExecContext(r.Context(), `UPDATE boards SET updated_at = NOW() WHERE id = $1`, boardID)
	kind := "card.updated"
	if columnChanged {
		kind = "card.moved"
	}
	var mentionsOut any
	if proseChanged {
		mentionsOut = mentions
	}
	boardEvent(r.Context(), companyID, kind, boardID, map[string]any{
		"cardId": cardID, "mentions": mentionsOut, "actorId": uid,
	})
	s.Wake(companyID, mentions, uid)
	// 重指派也唤醒新 assignee(键在请求体出现才算变化路径)。
	if v, has := raw["assigneeId"]; has && string(v) != "null" {
		var newAssignee string
		_ = json.Unmarshal(v, &newAssignee)
		newAssignee = strings.TrimSpace(newAssignee)
		if newAssignee != "" && newAssignee != uid {
			s.Wake(companyID, []string{newAssignee}, uid)
		}
	}
	resp := map[string]any{"ok": true}
	if mentionsOut != nil {
		resp["mentions"] = mentionsOut
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *Server) DeleteCard(w http.ResponseWriter, r *http.Request, bid string, cid string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID, cardID := bid, cid
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM board_cards WHERE id = $1 AND board_id = $2`, cardID, boardID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	boardEvent(r.Context(), companyID, "card.deleted", boardID, map[string]any{"cardId": cardID, "actorId": uid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) ListCardComments(w http.ResponseWriter, r *http.Request, bid string, cid string) {
	_, _, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID, cardID := bid, cid
	var cardExists bool
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM board_cards WHERE id = $1 AND board_id = $2 LIMIT 1`, cardID, boardID).Scan(&cardExists)
	if !cardExists {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, author_id, body, mentions::text, created_at
		  FROM board_card_comments WHERE card_id = $1 ORDER BY created_at ASC`, cardID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, authorID, body string
		var mentions sql.NullString
		var createdAt sql.NullTime
		if rows.Scan(&id, &authorID, &body, &mentions, &createdAt) == nil {
			var m []string
			_ = json.Unmarshal([]byte(mentions.String), &m)
			if m == nil {
				m = []string{}
			}
			out = append(out, map[string]any{
				"id": id, "authorId": authorID, "body": body,
				"mentions": m, "createdAt": createdAt.Time.UTC(),
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) AddCardComment(w http.ResponseWriter, r *http.Request, bid string, cid string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID, cardID := bid, cid
	var body struct {
		Body string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	text := strings.TrimSpace(body.Body)
	text = httpx.UTF16Cap(text, 8000)
	if text == "" {
		httpx.WriteError(w, http.StatusBadRequest, "body required")
		return
	}
	var cardExists bool
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM board_cards WHERE id = $1 AND board_id = $2 LIMIT 1`, cardID, boardID).Scan(&cardExists)
	if !cardExists {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	mentions := parseMentions(r.Context(), s.DB, companyID, text)
	commentID := "cmt-" + authn.NewToken()[:12]
	mj, _ := json.Marshal(mentions)
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO board_card_comments (id, card_id, author_id, body, mentions)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, commentID, cardID, uid, text, mj); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_, _ = s.DB.ExecContext(r.Context(), `UPDATE board_cards SET updated_at = NOW() WHERE id = $1`, cardID)
	_, _ = s.DB.ExecContext(r.Context(), `UPDATE boards SET updated_at = NOW() WHERE id = $1`, boardID)
	boardEvent(r.Context(), companyID, "comment.created", boardID, map[string]any{
		"cardId": cardID, "commentId": commentID, "mentions": mentions, "actorId": uid,
	})
	s.Wake(companyID, mentions, uid)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": commentID, "mentions": mentions})
}

func (s *Server) DeleteCardComment(w http.ResponseWriter, r *http.Request, bid string, cid string, mid string) {
	uid, companyID, ok := boardAccess(w, r, s.DB, bid)
	if !ok {
		return
	}
	boardID, cardID, commentID := bid, cid, mid
	// baseline:WHERE author_id = me 的单条 DELETE;行不存在/非作者
	// 一律 404(存在性不透明)。
	uidNow, _ := httpx.UserID(r)
	res, err := s.DB.ExecContext(r.Context(),
		`DELETE FROM board_card_comments WHERE id = $1 AND card_id = $2 AND author_id = $3`,
		commentID, cardID, uidNow)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	boardEvent(r.Context(), companyID, "comment.deleted", boardID, map[string]any{
		"cardId": cardID, "commentId": commentID, "actorId": uid,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}
