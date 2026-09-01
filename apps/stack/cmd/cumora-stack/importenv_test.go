// import-env 单测(#284):分类面、红线、凭据隔离(toml 零凭据)、
// 幂等拒覆盖、daemon.env 原样拷、键面等价(AC:现有部署导入后行为等价)。
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
)

// fixtureEnv —— 真实 .env 的形状(键面同构,值脱敏)。
const fixtureEnv = `NODE_ENV=production
DATABASE_URL=postgres://cumora:secret@127.0.0.1:5432/cumora
CUMORA_SECRETS_KEY=k
GITHUB_CLIENT_ID=iv
GITHUB_CLIENT_SECRET=sv
CUMORA_AUTH_RETURN_ALLOWLIST=x
YJS_SIDECAR_TOKEN=tv
OPENAI_API_KEY=ov
RESEND_API_KEY=rv
EMAIL_DOMAIN=example.com
R2_ENDPOINT=https://r2.invalid
R2_SECRET_ACCESS_KEY=r2v
`

func setupImport(t *testing.T) (envPath, daemonPath, cfgDir, dataHome string) {
	t.Helper()
	// redis 形态判定探针钉死:环境无关(存量部署面 = 系统位有应答)。
	importDeps = probe.Deps{Redis: func(context.Context, string) error { return nil }}
	t.Cleanup(func() { importDeps = probe.NewDeps() })
	dir := t.TempDir()
	envPath = filepath.Join(dir, "source.env")
	daemonPath = filepath.Join(dir, "source-daemon.env")
	cfgDir = filepath.Join(dir, "config", "cumora")
	dataHome = filepath.Join(dir, "data")
	if err := os.WriteFile(envPath, []byte(fixtureEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonPath, []byte("ANTHROPIC_AUTH_TOKEN=av\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return
}

func TestImportEnvExistingDeployment(t *testing.T) {
	envPath, daemonPath, cfgDir, dataHome := setupImport(t)
	code := cmdImportEnv([]string{"--env-file", envPath, "--daemon-env-file", daemonPath,
		"--config-dir", cfgDir, "--data-home", dataHome, "--json"})
	if code != 0 {
		t.Fatalf("存量部署导入应退 0(红线全在): %d", code)
	}

	// toml:机器事实 + external 形态(DATABASE_URL 在)。
	cfg, err := stackconfig.Load(filepath.Join(cfgDir, "stack.toml"))
	if err != nil {
		t.Fatalf("生成的 toml 必须可载: %v", err)
	}
	if cfg.PG.Mode != stackconfig.ModeExternal || cfg.Redis.Mode != stackconfig.ModeExternal {
		t.Fatalf("存量部署应 external: %+v %+v", cfg.PG, cfg.Redis)
	}
	if cfg.Data.Home != dataHome {
		t.Fatalf("data-home 覆盖: %s", cfg.Data.Home)
	}

	// stack.env:除机器事实外全键原样(键面等价)。
	data, err := os.ReadFile(filepath.Join(cfgDir, "stack.env"))
	if err != nil {
		t.Fatal(err)
	}
	got := probe.ParseEnvFile(data)
	src := probe.ParseEnvFile([]byte(fixtureEnv))
	if got["DATABASE_URL"] != src["DATABASE_URL"] ||
		got["GITHUB_CLIENT_SECRET"] != src["GITHUB_CLIENT_SECRET"] ||
		got["R2_SECRET_ACCESS_KEY"] != src["R2_SECRET_ACCESS_KEY"] {
		t.Fatal("凭据键应原样搬 stack.env")
	}
	for k := range src {
		if k == "CUMORA_UPLOADS_DIR" || k == "CUMORA_GO_LISTEN" || k == "YJS_SIDECAR_PORT" {
			continue // fixture 不含机器事实键
		}
		if got[k] != src[k] {
			t.Fatalf("键 %s 值漂移: %q → %q", k, src[k], got[k])
		}
	}

	// daemon.env 原样拷。
	denv, err := os.ReadFile(filepath.Join(cfgDir, "daemon.env"))
	if err != nil || strings.TrimSpace(string(denv)) != "ANTHROPIC_AUTH_TOKEN=av" {
		t.Fatalf("daemon.env 应原样拷: %q %v", denv, err)
	}

	// 权限:凭据房 0600。
	for _, f := range []string{"stack.env", "daemon.env"} {
		st, err := os.Stat(filepath.Join(cfgDir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s 应 0600: %v %v", f, st, err)
		}
	}
}

// AC3 的命令面半边:生成的 toml 不含任何源 env 的凭据值。
func TestImportEnvTomlHasNoCredentialValues(t *testing.T) {
	envPath, daemonPath, cfgDir, dataHome := setupImport(t)
	if code := cmdImportEnv([]string{"--env-file", envPath, "--daemon-env-file", daemonPath,
		"--config-dir", cfgDir, "--data-home", dataHome}); code != 0 {
		t.Fatalf("导入: %d", code)
	}
	toml, err := os.ReadFile(filepath.Join(cfgDir, "stack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "sv", "r2v", "tv", "ov", "av", "iv"} {
		if strings.Contains(string(toml), secret) {
			t.Errorf("toml 泄漏凭据值 %q:\n%s", secret, toml)
		}
	}
}

// 净机路径:无 DATABASE_URL → internal/internal。
func TestImportEnvCleanMachineInternalModes(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "fresh.env")
	env := strings.Replace(fixtureEnv, "DATABASE_URL=postgres://cumora:secret@127.0.0.1:5432/cumora\n", "", 1)
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dir, "cfg")
	importDeps = probe.Deps{Redis: func(context.Context, string) error { return context.DeadlineExceeded }}
	if code := cmdImportEnv([]string{"--env-file", envPath, "--config-dir", cfgDir,
		"--data-home", filepath.Join(dir, "data")}); code != 0 {
		t.Fatalf("导入: %d", code)
	}
	cfg, err := stackconfig.Load(filepath.Join(cfgDir, "stack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PG.Mode != stackconfig.ModeInternal || cfg.Redis.Mode != stackconfig.ModeInternal {
		t.Fatalf("净机应 internal: %+v %+v", cfg.PG, cfg.Redis)
	}
}

// 红线:GITHUB 缺失 → 退 1,报告点名(向导阻断面)。
func TestImportEnvRedlineMissingGithub(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "noauth.env")
	env := strings.Replace(fixtureEnv, "GITHUB_CLIENT_SECRET=sv\n", "", 1)
	if err := os.WriteFile(envPath, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dir, "cfg")
	code := cmdImportEnv([]string{"--env-file", envPath, "--config-dir", cfgDir,
		"--data-home", filepath.Join(dir, "data")})
	if code != 1 {
		t.Fatalf("缺 GITHUB_CLIENT_SECRET 应退 1: %d", code)
	}
	// 红线不阻断产物落盘(向导要先看到全貌再让用户补)。
	if _, err := os.Stat(filepath.Join(cfgDir, "stack.toml")); err != nil {
		t.Fatalf("红线时产物仍应落盘: %v", err)
	}
}

// 幂等护栏:目标已存在无 --force 拒覆盖。
func TestImportEnvRefusesOverwriteWithoutForce(t *testing.T) {
	envPath, daemonPath, cfgDir, dataHome := setupImport(t)
	args := []string{"--env-file", envPath, "--daemon-env-file", daemonPath,
		"--config-dir", cfgDir, "--data-home", dataHome}
	if code := cmdImportEnv(args); code != 0 {
		t.Fatalf("首导: %d", code)
	}
	tomlBefore, _ := os.ReadFile(filepath.Join(cfgDir, "stack.toml"))
	// 手工修订 toml(改端口),重跑无 --force → 修订保留。
	os.WriteFile(filepath.Join(cfgDir, "stack.toml"),
		[]byte(strings.Replace(string(tomlBefore), "sidecar_port = 5182", "sidecar_port = 15182", 1)), 0o644)
	if code := cmdImportEnv(args); code != 0 {
		t.Fatalf("重跑(全拒覆盖)应仍退 0: %d", code)
	}
	after, _ := os.ReadFile(filepath.Join(cfgDir, "stack.toml"))
	if !strings.Contains(string(after), "sidecar_port = 15182") {
		t.Fatal("无 --force 不得抹掉手工修订")
	}
	// --force 才覆盖回生成值。
	if code := cmdImportEnv(append(args, "--force")); code != 0 {
		t.Fatalf("force 重跑: %d", code)
	}
	forced, _ := os.ReadFile(filepath.Join(cfgDir, "stack.toml"))
	if strings.Contains(string(forced), "sidecar_port = 15182") {
		t.Fatal("--force 应重新生成")
	}
}

// dry-run 零落盘。
func TestImportEnvDryRunWritesNothing(t *testing.T) {
	envPath, daemonPath, cfgDir, dataHome := setupImport(t)
	if code := cmdImportEnv([]string{"--env-file", envPath, "--daemon-env-file", daemonPath,
		"--config-dir", cfgDir, "--data-home", dataHome, "--dry-run"}); code != 0 {
		t.Fatalf("dry-run: %d", code)
	}
	for _, f := range []string{"stack.toml", "stack.env", "daemon.env"} {
		if _, err := os.Stat(filepath.Join(cfgDir, f)); !os.IsNotExist(err) {
			t.Errorf("dry-run 不应落盘 %s", f)
		}
	}
}

// 机器事实键转 toml 后不再留在 stack.env(单一事实源)。
func TestImportEnvMachineFactsLeaveEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "mf.env")
	body := fixtureEnv + "CUMORA_GO_LISTEN=127.0.0.1:15181\nYJS_SIDECAR_PORT=15182\nCUMORA_UPLOADS_DIR=/srv/up\n"
	if err := os.WriteFile(envPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(dir, "cfg")
	if code := cmdImportEnv([]string{"--env-file", envPath, "--config-dir", cfgDir,
		"--data-home", filepath.Join(dir, "data")}); code != 0 {
		t.Fatalf("导入: %d", code)
	}
	cfg, err := stackconfig.Load(filepath.Join(cfgDir, "stack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Net.ServerAddr != "127.0.0.1:15181" || cfg.Net.SidecarPort != 15182 || cfg.Data.UploadsDir != "/srv/up" {
		t.Fatalf("机器事实未转 toml: %+v %+v %+v", cfg.Net, cfg.Data, cfg.Data)
	}
	data, _ := os.ReadFile(filepath.Join(cfgDir, "stack.env"))
	if strings.Contains(string(data), "CUMORA_GO_LISTEN=") ||
		strings.Contains(string(data), "YJS_SIDECAR_PORT=") ||
		strings.Contains(string(data), "CUMORA_UPLOADS_DIR=") {
		t.Fatalf("机器事实键应离开 stack.env:\n%s", data)
	}
}

// JSON 报告键名面:无值(抽检红线与可选段)。
func TestImportEnvJSONReportShape(t *testing.T) {
	envPath, daemonPath, cfgDir, dataHome := setupImport(t)
	// stdout 捕获:json 输出走 os.Stdout,借 pipe 抓。
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code := cmdImportEnv([]string{"--env-file", envPath, "--daemon-env-file", daemonPath,
		"--config-dir", cfgDir, "--data-home", dataHome, "--json"})
	w.Close()
	os.Stdout = old
	out := make([]byte, 1<<16)
	n, _ := r.Read(out)
	if code != 0 {
		t.Fatalf("导入: %d", code)
	}
	var rep ImportReport
	if err := json.Unmarshal(out[:n], &rep); err != nil {
		t.Fatalf("JSON 报告解析: %v\n%s", err, out[:n])
	}
	if len(rep.MissingRequired) != 0 || len(rep.SourceKeys) == 0 {
		t.Fatalf("报告面: %+v", rep)
	}
	joined := string(out[:n])
	for _, secret := range []string{"secret", "r2v", "sv"} {
		if strings.Contains(joined, secret) {
			t.Errorf("JSON 报告不得含值 %q:\n%s", secret, joined)
		}
	}
	if len(rep.OptionalPresent) == 0 || rep.OptionalPresent[0] != "R2_ENDPOINT" {
		t.Fatalf("R2_* 应标可选: %v", rep.OptionalPresent)
	}
}
