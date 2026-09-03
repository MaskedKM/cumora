// stackd 单测(#282 PR-B):节点装配对照、daemon.env 合并、
// 以及带假二进制的 Run 全链(真进程 + 假探针 + SIGTERM 优雅停)。
package stackd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/chain"
	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
	"github.com/MaskedKM/cumora/apps/stack/internal/supervise"
)

// fakeCurrent —— 造一个含全部五件二进制的临时制品目录(假二进制 =
// 可执行 sh 脚本;探针全假,脚本只要活着)。
func fakeCurrent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, bin := range []string{"cumora-sidecar", "cumora-server", "cumora-daemon", "cumora-stack", "cumora-stackd"} {
		p := filepath.Join(dir, bin)
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func greenProbes() probe.Deps {
	return probe.Deps{
		PG: func(context.Context, string) (probe.PGInfo, error) {
			return probe.PGInfo{Version: "x", PgvectorAvailable: true}, nil
		},
		EnsureDatabase: func(context.Context, string, string) error { return nil },
		Redis:          func(context.Context, string) error { return nil },
		HTTP:           func(string, string) (int, error) { return 200, nil },
	}
}

func testConfig(t *testing.T) Config {
	return Config{
		CurrentDir:         fakeCurrent(t),
		WorkDir:            t.TempDir(),
		StateFile:          filepath.Join(t.TempDir(), "state.json"),
		InstanceID:         "test-stackd",
		ServerAddr:         "127.0.0.1:15181",
		SidecarPort:        15182,
		SidecarGateTimeout: 2 * time.Second,
		ServerGateTimeout:  2 * time.Second,
		Probes:             greenProbes(),
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildNodesOrderAndEnv(t *testing.T) {
	// 引擎目录断言环境无关:假 HOME 带 .nvm bin(容器/开发机通吃)。
	fakeHome := t.TempDir()
	nvmBin := filepath.Join(fakeHome, ".nvm/versions/node/v99.0.0/bin")
	if err := os.MkdirAll(nvmBin, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	cfg := testConfig(t)
	// daemon.env 合并面。
	denv := filepath.Join(t.TempDir(), "daemon.env")
	if err := os.WriteFile(denv, []byte("ANTHROPIC_AUTH_TOKEN=tok\nFAKE_KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.DaemonEnvFile = denv

	nodes, err := BuildNodes(cfg)
	if err != nil {
		t.Fatalf("BuildNodes: %v", err)
	}
	if len(nodes) != len(NodeNames) {
		t.Fatalf("节点数 %d, want %d", len(nodes), len(NodeNames))
	}
	for i, n := range nodes {
		if n.Name != NodeNames[i] {
			t.Fatalf("链序错位 [%d]=%s, want %s", i, n.Name, NodeNames[i])
		}
	}
	if nodes[0].Child != nil || nodes[2].Child == nil {
		t.Fatal("节点装配形态错误(external 应无 Child,managed 应有)")
	}
	// sidecar/server 注入 env 面。
	sEnv := strings.Join(nodes[2].Child.Env, "\n")
	if !strings.Contains(sEnv, "YJS_SIDECAR_PORT=15182") {
		t.Fatalf("sidecar env 缺端口注入: %s", sEnv)
	}
	vEnv := strings.Join(nodes[3].Child.Env, "\n")
	if !strings.Contains(vEnv, "CUMORA_GO_LISTEN=127.0.0.1:15181") ||
		!strings.Contains(vEnv, "CUMORA_GO_MIGRATIONS=") {
		t.Fatalf("server env 缺注入: %s", vEnv)
	}
	// daemon env:daemon.env 键 + PATH 钉扎(引擎目录并入)。
	dEnv := strings.Join(nodes[4].Child.Env, "\n")
	if !strings.Contains(dEnv, "ANTHROPIC_AUTH_TOKEN=tok") {
		t.Fatalf("daemon env 缺凭据合并: %s", dEnv)
	}
	if !strings.Contains(dEnv, "PATH=") || !strings.Contains(dEnv, nvmBin) {
		t.Fatalf("daemon env 缺引擎 PATH 钉扎: %s", dEnv)
	}
	// daemon args:轮询客户端形态。
	if strings.Join(nodes[4].Child.Args, " ") != "agent computer --server http://127.0.0.1:15181" {
		t.Fatalf("daemon args: %v", nodes[4].Child.Args)
	}
	// 自更新语义位:旧 unit 的 Environment=CUMORA_SUPERVISED=1 必须继承
	//(丢了 = selfupdate 静默失效,评审 P0-1 的回归锁)。
	if !strings.Contains(dEnv, "CUMORA_SUPERVISED=1") {
		t.Fatalf("daemon env 缺 CUMORA_SUPERVISED=1: %s", dEnv)
	}
}

// TestGateAcceptanceCodes —— 门接受面钉死:sidecar healthz 200|401 都绿
// (Bearer 面在岗),server livez 200|503 都绿(依赖红的诚实信号);其余码红。
func TestGateAcceptanceCodes(t *testing.T) {
	cfg := testConfig(t)
	code := 200
	cfg.Probes.HTTP = func(string, string) (int, error) { return code, nil }
	nodes, err := BuildNodes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sidecarGate := nodes[2].Child.Gate
	serverGate := nodes[3].Child.Gate
	for _, c := range []int{200, 401} {
		code = c
		if err := sidecarGate(context.Background()); err != nil {
			t.Fatalf("sidecar 门应接受 healthz %d: %v", c, err)
		}
	}
	for _, c := range []int{200, 503} {
		code = c
		if err := serverGate(context.Background()); err != nil {
			t.Fatalf("server 门应接受 livez %d: %v", c, err)
		}
	}
	for _, c := range []int{500, 404, 502} {
		code = c
		if err := sidecarGate(context.Background()); err == nil {
			t.Fatalf("sidecar 门应拒绝 %d", c)
		}
		if c != 503 && serverGate(context.Background()) == nil {
			t.Fatalf("server 门应拒绝 %d", c)
		}
	}
}

// TestStableInstanceID —— boot+uid 派生:同进程稳定;跨 stackd 世代
// (同 boot 同 uid)共享 = 孤儿认领有效(评审 P0-2 的机制锁)。
func TestStableInstanceID(t *testing.T) {
	a, b := StableInstanceID(), StableInstanceID()
	if a == "" || a != b {
		t.Fatalf("实例 ID 应非空且稳定: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "stackd-") {
		t.Fatalf("实例 ID 前缀: %q", a)
	}
	// 用稳定 ID 标记的"上一世残留"必须被认领清杀。
	stray := exec.Command("/bin/sh", "-c", "sleep 60")
	stray.Env = append(os.Environ(),
		"CUMORA_STACK_CHILD=1", "CUMORA_STACK_INSTANCE="+a)
	if err := stray.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stray.Process.Kill() }()
	time.Sleep(50 * time.Millisecond) // 等 /proc/<pid>/environ 可读
	if n := supervise.KillInstanceOrphans(a); n < 1 {
		t.Fatalf("稳定 ID 应认领上一世残留,击杀 %d", n)
	}
}

func TestBuildNodesRejectsMissingBinaries(t *testing.T) {
	cfg := testConfig(t)
	if err := os.Remove(filepath.Join(cfg.CurrentDir, "cumora-stackd")); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildNodes(cfg); err == nil {
		t.Fatal("缺 stackd 二进制应拒绝装配")
	}
}

func TestDaemonEnvFileMissingIsNotError(t *testing.T) {
	env, err := loadDaemonEnv(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil || env != nil {
		t.Fatalf("缺失的 daemon.env 应为空集无错: %v %v", env, err)
	}
}

// runningCount —— 从状态文件数 running 子进程;-1 = 读/解析失败。
func runningCount(stateFile string) int {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return -1
	}
	var snap Snapshot
	if json.Unmarshal(data, &snap) != nil {
		return -1
	}
	n := 0
	for _, c := range snap.Children {
		if c.Running {
			n++
		}
	}
	return n
}

// TestRunFakeStack —— 全链集成:Run(假二进制+绿探针)→ 状态文件出现
// 三个 running 子进程 → SIGTERM → Run 返回 nil,状态文件收尾为停机态。
func TestRunFakeStack(t *testing.T) {
	cfg := testConfig(t)
	done := make(chan error, 1)
	go func() { done <- Run(cfg, discardLogger()) }()

	deadline := time.Now().Add(10 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		if runningCount(cfg.StateFile) == 3 {
			up = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !up {
		t.Fatal("10s 内未见到三个 running 子进程")
	}
	started := time.Now()
	// NotifyContext 拦截 SIGTERM,测试进程不死。
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅停机应返回 nil: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("SIGTERM 后 15s 未退出")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("停链耗时过长: %s", elapsed)
	}
	if n := runningCount(cfg.StateFile); n != 0 {
		t.Fatalf("停机后不应有 running 子进程,实得 %d", n)
	}
}

// TestRunGateFailureAborts —— 门红到底 → Run 返回错误(启动失败交
// systemd Restart=always,与旧 unit ExecStartPost 门同语义)。
func TestRunGateFailureAborts(t *testing.T) {
	cfg := testConfig(t)
	// sidecar 的 healthz 门红到底(GateTimeout 由 BuildNodes 设 60s ——
	// 压短:直接把 HTTP 探针做成永红,60s 预算内必失败)。
	cfg.Probes.HTTP = func(string, string) (int, error) { return 0, context.DeadlineExceeded }
	done := make(chan error, 1)
	go func() { done <- Run(cfg, discardLogger()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("门红到底应返回错误")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("门超时(压短为 2s)后未返回")
	}
}

// fakeCurrentInternal —— fakeCurrent + 受管形态三件(redis-server、
// pg/bin/postgres、pg/bin/initdb)。initdb 桩按 -D 落 PG_VERSION,并在
// 数据目录旁留运行标记(幂等断言用)。
func fakeCurrentInternal(t *testing.T) string {
	t.Helper()
	dir := fakeCurrent(t)
	for _, rel := range []string{"redis-server", "pg/bin/postgres"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initdb := filepath.Join(dir, "pg/bin/initdb")
	script := `#!/bin/sh
D=""
while [ $# -gt 0 ]; do
  case "$1" in
    -D) D="$2"; shift ;;
    --auth-local|--auth-host|-U) shift ;;
  esac
  shift
done
[ -n "$D" ] || { echo "no -D" >&2; exit 1; }
mkdir -p "$D"
echo 16 > "$D/PG_VERSION"
echo ran >> "$(dirname "$D")/initdb-ran.log"
`
	if err := os.WriteFile(initdb, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func internalConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.CurrentDir = fakeCurrentInternal(t)
	cfg.DataHome = t.TempDir()
	cfg.PGMode = stackconfig.ModeInternal
	cfg.RedisMode = stackconfig.ModeInternal
	return cfg
}

// 受管装配面:pg/redis 节点切 Managed、postgres 参数 socket-only、
// redis 参数 unix socket + 端口 0、server 注入派生 DSN/URL。
func TestBuildNodesInternalManaged(t *testing.T) {
	cfg := internalConfig(t)
	nodes, err := BuildNodes(cfg)
	if err != nil {
		t.Fatalf("BuildNodes: %v", err)
	}
	pg, redis := nodes[0], nodes[1]
	if pg.Mode != chain.Managed || pg.Child == nil {
		t.Fatalf("受管形态 pg 节点应 Managed 带 Child: %+v", pg)
	}
	if pg.Child.Path != filepath.Join(cfg.CurrentDir, "pg/bin/postgres") {
		t.Fatalf("pg 二进制约束: %s", pg.Child.Path)
	}
	args := strings.Join(pg.Child.Args, " ")
	for _, want := range []string{"-D " + cfg.pgDataDir(), "-k " + cfg.runDir(), "-h"} {
		if !strings.Contains(args, want) {
			t.Fatalf("postgres args 缺 %q: %s", want, args)
		}
	}
	if redis.Mode != chain.Managed || redis.Child == nil {
		t.Fatalf("受管形态 redis 节点应 Managed 带 Child: %+v", redis)
	}
	rargs := strings.Join(redis.Child.Args, " ")
	if !strings.Contains(rargs, "--unixsocket "+cfg.redisSocket()) || !strings.Contains(rargs, "--port 0") {
		t.Fatalf("redis args: %s", rargs)
	}
	vEnv := strings.Join(nodes[3].Child.Env, "\n")
	if !strings.Contains(vEnv, "DATABASE_URL=host="+cfg.runDir()) ||
		!strings.Contains(vEnv, "dbname="+cfg.pgDatabase()) {
		t.Fatalf("server env 缺受管 DATABASE_URL: %s", vEnv)
	}
	if !strings.Contains(vEnv, "REDIS_URL=unix://"+cfg.redisSocket()) {
		t.Fatalf("server env 缺受管 REDIS_URL: %s", vEnv)
	}
	// #333:sidecar 与 server 同款注入 —— 缺这半边时 sidecar 捡继承的
	// 外部 DSN/客户端默认 TCP,internal 形态启动即 fatal。DATABASE_URL
	// 是 URL 形态(node-pg 不认 libpq 关键字串),REDIS_URL 与 server 同值。
	sEnv := strings.Join(nodes[2].Child.Env, "\n")
	if !strings.Contains(sEnv, "YJS_SIDECAR_PORT=") {
		t.Fatalf("sidecar env 缺端口注入: %s", sEnv)
	}
	if !strings.Contains(sEnv, "DATABASE_URL=postgres://cumora@localhost/"+cfg.pgDatabase()+"?host="+cfg.runDir()) {
		t.Fatalf("sidecar env 缺受管 DATABASE_URL(URL 形态): %s", sEnv)
	}
	if !strings.Contains(sEnv, "REDIS_URL=unix://"+cfg.redisSocket()) {
		t.Fatalf("sidecar env 缺受管 REDIS_URL: %s", sEnv)
	}
}

// external 装配零变:存量部署回归锁(不注入 DSN,保持探测形态)。
func TestBuildNodesExternalDoesNotInjectDSN(t *testing.T) {
	cfg := testConfig(t)
	cfg.CurrentDir = fakeCurrent(t) // 无 pg/redis 件也能装配
	nodes, err := BuildNodes(cfg)
	if err != nil {
		t.Fatalf("BuildNodes: %v", err)
	}
	if nodes[0].Mode != chain.External || nodes[1].Mode != chain.External {
		t.Fatal("缺省形态应 external")
	}
	for _, n := range []struct {
		name string
		idx  int
	}{{"server", 3}, {"sidecar", 2}} {
		env := strings.Join(nodes[n.idx].Child.Env, "\n")
		if strings.Contains(env, "DATABASE_URL=") || strings.Contains(env, "REDIS_URL=") {
			t.Fatalf("external 不应给 %s 注入 DSN: %s", n.name, env)
		}
	}
}

// 受管形态缺件拒绝装配(载荷不完整 = 当场红,不进运行期)。
func TestBuildNodesInternalMissingDeps(t *testing.T) {
	cfg := testConfig(t)
	cfg.PGMode = stackconfig.ModeInternal
	cfg.DataHome = t.TempDir()
	if _, err := BuildNodes(cfg); err == nil {
		t.Fatal("缺 pg/redis 件应报错")
	}
}

// pg 门硬要求 pgvector(migrations 的 vector 列依赖;缺扩展不靠 server 扑空)。
func TestInternalGateRequiresPgvector(t *testing.T) {
	cfg := internalConfig(t)
	cfg.Probes.PG = func(context.Context, string) (probe.PGInfo, error) {
		return probe.PGInfo{Version: "x", PgvectorAvailable: false}, nil
	}
	nodes, err := BuildNodes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodes[0].Child.Gate(context.Background()); err == nil || !strings.Contains(err.Error(), "pgvector") {
		t.Fatalf("缺 pgvector 应门红: %v", err)
	}
}

// initdb bootstrap:首启落 PG_VERSION(staging 原子落位);二次调用零动作
// (幂等);socket 目录无条件收紧 0700。
func TestEnsureInternalPGIdempotent(t *testing.T) {
	dir := fakeCurrentInternal(t)
	dataHome := t.TempDir()
	dataDir := filepath.Join(dataHome, "pgdata")
	runDir := filepath.Join(dataHome, "run")
	// 预置 0755 run 目录:EnsureInternalPG 必须收紧(评审 P3-12)。
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureInternalPG(context.Background(), filepath.Join(dir, "pg/bin"), dataDir, runDir, discardLogger()); err != nil {
		t.Fatalf("首启: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "PG_VERSION")); err != nil {
		t.Fatalf("PG_VERSION: %v", err)
	}
	st, err := os.Stat(runDir)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("runDir 应无条件 0700: %v %v", err, st)
	}
	first, _ := os.ReadFile(filepath.Join(dataHome, "initdb-ran.log"))
	if err := EnsureInternalPG(context.Background(), filepath.Join(dir, "pg/bin"), dataDir, runDir, discardLogger()); err != nil {
		t.Fatalf("二次: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(dataHome, "initdb-ran.log"))
	if string(first) != string(second) {
		t.Fatalf("二次调用不应重跑 initdb: %q → %q", first, second)
	}
}

// initdb 失败:pgdata 不落位、staging 清理(评审 P2 —— 残缺集群会把
// 幂等门永久卡死,净机无自愈路径)。
func TestEnsureInternalPGFailureCleansUp(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "pg", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "initdb"),
		[]byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	dataDir := filepath.Join(dataHome, "pgdata")
	if err := EnsureInternalPG(context.Background(), binDir, dataDir, filepath.Join(dataHome, "run"), discardLogger()); err == nil {
		t.Fatal("initdb 失败应返回错误")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("失败后 pgdata 不应存在: %v", err)
	}
	entries, _ := os.ReadDir(dataHome)
	leftover := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pgdata.staging") {
			leftover++
		}
	}
	if leftover != 0 {
		t.Fatalf("staging 残留 %d 份", leftover)
	}
}

// ctx 取消(SIGTERM 同道)必须即刻打断 initdb(#313 评审 P3-1:冷启
// 5 分钟预算内,栈停机不能被 initdb 拖住等 systemd SIGKILL)。同时断言
// 失败路径的原子性:pgdata 不落位、staging 清理。
func TestEnsureInternalPGContextCancelInterrupts(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "pg", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// initdb 桩:长跑(exec sleep —— 进程本体就是长跑者;若经 shell 留
	// 孙进程,KILL 后孙进程抱管道会把 CombinedOutput 挂死,WaitDelay 保
	// 险带正是为此)。取消必须把进程杀掉 CombinedOutput 才会返回。
	if err := os.WriteFile(filepath.Join(binDir, "initdb"),
		[]byte("#!/bin/sh\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	dataDir := filepath.Join(dataHome, "pgdata")

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- EnsureInternalPG(ctx, binDir, dataDir, filepath.Join(dataHome, "run"), discardLogger())
	}()
	time.Sleep(200 * time.Millisecond) // 等 initdb 真正起跑
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("取消后应返回错误")
		}
		// 断言吃错误链而非中文文案(评审 P3:换文案即碎)。kill 成功
		// 路径 Wait 直接返回 ctx.Err()。
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "被取消") {
			t.Fatalf("错误应注明取消语义,实得: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消未打断 initdb(5s 仍在跑)")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("取消后 pgdata 不应落位: %v", err)
	}
	entries, _ := os.ReadDir(dataHome)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pgdata.staging") {
			t.Fatalf("取消后 staging 应清理,残留 %s", e.Name())
		}
	}
}

// WaitDelay 保险带的回归防线(评审 P3:误删 cmd.WaitDelay 无测试会红):
// 桩不带 exec —— shell 被杀后 sleep 孙进程抱住输出管道,CombinedOutput
// 若无 WaitDelay 会等管道 EOF 挂满 300s;有 WaitDelay 则取消后至多 5s
// 强制收口返回。
func TestEnsureInternalPGWaitDelayClosesOrphanedPipe(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "pg", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "initdb"),
		[]byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dataHome := t.TempDir()
	dataDir := filepath.Join(dataHome, "pgdata")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	started := time.Now()
	go func() {
		errc <- EnsureInternalPG(ctx, binDir, dataDir, filepath.Join(dataHome, "run"), discardLogger())
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	// WaitDelay=5s + 起跑余量:10s 内必须返回(无保险带 = 挂到 300s)。
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("取消后应返回错误")
		}
		if el := time.Since(started); el > 10*time.Second {
			t.Fatalf("返回过迟(%s):WaitDelay 保险带疑似失效", el)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("孤儿管道未被 WaitDelay 收口(10s 仍在等 EOF)")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("取消后 pgdata 不应落位: %v", err)
	}
}

// 混合形态(评审 P1):pg=internal + redis=external 是合法组合,
// 不得因缺 redis-server 拒装配。
func TestBuildNodesMixedModesNoRedisBinaryNeeded(t *testing.T) {
	cfg := testConfig(t) // fakeCurrent:无 redis-server / 无 pg/
	cfg.DataHome = t.TempDir()
	// 造一个只有 pg 件的制品目录。
	dir := fakeCurrent(t)
	pgBin := filepath.Join(dir, "pg", "bin")
	if err := os.MkdirAll(pgBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"postgres", "initdb"} {
		if err := os.WriteFile(filepath.Join(pgBin, rel),
			[]byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg.CurrentDir = dir
	cfg.PGMode = stackconfig.ModeInternal
	cfg.RedisMode = stackconfig.ModeExternal
	nodes, err := BuildNodes(cfg)
	if err != nil {
		t.Fatalf("混合形态应可装配: %v", err)
	}
	if nodes[0].Mode != chain.Managed || nodes[1].Mode != chain.External {
		t.Fatalf("混合形态装配: pg=%s redis=%s", nodes[0].Mode, nodes[1].Mode)
	}
}

// 受管全链 Run:postgres/redis 以子进程起,状态面五件全 running。
func TestRunFakeStackInternal(t *testing.T) {
	cfg := internalConfig(t)
	cfg.InternalGateTimeout = 2 * time.Second
	done := make(chan error, 1)
	go func() { done <- Run(cfg, discardLogger()) }()

	deadline := time.Now().Add(10 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		if runningCount(cfg.StateFile) == 5 {
			up = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !up {
		t.Fatalf("10s 内未见五个 running 子进程(实得 %d)", runningCount(cfg.StateFile))
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("优雅停机应返回 nil: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("SIGTERM 后 15s 未退出")
	}
}
