// email —— 邮件核心(#58 出站半边):地址解析/铸造、主题与 HTML 清洗、
// Provider 发送(Resend/mock)、messages+email_messages 原子持久化、
// email 会话线程(in-reply-to/references 归并 + 成员修补)。
// 对齐 server/src/email.ts 与 router.ts 的 resolveHttp* 三件。
package email

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	randv2 "math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

/* ───────────── 地址铸造(与 contacts 包同语义) ───────────── */

func RootDomain() string { return strings.Trim(strings.ToLower(os.Getenv("EMAIL_DOMAIN")), ".") }

var localPartAllowed = func(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
}
var slugAllowed = func(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
}

func sanitizePart(s string, allowed func(rune) bool, trimSet string, max int) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if allowed(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), trimSet)
	if runes := []rune(out); len(runes) > max {
		out = string(runes[:max])
	}
	return out
}

func SafeLocalPart(id string) string { return sanitizePart(id, localPartAllowed, "-_", 64) }
func SafeSlugPart(slug string) string {
	return sanitizePart(slug, slugAllowed, "-", 63)
}

func ComputeAgentAddress(agentID, companySlug string) string {
	dom := RootDomain()
	local := SafeLocalPart(agentID)
	slug := SafeSlugPart(companySlug)
	if dom == "" || local == "" || slug == "" {
		return ""
	}
	return local + "." + slug + "@" + dom
}

/* ───────────── 地址解析/格式化 ───────────── */

var addrAngleRe = regexp.MustCompile(`^\s*(?:"?([^"<]*?)"?\s*)?<([^>]+)>\s*$`)
var addrValidRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)

// ParseAddress 对齐 email.ts parseAddress:"Name <a@b>" 或裸地址;小写化;
// 垃圾输入返回 ok=false。
func ParseAddress(raw string) (addr string, name string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", false
	}
	if m := addrAngleRe.FindStringSubmatch(trimmed); m != nil {
		a := strings.ToLower(strings.TrimSpace(m[2]))
		n := strings.TrimSpace(m[1])
		if addrValidRe.MatchString(a) {
			if n == "" {
				return a, "", true
			}
			return a, n, true
		}
		return "", "", false
	}
	a := strings.ToLower(trimmed)
	if addrValidRe.MatchString(a) {
		return a, "", true
	}
	return "", "", false
}

var nameQuoteNeeded = regexp.MustCompile(`["<>,;:@()\[\]\\]`)

// utf16Len:JS string.length 的码元数。
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

// utf16Cap:按 UTF-16 码元截断(TS slice 语义;边界裂代理保整字,
// 与 TS 差半个字符,极端边缘)。
func Utf16Cap(s string, max int) string {
	n := 0
	for i, r := range s {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if n+units > max {
			return s[:i]
		}
		n += units
	}
	return s
}

// FormatAddress 对齐 formatAddress:有名则 "Name" <addr>(含会混淆解析器的
// 字符时加引号,内部引号转义)。
func FormatAddress(addr string, name string) string {
	if name == "" {
		return addr
	}
	safe := strings.ReplaceAll(name, `"`, `\"`)
	if nameQuoteNeeded.MatchString(name) {
		return `"` + safe + `" <` + addr + `>`
	}
	return safe + " <" + addr + ">"
}

/* ───────────── 主题/HTML 清洗 ───────────── */

// SanitizeSubject 对齐 sanitizeSubject:C0 控制+DEL→空格、零宽/方向标记/
// BOM→删除、空白折叠、200 字符截断。
func SanitizeSubject(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		switch r {
		case 0x200b, 0x200c, 0x200d, 0x200e, 0x200f, // 零宽
			0x202a, 0x202b, 0x202c, 0x202d, 0x202e, // 方向覆盖
			0x2060, 0xfeff: // word joiner / BOM
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	return Utf16Cap(out, 200)
}

var (
	dangerTags  = []string{"script", "style", "iframe", "object", "embed", "frame", "frameset", "applet", "svg", "math"}
	linkMetaRe  = regexp.MustCompile(`(?i)<(?:link|meta|base)\b[^>]*/?>`)
	commentRe   = regexp.MustCompile(`(?s)<!--.*?-->`)
	eventDqRe   = regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*"[^"]*"`)
	eventSqRe   = regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*'[^']*'`)
	eventBareRe = regexp.MustCompile(`(?i)\son[a-z]+\s*=\s*[^\s>]+`)
	// 捕获属性值尾(含引号内空格);危险 scheme 与 data:image 白名单
	// 判断在回调里做(RE2 无负向前瞻,对齐 TS 的 data:(?!image/…) 语义)。
	urlAttrRe = regexp.MustCompile(`(?i)\b(href|src|action|formaction|background|poster|xlink:href|data)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	srcdocRe  = regexp.MustCompile(`(?i)\bsrcdoc\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	dataImgRe = regexp.MustCompile(`^data:image/(png|jpeg|gif|webp|svg\+xml);`)
)

