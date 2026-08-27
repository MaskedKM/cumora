// domains/search —— 全局搜索(#68 补齐):GET /api/search 四桶
// (participants/rooms/groups/messages),SQL 逐字对齐 router.ts 4232–4355。
package search

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func Mount(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("GET /api/search", handler(db))
}

func handler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, ok := httpx.RequireAuth(w, r)
		if !ok {
			return
		}
		tenant, ok := httpx.ResolveCompany(w, r, db, uid)
		if !ok {
			return
		}
		raw := strings.TrimSpace(r.URL.Query().Get("q"))
		if raw == "" {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"participants": []any{}, "rooms": []any{}, "groups": []any{}, "messages": []any{},
			})
			return
		}
		// TS raw.length 是 UTF-16 码元长度——>200 判定按码元。
		if utf16Len(raw) > 200 {
			httpx.WriteError(w, http.StatusBadRequest, "query too long (max 200 chars)")
			return
		}
		esc := escapeLike(raw)
		contains := "%" + esc + "%"
		exact := esc
		prefix := esc + "%"

		type row = map[string]any
		participants := []row{}
		rows, err := db.QueryContext(r.Context(), `
			SELECT id, kind, name, role, initial, avatar_bg, avatar_url, status, bio
			  FROM participants
			 WHERE company_id = $1
			   AND departed_at IS NULL
			   AND (name ILIKE $2 ESCAPE '\' OR role ILIKE $2 ESCAPE '\' OR id ILIKE $2 ESCAPE '\')
			 ORDER BY
			   CASE WHEN lower(name) = lower($3) THEN 0
			        WHEN name ILIKE $4 ESCAPE '\' THEN 1
			        ELSE 2 END,
			   CASE kind WHEN 'agent' THEN 0 ELSE 1 END,
			   name
			 LIMIT 8`, tenant, contains, exact, prefix)
		if err == nil {
			for rows.Next() {
				var id, kind, name, initial, avatarBg, status string
				var role, avatarUrl, bio sql.NullString
				if rows.Scan(&id, &kind, &name, &role, &initial, &avatarBg, &avatarUrl, &status, &bio) == nil {
					participants = append(participants, row{
						"id": id, "kind": kind, "name": name, "role": nullStr(role),
						"initial": initial, "avatarBg": avatarBg, "avatarUrl": nullStr(avatarUrl),
						"status": status, "bio": nullStr(bio),
					})
				}
			}
			rows.Close()
		}

		rooms := []row{}
		rows, err = db.QueryContext(r.Context(), `
			WITH my_rooms AS (
			   SELECT c.id, c.kind,
			          CASE
			            WHEN c.kind = 'direct' THEN COALESCE(other_participant.name, c.title)
			            ELSE c.title
			          END AS title,
			          c.members, p.name AS project_name, c.updated_at
			     FROM conversations c
			     LEFT JOIN projects p ON p.id = c.project_id
			     LEFT JOIN LATERAL (
			       SELECT p_other.name
			         FROM jsonb_array_elements_text(c.members) WITH ORDINALITY AS member(id, ord)
			         JOIN participants p_other
			           ON p_other.id = member.id
			          AND p_other.company_id = c.company_id
			        WHERE member.id <> $2
			        ORDER BY member.ord
			        LIMIT 1
			     ) other_participant ON c.kind = 'direct'
			    WHERE c.company_id = $1
			      AND c.kind IN ('direct', 'whisper')
			      AND c.members @> to_jsonb(ARRAY[$2::text])
			 )
			 SELECT r.id, r.kind, r.title, r.members, r.project_name
			   FROM my_rooms r
			  WHERE r.title ILIKE $3 ESCAPE '\'
			     OR EXISTS (
			          SELECT 1 FROM participants p
			           WHERE p.company_id = $1
			             AND p.name ILIKE $3 ESCAPE '\'
			             AND p.id <> $2
			             AND r.members @> to_jsonb(ARRAY[p.id::text])
			        )
			  ORDER BY
			    CASE WHEN lower(r.title) = lower($4) THEN 0
			         WHEN r.title ILIKE $5 ESCAPE '\' THEN 1
			         ELSE 2 END,
			    r.updated_at DESC
			  LIMIT 8`, tenant, uid, contains, exact, prefix)
		if err == nil {
			rooms = scanConvoRows(rows)
		}

		groups := []row{}
		rows, err = db.QueryContext(r.Context(), `
			SELECT c.id, c.kind, c.title, c.members, p.name AS project_name
			  FROM conversations c
			  LEFT JOIN projects p ON p.id = c.project_id
			 WHERE c.company_id = $1
			   AND c.kind = 'group'
			   AND c.members @> to_jsonb(ARRAY[$2::text])
			   AND (c.title ILIKE $3 ESCAPE '\' OR (c.topic IS NOT NULL AND c.topic ILIKE $3 ESCAPE '\'))
			 ORDER BY
			   CASE WHEN lower(c.title) = lower($4) THEN 0
			        WHEN c.title ILIKE $5 ESCAPE '\' THEN 1
			        ELSE 2 END,
			   c.updated_at DESC
			 LIMIT 8`, tenant, uid, contains, exact, prefix)
		if err == nil {
			groups = scanConvoRows(rows)
		}

		messages := []row{}
		rows, err = db.QueryContext(r.Context(), `
			SELECT m.id, m.conversation_id,
			       CASE
			         WHEN c.kind = 'direct' THEN COALESCE(other_participant.name, c.title)
			         ELSE c.title
			       END, c.kind, m.author_id, p.name, m.body, m.created_at
			  FROM messages m
			  JOIN conversations c ON c.id = m.conversation_id
			  LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = c.company_id
			  LEFT JOIN LATERAL (
			    SELECT p_other.name
			      FROM jsonb_array_elements_text(c.members) WITH ORDINALITY AS member(id, ord)
			      JOIN participants p_other
			        ON p_other.id = member.id
			       AND p_other.company_id = c.company_id
			     WHERE member.id <> $2
			     ORDER BY member.ord
			     LIMIT 1
			  ) other_participant ON c.kind = 'direct'
			 WHERE c.company_id = $1
			   AND c.members @> to_jsonb(ARRAY[$2::text])
			   AND m.kind = 'text'
			   AND m.body ILIKE $3 ESCAPE '\'
			 ORDER BY m.created_at DESC
			 LIMIT 15`, tenant, uid, contains)
		if err == nil {
			for rows.Next() {
				var id, convoID, convoTitle, convoKind, authorID, body string
				var authorName sql.NullString
				var createdAt time.Time
				if rows.Scan(&id, &convoID, &convoTitle, &convoKind, &authorID, &authorName, &body, &createdAt) == nil {
					messages = append(messages, row{
						"id": id, "conversationId": convoID, "conversationTitle": convoTitle,
						"conversationKind": convoKind, "authorId": authorID,
						"authorName": nullStr(authorName), "body": nil,
						"snippet": snippetOf(body, raw), "createdAt": createdAt.UTC(),
					})
				}
			}
			rows.Close()
		}

		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"participants": participants, "rooms": rooms, "groups": groups, "messages": messages,
		})
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func scanConvoRows(rows *sql.Rows) []map[string]any {
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, kind, title string
		var members []byte
		var projectName sql.NullString
		if rows.Scan(&id, &kind, &title, &members, &projectName) == nil {
			var membersAny any
			_ = jsonUnmarshal(members, &membersAny)
			out = append(out, map[string]any{
				"id": id, "kind": kind, "title": title, "members": membersAny,
				"projectName": nullStr(projectName),
			})
		}
	}
	return out
}

func nullStr(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

// escapeLike:TS 的 /[\\%_]/g → '\\'+m(LIKE 元字符加反斜杠转义)。
func escapeLike(raw string) string {
	var b strings.Builder
	for _, c := range raw {
		switch c {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// snippetOf:±窗口命中摘录(TS 40 前/80 后 + 省略号;indexOf 大小写不敏感)。
func snippetOf(body, needleRaw string) string {
	needle := strings.ToLower(needleRaw)
	lower := strings.ToLower(body)
	idx := strings.Index(lower, needle)
	const before, after = 40, 80
	if idx < 0 {
		if utf16Len(body) <= before+after {
			return body
		}
		return utf16SliceStr(body, before+after)
	}
	start := idx - before
	if start < 0 {
		start = 0
	}
	end := idx + len(needle) + after
	if end > len(body) {
		end = len(body)
	}
	prefixStr, suffixStr := "", ""
	if start > 0 {
		prefixStr = "…"
	}
	if end < len(body) {
		suffixStr = "…"
	}
	return prefixStr + body[start:end] + suffixStr
}

func utf16SliceStr(s string, n int) string {
	count := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
}
