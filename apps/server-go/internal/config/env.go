// env —— 全仓 env 读取的单一事实源(#217)。
//
// 此前 74 处 os.Getenv 散落在 30 个包里(多在请求路径逐次读),同名
// 助手(envIntRaw×3 / getenv×2 / envInt×2 / envIntOr)各包自持副本。
// 此处收编为两层:
//
//   - 通用原语(Getenv/EnvOr/EnvInt/EnvIntRaw/EnvIntOr):解析语义只此
//     一份;EnvIntRaw 的 0/-1 透传是 TS「0=禁用」kill-switch 语义(#62
//     教训),合并时逐字保留,绝不用 EnvIntOr(0→默认)替换。
//   - 命名键访问器:键名只在此出现一次。访问器默认返回原始值(不
//     trim),调用点的变换原样保留;只有全部调用点变换一致时才把变换
//     烘进访问器(逐键注释标明)。
//
// 语义保留:全部访问器仍是调用时读取(与收编前的请求时逐次读等价,
// t.Setenv 的测试不受影响);启动时快照仅 Config.Load() 一面。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

/* ───────────── 通用原语 ───────────── */

// Getenv:无变换透传。参数化读键(键名在调用点拼装/查表)与一次性开关
// 经此收编;命名键优先用下面的访问器。
func Getenv(name string) string { return os.Getenv(name) }

