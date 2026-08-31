// prodguard —— 生产模式的安全默认门(#212)。
//
// NODE_ENV=production 时两类危险配置直接拒绝启动(调用方 main 负责
// os.Exit),而不是带病运行:
//   - AGENT_RUNTIME_SECRET 未设:computers.agentRuntimeSecret() 会静默
//     回退到源码内置开发密钥,任何读过源码的人可伪造任意 agent 的
//     /runtime/* JWT(整个 agent 数据面)。
//   - CUMORA_GO_FAKE_AUTH=1:httpx.Authn 信任 x-test-user 头直注 uid,
//     是集成测试后门,生产开着等于免认证。
//
// 非生产照旧(开发与集成双跑依赖这两条路径);EnvFallbackWarnings 负责
// 非生产侧的提示性日志。
package config

import (
	"strings"
)

// ProdEnvViolations 返回生产模式下必须拒启的配置错误;空切片 = 放行。
// getenv 注入便于测试,生产调用方传 os.Getenv。
func ProdEnvViolations(getenv func(string) string) []string {
	if getenv("NODE_ENV") != "production" {
		return nil
	}
	var violations []string
	if strings.TrimSpace(getenv("AGENT_RUNTIME_SECRET")) == "" {
		violations = append(violations,
			"AGENT_RUNTIME_SECRET is not set — agent-runtime JWTs would fall back to the hardcoded dev secret, forgeable by anyone who has read the source (generate: openssl rand -hex 32)")
	}
	if getenv("CUMORA_GO_FAKE_AUTH") == "1" {
		violations = append(violations,
			"CUMORA_GO_FAKE_AUTH=1 — the auth middleware would trust the x-test-user header, an integration-test backdoor")
	}
	return violations
}

// EnvFallbackWarnings 返回非致命的提示日志(env → 文案);含生产侧仅
// Warn 不拦的项。CUMORA_SECRETS_KEY 是用时报错面(admin 加密路径 500,
// domains/admin 已显式报"not configured"),故生产缺失只提示不拒启。
func EnvFallbackWarnings(getenv func(string) string) []string {
	if getenv("NODE_ENV") == "production" {
		if strings.TrimSpace(getenv("CUMORA_SECRETS_KEY")) == "" {
			return []string{"CUMORA_SECRETS_KEY is not set — admin secret encryption will fail at use time"}
		}
		return nil
	}
	if strings.TrimSpace(getenv("AGENT_RUNTIME_SECRET")) == "" {
		return []string{"AGENT_RUNTIME_SECRET not set — using the dev fallback secret; production would refuse to start"}
	}
	return nil
}
