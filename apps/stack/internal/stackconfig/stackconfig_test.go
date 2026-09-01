package stackconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStrictUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stack.toml")
	write(t, p, "version = 1\n[net]\nserver_addr = \"127.0.0.1:5181\"\nsidecar_port = 5182\nbogus_port = 9999\n")
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "未知字段") || !strings.Contains(err.Error(), "net.bogus_port") {
		t.Fatalf("未知键应报红并点名键: got %v", err)
	}
}

func TestLoadStrictBadValue(t *testing.T) {
	cases := map[string]string{
		"版本不符":     "version = 2\n",
		"非绝对 home": "version = 1\ndata = { home = \"relative/x\" }\n",
		"坏 addr":   "version = 1\nnet = { server_addr = \"no-port\", sidecar_port = 5182 }\n",
		"端口越界":     "version = 1\nnet = { server_addr = \"127.0.0.1:5181\", sidecar_port = 70000 }\n",
		"坏 mode":   "version = 1\npg = { mode = \"magical\" }\n",
		"保留份数越界":   "version = 1\nstack = { keep_releases = 0 }\n",
	}
	for name, body := range cases {
		p := filepath.Join(t.TempDir(), "stack.toml")
		write(t, p, body)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: 应校验失败", name)
		}
	}
}

func TestLoadTildeExpansion(t *testing.T) {
	t.Setenv("HOME", "/home/fake")
	p := filepath.Join(t.TempDir(), "stack.toml")
	write(t, p, "version = 1\ndata = { home = \"~/data\" }\n")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Data.Home != "/home/fake/data" {
		t.Fatalf("~ 应展开: %q", c.Data.Home)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, _, err := LoadOrDefaults(filepath.Join(t.TempDir(), "absent.toml")); err != nil {
		t.Fatalf("缺文件应走缺省: %v", err)
	}
}

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	c := Defaults()
	c.Data.Home = dir
	c.PG.Mode = ModeInternal
	c.Redis.Mode = ModeInternal
	c.Engines.ExtraDirs = []string{"/opt/engines", "/home/x/tools"}
	c.Net.ServerAddr = "127.0.0.1:15181"
	c.Net.SidecarPort = 15182
	if err := Save(filepath.Join(dir, "stack.toml"), c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(filepath.Join(dir, "stack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Net != c.Net || got.PG != c.PG || got.Redis != c.Redis || got.Stack != c.Stack ||
		got.Data.Home != c.Data.Home || got.Data.UploadsDir != c.Data.UploadsDir ||
		strings.Join(got.Engines.ExtraDirs, ",") != strings.Join(c.Engines.ExtraDirs, ",") {
		t.Fatalf("往返不一致:\n got %+v\nwant %+v", got, c)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	c := Defaults()
	c.Net.ServerAddr = "broken"
	if err := Save(filepath.Join(t.TempDir(), "x.toml"), c); err == nil {
		t.Fatal("非法配置不应落盘")
	}
}

func TestDerivedPaths(t *testing.T) {
	c := Defaults()
	c.Data.Home = "/data/cumora"
	if got, want := c.ReleasesDir(), "/data/cumora/releases"; got != want {
		t.Errorf("releases: %s", got)
	}
	if got, want := c.CurrentDir(), "/data/cumora/current"; got != want {
		t.Errorf("current: %s", got)
	}
	if got, want := c.PGDataDir(), "/data/cumora/pgdata"; got != want {
		t.Errorf("pgdata: %s", got)
	}
	if got, want := c.RedisSocket(), "/data/cumora/run/redis.sock"; got != want {
		t.Errorf("redis socket: %s", got)
	}
	if got, want := c.UploadsDir(), "/data/cumora/uploads"; got != want {
		t.Errorf("uploads 缺省: %s", got)
	}
	c.Data.UploadsDir = "/srv/uploads"
	if got := c.UploadsDir(); got != "/srv/uploads" {
		t.Errorf("uploads 覆盖: %s", got)
	}
	if !strings.Contains(c.InternalDSN(), "host=/data/cumora/run") || !strings.Contains(c.InternalDSN(), "dbname=cumora") {
		t.Errorf("InternalDSN 形态: %s", c.InternalDSN())
	}
	if got, want := c.InternalRedisURL(), "unix:///data/cumora/run/redis.sock"; got != want {
		t.Errorf("redis URL: %s", got)
	}
}

func TestSaveFileHasNoSecretMarkers(t *testing.T) {
	// 生成器绝不写出任何凭据字段:模板面即 schema 面(结构体无凭据字段,
	// 模板手写有漂移风险——锁死注释性断言)。
	dir := t.TempDir()
	if err := Save(filepath.Join(dir, "stack.toml"), Defaults()); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "stack.toml"))
	// 断言赋值键形态(而非注释措辞):生成的 toml 里不得出现任何凭据键名。
	for _, banned := range []string{"DATABASE_URL", "GITHUB_", "OPENAI_", "RESEND_", "ANTHROPIC_", "R2_", "_TOKEN", "_SECRET", "_KEY"} {
		if strings.Contains(string(data), banned) {
			t.Errorf("生成 toml 不应含凭据键 %q:\n%s", banned, data)
		}
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
