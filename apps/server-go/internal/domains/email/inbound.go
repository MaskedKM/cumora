// email/inbound —— 入站邮件门回调(#58):Cloudflare Email Worker →
// POST /webhooks/email/inbound(raw body + HMAC-SHA256 签名)。
// 对齐 已退役 TS server 的 api/inbound-email.ts:签名常时比较、Message-ID 幂等、
// SES 回声启发式去重、附件本地落盘、按收件人扇出(agent/人类/external 发
// 送者解析)、竞态唯一键冲突折叠为 dedup。
package email

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	core "github.com/MaskedKM/cumora/apps/server-go/internal/email"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

const inboundMaxBody = 25 << 20 // 25mb,对齐上传上限

type inboundAttachment struct {
	Filename      string `json:"filename"`
	MimeType      string `json:"mimeType"`
	SizeBytes     int64  `json:"sizeBytes"`
	ContentBase64 string `json:"contentBase64"`
	Truncated     bool   `json:"truncated"`
}

type inboundPayload struct {
	MessageID     string              `json:"messageId"`
	InReplyTo     *string             `json:"inReplyTo"`
	References    []string            `json:"references"`
	From          string              `json:"from"`
	To            []string            `json:"to"`
	CC            []string            `json:"cc"`
	Subject       string              `json:"subject"`
	Text          string              `json:"text"`
	HTML          *string             `json:"html"`
	RawSizeBytes  int64               `json:"rawSizeBytes"`
	AutoSubmitted *string             `json:"autoSubmitted"`
	Attachments   []inboundAttachment `json:"attachments"`
}

// verifySignature:hex HMAC-SHA256 常时比较;sha256= 前缀容忍。
func verifySignature(raw []byte, signature string) bool {
	secret := config.EmailInboundHMACSecret()
	if secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	want := mac.Sum(nil)
	got := strings.ToLower(strings.TrimSpace(signature))
	got = strings.TrimPrefix(got, "sha256=")
	rawGot, err := hex.DecodeString(got)
	if err != nil || len(rawGot) != len(want) {
		return false
	}
	return hmac.Equal(want, rawGot)
}

// resolveInboundRecipient:先按地址直查(全租户),再按
// <id>.<slug>@EMAIL_DOMAIN 模式解码(末点分割;未铸地址惰铸)。
func resolveInboundRecipient(ctx context.Context, db *sql.DB, addr string) (companyID, participantID, name, kind string, ok bool) {
	lc := strings.ToLower(strings.TrimSpace(addr))
	if lc == "" {
		return "", "", "", "", false
	}
	err := db.QueryRowContext(ctx, `
		SELECT id, name, kind, company_id FROM participants
		 WHERE LOWER(email) = $1 AND departed_at IS NULL LIMIT 1`, lc).
		Scan(&participantID, &name, &kind, &companyID)
	if err == nil {
		return companyID, participantID, name, kind, true
	}
	dom := core.RootDomain()
	if dom == "" {
		return "", "", "", "", false
	}
	at := strings.Index(lc, "@")
	if at < 0 {
		return "", "", "", "", false
	}
	localPart, domPart := lc[:at], lc[at+1:]
	if domPart != dom {
		return "", "", "", "", false
	}
	lastDot := strings.LastIndex(localPart, ".")
	if lastDot <= 0 || lastDot >= len(localPart)-1 {
		return "", "", "", "", false
	}
	localID, slug := localPart[:lastDot], localPart[lastDot+1:]
	err = db.QueryRowContext(ctx, `
		SELECT p.id, p.name, p.kind, p.company_id
		  FROM participants p JOIN companies c ON c.id = p.company_id
		 WHERE p.departed_at IS NULL AND LOWER(c.slug) = $2
		   AND (LOWER(p.id) = $1 OR LOWER(REPLACE(p.id, '_', '-')) = $1)
		 LIMIT 1`, localID, slug).
		Scan(&participantID, &name, &kind, &companyID)
	if err != nil {
		return "", "", "", "", false
	}
	_, _ = core.EnsureParticipantAddress(ctx, db, participantID, companyID)
	return companyID, participantID, name, kind, true
}

// resolveSender:同租户 agent → 同租户 human → synthetic external:<addr>
// (并记录外部联系人)。
func resolveSender(ctx context.Context, db *sql.DB, fromAddr, fromName, companyID string) (participantID, displayName string) {
	if pid, name, _, ok := core.FindParticipantByAddress(ctx, db, fromAddr, companyID); ok {
		return pid, name
	}
	if uid, dname, ok := core.FindUserInCompanyByAuthEmail(ctx, db, fromAddr, companyID); ok {
		return uid, dname
	}
	core.RecordExternalContact(ctx, db, companyID, fromAddr, &fromName)
	return "external:" + strings.ToLower(fromAddr), fromName
}

