// migratewatch 单测(#315):锁持有者死活甄别、runWatched 的 PID/心跳/
// 预算三面、withBudget 超时路径。真进程桩 + stdout 捕获,不碰真 pg。
package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// captureStdout —— 临时接管 os.Stdout 收 runWatched/withBudget 的观测行。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("stdout 捕获超时")
		return ""
	}
}

// deadPID —— 找一个确定已死的 pid(spawn sleep 0 + Wait)。
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestProcAlive(t *testing.T) {
	if !procAlive(os.Getpid()) {
		t.Fatal("自身 pid 应判活")
	}
	if procAlive(deadPID(t)) {
		t.Fatal("已回收 pid 不应判活")
	}
	if procAlive(0) || procAlive(-1) {
		t.Fatal("非法 pid 应判死")
	}
}

func TestLockFileDistinguishesLiveVsStale(t *testing.T) {
	dir := t.TempDir()

	// 活锁:写入自身 pid → 拒绝信息点名"在跑"。
	live := filepath.Join(dir, "live.lock")
	if err := os.WriteFile(live, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := lockFile(live)
	if err == nil || !strings.Contains(err.Error(), "在跑") {
		t.Fatalf("活锁应点名并发在跑,实得: %v", err)
	}

	// 陈旧锁:已死 pid → 指引手工清(区分于并发)。
	stale := filepath.Join(dir, "stale.lock")
	if err := os.WriteFile(stale, []byte(strconv.Itoa(deadPID(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = lockFile(stale)
	if err == nil || !strings.Contains(err.Error(), "残留锁") {
		t.Fatalf("死锁应指引手工清,实得: %v", err)
	}

	// 不可判读锁:仍拒绝(不 fail-open)。
	garbage := filepath.Join(dir, "garbage.lock")
	if err := os.WriteFile(garbage, []byte("not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = lockFile(garbage)
	if err == nil || !strings.Contains(err.Error(), "不可判读") {
		t.Fatalf("坏锁应拒且点名不可判读,实得: %v", err)
	}
}

func TestRunWatchedReportsPidAndExit(t *testing.T) {
	out := captureStdout(t, func() {
		got, err := runWatched(context.Background(), "sh", "-c", "echo hello")
		if err != nil {
			t.Fatalf("快命令应成功: %v", err)
		}
		if !strings.Contains(got, "hello") {
			t.Fatalf("输出应捕获,实得 %q", got)
		}
	})
	for _, want := range []string{"pid=", "退出(ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("观测行缺 %q,实得:\n%s", want, out)
		}
	}
}

func TestRunWatchedBudgetKills(t *testing.T) {
	// sleep 30 超出预算(budgetFor 对无表工具=5min → 用 pg_dump 名入表?
	// 直接测 cctx 路径:预算表外工具 5min 太长,本测走 dump 名+缩短表)。
	old := runBudgets["pg_dump"]
	runBudgets["pg_dump"] = 300 * time.Millisecond
	defer func() { runBudgets["pg_dump"] = old }()

	dir := t.TempDir()
	stub := filepath.Join(dir, "pg_dump")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := runWatched(context.Background(), stub)
	if err == nil || !strings.Contains(err.Error(), "预算耗尽") {
		t.Fatalf("超预算应报预算语义,实得: %v", err)
	}
	if el := time.Since(started); el > 5*time.Second {
		t.Fatalf("杀子应秒回,实耗时 %s", el)
	}
}

func TestRunWatchedHeartbeat(t *testing.T) {
	oldHB := migrateHeartbeat
	migrateHeartbeat = 80 * time.Millisecond
	defer func() { migrateHeartbeat = oldHB }()

	dir := t.TempDir()
	stub := filepath.Join(dir, "pg_dump")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if _, err := runWatched(context.Background(), stub); err != nil {
			t.Fatalf("预算内应成功: %v", err)
		}
	})
	if n := strings.Count(out, "仍在等"); n < 1 {
		t.Fatalf("1s 等待 + 80ms 心跳应至少打 1 行,实得:\n%s", out)
	}
}

func TestWithBudgetTimesOut(t *testing.T) {
	oldHB := migrateHeartbeat
	migrateHeartbeat = 40 * time.Millisecond
	defer func() { migrateHeartbeat = oldHB }()

	err := withBudget("测试步骤", 200*time.Millisecond, func() error {
		time.Sleep(2 * time.Second)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "预算") {
		t.Fatalf("超时应报预算停死,实得: %v", err)
	}
}
