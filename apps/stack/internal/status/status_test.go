// status 单测(#281):livez/healthz 三态语义 + 版本报告,fake 注入。
package status

import (
	"errors"
	"testing"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
)

type fakeDeps struct {
	httpCodes map[string]int // url → code;缺省 = 网络错误
	httpErr   map[string]error
	units     map[string]probe.UnitState
	unitErr   map[string]error // systemctl 失败注入(非 systemd 环境)
	version   string           // VERSION 文件内容;空 = 读不到
	current   string           // EvalSymlinks 结果;空 = 错误
}

func (f fakeDeps) deps() probe.Deps {
	return probe.Deps{
		HTTP: func(url, _ string) (int, error) {
			if err, ok := f.httpErr[url]; ok {
				return 0, err
			}
			if c, ok := f.httpCodes[url]; ok {
				return c, nil
			}
			return 0, errors.New("connection refused")
		},
		Systemd: func(unit string) (probe.UnitState, error) {
			if err, ok := f.unitErr[unit]; ok {
				return probe.UnitState{}, err
			}
			if st, ok := f.units[unit]; ok {
				return st, nil
			}
			return probe.UnitState{Load: "loaded", Active: "active", Sub: "running",
				Timestamp: "Mon 2026-09-01 09:35:12 UTC"}, nil
		},
		ReadFile: func(string) ([]byte, error) {
			if f.version == "" {
				return nil, errors.New("no such file")
			}
			return []byte(f.version), nil
		},
		Readlink: func(string) (string, error) {
			if f.current == "" {
				return "", errors.New("no such dir")
			}
			return f.current, nil
		},
	}
}

const (
	livezURL   = "http://127.0.0.1:5181/api/livez"
	healthzURL = "http://127.0.0.1:5182/internal/healthz"
)

func cfg() Config {
	return Config{
		Units:       []string{"cumora-go"},
		LivezURL:    livezURL,
		HealthzURL:  healthzURL,
		VersionFile: "/fake/current/VERSION",
		CurrentDir:  "/fake/current",
	}
}

func TestLivezStates(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{200, "ok"},
		{503, "warn"}, // 依赖红的诚实信号,不是 livez 死
		{404, "warn"},
	}
	for _, c := range cases {
		r := Run((fakeDeps{httpCodes: map[string]int{livezURL: c.code, healthzURL: 200}}).deps(), cfg())
		if r.Livez.Status != c.want {
			t.Fatalf("livez %d → %s, want %s", c.code, r.Livez.Status, c.want)
		}
	}
	// 连接建立不起来 = fail。
	r := Run((fakeDeps{httpCodes: map[string]int{healthzURL: 200}}).deps(), cfg())
	if r.Livez.Status != "fail" {
		t.Fatalf("livez 连不上应 fail: %s", r.Livez.Status)
	}
}

func TestHealthzAuthSemantics(t *testing.T) {
	// 200 与 401 都算活着(与 cumora-sidecar.service 探针同语义)。
	for _, code := range []int{200, 401} {
		r := Run((fakeDeps{httpCodes: map[string]int{livezURL: 200, healthzURL: code}}).deps(), cfg())
		if r.Healthz.Status != "ok" {
			t.Fatalf("healthz %d → %s, want ok", code, r.Healthz.Status)
		}
	}
	r := Run((fakeDeps{httpCodes: map[string]int{livezURL: 200, healthzURL: 502}}).deps(), cfg())
	if r.Healthz.Status != "warn" {
		t.Fatalf("healthz 502 → %s, want warn", r.Healthz.Status)
	}
}

func TestVersionReport(t *testing.T) {
	r := Run((fakeDeps{httpCodes: map[string]int{livezURL: 200, healthzURL: 200},
		version: "v0.3.0-go.5\n", current: "/home/x/.local/share/cumora/releases/v0.3.0-go.5"}).deps(), cfg())
	if r.Version != "v0.3.0-go.5" {
		t.Fatalf("VERSION = %q", r.Version)
	}
	if r.Current == "" {
		t.Fatal("current 指向应报告")
	}
}

func TestUptimeFromTimestamp(t *testing.T) {
	r := Run((fakeDeps{httpCodes: map[string]int{livezURL: 200, healthzURL: 200}}).deps(), cfg())
	if r.Units[0].Uptime == "" {
		t.Fatal("可解析时间戳应得 uptime")
	}
	// 不可解析时间戳:Uptime 留空不猜,Active/Sub 照报。
	r2 := Run((fakeDeps{
		httpCodes: map[string]int{livezURL: 200, healthzURL: 200},
		units:     map[string]probe.UnitState{"cumora-go": {Load: "loaded", Active: "active", Sub: "running", Timestamp: "garbage"}},
	}).deps(), cfg())
	if r2.Units[0].Uptime != "" || r2.Units[0].Active != "active" {
		t.Fatalf("坏时间戳应只留空 uptime: %+v", r2.Units[0])
	}
}

func TestUnitErrorPopulated(t *testing.T) {
	// systemctl 失败(非 systemd 环境 / dbus 断):错误进 Error 字段,
	// 其余采集(livez/healthz/版本)照常 —— 单路失败不中断。
	r := Run((fakeDeps{
		httpCodes: map[string]int{livezURL: 200, healthzURL: 200},
		unitErr:   map[string]error{"cumora-go": errors.New("exec: systemctl: not found")},
	}).deps(), cfg())
	if r.Units[0].Error == "" {
		t.Fatalf("systemctl 失败应进 Error 字段: %+v", r.Units[0])
	}
	if r.Livez.Status != "ok" || r.Healthz.Status != "ok" {
		t.Fatal("unit 探测失败不应拖垮 HTTP 探测")
	}
}
