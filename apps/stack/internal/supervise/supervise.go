// supervise —— stackd 的子进程管理引擎(#282 PR-A,ADR 0005 阶段 1)。
//
// 职责边界(与 server 侧 watchdog 互补不重叠,票面 AC):
//
//	supervise 管"进程死了按退避拉起"(本包);
//	watchdog(#259/#262)管"进程活着但业务层失联判死"(server 侧)。
//
// 因此本包永不因"健康探针变黄"杀子进程——只响应进程退出与停机指令。
//
// 语义:
//   - 拉起 = spawn + 健康门轮询(门过才算 up;门超时 = 杀 + 记失败)
//   - 退出重启 = 指数退避(StartBackoff 起步,稳定超 StableAfter 重置,
//     封顶 MaxBackoff);熔断 = CircuitWindow 内满 CircuitFails 次
//     → 停止重启,状态 CircuitOpen(degraded 显性化交消费方)
//   - 优雅停 = TERM → StopGrace 宽限 → KILL(进程组,连孙进程收口)
//   - 孤儿认领 = stackd 自身被 systemd 拉回后,按实例标记清杀上一世
//     的残留子进程(KillInstanceOrphans)
//
// 收尸纪律:每个 cmd 恰好一个 Wait 调用者 —— 成功提交给 run 的世代由
// monitor 收尸(killRun 等 lifeDone 信号);门超时世代的 cmd 尚未提交,
// 由 spawn 调用者经 terminateSolo 收尸。exec.Cmd 禁止并发 Wait(Go 1.24
// 的 awaitGoroutines:第二个 Wait 永阻塞)——这条纪律不可破。
package supervise

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 子进程标记:孤儿认领的依据。INSTANCE 由 stackd 生成(每次进程生命期
// 唯一),CHILD 区分 stackd 直属子进程与其孙进程(daemon 的引擎)。
const (
	EnvChild    = "CUMORA_STACK_CHILD"
	EnvInstance = "CUMORA_STACK_INSTANCE"
)

// Gate —— 启动健康门:返回 nil 视为就绪。nil Gate = 进程存活即过。
type Gate func(ctx context.Context) error

// Child —— 受管子进程声明。
type Child struct {
	Name        string
	Path        string
	Args        []string
	Env         []string // 追加在 os.Environ() 之上
	Dir         string
	Gate        Gate
	GateEvery   time.Duration // 门轮询间隔;0 = 250ms
	GateTimeout time.Duration // 门总预算;0 = 30s
	StopGrace   time.Duration // TERM→KILL 宽限;0 = 10s
}

func (c *Child) gateEvery() time.Duration {
	if c.GateEvery > 0 {
		return c.GateEvery
	}
	return 250 * time.Millisecond
}

func (c *Child) gateTimeout() time.Duration {
	if c.GateTimeout > 0 {
		return c.GateTimeout
	}
	return 30 * time.Second
}

func (c *Child) stopGrace() time.Duration {
	if c.StopGrace > 0 {
		return c.StopGrace
	}
	return 10 * time.Second
}

// Options —— 引擎参数(全部有默认;测试注入小值)。
type Options struct {
	InstanceID    string        // 孤儿认领标记;空 = 用自身 PID
	StartBackoff  time.Duration // 0 = 1s
	MaxBackoff    time.Duration // 0 = 30s
	StableAfter   time.Duration // 0 = 60s(单次存活多久算稳定 → 重置退避)
	CircuitWindow time.Duration // 0 = 60s
	CircuitFails  int           // 0 = 5
	// Log —— 引擎事件与子进程输出出口(svc=name 由本包注入)。
	Log func(msg string, kv ...any)
}

// State —— 单子进程状态快照(JSON 形态由 stackd 落状态文件,PR-B)。
type State struct {
	Name        string    `json:"name"`
	Running     bool      `json:"running"`
	PID         int       `json:"pid,omitempty"`
	Restarts    int       `json:"restarts"`
	LastErr     string    `json:"lastErr,omitempty"`
	CircuitOpen bool      `json:"circuitOpen"`
	StartedAt   time.Time `json:"startedAt,omitzero"`
	HealthyAt   time.Time `json:"healthyAt,omitzero"`
}

// childRun —— 一个受管子进程的跨世代载体。cmd/lifeDone/started 描述
// "当前已提交的世代"(spawn 成功才提交),读写下均持 m.mu。
type childRun struct {
	child    Child
	cancel   context.CancelFunc // 停该子的监控循环
	cmd      *exec.Cmd          // 当前世代(spawn 成功时提交)
	lifeDone chan struct{}      // 当前世代退出信号(monitor Wait 后 close)
	started  time.Time          // 当前世代启动时刻(退避重置判据)
}

// Manager —— 子进程集合管理器。Start 按依赖链顺序串行调用;
// Shutdown 逆启动序全停;States 线程安全。
type Manager struct {
	opts   Options
	mu     sync.Mutex
	runs   []*childRun // 启动序(Shutdown 逆序遍历)
	states map[string]*State
	fails  map[string][]time.Time // 熔断滑窗
	ctx    context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup
}

func New(opts Options) *Manager {
	if opts.StartBackoff <= 0 {
		opts.StartBackoff = time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 30 * time.Second
	}
	if opts.StableAfter <= 0 {
		opts.StableAfter = time.Minute
	}
	if opts.CircuitWindow <= 0 {
		opts.CircuitWindow = time.Minute
	}
	if opts.CircuitFails <= 0 {
		opts.CircuitFails = 5
	}
	if opts.InstanceID == "" {
		opts.InstanceID = "stackd-" + strconv.Itoa(os.Getpid())
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		opts:   opts,
		states: map[string]*State{},
		fails:  map[string][]time.Time{},
		ctx:    ctx,
		stop:   cancel,
	}
}

func (m *Manager) log(msg, name string, kv ...any) {
	m.opts.Log(msg, append([]any{"svc", name}, kv...)...)
}

// Start —— 同步拉起:spawn → 门轮询到过(或超时);成功转后台监控。
// 门超时/spawn 失败返回 error(子进程已清理,失败已记滑窗)。
// 同名子进程已在管理中返回错误。
func (m *Manager) Start(child Child) error {
	m.mu.Lock()
	if _, exists := m.states[child.Name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("supervise: %s 已在管理中", child.Name)
	}
	st := &State{Name: child.Name}
	m.states[child.Name] = st
	m.mu.Unlock()

	run := &childRun{child: child}
	if err := m.spawn(run, st); err != nil {
		m.recordFailure(child.Name, err)
		return err
	}

	runCtx, runCancel := context.WithCancel(m.ctx)
	run.cancel = runCancel
	m.mu.Lock()
	m.runs = append(m.runs, run)
	m.mu.Unlock()

	m.wg.Add(1)
	go m.monitor(runCtx, run, st)
	return nil
}

// spawn —— 起"候选世代"并过门;只有门通过才提交进 run(见收尸纪律)。
// 可从 Start(无 monitor)与 monitor 内环(不在 Wait 中)调用。
func (m *Manager) spawn(run *childRun, st *State) error {
	c := run.child
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Env = append(cmd.Env, EnvChild+"=1", EnvInstance+"="+m.opts.InstanceID)
	cmd.Stdout = lineWriter{m: m, name: c.Name}
	cmd.Stderr = lineWriter{m: m, name: c.Name}
	// 独立进程组:KILL 升级时连孙进程(daemon 的引擎)一起收口,
	// 不波及 stackd 自身组。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", c.Path, err)
	}

	// 健康门:轮询到过或超时;超时 = 候选世代就此终结(独立收尸,
	// 不提交进 run),记失败返回。
	if c.Gate != nil {
		gateCtx, cancel := context.WithTimeout(m.ctx, c.gateTimeout())
		defer cancel()
		tick := time.NewTicker(c.gateEvery())
		defer tick.Stop()
		for {
			err := c.Gate(gateCtx)
			if err == nil {
				break
			}
			if gateCtx.Err() != nil {
				terminateSolo(cmd, c.stopGrace())
				return fmt.Errorf("gate 超时(%s): %w", c.gateTimeout(), err)
			}
			select {
			case <-gateCtx.Done():
			case <-tick.C:
			}
		}
	}

	now := time.Now()
	m.mu.Lock()
	run.cmd = cmd
	run.lifeDone = make(chan struct{})
	run.started = now
	m.mu.Unlock()
	st.Running = true
	st.PID = cmd.Process.Pid
	st.StartedAt = now
	st.LastErr = ""
	st.HealthyAt = now
	m.log("child up", c.Name, "pid", st.PID)
	return nil
}

