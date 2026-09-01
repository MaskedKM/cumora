// import-env —— 首启向导的命令面(#284,ADR 0005 §4):一次性导入
// .env/daemon.env。机器事实转 stack.toml,凭据原样搬同目录 stack.env /
// daemon.env(0600);GITHUB_CLIENT_ID/SECRET 缺失 = 红线退出 1;输出只报
// 键名,永不回显值。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
)

// machineFactKeys —— 转 toml 的机器事实键(其余键一概 stack.env 原样:
// 等价优先,宁可多搬也不静默丢键)。
var machineFactKeys = map[string]bool{
	"CUMORA_GO_LISTEN":   true, // → net.server_addr
	"YJS_SIDECAR_PORT":   true, // → net.sidecar_port
	"CUMORA_UPLOADS_DIR": true, // → data.uploads_dir
}

// requiredKeys —— 红线:登录链的硬依赖(缺失 = 向导阻断,doctor 同名单)。
var requiredKeys = []string{"GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET"}

// legacyNoReaderKeys —— 全仓无读取点的历史键(只提示,不丢:等价优先)。
var legacyNoReaderKeys = []string{"PORT", "FILE_BINDING_ALLOWLIST_ROOTS", "R2_URL_SIGNING_SECRET"}

// optionalPrefixes —— 向导标可选(本部署未启用)。
var optionalPrefixes = []string{"R2_"}

// importDeps —— 形态判定探针;包级变量供测试注入(否则单测结果随宿主
// 是否恰有 redis 监听 6379 而抖)。
var importDeps = probe.NewDeps()

// systemRedisAlive —— server 缺省 redis 位有无应答。
func systemRedisAlive() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return importDeps.Redis(ctx, "") == nil
}

// FileAction —— 报告里的一个目标文件动作。
type FileAction struct {
	Path   string `json:"path"`
	Action string `json:"action"` // created | updated | kept-existing | skipped | planned
}

// ImportReport —— --json 面;全部键名,无值。
type ImportReport struct {
	ConfigDir       string       `json:"configDir"`
	PGMode          string       `json:"pgMode"`
	RedisMode       string       `json:"redisMode"`
	MovedToToml     []string     `json:"movedToToml"`
	KeptInEnv       []string     `json:"keptInEnv"`
	SourceKeys      []string     `json:"sourceKeys"`
	MissingRequired []string     `json:"missingRequired"`
	OptionalPresent []string     `json:"optionalPresent"`
	LegacyNoReader  []string     `json:"legacyNoReader"`
	Files           []FileAction `json:"files"`
}