// dangerousScheme:javascript:/vbscript:/file: 恒危险;data: 仅白名单
// image MIME 之外危险。
func dangerousScheme(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "vbscript:") || strings.HasPrefix(lower, "file:") {
		return true
	}
	if strings.HasPrefix(lower, "data:") && !dataImgRe.MatchString(lower) {
		return true
	}
	return false
}

// SanitizeEmailHtml 对齐 sanitizeEmailHtml(保守:删不转)。RE2 无负向前瞻,
// data: 的 image 白名单在替换回调里判。
func SanitizeEmailHtml(raw string) string {
	if raw == "" {
		return ""
	}
	// 逐 tag 构造(TS 同):开标签允许属性(\b),配对体整删;再清残开标签。
	out := raw
	for _, tag := range dangerTags {
		bodyRe := regexp.MustCompile(`(?is)<` + tag + `\b[\s\S]*?</` + tag + `>`)
		out = bodyRe.ReplaceAllString(out, "")
		openRe := regexp.MustCompile(`(?i)<` + tag + `\b[^>]*/?>`)
		out = openRe.ReplaceAllString(out, "")
	}
	out = linkMetaRe.ReplaceAllString(out, "")
	out = commentRe.ReplaceAllString(out, "")
	out = eventDqRe.ReplaceAllString(out, "")
	out = eventSqRe.ReplaceAllString(out, "")
	out = eventBareRe.ReplaceAllString(out, "")
	out = urlAttrRe.ReplaceAllStringFunc(out, func(m string) string {
		sub := urlAttrRe.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		attr, value := sub[1], sub[2]
		if value == "" {
			value = sub[3]
		}
		if value == "" {
			value = sub[4]
		}
		if !dangerousScheme(value) {
			return m
		}
		if strings.Contains(sub[0], `="`) {
			return attr + `="#"`
		}
		if strings.Contains(sub[0], "='") {
			return attr + "='#'"
		}
		return attr + `="#"`
	})
	out = srcdocRe.ReplaceAllString(out, "")
	return out
}

// SplitReplyAddresses 对齐 splitReplyAddresses:TO=原 From,CC=原 To+Cc
// 去自去重(selfAddresses 为小写)。
func SplitReplyAddresses(originalFrom string, originalTo, originalCc, selfAddresses []string) (to []string, cc []string) {
	seen := map[string]bool{}
	for _, s := range selfAddresses {
		seen[strings.ToLower(s)] = true
	}
	if addr, name, ok := ParseAddress(originalFrom); ok && !seen[addr] {
		seen[addr] = true
		to = append(to, FormatAddress(addr, name))
	}
	for _, raw := range append(append([]string{}, originalTo...), originalCc...) {
		addr, name, ok := ParseAddress(raw)
		if !ok || seen[addr] {
			continue
		}
		seen[addr] = true
		cc = append(cc, FormatAddress(addr, name))
	}
	return to, cc
}

/* ───────────── Message-ID ───────────── */

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randUUID36:randomUUID() 的虚线形态(8-4-4-4-12)。
func randUUID36() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// MintMessageId 对齐 mintMessageId:<36 进制时间>-<22hex>@<EMAIL_DOMAIN>。
func MintMessageId() string {
	dom := RootDomain()
	if dom == "" {
		dom = "cumora.local"
	}
	return fmt.Sprintf("%s-%s@%s", strconv36(time.Now().UnixMilli()), randHex(11), dom)
}

