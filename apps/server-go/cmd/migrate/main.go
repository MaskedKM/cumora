// migrate —— 一次性应用 Go 迁移后退出(#70 补充件)。server 进程启动时
// 会自动应用同一套迁移;本命令服务两个不听端口的场景:
//   - CI:unit 前给测试库引导 schema(yjs-sidecar 的 http 单测读 documents);
//   - 运维:手工修复/预演时只动 schema 不起服。
//
// 用法:DATABASE_URL=… [CUMORA_GO_MIGRATIONS=…] go run ./cmd/migrate
package main

import (
	"fmt"
	"os"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/db"
)

func main() {
	cfg := config.Load()
	pool, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pg connect failed:", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := db.Migrate(pool, cfg.MigrationsDir); err != nil {
		fmt.Fprintln(os.Stderr, "migrations apply failed:", err)
		os.Exit(1)
	}
	fmt.Println("schema at baseline:", cfg.MigrationsDir)
}
