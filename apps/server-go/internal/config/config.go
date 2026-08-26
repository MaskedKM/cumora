// config —— 环境装载(#51)。变量命名与 TS 侧 env.ts 对齐(DATABASE_URL 等),
// Go 侧新增一律 CUMORA_GO_ 前缀。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	ListenAddr    string
	DatabaseURL   string
	MigrationsDir string
	RedisURL      string
	// Yjs sidecar 内表面(127.0.0.1+token,ADR 0004)与 relay 实例标识。
	// 变量名对齐 server/src/env.ts。
	YjsSidecarURL      string
	YjsSidecarToken    string
	YjsSidecarTimeout  int // ms
	InstanceID         string
}

func Load() Config {
	instanceID := os.Getenv("INSTANCE_ID")
	if instanceID == "" {
		// 每实例唯一:sidecar 的 subscriberId / 回声抑制的 origin 前缀
		b := make([]byte, 5)
		_, _ = rand.Read(b)
		instanceID = "app-go-" + hex.EncodeToString(b)
	}
	return Config{
		ListenAddr:    envOr("CUMORA_GO_LISTEN", ":5190"),
		DatabaseURL:   withSSLModeDisabled(envOr("DATABASE_URL", "postgres://localhost:5432/cumora")),
		MigrationsDir: envOr("CUMORA_GO_MIGRATIONS", "migrations"),
		RedisURL:      envOr("REDIS_URL", "redis://localhost:6379"),
		YjsSidecarURL: envOr("YJS_SIDECAR_URL", "http://127.0.0.1:5182"),
		YjsSidecarToken: os.Getenv("YJS_SIDECAR_TOKEN"),
		YjsSidecarTimeout:  envInt("YJS_SIDECAR_TIMEOUT_MS", 5000),
		InstanceID:         instanceID,
	}
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
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