func strconv36(n int64) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	var b []byte
	for n > 0 {
		b = append([]byte{digits[n%36]}, b...)
		n /= 36
	}
	return string(b)
}

var angleTrimRe = regexp.MustCompile(`^<+|>+$`)

// NormalizeMessageId 对齐 normalizeMessageId:剥 <>、小写。
func NormalizeMessageId(raw string) string {
	trimmed := strings.TrimSpace(angleTrimRe.ReplaceAllString(strings.TrimSpace(raw), ""))
	if trimmed == "" {
		return ""
	}
	return strings.ToLower(trimmed)
}

/* ───────────── DB:参与者地址 ───────────── */

type ParticipantAddr struct {
	ParticipantID string
	Email         string
	DisplayName   string
	Kind          string
}

// EnsureParticipantAddress 对齐 email.ts:读参与者+公司 slug,email 未铸则
// 竞态安全地铸确定性地址。
func EnsureParticipantAddress(ctx context.Context, db *sql.DB, participantID, companyID string) (*ParticipantAddr, bool) {
	var email sql.NullString
	var displayName, kind, slug string
	err := db.QueryRowContext(ctx, `
		SELECT p.email, p.name, p.kind, c.slug
		  FROM participants p JOIN companies c ON c.id = p.company_id
		 WHERE p.id = $1 AND p.company_id = $2 AND p.departed_at IS NULL LIMIT 1`,
		participantID, companyID).Scan(&email, &displayName, &kind, &slug)
	if err != nil {
		return nil, false
	}
	if email.Valid && email.String != "" {
		return &ParticipantAddr{ParticipantID: participantID, Email: email.String, DisplayName: displayName, Kind: kind}, true
	}
	minted := ComputeAgentAddress(participantID, slug)
	if minted == "" {
		return nil, false
	}
	_, _ = db.ExecContext(ctx,
		`UPDATE participants SET email = $2 WHERE id = $1 AND company_id = $3 AND email IS NULL`,
		participantID, minted, companyID)
	return &ParticipantAddr{ParticipantID: participantID, Email: minted, DisplayName: displayName, Kind: kind}, true
}

// FindParticipantByAddress:同公司按地址(小写)找活跃参与者。
func FindParticipantByAddress(ctx context.Context, db *sql.DB, addr, companyID string) (id, name, kind string, ok bool) {
	lc := strings.ToLower(strings.TrimSpace(addr))
	if lc == "" {
		return "", "", "", false
	}
	err := db.QueryRowContext(ctx, `
		SELECT id, name, kind FROM participants
		 WHERE company_id = $1 AND LOWER(email) = $2 AND departed_at IS NULL LIMIT 1`,
		companyID, lc).Scan(&id, &name, &kind)
	return id, name, kind, err == nil
}

// FindUserInCompanyByAuthEmail:auth email + 成员资格双守卫。
func FindUserInCompanyByAuthEmail(ctx context.Context, db *sql.DB, authEmail, companyID string) (id, displayName string, ok bool) {
	lc := strings.ToLower(strings.TrimSpace(authEmail))
	if lc == "" {
		return "", "", false
	}
	err := db.QueryRowContext(ctx, `
		SELECT u.id, u.display_name FROM users u
		 JOIN company_members cm ON cm.user_id = u.id
		WHERE LOWER(u.email) = $1 AND cm.company_id = $2 LIMIT 1`,
		lc, companyID).Scan(&id, &displayName)
	return id, displayName, err == nil
}

// RecordExternalContact 对齐 recordExternalContact upsert。
func RecordExternalContact(ctx context.Context, db *sql.DB, companyID, address string, displayName *string) {
	lc := strings.ToLower(strings.TrimSpace(address))
	if lc == "" {
		return
	}
	_, _ = db.ExecContext(ctx, `
		INSERT INTO email_contacts (company_id, address, display_name, message_count, last_seen_at)
		VALUES ($1, $2, $3, 1, NOW())
		ON CONFLICT (company_id, address) DO UPDATE
		  SET message_count = email_contacts.message_count + 1,
		      last_seen_at = NOW(),
		      display_name = COALESCE(EXCLUDED.display_name, email_contacts.display_name)`,
		companyID, lc, displayName)
}

