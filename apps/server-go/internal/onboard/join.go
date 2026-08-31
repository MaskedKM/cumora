// join —— #all-hands 自动入组(#68 评审 F7 合一)。此前 core/agents/
// invitations 三处各持一副本:core 变体缺系统消息与 redis 扇出,另两副
// 本缺扇出。此处按 已退役 TS server 的 onboardCompany.ts joinAllHands 全量对齐:
// 组未建 no-op;成员幂等追加;kind='system' 入组消息;CH_MESSAGE_NEW
// 广播;human/agent 再发 CH_STATUS participants.added(前端 byId 库补
// 人,否则系统行引用无人认识的 participantId)。
package onboard

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

func joinUUID() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// JoinAllHands 把新成员(agent 或 human)并入公司 #all-hands 并广播
// "X joined" 系统消息。幂等:已在 members 时整链跳过(含系统消息)。
func JoinAllHands(ctx context.Context, db *sql.DB, companyID, participantID string) {
	var convID sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT all_hands_conversation_id FROM companies WHERE id = $1`, companyID).Scan(&convID); err != nil || !convID.Valid {
		return // 组未建 → no-op(legacy/seed 竞态,baseline 语义)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE conversations
		   SET members = members || to_jsonb(ARRAY[$2::text]), updated_at = NOW()
		 WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id = conversations.id AND cm.participant_id = $2)`, convID.String, participantID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // 已是成员 → 不双发
	}
	var seq int
	if err := db.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1`, convID.String).Scan(&seq); err != nil {
		seq = 1
	}
	messageID := "m-" + joinUUID()
	body, _ := json.Marshal(map[string]string{"kind": "joined", "participantId": participantID})
	_, _ = db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		VALUES ($1, $2, $3, 'system', $4, $5, $6)`,
		messageID, convID.String, participantID, string(body), seq, companyID)

	// 已开着的客户端实时看到入组。
	events.MessageNew(ctx, companyID, convID.String, map[string]any{
		"id": messageID, "conversationId": convID.String, "authorId": participantID,
		"kind": "system", "body": string(body), "sequence": seq, "at": httpx.ISOms(time.Now().UTC()),
	})

	// 补发完整 participant 载荷:系统行引用的 participantId 若无人认识,
	// 前端 SystemRow 直接弃渲染。广播 miss 可容忍(60s refresher 兜底)。
	var kind, name, initial, avatarBg, status string
	var role sql.NullString
	var avatarURL sql.NullString
	var statusUpdatedAt sql.NullTime
	err = db.QueryRowContext(ctx, `
		SELECT kind, name, role, initial, avatar_bg, avatar_url, status, status_updated_at
		  FROM participants WHERE id = $1 AND company_id = $2`, participantID, companyID).
		Scan(&kind, &name, &role, &initial, &avatarBg, &avatarURL, &status, &statusUpdatedAt)
	if err != nil || (kind != "human" && kind != "agent") {
		return
	}
	var roleAny any
	if role.Valid {
		roleAny = role.String
	}
	var avatarAny any
	if avatarURL.Valid {
		avatarAny = avatarURL.String
	}
	var statusAt any
	if statusUpdatedAt.Valid {
		statusAt = httpx.ISOms(statusUpdatedAt.Time.UTC())
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "participants.added", "companyId": companyID, "conversationId": convID.String,
		"participant": map[string]any{
			"id": participantID, "kind": kind, "name": name, "role": roleAny,
			"initial": initial, "avatarBg": avatarBg, "avatarUrl": avatarAny,
			"status": status, "statusUpdatedAt": statusAt,
		},
	})
	_ = events.PublishRaw(ctx, events.ChStatus, payload)
}
