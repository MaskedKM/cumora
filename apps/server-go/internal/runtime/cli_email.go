// runtime 包 email 面 —— cli.ts 的 cmdEmail 六子命令(whoami/contacts/
// inbox/show/send/reply)+ listEmailContacts/listAgentEmailThreads 收件人
// 与线程面。Provider/持久层复用 internal/email(已与 email.ts 对齐)。
package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/email"
)

func (s *Service) cliCmdEmail(ctx context.Context, parsed cliParsed) cliResult {
	sub := ""
	if len(parsed.positional) > 0 {
		sub = parsed.positional[0]
	}
	if sub == "" {
		return cliErr(strings.Join([]string{
			"usage:",
			`  email send --to <addr|id>[,<addr|id>...] [--cc <...>] --subject "..." --body "..." [--attach <path>[,<path>...]] [--as <id>]`,
			`  email reply <message_id> --body "..." [--cc <addr|id>...] [--attach <path>[,<path>...]] [--as <id>]`,
			`  email inbox [--unread] [--limit N] [--as <id>]`,
			`  email show <conversation_id> [--tail N] [--as <id>]`,
			`  email contacts [<query>] [--as <id>]   (or just: cumora contacts [<query>])`,
			`  email whoami [--as <id>]   — your own address`,
		}, "\n"))
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErr(err.Error())
	}
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErr(err.Error())
	}
	if companyID == "" {
		return cliErr(fmt.Sprintf("unknown agent %s (no company)", me))
	}
	switch sub {
	case "whoami":
		return s.cliEmailWhoami(ctx, me)
	case "contacts":
		return s.cliEmailContacts(ctx, parsed, me, companyID, parsed.flagTruey("json"))
	case "inbox":
		return s.cliEmailInbox(ctx, parsed, me, companyID)
	case "show":
		return s.cliEmailShow(ctx, parsed, me, companyID)
	case "send":
		return s.cliEmailSend(ctx, parsed, me, companyID)
	case "reply":
		return s.cliEmailReplyCmd(ctx, parsed, me, companyID)
	default:
		return cliErr(fmt.Sprintf("unknown email subcommand: %s", sub))
	}
}

// cliEmailWhoami:自己的地址(惰铸)。
func (s *Service) cliEmailWhoami(ctx context.Context, me string) cliResult {
	addr := s.cliEnsureAgentAddress(ctx, me)
	if addr == nil {
		if email.RootDomain() == "" {
			return cliErr("email feature not configured (set EMAIL_DOMAIN)")
		}
		return cliErr(fmt.Sprintf("no email address available for %s (not an agent, or company missing)", me))
	}
	return cliOK(fmt.Sprintf("%s <%s>", addr.DisplayName, addr.Email))
}

// cliEnsureAgentAddress:ensureAgentAddress 等价 —— 先按 agent 找公司,再
// 惰铸地址;非 agent / 无公司 → nil。
func (s *Service) cliEnsureAgentAddress(ctx context.Context, agentID string) *email.ParticipantAddr {
	var companyID string
	err := s.DB.QueryRowContext(ctx, `
		SELECT company_id FROM participants
		 WHERE id = $1 AND kind = 'agent' AND departed_at IS NULL LIMIT 1`, agentID).Scan(&companyID)
	if err != nil {
		return nil
	}
	addr, _ := email.EnsureParticipantAddress(ctx, s.DB, agentID, companyID)
	return addr
}

/* ───────────── contacts ───────────── */

// cliEmailContact:key 序 = participantId, name, address, kind[, role]。
// role 仅 agent 行携带(列可为 null);human/external 根本没有该键。
type cliEmailContact struct {
	ParticipantID *string
	Name          string
	Address       string
	Kind          string
	Role          *string
	HasRole       bool
}

// jsonStringNoEscape:编码字符串但不做 &<> 的 HTML 转义(外层
// SetEscapeHTML(false) 不会解开内层 json.Marshal 的转义序列)。
func jsonStringNoEscape(s string) []byte {
	var buf bytes.Buffer
	enc := newJSONEncoderNoEscape(&buf)
	_ = enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func (c cliEmailContact) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteString(`{"participantId":`)
	if c.ParticipantID == nil {
		b.WriteString("null")
	} else {
		b.Write(jsonStringNoEscape(*c.ParticipantID))
	}
	b.WriteString(`,"name":`)
	b.Write(jsonStringNoEscape(c.Name))
	b.WriteString(`,"address":`)
	b.Write(jsonStringNoEscape(c.Address))
	b.WriteString(`,"kind":`)
	b.Write(jsonStringNoEscape(c.Kind))
	if c.HasRole {
		b.WriteString(`,"role":`)
		if c.Role == nil {
			b.WriteString("null")
		} else {
			b.Write(jsonStringNoEscape(*c.Role))
		}
	}
	b.WriteString("}")
	return []byte(b.String()), nil
}

