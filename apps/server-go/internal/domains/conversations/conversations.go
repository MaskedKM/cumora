// domains/conversations —— 会话域(#53):列表(含 lastMessage/unreadCount/
// muted 的完整 SQL 平价)、建群、topic/title/pin/mute/members/leave/
// typing/read;direct 开 DM;消息列表(分页)/发送(202);回复线程。
// WS 广播经 Redis CH_MESSAGE_NEW publish(由 #53 的 ws 票接管消费侧)。
package conversations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/conversations", list(db))
	mux.HandleFunc("POST /api/conversations", createGroup(db))
	mux.HandleFunc("POST /api/conversations/direct", openDirect(db))
	mux.HandleFunc("GET /api/conversations/{id}/messages", messages(db))
	mux.HandleFunc("POST /api/conversations/{id}/messages", sendMessage(db))
	mux.HandleFunc("POST /api/conversations/{id}/topic", setTopic(db))
	mux.HandleFunc("POST /api/conversations/{id}/title", setTitle(db))
	mux.HandleFunc("POST /api/conversations/{id}/pin", setPin(db))
	mux.HandleFunc("POST /api/conversations/{id}/mute", setMute(db))
	mux.HandleFunc("POST /api/conversations/{id}/members", addMember(db))
	mux.HandleFunc("POST /api/conversations/{id}/leave", leave(db))
	mux.HandleFunc("POST /api/conversations/{id}/typing", typing(db))
	mux.HandleFunc("POST /api/conversations/{id}/read", markRead(db))
	mux.HandleFunc("GET /api/conversations/{id}/messages/{rootId}/replies", replies(db))
}

// memberGate 校验会话存在+成员资格,返回成员列表;非成员 403。
func memberGate(w http.ResponseWriter, r *http.Request, db *sql.DB, uid, companyID, convID string) ([]string, bool) {
	var membersJSON string
	err := db.QueryRowContext(r.Context(),
		`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2`, convID, companyID).Scan(&membersJSON)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "conversation not found")
		return nil, false
	}
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	for _, m := range members {
		if m == uid {
			return members, true
		}
	}
	httpx.WriteError(w, http.StatusForbidden, "not a member of this conversation")
	return nil, false
}

func requireConv(w http.ResponseWriter, r *http.Request, db *sql.DB) (uid, companyID string, ok bool) {
	uid, ok = httpx.RequireAuth(w, r)
	if !ok {
		return "", "", false
	}
	companyID, ok = httpx.ResolveCompany(w, r, db, uid)
	if !ok {
		return "", "", false
	}
	return uid, companyID, true
}

func setTopic(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		if _, ok := memberGate(w, r, db, uid, companyID, convID); !ok {
			return
		}
		var body struct {
			Topic *string `json:"topic"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var topic any
		if body.Topic != nil {
			topic = *body.Topic
		}
		if _, err := db.ExecContext(r.Context(),
			`UPDATE conversations SET topic = $2, updated_at = NOW() WHERE id = $1`, convID, topic); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "topic": topic})
	}
}

func setTitle(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		if _, ok := memberGate(w, r, db, uid, companyID, convID); !ok {
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.TrimSpace(body.Title) == "" {
			httpx.WriteError(w, http.StatusBadRequest, "title required")
			return
		}
		if _, err := db.ExecContext(r.Context(),
			`UPDATE conversations SET title = $2, updated_at = NOW() WHERE id = $1`, convID, body.Title); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "title": body.Title})
	}
}

func setPin(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		if _, ok := memberGate(w, r, db, uid, companyID, convID); !ok {
			return
		}
		var body struct {
			Pinned *bool `json:"pinned"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pinned := true
		if body.Pinned != nil {
			pinned = *body.Pinned
		} else {
			var cur bool
			_ = db.QueryRowContext(r.Context(), `SELECT pinned FROM conversations WHERE id = $1`, convID).Scan(&cur)
			pinned = !cur
		}
		if _, err := db.ExecContext(r.Context(),
			`UPDATE conversations SET pinned = $2 WHERE id = $1`, convID, pinned); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "pinned": pinned})
	}
}

