// db —— 事务形态合一(#141、#213)。#138 收编 postSystemMessage,#213
// 再收编 9 处(mailbox 静音、agent 建板、agent ship 两写、shipping
// feature 两写、删号、admin 停用、email 重试认领);语义不变:
// 任一步失败整体回滚,提交后 Rollback 为 no-op,panic 先回滚再上抛。
// 手写 BeginTx 仅存豁免形态(SAVEPOINT 嵌套、mid-body 提交/回滚、
// tx 跨函数传递、错误映射无法字节等价),豁免点均带 #213 注释;
// db.go 迁移 mini-runner 自有事务形态不在收编范围。
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