// FindEmailConversationByMessageIds:任一 id 命中即返回其会话(最近优先)。
func FindEmailConversationByMessageIds(ctx context.Context, db *sql.DB, messageIDs []string, companyID string) (string, bool) {
	norm := make([]string, 0, len(messageIDs))
	for _, m := range messageIDs {
		if n := NormalizeMessageId(m); n != "" {
			norm = append(norm, n)
		}
	}
	if len(norm) == 0 {
		return "", false
	}
	var convID string
	err := db.QueryRowContext(ctx, `
		SELECT conversation_id FROM email_messages
		 WHERE company_id = $1 AND LOWER(smtp_message_id) = ANY($2::text[])
		 ORDER BY created_at DESC LIMIT 1`, companyID, norm).Scan(&convID)
	return convID, err == nil
}

/* ───────────── Provider(Resend / mock) ───────────── */

type Attachment struct {
	Filename string
	MimeType string
	Base64   string
	Path     string
}

type SendArgs struct {
	From          string
	To            []string
	CC            []string
	BCC           []string
	Subject       string
	Text          string
	HTML          string
	InReplyTo     string
	References    []string
	MessageID     string
	AutoSubmitted string // 'auto-replied' | 'auto-generated' | ''
	ReplyTo       string
	Attachments   []Attachment
}