func cmdImportEnv(args []string) int {
	fs := flag.NewFlagSet("import-env", flag.ExitOnError)
	envFile := fs.String("env-file", envOr("CUMORA_ENV_FILE", home("Code/cumora/.env")), "源 .env 路径")
	daemonEnvFile := fs.String("daemon-env-file",
		envOr("CUMORA_DAEMON_ENV_FILE", stackconfig.LegacyDaemonEnvPath()), "源 daemon.env 路径")
	configDir := fs.String("config-dir", filepath.Dir(stackconfig.DefaultPath()),
		"目标配置目录(stack.toml/stack.env/daemon.env 落点)")
	dataHome := fs.String("data-home", "", "数据根覆盖(缺省内置布局;净机向导默认)")
	dryRun := fs.Bool("dry-run", false, "只报告不落盘")
	force := fs.Bool("force", false, "覆盖既有目标文件(缺省拒绝,幂等重跑需显式)")
	jsonOut := fs.Bool("json", false, "JSON 输出")
	_ = fs.Parse(args)

	rep := ImportReport{
		ConfigDir:       *configDir,
		MovedToToml:     []string{},
		KeptInEnv:       []string{},
		SourceKeys:      []string{},
		MissingRequired: []string{},
		OptionalPresent: []string{},
		LegacyNoReader:  []string{},
		Files:           []FileAction{},
	}

	// 1) 读源(缺文件 = 空集:净机路径凭据由向导表单提供成临时 env 文件)。
	envMap := map[string]string{}
	if data, err := os.ReadFile(*envFile); err == nil {
		envMap = probe.ParseEnvFile(data)
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "import-env: 读 %s: %v\n", *envFile, err)
		return 2
	}
	for k := range envMap {
		rep.SourceKeys = append(rep.SourceKeys, k)
	}
	sort.Strings(rep.SourceKeys)

	// 2) 分类:机器事实 → toml;其余 → stack.env 原样。
	cfg := stackconfig.Defaults()
	if *dataHome != "" {
		cfg.Data.Home = *dataHome
	}
	for k, v := range envMap {
		switch k {
		case "CUMORA_GO_LISTEN":
			cfg.Net.ServerAddr = v
			rep.MovedToToml = append(rep.MovedToToml, k)
		case "YJS_SIDECAR_PORT":
			if port, err := strconv.Atoi(v); err == nil {
				cfg.Net.SidecarPort = port
				rep.MovedToToml = append(rep.MovedToToml, k)
			} else {
				rep.KeptInEnv = append(rep.KeptInEnv, k) // 非整数:原样搬,不猜
			}
		case "CUMORA_UPLOADS_DIR":
			cfg.Data.UploadsDir = v
			rep.MovedToToml = append(rep.MovedToToml, k)
		default:
			rep.KeptInEnv = append(rep.KeptInEnv, k)
		}
	}
	sort.Strings(rep.MovedToToml)
	sort.Strings(rep.KeptInEnv)

	// 3) pg/redis 形态。pg:DATABASE_URL 在 = external(存量部署指路),
	// 不在 = internal(净机)。redis:REDIS_URL 在 = external;不在时探
	// server 缺省位 redis://localhost:6379 —— 有应答 = 存量部署用系统
	// redis(server 的缺省值即此,不写键行为等价),无应答 = 净机 internal。
	if envMap["DATABASE_URL"] != "" {
		cfg.PG.Mode = stackconfig.ModeExternal
	} else {
		cfg.PG.Mode = stackconfig.ModeInternal
	}
	switch {
	case envMap["REDIS_URL"] != "":
		cfg.Redis.Mode = stackconfig.ModeExternal
	default:
		if systemRedisAlive() {
			cfg.Redis.Mode = stackconfig.ModeExternal
		} else {
			cfg.Redis.Mode = stackconfig.ModeInternal
		}
	}
	rep.PGMode = cfg.PG.Mode
	rep.RedisMode = cfg.Redis.Mode

	// 4) 红线与可选面(键名 only)。
	for _, k := range requiredKeys {
		if envMap[k] == "" {
			rep.MissingRequired = append(rep.MissingRequired, k)
		}
	}
	for _, k := range rep.SourceKeys {
		for _, p := range optionalPrefixes {
			if strings.HasPrefix(k, p) {
				rep.OptionalPresent = append(rep.OptionalPresent, k)
			}
		}
	}
	for _, k := range legacyNoReaderKeys {
		if envMap[k] != "" {
			rep.LegacyNoReader = append(rep.LegacyNoReader, k)
		}
	}

	// 5) 落盘(toml + stack.env + daemon.env 原样拷)。已存在目标无
	//    --force 拒覆盖(防向导重跑抹掉手工修订)。
	tomlPath := filepath.Join(*configDir, "stack.toml")
	stackEnvPath := filepath.Join(*configDir, "stack.env")
	daemonEnvPath := filepath.Join(*configDir, "daemon.env")
	if !*dryRun {
		if err := os.MkdirAll(*configDir, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "import-env: 建配置目录 %s: %v\n", *configDir, err)
			return 2
		}
	}

	plan := []struct {
		path     string
		write    func() error
		skipWhen string // 非空 = 源缺失时跳过(daemon.env)
	}{
		{tomlPath, func() error { return stackconfig.Save(tomlPath, cfg) }, ""},
		{stackEnvPath, func() error { return writeEnvFile(stackEnvPath, envMap, machineFactKeys) }, ""},
		{daemonEnvPath, func() error {
			data, err := os.ReadFile(*daemonEnvFile)
			if err != nil {
				return err
			}
			// tmp+rename:中断不留半截凭据文件(与其余两件同纪律)。
			tmp := daemonEnvPath + ".tmp"
			if err := os.WriteFile(tmp, data, 0o600); err != nil {
				return err
			}
			return os.Rename(tmp, daemonEnvPath)
		}, "no-source"},
	}
	for _, p := range plan {
		if p.skipWhen != "" {
			if _, err := os.Stat(*daemonEnvFile); os.IsNotExist(err) {
				rep.Files = append(rep.Files, FileAction{Path: p.path, Action: "skipped"})
				continue
			}
		}
		exists, _ := fileExists(p.path)
		action := "created"
		if exists {
			if !*force {
				rep.Files = append(rep.Files, FileAction{Path: p.path, Action: "kept-existing"})
				continue
			}
			action = "updated"
		}
		if *dryRun {
			action = "planned-" + action
		} else {
			if err := p.write(); err != nil {
				fmt.Fprintf(os.Stderr, "import-env: 写 %s: %v\n", p.path, err)
				return 2
			}
		}
		rep.Files = append(rep.Files, FileAction{Path: p.path, Action: action})
	}

	if *jsonOut {
		printJSON(rep)
	} else {
		printImportReport(rep)
	}

	// 红线最后判:报告完整呈现后再退 1(向导要能看到全部缺什么)。
	if len(rep.MissingRequired) > 0 {
		fmt.Fprintf(os.Stderr, "import-env: 红线键缺失 %v(GitHub OAuth 登录链硬依赖)\n", rep.MissingRequired)
		return 1
	}
	return 0
}