func setMute(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		if _, ok := memberGate(w, r, db, uid, companyID, convID); !ok {
			return
		}
		var body struct {
			Mute  bool   `json:"mute"`
			Until string `json:"until"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Mute {
			_, _ = db.ExecContext(r.Context(),
				`DELETE FROM conversation_mutes WHERE conversation_id = $1 AND user_id = $2`, convID, uid)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "muted": false, "mutedUntil": nil})
			return
		}
		var until any
		if body.Until != "" {
			until = body.Until
		}
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO conversation_mutes (conversation_id, user_id, muted_until) VALUES ($1, $2, $3::timestamptz)
			ON CONFLICT (conversation_id, user_id) DO UPDATE SET muted_until = $3::timestamptz`,
			convID, uid, until); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "muted": true, "mutedUntil": until})
	}
}

func addMember(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		members, ok := memberGate(w, r, db, uid, companyID, convID)
		if !ok {
			return
		}
		var body struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ID == "" {
			httpx.WriteError(w, http.StatusBadRequest, "id required")
			return
		}
		for _, m := range members {
			if m == body.ID {
				httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "members": members, "alreadyIn": true})
				return
			}
		}
		var exists bool
		_ = db.QueryRowContext(r.Context(),
			`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`, body.ID, companyID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusBadRequest, "unknown participant")
			return
		}
		next := append(members, body.ID)
		nj, _ := json.Marshal(next)
		if _, err := db.ExecContext(r.Context(),
			`UPDATE conversations SET members = $2::jsonb, updated_at = NOW() WHERE id = $1`, convID, nj); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "members": next})
	}
}

func leave(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		members, ok := memberGate(w, r, db, uid, companyID, convID)
		if !ok {
			return
		}
		var next []string
		for _, m := range members {
			if m != uid {
				next = append(next, m)
			}
		}
		nj, _ := json.Marshal(next)
		if _, err := db.ExecContext(r.Context(),
			`UPDATE conversations SET members = $2::jsonb, updated_at = NOW() WHERE id = $1`, convID, nj); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "update failed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "members": next})
	}
}

