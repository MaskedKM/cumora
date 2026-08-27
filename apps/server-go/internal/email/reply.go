// ReplyInEmailConversation:email.ts replyInEmailConversation 的 Go 等价
// —— 在 kind='email' 会话里代发一封真正的回复。写库先行(见 email.ts
// 的重复投递事故注释),发件失败交 retry worker。
package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type ReplyResult struct {
	MessageID       string
	Sequence        int
	TransportStatus string // 'sent' | 'failed'
	Mock            bool
	Error           string
}

var reSubject = regexp.MustCompile(`(?i)^(re|fwd|fw)\s*:`)

// ReplyInEmailConversation 对齐 email.ts:锚定最新 email_messages 行,
// reply-all 或 continue-thread 语义拆收件人,写库(sending)→ Resend →
// 终态 UPDATE。
type ReplyArgs struct {
	ConversationID string
	CompanyID      string
	AuthorID       string
	Body           string
	AutoSubmitted  bool
}

func ReplyInEmailConversation(ctx context.Context, db *sql.DB, args ReplyArgs) (*ReplyResult, error) {
	// 最新的 email 行锚定回复;kind='email' 会话必有至少一行。
	var (
		smtpMessageID sql.NullString
		referencesCh  cliStrSlice
		subject       string
		fromAddr      string
		toAddrs       cliStrSlice
		ccAddrs       cliStrSlice
	)
	err := db.QueryRowContext(ctx, `
		SELECT em.smtp_message_id, em.references_chain, em.subject,
		       em.from_addr, em.to_addrs, em.cc_addrs
		  FROM email_messages em
		 WHERE em.conversation_id = $1
		 ORDER BY em.created_at DESC
		 LIMIT 1`, args.ConversationID,
	).Scan(&smtpMessageID, &referencesCh, &subject, &fromAddr, &toAddrs, &ccAddrs)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("replyInEmailConversation: conversation %s has no email_messages parent", args.ConversationID)
	}
	if err != nil {
		return nil, err
	}

	sender, ok := EnsureParticipantAddress(ctx, db, args.AuthorID, args.CompanyID)
	if !ok {
		return nil, fmt.Errorf("replyInEmailConversation: no cumora address for participant %s in %s", args.AuthorID, args.CompanyID)
	}
	// 人类作者把 auth email 也算"自己"(原信可能用任一地址称呼我);
	// agent 没有 auth email,查询为空。
	var authEmail sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = $1 LIMIT 1`, args.AuthorID).Scan(&authEmail)
	selfAddrs := []string{strings.ToLower(sender.Email)}
	if authEmail.Valid && authEmail.String != "" {
		selfAddrs = append(selfAddrs, strings.ToLower(authEmail.String))
	}

	// 两种锚定:
	//  (a) 原信来自他人 → reply-all:TO=原 From,CC=原 To+Cc 去己;
	//  (b) 原信是我们自己发的 → 沿用原 to/cc(去己)继续同一线程。
	var replyTo, replyCC []string
	parentAddr, _, pok := ParseAddress(fromAddr)
	parentFromIsSelf := false
	if pok {
		for _, s := range selfAddrs {
			if s == strings.ToLower(parentAddr) {
				parentFromIsSelf = true
			}
		}
	}
	if parentFromIsSelf {
		filterSelf := func(addrs []string) []string {
			var out []string
			for _, raw := range addrs {
				a, _, ok := ParseAddress(raw)
				isSelf := false
				if ok {
					for _, s := range selfAddrs {
						if s == strings.ToLower(a) {
							isSelf = true
						}
					}
				}
				if !isSelf {
					out = append(out, raw)
				}
			}
			return out
		}
		replyTo = filterSelf(toAddrs)
		replyCC = filterSelf(ccAddrs)
	} else {
		replyTo, replyCC = SplitReplyAddresses(fromAddr, toAddrs, ccAddrs, selfAddrs)
	}
	if len(replyTo) == 0 {
		return nil, fmt.Errorf("replyInEmailConversation: no remaining recipients after self-removal for %s", args.ConversationID)
	}

	finalSubject := sanitizeSubjectReply(subject)
	newReferences := append([]string{}, referencesCh...)
	if smtpMessageID.Valid && smtpMessageID.String != "" {
		newReferences = append(newReferences, smtpMessageID.String)
	}
	inReplyTo := ""
	if smtpMessageID.Valid && smtpMessageID.String != "" {
		inReplyTo = NormalizeMessageId(smtpMessageID.String)
	}
	messageID := MintMessageId()
	fromLine := FormatAddress(sender.Email, sender.DisplayName)

	// 写库先行:Message-ID 先落库;发件失败行留 sending/failed 给 retry
	// worker(Resend 按 Message-ID 幂等,不重复进收件箱)。
	persistedID, sequence, err := PersistEmailMessage(ctx, db, PersistArgs{
		ConversationID:  args.ConversationID,
		CompanyID:       args.CompanyID,
		AuthorID:        args.AuthorID,
		Direction:       "out",
		TransportStatus: "sending",
		SmtpMessageID:   messageID,
		InReplyTo:       inReplyTo,
		References:      newReferences,
		Subject:         finalSubject,
		FromAddr:        fromLine,
		ToAddrs:         replyTo,
		CCAddrs:         replyCC,
		Body:            args.Body,
		AutoSubmitted:   args.AutoSubmitted,
	})
	if err != nil {
		return nil, err
	}

	autoSubmitted := ""
	if args.AutoSubmitted {
		autoSubmitted = "auto-replied"
	}
	sendRes := SendViaProvider(ctx, SendArgs{
		From:          fromLine,
		To:            replyTo,
		CC:            replyCC,
		Subject:       finalSubject,
		Text:          args.Body,
		InReplyTo:     inReplyTo,
		References:    newReferences,
		MessageID:     messageID,
		AutoSubmitted: autoSubmitted,
	})

	finalStatus := "failed"
	if sendRes.OK {
		finalStatus = "sent"
	}
	finalSmtpID := sendRes.SmtpMessageID
	if finalSmtpID == "" {
		finalSmtpID = messageID
	}
	var nextRetry any
	if finalStatus == "failed" {
		nextRetry = time.Now().Add(60 * time.Second)
	}
	// 终态 UPDATE 失败不抛:retry worker 会按 sending/failed 兜底。
	_, _ = db.ExecContext(ctx, `
		UPDATE email_messages
		   SET transport_status = $1,
		       transport_error  = $2,
		       smtp_message_id  = $3,
		       next_retry_at    = $4
		 WHERE message_id = $5`,
		finalStatus, sendRes.Error, finalSmtpID, nextRetry, persistedID)

	return &ReplyResult{
		MessageID:       persistedID,
		Sequence:        sequence,
		TransportStatus: finalStatus,
		Mock:            sendRes.Mock,
		Error:           sendRes.Error,
	}, nil
}

// sanitizeSubjectReply:已带 Re:/Fwd: 前缀 → 清洗保持;否则补 `Re: `。
func sanitizeSubjectReply(parentSubject string) string {
	if reSubject.MatchString(parentSubject) {
		return SanitizeSubject(parentSubject)
	}
	return SanitizeSubject("Re: " + parentSubject)
}

// cliStrSlice:email_messages 的 jsonb 字符串数组列。
type cliStrSlice []string

func (a *cliStrSlice) Scan(src any) error {
	switch t := src.(type) {
	case nil:
		*a = nil
		return nil
	case []byte:
		return json.Unmarshal(t, a)
	case string:
		return json.Unmarshal([]byte(t), a)
	default:
		if s, ok := src.([]string); ok {
			*a = s
			return nil
		}
		return fmt.Errorf("cliStrSlice: unsupported %T", src)
	}
}