func (s *Service) cliListEmailContacts(ctx context.Context, companyID, viewerID, query string) []cliEmailContact {
	q := strings.ToLower(strings.TrimSpace(query))
	matches := func(c cliEmailContact) bool {
		if q == "" {
			return true
		}
		if strings.Contains(strings.ToLower(c.Name), q) || strings.Contains(strings.ToLower(c.Address), q) {
			return true
		}
		if c.ParticipantID != nil && strings.Contains(strings.ToLower(*c.ParticipantID), q) {
			return true
		}
		if c.Role != nil && strings.Contains(strings.ToLower(*c.Role), q) {
			return true
		}
		return false
	}
	out := []cliEmailContact{}
	// 1. 同租户 agents(排除自己)——email 未铸的用确定性地址补上。
	rows, err := s.DB.QueryContext(ctx, `
		SELECT p.id, p.name, p.email, p.role, c.slug
		  FROM participants p JOIN companies c ON c.id = p.company_id
		 WHERE p.company_id = $1 AND p.kind = 'agent' AND p.departed_at IS NULL
		   AND p.id <> $2
		 ORDER BY p.name ASC`, companyID, viewerID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, name, slug string
			var emailCol, role sql.NullString
			if rows.Scan(&id, &name, &emailCol, &role, &slug) != nil {
				continue
			}
			address := ""
			switch {
			case emailCol.Valid && emailCol.String != "":
				address = emailCol.String
			default:
				if minted := email.ComputeAgentAddress(id, slug); minted != "" {
					address = minted
				} else {
					address = "(chat only)"
				}
			}
			c := cliEmailContact{ParticipantID: &id, Name: name, Address: address, Kind: "agent", HasRole: true}
			if role.Valid {
				c.Role = &role.String
			}
			if matches(c) {
				out = append(out, c)
			}
		}
		rows.Close()
	}
	// 2. 工作区 humans(auth email)。
	humans, err := s.DB.QueryContext(ctx, `
		SELECT u.id, u.display_name, u.email
		  FROM users u JOIN company_members cm ON cm.user_id = u.id
		 WHERE cm.company_id = $1 AND u.email IS NOT NULL
		 ORDER BY u.display_name ASC`, companyID)
	if err == nil {
		defer humans.Close()
		for humans.Next() {
			var id, displayName, mail string
			if humans.Scan(&id, &displayName, &mail) != nil {
				continue
			}
			c := cliEmailContact{ParticipantID: &id, Name: displayName, Address: mail, Kind: "human"}
			if matches(c) {
				out = append(out, c)
			}
		}
		humans.Close()
	}
	// 3. 外部往来地址(带过滤放大检索面)。
	limit := 30
	if q != "" {
		limit = 200
	}
	ext, err := s.DB.QueryContext(ctx, `
		SELECT address, display_name, message_count FROM email_contacts
		 WHERE company_id = $1 ORDER BY last_seen_at DESC LIMIT $2`, companyID, limit)
	if err == nil {
		defer ext.Close()
		for ext.Next() {
			var address string
			var displayName sql.NullString
			var cnt int
			if ext.Scan(&address, &displayName, &cnt) != nil {
				continue
			}
			name := address
			if displayName.Valid && displayName.String != "" {
				name = displayName.String
			}
			c := cliEmailContact{ParticipantID: nil, Name: name, Address: address, Kind: "external"}
			if matches(c) {
				out = append(out, c)
			}
		}
		ext.Close()
	}
	return out
}

