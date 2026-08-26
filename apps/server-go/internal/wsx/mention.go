// wsx/mention —— doc.mention.notify 的落库与扇出(对齐 ws.ts 的
// processDocMention)。agent 唤醒(postDocMentionWake:合成 text 消息
// 走邮箱调度器)依赖 #60 的真实消息运行时,拆至 #82;本票保留
// mention 行、agent_log 面包屑与 CH_DOC_MENTION 事件。
package wsx

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
)

func shortID16(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// resolveDisplayName 对齐 ws.ts 的实际行为:其 users 查询引用不存在的
// users.name 列(schema 里是 display_name)——该查询恒报错被 catch 吞掉,
// 所以基线总是落到 participants.name,再回退 id。严格平价 = 复刻这一
// 有效行为(修复基线 bug 是切换日后的独立决定)。
func (g *Gateway) resolveDisplayName(ctx context.Context, id, companyID string) string {
	var name string
	if err := g.db.QueryRowContext(ctx,
		`SELECT name FROM participants WHERE id = $1 AND company_id = $2 LIMIT 1`,
		id, companyID).Scan(&name); err == nil && name != "" {
		return name
	}
	return id
}

// processDocMention:①按租户过滤提及对象;②60 秒窗口去重(吸收编辑器
// 每击键的重复通知,不做全局唯一);③写 document_mentions;④agent 追加
// agent_log 面包屑(cumora log 可见);⑤CH_DOC_MENTION 扇出。
func (g *Gateway) processDocMention(ctx context.Context, documentID, companyID, mentionerID string, requestedIDs []string) error {
	rows, err := g.db.QueryContext(ctx, `
		SELECT id, kind, name FROM participants
		 WHERE company_id = $1 AND id = ANY($2::text[])`, companyID, requestedIDs)
	if err != nil {
		return err
	}
	type mentioned struct{ id, kind, name string }
	valid := []mentioned{}
	for rows.Next() {
		var m mentioned
		if rows.Scan(&m.id, &m.kind, &m.name) == nil {
			valid = append(valid, m)
		}
	}
	rows.Close()
	if len(valid) == 0 {
		return nil
	}

	var docTitle sql.NullString
	var pinnedConvo sql.NullString
	_ = g.db.QueryRowContext(ctx,
		`SELECT title, conversation_id FROM documents WHERE id = $1 AND company_id = $2`,
		documentID, companyID).Scan(&docTitle, &pinnedConvo)
	title := docTitle.String
	if title == "" {
		title = "Untitled"
	}
	mentionerName := g.resolveDisplayName(ctx, mentionerID, companyID)

	freshIDs := []string{}
	for _, m := range valid {
		var recent string
		err := g.db.QueryRowContext(ctx, `
			SELECT id FROM document_mentions
			 WHERE document_id = $1 AND mentioner_id = $2 AND mentioned_id = $3
			   AND created_at > NOW() - INTERVAL '60 seconds' LIMIT 1`,
			documentID, mentionerID, m.id).Scan(&recent)
		if err == nil {
			continue // 窗口内重复
		}
		if err != sql.ErrNoRows {
			return err // 去重查询失败向上抛 → doc.error(TS 同)
		}
		if _, err := g.db.ExecContext(ctx, `
			INSERT INTO document_mentions (id, document_id, company_id, mentioner_id, mentioned_id)
			VALUES ($1, $2, $3, $4, $5)`,
			shortID16("dm_"), documentID, companyID, mentionerID, m.id); err != nil {
			return err
		}
		freshIDs = append(freshIDs, m.id)
		if m.kind == "agent" {
			ref, _ := json.Marshal(map[string]string{"documentId": documentID, "mentionerId": mentionerID})
			// 面包屑失败不阻断其余提及(对齐 TS 的 catch-warn)。
			// 标题原样嵌入(TS 是裸拼接,%q 会转义引号造成字节级漂移)。
			_, _ = g.db.ExecContext(ctx, `
				INSERT INTO agent_log (id, agent_id, company_id, kind, body, ref)
				VALUES ($1, $2, $3, 'doc_mention', $4, $5::jsonb)`,
				shortID16("log_"), m.id, companyID,
				fmt.Sprintf(`%s @-mentioned you in doc "%s"`, mentionerName, title), ref)
			// agent 唤醒(合成唤醒消息)→ #82
		}
	}
	if len(freshIDs) == 0 {
		return nil
	}
	events.DocMention(ctx, companyID, documentID, title, mentionerID, mentionerName, freshIDs)
	return nil
}
