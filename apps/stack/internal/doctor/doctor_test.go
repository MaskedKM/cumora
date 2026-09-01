// doctor 单测(#281):红绿语义全覆盖,全部走 fake 探针,零宿主依赖。
package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
)

// fakeDeps —— 按测试意图组装的探针集。零值语义:pg/redis 健、unit
// active、must 端口开;要红哪一路就覆写哪一项。env 文件按路径含
// "daemon" 分流,支持主/副文件一读一错的组合场景。
type fakeDeps struct {
	envData       string
	envErr        error
	daemonEnvData string
	daemonEnvErr  error
	pgErr         error
	pgNoVector    bool
	redisErr      error
	unitStates    map[string]probe.UnitState
	unitErr       map[string]error
	closedPorts   map[string]bool
	missingEng    map[string]bool
}

func (f fakeDeps) deps() probe.Deps {
	return probe.Deps{
		ReadFile: func(path string) ([]byte, error) {
			if strings.Contains(path, "daemon") {
				if f.daemonEnvErr != nil {
					return nil, f.daemonEnvErr
				}
				return []byte(f.daemonEnvData), nil
			}
			if f.envErr != nil {
				return nil, f.envErr
			}
			return []byte(f.envData), nil
		},
		PG: func(context.Context, string) (probe.PGInfo, error) {
			if f.pgErr != nil {
				return probe.PGInfo{}, f.pgErr
			}
			return probe.PGInfo{Version: "PostgreSQL 16.15", PgvectorAvailable: !f.pgNoVector}, nil
		},
		Redis: func(context.Context, string) error { return f.redisErr },
		Systemd: func(unit string) (probe.UnitState, error) {
			if err, ok := f.unitErr[unit]; ok {
				return probe.UnitState{}, err
			}
			if st, ok := f.unitStates[unit]; ok {
				return st, nil
			}
			return probe.UnitState{Load: "loaded", Active: "active", Sub: "running"}, nil
		},
		Dial: func(addr string) error {
			if f.closedPorts[addr] {
				return errors.New("connection refused")
			}
			return nil
		},
		LookPath: func(name string, _ []string) (string, error) {
			if f.missingEng[name] {
				return "", errors.New("not found")
			}
			return "/fake/bin/" + name, nil
		},
	}
}

func testCfg() Config {
	return Config{
		EnvFile:       "/fake/.env",
		DaemonEnvFile: "/fake/daemon.env",
		Units:         []string{"cumora-sidecar", "cumora-go", "cumora-daemon"},
		StackAddrs: []AddrExpect{
			{Name: "server", Addr: "127.0.0.1:5181", Kind: "must"},
			{Name: "loopback", Addr: "127.0.0.1:47823", Kind: "desktop"},
		},
		Engines:        []string{"claude"},
		EngineExtraDir: func() []string { return nil },
	}
}

const fullEnv = `DATABASE_URL=postgres://x
CUMORA_SECRETS_KEY=k
GITHUB_CLIENT_ID=id
GITHUB_CLIENT_SECRET=s
CUMORA_AUTH_RETURN_ALLOWLIST=cumora://auth
YJS_SIDECAR_TOKEN=t
OPENAI_API_KEY=o
`

const fullDaemonEnv = "ANTHROPIC_AUTH_TOKEN=tok\n"

func find(t *testing.T, r Report, group, name string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Group == group && c.Name == name {
			return c
		}
	}
	t.Fatalf("找不到检查项 %s/%s", group, name)
	return Check{}
}

func TestAllGreen(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv}).deps(), testCfg())
	if r.AnyFail {
		t.Fatalf("全绿场景不应有 fail: %+v", r.Checks)
	}
	if find(t, r, "engines", "claude").Status != OK {
		t.Fatal("引擎应发现")
	}
}

func TestRedisDownFails(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, redisErr: errors.New("dial tcp refused")}).deps(), testCfg())
	c := find(t, r, "redis", "可达")
	if c.Status != Fail || !strings.Contains(c.Detail, "refused") {
		t.Fatalf("redis 断应红且带原因: %+v", c)
	}
	if !r.AnyFail {
		t.Fatal("AnyFail 应置位")
	}
}

func TestMissingOAuthKeyFails(t *testing.T) {
	broken := strings.Replace(fullEnv, "GITHUB_CLIENT_ID=id\n", "", 1)
	r := Run((fakeDeps{envData: broken}).deps(), testCfg())
	if c := find(t, r, "env", "GITHUB_CLIENT_ID"); c.Status != Fail {
		t.Fatalf("缺 OAuth 键应红: %+v", c)
	}
}

func TestWarnKeyMissingIsWarn(t *testing.T) {
	noOpenAI := strings.Replace(fullEnv, "OPENAI_API_KEY=o\n", "", 1)
	r := Run((fakeDeps{envData: noOpenAI}).deps(), testCfg())
	if c := find(t, r, "env", "OPENAI_API_KEY"); c.Status != Warn {
		t.Fatalf("降级键缺席应黄: %+v", c)
	}
	if r.AnyFail {
		t.Fatal("warn 不应置 AnyFail")
	}
}

