// stackd —— Stack 守护进程的装配与运行(#282 PR-B,ADR 0005 阶段 1)。
//
// 拓扑(单 systemd user unit 只保证本进程存在;五个子面全在本进程内管):
//
//	pg(external 探测)→ redis(external 探测)→ sidecar → server → daemon
//
// 阶段 1 pg/redis 是系统级服务(external);#283 打包链落地后切 managed。
// 环境传递:unit 的 EnvironmentFile(.env)→ stackd 继承 → 子进程;
// daemon 子进程额外合并 daemon.env 并钉扎引擎 PATH(nvm/npx 发现)。
package stackd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/chain"
	"github.com/MaskedKM/cumora/apps/stack/internal/engdirs"
	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/supervise"
)

// Config —— stackd 装配输入。路径/地址全部可注入(默认值属本部署
// 布局,main 从 flag/env 填;测试直填)。
type Config struct {
	CurrentDir    string // releases/<ver> 经 current symlink(四件二进制)
	WorkDir       string // 子进程 cwd(仓库根:sidecar dotenv/.env 解析)
	DaemonEnvFile string // daemon.env(引擎凭据,合并进 daemon 子进程)
	StateFile     string // stackd-state.json(原子写)
	InstanceID    string // 孤儿认领标记(空 = supervise 默认)

	ServerAddr  string // CUMORA_GO_LISTEN 注入值(127.0.0.1:5181)
	SidecarPort int    // YJS_SIDECAR_PORT 注入值(5182)
	// 门预算覆盖(0 = 缺省 sidecar 60s / server 120s;测试压短)。
	SidecarGateTimeout time.Duration
	ServerGateTimeout  time.Duration
	UploadsDir         string // CUMORA_UPLOADS_DIR 注入值
	DSN                string // external pg 探测用(空 = probe 缺省)
	RedisURL           string // external redis 探测用
	SidecarToken       string // sidecar 健康门 Bearer

	// Probes —— 探针注入;零值 = 生产探针(probe.NewDeps)。
	Probes probe.Deps
}

func (c Config) probes() probe.Deps {
	if c.Probes.PG != nil || c.Probes.HTTP != nil {
		return c.Probes
	}
	return probe.NewDeps()
}

// NodeNames —— 链序(装配结果的对照面,测试与文档共用)。
var NodeNames = []string{"postgres", "redis", "sidecar", "server", "daemon"}

// BuildNodes —— 按链序装配节点。健康门语义与三 unit 时代一致:
// sidecar healthz 200|401 都算活(Bearer 面);server livez 200|503 都算
// 就绪(503 = Redis 红的诚实信号,livez 本身活着 —— cumora-go.service
// 探针注释的语义原样继承)。
func BuildNodes(cfg Config) ([]chain.Node, error) {
	d := cfg.probes()
	bin := func(name string) string { return filepath.Join(cfg.CurrentDir, name) }
	for _, name := range []string{"cumora-sidecar", "cumora-server", "cumora-daemon", "cumora-stack", "cumora-stackd"} {
		if _, err := os.Stat(bin(name)); err != nil {
			return nil, fmt.Errorf("stackd: %s 缺失于 %s(先 deploy-release 新制品): %w", name, cfg.CurrentDir, err)
		}
	}

	daemonEnv, err := loadDaemonEnv(cfg.DaemonEnvFile)
	if err != nil {
		return nil, err
	}

	uploads := cfg.UploadsDir
	if uploads == "" {
		uploads = filepath.Join(homeDir(), ".local/share/cumora/uploads")
	}

	gateSidecar := func(ctx context.Context) error {
		code, err := d.HTTP(fmt.Sprintf("http://127.0.0.1:%d/internal/healthz", cfg.SidecarPort), cfg.SidecarToken)
		if err != nil {
			return err
		}
		if code == 200 || code == 401 {
			return nil
		}
		return fmt.Errorf("healthz HTTP %d", code)
	}
	gateServer := func(ctx context.Context) error {
		code, err := d.HTTP("http://"+cfg.ServerAddr+"/api/livez", "")
		if err != nil {
			return err
		}
		if code == 200 || code == 503 {
			return nil
		}
		return fmt.Errorf("livez HTTP %d", code)
	}

	return []chain.Node{
		{Name: "postgres", Mode: chain.External, Probe: func(ctx context.Context) error {
			_, err := d.PG(ctx, cfg.DSN)
			return err
		}},
		{Name: "redis", Mode: chain.External, Probe: func(ctx context.Context) error {
			return d.Redis(ctx, cfg.RedisURL)
		}},
		{Name: "sidecar", Mode: chain.Managed, Child: &supervise.Child{
			Name: "sidecar", Path: bin("cumora-sidecar"),
			Dir:  cfg.WorkDir,
			Env:  []string{"YJS_SIDECAR_PORT=" + strconv.Itoa(cfg.SidecarPort), "CUMORA_UPLOADS_DIR=" + uploads},
			Gate: gateSidecar, GateEvery: 500 * time.Millisecond,
			GateTimeout: orDefault(cfg.SidecarGateTimeout, 60*time.Second),
		}},
		{Name: "server", Mode: chain.Managed, Child: &supervise.Child{
			Name: "server", Path: bin("cumora-server"),
			Dir: cfg.WorkDir,
			Env: []string{
				"CUMORA_GO_LISTEN=" + cfg.ServerAddr,
				"CUMORA_UPLOADS_DIR=" + uploads,
				"CUMORA_GO_MIGRATIONS=" + filepath.Join(cfg.CurrentDir, "migrations"),
			},
			Gate: gateServer, GateEvery: time.Second,
			GateTimeout: orDefault(cfg.ServerGateTimeout, 120*time.Second),
		}},
		{Name: "daemon", Mode: chain.Managed, Child: &supervise.Child{
			// daemon 是 server 的轮询客户端(SSE wake+心跳),无 HTTP 面
			// —— 不设门,进程存活即就位(cumora-daemon.service 同语义)。
			Name: "daemon", Path: bin("cumora-daemon"),
			Args: []string{"agent", "computer", "--server", "http://" + cfg.ServerAddr},
			Dir:  cfg.WorkDir,
			Env:  append(daemonEnv, "PATH="+pinnedPATH()),
		}},
	}, nil
}