func printImportReport(rep ImportReport) {
	fmt.Println("[import-env]")
	fmt.Printf("  配置目录   %s\n", rep.ConfigDir)
	fmt.Printf("  pg 形态    %s\n", rep.PGMode)
	fmt.Printf("  redis 形态 %s(redis 由 6379 探活推断,可手改 toml)\n", rep.RedisMode)
	fmt.Printf("  机器事实转 toml: %s\n", keysOrNone(rep.MovedToToml))
	fmt.Printf("  原样搬 stack.env: %d 键\n", len(rep.KeptInEnv))
	if len(rep.LegacyNoReader) > 0 {
		fmt.Printf("  遗留键(无读取点,仅随迁): %s\n", keysOrNone(rep.LegacyNoReader))
	}
	if len(rep.OptionalPresent) > 0 {
		fmt.Printf("  可选键(R2_*): %s\n", keysOrNone(rep.OptionalPresent))
	}
	fmt.Println("[files]")
	for _, f := range rep.Files {
		fmt.Printf("  %-12s %s\n", f.Action, f.Path)
	}
	if len(rep.MissingRequired) > 0 {
		fmt.Printf("结果: 红线缺失 %v —— 凭据补齐后重跑(退出码 1)\n", rep.MissingRequired)
	} else {
		fmt.Println("结果: 就绪")
	}
}

// writeEnvFile —— stack.env 生成:KEY=VALUE 排序落盘(0600:凭据房)。
// 值含空白/#/引号时加双引号转义(systemd EnvironmentFile 语义往返)。
func writeEnvFile(path string, envMap map[string]string, skip map[string]bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	keys := make([]string, 0, len(envMap))
	for k := range envMap {
		if skip[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# stack.env —— 凭据与应用配置(cumora-stack import-env 生成)\n")
	b.WriteString("# 机器事实在 stack.toml;本文件进 unit EnvironmentFile,权限 0600。\n")
	for _, k := range keys {
		v := envMap[k]
		// 只加引号不转义:probe.ParseEnvFile 只剥外层引号,转义会破坏
		// 往返等价(值含内嵌双引号属荒诞凭据,不为它引入转义协议)。
		if strings.ContainsAny(v, " \t#'") {
			v = `"` + v + `"`
		}
		b.WriteString(k + "=" + v + "\n")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func keysOrNone(keys []string) string {
	if len(keys) == 0 {
		return "-"
	}
	return strings.Join(keys, ", ")
}
