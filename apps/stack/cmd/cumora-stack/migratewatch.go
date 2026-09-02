// migratewatch —— migrate-pg 可观测性加固(#315,#287 余量)。
//
// 背景:2026-09-02 生产首跑无输出停死(SIGQUIT 栈被截断未定案,二跑
// 干净)。问题本质是观测面缺失:停死后无法从日志定位卡在哪一步。本文件
// 三件套让"永远停死"变成"可定位的失败":
//
//	stepTracker —— 每步 begin/done 配对行,带墙钟与耗时;
//	runWatched  —— 子进程透明:PID + 脱敏 argv + 30s 心跳 + 按工具预算
//	               (预算是停死定位墙,不是容量假设 —— 外层 30min 窗口
//	               才是总界);
//	withBudget  —— 非子进程步骤(停链等)的同款心跳与预算。
//
// 预算耗尽 = 杀子/放弃等待并报错退出,窗口脚本 defer 照常起链恢复原状。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// migrateHeartbeat —— 长步骤心跳间隔(包级变量 = 测试注入口)。
var migrateHeartbeat = 30 * time.Second

// runBudgets —— 按工具的预算墙(文件基名 → 预算)。缺省 5min。
// pg_dump 20min / pg_restore 25min 在外层 30min 窗口内留出收尾余量;
// 真超大库超预算 = 大声失败,由操作者扩窗口重跑,优于静默挂死。
var runBudgets = map[string]time.Duration{
	"pg_dump":    20 * time.Minute,
	"pg_restore": 25 * time.Minute,
	"pg_ctl":     5 * time.Minute,
}

func budgetFor(path string) time.Duration {
	if d, ok := runBudgets[filepath.Base(path)]; ok {
		return d
	}
	return 5 * time.Minute
}

func migStamp() string { return time.Now().Format("15:04:05") }

// stepTracker —— 编号步骤面:[k/N] name begin → done(耗时)。
type stepTracker struct {
	total int
	n     int
}

func newStepTracker(total int) *stepTracker { return &stepTracker{total: total} }

// begin 打印起始行并返回收尾闭包(耗时落行;闭包幂等,防 defer 双调)。
func (s *stepTracker) begin(name string) func() {
	s.n++
	k, started := s.n, time.Now()
	fmt.Printf("%s migrate-pg: [%d/%d] %s …\n", migStamp(), k, s.total, name)
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		fmt.Printf("%s migrate-pg: [%d/%d] %s 完成(%s)\n", migStamp(), k, s.total, name, time.Since(started).Round(time.Millisecond))
	}
}

// runWatched —— RunCmd 的生产实现(#315):起子进程即打 PID + 脱敏
// argv 摘要;等待期每 migrateHeartbeat 打一行心跳;退出打 exit code +
// 耗时。预算到点 CommandContext 杀子,错误注明预算语义。
func runWatched(ctx context.Context, path string, args ...string) (string, error) {
	budget := budgetFor(path)
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, args...)
	// 孤儿抱管道保险带(stackd #313 同族实测:KILL 杀的是工具本体,
	// 抱着管道的孙进程会把 Wait 挂死)。
	cmd.WaitDelay = 10 * time.Second
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	fmt.Printf("%s migrate-pg: 子进程 %s pid=%d argv=%s 预算=%s\n",
		migStamp(), filepath.Base(path), cmd.Process.Pid, summarizeArgs(args), budget)
	started := time.Now()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(migrateHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Printf("%s migrate-pg: …仍在等 %s(pid %d),已 %s\n",
					migStamp(), filepath.Base(path), cmd.Process.Pid, time.Since(started).Round(time.Second))
			}
		}
	}()
	waitErr := cmd.Wait()
	exitNote := "ok"
	if waitErr != nil {
		exitNote = waitErr.Error()
	}
	fmt.Printf("%s migrate-pg: 子进程 %s pid=%d 退出(%s;耗时 %s)\n",
		migStamp(), filepath.Base(path), cmd.Process.Pid, exitNote, time.Since(started).Round(time.Millisecond))
	if cctx.Err() != nil {
		return buf.String(), fmt.Errorf("%s 预算耗尽/取消(%v;已杀,窗口 defer 会起链恢复): %w", filepath.Base(path), cctx.Err(), waitErr)
	}
	return buf.String(), waitErr
}

// withBudget —— 非子进程步骤(停链等纯函数调用)的心跳 + 预算。
// 超时后 fn 的 goroutine 无法强杀(CLI 进程随后退出,泄漏可忍);
// 心跳行让"systemd 停链停死"这类无子进程停区同样可定位。
func withBudget(name string, budget time.Duration, fn func() error) error {
	fmt.Printf("%s migrate-pg: [%s] begin(预算=%s)\n", migStamp(), name, budget)
	started := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- fn() }()
	t := time.NewTicker(migrateHeartbeat)
	defer t.Stop()
	// deadline 必须在循环外建好:循环内 time.After 每轮新建计时器,
	// 心跳比预算密时永不触发(实测踩实)。
	deadline := time.After(budget)
	for {
		select {
		case err := <-errc:
			fmt.Printf("%s migrate-pg: [%s] 完成(耗时 %s)\n", migStamp(), name, time.Since(started).Round(time.Millisecond))
			return err
		case <-t.C:
			fmt.Printf("%s migrate-pg: …仍在等 [%s],已 %s\n", migStamp(), name, time.Since(started).Round(time.Second))
		case <-deadline:
			return fmt.Errorf("%s 超过预算 %s(疑似停死;取证:kill -QUIT <migrate-pg pid> 看 goroutine 栈)", name, budget)
		}
	}
}

// summarizeArgs —— argv 摘要:DSN 类参数脱敏(凭据不进日志),整体
// 截断防长串刷屏。
func summarizeArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if strings.Contains(a, "://") || strings.Contains(a, "password=") {
			a = redactDSN(a)
		}
		parts = append(parts, a)
	}
	s := strings.Join(parts, " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return `"` + s + `"`
}

// procAlive —— 信号 0 探活(EPERM = 进程在但非我们所有,同样算活)。
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// readLockPID —— 锁文件里的 pid(写坏 = 0)。
func readLockPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}
