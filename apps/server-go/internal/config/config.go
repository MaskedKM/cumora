// config —— 环境装载(#51)。变量命名与 TS 侧 env.ts 对齐(DATABASE_URL 等),
// Go 侧新增一律 CUMORA_GO_ 前缀。
package config

import "os"

type Config struct {
	ListenAddr     string
	DatabaseURL    string
	MigrationsDir  string
	InstanceID     string
}

func Load() Config {
	return Config{
		ListenAddr:    envOr("CUMORA_GO_LISTEN", ":5190"),
		DatabaseURL:   envOr("DATABASE_URL", "postgres://localhost:5432/cumora"),
		MigrationsDir: envOr("CUMORA_GO_MIGRATIONS", "migrations"),
		InstanceID:    envOr("INSTANCE_ID", "go-1"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