// pinnedPATH —— daemon 子进程的 PATH:当前 PATH + 引擎发现目录(nvm/
// npx glob)。PATH 钉扎坑(fresh-boot 用户管理器不带 nvm)的机制化
// 收口,替代旧 unit 的手钉行。
func pinnedPATH() string {
	return os.Getenv("PATH") + ":" + joinDirs(engdirs.Dirs(""))
}

func joinDirs(dirs []string) string {
	out := ""
	for i, d := range dirs {
		if i > 0 {
			out += ":"
		}
		out += d
	}
	return out
}

// loadDaemonEnv —— daemon.env 的键值对(空文件/文件缺失 = 空集,仅
// 引擎凭据缺失属降级不阻断;与 doctor 的黄灯语义一致)。
func loadDaemonEnv(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stackd: 读 %s: %w", path, err)
	}
	m := probe.ParseEnvFile(data)
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Snapshot —— 状态文件形态(`cumora-stack status` 的 stackd 段契约)。
type Snapshot struct {
	InstanceID string            `json:"instanceId"`
	UpdatedAt  time.Time         `json:"updatedAt"`
	Children   []supervise.State `json:"children"`
}

// Run —— stackd 主循环:孤儿认领 → 链式拉起 → 周期落状态文件 →
// SIGTERM/SIGINT 逆序优雅停链。BringUp 失败 = 启动失败,交 systemd
// Restart=always(与旧三 unit 的 ExecStartPost 门同语义)。
func Run(cfg Config, log *slog.Logger) error {
	if killed := supervise.KillInstanceOrphans(cfg.InstanceID); killed > 0 {
		log.Warn("上一世残留子进程已清杀", "count", killed)
	}

	m := supervise.New(supervise.Options{
		InstanceID: cfg.InstanceID,
		Log: func(msg string, kv ...any) {
			log.Info(msg, kv...)
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	nodes, err := BuildNodes(cfg)
	if err != nil {
		return err
	}
	if err := chain.BringUp(ctx, nodes, m); err != nil {
		m.Shutdown()
		return fmt.Errorf("stackd: 链式拉起失败: %w", err)
	}
	log.Info("stack up", "nodes", NodeNames)
	writeState(cfg, m, log)

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("停机信号,逆序停链")
			m.Shutdown()
			writeState(cfg, m, log)
			return nil
		case <-tick.C:
			writeState(cfg, m, log)
		}
	}
}

// writeState —— 原子落状态文件(tmp+rename;失败仅告警不崩 —— 状态面
// 可观测性不该有能力杀死栈)。
func writeState(cfg Config, m *supervise.Manager, log *slog.Logger) {
	if cfg.StateFile == "" {
		return
	}
	snap := Snapshot{InstanceID: cfg.InstanceID, UpdatedAt: time.Now(), Children: m.States()}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		log.Warn("状态序列化失败", "err", err)
		return
	}
	tmp := cfg.StateFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(cfg.StateFile), 0o755); err != nil {
		log.Warn("状态目录创建失败", "err", err)
		return
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Warn("状态写入失败", "err", err)
		return
	}
	if err := os.Rename(tmp, cfg.StateFile); err != nil {
		log.Warn("状态原子替换失败", "err", err)
	}
}

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
