// db —— counter upsert 合一(#141):conversations 域 sendMessage/
// postSystemMessage 与 agent 数据面 NextConversationSequence 三处
// 逐字节相同的取序 SQL 收编为一。种子 2、RETURNING next-1:首次消息
// sequence=1。ErrNoRows→1 是防御镜像(INSERT..RETURNING 恒有行),
// 保留三处原语义。
package db

import (
	"context"
	"database/sql"
)

// Querier:db 与 tx 共有的取行原语,让取序在两种连接态下复用。
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// AllocSequence:会话取序(counter upsert,原子自增)。
func AllocSequence(ctx context.Context, q Querier, conversationID string) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1 AS seq`, conversationID).Scan(&seq)
	if err == sql.ErrNoRows {
		return 1, nil
	}
	return seq, err
}