// cliEmailContacts:目录渲染。宽度按各列最长项驱动(带 cap),空结果时
// 显式区分"未配置"与"无匹配"(agent 会拿这个信号去问用户要地址)。
func (s *Service) cliEmailContacts(ctx context.Context, parsed cliParsed, me, companyID string, asJSON bool) cliResult {
	query := ""
	if len(parsed.positional) > 1 {
		query = strings.TrimSpace(parsed.positional[1])
	}
	list := s.cliListEmailContacts(ctx, companyID, me, query)
	if asJSON {
		txt, jerr := cliJSONStringify(list)
		if jerr != nil {
			return cliErrCode(fmt.Sprintf("error: %v", jerr), 2)
		}
		return cliOK(txt)
	}
	if len(list) == 0 {
		if email.RootDomain() == "" {
			return cliOK("(email feature not configured — set EMAIL_DOMAIN to enable)")
		}
		if query != "" {
			return cliOK(fmt.Sprintf("(no contacts match %q. If the user named someone you don't recognize, ASK them for the email address before guessing — don't silently skip the task.)", query))
		}
		return cliOK("(no email contacts yet — invite someone or wait for inbound mail)")
	}
	const kindW = 8
	nameW := 12
	for _, c := range list {
		if n := utf16Len(c.Name); n > nameW {
			nameW = n
		}
	}
	if nameW > 40 {
		nameW = 40
	}
	roleW := 4
	for _, c := range list {
		r := ""
		if c.Role != nil {
			r = *c.Role
		}
		if n := utf16Len(r); n > roleW {
			roleW = n
		}
	}
	if roleW > 24 {
		roleW = 24
	}
	addrW := 20
	for _, c := range list {
		if n := utf16Len(c.Address); n > addrW {
			addrW = n
		}
	}
	if addrW > 60 {
		addrW = 60
	}
	lines := []string{
		utf16PadEnd("kind", kindW) + " " + utf16PadEnd("name", nameW) + "  " + utf16PadEnd("role", roleW) + "  " + utf16PadEnd("address", addrW) + "  id",
		strings.Repeat("-", kindW+1+nameW+2+roleW+2+addrW+2+6),
	}
	for _, c := range list {
		roleTxt := "—"
		if c.Role != nil {
			roleTxt = *c.Role
		}
		pid := "—"
		if c.ParticipantID != nil {
			pid = *c.ParticipantID
		}
		lines = append(lines,
			utf16PadEnd(c.Kind, kindW)+" "+utf16PadEnd(utf16Slice(c.Name, nameW), nameW)+"  "+
				utf16PadEnd(utf16Slice(roleTxt, roleW), roleW)+"  "+
				utf16PadEnd(utf16Slice(c.Address, addrW), addrW)+"  "+pid)
	}
	return cliOK(strings.Join(lines, "\n"))
}

/* ───────────── inbox ───────────── */

// cliEmailThreadRow:--json 键序 = SELECT 列序。
type cliEmailThreadRow struct {
	ConversationID string  `json:"conversation_id"`
	Title          string  `json:"title"`
	UpdatedAt      string  `json:"updated_at"`
	UnreadCount    int     `json:"unread_count"`
	LastSubject    *string `json:"last_subject"`
	LastFrom       *string `json:"last_from"`
	LastAt         *string `json:"last_at"`
	LastBody       *string `json:"last_body"`
}

func (s *Service) cliEmailInbox(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	unread := parsed.flagTruey("unread")
	limit, err := cliMsgFlagNum(parsed, "limit", 20, 50)
	if err != nil {
		return cliErr(err.Error())
	}
	rows, qerr := s.DB.QueryContext(ctx, `
		WITH my_threads AS (
		   SELECT c.id, c.title, c.updated_at
		     FROM conversations c
		    WHERE c.kind = 'email'
		      AND c.company_id = $1
		      AND c.members @> to_jsonb(ARRAY[$2::text])
		 ),
		 last_msg AS (
		   SELECT DISTINCT ON (em.conversation_id)
		          em.conversation_id, em.subject, em.from_addr,
		          m.body, m.created_at AS at
		     FROM email_messages em
		     JOIN messages m ON m.id = em.message_id
		    WHERE em.company_id = $1
		    ORDER BY em.conversation_id, em.created_at DESC
		 ),
		 unread AS (
		   SELECT m.conversation_id, COUNT(*)::int AS n
		     FROM messages m
		     LEFT JOIN conversation_reads r
		            ON r.conversation_id = m.conversation_id AND r.user_id = $2
		    WHERE m.kind = 'email'
		      AND m.company_id = $1
		      AND m.author_id <> $2
		      AND (r.last_read_at IS NULL OR m.created_at > r.last_read_at)
		    GROUP BY m.conversation_id
		 )
		 SELECT t.id AS conversation_id, t.title, t.updated_at::text,
		        COALESCE(u.n, 0) AS unread_count,
		        l.subject AS last_subject, l.from_addr AS last_from,
		        l.at::text AS last_at, l.body AS last_body
		   FROM my_threads t
		   LEFT JOIN last_msg l ON l.conversation_id = t.id
		   LEFT JOIN unread   u ON u.conversation_id = t.id
		  WHERE NOT $3 OR COALESCE(u.n, 0) > 0
		  ORDER BY t.updated_at DESC
		  LIMIT $4`, companyID, me, unread, limit)
	if qerr != nil {
		return cliErrCode(fmt.Sprintf("error: %v", qerr), 2)
	}
	defer rows.Close()
	threads := []cliEmailThreadRow{}
	for rows.Next() {
		var t cliEmailThreadRow
		if rows.Scan(&t.ConversationID, &t.Title, &t.UpdatedAt, &t.UnreadCount,
			&t.LastSubject, &t.LastFrom, &t.LastAt, &t.LastBody) != nil {
			continue
		}
		threads = append(threads, t)
	}
	if parsed.flagTruey("json") {
		txt, jerr := cliJSONStringify(threads)
		if jerr != nil {
			return cliErrCode(fmt.Sprintf("error: %v", jerr), 2)
		}
		return cliOK(txt)
	}
	if len(threads) == 0 {
		if email.RootDomain() == "" {
			return cliOK("(email feature not configured — set EMAIL_DOMAIN to enable inbound + outbound)")
		}
		if unread {
			return cliOK(fmt.Sprintf("(no unread email for %s)", me))
		}
		return cliOK(fmt.Sprintf("(no email threads for %s yet)", me))
	}
	lines := []string{}
	for _, t := range threads {
		unreadTag := ""
		if t.UnreadCount > 0 {
			unreadTag = fmt.Sprintf(" ★%d", t.UnreadCount)
		}
		subject := "(no subject)"
		if t.LastSubject != nil && *t.LastSubject != "" {
			subject = *t.LastSubject
		} else if t.Title != "" {
			subject = t.Title
		}
		from := "?"
		if t.LastFrom != nil {
			from = *t.LastFrom
		}
		snippet := ""
		if t.LastBody != nil {
			snippet = utf16Slice(*t.LastBody, 240)
		}
		snippet = emailSnippetFlatten.ReplaceAllString(snippet, " \\n ")
		at := ""
		if t.LastAt != nil && *t.LastAt != "" {
			if ts, ok := parseJSDate(*t.LastAt); ok {
				at = strings.ReplaceAll(ts.UTC().Format("2006-01-02T15:04:05.000Z"), "T", " ")[:16]
			}
		}
		lines = append(lines, fmt.Sprintf("# %s%s  [%s]", t.ConversationID, unreadTag, at))
		lines = append(lines, "  from:    "+from)
		lines = append(lines, "  subject: "+subject)
		if snippet != "" {
			lines = append(lines, "  body:    "+snippet)
		}
		lines = append(lines, "")
	}
	lines = append(lines, "run `cumora email show <conversation_id>` to read the full thread, then `cumora email reply <message_id> --body \"...\"` to respond. `cumora ack <conversation_id>` clears unread state.")
	return cliOK(strings.Join(lines, "\n"))
}