// monitor —— 单子进程监控循环。外环等当前世代退出;内环退避重试
// 到新世代成功提交为止 —— 内环绝不回落去 Wait 已收尸的世代(那是
// 并发 Wait 死锁的 cousins)。
func (m *Manager) monitor(ctx context.Context, run *childRun, st *State) {
	defer m.wg.Done()
	backoff := m.opts.StartBackoff
	for {
		m.mu.Lock()
		cmd := run.cmd
		lifeDone := run.lifeDone
		started := run.started
		m.mu.Unlock()
		err := cmd.Wait()
		close(lifeDone)
		m.mu.Lock()
		st.Running = false
		st.PID = 0
		st.LastErr = errString(err)
		m.mu.Unlock()
		m.log("child exited", run.child.Name, "err", errString(err))

		if ctx.Err() != nil {
			return // 停机路径:Stop/Shutdown 已清理,不重启
		}
		if m.recordFailure(run.child.Name, fmt.Errorf("exit: %s", errString(err))) {
			m.log("circuit open — 停止重启", run.child.Name)
			return
		}

		for {
			backoff = NextBackoff(backoff, m.opts.StartBackoff, m.opts.MaxBackoff,
				m.opts.StableAfter, time.Since(started))
			m.log("restarting", run.child.Name, "in", backoff.String())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if err := m.spawn(run, st); err == nil {
				m.mu.Lock()
				st.Restarts++
				m.mu.Unlock()
				break // 新世代已提交,回外环等它
			} else {
				// 候选世代失败(spawn/门超时)同样计入熔断滑窗。
				if m.recordFailure(run.child.Name, err) {
					m.log("circuit open — 停止重启", run.child.Name)
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}
	}
}

// recordFailure —— 失败记入滑窗;返回 true = 熔断已开(应停止重启)。
func (m *Manager) recordFailure(name string, err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.states[name]; st != nil {
		st.LastErr = err.Error()
	}
	now := time.Now()
	win := m.fails[name]
	keep := win[:0]
	for _, t := range win {
		if now.Sub(t) <= m.opts.CircuitWindow {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	m.fails[name] = keep
	if len(keep) >= m.opts.CircuitFails {
		if st := m.states[name]; st != nil {
			st.CircuitOpen = true
		}
		return true
	}
	return false
}

// Stop —— 优雅停单个子进程并结束其监控循环。
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	var run *childRun
	for i, r := range m.runs {
		if r.child.Name == name {
			run = r
			m.runs = append(m.runs[:i], m.runs[i+1:]...)
			break
		}
	}
	st := m.states[name]
	m.mu.Unlock()
	if run == nil {
		return fmt.Errorf("supervise: %s 不在管理中", name)
	}
	run.cancel() // 先断监控循环(本次退出不再触发重启)
	m.killRun(run, run.child.stopGrace())
	if st != nil {
		st.Running = false
		st.PID = 0
	}
	m.log("child stopped", name)
	return nil
}

// Shutdown —— 逆启动序全停(依赖链逆序 = 先停依赖方),等监控循环收尾。
func (m *Manager) Shutdown() {
	m.stop()
	m.mu.Lock()
	runs := m.runs
	m.runs = nil
	m.mu.Unlock()
	for i := len(runs) - 1; i >= 0; i-- {
		m.killRun(runs[i], runs[i].child.stopGrace())
		if st := m.states[runs[i].child.Name]; st != nil {
			st.Running = false
			st.PID = 0
		}
	}
	m.wg.Wait()
}

// Wait —— 阻塞到所有监控循环退出(测试用)。
func (m *Manager) Wait() { m.wg.Wait() }

// States —— 状态快照(先按启动序,已出 runs 的垫后)。
func (m *Manager) States() []State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.states))
	seen := map[string]bool{}
	for _, r := range m.runs {
		if st, ok := m.states[r.child.Name]; ok {
			out = append(out, *st)
			seen[r.child.Name] = true
		}
	}
	for name, st := range m.states {
		if !seen[name] {
			out = append(out, *st)
		}
	}
	return out
}

