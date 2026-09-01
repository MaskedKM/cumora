// supervise 单测(#282 PR-A):真子进程监督,非 mock —— 退出/挂死/
// crash-loop/TERM 陷阱/孤儿认领全部是真进程行为。子脚本只用 POSIX sh
// 语法(CI 容器为 alpine/busybox sh)。
package supervise

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitFor —— 轮询断言:cond 为真或超时致命。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时(%s)", what, timeout)
}

func stateByName(t *testing.T, m *Manager, name string) State {
	t.Helper()
	for _, st := range m.States() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("找不到 %s 的状态", name)
	return State{}
}

func shChild(name, script string, mod func(*Child)) Child {
	c := Child{Name: name, Path: "/bin/sh", Args: []string{"-c", script}}
	if mod != nil {
		mod(&c)
	}
	return c
}

func fastOpts() Options {
	return Options{
		StartBackoff:  20 * time.Millisecond,
		MaxBackoff:    160 * time.Millisecond,
		StableAfter:   10 * time.Second, // 测试内不会触发稳定重置
		CircuitWindow: 30 * time.Second,
		CircuitFails:  100, // 默认不熔断
	}
}

func TestStartGateNil_RecordsRunning(t *testing.T) {
	m := New(fastOpts())
	defer m.Shutdown()
	if err := m.Start(shChild("sleeper", "sleep 30", nil)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := stateByName(t, m, "sleeper")
	if !st.Running || st.PID == 0 {
		t.Fatalf("门 nil 应立即记 running: %+v", st)
	}
	m.Shutdown()
	st = stateByName(t, m, "sleeper")
	if st.Running {
		t.Fatal("Shutdown 后不应 running")
	}
}

func TestChildLogPiped(t *testing.T) {
	var mu = make(chan string, 8)
	opts := fastOpts()
	opts.Log = func(msg string, kv ...any) {
		if msg == "child log" {
			for i := 0; i+1 < len(kv); i += 2 {
				if kv[i] == "line" {
					mu <- kv[i+1].(string)
				}
			}
		}
	}
	m := New(opts)
	defer m.Shutdown()
	_ = m.Start(shChild("talker", "echo hello-from-child; sleep 30", nil))
	select {
	case line := <-mu:
		if line != "hello-from-child" {
			t.Fatalf("子进程输出应按行透传: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内未收到子进程输出行")
	}
}

func TestGateTimeout_KillsAndRecords(t *testing.T) {
	m := New(fastOpts())
	defer m.Shutdown()
	c := shChild("gated", "sleep 30", func(c *Child) {
		c.Gate = func(context.Context) error { return context.DeadlineExceeded }
		c.GateEvery = 20 * time.Millisecond
		c.GateTimeout = 150 * time.Millisecond
	})
	err := m.Start(c)
	if err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("门永不过应返回含 gate 的错误,实得: %v", err)
	}
	// 语义(评审 P2-1):Start 失败 = 状态条目移除、同名可重试。
	// 候选世代的清理由 terminateSolo 完成,不存在遗留 Running 状态。
	for _, st := range m.States() {
		if st.Name == "gated" {
			t.Fatalf("Start 失败后不应残留状态条目: %+v", st)
		}
	}
	// 同名重试入口仍在(新的 Start 不再报"已在管理中")。
	if err := m.Start(shChild("gated", "sleep 30", func(c *Child) {
		c.Gate = nil // 本次无门,应成功——证明名字未被毒化
	})); err != nil {
		t.Fatalf("失败后同名应可重试: %v", err)
	}
}

func TestCrashLoopRestarts(t *testing.T) {
	m := New(fastOpts())
	defer m.Shutdown()
	if err := m.Start(shChild("crasher", "exit 3", nil)); err != nil {
		t.Fatalf("首世 spawn 应成功: %v", err)
	}
	waitFor(t, 2*time.Second, "至少 3 次重启", func() bool {
		return stateByName(t, m, "crasher").Restarts >= 3
	})
	if stateByName(t, m, "crasher").CircuitOpen {
		t.Fatal("CircuitFails=100 时不应熔断")
	}
}

func TestCircuitOpensAndStopsRestarting(t *testing.T) {
	opts := fastOpts()
	opts.CircuitFails = 3
	opts.CircuitWindow = 30 * time.Second
	m := New(opts)
	defer m.Shutdown()
	if err := m.Start(shChild("burner", "exit 1", nil)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 3*time.Second, "熔断打开", func() bool {
		return stateByName(t, m, "burner").CircuitOpen
	})
	r1 := stateByName(t, m, "burner").Restarts
	time.Sleep(300 * time.Millisecond) // 3×StartBackoff 以上,足够再重启好几轮
	r2 := stateByName(t, m, "burner").Restarts
	if r2 != r1 {
		t.Fatalf("熔断后不应继续重启: %d → %d", r1, r2)
	}
	if stateByName(t, m, "burner").Running {
		t.Fatal("熔断态不应有活进程")
	}
}

func TestGracefulStop_TermTrap(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "flag")
	ready := filepath.Join(dir, "ready")
	m := New(fastOpts())
	// ready 标记在 trap 之后:标记出现 = 陷阱已装好(放在 trap 前会留
	// "TERM 落在两行之间走默认处置"的窗口)。
	script := "trap 'touch " + flag + "; exit 0' TERM\ntouch " + ready + "\nwhile :; do sleep 1; done"
	if err := m.Start(shChild("trapper", script, nil)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 2*time.Second, "陷阱装好", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	start := time.Now()
	if err := m.Stop("trapper"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("TERM 陷阱应立即退出,耗时 %s", elapsed)
	}
	if _, err := os.Stat(flag); err != nil {
		t.Fatal("子进程应执行 TERM 陷阱(优雅停机语义)")
	}
	if stateByName(t, m, "trapper").Running {
		t.Fatal("Stop 后不应 running")
	}
}

func TestKillAfterGrace_IgnoredTerm(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	opts := fastOpts()
	m := New(opts)
	// ready 标记在 trap '' 之后:标记出现 = 忽略陷阱已装好(放前面留
	// 默认处置秒退的窗口,耗时断言会误报)。
	script := "trap '' TERM\ntouch " + ready + "\nwhile :; do sleep 1; done"
	if err := m.Start(shChild("ignorer", script, func(c *Child) {
		c.StopGrace = 120 * time.Millisecond
	})); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, 2*time.Second, "忽略陷阱装好", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	start := time.Now()
	m.Shutdown()
	elapsed := time.Since(start)
	if elapsed < 120*time.Millisecond {
		t.Fatalf("忽略 TERM 的子进程应等到宽限后被 KILL(耗时 %s)", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("KILL 升级应发生在宽限附近(耗时 %s)", elapsed)
	}
	if stateByName(t, m, "ignorer").Running {
		t.Fatal("KILL 后不应 running")
	}
}

func TestShutdownReverseOrder(t *testing.T) {
	dir := t.TempDir()
	order := filepath.Join(dir, "order")
	mk := func(name string) Child {
		// started 标记在 trap 之后:标记出现 = 陷阱已装好(放在 trap 前
		// 会留"TERM 落在两行之间走默认处置"的窗口)。
		script := "trap 'echo " + name + " >> " + order + "; exit 0' TERM\n" +
			"echo " + name + "-started >> " + order + "\n" +
			"while :; do sleep 1; done"
		return shChild(name, script, nil)
	}
	m := New(fastOpts())
	if err := m.Start(mk("first")); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	if err := m.Start(mk("second")); err != nil {
		t.Fatalf("Start second: %v", err)
	}
	waitFor(t, 2*time.Second, "两个子进程都装好陷阱", func() bool {
		data, _ := os.ReadFile(order)
		return strings.Count(string(data), "-started") == 2
	})
	m.Shutdown()
	data, err := os.ReadFile(order)
	if err != nil {
		t.Fatalf("读顺序文件: %v", err)
	}
	var got []string
	for _, f := range strings.Fields(string(data)) {
		if !strings.HasSuffix(f, "-started") {
			got = append(got, f)
		}
	}
	if len(got) != 2 || got[0] != "second" || got[1] != "first" {
		t.Fatalf("停机应逆启动序(second→first),实得 %v", got)
	}
}

func TestKillInstanceOrphans(t *testing.T) {
	const inst = "orphan-test-inst"
	stray := spawnStray(t, inst)
	defer func() { _ = stray.Process.Kill() }()

	if n := KillInstanceOrphans(inst); n < 1 {
		t.Fatalf("应击杀至少 1 个残留,实得 %d", n)
	}
	// 进程确已死(Wait 很快返回)。
	done := make(chan struct{})
	go func() { _ = stray.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("残留进程未被击杀")
	}
	// 不同实例标记的不误伤。
	other := spawnStray(t, "other-inst")
	defer func() { _ = other.Process.Kill() }()
	if n := KillInstanceOrphans(inst); n != 0 {
		t.Fatalf("不同实例标记不应被误伤,实得 %d", n)
	}
	_ = other.Process.Kill()
}

// spawnStray —— 手工造一个带本包标记的"上一世残留"进程。
func spawnStray(t *testing.T, inst string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	cmd.Env = append(os.Environ(), EnvChild+"=1", EnvInstance+"="+inst)
	if err := cmd.Start(); err != nil {
		t.Fatalf("造残留进程: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // 让 /proc/<pid>/environ 可读
	return cmd
}

func TestNextBackoffPure(t *testing.T) {
	cases := []struct {
		name                    string
		cur, start, max, stable time.Duration
		ran                     time.Duration
		want                    time.Duration
	}{
		{"稳定重置", 8 * time.Second, time.Second, 30 * time.Second, 60 * time.Second, 90 * time.Second, time.Second},
		{"翻倍", time.Second, time.Second, 30 * time.Second, 60 * time.Second, 5 * time.Second, 2 * time.Second},
		{"封顶", 20 * time.Second, time.Second, 30 * time.Second, 60 * time.Second, 5 * time.Second, 30 * time.Second},
	}
	for _, c := range cases {
		if got := NextBackoff(c.cur, c.start, c.max, c.stable, c.ran); got != c.want {
			t.Fatalf("%s: NextBackoff=%s, want %s", c.name, got, c.want)
		}
	}
}

func TestStartAfterShutdownCancels(t *testing.T) {
	// P2-2 路径:根 ctx 已取消后的 Start —— run 已提交也必须就地收口
	// (出列、清状态、terminateSolo 唯一收尸、close lifeDone),返回错误
	// 而不是留下 Shutdown 清单外的漏网活世代。
	m := New(fastOpts())
	m.Shutdown() // 根 ctx 取消
	err := m.Start(shChild("late", "sleep 30", nil))
	if err == nil {
		t.Fatal("Shutdown 后 Start 应报错")
	}
	for _, st := range m.States() {
		if st.Name == "late" {
			t.Fatalf("取消路径不应残留状态条目: %+v", st)
		}
	}
	// 无泄漏的监控协程:Wait 立即返回(wg 计数为零,monitor 从未起)。
	done := make(chan struct{})
	go func() { m.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("不应有监控协程残留")
	}
}
