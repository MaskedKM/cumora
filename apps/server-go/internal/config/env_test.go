package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

// EnvIntRaw 的 0/-1 透传是「0=禁用」kill-switch 语义(#62 教训)——
// 这些用例锁死:合并/复用时绝不可换成 EnvIntOr。
func TestEnvIntRaw(t *testing.T) {
	const k = "CUMORA_TEST_ENVINTRAW"
	for _, tc := range []struct {
		val  string
		want int64
		ok   bool
	}{
		{"", 0, false},      // 未设
		{"abc", 0, false},   // 非数
		{"1.5", 0, false},   // 非整数
		{"0", 0, true},      // 0 原样透传(禁用分支)
		{"-1", -1, true},    // 负数原样透传
		{"42", 42, true},    //
		{" 7 ", 7, true},    // 首尾空白容忍
		{"+8", 8, true},     // ParseInt 接受显式符号
		{"007", 7, true},    // 前导零
		{"1_000", 0, false}, // ParseInt(base 显式)拒下划线
	} {
		t.Setenv(k, tc.val)
		if tc.val == "" {
			t.Setenv(k, "") // 显式空 = 未设
		}
		got, ok := EnvIntRaw(k)
		if got != tc.want || ok != tc.ok {
			t.Errorf("EnvIntRaw(%q) = (%d,%v), want (%d,%v)", tc.val, got, ok, tc.want, tc.ok)
		}
	}
}

// EnvIntOr:0 不当作显式设值(与 EnvIntRaw 相反的家族);纯十进制数字
// 才生效。
func TestEnvIntOr(t *testing.T) {
	const k = "CUMORA_TEST_ENVINTOR"
	for _, tc := range []struct {
		val  string
		want int64
	}{
		{"", 99},
		{"abc", 99},
		{"-1", 99}, // 负号非数字字符 → 默认
		{"+7", 99}, // 显式符号拒
		{"0", 99},  // 0 → 默认(家族语义)
		{" 5 ", 5}, // 首尾空白容忍
		{"5x", 99}, // 混合 → 默认
		{"12", 12}, //
	} {
		t.Setenv(k, tc.val)
		if got := EnvIntOr(k, 99); got != tc.want {
			t.Errorf("EnvIntOr(%q, 99) = %d, want %d", tc.val, got, tc.want)
		}
	}
}

func TestEnvIntAndEnvOr(t *testing.T) {
	const ki = "CUMORA_TEST_ENVINT"
	t.Setenv(ki, "")
	if got := EnvInt(ki, 7); got != 7 {
		t.Errorf("empty: got %d", got)
	}
	t.Setenv(ki, "abc")
	if got := EnvInt(ki, 7); got != 7 {
		t.Errorf("non-numeric: got %d", got)
	}
	t.Setenv(ki, "0")
	if got := EnvInt(ki, 7); got != 0 {
		t.Errorf("zero is honored here (not a kill-switch family): got %d", got)
	}

	const ks = "CUMORA_TEST_ENVOR"
	t.Setenv(ks, "")
	if got := EnvOr(ks, "d"); got != "d" {
		t.Errorf("empty: got %q", got)
	}
	t.Setenv(ks, " ")
	if got := EnvOr(ks, "d"); got != " " {
		t.Errorf("whitespace is a value (no trim): got %q", got)
	}
}