func typing(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		if _, ok := memberGate(w, r, db, uid, companyID, convID); !ok {
			return
		}
		// WS typing 广播在 ws 域接管;此处 200 即可(与 baseline 的
		// publish-only-when-connected 行为等价:无订阅即丢弃)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func markRead(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		if _, ok := memberGate(w, r, db, uid, companyID, convID); !ok {
			return
		}
		_, _ = db.ExecContext(r.Context(), `
			INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
			VALUES ($1, $2, NOW()) ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`,
			uid, convID)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func replies(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, companyID, ok := requireConv(w, r, db)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		if _, ok := memberGate(w, r, db, uid, companyID, convID); !ok {
			return
		}
		rootID := r.PathValue("rootId")
		rows, err := db.QueryContext(r.Context(), `
			SELECT m.id, m.conversation_id, m.author_id, m.kind, m.body, m.sequence, m.created_at
			  FROM messages m WHERE m.conversation_id = $1 AND m.quoted_message_id = $2
			 ORDER BY m.sequence ASC`, convID, rootID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, convID2, authorID, kind, body string
			var sequence int
			var createdAt sql.NullTime
			if rows.Scan(&id, &convID2, &authorID, &kind, &body, &sequence, &createdAt) == nil {
				out = append(out, map[string]any{
					"id": id, "conversationId": convID2, "authorId": authorID,
					"kind": kind, "body": body, "sequence": sequence, "createdAt": createdAt.Time.UTC(),
				})
			}
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

func list(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		companyID, ok := httpx.ResolveCompany(w, r, db, uid)
		if !ok {
			return
		}
		// 平价 baseline 的列表 SQL(见 router.ts GET /conversations):
		// direct 标题取对方参与者名;muted 由未过期 mute 行推导;
		// lastMessage 取 seq 最新一条(含 email 三字段);unreadCount 排除
		// 自己且晚于 last_read_at。
		rows, err := db.QueryContext(r.Context(), `
			SELECT
				c.id, c.kind,
				CASE WHEN c.kind = 'direct' THEN COALESCE(op.name, c.title) ELSE c.title END AS title,
				c.subtitle, c.topic, c.members::text, c.pinned, c.tag,
				c.pulled_by::text, c.project_id, p.name, p.color,
				c.created_at, c.updated_at,
				(mu.user_id IS NOT NULL AND (mu.muted_until IS NULL OR mu.muted_until > NOW())),
				mu.muted_until,
				(
					SELECT json_build_object(
						'id', m.id, 'authorId', m.author_id, 'kind', m.kind, 'body', m.body,
						'tool', m.tool, 'attachment', m.attachment, 'createdAt', m.created_at,
						'email', (SELECT jsonb_build_object('subject', em.subject, 'direction', em.direction, 'from', em.from_addr)
						            FROM email_messages em WHERE em.message_id = m.id)
					) FROM messages m WHERE m.conversation_id = c.id ORDER BY m.sequence DESC LIMIT 1
				)::text,
				COALESCE((
					SELECT COUNT(*)::int FROM messages m
					 WHERE m.conversation_id = c.id AND m.author_id <> $1
					   AND m.created_at > COALESCE(
					     (SELECT last_read_at FROM conversation_reads WHERE user_id = $1 AND conversation_id = c.id),
					     '1970-01-01T00:00:00Z'::timestamptz)
				), 0)
			FROM conversations c
			LEFT JOIN projects p ON p.id = c.project_id
			LEFT JOIN conversation_mutes mu ON mu.conversation_id = c.id AND mu.user_id = $1
			LEFT JOIN LATERAL (
				SELECT po.name FROM jsonb_array_elements_text(c.members) WITH ORDINALITY AS member(id, ord)
				JOIN participants po ON po.id = member.id AND po.company_id = c.company_id
				WHERE member.id <> $1 ORDER BY member.ord LIMIT 1
			) op ON c.kind = 'direct'
			WHERE c.company_id = $2 AND c.members @> to_jsonb(ARRAY[$1::text])
			ORDER BY c.pinned DESC, c.updated_at DESC`, uid, companyID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, kind, title string
			var subtitle, topic, membersJSON, tag, pulledBy, projectID, projectName, projectColor sql.NullString
			var pinned bool
			var createdAt, updatedAt, mutedUntil sql.NullTime
			var muted bool
			var lastMessageJSON sql.NullString
			var unread int
			if err := rows.Scan(&id, &kind, &title, &subtitle, &topic, &membersJSON, &pinned, &tag,
				&pulledBy, &projectID, &projectName, &projectColor, &createdAt, &updatedAt,
				&muted, &mutedUntil, &lastMessageJSON, &unread); err != nil {
				continue
			}
			var members []string
			_ = json.Unmarshal([]byte(membersJSON.String), &members)
			var lastMessage any
			if lastMessageJSON.Valid && lastMessageJSON.String != "" {
				_ = json.Unmarshal([]byte(lastMessageJSON.String), &lastMessage)
			}
			out = append(out, map[string]any{
				"id": id, "kind": kind, "title": title,
				"subtitle": nullOr(subtitle), "topic": nullOr(topic),
				"members": members, "pinned": pinned, "tag": nullOr(tag),
				"pulledBy": jsonOrNull(pulledBy), "projectId": nullOr(projectID),
				"projectName": nullOr(projectName), "projectColor": nullOr(projectColor),
				"createdAt": createdAt.Time.UTC(), "updatedAt": updatedAt.Time.UTC(),
				"muted": muted, "mutedUntil": nullTimeOr(mutedUntil),
				"lastMessage": lastMessage, "unreadCount": unread,
			})
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

func createGroup(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		companyID, ok := httpx.ResolveCompany(w, r, db, uid)
		if !ok {
			return
		}
		var body struct {
			Title     string   `json:"title"`
			Topic     string   `json:"topic"`
			ProjectID string   `json:"projectId"`
			Members   []string `json:"members"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		title := strings.TrimSpace(body.Title)
		if runes := []rune(title); len(runes) > 80 {
			title = string(runes[:80])
		}
		if title == "" {
			httpx.WriteError(w, http.StatusBadRequest, "title required")
			return
		}
		memberSet := map[string]bool{uid: true}
		for _, m := range body.Members {
			if m = strings.TrimSpace(m); m != "" {
				memberSet[m] = true
			}
		}
		members := keys(memberSet)
		if len(members) < 2 {
			httpx.WriteError(w, http.StatusBadRequest, "pick at least one teammate")
			return
		}
		// 校验成员同租户
		valid := map[string]bool{}
		vrows, _ := db.QueryContext(r.Context(),
			`SELECT id FROM participants WHERE id = ANY($1::text[]) AND company_id = $2`,
			arrayLiteral(members), companyID)
		if vrows != nil {
			defer vrows.Close()
			for vrows.Next() {
				var vid string
				_ = vrows.Scan(&vid)
				valid[vid] = true
			}
		}
		var missing []string
		for _, m := range members {
			if !valid[m] {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			httpx.WriteError(w, http.StatusBadRequest, fmt.Sprintf("unknown participant(s): %s", strings.Join(missing, ", ")))
			return
		}
		var projectID any
		if body.ProjectID != "" {
			var exists bool
			_ = db.QueryRowContext(r.Context(),
				`SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`, body.ProjectID, companyID).Scan(&exists)
			if !exists {
				httpx.WriteError(w, http.StatusBadRequest, "unknown project")
				return
			}
			projectID = body.ProjectID
		}
		id := "g-" + authn.NewToken()[:8]
		membersJSON, _ := json.Marshal(members)
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO conversations (id, kind, title, topic, members, pinned, company_id, project_id)
			VALUES ($1, 'group', $2, NULLIF($3,''), $4::jsonb, FALSE, $5, $6)`,
			id, title, body.Topic, membersJSON, companyID, projectID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "insert failed")
			return
		}
		_, _ = db.ExecContext(r.Context(),
			`INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 1)`, id)
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "members": members, "projectId": projectID})
	}
}

