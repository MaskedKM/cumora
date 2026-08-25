// config —— 环境装载(#51)。变量命名与 TS 侧 env.ts 对齐(DATABASE_URL 等),
// Go 侧新增一律 CUMORA_GO_ 前缀。
package config

import (
	"os"
	"strings"
)

type Config struct {
	ListenAddr    string
	DatabaseURL   string
	MigrationsDir string
}

func Load() Config {
	return Config{
		ListenAddr:    envOr("CUMORA_GO_LISTEN", ":5190"),
		DatabaseURL:   withSSLModeDisabled(envOr("DATABASE_URL", "postgres://localhost:5432/cumora")),
		MigrationsDir: envOr("CUMORA_GO_MIGRATIONS", "migrations"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// withSSLModeDisabled 在 URL 未显式指定 sslmode 时追加 disable —— 自托管
// 本机 pg 默认无 TLS,pgx 默认 prefer 会握手失败(TS 侧 pg 驱动默认不要求)。
func withSSLModeDisabled(url string) string {
	if strings.Contains(url, "sslmode=") {
		return url
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "sslmode=disable"
}
