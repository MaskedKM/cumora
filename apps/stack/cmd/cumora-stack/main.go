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
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
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
	case "releases":
		os.Exit(cmdReleases(os.Args[2:]))
	case "restart":
		os.Exit(cmdRestart(os.Args[2:]))
	case "rollback":
		os.Exit(cmdRollback(os.Args[2:]))
	case "uninstall":
		os.Exit(cmdUninstall(os.Args[2:]))
	case "import-env":
		os.Exit(cmdImportEnv(os.Args[2:]))
	case "migrate-pg":
		os.Exit(cmdMigratePG(os.Args[2:]))
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
  cumora-stack absorb [flags] <载荷目录>  bootstrap(#283):制品面 → releases/
                                         <ver>/ + 原子切 current;MANIFEST
                                         逐文件 sha 校验,同版本重铺拒绝
                                         (flags 须在目录参数前)
  cumora-stack install                   切到单 unit 形态:装 cumora.service,
                                         停用旧三 unit(文件保留),stackd 接管
                                         (前置:current 制品已含 stackd)
  cumora-stack uninstall                 回滚:停用 cumora.service,恢复旧三 unit
  cumora-stack logs [-f] [--svc NAME]    stackd 及子进程日志(journal,svc 过滤)
  cumora-stack releases [--json]        releases 清单(#286 管理面):版本/
                                         迁移数/当前标记/回滚安全门
  cumora-stack restart                  form-aware 重启(单 unit 或三 unit)
  cumora-stack rollback <版本>           current 切回旧 release + 重启;
                                         迁移数回退=拒绝(pg 不可逆明示)
  cumora-stack import-env [flags]       首启向导命令面(#284):.env/daemon.env
                                         一次性导入 —— 机器事实转 stack.toml,
                                         凭据原样搬 stack.env/daemon.env;
                                         GITHUB OAuth 缺失=红线退 1
  cumora-stack migrate-pg [flags]       存量迁入(#285):停链→备份源库→
                                         恢复进内置 pg→行数比对→toml 切
                                         internal→起链;幂等(重跑 no-op,
                                         --force 重做);源库全程只读

通用 flags(路径缺省优先级:flag > env > stack.toml > 内置布局;
stack.toml 位 = $CUMORA_CONFIG_FILE,缺省 ~/.config/cumora/stack.toml):
  --env-file PATH        主 .env   (默认 $CUMORA_ENV_FILE;缺省先取规范位
                         ~/.config/cumora/stack.env,不存在回退 ~/Code/cumora/.env)
  --units a,b,c          覆盖 unit 集合(默认 cumora-sidecar,cumora-go,cumora-daemon)
  --json                 机器可读输出

doctor 专用:
  --daemon-env-file PATH daemon.env(默认 $CUMORA_DAEMON_ENV_FILE;缺省先取
                         规范位 ~/.config/cumora/daemon.env,回退 ~/.cumora/daemon.env)
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

// loadCfg —— stack.toml 层(#284):坏文件返回错误+缺省值,由调用方
// 决定呈现(doctor 判红 / status 警示后继续)。
func loadCfg() (stackconfig.Config, string, bool, error) {
	path := envOr("CUMORA_CONFIG_FILE", stackconfig.DefaultPath())
	c, found, err := stackconfig.LoadOrDefaults(path)
	return c, path, found, err
}

// defaultEnvFile —— 主 env 缺省:规范位(import-env 产物)在则用之,
// 否则存量布局(行为零变)。
func defaultEnvFile() string {
	if p := stackconfig.StackEnvPath(); fileExistsQuiet(p) {
		return p
	}
	return home("Code/cumora/.env")
}

// defaultDaemonEnvFile —— daemon.env 缺省与 stackd 同规则(评审 P2:
// 两面漂移会让 doctor 检查守护进程根本不读的文件)。
func defaultDaemonEnvFile() string {
	if p := stackconfig.DaemonEnvPath(); fileExistsQuiet(p) {
		return p
	}
	return stackconfig.LegacyDaemonEnvPath()
}

// serverHostPort —— doctor 端口检查的 Name 标签(展示 cfg 实际地址,
// 沙箱/改端口部署下标签不撒谎;Addr 侧必须是可拨号的裸地址)。
func serverHostPort(cfg stackconfig.Config) string {
	return fmt.Sprintf("server %s", cfg.Net.ServerAddr)
}

// buildDoctorAddrs —— cmdDoctor 的端口检查装配(评审 P0 的回归锁:
// Name 是标签、Addr 必须裸地址可拨号)。
func buildDoctorAddrs(cfg stackconfig.Config) []doctor.AddrExpect {
	addrs := []doctor.AddrExpect{
		{Name: serverHostPort(cfg), Addr: cfg.Net.ServerAddr, Kind: "must"},
		{Name: fmt.Sprintf("sidecar :%d", cfg.Net.SidecarPort), Addr: fmt.Sprintf("127.0.0.1:%d", cfg.Net.SidecarPort), Kind: "must"},
		{Name: "auth-loopback :47823", Addr: "127.0.0.1:47823", Kind: "desktop"},
	}
	if cfg.PG.Mode != stackconfig.ModeInternal {
		addrs = append(addrs, doctor.AddrExpect{Name: "postgres :5432", Addr: "127.0.0.1:5432", Kind: "info"})
	}
	if cfg.Redis.Mode != stackconfig.ModeInternal {
		addrs = append(addrs, doctor.AddrExpect{Name: "redis :6379", Addr: "127.0.0.1:6379", Kind: "info"})
	}
	return addrs
}

func fileExistsQuiet(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	fs.StringVar(&c.envFile, "env-file", envOr("CUMORA_ENV_FILE", defaultEnvFile()), "主 .env 路径")
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
		envOr("CUMORA_DAEMON_ENV_FILE", defaultDaemonEnvFile()), "daemon.env 路径")
	enginesCSV := fs.String("engines", "", "覆盖引擎集合(逗号分隔;默认 claude,codex,grok,cursor)")
	_ = fs.Parse(args)

	engines := defaultEngines
	if *enginesCSV != "" {
		engines = splitCSV(*enginesCSV)
	}
	// stack.toml(#284):坏文件 → [config] 红面;值面退内置缺省继续体检。
	cfg, cfgPath, cfgFound, cfgErr := loadCfg()
	cfgErrStr := ""
	if cfgErr != nil {
		cfgErrStr = cfgErr.Error()
		cfg = stackconfig.Defaults()
	}
	// 受管形态下 postgres/redis 不再探 TCP 5432/6379(socket 面已由
	// [postgres]/[redis] 组覆盖);external 形态照旧 info 记录。
	addrs := buildDoctorAddrs(cfg)
	rep := doctor.Run(probe.NewDeps(), doctor.Config{
		EnvFile:          c.envFile,
		DaemonEnvFile:    *daemonEnv,
		Units:            c.units(),
		StackdUnit:       envOr("CUMORA_STACKD_UNIT", "cumora.service"),
		StateFile:        envOr("CUMORA_STATE_FILE", cfg.StateFile()),
		StackAddrs:       addrs,
		ConfigFile:       cfgPath,
		ConfigFound:      cfgFound,
		ConfigErr:        cfgErrStr,
		PGMode:           cfg.PG.Mode,
		InternalDSN:      cfg.InternalDSN(),
		RedisMode:        cfg.Redis.Mode,
		InternalRedisURL: cfg.InternalRedisURL(),
		Engines:          engines,
		EngineExtraDir:   engineDirs,
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
	// stack.toml(#284):URL/路径缺省从 toml 派生;坏文件退内置缺省并警示
	//(status 只报告,不当退出码门)。
	cfg, _, _, cfgErr := loadCfg()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "status: stack.toml 非法,按内置缺省报告: %v\n", cfgErr)
		cfg = stackconfig.Defaults()
	}
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var c commonFlags
	parseCommon(fs, &c)
	livez := fs.String("livez-url", "http://"+cfg.Net.ServerAddr+"/api/livez", "livez URL")
	healthz := fs.String("healthz-url",
		fmt.Sprintf("http://127.0.0.1:%d/internal/healthz", cfg.Net.SidecarPort), "healthz URL")
	sid := fs.String("sid-token", os.Getenv("YJS_SIDECAR_TOKEN"), "sidecar Bearer token")
	current := fs.String("current-dir",
		envOr("CUMORA_CURRENT_DIR", cfg.CurrentDir()), "current symlink 目录")
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