func TestMaskedUnitFails(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, unitStates: map[string]probe.UnitState{
		"cumora-go": {Load: "masked", Active: "inactive", Sub: "dead"},
	}}).deps(), testCfg())
	c := find(t, r, "units", "cumora-go")
	if c.Status != Fail || !strings.Contains(c.Detail, "masked") {
		t.Fatalf("masked 应红且单列: %+v", c)
	}
}

func TestInactiveUnitFails(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, unitStates: map[string]probe.UnitState{
		"cumora-go": {Load: "loaded", Active: "failed", Sub: "failed"},
	}}).deps(), testCfg())
	if find(t, r, "units", "cumora-go").Status != Fail {
		t.Fatal("failed unit 应红")
	}
}

func TestPgvectorMissingFails(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, pgNoVector: true}).deps(), testCfg())
	if find(t, r, "postgres", "pgvector").Status != Fail {
		t.Fatal("无 pgvector 应红")
	}
}

func TestDesktopPortClosedIsWarn(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, closedPorts: map[string]bool{"127.0.0.1:47823": true}}).deps(), testCfg())
	if c := find(t, r, "ports", "loopback"); c.Status != Warn {
		t.Fatalf("桌面回环端口闭应黄不红: %+v", c)
	}
	if r.AnyFail {
		t.Fatal("仅桌面端口闭不应 AnyFail")
	}
}

func TestMustPortClosedFails(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, closedPorts: map[string]bool{"127.0.0.1:5181": true}}).deps(), testCfg())
	if find(t, r, "ports", "server").Status != Fail {
		t.Fatal("栈端口闭应红")
	}
}

func TestEnvFileUnreadableFails(t *testing.T) {
	r := Run((fakeDeps{envErr: errors.New("no such file")}).deps(), testCfg())
	if find(t, r, "env", ".env").Status != Fail {
		t.Fatal(".env 读不到应红")
	}
}

func TestNotFoundUnitFails(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, unitStates: map[string]probe.UnitState{
		"cumora-daemon": {Load: "not-found", Active: "inactive", Sub: "dead"},
	}}).deps(), testCfg())
	c := find(t, r, "units", "cumora-daemon")
	if c.Status != Fail || !strings.Contains(c.Detail, "未安装") {
		t.Fatalf("not-found 应红且提示未安装: %+v", c)
	}
}

func TestSystemctlUnavailableFails(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, unitErr: map[string]error{
		"cumora-go": errors.New("exec: systemctl: not found"),
	}}).deps(), testCfg())
	c := find(t, r, "units", "cumora-go")
	if c.Status != Fail || !strings.Contains(c.Detail, "systemctl") {
		t.Fatalf("systemctl 缺失应红且带原因: %+v", c)
	}
}

func TestDaemonTokenMissingIsWarn(t *testing.T) {
	// daemon.env 可读但无凭据(空文件)→ warn 不红。
	r := Run((fakeDeps{envData: fullEnv}).deps(), testCfg())
	if c := find(t, r, "env", "ANTHROPIC_AUTH_TOKEN"); c.Status != Warn {
		t.Fatalf("daemon 凭据缺席应黄: %+v", c)
	}
}

func TestDaemonEnvUnreadableIsWarn(t *testing.T) {
	// daemon.env 读不到 → 黄(引擎凭据缺失是降级,不是部署断裂)。
	r := Run((fakeDeps{envData: fullEnv, daemonEnvErr: errors.New("no such file")}).deps(), testCfg())
	if c := find(t, r, "env", "daemon.env"); c.Status != Warn {
		t.Fatalf("daemon.env 读不到应黄: %+v", c)
	}
	if r.AnyFail {
		t.Fatal("仅 daemon.env 读不到不应 AnyFail")
	}
}

func TestDaemonEnvPresentIsOk(t *testing.T) {
	r := Run((fakeDeps{envData: fullEnv, daemonEnvData: fullDaemonEnv}).deps(), testCfg())
	if c := find(t, r, "env", "ANTHROPIC_AUTH_TOKEN"); c.Status != OK {
		t.Fatalf("daemon 凭据在位应绿: %+v", c)
	}
}

func TestParseEnvFileVariants(t *testing.T) {
	m := probe.ParseEnvFile([]byte("# 注释\nA=1\nexport B=\"two words\"\nC='sq'\n\nD=\nbogus-line\n"))
	want := map[string]string{"A": "1", "B": "two words", "C": "sq", "D": ""}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("%s = %q, want %q", k, m[k], v)
		}
	}
	if _, ok := m["bogus-line"]; ok {
		t.Fatal("非键值行不应入表")
	}
}