func openDirect(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		companyID, ok := httpx.ResolveCompany(w, r, db, uid)
		if !ok {
			return
		}
		var body struct {
			OtherID string `json:"otherId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.OtherID == "" || body.OtherID == uid {
			httpx.WriteError(w, http.StatusBadRequest, "otherId required")
			return
		}
		var exists bool
		_ = db.QueryRowContext(r.Context(),
			`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`, body.OtherID, companyID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusBadRequest, "unknown participant")
			return
		}
		// 已有同成员 DM → 复用(members 严格等价:恰为 [a,b] 或 [b,a])
		membersJSON, _ := json.Marshal([]string{uid, body.OtherID})
		otherJSON, _ := json.Marshal([]string{body.OtherID, uid})
		var existing string
		err := db.QueryRowContext(r.Context(), `
			SELECT id FROM conversations
			 WHERE company_id = $1 AND kind = 'direct' AND (members = $2::jsonb OR members = $3::jsonb)
			 LIMIT 1`, companyID, membersJSON, otherJSON).Scan(&existing)
		if err == nil {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": existing, "created": false})
			return
		}
		id := "d-" + authn.NewToken()[:8]
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO conversations (id, kind, title, members, company_id)
			VALUES ($1, 'direct', '', $2::jsonb, $3)`, id, membersJSON, companyID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "insert failed")
			return
		}
		_, _ = db.ExecContext(r.Context(),
			`INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 1)`, id)
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "created": true})
	}
}

