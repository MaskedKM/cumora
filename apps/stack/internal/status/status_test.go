// status 单测(#281):livez/healthz 三态语义 + 版本报告,fake 注入。
package status

import (
	"errors"
	"strings"
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
	stateJSON string           // stackd-state.json 内容;空 = 读不到
	manifest  string           // MANIFEST 内容;空 = 读不到
}

// withStateFile —— 状态文件内容注入(返回配好的 deps)。
func (f fakeDeps) withStateFile(s string) probe.Deps {
	f.stateJSON = s
	return f.deps()
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
		ReadFile: func(path string) ([]byte, error) {
			if strings.HasSuffix(path, "stackd-state.json") {
				if f.stateJSON == "" {
					return nil, errors.New("no such file")
				}
				return []byte(f.stateJSON), nil
			}
			if strings.HasSuffix(path, "MANIFEST") {
				if f.manifest == "" {
					return nil, errors.New("no such file")
				}
				return []byte(f.manifest), nil
			}
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
		Units:        []string{"cumora-go"},
		LivezURL:     livezURL,
		HealthzURL:   healthzURL,
		VersionFile:  "/fake/current/VERSION",
		CurrentDir:   "/fake/current",
		StateFile:    "/fake/current/stackd-state.json",
		ManifestFile: "/fake/current/MANIFEST",
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

func TestUnitsSectionByForm(t *testing.T) {
	// #298:单 unit 形态(LoadState=loaded)→ [units] 只报 stackd unit;
	// 三 unit 形态 → 旧 unit 集照旧。
	base := fakeDeps{httpCodes: map[string]int{livezURL: 200, healthzURL: 200}}
	c := cfg()
	c.StackdUnit = "cumora.service"

	r := Run(base.deps(), c) // fake 缺省:cumora.service loaded/active
	if len(r.Units) != 1 || r.Units[0].Unit != "cumora.service" {
		t.Fatalf("单 unit 形态应只报 cumora.service: %+v", r.Units)
	}

	r2 := Run((fakeDeps{
		httpCodes: base.httpCodes,
		units:     map[string]probe.UnitState{"cumora.service": {Load: "not-found", Active: "inactive", Sub: "dead"}},
	}).deps(), c)
	if len(r2.Units) != 1 || r2.Units[0].Unit != "cumora-go" {
		t.Fatalf("三 unit 形态应报旧 unit 集: %+v", r2.Units)
	}

	// 未配置 StackdUnit(空串)= 不感知,行为与旧版一致。
	r3 := Run(base.deps(), cfg())
	if len(r3.Units) != 1 || r3.Units[0].Unit != "cumora-go" {
		t.Fatalf("未配置形态感知时行为不变: %+v", r3.Units)
	}
}

func TestManifestSection(t *testing.T) {
	// #283 PR-B:current/MANIFEST 可读 → Manifest 段带出;读不到(旧
	// release 无清单)= 省略(omitempty,JSON 契约向后兼容)。
	state := `{"instanceId":"x","updatedAt":"2026-09-01T12:00:00Z","children":[]}`
	mf := `{"version":"0.4.0","files":{"cumora-server":"ab"},"deps":{"postgresql":{"version":"16.15","sourceSha256":"c1"},"redis":{"version":"7.2.16","sourceSha256":"96"}}}`
	r := Run((fakeDeps{
		httpCodes: map[string]int{livezURL: 200, healthzURL: 200},
		version:   "0.4.0\n", current: "/x/releases/v0.4.0",
		stateJSON: state, manifest: mf,
	}).deps(), cfg())
	if r.Manifest == nil {
		t.Fatal("MANIFEST 可读应带出 Manifest 段")
	}
	if r.Manifest.Version != "0.4.0" || r.Manifest.Deps["postgresql"] != "16.15" || r.Manifest.Deps["redis"] != "7.2.16" {
		t.Fatalf("Manifest 段内容: %+v", r.Manifest)
	}

	r2 := Run((fakeDeps{
		httpCodes: map[string]int{livezURL: 200, healthzURL: 200},
		version:   "v0.3.0-go.8\n", current: "/x/releases/v0.3.0-go.8",
	}).deps(), cfg())
	if r2.Manifest != nil {
		t.Fatalf("无 MANIFEST 时段应省略: %+v", r2.Manifest)
	}
}

func TestStackdSectionFromStateFile(t *testing.T) {
	// 单 unit 形态:状态文件可读 → Stackd 段带出(阶段 3 JSON 契约)。
	state := `{"instanceId":"stackd-ab12cd34-1000","updatedAt":"2026-09-01T12:00:00Z",
	  "children":[{"name":"server","running":true,"pid":123,"restarts":0}]}`
	r := Run((fakeDeps{
		httpCodes: map[string]int{livezURL: 200, healthzURL: 200},
		version:   "v0.4.0\n", current: "/x/releases/v0.4.0",
	}).withStateFile(state), cfg())
	if r.Stackd == nil {
		t.Fatal("状态文件可读应带出 Stackd 段")
	}
	if r.Stackd.InstanceID != "stackd-ab12cd34-1000" || len(r.Stackd.Children) != 1 {
		t.Fatalf("Stackd 段内容: %+v", r.Stackd)
	}
	if !r.Stackd.Children[0].Running {
		t.Fatal("children[0] 应 running")
	}
	// 三 unit 形态:无状态文件 → 段省略(omitempty 契约)。
	r2 := Run((fakeDeps{
		httpCodes: map[string]int{livezURL: 200, healthzURL: 200},
	}).deps(), cfg())
	if r2.Stackd != nil {
		t.Fatalf("无状态文件时 Stackd 段应省略: %+v", r2.Stackd)
	}
}
