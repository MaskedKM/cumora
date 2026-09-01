// stackd 单测(#282 PR-B):节点装配对照、daemon.env 合并、
// 以及带假二进制的 Run 全链(真进程 + 假探针 + SIGTERM 优雅停)。
package stackd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
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
		Redis: func(context.Context, string) error { return nil },
		HTTP:  func(string, string) (int, error) { return 200, nil },
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
