// stackd —— Stack 守护进程的装配与运行(#282 PR-B,ADR 0005 阶段 1)。
//
// 拓扑(单 systemd user unit 只保证本进程存在;五个子面全在本进程内管):
//
//	pg → redis → sidecar → server → daemon
//
// pg/redis 双形态(#284 受管化):external = 系统级服务,探测等就绪
// (存量部署零变);internal = 本进程拉起受管实例 —— pg 走 unix socket
// (trust-local + reject-host,凭据零落盘),redis 走 unix socket +
// 端口 0(本机 6379 已被占)。受管形态下 server 的 DATABASE_URL/
// REDIS_URL 由 stackd 派生注入(后写覆盖,见 supervise.overrideEnv)。
// 环境传递:unit 的 EnvironmentFile(.env)→ stackd 继承 → 子进程;
// daemon 子进程额外合并 daemon.env 并钉扎引擎 PATH(nvm/npx 发现)。
package stackd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/chain"
	"github.com/MaskedKM/cumora/apps/stack/internal/engdirs"
	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
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
	SidecarGateTimeout  time.Duration
	ServerGateTimeout   time.Duration
	InternalGateTimeout time.Duration // 受管 pg/redis 门预算(0 = 120s;首启含 initdb 后冷启)
	UploadsDir          string        // CUMORA_UPLOADS_DIR 注入值
	DSN                 string        // external pg 探测用(空 = probe 缺省)
	RedisURL            string        // external redis 探测用
	SidecarToken        string        // sidecar 健康门 Bearer

	// 受管 pg/redis(#284):Mode = stackconfig.ModeInternal 时本进程拉起,
	// DSN/RedisURL 忽略、位置由下列字段定;其余值 = external 探测(缺省)。
	PGMode      string
	RedisMode   string
	PGDataDir   string // 缺省 <dataHome>/pgdata
	RunDir      string // 缺省 <dataHome>/run(socket 目录,0700)
	RedisSocket string // 缺省 <runDir>/redis.sock
	PGDatabase  string // 缺省 cumora
	DataHome    string // 三项位置缺省的锚点(缺省 ~/.local/share/cumora)

	// Probes —— 探针注入;零值 = 生产探针(probe.NewDeps)。
	Probes probe.Deps
}

func (c Config) probes() probe.Deps {
	if c.Probes.PG != nil || c.Probes.HTTP != nil {
		return c.Probes
	}
	return probe.NewDeps()
}

func (c Config) pgInternal() bool { return c.PGMode == stackconfig.ModeInternal }

func (c Config) redisInternal() bool { return c.RedisMode == stackconfig.ModeInternal }

func (c Config) dataHome() string {
	if c.DataHome != "" {
		return c.DataHome
	}
	return filepath.Join(homeDir(), ".local/share/cumora")
}

func (c Config) pgDataDir() string {
	return orString(c.PGDataDir, filepath.Join(c.dataHome(), "pgdata"))
}

func (c Config) runDir() string {
	return orString(c.RunDir, filepath.Join(c.dataHome(), "run"))
}

func (c Config) redisSocket() string {
	return orString(c.RedisSocket, filepath.Join(c.runDir(), "redis.sock"))
}

func (c Config) pgDatabase() string { return orString(c.PGDatabase, "cumora") }

// internalDSN —— 受管 pg 的应用连接串(socket-only;probe.withSSLModeDisabled
// 的 url 分支管不到 keyword 形态,这里直接带 sslmode=disable)。
func (c Config) internalDSN() string {
	return fmt.Sprintf("host=%s user=cumora dbname=%s sslmode=disable", c.runDir(), c.pgDatabase())
}

// adminDSN —— 维护连接(postgres 库:CREATE DATABASE / 探活门)。
func (c Config) adminDSN() string {
	return fmt.Sprintf("host=%s user=cumora dbname=postgres sslmode=disable", c.runDir())
}

