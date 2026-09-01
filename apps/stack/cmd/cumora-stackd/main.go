// cumora-stackd —— Stack 守护进程(#282 PR-B):单 systemd user unit
// (cumora.service)的 ExecStart,进程内管 pg→redis→sidecar→server→
// daemon 五面。pg/redis 双形态(#284):stack.toml mode=internal 时本进程
// 拉起受管实例(unix socket),否则 external 探测(存量部署)。装配与
// 语义见 internal/stackd 包文档;ADR 0005 阶段 1/2b。
//
// 配置优先级与全家族一致:flag > env > stack.toml > 内置缺省。
// stack.toml 位置:CUMORA_CONFIG_FILE 覆盖,缺省 XDG 配置目录。
package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackd"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// stack.toml 先于 flags 载入:非法即拒启(校验红不静默,journal 可见)。
	cfgPath := envOr("CUMORA_CONFIG_FILE", stackconfig.DefaultPath())
	cfg, _, err := stackconfig.LoadOrDefaults(cfgPath)
	if err != nil {
		log.Error("stack.toml 非法,拒绝启动", "path", cfgPath, "err", err)
		os.Exit(1)
	}

	// 子进程 cwd:存量布局优先(仓库根),净机退栈数据根(sidecar 的
	// dotenv .env 解析在存量机仍有意义;净机 env 全部来自父进程继承)。
	workDefault := home("Code/cumora")
	if _, err := os.Stat(workDefault); err != nil {
		workDefault = cfg.Data.Home
	}
	// daemon.env:规范位优先(import-env 产物),存量布局回退。
	daemonEnvDefault := stackconfig.DaemonEnvPath()
	if _, err := os.Stat(daemonEnvDefault); err != nil {
		daemonEnvDefault = stackconfig.LegacyDaemonEnvPath()
	}

	fs := flag.NewFlagSet("cumora-stackd", flag.ExitOnError)
	current := fs.String("current-dir",
		envOr("CUMORA_CURRENT_DIR", cfg.CurrentDir()),
		"release 制品目录(current symlink)")
	work := fs.String("work-dir", envOr("CUMORA_WORK_DIR", workDefault),
		"子进程工作目录")
	daemonEnv := fs.String("daemon-env-file",
		envOr("CUMORA_DAEMON_ENV_FILE", daemonEnvDefault), "daemon.env 路径")
	state := fs.String("state-file",
		envOr("CUMORA_STATE_FILE", cfg.StateFile()),
		"状态文件(原子写,status 消费)")
	serverAddr := fs.String("server-addr", envOr("CUMORA_GO_LISTEN", cfg.Net.ServerAddr),
		"server 监听地址(CUMORA_GO_LISTEN 注入值)")
	sidecarPort := fs.Int("sidecar-port",
		envOrInt("YJS_SIDECAR_PORT", cfg.Net.SidecarPort), "sidecar 端口(YJS_SIDECAR_PORT 注入值)")
	uploads := fs.String("uploads-dir",
		envOr("CUMORA_UPLOADS_DIR", cfg.UploadsDir()), "上传目录")
	instance := fs.String("instance-id", "",
		"实例标记覆盖(缺省 boot+uid 稳定派生;同机多栈沙箱必填,防孤儿认领互杀)")
	_ = fs.Parse(os.Args[1:])

	inst := stackd.StableInstanceID()
	if *instance != "" {
		inst = *instance
	}
	runCfg := stackd.Config{
		CurrentDir:    *current,
		WorkDir:       *work,
		DaemonEnvFile: *daemonEnv,
		StateFile:     *state,
		// 稳定实例 ID(boot+uid):崩溃重启后孤儿认领仍能找到上一世
		// 子进程(评审 P0-2;pid 派生 = 每世新 ID,认领恒空转)。
		InstanceID:   inst,
		ServerAddr:   *serverAddr,
		SidecarPort:  *sidecarPort,
		UploadsDir:   *uploads,
		DSN:          os.Getenv("DATABASE_URL"),
		RedisURL:     os.Getenv("REDIS_URL"),
		SidecarToken: os.Getenv("YJS_SIDECAR_TOKEN"),

		// 受管形态(#284):位置全部来自 toml 单源;external 语义零变。
		PGMode:      cfg.PG.Mode,
		RedisMode:   cfg.Redis.Mode,
		PGDataDir:   cfg.PGDataDir(),
		RunDir:      cfg.RunDir(),
		RedisSocket: cfg.RedisSocket(),
		PGDatabase:  "cumora",
		DataHome:    cfg.Data.Home,
	}
	if err := stackd.Run(runCfg, log); err != nil {
		log.Error("stackd 退出", "err", err)
		os.Exit(1)
	}
}

func envOr(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

func envOrInt(env string, fallback int) int {
	if v := os.Getenv(env); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
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
