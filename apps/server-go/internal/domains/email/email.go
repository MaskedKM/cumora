// domains/email —— 邮件 HTTP 面(#58 出站半边):send / :id/html /
// reply。对齐 router.ts 的 /email 三端点;核心管线在 internal/email。
// 入站 webhook(HMAC)与重试/GC 任务在后续 commit(同票)。
package email

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	contract "github.com/MaskedKM/cumora/apps/server-go/internal/contract/email"
	core "github.com/MaskedKM/cumora/apps/server-go/internal/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// Server:contract.email ServerInterface 的域实现(#187 机械迁移,
// documents 范式)。方法体自原闭包工厂原样搬运。
type Server struct{ DB *sql.DB }

// 编译期接口把关:规范改动 operation 而域未跟 = 构建红。
var _ contract.ServerInterface = (*Server)(nil)

// Mount:注册串来自契约生成物(pattern 即规范,#139)。
func Mount(mux *http.ServeMux, db *sql.DB) {
	_ = contract.HandlerFromMux(&Server{DB: db}, mux)
}

func decodeBody(w http.ResponseWriter, r *http.Request) map[string]json.RawMessage {
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body
}

func arrOf(body map[string]json.RawMessage, key string) []any {
	var arr []any
	if raw, ok := body[key]; ok {
		_ = json.Unmarshal(raw, &arr)
	}
	return arr
}

func (s *Server) SendEmail(w http.ResponseWriter, r *http.Request) {
	me, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	body := decodeBody(w, r)
	var subjectRaw any
	_ = json.Unmarshal(body["subject"], &subjectRaw)
	subject := core.SanitizeSubject(anyString(subjectRaw))
	var bodyRaw any
	_ = json.Unmarshal(body["body"], &bodyRaw)
	text := strings.TrimSpace(anyString(bodyRaw))
	text = core.Utf16Cap(text, 50_000)
	toRaw := arrOf(body, "to")
	if len(toRaw) == 0 || subject == "" || text == "" {
		httpx.WriteError(w, http.StatusBadRequest, "to, subject, body required")
		return
	}
	attachments, attErr := core.ResolveAttachments(arrOf(body, "attachments"))
	if attErr != "" {
		httpx.WriteError(w, http.StatusBadRequest, attErr)
		return
	}
	sender, ok2 := core.EnsureParticipantAddress(r.Context(), s.DB, me, tenant)
	if !ok2 {
		httpx.WriteError(w, http.StatusBadRequest, "no email address available for your account in this team")
		return
	}
	resolveAll := func(raw []any, label string) ([]rcpt, string) {
		out := []rcpt{}
		for _, e := range raw {
			recv := anyString(e)
			addr, name, ok3 := core.ResolveRecipient(r.Context(), s.DB, recv, tenant)
			if !ok3 {
				return nil, "unresolved " + label + ": " + recv
			}
			out = append(out, rcpt{addr, name})
		}
		return out, ""
	}
	toResolved, err1 := resolveAll(toRaw, "recipient")
	if err1 != "" {
		httpx.WriteError(w, http.StatusBadRequest, err1)
		return
	}
	ccResolved, err2 := resolveAll(arrOf(body, "cc"), "cc")
	if err2 != "" {
		httpx.WriteError(w, http.StatusBadRequest, err2)
		return
	}
	// 会话成员 = 发送者 + 每个同租户收件人(agent 按地址、human 按 auth email)。
	memberIDs := []string{me} // TS:new Set([me, ...]) 保插入序
	seenMember := map[string]bool{me: true}
	addMember := func(id string) {
		if !seenMember[id] {
			seenMember[id] = true
			memberIDs = append(memberIDs, id)
		}
	}
	for _, rc := range append(append([]rcpt{}, toResolved...), ccResolved...) {
		if pid, _, _, ok3 := core.FindParticipantByAddress(r.Context(), s.DB, rc.addr, tenant); ok3 {
			addMember(pid)
			continue
		}
		var uid string
		if s.DB.QueryRowContext(r.Context(), `
			SELECT u.id FROM users u JOIN company_members cm ON cm.user_id = u.id
			WHERE LOWER(u.email) = $1 AND cm.company_id = $2 LIMIT 1`, rc.addr, tenant).Scan(&uid) == nil {
			addMember(uid)
		}
	}
	messageID := core.MintMessageId()
	convID, _, err := core.FindOrCreateEmailConversation(r.Context(), s.DB, tenant, "", nil, subject, memberIDs)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	fromLine := core.FromLine(sender.Email, sender.DisplayName)
	providerAtts := []core.Attachment{}
	persistAtts := []core.PersistAttachment{}
	for _, a := range attachments {
		providerAtts = append(providerAtts, core.Attachment{Filename: a.Filename, MimeType: a.MimeType, Path: a.PublicURL})
		key := a.StorageKey
		persistAtts = append(persistAtts, core.PersistAttachment{
			Filename: a.Filename, MimeType: a.MimeType, SizeBytes: a.SizeBytes, StorageKey: &key,
		})
	}
	sendRes := core.SendViaProvider(r.Context(), core.SendArgs{
		From: fromLine,
		To:   formatAll(toResolved), CC: formatAll(ccResolved),
		Subject: subject, Text: text, MessageID: messageID,
		Attachments: providerAtts,
	})
	status := "sent"
	if !sendRes.OK {
		status = "failed"
	}
	persistedID, _, err := core.PersistEmailMessage(r.Context(), s.DB, core.PersistArgs{
		ConversationID: convID, CompanyID: tenant, AuthorID: me,
		Direction: "out", TransportStatus: status,
		TransportError: sendRes.Error,
		SmtpMessageID:  smtpIDOr(sendRes, messageID),
		Subject:        subject, FromAddr: fromLine,
		ToAddrs: formatAll(toResolved), CCAddrs: formatAll(ccResolved),
		Body: text, Attachments: persistAtts,
	})
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	code := http.StatusOK
	if !sendRes.OK {
		code = http.StatusBadGateway
	}
	var errAny any
	if sendRes.Error != "" {
		errAny = sendRes.Error
	}
	httpx.WriteJSON(w, code, map[string]any{
		"messageId": persistedID, "conversationId": convID,
		"transportStatus": status, "mock": sendRes.Mock, "error": errAny,
	})
}

