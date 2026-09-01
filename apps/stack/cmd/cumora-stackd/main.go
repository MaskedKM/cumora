// cumora-stackd —— Stack 守护进程(#282 PR-B):单 systemd user unit
// (cumora.service)的 ExecStart,进程内管 pg→redis→sidecar→server→
// daemon 五面(pg/redis 阶段 1 为 external 探测)。装配与语义见
// internal/stackd 包文档;ADR 0005 阶段 1。
package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/MaskedKM/cumora/apps/stack/internal/stackd"
)

func main() {
	fs := flag.NewFlagSet("cumora-stackd", flag.ExitOnError)
	current := fs.String("current-dir",
		envOr("CUMORA_CURRENT_DIR", home(".local/share/cumora/current")),
		"release 制品目录(current symlink)")
	work := fs.String("work-dir", envOr("CUMORA_WORK_DIR", home("Code/cumora")),
		"子进程工作目录(.env 经 dotenv 从 cwd 解析)")
	daemonEnv := fs.String("daemon-env-file",
		envOr("CUMORA_DAEMON_ENV_FILE", home(".cumora/daemon.env")), "daemon.env 路径")
	state := fs.String("state-file",
		envOr("CUMORA_STATE_FILE", home(".local/share/cumora/stackd-state.json")),
		"状态文件(原子写,status 消费)")
	serverAddr := fs.String("server-addr", envOr("CUMORA_GO_LISTEN", "127.0.0.1:5181"),
		"server 监听地址(CUMORA_GO_LISTEN 注入值)")
	sidecarPort := fs.Int("sidecar-port",
		envOrInt("YJS_SIDECAR_PORT", 5182), "sidecar 端口(YJS_SIDECAR_PORT 注入值)")
	uploads := fs.String("uploads-dir",
		envOr("CUMORA_UPLOADS_DIR", home(".local/share/cumora/uploads")), "上传目录")
	_ = fs.Parse(os.Args[1:])

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg := stackd.Config{
		CurrentDir:    *current,
		WorkDir:       *work,
		DaemonEnvFile: *daemonEnv,
		StateFile:     *state,
		// 稳定实例 ID(boot+uid):崩溃重启后孤儿认领仍能找到上一世
		// 子进程(评审 P0-2;pid 派生 = 每世新 ID,认领恒空转)。
		InstanceID:   stackd.StableInstanceID(),
		ServerAddr:   *serverAddr,
		SidecarPort:  *sidecarPort,
		UploadsDir:   *uploads,
		DSN:          os.Getenv("DATABASE_URL"),
		RedisURL:     os.Getenv("REDIS_URL"),
		SidecarToken: os.Getenv("YJS_SIDECAR_TOKEN"),
	}
	if err := stackd.Run(cfg, log); err != nil {
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
