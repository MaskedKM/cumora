// db —— 事务形态合一(#141)。#138 给 sendMessage/postSystemMessage
// 手写的 Begin → defer Rollback → Commit 形态收编为 WithTx;语义不变:
// 任一步失败整体回滚,提交后 Rollback 为 no-op,panic 先回滚再上抛。
package db

import (
	"context"
	"database/sql"
)

// WithTx:Begin → fn → 错误回滚/成功提交。fn 内多步写同生共死。
func WithTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
