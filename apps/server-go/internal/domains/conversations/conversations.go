// domains/conversations —— 会话域(#53):列表(含 lastMessage/unreadCount/
// muted 的完整 SQL 平价)、建群、topic/title/pin/mute/members/leave/
// typing/read;direct 开 DM;消息列表(分页)/发送(202);回复线程。
// WS 广播经 Redis CH_MESSAGE_NEW publish(由 #53 的 ws 票接管消费侧)。
// #187 批次 7:conversations tag(21 路由)走 ServerInterface;polls 3
// 条与 convene 3 条跨包路由经导出函数委托(pollsdomain/invitationsdomain)。
package conversations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/conversations"
	dbpkg "github.com/MaskedKM/cumora/apps/server-go/internal/db"
	invitationsdomain "github.com/MaskedKM/cumora/apps/server-go/internal/domains/invitations"
	pollsdomain "github.com/MaskedKM/cumora/apps/server-go/internal/domains/polls"
	emailpkg "github.com/MaskedKM/cumora/apps/server-go/internal/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/push"
)

// pushFanoutBudget:#136 推送扇出总预算——收件人/标题/作者查询 + 单轮
// 并发推送(单端点 15s 客户端超时)的硬上限,挂起端点不再无限连坐
// goroutine 与池连接。
const pushFanoutBudget = 60 * time.Second

// Server:conversations tag(21 路由)的域实现。15 方法体自原闭包工厂
// 逐字上移(#187 批次 7);polls/convene 六条跨包路由一行委托。
type Server struct{ DB *sql.DB }

var _ contract.ServerInterface = (*Server)(nil)

func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

/* ───────── 跨包委托(polls 3 + convene 3) ───────── */

func (s *Server) CreatePoll(w http.ResponseWriter, r *http.Request) {
	pollsdomain.CreatePoll(s.DB, w, r)
}

func (s *Server) CastPollVote(w http.ResponseWriter, r *http.Request, messageId string) {
	pollsdomain.CastVote(s.DB, w, r, messageId)
}

func (s *Server) ClosePoll(w http.ResponseWriter, r *http.Request, messageId string) {
	pollsdomain.ClosePoll(s.DB, w, r, messageId)
}

func (s *Server) StartConvene(w http.ResponseWriter, r *http.Request, id string) {
	invitationsdomain.StartConvene(s.DB, w, r, id)
}

func (s *Server) GetActiveConvene(w http.ResponseWriter, r *http.Request, id string) {
	invitationsdomain.ActiveConvene(s.DB, w, r, id)
}

func (s *Server) GetConveneTranscript(w http.ResponseWriter, r *http.Request, sessionId string) {
	invitationsdomain.ConveneTranscript(s.DB, w, r, sessionId)
}

// memberCheck:会话存在+成员资格。非成员返回 404(baseline 的存在性
// 不透明策略:探测者分不出"不存在"和"不在内");路由级差异消息由调用方覆写。
func memberCheck(ctx context.Context, db *sql.DB, uid, companyID, convID string) ([]string, int, string) {
	var membersJSON string
	err := db.QueryRowContext(ctx,
		`SELECT members::text FROM conversations WHERE id = $1 AND company_id = $2`, convID, companyID).Scan(&membersJSON)
	if err != nil {
		return nil, http.StatusNotFound, "conversation not found"
	}
	var members []string
	_ = json.Unmarshal([]byte(membersJSON), &members)
	for _, m := range members {
		if m == uid {
			return members, 0, ""
		}
	}
	return nil, http.StatusNotFound, "conversation not found"
}