var emailSnippetFlatten = regexp.MustCompile(`\n+`)

/* ───────────── show ───────────── */

// cliEmailShowMsg:--json 键序 = SELECT 列序。
type cliEmailShowMsg struct {
	ID              string    `json:"id"`
	CreatedAt       string    `json:"created_at"`
	Body            string    `json:"body"`
	FromAddr        string    `json:"from_addr"`
	ToAddrs         cliStrArr `json:"to_addrs"`
	CCAddrs         cliStrArr `json:"cc_addrs"`
	Subject         string    `json:"subject"`
	SmtpMessageID   *string   `json:"smtp_message_id"`
	InReplyTo       *string   `json:"in_reply_to"`
	Direction       string    `json:"direction"`
	TransportStatus string    `json:"transport_status"`
}

func (s *Service) cliEmailShow(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	convoID := ""
	if len(parsed.positional) > 1 {
		convoID = parsed.positional[1]
	}
	if convoID == "" {
		return cliErr("usage: email show <conversation_id> [--tail N]")
	}
	tail, err := cliMsgFlagNum(parsed, "tail", 10, 50)
	if err != nil {
		return cliErr(err.Error())
	}
	var members cliStrArr
	var title string
	qerr := s.DB.QueryRowContext(ctx, `
		SELECT members, title FROM conversations
		 WHERE id = $1 AND company_id = $2 AND kind = 'email' LIMIT 1`, convoID, companyID).Scan(&members, &title)
	if qerr != nil {
		return cliErr(fmt.Sprintf("unknown email thread %s", convoID))
	}
	if !containsString(members, me) {
		return cliErr(fmt.Sprintf("%s is not a member of %s", me, convoID))
	}
	rows, qerr := s.DB.QueryContext(ctx, `
		SELECT m.id, m.created_at::text, m.body,
		       em.from_addr, em.to_addrs, em.cc_addrs, em.subject,
		       em.smtp_message_id, em.in_reply_to, em.direction, em.transport_status
		  FROM messages m
		  JOIN email_messages em ON em.message_id = m.id
		 WHERE m.conversation_id = $1
		 ORDER BY m.sequence DESC
		 LIMIT $2`, convoID, tail)
	if qerr != nil {
		return cliErrCode(fmt.Sprintf("error: %v", qerr), 2)
	}
	defer rows.Close()
	msgs := []cliEmailShowMsg{}
	for rows.Next() {
		var m cliEmailShowMsg
		if rows.Scan(&m.ID, &m.CreatedAt, &m.Body, &m.FromAddr, &m.ToAddrs, &m.CCAddrs,
			&m.Subject, &m.SmtpMessageID, &m.InReplyTo, &m.Direction, &m.TransportStatus) != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	// reverse(TS msgs.reverse())
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	if parsed.flagTruey("json") {
		txt, jerr := cliJSONStringify(struct {
			Thread   string            `json:"thread"`
			Title    string            `json:"title"`
			Messages []cliEmailShowMsg `json:"messages"`
		}{convoID, title, msgs})
		if jerr != nil {
			return cliErrCode(fmt.Sprintf("error: %v", jerr), 2)
		}
		return cliOK(txt)
	}
	if len(msgs) == 0 {
		return cliOK(fmt.Sprintf("(thread %s has no email messages)", convoID))
	}
	lines := []string{fmt.Sprintf("thread %s  \"%s\"", convoID, title), ""}
	for _, m := range msgs {
		at := ""
		if ts, ok := parseJSDate(m.CreatedAt); ok {
			at = strings.ReplaceAll(ts.UTC().Format("2006-01-02T15:04:05.000Z"), "T", " ")[:16]
		}
		arrow := "↑ out"
		if m.Direction == "in" {
			arrow = "↓ in"
		}
		lines = append(lines, fmt.Sprintf("────  [%s]  %s  %s  %s", m.ID, arrow, m.TransportStatus, at))
		lines = append(lines, "from:    "+m.FromAddr)
		if len(m.ToAddrs) > 0 {
			lines = append(lines, "to:      "+strings.Join(m.ToAddrs, ", "))
		}
		if len(m.CCAddrs) > 0 {
			lines = append(lines, "cc:      "+strings.Join(m.CCAddrs, ", "))
		}
		lines = append(lines, "subject: "+m.Subject)
		if m.InReplyTo != nil {
			lines = append(lines, fmt.Sprintf("in-reply-to: <%s>", *m.InReplyTo))
		}
		lines = append(lines, "")
		lines = append(lines, m.Body)
		lines = append(lines, "")
	}
	lines = append(lines, fmt.Sprintf("reply with `cumora email reply %s --body \"...\"`.", msgs[len(msgs)-1].ID))
	return cliOK(strings.Join(lines, "\n"))
}

/* ───────────── send ───────────── */

// cliEmailLoadedAttachment:loadEmailAttachmentFromPath 的产物。
type cliEmailLoadedAttachment struct {
	Filename   string
	MimeType   string
	SizeBytes  int64
	Base64     string
	StorageKey string
	PublicURL  string
}

// nodeFsErrorText:把 Go 的 os 错误翻成 Node fs 的报错文本,dual-run 才能
// 逐字节一致(ENOENT/EACCES/EISDIR 三类;其余回退原始文本)。
func nodeFsErrorText(err error, path string) string {
	switch {
	case os.IsNotExist(err):
		return fmt.Sprintf("ENOENT: no such file or directory, open '%s'", path)
	case os.IsPermission(err):
		return fmt.Sprintf("EACCES: permission denied, open '%s'", path)
	default:
		return err.Error()
	}
}

func (s *Service) cliLoadEmailAttachment(path string) (cliEmailLoadedAttachment, error) {
	const maxBytes = 20 * 1024 * 1024
	buf, err := os.ReadFile(path)
	if err != nil {
		return cliEmailLoadedAttachment{}, fmt.Errorf("%s", nodeFsErrorText(err, path))
	}
	if len(buf) == 0 {
		return cliEmailLoadedAttachment{}, fmt.Errorf("empty file: %s", path)
	}
	if int64(len(buf)) > maxBytes {
		return cliEmailLoadedAttachment{}, fmt.Errorf("file too large: %d bytes (max %d)", len(buf), maxBytes)
	}
	filename := filepath.Base(path)
	ext := "bin"
	if i := strings.LastIndex(filename, "."); i >= 0 && i+1 < len(filename) {
		ext = strings.ToLower(filename[i+1:])
	}
	mimeType := extToMime(ext)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	storageKey := fmt.Sprintf("email-attachments/%s.%s", randHex32(), ext)
	url, err := cliStoragePut(storageKey, buf)
	if err != nil {
		return cliEmailLoadedAttachment{}, err
	}
	return cliEmailLoadedAttachment{
		Filename:   filename,
		MimeType:   mimeType,
		SizeBytes:  int64(len(buf)),
		Base64:     base64.StdEncoding.EncodeToString(buf),
		StorageKey: storageKey,
		PublicURL:  url,
	}, nil
}

func splitCommaList(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// cliEmailResolvedAddr:resolveEmailRecipient 的产物。
type cliEmailResolvedAddr struct {
	Addr string
	Name *string
}

func (s *Service) cliResolveEmailRecipient(ctx context.Context, raw, companyID string) (cliEmailResolvedAddr, bool) {
	if addr, name, ok := email.ResolveRecipient(ctx, s.DB, raw, companyID); ok {
		var np *string
		if name != "" {
			np = &name
		}
		return cliEmailResolvedAddr{Addr: addr, Name: np}, true
	}
	return cliEmailResolvedAddr{}, false
}

func (s *Service) cliEmailSend(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	toRaw := parsed.flagStrOr("to", "")
	ccRaw := parsed.flagStrOr("cc", "")
	subject := email.SanitizeSubject(cliUnescapeChat(parsed.flagStrOr("subject", "")))
	body := strings.TrimSpace(cliUnescapeChat(parsed.flagStrOr("body", "")))
	attachRaw := parsed.flagStrOr("attach", "")
	if toRaw == "" || subject == "" || body == "" {
		return cliErr(`usage: email send --to <addr|id>[,...] [--cc <...>] --subject "..." --body "..." [--attach <path>[,<path>...]]`)
	}
	var loaded []cliEmailLoadedAttachment
	for _, p := range splitCommaList(attachRaw) {
		att, err := s.cliLoadEmailAttachment(p)
		if err != nil {
			return cliErr(fmt.Sprintf("attachment %s: %s", p, err.Error()))
		}
		loaded = append(loaded, att)
	}
	sender := s.cliEnsureAgentAddress(ctx, me)
	if sender == nil {
		return cliErr("agent has no email address (EMAIL_DOMAIN unset or company missing)")
	}

	var toResolved, ccResolved []cliEmailResolvedAddr
	for _, t := range splitCommaList(toRaw) {
		r, ok := s.cliResolveEmailRecipient(ctx, t, companyID)
		if !ok {
			return cliErr(fmt.Sprintf("can't resolve recipient: %s", t))
		}
		toResolved = append(toResolved, r)
	}
	for _, c := range splitCommaList(ccRaw) {
		r, ok := s.cliResolveEmailRecipient(ctx, c, companyID)
		if !ok {
			return cliErr(fmt.Sprintf("can't resolve cc: %s", c))
		}
		ccResolved = append(ccResolved, r)
	}
	if len(toResolved) == 0 {
		return cliErr("at least one --to recipient required")
	}

	// 同租户的 agent 收件人成为会话成员。
	memberSet := map[string]bool{me: true}
	memberOrder := []string{me}
	for _, r := range append(append([]cliEmailResolvedAddr{}, toResolved...), ccResolved...) {
		var inHouse string
		if s.DB.QueryRowContext(ctx, `
			SELECT id FROM participants
			 WHERE LOWER(email) = $1 AND company_id = $2 AND departed_at IS NULL LIMIT 1`,
			strings.ToLower(r.Addr), companyID).Scan(&inHouse) == nil && !memberSet[inHouse] {
			memberSet[inHouse] = true
			memberOrder = append(memberOrder, inHouse)
		}
	}

	messageID := email.MintMessageId()
	convoID, _, err := email.FindOrCreateEmailConversation(ctx, s.DB, companyID, "", nil, subject, memberOrder)
	if err != nil {
		return cliErrCode(fmt.Sprintf("error: %v", err), 2)
	}

	fromLine := email.FormatAddress(sender.Email, sender.DisplayName)
	toLines := make([]string, 0, len(toResolved))
	for _, r := range toResolved {
		name := ""
		if r.Name != nil {
			name = *r.Name
		}
		toLines = append(toLines, email.FormatAddress(r.Addr, name))
	}
	ccLines := make([]string, 0, len(ccResolved))
	for _, r := range ccResolved {
		name := ""
		if r.Name != nil {
			name = *r.Name
		}
		ccLines = append(ccLines, email.FormatAddress(r.Addr, name))
	}
	sendAttachments := make([]email.Attachment, 0, len(loaded))
	for _, a := range loaded {
		sendAttachments = append(sendAttachments, email.Attachment{
			Filename: a.Filename, MimeType: a.MimeType, Base64: a.Base64,
		})
	}
	sendRes := email.SendViaProvider(ctx, email.SendArgs{
		From:          fromLine,
		To:            toLines,
		CC:            ccLines,
		Subject:       subject,
		Text:          body,
		MessageID:     messageID,
		AutoSubmitted: "auto-generated",
		Attachments:   sendAttachments,
	})

	transportStatus := "failed"
	if sendRes.OK {
		transportStatus = "sent"
	}
	persistAttachments := make([]email.PersistAttachment, 0, len(loaded))
	for _, a := range loaded {
		key := a.StorageKey
		persistAttachments = append(persistAttachments, email.PersistAttachment{
			Filename: a.Filename, MimeType: a.MimeType, SizeBytes: a.SizeBytes, StorageKey: &key,
		})
	}
	smtpID := sendRes.SmtpMessageID
	if smtpID == "" {
		smtpID = messageID
	}
	persistedID, _, perr := email.PersistEmailMessage(ctx, s.DB, email.PersistArgs{
		ConversationID:  convoID,
		CompanyID:       companyID,
		AuthorID:        me,
		Direction:       "out",
		TransportStatus: transportStatus,
		TransportError:  sendRes.Error,
		SmtpMessageID:   smtpID,
		Subject:         subject,
		FromAddr:        fromLine,
		ToAddrs:         toLines,
		CCAddrs:         ccLines,
		Body:            body,
		AutoSubmitted:   true,
		Attachments:     persistAttachments,
	})
	if perr != nil {
		return cliErrCode(fmt.Sprintf("error: %v", perr), 2)
	}

	if !sendRes.OK {
		return cliErrCode(fmt.Sprintf("email persisted as failed: %s · message_id=%s", sendRes.Error, persistedID), 1)
	}
	mockTag := ""
	if sendRes.Mock {
		mockTag = " (mock — no real send)"
	}
	toAddrs := make([]string, 0, len(toResolved))
	for _, r := range toResolved {
		toAddrs = append(toAddrs, r.Addr)
	}
	ccAddrs := make([]string, 0, len(ccResolved))
	for _, r := range ccResolved {
		ccAddrs = append(ccAddrs, r.Addr)
	}
	return cliOK(fmt.Sprintf("sent%s · %s · thread %s", mockTag, persistedID, convoID), cliSideEffect{
		"event":           "email.sent",
		"command":         "email send",
		"conversationId":  convoID,
		"messageId":       persistedID,
		"authorId":        me,
		"companyId":       companyID,
		"subject":         subject,
		"to":              toAddrs,
		"cc":              ccAddrs,
		"attachmentCount": len(loaded),
		"transportStatus": "sent",
		"mock":            sendRes.Mock,
		"visibleToUser":   true,
	})
}

/* ───────────── reply ───────────── */

var emailExtractAddrRe = regexp.MustCompile(`<([^>]+)>`)
var emailSubjectRePrefix = regexp.MustCompile(`(?i)^(re|fwd|fw)\s*:`)

func (s *Service) cliEmailReplyCmd(ctx context.Context, parsed cliParsed, me, companyID string) cliResult {
	replyTo := ""
	if len(parsed.positional) > 1 {
		replyTo = parsed.positional[1]
	}
	body := strings.TrimSpace(cliUnescapeChat(parsed.flagStrOr("body", "")))
	attachRaw := parsed.flagStrOr("attach", "")
	if replyTo == "" || body == "" {
		return cliErr(`usage: email reply <message_id> --body "..." [--cc <addr|id>...] [--attach <path>[,<path>...]]`)
	}
	var loaded []cliEmailLoadedAttachment
	for _, p := range splitCommaList(attachRaw) {
		att, err := s.cliLoadEmailAttachment(p)
		if err != nil {
			return cliErr(fmt.Sprintf("attachment %s: %s", p, err.Error()))
		}
		loaded = append(loaded, att)
	}
	// 原邮件行 + 会话上下文。
	var oConvo string
	var oSmtp sql.NullString
	var oRefs cliStrArr
	var oSubject, oFrom string
	var oTo, oCc cliStrArr
	err := s.DB.QueryRowContext(ctx, `
		SELECT conversation_id, smtp_message_id, references_chain,
		       subject, from_addr, to_addrs, cc_addrs
		  FROM email_messages WHERE message_id = $1 AND company_id = $2`,
		replyTo, companyID).Scan(&oConvo, &oSmtp, &oRefs, &oSubject, &oFrom, &oTo, &oCc)
	if err != nil {
		return cliErr(fmt.Sprintf("unknown email message %s", replyTo))
	}
	var members cliStrArr
	if s.DB.QueryRowContext(ctx, `SELECT members FROM conversations WHERE id = $1`, oConvo).Scan(&members) != nil ||
		!containsString(members, me) {
		return cliErr(fmt.Sprintf("%s is not a member of thread %s", me, oConvo))
	}
	sender := s.cliEnsureAgentAddress(ctx, me)
	if sender == nil {
		return cliErr("agent has no email address (EMAIL_DOMAIN unset or company missing)")
	}

	// reply-all 切分:TO = 原 From,CC = 原 To+Cc 去掉自己。
	toAddrs, ccFromOriginal := email.SplitReplyAddresses(oFrom, oTo, oCc, []string{sender.Email})
	if len(toAddrs) == 0 {
		return cliErr("no other recipients to reply to")
	}

	var ccResolved []cliEmailResolvedAddr
	for _, c := range splitCommaList(parsed.flagStrOr("cc", "")) {
		r, ok := s.cliResolveEmailRecipient(ctx, c, companyID)
		if !ok {
			return cliErr(fmt.Sprintf("can't resolve cc: %s", c))
		}
		ccResolved = append(ccResolved, r)
	}
	extractAddr := func(raw string) string {
		if m := emailExtractAddrRe.FindStringSubmatch(raw); m != nil {
			return strings.ToLower(m[1])
		}
		return strings.ToLower(raw)
	}
	ccSeen := map[string]bool{strings.ToLower(sender.Email): true}
	for _, t := range toAddrs {
		ccSeen[extractAddr(t)] = true
	}
	for _, c := range ccFromOriginal {
		ccSeen[extractAddr(c)] = true
	}
	ccCombined := append([]string{}, ccFromOriginal...)
	for _, r := range ccResolved {
		if ccSeen[strings.ToLower(r.Addr)] {
			continue
		}
		ccSeen[strings.ToLower(r.Addr)] = true
		name := ""
		if r.Name != nil {
			name = *r.Name
		}
		ccCombined = append(ccCombined, email.FormatAddress(r.Addr, name))
	}

	subject := email.SanitizeSubject("Re: " + oSubject)
	if emailSubjectRePrefix.MatchString(oSubject) {
		subject = email.SanitizeSubject(oSubject)
	}
	newReferences := []string{}
	newReferences = append(newReferences, oRefs...)
	if oSmtp.Valid && oSmtp.String != "" {
		newReferences = append(newReferences, oSmtp.String)
	}
	inReplyTo := ""
	if oSmtp.Valid && oSmtp.String != "" {
		inReplyTo = email.NormalizeMessageId(oSmtp.String)
	}
	messageID := email.MintMessageId()

	fromLine := email.FormatAddress(sender.Email, sender.DisplayName)
	sendAttachments := make([]email.Attachment, 0, len(loaded))
	for _, a := range loaded {
		sendAttachments = append(sendAttachments, email.Attachment{
			Filename: a.Filename, MimeType: a.MimeType, Base64: a.Base64,
		})
	}
	sendRes := email.SendViaProvider(ctx, email.SendArgs{
		From:          fromLine,
		To:            toAddrs,
		CC:            ccCombined,
		Subject:       subject,
		Text:          body,
		InReplyTo:     inReplyTo,
		References:    newReferences,
		MessageID:     messageID,
		AutoSubmitted: "auto-replied",
		Attachments:   sendAttachments,
	})

	transportStatus := "failed"
	if sendRes.OK {
		transportStatus = "sent"
	}
	persistAttachments := make([]email.PersistAttachment, 0, len(loaded))
	for _, a := range loaded {
		key := a.StorageKey
		persistAttachments = append(persistAttachments, email.PersistAttachment{
			Filename: a.Filename, MimeType: a.MimeType, SizeBytes: a.SizeBytes, StorageKey: &key,
		})
	}
	smtpID := sendRes.SmtpMessageID
	if smtpID == "" {
		smtpID = messageID
	}
	persistedID, _, perr := email.PersistEmailMessage(ctx, s.DB, email.PersistArgs{
		ConversationID:  oConvo,
		CompanyID:       companyID,
		AuthorID:        me,
		Direction:       "out",
		TransportStatus: transportStatus,
		TransportError:  sendRes.Error,
		SmtpMessageID:   smtpID,
		InReplyTo:       inReplyTo,
		References:      newReferences,
		Subject:         subject,
		FromAddr:        fromLine,
		ToAddrs:         toAddrs,
		CCAddrs:         ccCombined,
		Body:            body,
		AutoSubmitted:   true,
		Attachments:     persistAttachments,
	})
	if perr != nil {
		return cliErrCode(fmt.Sprintf("error: %v", perr), 2)
	}

	// 回复即已读(auto-ack)。
	_, _ = s.DB.ExecContext(ctx, `
		INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`, me, oConvo)

	if !sendRes.OK {
		return cliErrCode(fmt.Sprintf("email persisted as failed: %s · message_id=%s", sendRes.Error, persistedID), 1)
	}
	mockTag := ""
	if sendRes.Mock {
		mockTag = " (mock)"
	}
	return cliOK(fmt.Sprintf("replied%s · %s · thread %s", mockTag, persistedID, oConvo), cliSideEffect{
		"event":            "email.sent",
		"command":          "email reply",
		"conversationId":   oConvo,
		"messageId":        persistedID,
		"authorId":         me,
		"companyId":        companyID,
		"replyToMessageId": replyTo,
		"subject":          subject,
		"to":               toAddrs,
		"cc":               ccCombined,
		"attachmentCount":  len(loaded),
		"transportStatus":  "sent",
		"mock":             sendRes.Mock,
		"visibleToUser":    true,
	})
}

// cliCmdContacts:`contacts` 顶层别名 —— positional 前插 'contacts' 占位
// 对齐 cmdEmailContacts 的下标读取。
func (s *Service) cliCmdContacts(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErr(err.Error())
	}
	companyID, err := s.cliAgentCompany(ctx, me)
	if err != nil {
		return cliErr(err.Error())
	}
	if companyID == "" {
		return cliErr(fmt.Sprintf("unknown agent %s (no company)", me))
	}
	shimmed := parsed
	shimmed.positional = append([]string{"contacts"}, parsed.positional...)
	return s.cliEmailContacts(ctx, shimmed, me, companyID, parsed.flagTruey("json"))
}