func orString(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// NodeNames —— 链序(装配结果的对照面,测试与文档共用)。
var NodeNames = []string{"postgres", "redis", "sidecar", "server", "daemon"}

// BuildNodes —— 按链序装配节点。健康门语义与三 unit 时代一致:
// sidecar healthz 200|401 都算活(Bearer 面);server livez 200|503 都算
// 就绪(503 = Redis 红的诚实信号,livez 本身活着 —— cumora-go.service
// 探针注释的语义原样继承)。受管 pg 门额外要求 pgvector 可用(migrations
// 的 vector 列是硬依赖,缺扩展的"活 pg"过不了门,不在 server 侧扑空)。
func BuildNodes(cfg Config) ([]chain.Node, error) {
	d := cfg.probes()
	bin := func(name string) string { return filepath.Join(cfg.CurrentDir, name) }
	for _, name := range []string{"cumora-sidecar", "cumora-server", "cumora-daemon", "cumora-stack", "cumora-stackd"} {
		if _, err := os.Stat(bin(name)); err != nil {
			return nil, fmt.Errorf("stackd: %s 缺失于 %s(先 deploy-release 新制品): %w", name, cfg.CurrentDir, err)
		}
	}
	// 前置检查按形态各自独立(评审 P1:pg=internal+redis=external 的合法
	// 混合形态不得因缺 redis-server 拒装配 —— 那形态根本不用它)。
	if cfg.redisInternal() {
		if _, err := os.Stat(bin("redis-server")); err != nil {
			return nil, fmt.Errorf("stackd: 受管 redis 缺 redis-server 于 %s: %w", cfg.CurrentDir, err)
		}
	}
	if cfg.pgInternal() {
		for _, name := range []string{"pg/bin/postgres", "pg/bin/initdb"} {
			if _, err := os.Stat(bin(name)); err != nil {
				return nil, fmt.Errorf("stackd: 受管 pg 缺 %s 于 %s(制品载荷不完整?): %w", name, cfg.CurrentDir, err)
			}
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

	// pg 节点:external = 探测系统实例;internal = 受管 postgres 子进程
	// (socket-only),门 = admin 库连通 + pgvector 可用 + 目标库就位
	// (首过门时 CREATE DATABASE,幂等)。
	var pgNode chain.Node
	if cfg.pgInternal() {
		dbEnsured := false
		pgNode = chain.Node{Name: "postgres", Mode: chain.Managed, Child: &supervise.Child{
			Name: "postgres", Path: bin("pg/bin/postgres"),
			Args: []string{"-D", cfg.pgDataDir(), "-k", cfg.runDir(), "-h", ""},
			Dir:  cfg.CurrentDir,
			Gate: func(ctx context.Context) error {
				info, err := d.PG(ctx, cfg.adminDSN())
				if err != nil {
					return err
				}
				if !info.PgvectorAvailable {
					return fmt.Errorf("受管 pg 缺 pgvector 扩展(载荷 pg/ 不完整?)")
				}
				if !dbEnsured {
					if err := d.EnsureDatabase(ctx, cfg.adminDSN(), cfg.pgDatabase()); err != nil {
						return err
					}
					dbEnsured = true
				}
				return nil
			},
			GateEvery:   500 * time.Millisecond,
			GateTimeout: orDefault(cfg.InternalGateTimeout, 120*time.Second),
		}}
	} else {
		pgNode = chain.Node{Name: "postgres", Mode: chain.External, Probe: func(ctx context.Context) error {
			_, err := d.PG(ctx, cfg.DSN)
			return err
		}}
	}

	// redis 节点:external = 探测系统实例;internal = 受管 redis-server
	// 子进程(unix socket + 端口 0,持久化关 —— pub/sub 总线非硬状态)。
	var redisNode chain.Node
	if cfg.redisInternal() {
		redisNode = chain.Node{Name: "redis", Mode: chain.Managed, Child: &supervise.Child{
			Name: "redis", Path: bin("redis-server"),
			Args: []string{
				"--unixsocket", cfg.redisSocket(), "--unixsocketperm", "700",
				"--port", "0", "--save", "", "--appendonly", "no",
				"--dir", cfg.runDir(),
			},
			Dir:         cfg.CurrentDir,
			Gate:        func(ctx context.Context) error { return d.Redis(ctx, "unix://"+cfg.redisSocket()) },
			GateEvery:   250 * time.Millisecond,
			GateTimeout: orDefault(cfg.InternalGateTimeout, 120*time.Second),
		}}
	} else {
		redisNode = chain.Node{Name: "redis", Mode: chain.External, Probe: func(ctx context.Context) error {
			return d.Redis(ctx, cfg.RedisURL)
		}}
	}

	// server 注入:受管形态下 DATABASE_URL/REDIS_URL 由 stackd 派生
	// (overrideEnv 后写覆盖 —— stack.env 里的存量外部 DSN 会被有意压掉,
	// 这是迁移窗口内"切内置库"的机制)。
	serverEnv := []string{
		"CUMORA_GO_LISTEN=" + cfg.ServerAddr,
		"CUMORA_UPLOADS_DIR=" + uploads,
		"CUMORA_GO_MIGRATIONS=" + filepath.Join(cfg.CurrentDir, "migrations"),
	}
	if cfg.pgInternal() {
		serverEnv = append(serverEnv, "DATABASE_URL="+cfg.internalDSN())
	}
	if cfg.redisInternal() {
		serverEnv = append(serverEnv, "REDIS_URL="+cfg.internalRedisURLFace())
	}

	return []chain.Node{
		pgNode,
		redisNode,
		{Name: "sidecar", Mode: chain.Managed, Child: &supervise.Child{
			Name: "sidecar", Path: bin("cumora-sidecar"),
			Dir:  cfg.WorkDir,
			Env:  []string{"YJS_SIDECAR_PORT=" + strconv.Itoa(cfg.SidecarPort), "CUMORA_UPLOADS_DIR=" + uploads},
			Gate: gateSidecar, GateEvery: 500 * time.Millisecond,
			GateTimeout: orDefault(cfg.SidecarGateTimeout, 60*time.Second),
		}},
		{Name: "server", Mode: chain.Managed, Child: &supervise.Child{
			Name: "server", Path: bin("cumora-server"),
			Dir:  cfg.WorkDir,
			Env:  serverEnv,
			Gate: gateServer, GateEvery: time.Second,
			GateTimeout: orDefault(cfg.ServerGateTimeout, 120*time.Second),
		}},
		{Name: "daemon", Mode: chain.Managed, Child: &supervise.Child{
			// daemon 是 server 的轮询客户端(SSE wake+心跳),无 HTTP 面
			// —— 不设门,进程存活即就位(cumora-daemon.service 同语义)。
			// CUMORA_SUPERVISED=1:自更新语义依赖(selfupdate.go supervised
			// 探测;心跳/pair 报文也上报该位)——旧 unit 的 Environment 行
			// 必须原样继承,丢了 = 自更新静默失效(评审 P0-1)。
			Name: "daemon", Path: bin("cumora-daemon"),
			Args: []string{"agent", "computer", "--server", "http://" + cfg.ServerAddr},
			Dir:  cfg.WorkDir,
			Env:  append(daemonEnv, "CUMORA_SUPERVISED=1", "PATH="+pinnedPATH()),
		}},
	}, nil
}

// internalRedisURLFace —— 受管 redis 的 URL 面(与 stackconfig.InternalRedisURL
// 同形态;独立于 toml 层以便 flag 注入路径复用)。
func (c Config) internalRedisURLFace() string { return "unix://" + c.redisSocket() }

// EnsureInternalPG —— 受管 pg 首启 bootstrap:pgdata 无 PG_VERSION 即
// initdb(trust-local + reject-host:隔离靠 socket 目录 0700,凭据零落盘,
// TCP 面关死)。幂等:既有集群只补建 socket 目录(并收紧其权限 ——
// MkdirAll 不回收已存在目录的 0700 不变量,评审 P3)。
//
// 原子性(评审 P2):initdb 落在同级 staging 目录,成功才 rename 进
// pgdata —— 中途被杀/失败不残缺集群,残缺目录会把幂等门(PG_VERSION
// 在=跳过 / 目录非空=initdb 拒跑)永久卡死,净机向导无自愈路径。
//
// ctx(评审 P3,#313):caller 的取消/SIGTERM 即刻打断 initdb —— 净机
// 冷启最坏 5 分钟预算内,栈停机不应被 initdb 拖住(systemd
// TimeoutStopSec 会先 SIGKILL 留 staging 残骸)。
func EnsureInternalPG(ctx context.Context, binDir, dataDir, runDir string, log *slog.Logger) error {
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("stackd: 建 socket 目录 %s: %w", runDir, err)
	}
	if err := os.Chmod(runDir, 0o700); err != nil {
		return fmt.Errorf("stackd: 收紧 socket 目录权限 %s: %w", runDir, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stackd: stat %s: %w", dataDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(dataDir), 0o755); err != nil {
		return fmt.Errorf("stackd: 建 pgdata 父目录: %w", err)
	}
	if log != nil {
		log.Info("initdb: 初建受管 pg 集群", "dir", dataDir)
	}
	staging := fmt.Sprintf("%s.staging-initdb.%d", dataDir, os.Getpid())
	defer os.RemoveAll(staging)
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ictx, filepath.Join(binDir, "initdb"),
		"-D", staging, "-U", "cumora",
		"--auth-local=trust", "--auth-host=reject",
		"--encoding=UTF8", "--no-locale")
	// WaitDelay 保险带(实测踩实):ctx 取消杀的是 initdb 本体,若它留有
	// 抱着 stdout/stderr 管道的孙进程,CombinedOutput 会等 EOF 挂死 ——
	// 取消后至多再等 5s 强制收口(SIGTERM 语义优先于日志收全)。
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ictx.Err() != nil {
			return fmt.Errorf("stackd: initdb 被取消/超时(%v;staging 已清理,可重试): %w", ictx.Err(), err)
		}
		return fmt.Errorf("stackd: initdb 失败(staging 已清理,可重试): %w\n%s", err, tailLines(out, 25))
	}
	if err := os.Rename(staging, dataDir); err != nil {
		return fmt.Errorf("stackd: pgdata 落位 %s: %w", dataDir, err)
	}
	return nil
}

func tailLines(data []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// pinnedPATH —— daemon 子进程的 PATH:当前 PATH + 引擎发现目录(nvm/
// npx glob)。PATH 钉扎坑(fresh-boot 用户管理器不带 nvm)的机制化
// 收口,替代旧 unit 的手钉行。已知取舍(评审 P2-6):比旧 unit 的手钉
// 面窄(不含仓库各级 node_modules/.bin、node-gyp-bin、/snap/bin 等)
// —— 引擎主驻留位(nvm/npx)已覆盖,doctor 的发现视图与本函数同源,
// 两面一致;后续确需扩面改 internal/engdirs 单点。
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
	// 停机信号面从第一行就位(#313 评审 P3):SIGTERM 必须能打断冷启
	// initdb(EnsureInternalPG 穿本 ctx),不能等链起来才有取消面。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if killed := supervise.KillInstanceOrphans(cfg.InstanceID); killed > 0 {
		log.Warn("上一世残留子进程已清杀", "count", killed)
	}

	// 子进程 cwd 必须存在(净机首启时数据根尚未落盘;spawn 对不存在的
	// Dir 直接失败)。幂等创建。
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return fmt.Errorf("stackd: 建子进程工作目录 %s: %w", cfg.WorkDir, err)
	}

	// 受管 pg 首启 bootstrap(幂等):既有集群零动作;净机首启 initdb。
	if cfg.pgInternal() {
		if err := EnsureInternalPG(ctx, filepath.Join(cfg.CurrentDir, "pg/bin"), cfg.pgDataDir(), cfg.runDir(), log); err != nil {
			return err
		}
	}

	m := supervise.New(supervise.Options{
		InstanceID: cfg.InstanceID,
		Log: func(msg string, kv ...any) {
			log.Info(msg, kv...)
		},
	})

	nodes, err := BuildNodes(cfg)
	if err != nil {
		return err
	}
	// BringUp 与停机信号赛跑(评审 P2-5):TERM 落在红门等待期时,靠
	// m.Shutdown 取消 Manager 根 ctx 中止门,而不是干等门预算耗尽
	// (systemd TimeoutStopSec=90s 会先 SIGKILL 留下孤儿)。
	bringUp := make(chan error, 1)
	go func() { bringUp <- chain.BringUp(ctx, nodes, m) }()
	select {
	case err := <-bringUp:
		if err != nil {
			m.Shutdown()
			writeState(cfg, m, log) // 失败也落状态:别让上一世的 running=true 装活(评审 P3-12)
			return fmt.Errorf("stackd: 链式拉起失败: %w", err)
		}
	case <-ctx.Done():
		m.Shutdown()
		<-bringUp // Shutdown 中止门后 BringUp 必然立刻返回
		writeState(cfg, m, log)
		log.Info("启动途中停机,链已收口")
		return nil
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

// StableInstanceID —— 跨 stackd 世代稳定的实例标记:boot_id+uid 派生。
// pid 派生(每世新 ID)会让 KillInstanceOrphans 永远找不到上一世孤儿
// (评审 P0-2);同 boot 同 uid 的崩溃重启共享 ID = 孤儿可认领,跨 boot/
// 跨用户天然隔离(重启后无残留进程,他人栈不相干)。
//
// 反面(评审 P3,#313 落文档):同机手动起第二个 stackd(沙箱/调试)
// 不带 --instance-id 时与生产共享本 ID → 两者 KillInstanceOrphans 互认
// 对方子进程为孤儿**互杀**。#284 沙箱已实踩;同机多栈必带
// --instance-id(cumora-stackd -h 有说明)。
func StableInstanceID() string {
	boot := "unknown-boot"
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		boot = strings.TrimSpace(string(data))
		if len(boot) >= 8 {
			boot = boot[:8]
		}
	}
	return fmt.Sprintf("stackd-%s-%d", boot, os.Getuid())
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