// postSystemMessage 对齐 membership.postMembershipSystemMessage:
// body = JSON {kind, participantId, actorId};消费 WS 广播留 Redis 面。
// #138:取序 + INSERT 单事务(对齐 runtime/cli_reply.go 的事务形态),
// 任一步失败整体回滚 + slog——此前两步裸跑、`_,_=` 吞错,半写会留孤儿
// counter(序号断档)且无排障痕迹。系统消息是礼节性写:失败不阻断
// 成员变更主路径,只留日志。
func postSystemMessage(ctx context.Context, db *sql.DB, convID, companyID, actorID, sysKind, participantID string) {
	var sequence int64
	body, _ := json.Marshal(map[string]string{
		"kind": sysKind, "participantId": participantID, "actorId": actorID,
	})
	msgID := "m-" + authn.NewToken()[:12]
	if err := dbpkg.WithTx(ctx, db, func(tx *sql.Tx) error {
		seq, err := dbpkg.AllocSequence(ctx, tx, convID)
		if err != nil {
			return err
		}
		sequence = seq
		_, err = tx.ExecContext(ctx, `
			INSERT INTO messages (id, conversation_id, company_id, author_id, kind, body, sequence)
			VALUES ($1, $2, $3, $4, 'system', $5, $6)`,
			msgID, convID, companyID, actorID, body, sequence)
		return err
	}); err != nil {
		slog.Warn("postSystemMessage tx failed", "conv", convID, "kind", sysKind, "err", err)
		return
	}
	events.MessageNew(ctx, companyID, convID, map[string]any{
		"id": msgID, "conversationId": convID, "authorId": actorID,
		"kind": "system", "body": string(body), "sequence": sequence,
		"at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func memberGate(w http.ResponseWriter, r *http.Request, db *sql.DB, uid, companyID, convID string) ([]string, bool) {
	members, code, msg := memberCheck(r.Context(), db, uid, companyID, convID)
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return nil, false
	}
	return members, true
}

func (s *Server) SetTopic(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	if _, ok := memberGate(w, r, s.DB, uid, companyID, convID); !ok {
		return
	}
	var body struct {
		Topic *string `json:"topic"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	var topic any
	if body.Topic != nil {
		t := httpx.UTF16Cap(strings.TrimSpace(*body.Topic), 200)
		if t == "" {
			topic = nil
		} else {
			topic = t
		}
	}
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE conversations SET topic = $2, updated_at = NOW() WHERE id = $1`, convID, topic); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "topic": topic})
}

func (s *Server) SetTitle(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	if _, ok := memberGate(w, r, s.DB, uid, companyID, convID); !ok {
		return
	}
	// F16:TS setTitle 是 String(x ?? '') 强转,struct 解码丢非串体。
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	var titleRaw any
	_ = json.Unmarshal(raw["title"], &titleRaw)
	title := strings.TrimSpace(httpx.JSStringOrNullish(titleRaw))
	title = httpx.UTF16Cap(title, 80)
	if title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "title required")
		return
	}
	var kind string
	_ = s.DB.QueryRowContext(r.Context(), `SELECT kind FROM conversations WHERE id = $1`, convID).Scan(&kind)
	if kind != "group" {
		httpx.WriteError(w, http.StatusBadRequest, "only group chats can be renamed")
		return
	}
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE conversations SET title = $2, updated_at = NOW() WHERE id = $1`, convID, title); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "title": title})
}

func (s *Server) TogglePin(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	if _, ok := memberGate(w, r, s.DB, uid, companyID, convID); !ok {
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
		_ = s.DB.QueryRowContext(r.Context(), `SELECT pinned FROM conversations WHERE id = $1`, convID).Scan(&cur)
		pinned = !cur
	}
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE conversations SET pinned = $2, updated_at = NOW() WHERE id = $1`, convID, pinned); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "pinned": pinned})
}

func (s *Server) SetMute(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	if _, ok := memberGate(w, r, s.DB, uid, companyID, convID); !ok {
		return
	}
	var body struct {
		Mute  bool   `json:"mute"`
		Until string `json:"until"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Mute {
		_, _ = s.DB.ExecContext(r.Context(),
			`DELETE FROM conversation_mutes WHERE conversation_id = $1 AND user_id = $2`, convID, uid)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "muted": false, "mutedUntil": nil})
		return
	}
	var until any
	if body.Until != "" {
		t, err := time.Parse(time.RFC3339, body.Until)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "invalid until timestamp")
			return
		}
		if !t.After(time.Now()) {
			httpx.WriteError(w, http.StatusBadRequest, "until must be in the future")
			return
		}
		until = body.Until
	}
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO conversation_mutes (conversation_id, user_id, muted_until) VALUES ($1, $2, $3::timestamptz)
		ON CONFLICT (conversation_id, user_id) DO UPDATE SET muted_until = $3::timestamptz`,
		convID, uid, until); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "muted": true, "mutedUntil": until})
}

func (s *Server) AddMember(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	members, ok := memberGate(w, r, s.DB, uid, companyID, convID)
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
	var convKind string
	_ = s.DB.QueryRowContext(r.Context(), `SELECT kind FROM conversations WHERE id = $1`, convID).Scan(&convKind)
	if convKind == "direct" {
		httpx.WriteError(w, http.StatusBadRequest, "cannot add to a direct conversation")
		return
	}
	for _, m := range members {
		if m == body.ID {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "members": members, "alreadyIn": true})
			return
		}
	}
	var exists bool
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`, body.ID, companyID).Scan(&exists)
	if !exists {
		httpx.WriteError(w, http.StatusBadRequest, "unknown participant")
		return
	}
	next := append(members, body.ID)
	nj, _ := json.Marshal(next)
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE conversations SET members = $2::jsonb, updated_at = NOW() WHERE id = $1`, convID, nj); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	postSystemMessage(r.Context(), s.DB, convID, companyID, uid, "joined", body.ID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "members": next})
}