var (
	stripScriptRe = regexp.MustCompile(`(?is)<script[\s\S]*?</script>`)
	stripStyleRe  = regexp.MustCompile(`(?is)<style[\s\S]*?</style>`)
	stripBrRe     = regexp.MustCompile(`(?i)<br\s*/?>`)
	stripPEndRe   = regexp.MustCompile(`(?i)</p>`)
	stripTagRe    = regexp.MustCompile(`<[^>]+>`)
	spaceNlRe     = regexp.MustCompile(`[ \t]+\n`)
	nl3Re         = regexp.MustCompile(`\n{3,}`)
)

// stripHtml 对齐 inbound-email.ts 的粗降级(仅 html-only 入站用)。
func stripHtml(html string) string {
	out := stripScriptRe.ReplaceAllString(html, " ")
	out = stripStyleRe.ReplaceAllString(out, " ")
	out = stripBrRe.ReplaceAllString(out, "\n")
	out = stripPEndRe.ReplaceAllString(out, "\n\n")
	out = stripTagRe.ReplaceAllString(out, " ")
	out = strings.ReplaceAll(out, "&nbsp;", " ")
	out = strings.ReplaceAll(out, "&amp;", "&")
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&quot;", `"`)
	out = strings.ReplaceAll(out, "&#39;", "'")
	out = spaceNlRe.ReplaceAllString(out, "\n")
	out = nl3Re.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

var extSanRe = regexp.MustCompile(`[^a-z0-9]`)

// putAttachment 本地存储:UPLOAD_DIR(server/uploads)/<key>;与 TS 本地
// storage.put 同布局。
func putAttachment(key string, bytes []byte, mimeType string) error {
	root := config.EmailUploadDir()
	full := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, bytes, 0o644)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
}

// InboundWebhook:/webhooks/email/inbound 的域实现(#187 批次 8 起
// core tag 经 ServerInterface 委托到此;根 mux 的 MountInbound 同挂
// 本函数 —— 单一函数体,双注册)。
func InboundWebhook(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sig := r.Header.Get("x-cumora-signature")
	if sig == "" {
		httpx.WriteError(w, http.StatusBadRequest, "missing signature or body")
		return
	}
	if config.EmailInboundHMACSecret() == "" {
		httpx.WriteError(w, http.StatusServiceUnavailable, "inbound email disabled (EMAIL_INBOUND_HMAC_SECRET unset)")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, inboundMaxBody+1))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "missing signature or body")
		return
	}
	if len(raw) > inboundMaxBody {
		httpx.WriteError(w, http.StatusRequestEntityTooLarge, "request entity too large")
		return
	}
	if len(raw) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "missing signature or body")
		return
	}
	if !verifySignature(raw, sig) {
		httpx.WriteError(w, http.StatusUnauthorized, "bad signature")
		return
	}
	var payload inboundPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad payload — need messageId + from")
		return
	}
	fromAddr, fromName, ok := core.ParseAddress(payload.From)
	if !ok {
		httpx.WriteError(w, http.StatusBadRequest, "unparseable from: "+payload.From)
		return
	}
	type rcpt struct{ addr, name string }
	recipients := []rcpt{}
	for _, s := range append(append([]string{}, payload.To...), payload.CC...) {
		if a, n, ok2 := core.ParseAddress(s); ok2 {
			recipients = append(recipients, rcpt{a, n})
		}
	}
	if len(recipients) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "no recipients")
		return
	}
	subject := strings.TrimSpace(payload.Subject)
	body := strings.TrimSpace(payload.Text)
	var html string
	if payload.HTML != nil {
		html = *payload.HTML
	}
	if body == "" {
		body = stripHtml(html)
	}
	messageIDNorm := core.NormalizeMessageId(payload.MessageID)
	if messageIDNorm == "" {
		// TS:typeof '' === 'string' 过载荷检查,到 normalize 才拒
		httpx.WriteError(w, http.StatusBadRequest, "invalid messageId")
		return
	}
	// 幂等:同 Message-ID 重投不建重复线程。
	var dupID string
	if db.QueryRowContext(ctx,
		`SELECT message_id FROM email_messages WHERE LOWER(smtp_message_id) = $1 LIMIT 1`,
		messageIDNorm).Scan(&dupID) == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "deduplicated": true, "messageId": dupID})
		return
	}
	// 回声去重:SES 改写 Message-ID 时,10 分钟内同 (from,to,subject)
	// 的出站行即为我们刚发的那封。
	subjectKey := subject
	if subjectKey == "" {
		subjectKey = "(no subject)"
	}
	fromFull := core.FormatAddress(fromAddr, fromName)
	inboundToJSON := marshalNoEscape(payload.To)
	var echoID string
	if db.QueryRowContext(ctx, `
			SELECT message_id FROM email_messages
			 WHERE direction = 'out' AND created_at > NOW() - INTERVAL '10 minutes'
			   AND LOWER(subject) = LOWER($1)
			   AND LOWER(from_addr) = LOWER($2)
			   AND LOWER(to_addrs::text) = LOWER($3)
			 ORDER BY created_at DESC LIMIT 1`,
		subjectKey, fromFull, string(inboundToJSON)).Scan(&echoID) == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "deduplicated": true, "echo": true, "messageId": echoID})
		return
	}
	// 附件:一次上传,逐收件人共享 storage key。
	type uploaded struct {
		filename  string
		mimeType  string
		sizeBytes int64
		key       *string
		truncated bool
	}
	uploads := []uploaded{}
	for _, a := range payload.Attachments {
		// TS inbound-email.ts:326-327 `.slice(0, 200/120)` 按 UTF-16
		// 码元(#185 评审 P2:出站点已换,入站孪生点补齐)。
		filename := a.Filename
		if filename == "" {
			filename = "attachment"
		}
		filename = httpx.UTF16Cap(filename, 200)
		mimeType := a.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		mimeType = httpx.UTF16Cap(mimeType, 120)
		size := a.SizeBytes
		if size < 0 {
			size = 0
		}
		if a.Truncated || a.ContentBase64 == "" {
			uploads = append(uploads, uploaded{filename, mimeType, size, nil, true})
			continue
		}
		bytes, err := base64Decode(a.ContentBase64)
		if err != nil {
			uploads = append(uploads, uploaded{filename, mimeType, size, nil, true})
			continue
		}
		ext := ""
		if i := strings.LastIndex(filename, "."); i > 0 {
			ext = extSanRe.ReplaceAllString(strings.ToLower(filename[i+1:]), "")
			if len(ext) > 8 {
				ext = ext[:8]
			}
		}
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		key := "email-attachments/" + hex.EncodeToString(b)
		if ext != "" {
			key += "." + ext
		}
		if err := putAttachment(key, bytes, mimeType); err != nil {
			uploads = append(uploads, uploaded{filename, mimeType, size, nil, true})
			continue
		}
		uploads = append(uploads, uploaded{filename, mimeType, size, &key, false})
	}
	// 扇出:每个可解析到租户的收件人一条投递。
	type delivery struct {
		CompanyID      string `json:"companyId"`
		ConversationID string `json:"conversationId"`
		MessageID      string `json:"messageId"`
	}
	inserts := []delivery{}
	for _, rc := range recipients {
		companyID, _, _, _, resolved := resolveInboundRecipient(ctx, db, rc.addr)
		if !resolved {
			continue
		}
		senderID, _ := resolveSender(ctx, db, fromAddr, fromName, companyID)
		memberIDs := []string{senderID} // TS:new Set([sender, ...]) 保插入序
		seenMember := map[string]bool{senderID: true}
		for _, r2 := range recipients {
			if c2, pid2, _, _, ok2 := resolveInboundRecipient(ctx, db, r2.addr); ok2 && c2 == companyID && !seenMember[pid2] {
				seenMember[pid2] = true
				memberIDs = append(memberIDs, pid2)
			}
		}
		var inReplyTo string
		if payload.InReplyTo != nil {
			inReplyTo = *payload.InReplyTo
		}
		convID, _, err := core.FindOrCreateEmailConversation(ctx, db, companyID, inReplyTo, payload.References, subjectKey, memberIDs)
		if err != nil {
			continue
		}
		atts := []core.PersistAttachment{}
		for _, u := range uploads {
			atts = append(atts, core.PersistAttachment{
				Filename: u.filename, MimeType: u.mimeType, SizeBytes: u.sizeBytes,
				StorageKey: u.key, Truncated: u.truncated,
			})
		}
		persistedID, _, err := core.PersistEmailMessage(ctx, db, core.PersistArgs{
			ConversationID: convID, CompanyID: companyID, AuthorID: senderID,
			Direction: "in", TransportStatus: "received",
			SmtpMessageID: messageIDNorm, InReplyTo: inReplyTo,
			References: payload.References, Subject: subjectKey,
			FromAddr: fromFull, ToAddrs: payload.To, CCAddrs: payload.CC,
			Body: body, HTML: html, RawSizeBytes: payload.RawSizeBytes,
			AutoSubmitted: payload.AutoSubmitted != nil, // TS Boolean(payload.autoSubmitted):"no" 亦真
			Attachments:   atts,
		})
		if err != nil {
			if isUniqueViolation(err) {
				continue // 双 worker 并发同投:竞态 dedup
			}
			continue
		}
		inserts = append(inserts, delivery{companyID, convID, persistedID})
	}
	if len(inserts) == 0 {
		httpx.WriteError(w, http.StatusNotFound, "no recipient resolved to a known agent")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "deliveries": inserts})
}

// MountInbound 挂 /webhooks/email/inbound(不在 /api 认证链内,自有
// HMAC 面;根 mux 特定 pattern 优先于 /api/ 子树)。core tag 的接口
// 方法亦委托 InboundWebhook —— 单一函数体,双注册。
func MountInbound(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("POST /webhooks/email/inbound", func(w http.ResponseWriter, r *http.Request) {
		InboundWebhook(db, w, r)
	})
}

// marshalNoEscape:对齐 JSON.stringify(不转义 <>&)。
func marshalNoEscape(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// base64Decode:Node Buffer.from 宽容语义(去垫/URL 变体回退)。
func base64Decode(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{
		base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("illegal base64 data")
}