func smtpIDOr(res core.ProviderSendResult, fallback string) string {
	if res.SmtpMessageID != "" {
		return res.SmtpMessageID
	}
	return fallback
}

type rcpt struct{ addr, name string }

func formatAll(xs []rcpt) []string {
	out := []string{}
	for _, x := range xs {
		out = append(out, core.FormatAddress(x.addr, x.name))
	}
	return out
}

func anyString(v any) string {
	s, _ := v.(string)
	return s
}

func (s *Server) FetchEmailHtml(w http.ResponseWriter, r *http.Request, messageId string) {
	me, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	var conversationID string
	var htmlBody sql.NullString
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT em.conversation_id, em.html FROM email_messages em
		WHERE em.message_id = $1 AND em.company_id = $2 LIMIT 1`,
		messageId, tenant).Scan(&conversationID, &htmlBody)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "unknown email message")
		return
	}
	if !htmlBody.Valid || htmlBody.String == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var members []byte
	if s.DB.QueryRowContext(r.Context(), `SELECT members FROM conversations WHERE id = $1`, conversationID).Scan(&members) != nil {
		httpx.WriteError(w, http.StatusForbidden, "not a member of this thread")
		return
	}
	var arr []string
	_ = json.Unmarshal(members, &arr)
	isMember := false
	for _, m := range arr {
		if m == me {
			isMember = true
		}
	}
	if !isMember {
		httpx.WriteError(w, http.StatusForbidden, "not a member of this thread")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src https: data:; style-src 'unsafe-inline'; font-src https: data:")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(core.SanitizeEmailHtml(htmlBody.String)))
}

var replyPrefixRe = regexp.MustCompile(`(?i)^(re|fwd|fw)\s*:`)

func (s *Server) ReplyEmail(w http.ResponseWriter, r *http.Request, messageId string) {
	me, tenant, ok := httpx.RequireCompany(w, r, s.DB)
	if !ok {
		return
	}
	body := decodeBody(w, r)
	var bodyRaw any
	_ = json.Unmarshal(body["body"], &bodyRaw)
	text := strings.TrimSpace(anyString(bodyRaw))
	text = core.Utf16Cap(text, 50_000)
	if text == "" {
		httpx.WriteError(w, http.StatusBadRequest, "body required")
		return
	}
	attachments, attErr := core.ResolveAttachments(arrOf(body, "attachments"))
	if attErr != "" {
		httpx.WriteError(w, http.StatusBadRequest, attErr)
		return
	}
	var conversationID string
	var smtpMessageID sql.NullString
	var referencesChain []byte
	var subject, fromAddr string
	var toAddrs, ccAddrs []byte
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT conversation_id, smtp_message_id, references_chain, subject, from_addr, to_addrs, cc_addrs
		FROM email_messages WHERE message_id = $1 AND company_id = $2`,
		messageId, tenant).
		Scan(&conversationID, &smtpMessageID, &referencesChain, &subject, &fromAddr, &toAddrs, &ccAddrs)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "unknown email message")
		return
	}
	var members []byte
	if s.DB.QueryRowContext(r.Context(), `SELECT members FROM conversations WHERE id = $1`, conversationID).Scan(&members) != nil {
		httpx.WriteError(w, http.StatusForbidden, "not a member of this thread")
		return
	}
	if !memberOf(members, me) {
		httpx.WriteError(w, http.StatusForbidden, "not a member of this thread")
		return
	}
	sender, ok2 := core.EnsureParticipantAddress(r.Context(), s.DB, me, tenant)
	if !ok2 {
		httpx.WriteError(w, http.StatusBadRequest, "no email address available for your account in this team")
		return
	}
	var userAuthEmail sql.NullString
	_ = s.DB.QueryRowContext(r.Context(), `SELECT email FROM users WHERE id = $1`, me).Scan(&userAuthEmail)
	selfAddrs := []string{strings.ToLower(sender.Email)}
	if userAuthEmail.Valid && userAuthEmail.String != "" {
		selfAddrs = append(selfAddrs, strings.ToLower(userAuthEmail.String))
	}
	var origTo, origCc []string
	_ = json.Unmarshal(toAddrs, &origTo)
	_ = json.Unmarshal(ccAddrs, &origCc)
	replyTo, replyCc := core.SplitReplyAddresses(fromAddr, origTo, origCc, selfAddrs)
	if len(replyTo) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "no other recipients to reply to")
		return
	}
	var ccResolved []rcpt
	for _, e := range arrOf(body, "cc") {
		recv := anyString(e)
		addr, name, ok3 := core.ResolveRecipient(r.Context(), s.DB, recv, tenant)
		if !ok3 {
			httpx.WriteError(w, http.StatusBadRequest, "unresolved cc: "+recv)
			return
		}
		ccResolved = append(ccResolved, rcpt{addr, name})
	}
	angleAddr := func(s string) string {
		if a, _, ok4 := core.ParseAddress(s); ok4 {
			return a
		}
		return s
	}
	ccSeen := map[string]bool{}
	for _, sa := range selfAddrs {
		ccSeen[sa] = true
	}
	for _, t := range replyTo {
		ccSeen[strings.ToLower(angleAddr(t))] = true
	}
	for _, c := range replyCc {
		ccSeen[strings.ToLower(angleAddr(c))] = true
	}
	ccCombined := append([]string{}, replyCc...)
	for _, rc := range ccResolved {
		if ccSeen[rc.addr] {
			continue
		}
		ccSeen[rc.addr] = true
		ccCombined = append(ccCombined, core.FormatAddress(rc.addr, rc.name))
	}
	var newSubject string
	if replyPrefixRe.MatchString(subject) { // TS 测原值,不 trim
		newSubject = core.SanitizeSubject(subject)
	} else {
		newSubject = core.SanitizeSubject("Re: " + subject)
	}
	var refs []string
	_ = json.Unmarshal(referencesChain, &refs)
	if smtpMessageID.Valid && smtpMessageID.String != "" {
		refs = append(refs, smtpMessageID.String)
	}
	var inReplyTo string
	if smtpMessageID.Valid && smtpMessageID.String != "" {
		inReplyTo = core.NormalizeMessageId(smtpMessageID.String)
	}
	messageID := core.MintMessageId()
	fromLine := core.FromLine(sender.Email, sender.DisplayName)
	providerAtts := []core.Attachment{}
	persistAtts := []core.PersistAttachment{}
	for _, a := range attachments {
		providerAtts = append(providerAtts, core.Attachment{Filename: a.Filename, MimeType: a.MimeType, Path: a.PublicURL})
		key := a.StorageKey
		persistAtts = append(persistAtts, core.PersistAttachment{
			Filename: a.Filename, MimeType: a.MimeType, SizeBytes: a.SizeBytes, StorageKey: &key,
		})
	}
	sendRes := core.SendViaProvider(r.Context(), core.SendArgs{
		From: fromLine, To: replyTo, CC: ccCombined,
		Subject: newSubject, Text: text,
		InReplyTo: inReplyTo, References: refs, MessageID: messageID,
		Attachments: providerAtts,
	})
	status := "sent"
	if !sendRes.OK {
		status = "failed"
	}
	persistedID, _, err := core.PersistEmailMessage(r.Context(), s.DB, core.PersistArgs{
		ConversationID: conversationID, CompanyID: tenant, AuthorID: me,
		Direction: "out", TransportStatus: status,
		TransportError: sendRes.Error,
		SmtpMessageID:  smtpIDOr(sendRes, messageID),
		InReplyTo:      inReplyTo, References: refs,
		Subject: newSubject, FromAddr: fromLine,
		ToAddrs: replyTo, CCAddrs: ccCombined,
		Body: text, Attachments: persistAtts,
	})
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	// Auto-ack:回复即已读(TS 同位)。
	_, _ = s.DB.ExecContext(r.Context(), `
		INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`, me, conversationID)
	code := http.StatusOK
	if !sendRes.OK {
		code = http.StatusBadGateway
	}
	var errAny any
	if sendRes.Error != "" {
		errAny = sendRes.Error
	}
	httpx.WriteJSON(w, code, map[string]any{
		"messageId": persistedID, "conversationId": conversationID,
		"transportStatus": status, "mock": sendRes.Mock, "error": errAny,
	})
}

func memberOf(members []byte, uid string) bool {
	var arr []string
	if json.Unmarshal(members, &arr) != nil {
		return false
	}
	for _, m := range arr {
		if m == uid {
			return true
		}
	}
	return false
}