// killRun —— TERM → 宽限 → KILL(负 pid = 进程组,收孙进程)。
// 不直接 Wait(收尸纪律):等 monitor close lifeDone;宽限内未退则
// 组 KILL —— KILL 不可忽略,monitor 的 Wait 必然随后返回并 close。
// 5s 兜底后放弃等待(理论不可达,防御性)。
func (m *Manager) killRun(run *childRun, grace time.Duration) {
	m.mu.Lock()
	cmd := run.cmd
	lifeDone := run.lifeDone
	name := run.child.Name
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil || lifeDone == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-lifeDone:
		return
	case <-time.After(grace):
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-lifeDone:
	case <-time.After(5 * time.Second):
		m.log("kill escalation unanswered", name, "pid", cmd.Process.Pid)
	}
}

// terminateSolo —— 无 monitor 世代(门超时的候选世代)的独立收尸:
// 调用者是该 cmd 的唯一属主,goroutine 里的 Wait 就是全宇宙唯一一个。
func terminateSolo(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

// KillInstanceOrphans —— 清杀本实例标记(CUMORA_STACK_CHILD=1 +
// CUMORA_STACK_INSTANCE=<id>)的残留进程,返回击杀数。stackd 每次启动
// 第一步调用:自己死过必须认领,否则上一世残留与新世抢端口/双写。
func KillInstanceOrphans(instanceID string) int {
	if instanceID == "" {
		return 0
	}
	killed := 0
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil {
			continue
		}
		var child, inst bool
		for _, kv := range strings.Split(string(data), "\x00") {
			switch kv {
			case EnvChild + "=1":
				child = true
			case EnvInstance + "=" + instanceID:
				inst = true
			}
			if child && inst {
				break
			}
		}
		if child && inst {
			if syscall.Kill(pid, syscall.SIGKILL) == nil {
				killed++
			}
		}
	}
	return killed
}

// lineWriter —— 子进程输出按行回灌 Log(svc= 结构化标签的来源)。
type lineWriter struct {
	m    *Manager
	name string
	buf  []byte
}

func (w lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if line != "" {
			w.m.log("child log", w.name, "line", line)
		}
	}
	return len(p), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func errString(err error) string {
	if err == nil {
		return "clean exit"
	}
	return err.Error()
}

// NextBackoff —— 退避推算纯函数:上一世代存活 ≥ stableAfter → 回地板
// (那是"新问题"不是"老毛病");否则翻倍封顶。
func NextBackoff(cur, startBackoff, maxBackoff, stableAfter, ran time.Duration) time.Duration {
	if ran >= stableAfter {
		return startBackoff
	}
	next := cur * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	return next
}