type ProviderSendResult struct {
	OK            bool
	SmtpMessageID string
	Error         string
	Mock          bool
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

// SendViaProvider 对齐 email.ts:RESEND_API_KEY 空 = mock(EMAIL_MOCK_FAIL_RATE
// 注入失败);否则 POST api.resend.com/emails,In-Reply-To/References 经
// headers 字段,Message-ID 显式下发保持线程一致。永不 panic,失败进 Error。
func SendViaProvider(ctx context.Context, args SendArgs) ProviderSendResult {
	apiKey := os.Getenv("RESEND_API_KEY")
	if apiKey == "" {
		failRate := clamp01(parseFloatEnv("EMAIL_MOCK_FAIL_RATE"))
		if failRate > 0 && randv2.Float64() < failRate {
			return ProviderSendResult{OK: false, Error: fmt.Sprintf("mock_injected_failure (rate=%v)", failRate), Mock: true}
		}
		id := args.MessageID
		if id == "" {
			id = MintMessageId()
		}
		return ProviderSendResult{OK: true, SmtpMessageID: id, Mock: true}
	}
	body := map[string]any{
		"from": args.From, "to": args.To, "subject": args.Subject, "text": args.Text,
	}
	headers := map[string]string{}
	if args.MessageID != "" {
		headers["Message-ID"] = "<" + args.MessageID + ">"
	}
	if args.InReplyTo != "" {
		headers["In-Reply-To"] = "<" + args.InReplyTo + ">"
	}
	if len(args.References) > 0 {
		refs := make([]string, len(args.References))
		for i, r := range args.References {
			refs[i] = "<" + r + ">"
		}
		headers["References"] = strings.Join(refs, " ")
	}
	if args.AutoSubmitted != "" {
		headers["Auto-Submitted"] = args.AutoSubmitted
	}
	if len(headers) > 0 {
		body["headers"] = headers
	}
	if args.HTML != "" {
		body["html"] = args.HTML
	}
	if len(args.CC) > 0 {
		body["cc"] = args.CC
	}
	if len(args.BCC) > 0 {
		body["bcc"] = args.BCC
	}
	if args.ReplyTo != "" {
		body["reply_to"] = args.ReplyTo
	}
	atts := []map[string]string{}
	for _, a := range args.Attachments {
		if a.Base64 != "" {
			atts = append(atts, map[string]string{"filename": a.Filename, "content": a.Base64})
		} else if a.Path != "" {
			atts = append(atts, map[string]string{"filename": a.Filename, "path": a.Path})
		}
	}
	if len(atts) > 0 {
		body["attachments"] = atts
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", strings.NewReader(string(raw)))
	if err != nil {
		return ProviderSendResult{Error: "network: " + err.Error()}
	}
	req.Header.Set("authorization", "Bearer "+apiKey)
	req.Header.Set("content-type", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return ProviderSendResult{Error: "network: " + err.Error()}
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(res.Body, 400))
		return ProviderSendResult{Error: fmt.Sprintf("resend %d: %s", res.StatusCode, string(text))}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	id := args.MessageID
	if id == "" {
		id = MintMessageId()
	}
	return ProviderSendResult{OK: true, SmtpMessageID: id}
}

func parseFloatEnv(key string) float64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	var f float64
	for _, c := range v {
		if c == '.' || (c >= '0' && c <= '9') {
			continue
		}
		return 0
	}
	fmt.Sscanf(v, "%g", &f)
	return f
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

/* ───────────── 持久化 ───────────── */

type PersistAttachment struct {
	Filename   string
	MimeType   string
	SizeBytes  int64
	StorageKey *string
	Truncated  bool
}

type PersistArgs struct {
	ConversationID  string
	CompanyID       string
	AuthorID        string
	Direction       string // 'in' | 'out'
	TransportStatus string
	TransportError  string
	SmtpMessageID   string
	InReplyTo       string
	References      []string
	Subject         string
	FromAddr        string
	ToAddrs         []string
	CCAddrs         []string
	BCCAddrs        []string
	Body            string
	HTML            string
	RawSizeBytes    int64
	AutoSubmitted   bool
	Attachments     []PersistAttachment
}

func marshalStrings(xs []string) string {
	if len(xs) == 0 {
		return "[]" // TS (args.x ?? []).slice —— 恒数组
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

// PersistEmailMessage 对齐 email.ts:messages+email_messages 对行、出站失败
// 排 60s 后首试、email_attachments 行、CH_MESSAGE_NEW 带完整 email 头字段
// (attachments 附 publicUrl)。返回 messageId/sequence。
func PersistEmailMessage(ctx context.Context, db *sql.DB, args PersistArgs) (string, int, error) {
	// TS 恒写数组:(ccAddrs ?? []) 等。nil → 空数组。
	if args.ToAddrs == nil {
		args.ToAddrs = []string{}
	}
	if args.CCAddrs == nil {
		args.CCAddrs = []string{}
	}
	if args.BCCAddrs == nil {
		args.BCCAddrs = []string{}
	}
	messageID := "m-" + randUUID36()
	var sequence int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1`, args.ConversationID).Scan(&sequence); err != nil {
		return "", 0, err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		VALUES ($1, $2, $3, 'email', $4, $5, $6)`,
		messageID, args.ConversationID, args.AuthorID, args.Body, sequence, args.CompanyID); err != nil {
		return "", 0, err
	}
	var initialRetryAt any
	if args.Direction == "out" && args.TransportStatus == "failed" {
		initialRetryAt = time.Now().Add(60 * time.Second)
	}
	refs := make([]string, 0, len(args.References))
	for _, r := range args.References {
		if n := NormalizeMessageId(r); n != "" {
			refs = append(refs, n)
		}
	}
	trunc := Utf16Cap
	toInt64 := func(xs []string) []string {
		if len(xs) > 64 {
			return xs[:64]
		}
		return xs
	}
	var html, transportErr any
	if args.HTML != "" {
		html = args.HTML
	}
	if args.TransportError != "" {
		transportErr = args.TransportError
	}
	var rawSize any
	if args.RawSizeBytes > 0 {
		rawSize = args.RawSizeBytes
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO email_messages (
			message_id, conversation_id, company_id, direction, transport_status,
			transport_error, smtp_message_id, in_reply_to, references_chain,
			subject, from_addr, to_addrs, cc_addrs, bcc_addrs, html, raw_size_bytes,
			auto_submitted, next_retry_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12::jsonb,$13::jsonb,$14::jsonb,$15,$16,$17,$18)`,
		messageID, args.ConversationID, args.CompanyID, args.Direction, args.TransportStatus,
		transportErr, NormalizeMessageId(args.SmtpMessageID), NormalizeMessageId(args.InReplyTo),
		marshalStrings(refs), trunc(args.Subject, 1000), trunc(args.FromAddr, 320),
		marshalStrings(toInt64(args.ToAddrs)), marshalStrings(toInt64(args.CCAddrs)),
		marshalStrings(toInt64(args.BCCAddrs)), html, rawSize, args.AutoSubmitted, initialRetryAt); err != nil {
		return "", 0, err
	}
	type persistedAtt struct {
		ID         string
		Filename   string
		MimeType   string
		SizeBytes  int64
		StorageKey *string
		Truncated  bool
	}
	persisted := []persistedAtt{}
	for _, a := range args.Attachments {
		attID := "eatt-" + randHex(6)
		mime := trunc(a.MimeType, 120)
		if mime == "" {
			mime = "application/octet-stream"
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO email_attachments
			  (id, message_id, conversation_id, company_id, filename, mime_type, size_bytes, storage_key, truncated)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			attID, messageID, args.ConversationID, args.CompanyID,
			trunc(a.Filename, 200), mime, a.SizeBytes, a.StorageKey, a.Truncated); err != nil {
			return "", 0, err
		}
		persisted = append(persisted, persistedAtt{attID, trunc(a.Filename, 200), mime, a.SizeBytes, a.StorageKey, a.Truncated})
	}
	_, _ = db.ExecContext(ctx, `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, args.ConversationID)
	// 唤醒载荷:email 头字段全量(attachments 附 publicUrl;本地模式
	// /uploads/<key> 相对路径,与 TS storage.publicUrl 一致)。
	wakeAtts := []map[string]any{}
	for _, a := range persisted {
		var url any
		if a.StorageKey != nil && !a.Truncated {
			url = "/uploads/" + *a.StorageKey
		}
		wakeAtts = append(wakeAtts, map[string]any{
			"id": a.ID, "filename": a.Filename, "mimeType": a.MimeType,
			"sizeBytes": a.SizeBytes, "url": url, "truncated": a.Truncated,
		})
	}
	var transportErrAny any
	if args.TransportError != "" {
		transportErrAny = args.TransportError
	}
	events.MessageNew(ctx, args.CompanyID, args.ConversationID, map[string]any{
		"id": messageID, "conversationId": args.ConversationID, "authorId": args.AuthorID,
		"kind": "email", "body": args.Body, "sequence": sequence,
		"at": httpx.ISOms(time.Now()),
		"email": map[string]any{
			"subject": args.Subject, "from": args.FromAddr, "to": args.ToAddrs,
			"cc": args.CCAddrs, "direction": args.Direction,
			"transportStatus": args.TransportStatus, "transportError": transportErrAny,
			"smtpMessageId": NormalizeMessageId(args.SmtpMessageID),
			"inReplyTo":     NormalizeMessageId(args.InReplyTo),
			"hasHtml":       args.HTML != "", "autoSubmitted": args.AutoSubmitted,
			"attachments": wakeAtts,
		},
	})
	return messageID, sequence, nil
}

/* ───────────── 会话线程 ───────────── */

var subjectPrefixRe = regexp.MustCompile(`(?i)^\s*((re|fwd|fw)\s*:\s*)+`)

// FindOrCreateEmailConversation 对齐 email.ts:in-reply-to/references 命中
// 即复用(并做成员修补 union),否则新建 kind='email' 会话(标题剥 Re:/Fwd:)
func FindOrCreateEmailConversation(ctx context.Context, db *sql.DB, companyID string, inReplyTo string, references []string, subject string, memberIDs []string) (string, bool, error) {
	candidates := append([]string{inReplyTo}, references...)
	if existing, ok := FindEmailConversationByMessageIds(ctx, db, candidates, companyID); ok {
		_, err := db.ExecContext(ctx, `
			UPDATE conversations SET members = (
				SELECT to_jsonb(ARRAY(
					SELECT DISTINCT m FROM (
						SELECT jsonb_array_elements_text(members) AS m
						UNION
						SELECT unnest($2::text[]) AS m
					) u
				))
			) WHERE id = $1`, existing, memberIDs)
		return existing, false, err
	}
	cleanSubject := strings.TrimSpace(subjectPrefixRe.ReplaceAllString(subject, ""))
	if cleanSubject == "" {
		cleanSubject = "(no subject)"
	}
	cleanSubject = Utf16Cap(cleanSubject, 200)
	id := "email-" + randUUID36()[:12]
	seen := map[string]bool{}
	unique := []string{}
	for _, m := range memberIDs {
		if !seen[m] {
			seen[m] = true
			unique = append(unique, m)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO conversations (id, kind, title, members, company_id, topic)
		VALUES ($1, 'email', $2, $3::jsonb, $4, $5)`,
		id, cleanSubject, marshalStrings(unique), companyID, cleanSubject); err != nil {
		return "", false, err
	}
	return id, true, nil
}

/* ───────────── HTTP 收件人/附件解析(router.ts 对端) ───────────── */

// ResolveRecipient 对齐 resolveHttpRecipient:external: 拒收;先按地址;
// 再按参与者 id(agent→cumora 地址可惰铸,human→auth email);最后按
// users.id(需同租户成员)。
func ResolveRecipient(ctx context.Context, db *sql.DB, raw, companyID string) (addr string, name string, ok bool) {
	if strings.HasPrefix(raw, "external:") {
		return "", "", false
	}
	if a, n, ok2 := ParseAddress(raw); ok2 {
		return a, n, true
	}
	var pName string
	var pEmail sql.NullString
	var kind string
	err := db.QueryRowContext(ctx, `
		SELECT name, email, kind FROM participants
		 WHERE id = $1 AND company_id = $2 AND departed_at IS NULL LIMIT 1`,
		raw, companyID).Scan(&pName, &pEmail, &kind)
	if err == nil {
		if kind == "agent" {
			if pEmail.Valid && pEmail.String != "" {
				return pEmail.String, pName, true
			}
			if ensured, ok2 := EnsureParticipantAddress(ctx, db, raw, companyID); ok2 {
				return ensured.Email, ensured.DisplayName, true
			}
		}
		if kind == "human" {
			var uEmail sql.NullString
			if db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1 LIMIT 1`, raw).Scan(&uEmail) == nil && uEmail.Valid && uEmail.String != "" {
				return uEmail.String, pName, true
			}
		}
	}
	var dName, uEmail2 string
	err = db.QueryRowContext(ctx, `
		SELECT u.display_name, u.email FROM users u
		 JOIN company_members cm ON cm.user_id = u.id
		WHERE u.id = $1 AND cm.company_id = $2 LIMIT 1`, raw, companyID).Scan(&dName, &uEmail2)
	if err == nil {
		return uEmail2, dName, true
	}
	return "", "", false
}

// ResolvedAttachment 对齐 resolveHttpAttachments 的产物。
type ResolvedAttachment struct {
	Filename   string
	MimeType   string
	SizeBytes  int64
	StorageKey string
	PublicURL  string
}

// ResolveAttachments 对齐 resolveHttpAttachments:≤16 个、总 25MB、
// key+filename 必填;publicUrl = /uploads/<key>(本地模式与 TS 一致)。
func ResolveAttachments(raw []any) ([]ResolvedAttachment, string) {
	const maxAttachments = 16
	const maxTotal = 25 * 1024 * 1024
	if len(raw) > maxAttachments {
		return nil, fmt.Sprintf("too many attachments (%d > %d)", len(raw), maxAttachments)
	}
	out := []ResolvedAttachment{}
	var total int64
	for _, entry := range raw {
		a, _ := entry.(map[string]any)
		key, _ := a["key"].(string)
		filename, _ := a["filename"].(string)
		filename = Utf16Cap(filename, 200)
		mimeType, _ := a["mimeType"].(string)
		if runes := []rune(mimeType); len(runes) > 120 {
			mimeType = string(runes[:120])
		}
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		var sizeBytes float64
		if f, ok := a["sizeBytes"].(float64); ok {
			sizeBytes = f
		}
		if sizeBytes < 0 {
			sizeBytes = 0
		}
		if key == "" || filename == "" {
			return nil, "each attachment needs key + filename"
		}
		total += int64(sizeBytes)
		if total > maxTotal {
			return nil, fmt.Sprintf("attachments exceed %d bytes total", maxTotal)
		}
		out = append(out, ResolvedAttachment{
			Filename: filename, MimeType: mimeType, SizeBytes: int64(sizeBytes),
			StorageKey: key, PublicURL: "/uploads/" + key,
		})
	}
	return out, ""
}

// FromLine 构造 From 头(displayName 可空)。
func FromLine(email, displayName string) string { return FormatAddress(email, displayName) }
