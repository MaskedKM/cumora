// cumora-stack —— Stack 管理 CLI 骨架(#281,ADR 0005 阶段 0b)。
//
// 本票只立 doctor/status 两个只读子命令:诊断面先行,消除"桌面无感知
// 停摆"(8-31 事故形态)。守护(stackd)与编排是 #282+,在此模块上生长。
// 路径默认值描述本部署布局(unit 注释同源),全部可用 flag/env 覆盖。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MaskedKM/cumora/apps/stack/internal/doctor"
	"github.com/MaskedKM/cumora/apps/stack/internal/engdirs"
	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/status"
)

// 生产三件套(#211 enable 链:sidecar → go → daemon)。
var defaultUnits = []string{"cumora-sidecar", "cumora-go", "cumora-daemon"}

// BYOA 引擎集合(CONTEXT.md Engine 词条:claude|codex|grok|cursor)。
var defaultEngines = []string{"claude", "codex", "grok", "cursor"}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "doctor":
		os.Exit(cmdDoctor(os.Args[2:]))
	case "status":
		cmdStatus(os.Args[2:])
	case "install":
		os.Exit(cmdInstall(os.Args[2:]))
	case "absorb":
		os.Exit(cmdAbsorb(os.Args[2:]))
	case "uninstall":
		os.Exit(cmdUninstall(os.Args[2:]))
	case "logs":
		os.Exit(cmdLogs(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cumora-stack — Cumora 本地栈管理 CLI(#281 骨架,#282 切换面)

用法:
  cumora-stack doctor [--json] [flags]   体检:为什么坏(任何 fail → 退出码 1)
  cumora-stack status [--json] [flags]   状态:现在跑得怎样(恒退出 0:doctor
                                         才是退出码门,status 只报告——脚本
                                         编排请以 doctor 为准)
  cumora-stack absorb <载荷目录> [flags] bootstrap(#283):制品面 → releases/
                                         <ver>/ + 原子切 current;MANIFEST
                                         逐文件 sha 校验,同版本重铺拒绝
  cumora-stack install                   切到单 unit 形态:装 cumora.service,
                                         停用旧三 unit(文件保留),stackd 接管
                                         (前置:current 制品已含 stackd)
  cumora-stack uninstall                 回滚:停用 cumora.service,恢复旧三 unit
  cumora-stack logs [-f] [--svc NAME]    stackd 及子进程日志(journal,svc 过滤)

通用 flags:
  --env-file PATH        主 .env   (默认 $CUMORA_ENV_FILE,或 ~/Code/cumora/.env)
  --units a,b,c          覆盖 unit 集合(默认 cumora-sidecar,cumora-go,cumora-daemon)
  --json                 机器可读输出

doctor 专用:
  --daemon-env-file PATH daemon.env(默认 $CUMORA_DAEMON_ENV_FILE,或 ~/.cumora/daemon.env)
  --engines a,b,c        覆盖引擎集合(默认 claude,codex,grok,cursor)
                         端口档暂为固定集(5181/5182 must、47823 desktop、
                         5432/6379 info)——#282 stackd 落地时随拓扑重开

status 专用:
  --livez-url URL        (默认 http://127.0.0.1:5181/api/livez)
  --healthz-url URL      (默认 http://127.0.0.1:5182/internal/healthz)
  --sid-token TOKEN      healthz Bearer(默认读 $YJS_SIDECAR_TOKEN 或 env-file)
  --current-dir PATH     (默认 $CUMORA_CURRENT_DIR,或 ~/.local/share/cumora/current)

absorb 专用:
  --releases-dir PATH    (默认 $CUMORA_RELEASES_DIR,或 ~/.local/share/cumora/releases)
  --current-dir PATH     (同 status)
`)
}

func envOr(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

func home(rel string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return rel
	}
	return filepath.Join(h, rel)
}

// commonFlags —— doctor/status 共用的 flag 面。
type commonFlags struct {
	json     bool
	envFile  string
	unitsCSV string
}

func parseCommon(fs *flag.FlagSet, c *commonFlags) {
	fs.BoolVar(&c.json, "json", false, "JSON 输出")
	fs.StringVar(&c.envFile, "env-file", envOr("CUMORA_ENV_FILE", home("Code/cumora/.env")), "主 .env 路径")
	fs.StringVar(&c.unitsCSV, "units", "", "覆盖 unit 集合(逗号分隔)")
}

func (c *commonFlags) units() []string {
	if c.unitsCSV == "" {
		return defaultUnits
	}
	return splitCSV(c.unitsCSV)
}

// splitCSV —— 逗号分隔清洗:trim 元素、丢空串("--units ' cumora-go'" 不
// 应探出幽灵 unit;同 server-go CORSOrigins 的 trim+filter 惯例)。
func splitCSV(s string) []string {
	var out []string
	for _, part := range splitComma(s) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	var c commonFlags
	parseCommon(fs, &c)
	daemonEnv := fs.String("daemon-env-file",
		envOr("CUMORA_DAEMON_ENV_FILE", home(".cumora/daemon.env")), "daemon.env 路径")
	enginesCSV := fs.String("engines", "", "覆盖引擎集合(逗号分隔;默认 claude,codex,grok,cursor)")
	_ = fs.Parse(args)

	engines := defaultEngines
	if *enginesCSV != "" {
		engines = splitCSV(*enginesCSV)
	}
	rep := doctor.Run(probe.NewDeps(), doctor.Config{
		EnvFile:       c.envFile,
		DaemonEnvFile: *daemonEnv,
		Units:         c.units(),
		StackdUnit:    envOr("CUMORA_STACKD_UNIT", "cumora.service"),
		StateFile:     envOr("CUMORA_STATE_FILE", home(".local/share/cumora/stackd-state.json")),
		StackAddrs: []doctor.AddrExpect{
			{Name: "server :5181", Addr: "127.0.0.1:5181", Kind: "must"},
			{Name: "sidecar :5182", Addr: "127.0.0.1:5182", Kind: "must"},
			{Name: "auth-loopback :47823", Addr: "127.0.0.1:47823", Kind: "desktop"},
			{Name: "postgres :5432", Addr: "127.0.0.1:5432", Kind: "info"},
			{Name: "redis :6379", Addr: "127.0.0.1:6379", Kind: "info"},
		},
		Engines:        engines,
		EngineExtraDir: engineDirs,
	})

	if c.json {
		printJSON(rep)
	} else {
		printDoctor(rep)
	}
	if rep.AnyFail {
		return 1
	}
	return 0
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var c commonFlags
	parseCommon(fs, &c)
	livez := fs.String("livez-url", "http://127.0.0.1:5181/api/livez", "livez URL")
	healthz := fs.String("healthz-url", "http://127.0.0.1:5182/internal/healthz", "healthz URL")
	sid := fs.String("sid-token", os.Getenv("YJS_SIDECAR_TOKEN"), "sidecar Bearer token")
	current := fs.String("current-dir",
		envOr("CUMORA_CURRENT_DIR", home(".local/share/cumora/current")), "current symlink 目录")
	_ = fs.Parse(args)

	// token 缺省时从 env 文件补读(doctor 已验过键在不在,这里只管取值)。
	token := *sid
	if token == "" {
		if data, err := os.ReadFile(c.envFile); err == nil {
			token = probe.ParseEnvFile(data)["YJS_SIDECAR_TOKEN"]
		}
	}
	versionFile := filepath.Join(*current, "VERSION")

	rep := status.Run(probe.NewDeps(), status.Config{
		Units:        c.units(),
		StackdUnit:   envOr("CUMORA_STACKD_UNIT", "cumora.service"),
		LivezURL:     *livez,
		HealthzURL:   *healthz,
		SidToken:     token,
		VersionFile:  versionFile,
		CurrentDir:   *current,
		StateFile:    envOr("CUMORA_STATE_FILE", home(".local/share/cumora/stackd-state.json")),
		ManifestFile: filepath.Join(*current, "MANIFEST"),
	})

	if c.json {
		printJSON(rep)
	} else {
		printStatus(rep)
	}
}

// engineDirs —— 引擎发现的额外目录(internal/engdirs 单源:doctor 的
// 显性化面与 #282 stackd 给 daemon 子进程钉扎 PATH 共用一份)。
func engineDirs() []string { return engdirs.Dirs("") }

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func printDoctor(rep doctor.Report) {
	group := ""
	mark := map[doctor.Status]string{doctor.OK: "✓", doctor.Warn: "!", doctor.Fail: "✗", doctor.Info: "·"}
	for _, c := range rep.Checks {
		if c.Group != group {
			group = c.Group
			fmt.Printf("[%s]\n", group)
		}
		line := fmt.Sprintf("  %s %s", mark[c.Status], c.Name)
		if c.Detail != "" {
			line += " — " + c.Detail
		}
		fmt.Println(line)
	}
	if rep.AnyFail {
		fmt.Println("结果: 存在 fail(退出码 1)")
	} else {
		fmt.Println("结果: 无 fail")
	}
}

func printStatus(rep status.Report) {
	fmt.Println("[units]")
	for _, u := range rep.Units {
		line := fmt.Sprintf("  %-16s %s/%s", u.Unit, u.Active, u.Sub)
		if u.Uptime != "" {
			line += "  up " + u.Uptime
		}
		if u.Error != "" {
			line += "  err: " + u.Error
		}
		fmt.Println(line)
	}
	fmt.Println("[probes]")
	fmt.Printf("  livez    %s — %s\n", rep.Livez.Status, rep.Livez.Detail)
	fmt.Printf("  healthz  %s — %s\n", rep.Healthz.Status, rep.Healthz.Detail)
	fmt.Println("[version]")
	fmt.Printf("  current  %s\n", orDash(rep.Current))
	fmt.Printf("  VERSION  %s\n", orDash(rep.Version))
	if rep.Manifest != nil {
		fmt.Println("[manifest]")
		fmt.Printf("  version  %s\n", rep.Manifest.Version)
		for _, k := range sortedDepKeys(rep.Manifest.Deps) {
			fmt.Printf("  %-12s %s\n", k, rep.Manifest.Deps[k])
		}
	}
	if rep.Stackd != nil {
		fmt.Printf("[stackd] instance=%s updated=%s\n",
			rep.Stackd.InstanceID, rep.Stackd.UpdatedAt.Format("15:04:05"))
		for _, ch := range rep.Stackd.Children {
			fmt.Printf("  %-10s running=%-5v restarts=%d circuit=%v %s\n",
				ch.Name, ch.Running, ch.Restarts, ch.CircuitOpen, orDash(ch.LastErr))
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sortedDepKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