// EnvOr:非空否则 fallback(空串 = 未设)。
func EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// EnvInt:Atoi 成功才生效,否则 fallback(空串/非数/含空格都回落)。
func EnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// EnvIntRaw:符号感知的环境整数(0/-1 原样返回);缺键/非数 → ok=false。
// 与 EnvIntOr(0→默认)相反——间隔类 env 的 TS 语义是"0=禁用",
// 必须让 0 活着到达调用方的禁用分支(#62 教训:0→fallback 会吞掉
// kill-switch)。
func EnvIntRaw(name string) (int64, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// EnvIntOr:纯十进制数字才生效;空/非数/0 都回 def(TS 同形助手的
// 语义:0 不当"显式设值"对待)。
func EnvIntOr(name string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n := int64(0)
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int64(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

/* ───────────── 运行模式 / 实例 ───────────── */

// NodeEnv:NODE_ENV 原始值。
func NodeEnv() string { return os.Getenv("NODE_ENV") }

// IsProduction:NODE_ENV=production 判定(原各点 `== "production"` /
// `!= "production"` 的统一形)。
func IsProduction() bool { return os.Getenv("NODE_ENV") == "production" }

// InstanceIDEnv:INSTANCE_ID 原始值(空 = 未设;默认形状由消费方各自
// 生成——webapp 的 app-<rand5> 与 Load() 的 app-go-<rand10> 有意不同)。
func InstanceIDEnv() string { return os.Getenv("INSTANCE_ID") }

// FakeAuth:CUMORA_GO_FAKE_AUTH 原始值(=1 时 httpx.Authn 信任
// x-test-user 头;生产拒启守卫见 prodguard.go)。
func FakeAuth() string { return os.Getenv("CUMORA_GO_FAKE_AUTH") }

/* ───────────── 邮件域 ───────────── */

// EmailDomainRaw:EMAIL_DOMAIN 原始值(waitlist 欢迎信等原样拼接处用)。
func EmailDomainRaw() string { return os.Getenv("EMAIL_DOMAIN") }

// EmailDomain:对齐退役 TS env.ts 的规范化——小写化、去首尾点(不 trim
// 空白);空 = 未配置。原 email.RootDomain 与 contacts.rootDomain 双份
// 同形实现的合一。
func EmailDomain() string {
	return strings.Trim(strings.ToLower(os.Getenv("EMAIL_DOMAIN")), ".")
}

// ResendAPIKey:RESEND_API_KEY 原始值(空 = mock 发送路径)。
func ResendAPIKey() string { return os.Getenv("RESEND_API_KEY") }

// EmailInboundHMACSecret:EMAIL_INBOUND_HMAC_SECRET 原始值(空 = inbound
// webhook 关闭)。
func EmailInboundHMACSecret() string { return os.Getenv("EMAIL_INBOUND_HMAC_SECRET") }

/* ───────────── 上传目录 ───────────── */

// UploadsDir:全仓 uploads 根的单一解析点(#208)——写侧(domains/
// uploads、agent/cli_storage)、读侧(webapp 静态服务、core/oauth 头像
// 镜像)、email 域(入站附件、GC 根)与 workspaces 默认区全部经此取
// 路径,禁止再散落字面量。
//
// 解析链:CUMORA_UPLOADS_DIR > UPLOAD_DIR(Go email 域 86aa85f 首创的
// 历史键——TS 时代 server/src/storage.ts 的 UPLOAD_DIR 是常量、从未读
// env;该键未被任何 .env.example/部署单元/文档文档化,保留兼容) >
// server/uploads(相对 cwd,与 TS 本地
// storage.put 同布局)。新值仅做 filepath.Clean(去尾斜杠,防
// Join/前缀校验形态漂移),空白语义保留:主键不 trim、旧键 trim。
func UploadsDir() string {
	if d := os.Getenv("CUMORA_UPLOADS_DIR"); d != "" {
		return filepath.Clean(d)
	}
	if d := strings.TrimSpace(os.Getenv("UPLOAD_DIR")); d != "" {
		return filepath.Clean(d)
	}
	return filepath.Join("server", "uploads")
}

/* ───────────── LLM 提供方 ───────────── */

// OpenAIAPIKey:OPENAI_API_KEY 原始值。
func OpenAIAPIKey() string { return os.Getenv("OPENAI_API_KEY") }

// OpenAIBaseURL:OPENAI_BASE_URL 原始值——两个消费点变换不同(agent
// memory trim+TrimRight,cli_llm legacy 分支原样),故不烘变换。
func OpenAIBaseURL() string { return os.Getenv("OPENAI_BASE_URL") }

// OpenAIModelSupport:OPENAI_MODEL_SUPPORT,缺省 gpt-5.4-mini(TS 同)。
func OpenAIModelSupport() string {
	if m := os.Getenv("OPENAI_MODEL_SUPPORT"); m != "" {
		return m
	}
	return "gpt-5.4-mini"
}

// OpenAIImageModel:OPENAI_IMAGE_MODEL,缺省 gpt-image-2(TS 同)。
func OpenAIImageModel() string {
	if m := os.Getenv("OPENAI_IMAGE_MODEL"); m != "" {
		return m
	}
	return "gpt-image-2"
}

// TestEmbedOverride:CUMORA_TEST_EMBED_OVERRIDE 原始值(测试确定性
// 嵌入注入,见 agent/memory.go EmbedText)。
func TestEmbedOverride() string { return os.Getenv("CUMORA_TEST_EMBED_OVERRIDE") }

// Sub2APIInternalURL / Sub2APIAdminKey / Sub2APIPublicURL:sub2api 路由
// 三键原始值(cli_llm 的 TrimRight 变换留在调用点)。
func Sub2APIInternalURL() string { return os.Getenv("SUB2API_INTERNAL_URL") }
func Sub2APIAdminKey() string    { return os.Getenv("SUB2API_ADMIN_KEY") }
func Sub2APIPublicURL() string   { return os.Getenv("SUB2API_PUBLIC_URL") }

// NovitaAPIKey / NovitaBaseURL:novita chat-completions 翻译分支两键
// 原始值。
func NovitaAPIKey() string  { return os.Getenv("NOVITA_API_KEY") }
func NovitaBaseURL() string { return os.Getenv("NOVITA_BASE_URL") }

// ModelPricesJSON:CUMORA_MODEL_PRICES_JSON 原始值(costing 价格表
// 覆盖;调用点 trim)。
func ModelPricesJSON() string { return os.Getenv("CUMORA_MODEL_PRICES_JSON") }

// SkillHubURL:SKILLHUB_URL 原始值(空 = SkillHub 未配置)。
func SkillHubURL() string { return os.Getenv("SKILLHUB_URL") }

/* ───────────── 邀请 / OAuth / 管理员 ───────────── */

// InviteBaseURL:INVITE_BASE_URL 原始值。
func InviteBaseURL() string { return os.Getenv("INVITE_BASE_URL") }

// AuthDoneURL:AUTH_DONE_URL 原始值(注意与 CUMORA_AUTH_DONE_URL 是两
// 个不同的键)。
func AuthDoneURL() string { return os.Getenv("AUTH_DONE_URL") }

// InviteSignInBase:INVITE_BASE_URL → AUTH_DONE_URL 回退链(原
// waitlist/invitations 双份同形链的合一;TrimRight 等后处理留调用点)。
func InviteSignInBase() string {
	base := os.Getenv("INVITE_BASE_URL")
	if base == "" {
		base = os.Getenv("AUTH_DONE_URL")
	}
	return base
}

// OAuthGoogleBase:CUMORA_OAUTH_GOOGLE_BASE 原始值(测试桩覆盖)。
func OAuthGoogleBase() string { return os.Getenv("CUMORA_OAUTH_GOOGLE_BASE") }

func GoogleClientID() string     { return os.Getenv("GOOGLE_CLIENT_ID") }
func GoogleClientSecret() string { return os.Getenv("GOOGLE_CLIENT_SECRET") }

// OAuthGitHubBase:CUMORA_OAUTH_GITHUB_BASE 原始值(测试桩覆盖)。
func OAuthGitHubBase() string { return os.Getenv("CUMORA_OAUTH_GITHUB_BASE") }

func GitHubClientID() string     { return os.Getenv("GITHUB_CLIENT_ID") }
func GitHubClientSecret() string { return os.Getenv("GITHUB_CLIENT_SECRET") }

// PublicOrigin:CUMORA_PUBLIC_ORIGIN 原始值。
func PublicOrigin() string { return os.Getenv("CUMORA_PUBLIC_ORIGIN") }

// CumoraAuthDoneURL:CUMORA_AUTH_DONE_URL 原始值(oauth 完成后回跳)。
func CumoraAuthDoneURL() string { return os.Getenv("CUMORA_AUTH_DONE_URL") }

// AuthReturnAllowlist:CUMORA_AUTH_RETURN_ALLOWLIST 原始值(逗号分隔
// 前缀;切分留调用点)。
func AuthReturnAllowlist() string { return os.Getenv("CUMORA_AUTH_RETURN_ALLOWLIST") }

// AdminEmails:CUMORA_ADMIN_EMAILS 解析为规范化条目(逗号切分、逐项
// TrimSpace+ToLower、丢弃空项)。原 admin/helpers.go 与 core/oauth.go
// 双份同形循环的合一——空条目原本就不可能匹配(调用点判 mine != ""),
// 丢弃等价。
func AdminEmails() []string {
	var out []string
	for _, e := range strings.Split(os.Getenv("CUMORA_ADMIN_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// AppleJWKSURL:CUMORA_APPLE_JWKS_URL 原始值(仅测试/开发桩覆盖;
// 空 = Apple 正源,默认值留调用点)。
func AppleJWKSURL() string { return os.Getenv("CUMORA_APPLE_JWKS_URL") }

// R2PublicBase:R2_PUBLIC_BASE 原始值(TrimRight "/" 留调用点)。
func R2PublicBase() string { return os.Getenv("R2_PUBLIC_BASE") }

/* ───────────── 密钥 / agent 运行时 ───────────── */

// SecretsKey:CUMORA_SECRETS_KEY 原始值(admin 加密面主键;
// key=sha256(此值))。
func SecretsKey() string { return os.Getenv("CUMORA_SECRETS_KEY") }

// AgentRuntimeSecret:AGENT_RUNTIME_SECRET 原始值(computers 的开发
// 回退与生产拒启守卫的分层不变)。
func AgentRuntimeSecret() string { return os.Getenv("AGENT_RUNTIME_SECRET") }

// RuntimeClient:CUMORA_RUNTIME_CLIENT 原始值(=http 时 CLI 身份可被
// env 钉死)。
func RuntimeClient() string { return os.Getenv("CUMORA_RUNTIME_CLIENT") }

// CLIIdentitySource:CUMORA_CLI_IDENTITY_SOURCE 原始值(=agent-bash
// 同上)。
func CLIIdentitySource() string { return os.Getenv("CUMORA_CLI_IDENTITY_SOURCE") }

// AgentIDEnv:CUMORA_AGENT_ID 原始值(可信来源下的身份钉死)。
func AgentIDEnv() string { return os.Getenv("CUMORA_AGENT_ID") }

// DefaultAs:CUMORA_DEFAULT_AS 原始值(开发便利的默认 --as)。
func DefaultAs() string { return os.Getenv("CUMORA_DEFAULT_AS") }

/* ───────────── 推送(APNs/FCM,全部 TrimSpace——原 7 处读点一致) ───────────── */

func APNSKeyPath() string { return strings.TrimSpace(os.Getenv("APNS_KEY_PATH")) }
func APNSKeyID() string   { return strings.TrimSpace(os.Getenv("APNS_KEY_ID")) }
func APNSTeamID() string  { return strings.TrimSpace(os.Getenv("APNS_TEAM_ID")) }
func APNSTopic() string   { return strings.TrimSpace(os.Getenv("APNS_TOPIC")) }
func APNSEnv() string     { return strings.TrimSpace(os.Getenv("APNS_ENV")) }

func FCMServiceAccountJSON() string { return strings.TrimSpace(os.Getenv("FCM_SERVICE_ACCOUNT_JSON")) }
func FCMServiceAccountPath() string { return strings.TrimSpace(os.Getenv("FCM_SERVICE_ACCOUNT_PATH")) }
