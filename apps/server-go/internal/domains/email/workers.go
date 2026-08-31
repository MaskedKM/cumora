// email/workers —— 出站重试与附件 GC 任务(#58,受管任务组形态)。
// 对齐 已退役 TS server 的 email-retry.ts / email-gc.ts:
//
//	重试:SKIP LOCKED 认领到期失败行,保线程/收件人/附件重发,退避
//	60s·5m·30m·2h·6h·24h,封顶后 next_retry_at=NULL(终态)。
//	GC:枚举 email-attachments/ 前缀对账 DB 引用键,删超过安全窗的孤儿。
//
// EMAIL_RETRY_INTERVAL_MS / EMAIL_GC_INTERVAL_MS =0 关停。
package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	dbpkg "github.com/MaskedKM/cumora/apps/server-go/internal/db"
	core "github.com/MaskedKM/cumora/apps/server-go/internal/email"
)

var backoffSteps = []time.Duration{
	60 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

type retryRow struct {
	MessageID       string
	CompanyID       string
	SmtpMessageID   sql.NullString
	InReplyTo       sql.NullString
	ReferencesChain []byte
	Subject         string
	FromAddr        string
	ToAddrs         []byte
	CCAddrs         []byte
	Body            string
	AutoSubmitted   bool
	RetryAttempts   int
}

// claimDueRetries:事务内 SKIP LOCKED 认领,行内 next_retry_at 前推 5 分钟
// 防同副本下一 tick 重抓。
func claimDueRetries(ctx context.Context, db *sql.DB, limit int) ([]retryRow, error) {
	due := []retryRow{}
	// #213:收编 db.WithTx——各步失败均原样返回 err,错误映射单一。
	if err := dbpkg.WithTx(ctx, db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT em.message_id, em.company_id, em.smtp_message_id, em.in_reply_to,
			       em.references_chain, em.subject, em.from_addr, em.to_addrs, em.cc_addrs,
			       m.body, em.auto_submitted, em.retry_attempts
			  FROM email_messages em JOIN messages m ON m.id = em.message_id
			 WHERE em.direction = 'out' AND em.transport_status = 'failed'
			   AND em.next_retry_at IS NOT NULL AND em.next_retry_at <= NOW()
			 ORDER BY em.next_retry_at ASC
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r retryRow
			if rows.Scan(&r.MessageID, &r.CompanyID, &r.SmtpMessageID, &r.InReplyTo,
				&r.ReferencesChain, &r.Subject, &r.FromAddr, &r.ToAddrs, &r.CCAddrs,
				&r.Body, &r.AutoSubmitted, &r.RetryAttempts) == nil {
				due = append(due, r)
			}
		}
		rows.Close()
		if len(due) > 0 {
			ids := make([]string, len(due))
			for i, r := range due {
				ids[i] = r.MessageID
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE email_messages SET next_retry_at = NOW() + INTERVAL '5 minutes'
				  WHERE message_id = ANY($1::text[])`, ids); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return due, nil
}

func nextRetryAt(attemptsAfter int) (time.Time, bool) {
	if attemptsAfter >= len(backoffSteps) {
		return time.Time{}, false
	}
	return time.Now().Add(backoffSteps[attemptsAfter]), true
}

func retryOne(ctx context.Context, db *sql.DB, row retryRow) {
	attempt := row.RetryAttempts + 1
	// 附件:非 truncated 且有 key 的重发(publicUrl 本地相对路径)。
	attRows, err := db.QueryContext(ctx, `
		SELECT filename, mime_type, storage_key FROM email_attachments
		 WHERE message_id = $1 AND truncated = false AND storage_key IS NOT NULL
		 ORDER BY created_at`, row.MessageID)
	atts := []core.Attachment{}
	if err == nil {
		for attRows.Next() {
			var filename, mime, key string
			if attRows.Scan(&filename, &mime, &key) == nil {
				atts = append(atts, core.Attachment{Filename: filename, MimeType: mime, Path: "/uploads/" + key})
			}
		}
		attRows.Close()
	}
	var toAddrs, ccAddrs []string
	_ = json.Unmarshal(row.ToAddrs, &toAddrs)
	_ = json.Unmarshal(row.CCAddrs, &ccAddrs)
	fromLine := row.FromAddr
	if a, n, ok := core.ParseAddress(row.FromAddr); ok {
		fromLine = core.FormatAddress(a, n)
	}
	var inReplyTo string
	if row.InReplyTo.Valid {
		inReplyTo = row.InReplyTo.String
	}
	var refs []string
	_ = json.Unmarshal(row.ReferencesChain, &refs)
	var autoSubmitted string
	if row.AutoSubmitted {
		autoSubmitted = "auto-generated"
	}
	sendRes := core.SendViaProvider(ctx, core.SendArgs{
		From: fromLine, To: toAddrs, CC: ccAddrs,
		Subject: row.Subject, Text: row.Body,
		InReplyTo: inReplyTo, References: refs,
		MessageID:     row.SmtpMessageID.String,
		AutoSubmitted: autoSubmitted,
		Attachments:   atts,
	})
	if sendRes.OK {
		_, _ = db.ExecContext(ctx, `
			UPDATE email_messages SET transport_status = 'sent', transport_error = NULL,
			       smtp_message_id = COALESCE($2, smtp_message_id), retry_attempts = $3, next_retry_at = NULL
			WHERE message_id = $1`,
			row.MessageID, core.NormalizeMessageId(sendRes.SmtpMessageID), attempt)
		slog.Info("email.retry.ok", "message_id", row.MessageID, "attempt", attempt)
		return
	}
	next, hasNext := nextRetryAt(attempt)
	var nextAny any
	if hasNext {
		nextAny = next
	}
	_, _ = db.ExecContext(ctx, `
		UPDATE email_messages SET transport_error = $2, retry_attempts = $3, next_retry_at = $4
		WHERE message_id = $1`, row.MessageID, sendRes.Error, attempt, nextAny)
	slog.Warn("email.retry.fail", "message_id", row.MessageID, "attempt", attempt,
		"terminal", !hasNext, "error", sendRes.Error)
}

// RunRetryTick 跑一轮(认领+逐条);测试与 loop 共用。
func RunRetryTick(ctx context.Context, db *sql.DB, maxBatch int) int {
	due, err := claimDueRetries(ctx, db, maxBatch)
	if err != nil {
		slog.Error("email.retry claim failed", "err", err)
		return 0
	}
	for _, row := range due {
		retryOne(ctx, db, row)
	}
	return len(due)
}

// StartRetryWorker:受管周期任务;intervalMs<=0 关停。
func StartRetryWorker(ctx context.Context, db *sql.DB, intervalMS int) {
	if intervalMS <= 0 {
		slog.Info("email-retry disabled (EMAIL_RETRY_INTERVAL_MS=0)")
		return
	}
	interval := time.Duration(intervalMS) * time.Millisecond
	slog.Info("email-retry starting", "interval_ms", intervalMS, "max_attempts", len(backoffSteps))
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				RunRetryTick(ctx, db, 16)
			}
		}
	}()
}

/* ───────────── GC ───────────── */

const storagePrefix = "email-attachments/"

func gcSafetyAge() time.Duration { return time.Hour }

// PickOrphans 纯函数决策矩阵:在存储、不在 DB、超过安全窗 → 删。
func PickOrphans(inStorage map[string]time.Time, inDB map[string]bool, now time.Time) []string {
	cutoff := now.Add(-gcSafetyAge())
	out := []string{}
	for key, mod := range inStorage {
		if inDB[key] {
			continue
		}
		if mod.After(cutoff) {
			continue
		}
		out = append(out, key)
	}
	return out
}

// uploadsRoot:GC 扫描根,与入站写入同一解析点(config.UploadsDir(),
// #208)——env 只认写侧不认读侧时,附件会落新目录却永不被 GC。
func uploadsRoot() string { return config.UploadsDir() }

// RunGcTick:枚举前缀 → 对账 DB → 删孤儿。幂等(多副本安全)。
func RunGcTick(ctx context.Context, db *sql.DB) (inspected, deleted int) {
	prefixDir := filepath.Join(uploadsRoot(), storagePrefix)
	entries, err := os.ReadDir(prefixDir)
	if err != nil {
		return 0, 0 // 无前缀目录 = 无对象
	}
	inStorage := map[string]time.Time{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			inStorage[storagePrefix+e.Name()] = info.ModTime()
		}
	}
	inDB := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT storage_key FROM email_attachments WHERE storage_key IS NOT NULL`)
	if err != nil {
		return 0, 0 // 引用键不可知时绝不删(TS:Promise.all reject → 整轮中止)
	}
	for rows.Next() {
		var key string
		if rows.Scan(&key) == nil {
			inDB[key] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0
	}
	orphans := PickOrphans(inStorage, inDB, time.Now())
	for _, key := range orphans {
		if err := os.Remove(filepath.Join(uploadsRoot(), filepath.FromSlash(key))); err == nil {
			deleted++
		}
	}
	return len(inStorage), deleted
}

// StartGcWorker:受管周期任务;intervalMs<=0 关停。
func StartGcWorker(ctx context.Context, db *sql.DB, intervalMS int) {
	if intervalMS <= 0 {
		slog.Info("email-gc disabled (EMAIL_GC_INTERVAL_MS=0)")
		return
	}
	interval := time.Duration(intervalMS) * time.Millisecond
	slog.Info("email-gc starting", "interval_ms", intervalMS)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				inspected, deleted := RunGcTick(ctx, db)
				if deleted > 0 {
					slog.Info("email.gc.deleted", "inspected", inspected, "deleted", deleted)
				}
			}
		}
	}()
}