func (s *Server) LeaveConversation(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	members, ok := memberGate(w, r, s.DB, uid, companyID, convID)
	if !ok {
		return
	}
	var kind string
	_ = s.DB.QueryRowContext(r.Context(), `SELECT kind FROM conversations WHERE id = $1`, convID).Scan(&kind)
	if kind == "direct" {
		httpx.WriteError(w, http.StatusBadRequest, "cannot leave a direct conversation")
		return
	}
	postSystemMessage(r.Context(), s.DB, convID, companyID, uid, "left", uid)
	var next []string
	for _, m := range members {
		if m != uid {
			next = append(next, m)
		}
	}
	nj, _ := json.Marshal(next)
	if _, err := s.DB.ExecContext(r.Context(),
		`UPDATE conversations SET members = $2::jsonb, updated_at = NOW() WHERE id = $1`, convID, nj); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "members": next})
}

func (s *Server) EmitTyping(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	if _, ok := memberGate(w, r, s.DB, uid, companyID, convID); !ok {
		return
	}
	var body struct {
		Done bool `json:"done"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	events.Typing(r.Context(), companyID, convID, uid, body.Done)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) MarkRead(w http.ResponseWriter, r *http.Request, id string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	if _, ok := memberGate(w, r, s.DB, uid, companyID, convID); !ok {
		return
	}
	_, _ = s.DB.ExecContext(r.Context(), `
		INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
		VALUES ($1, $2, NOW()) ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`,
		uid, convID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) GetReplies(w http.ResponseWriter, r *http.Request, id string, rootId string) {
	uid, companyID, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	convID := id
	if _, ok := memberGate(w, r, s.DB, uid, companyID, convID); !ok {
		return
	}
	rootID := rootId
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT m.id, m.conversation_id, m.author_id, m.kind, m.body, m.sequence, m.created_at
		  FROM messages m WHERE m.conversation_id = $1 AND m.quoted_message_id = $2
		 ORDER BY m.sequence ASC`, convID, rootID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
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

func (s *Server) GetConversations(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	// 平价 baseline 的列表 SQL(见 router.ts GET /conversations):
	// direct 标题取对方参与者名;muted 由未过期 mute 行推导;
	// lastMessage 取 seq 最新一条(含 email 三字段);unreadCount 排除
	// 自己且晚于 last_read_at。
	rows, err := s.DB.QueryContext(r.Context(), `
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
		FROM conversation_members cmv
		JOIN conversations c ON c.id = cmv.conversation_id
		LEFT JOIN projects p ON p.id = c.project_id
		LEFT JOIN conversation_mutes mu ON mu.conversation_id = c.id AND mu.user_id = $1
		LEFT JOIN LATERAL (
			SELECT po.name FROM jsonb_array_elements_text(c.members) WITH ORDINALITY AS member(id, ord)
			JOIN participants po ON po.id = member.id AND po.company_id = c.company_id
			WHERE member.id <> $1 ORDER BY member.ord LIMIT 1
		) op ON c.kind = 'direct'
		WHERE cmv.participant_id = $1 AND c.company_id = $2
		ORDER BY c.pinned DESC, c.updated_at DESC`, uid, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
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

func (s *Server) CreateGroup(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	// F16:TS create 各键是 String(x ?? '')/truthy 门强转,struct 解码在
	// 非串值上会整包丢弃 → 改逐键解码。members 对齐 TS:非数组忽略,
	// 数组内只收 string(其余元素跳过)。
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	keyAny := func(k string) (any, bool) {
		v, has := raw[k]
		if !has {
			return nil, false
		}
		var a any
		_ = json.Unmarshal(v, &a)
		return a, true
	}
	titleRaw, _ := keyAny("title")
	title := httpx.UTF16Cap(strings.TrimSpace(httpx.JSStringOrNullish(titleRaw)), 80)
	if title == "" {
		httpx.WriteError(w, http.StatusBadRequest, "title required")
		return
	}
	var topic any
	if tRaw, has := keyAny("topic"); has && httpx.JSTruthy(tRaw) {
		t := httpx.UTF16Cap(strings.TrimSpace(httpx.JSToString(tRaw)), 200)
		if t != "" {
			topic = t
		}
	}
	// F-03:TS `projectId ? String(p).trim() : null` —— truthy 门后 trim
	// 出的空串以 '' 入库(bug 兼容,不抬成 NULL),仅非空才校验存在。
	// 校验顺序保持 TS 基线:成员校验在前、项目存在性在后(双错请求
	// 同报 unknown participant,评审 P3)。
	var projectID any
	var projectIDStr string
	if pRaw, has := keyAny("projectId"); has && httpx.JSTruthy(pRaw) {
		projectIDStr = strings.TrimSpace(httpx.JSToString(pRaw))
		projectID = projectIDStr
	}
	seen := map[string]bool{uid: true}
	members := []string{}
	if mRaw, has := keyAny("members"); has {
		var arr []any
		if b, err := json.Marshal(mRaw); err == nil && json.Unmarshal(b, &arr) == nil {
			for _, m := range arr {
				if s, isStr := m.(string); isStr {
					s = strings.TrimSpace(s)
					if s != "" && !seen[s] {
						seen[s] = true
						members = append(members, s)
					}
				}
			}
		}
	}
	if !seen[uid] || !contains(members, uid) {
		members = append(members, uid) // 调用者自动并入(baseline memberSet.add(me))
	}
	if len(members) < 2 {
		httpx.WriteError(w, http.StatusBadRequest, "pick at least one teammate")
		return
	}
	// 校验成员同租户
	valid := map[string]bool{}
	vrows, _ := s.DB.QueryContext(r.Context(),
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
	if projectIDStr != "" {
		var exists bool
		_ = s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`, projectIDStr, companyID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusBadRequest, "unknown project")
			return
		}
	}
	id := "g-" + authn.NewToken()[:8]
	membersJSON, _ := json.Marshal(members)
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO conversations (id, kind, title, topic, members, pinned, company_id, project_id)
		VALUES ($1, 'group', $2, $3, $4::jsonb, FALSE, $5, $6)`,
		id, title, topic, membersJSON, companyID, projectID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_, _ = s.DB.ExecContext(r.Context(),
		`INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 1)`, id)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "members": members, "projectId": projectID})
}

func (s *Server) OpenDirect(w http.ResponseWriter, r *http.Request) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
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
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`, body.OtherID, companyID).Scan(&exists)
	if !exists {
		httpx.WriteError(w, http.StatusBadRequest, "unknown participant")
		return
	}
	// 已有同成员 DM → 复用(members 严格等价:恰为 [a,b] 或 [b,a])
	membersJSON, _ := json.Marshal([]string{uid, body.OtherID})
	otherJSON, _ := json.Marshal([]string{body.OtherID, uid})
	var existing string
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT id FROM conversations
		 WHERE company_id = $1 AND kind = 'direct' AND (members = $2::jsonb OR members = $3::jsonb)
		 LIMIT 1`, companyID, membersJSON, otherJSON).Scan(&existing)
	if err == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"id": existing, "created": false})
		return
	}
	id := "d-" + authn.NewToken()[:8]
	if _, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO conversations (id, kind, title, members, company_id)
		VALUES ($1, 'direct', '', $2::jsonb, $3)`, id, membersJSON, companyID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	_, _ = s.DB.ExecContext(r.Context(),
		`INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 1)`, id)
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"id": id, "created": true})
}

func (s *Server) GetMessages(w http.ResponseWriter, r *http.Request, id string, params contract.GetMessagesParams) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	convID := id
	// 会话存在 + 成员资格(非成员 404,baseline 的"存在性不透明"策略)
	members, code, msg := memberCheck(r.Context(), s.DB, uid, companyID, convID)
	if code != 0 {
		httpx.WriteError(w, code, msg)
		return
	}
	_ = members
	// 分页对齐 baseline:默认 80,clamp [1,500],before 非数值忽略
	limit := 80
	if v := r.URL.Query().Get("limit"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			limit = n
			if limit < 1 {
				limit = 1
			}
			if limit > 500 {
				limit = 500
			}
		}
	}
	q := `
		SELECT m.id, m.conversation_id, m.author_id, m.kind, m.body, m.sequence,
		       m.client_id, m.tool::text, m.attachment::text, m.poll::text,
		       COALESCE((
		         SELECT jsonb_agg(jsonb_build_object('optionId', pv.option_id, 'count', pv.cnt, 'voterIds', pv.voter_ids)
		           ORDER BY pv.cnt DESC, pv.option_id ASC)
		         FROM (SELECT option_id, COUNT(*)::int AS cnt,
		                      array_agg(voter_participant_id ORDER BY voter_participant_id) AS voter_ids
		                 FROM poll_votes WHERE message_id = m.id GROUP BY option_id) pv
		       ), '[]'::jsonb)::text AS poll_tallies,
		       m.quoted_message_id, m.created_at,
		       (
		         SELECT jsonb_build_object('subject', em.subject, 'from', em.from_addr,
		             'to', em.to_addrs, 'cc', em.cc_addrs, 'direction', em.direction,
		             'transportStatus', em.transport_status, 'transportError', em.transport_error,
		             'smtpMessageId', em.smtp_message_id, 'inReplyTo', em.in_reply_to,
		             'hasHtml', em.html IS NOT NULL, 'autoSubmitted', em.auto_submitted,
		             'attachments', COALESCE((SELECT jsonb_agg(jsonb_build_object(
		                 'id', ea.id, 'filename', ea.filename, 'mimeType', ea.mime_type,
		                 'sizeBytes', ea.size_bytes, 'storageKey', ea.storage_key, 'truncated', ea.truncated
		               ) ORDER BY ea.created_at) FROM email_attachments ea WHERE ea.message_id = m.id), '[]'::jsonb)
		       )::text AS email_fields
		         FROM email_messages em WHERE em.message_id = m.id
		       ),
		       COALESCE((
		         SELECT jsonb_agg(jsonb_build_object('emoji', rx.emoji, 'count', rx.count, 'users', rx.users))
		         FROM (SELECT emoji, COUNT(*)::int AS count, array_agg(user_id ORDER BY user_id) AS users
		                 FROM message_reactions WHERE message_id = m.id GROUP BY emoji
		                ORDER BY count DESC, emoji ASC) rx
		       ), '[]'::jsonb)::text AS reactions_agg,
		       (
		         SELECT jsonb_build_object('id', qm.id, 'authorId', qm.author_id,
		             'authorName', COALESCE(qp.name, qu.display_name, qm.author_id),
		             'kind', qm.kind, 'body', LEFT(qm.body, 240), 'sequence', qm.sequence)
		         FROM messages qm
		         LEFT JOIN participants qp ON qp.id = qm.author_id AND qp.company_id = $2
		         LEFT JOIN users qu ON qu.id = qm.author_id
		         WHERE qm.id = m.quoted_message_id AND qm.conversation_id = m.conversation_id
		       )::text AS quoted_summary,
		       (SELECT COUNT(*)::int FROM messages rm WHERE rm.quoted_message_id = m.id) AS reply_count
		  FROM messages m WHERE m.conversation_id = $1`
	args := []any{convID, companyID}
	if v := r.URL.Query().Get("before"); v != "" {
		var b int
		if _, err := fmt.Sscanf(v, "%d", &b); err == nil {
			q += fmt.Sprintf(" AND m.sequence < $%d", len(args)+1)
			args = append(args, b)
		}
	}
	q += fmt.Sprintf(" ORDER BY m.sequence DESC LIMIT %d", limit)
	rows, err := s.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	// DESC 取最新 N 条,内存翻转为 ASC(baseline 语义:渲染端 append-only 假设)
	var ordered []*map[string]any
	for rows.Next() {
		var id, convID2, authorID, kind, body string
		var sequence int
		var clientID, tool, attachment, poll, pollTallies, quotedID sql.NullString
		var createdAt sql.NullTime
		var emailF, reactions, quotedSummary sql.NullString
		var replyCount int
		if err := rows.Scan(&id, &convID2, &authorID, &kind, &body, &sequence,
			&clientID, &tool, &attachment, &poll, &pollTallies, &quotedID, &createdAt,
			&emailF, &reactions, &quotedSummary, &replyCount); err != nil {
			continue
		}
		msg := map[string]any{
			"id": id, "conversationId": convID2, "authorId": authorID,
			"kind": kind, "body": body, "sequence": sequence,
			"pollTallies": jsonbOr(pollTallies, []any{}),
			"replyCount":  replyCount,
			"createdAt":   createdAt.Time.UTC(),
			"reactions":   jsonbOr(reactions, []any{}),
		}
		if clientID.Valid && clientID.String != "" {
			msg["clientId"] = clientID.String
		}
		msg["tool"] = jsonbOrNull(tool)
		msg["attachment"] = jsonbOrNull(attachment)
		msg["poll"] = jsonbOrNull(poll)
		msg["email"] = jsonbOrNull(emailF)
		if quotedID.Valid && quotedID.String != "" {
			msg["quotedMessageId"] = quotedID.String
		}
		msg["quoted"] = jsonbOrNull(quotedSummary)
		ordered = append(ordered, &msg)
	}
	// 反转为 ASC
	out := make([]map[string]any, len(ordered))
	for i, m := range ordered {
		out[len(ordered)-1-i] = *m
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) SendMessage(w http.ResponseWriter, r *http.Request, id string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	convID := id
	if _, code, msg := memberCheck(r.Context(), s.DB, uid, companyID, convID); code != 0 {
		httpx.WriteError(w, code, msg)
		return
	}
	// 逐键解码(#118):TS router.ts:3263 `String(req.body?.body ?? '')
	// .trim()` —— struct 解码会把非串值(123/true/[])整包丢弃,与 TS
	// 强转分叉;quotedMessageId 仅认字符串;clientId 门 = TS 3295:
	// 非 null 须为非空 ≤80 字符串,否则 400 'invalid clientId'(长度
	// 按 UTF-16 计,clientId 是 ASCII token,字节计等价)。
	var body struct {
		Body            string
		Attachment      json.RawMessage
		QuotedMessageID string
		ClientID        string
	}
	var raw map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	rawOf := func(k string) any {
		v, ok := raw[k]
		if !ok {
			return nil
		}
		var a any
		_ = json.Unmarshal(v, &a)
		return a
	}
	body.Body = strings.TrimSpace(httpx.JSStringOrNullish(rawOf("body")))
	if v, ok := raw["attachment"]; ok {
		body.Attachment = v
	}
	if s, isStr := rawOf("quotedMessageId").(string); isStr {
		body.QuotedMessageID = s
	}
	if v, ok := raw["clientId"]; ok && string(v) != "null" {
		var s string
		// TS 上限按 UTF-16 码元计;此处按字节 —— clientId 由客户端生成
		// 的 ASCII token,字节=码元,非 ASCII 形态只会更严不更宽。
		if json.Unmarshal(v, &s) != nil || s == "" || len(s) > 80 {
			httpx.WriteError(w, http.StatusBadRequest, "invalid clientId")
			return
		}
		body.ClientID = s
	}
	// attachment 形状对齐 baseline:要求 url+name 为字符串;kind 非白名单
	// 强转 'img'(从不拒绝)——见 router.ts:3264-3281 的内联 coerce 语义;
	// 畸形视同无附件(attachment=null 同款)。
	var attachmentJSON any
	if len(body.Attachment) > 0 && string(body.Attachment) != "null" {
		var att struct {
			URL  *string `json:"url"`
			Name *string `json:"name"`
			Kind string  `json:"kind"`
		}
		if json.Unmarshal(body.Attachment, &att) == nil && att.URL != nil && att.Name != nil {
			switch att.Kind {
			case "img", "pdf", "file", "fig":
			default:
				var fixed map[string]any
				_ = json.Unmarshal(body.Attachment, &fixed)
				fixed["kind"] = "img"
				if b, err := json.Marshal(fixed); err == nil {
					body.Attachment = b
				}
			}
			attachmentJSON = json.RawMessage(body.Attachment)
		}
	}
	// 双空门对齐 TS 次序与文案(router.ts:3284:coerce 后、email 分支前,
	// 恒 'empty message';Go 曾漂移 'body required' 且门在 email 分支后,
	// 空 body 进 email 会话会先吃到 'email replies require a body')。
	if body.Body == "" && attachmentJSON == nil {
		httpx.WriteError(w, http.StatusBadRequest, "empty message")
		return
	}
	// email 会话升格(#70 补线):聊天式回复代发真邮件——原语在
	// email.ReplyInEmailConversation(email.ts replyInEmailConversation
	// 的 Go 等价),TS router 在此分支代调;漏接则聊天框打字只落
	// kind='text' 行,外部收件人永远看不见。人类路径 autoSubmitted
	// 留 false(agent 走 CLI 路径自置 true)。
	var convKind string
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT kind FROM conversations WHERE id = $1`, convID).Scan(&convKind)
	if convKind == "email" {
		if body.Body == "" { // TS body 先 trim:空白串同样拒
			httpx.WriteError(w, http.StatusBadRequest,
				"email replies require a body (attachments-only sends not supported here yet)")
			return
		}
		result, err := emailpkg.ReplyInEmailConversation(r.Context(), s.DB, emailpkg.ReplyArgs{
			ConversationID: convID, CompanyID: companyID, AuthorID: uid, Body: body.Body,
		})
		if err != nil {
			// TS router.ts:3344 显式 catch 是 500 无条件透传 msg(非
			// errorHandler 面);#214 起随 #184 既定方向收编进
			// WriteInternalError(dev 透传 err、production 通用文案)。
			httpx.WriteInternalError(w, r, err)
			return
		}
		status := http.StatusBadGateway
		if result.TransportStatus == "sent" {
			status = http.StatusAccepted
		}
		var errAny any
		if result.Error != "" {
			errAny = result.Error
		}
		httpx.WriteJSON(w, status, map[string]any{
			"id": result.MessageID, "sequence": result.Sequence,
			"transportStatus": result.TransportStatus, "mock": result.Mock,
			"error": errAny,
		})
		return
	}
	// quoted 同会话校验(防跨房间泄漏;未知静默丢弃)
	var quoted any
	if body.QuotedMessageID != "" {
		var exists bool
		_ = s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM messages WHERE id = $1 AND conversation_id = $2 LIMIT 1`,
			body.QuotedMessageID, convID).Scan(&exists)
		if exists {
			quoted = body.QuotedMessageID
		}
	}
	// 写段单事务(#138):取序 + INSERT + updated_at 同生共死——任一步
	// 失败整体回滚,不再留孤儿 counter(序号断档);错误传播 + slog
	// (此前 updated_at 失败被 `_,_=` 吞)。事务形态对齐
	// runtime/cli_reply.go。赢在原子性不在时延:BEGIN/COMMIT 各加一趟
	// 往返,真正的往返大头(前置认证查询无缓存)另票(#141)。
	// 事务豁免(#213,#235 复审仍留):#213 的"sequence failed/
	// insert failed 文案区分"理由已被 #214 抹平(各步失败均为
	// WriteInternalError(err) 同构);仍豁免因 clientId 幂等重放分支需
	// mid-body 回滚(退掉本次预占的序号)+ 202 短路——2xx 走回滚路径
	// 出口,WithTx 单一错误通道需带载荷哨兵才能表达,手写形态更直白。
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer tx.Rollback() // 提交后为 no-op
	var sequence int64
	if sequence, err = dbpkg.AllocSequence(r.Context(), tx, convID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	msgID := "m-" + authn.NewToken()[:12]
	// clientId 幂等(#70 补线):同 (会话,作者,clientId) 重发/并发
	// 双投,经部分唯一索引 ON CONFLICT DO NOTHING + 回查取原行——
	// TS router 同款;裸 INSERT 会在重试时撞索引 500。
	var persistedID string
	var persistedSeq int64
	err = tx.QueryRowContext(r.Context(), `
		INSERT INTO messages (id, conversation_id, company_id, author_id, kind, body, sequence, quoted_message_id, client_id, attachment)
		VALUES ($1, $2, $3, $4, 'text', $5, $6, $7, NULLIF($8,''), $9)
		ON CONFLICT (conversation_id, author_id, client_id) WHERE client_id IS NOT NULL
		DO NOTHING
		RETURNING id, sequence`,
		msgID, convID, companyID, uid, body.Body, sequence, quoted, body.ClientID, attachmentJSON).Scan(&persistedID, &persistedSeq)
	if err != nil {
		// DO NOTHING 落空(零行)= 撞唯一索引:回查既有行(并发双投
		// 时另一请求已落);其余才是真错。复用路径照 TS 短路——不
		// bump updated_at、不重播 message.new、不重推(lost-ACK 重试
		// 不产生第二次副作用);回滚只退掉本次预占的序号(重发路径
		// 本就不该消耗序号)。
		if !(err == sql.ErrNoRows && body.ClientID != "" &&
			tx.QueryRowContext(r.Context(),
				`SELECT id, sequence FROM messages WHERE conversation_id = $1 AND author_id = $2 AND client_id = $3`,
				convID, uid, body.ClientID).Scan(&persistedID, &persistedSeq) == nil) {
			httpx.WriteInternalError(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"id": persistedID, "sequence": persistedSeq})
		return
	}
	msgID, sequence = persistedID, persistedSeq
	if _, err := tx.ExecContext(r.Context(), `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, convID); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	broadcastMsg := map[string]any{
		"id": msgID, "conversationId": convID, "authorId": uid,
		"kind": "text", "body": body.Body, "sequence": sequence,
		"at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	if attachmentJSON != nil {
		broadcastMsg["attachment"] = attachmentJSON
	}
	if body.QuotedMessageID != "" && quoted != nil {
		broadcastMsg["quotedMessageId"] = body.QuotedMessageID
	}
	if body.ClientID != "" {
		broadcastMsg["clientId"] = body.ClientID
	}
	events.MessageNew(r.Context(), companyID, convID, broadcastMsg)
	// 推送扇出(#59):不阻塞响应;凭据未配置时软关停为 no-op。
	go func(convID, uid, msgID, body, companyID string) {
		// TS 用 try/catch 包住整个扇出;Go 侧 panic 会杀进程,必须 recover。
		defer func() {
			if rec := recover(); rec != nil {
				slog.Warn("push fanout panicked", "err", rec)
			}
		}()
		// #136:扇出预算见 pushFanoutBudget;defer cancel 随 goroutine 退出。
		ctx, cancel := context.WithTimeout(context.Background(), pushFanoutBudget)
		defer cancel()
		recipients := push.ComputeMessageRecipients(ctx, s.DB, convID, uid)
		if len(recipients) == 0 {
			return
		}
		var title sql.NullString
		_ = s.DB.QueryRowContext(ctx, `SELECT title FROM conversations WHERE id = $1`, convID).Scan(&title)
		authorName := uid
		var dn string
		if s.DB.QueryRowContext(ctx, `SELECT display_name FROM users WHERE id = $1`, uid).Scan(&dn) == nil {
			authorName = dn // TS:display_name ?? me —— 空串也保留
		}
		push.NotifyMessage(ctx, s.DB, struct {
			ConversationID    string
			ConversationTitle *string
			AuthorID          string
			AuthorName        string
			MessageID         string
			Body              string
			CompanyID         string
			RecipientUserIDs  []string
		}{
			ConversationID: convID, ConversationTitle: nullStrPtr(title),
			AuthorID: uid, AuthorName: authorName, MessageID: msgID,
			Body: body, CompanyID: companyID, RecipientUserIDs: recipients,
		})
	}(convID, uid, msgID, body.Body, companyID)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"id": msgID, "sequence": sequence})
}

/* helpers */

func nullStrPtr(ns sql.NullString) *string {
	if ns.Valid {
		return &ns.String
	}
	return nil
}

func jsonbOr(ns sql.NullString, fallback any) any {
	if ns.Valid && ns.String != "" && ns.String != "null" {
		var v any
		if json.Unmarshal([]byte(ns.String), &v) == nil {
			return v
		}
	}
	return fallback
}

func jsonbOrNull(ns sql.NullString) any { return jsonbOr(ns, nil) }

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

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
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

/* ───────── reactions(#68 补齐:POST /api/messages/{id}/reactions) ───────── */

// toggleReaction:表情开关 + climate 信号(对 agent 消息的新增反应抬
// affinity)+ cumora:reactions 广播。非成员与跨租户同走 404(存在性
// 不透明策略)。
func (s *Server) ToggleReaction(w http.ResponseWriter, r *http.Request, id string) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return
	}
	tenant, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	var emojiRaw any
	_ = json.Unmarshal(body["emoji"], &emojiRaw)
	emoji := strings.TrimSpace(httpx.JSStringOrNullish(emojiRaw))
	if emoji == "" {
		httpx.WriteError(w, http.StatusBadRequest, "emoji required")
		return
	}
	var convoID, authorID, membersJSON string
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT m.conversation_id, m.author_id, c.members::text
		  FROM messages m
		  JOIN conversations c ON c.id = m.conversation_id
		 WHERE m.id = $1 AND c.company_id = $2 LIMIT 1`, id, tenant).
		Scan(&convoID, &authorID, &membersJSON)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "message not found")
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
		httpx.WriteError(w, http.StatusNotFound, "message not found")
		return
	}

	var count int64
	_ = s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3`,
		id, uid, emoji).Scan(&count)
	wasRemoval := count > 0
	if wasRemoval {
		_, _ = s.DB.ExecContext(r.Context(),
			`DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3`,
			id, uid, emoji)
	} else {
		_, _ = s.DB.ExecContext(r.Context(),
			`INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			id, uid, emoji)
	}
	if !wasRemoval {
		bumpClimate(r.Context(), s.DB, authorID, uid, 0.05, 0.02, "received "+emoji+" from "+uid)
	}

	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT emoji, COUNT(*)::int AS count, to_json(array_agg(user_id ORDER BY user_id))::text AS users
		  FROM message_reactions WHERE message_id = $1
		 GROUP BY emoji ORDER BY count DESC, emoji ASC`, id)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	agg := []map[string]any{}
	for rows.Next() {
		var em string
		var cnt int
		var usersJSON string
		if err := rows.Scan(&em, &cnt, &usersJSON); err != nil {
			continue
		}
		var users []string
		_ = json.Unmarshal([]byte(usersJSON), &users)
		agg = append(agg, map[string]any{"emoji": em, "count": cnt, "users": users})
	}
	_ = events.PublishRaw(r.Context(), "cumora:reactions", mustJSON(map[string]any{
		"type":           "message.reactions",
		"conversationId": convoID,
		"companyId":      tenant,
		"messageId":      id,
		"reactions":      agg,
	}))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"reactions": agg})
}

// bumpClimate:agents/climate.ts bumpClimate 的 Go 等价(仅 agent 作者
// 落行;±1 夹紧;失败仅告警——climate 是信号不是不变量)。
func bumpClimate(ctx context.Context, db *sql.DB, agentID, aboutID string, affinity, trust float64, note string) {
	if agentID == "" || aboutID == "" || agentID == aboutID {
		return
	}
	if affinity == 0 && trust == 0 {
		return
	}
	var kind string
	if err := db.QueryRowContext(ctx, `SELECT kind FROM participants WHERE id = $1 LIMIT 1`, agentID).Scan(&kind); err != nil || kind != "agent" {
		return
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO agent_climate (agent_id, about_id, affinity, trust, last_note, updated_at)
		VALUES ($1, $2,
		        GREATEST(-1, LEAST(1, $3::real)),
		        GREATEST(-1, LEAST(1, $4::real)),
		        $5, NOW())
		ON CONFLICT (agent_id, about_id) DO UPDATE
		   SET affinity = GREATEST(-1, LEAST(1, agent_climate.affinity + $3::real)),
		       trust    = GREATEST(-1, LEAST(1, agent_climate.trust    + $4::real)),
		       last_note = COALESCE($5, agent_climate.last_note),
		       updated_at = NOW()`,
		agentID, aboutID, clampF(affinity), clampF(trust), note)
	if err != nil {
		slog.Warn("[climate] bump failed", "err", err)
	}
}

func clampF(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