// 烘进访问器的变换逐键锁死(与各原调用点逐字等价)。
func TestBakedAccessors(t *testing.T) {
	t.Setenv("EMAIL_DOMAIN", ".ExAmple.COM.")
	// 规范化:小写 + 去首尾点,不 trim 空白(TS env.ts 同)。
	if got := EmailDomain(); got != "example.com" {
		t.Errorf("EmailDomain = %q", got)
	}
	t.Setenv("EMAIL_DOMAIN", " ExAmple.COM. ")
	if got := EmailDomain(); got != " example.com. " {
		t.Errorf("EmailDomain must not trim whitespace: %q", got)
	}
	if got := EmailDomainRaw(); got != " ExAmple.COM. " {
		t.Errorf("EmailDomainRaw = %q", got)
	}

	t.Setenv("CUMORA_ADMIN_EMAILS", " A@x.com ,, b@y.com ,")
	if got := AdminEmails(); !reflect.DeepEqual(got, []string{"a@x.com", "b@y.com"}) {
		t.Errorf("AdminEmails = %v", got)
	}

	// #208:三段解析链 CUMORA_UPLOADS_DIR > UPLOAD_DIR(旧键)> 默认,
	// 各段语义见 env.go 注释(主键不 trim、旧键 trim、Clean 去尾斜杠)。
	t.Setenv("CUMORA_UPLOADS_DIR", "")
	t.Setenv("UPLOAD_DIR", "")
	if got := UploadsDir(); got != filepath.Join("server", "uploads") {
		t.Errorf("UploadsDir fallback = %q", got)
	}
	t.Setenv("CUMORA_UPLOADS_DIR", " /data/up ") // 主键不 trim(原调用点语义)
	if got := UploadsDir(); got != " /data/up " {
		t.Errorf("UploadsDir raw = %q", got)
	}
	t.Setenv("CUMORA_UPLOADS_DIR", "/data/up/") // 尾斜杠 Clean 掉,防前缀校验漂移
	if got := UploadsDir(); got != "/data/up" {
		t.Errorf("UploadsDir clean = %q", got)
	}
	// 主键空 + 旧键 UPLOAD_DIR(TS 时代 email 域契约):trim 后生效。
	t.Setenv("CUMORA_UPLOADS_DIR", "")
	t.Setenv("UPLOAD_DIR", "  ")
	if got := UploadsDir(); got != filepath.Join("server", "uploads") {
		t.Errorf("UploadsDir blank legacy = %q", got)
	}
	t.Setenv("UPLOAD_DIR", " /data/mail ")
	if got := UploadsDir(); got != "/data/mail" {
		t.Errorf("UploadsDir legacy trim = %q", got)
	}
	// 主键优先于旧键。
	t.Setenv("CUMORA_UPLOADS_DIR", "/data/up")
	t.Setenv("UPLOAD_DIR", "/legacy ")
	if got := UploadsDir(); got != "/data/up" {
		t.Errorf("UploadsDir primary precedence = %q", got)
	}
	t.Setenv("UPLOAD_DIR", "")
	t.Setenv("CUMORA_UPLOADS_DIR", "")

	t.Setenv("INVITE_BASE_URL", "")
	t.Setenv("AUTH_DONE_URL", "https://done/")
	if got := InviteSignInBase(); got != "https://done/" {
		t.Errorf("InviteSignInBase chain = %q", got)
	}
	t.Setenv("INVITE_BASE_URL", "https://invite/")
	if got := InviteSignInBase(); got != "https://invite/" {
		t.Errorf("InviteSignInBase fallback = %q", got)
	}
	// 前缀键优先(TS env.ts/.env.example 的文档化键名,同族审计 P0-2):
	// CUMORA_INVITE_BASE_URL > INVITE_BASE_URL > CUMORA_AUTH_DONE_URL > AUTH_DONE_URL。
	t.Setenv("CUMORA_INVITE_BASE_URL", "https://invite-prefixed/")
	if got := InviteSignInBase(); got != "https://invite-prefixed/" {
		t.Errorf("InviteSignInBase prefixed primary = %q", got)
	}
	t.Setenv("CUMORA_INVITE_BASE_URL", "")
	t.Setenv("INVITE_BASE_URL", "")
	t.Setenv("CUMORA_AUTH_DONE_URL", "https://done-prefixed/")
	if got := InviteSignInBase(); got != "https://done-prefixed/" {
		t.Errorf("InviteSignInBase prefixed fallback = %q", got)
	}
	t.Setenv("CUMORA_AUTH_DONE_URL", "")

	t.Setenv("OPENAI_MODEL_SUPPORT", "")
	if got := OpenAIModelSupport(); got != "gpt-5.4-mini" {
		t.Errorf("OpenAIModelSupport default = %q", got)
	}
	t.Setenv("OPENAI_IMAGE_MODEL", "")
	if got := OpenAIImageModel(); got != "gpt-image-2" {
		t.Errorf("OpenAIImageModel default = %q", got)
	}

	t.Setenv("NODE_ENV", "production")
	if !IsProduction() {
		t.Error("IsProduction")
	}
	t.Setenv("NODE_ENV", " production ") // 不 trim,非 production
	if IsProduction() {
		t.Error("IsProduction must not trim")
	}

	t.Setenv("APNS_KEY_ID", " K1 ")
	if got := APNSKeyID(); got != "K1" {
		t.Errorf("APNSKeyID trim = %q", got)
	}
	t.Setenv("FCM_SERVICE_ACCOUNT_JSON", " {} ")
	if got := FCMServiceAccountJSON(); got != "{}" {
		t.Errorf("FCMServiceAccountJSON trim = %q", got)
	}
}

// Load 的监听默认:#217 后不设 env 只绑回环。
func TestLoadListenDefaultBindsLoopback(t *testing.T) {
	t.Setenv("CUMORA_GO_LISTEN", "")
	cfg := Load()
	if cfg.ListenAddr != "127.0.0.1:5190" {
		t.Errorf("ListenAddr default = %q, want loopback-only", cfg.ListenAddr)
	}
	t.Setenv("CUMORA_GO_LISTEN", ":5190")
	if got := Load().ListenAddr; got != ":5190" {
		t.Errorf("explicit override honored: %q", got)
	}
	// 默认 sidecar URL 与 sidecar 自持默认端口(5182)对齐。
	t.Setenv("YJS_SIDECAR_URL", "")
	if got := Load().YjsSidecarURL; got != "http://127.0.0.1:5182" {
		t.Errorf("YjsSidecarURL default = %q", got)
	}
}