func messages(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		companyID, ok := httpx.ResolveCompany(w, r, db, uid)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		// 会话存在 + 成员资格
		var membersJSON string
		err := db.QueryRowContext(r.Context(),
			`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2`, convID, companyID).Scan(&membersJSON)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "conversation not found")
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
			httpx.WriteError(w, http.StatusForbidden, "not a member of this conversation")
			return
		}
		limit, before := r.URL.Query().Get("limit"), r.URL.Query().Get("before")
		// baseline: 默认最近 50 条,倒序返回
		q := `
			SELECT m.id, m.conversation_id, m.author_id, m.kind, m.body, m.sequence, m.created_at,
			       m.reactions::text, m.tool::text, m.attachment::text
			  FROM messages m WHERE m.conversation_id = $1`
		args := []any{convID}
		if before != "" {
			q += fmt.Sprintf(" AND m.sequence < $%d", len(args)+1)
			args = append(args, before)
		}
		q += " ORDER BY m.sequence DESC"
		n := 50
		if limit != "" {
			fmt.Sscanf(limit, "%d", &n)
			if n <= 0 || n > 200 {
				n = 50
			}
		}
		q += fmt.Sprintf(" LIMIT %d", n)
		rows, err := db.QueryContext(r.Context(), q, args...)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, convID2, authorID, kind, body string
			var sequence int
			var createdAt sql.NullTime
			var reactions, tool, attachment sql.NullString
			if err := rows.Scan(&id, &convID2, &authorID, &kind, &body, &sequence, &createdAt, &reactions, &tool, &attachment); err != nil {
				continue
			}
			msg := map[string]any{
				"id": id, "conversationId": convID2, "authorId": authorID,
				"kind": kind, "body": body, "sequence": sequence,
				"createdAt": createdAt.Time.UTC(),
			}
			if reactions.Valid && reactions.String != "" && reactions.String != "null" {
				var dec []any
				if json.Unmarshal([]byte(reactions.String), &dec) == nil {
					msg["reactions"] = dec
				}
			}
			if tool.Valid && tool.String != "" && tool.String != "null" {
				var dec any
				if json.Unmarshal([]byte(tool.String), &dec) == nil {
					msg["tool"] = dec
				}
			}
			if attachment.Valid && attachment.String != "" && attachment.String != "null" {
				var dec any
				if json.Unmarshal([]byte(attachment.String), &dec) == nil {
					msg["attachment"] = dec
				}
			}
			out = append(out, msg)
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

func sendMessage(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		companyID, ok := httpx.ResolveCompany(w, r, db, uid)
		if !ok {
			return
		}
		convID := r.PathValue("id")
		var body struct {
			Body            string          `json:"body"`
			Attachment      json.RawMessage `json:"attachment"`
			QuotedMessageID string          `json:"quotedMessageId"`
			ClientID        string          `json:"clientId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.TrimSpace(body.Body) == "" && len(body.Attachment) == 0 {
			httpx.WriteError(w, http.StatusBadRequest, "body required")
			return
		}
		// 会话存在 + 成员资格(与 GET 同门)
		var membersJSON string
		err := db.QueryRowContext(r.Context(),
			`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2`, convID, companyID).Scan(&membersJSON)
		if err != nil {
			httpx.WriteError(w, http.StatusNotFound, "conversation not found")
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
			httpx.WriteError(w, http.StatusForbidden, "not a member of this conversation")
			return
		}
		// 原子取序(UPDATE RETURNING,与 baseline UPsert 等价语义)
		var sequence int
		if err := db.QueryRowContext(r.Context(), `
			UPDATE conversation_counters SET next_sequence = next_sequence + 1
			 WHERE conversation_id = $1 RETURNING next_sequence - 1`, convID).Scan(&sequence); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "sequence failed")
			return
		}
		id := "m-" + authn.NewToken()[:12]
		var quoted any
		if body.QuotedMessageID != "" {
			quoted = body.QuotedMessageID
		}
		if _, err := db.ExecContext(r.Context(), `
			INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, quoted_message_id, client_id)
			VALUES ($1, $2, $3, 'text', $4, $5, $6, NULLIF($7,''))`,
			id, convID, uid, body.Body, sequence, quoted, body.ClientID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "insert failed")
			return
		}
		_, _ = db.ExecContext(r.Context(), `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, convID)
		httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"id": id, "sequence": sequence})
	}
}

/* helpers */

func nullOr(ns sql.NullString) any {
	if ns.Valid {
		return ns.String
	}
	return nil
}

func jsonOrNull(ns sql.NullString) any {
	if ns.Valid && ns.String != "" {
		var v any
		if json.Unmarshal([]byte(ns.String), &v) == nil {
			return v
		}
	}
	return nil
}

func nullTimeOr(nt sql.NullTime) any {
	if nt.Valid {
		return nt.Time.UTC()
	}
	return nil
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

// arrayLiteral 生成 pg ANY() 需要的 text[] 字面量。
func arrayLiteral(items []string) string {
	parts := make([]string, len(items))
	for i, s := range items {
		b, _ := json.Marshal(s)
		parts[i] = string(b)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var _ = context.Background
