// console 单测(#286):releases 清单/回滚安全门/原子切链。form-aware
// restart 的 systemd 面不在单测里打桩(容器无 systemd,detectForm 恒 none
// —— 走"无 unit"分支即证明不误伤);真实形态在验收演示覆盖。
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeRelease —— 造一个 release 目录:五件二进制 + VERSION + N 个迁移。
func makeRelease(t *testing.T, releasesDir, ver string, migrations int) string {
	t.Helper()
	dir := filepath.Join(releasesDir, ver)
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{"cumora-server", "cumora-daemon", "cumora-sidecar", "cumora-stack", "cumora-stackd"} {
		if err := os.WriteFile(filepath.Join(dir, bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(ver+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < migrations; i++ {
		name := "000" + string(rune('1'+i)) + "_x.sql"
		if err := os.WriteFile(filepath.Join(dir, "migrations", name), []byte("-- x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func setupConsole(t *testing.T, current string) (releasesDir, currentLink string) {
	t.Helper()
	base := t.TempDir()
	releasesDir = filepath.Join(base, "releases")
	currentLink = filepath.Join(base, "current")
	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if current != "" {
		makeRelease(t, releasesDir, current, 2)
		if err := os.Symlink(filepath.Join(releasesDir, current), currentLink); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func TestListReleasesMarksCurrentAndBlocked(t *testing.T) {
	releasesDir, currentLink := setupConsole(t, "0.4.0")
	makeRelease(t, releasesDir, "0.4.1", 2) // 平级迁移数:可回滚
	makeRelease(t, releasesDir, "0.3.0", 1) // 迁移数回退:必须拦

	entries, err := listReleases(releasesDir, "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	byVer := map[string]ReleaseEntry{}
	for _, e := range entries {
		byVer[e.Version] = e
	}
	if !byVer["0.4.0"].Current || byVer["0.4.1"].Current {
		t.Fatalf("current 标记: %+v", byVer)
	}
	if byVer["0.3.0"].RolloutBlocked == "" {
		t.Fatalf("迁移数回退应拦: %+v", byVer["0.3.0"])
	}
	if byVer["0.4.1"].RolloutBlocked != "" {
		t.Fatalf("平级迁移数不应拦: %+v", byVer["0.4.1"])
	}
	if len(entries) != 3 {
		t.Fatalf("清单数: %d", len(entries))
	}
	_ = currentLink
}

func TestRollbackSwitchesCurrent(t *testing.T) {
	releasesDir, currentLink := setupConsole(t, "0.4.0")
	makeRelease(t, releasesDir, "0.3.0", 2) // 同迁移数:允许

	if code := cmdRollback([]string{
		"--releases-dir", releasesDir, "--current-dir", currentLink,
		"--no-restart", "0.3.0",
	}); code != 0 {
		t.Fatalf("平级回滚应退 0: %d", code)
	}
	link, err := os.Readlink(currentLink)
	if err != nil || filepath.Base(link) != "0.3.0" {
		t.Fatalf("current 应切 0.3.0: %s %v", link, err)
	}
}

func TestRollbackBlockedOnMigrationRegression(t *testing.T) {
	releasesDir, currentLink := setupConsole(t, "0.4.0")
	makeRelease(t, releasesDir, "0.3.0", 1)

	if code := cmdRollback([]string{
		"--releases-dir", releasesDir, "--current-dir", currentLink,
		"--no-restart", "0.3.0",
	}); code != 1 {
		t.Fatalf("迁移回退应拒: %d", code)
	}
	link, _ := os.Readlink(currentLink)
	if filepath.Base(link) != "0.4.0" {
		t.Fatal("拒绝时 current 不得变")
	}
}

func TestRollbackUnknownVersion(t *testing.T) {
	releasesDir, currentLink := setupConsole(t, "0.4.0")
	if code := cmdRollback([]string{
		"--releases-dir", releasesDir, "--current-dir", currentLink,
		"--no-restart", "9.9.9",
	}); code != 1 {
		t.Fatalf("未知版本应拒: %d", code)
	}
}

// 无 systemd 环境(容器):restart 走 none 分支退 1,不误伤。
// 有 systemctl 且本机装着 cumora unit 的机器(维护者自用机)主动跳过
// —— 单测绝不真重启用户的栈(评审 P2)。
func TestRestartNoUnitFormFailsClean(t *testing.T) {
	if path, err := exec.LookPath("systemctl"); err == nil {
		out, err := exec.Command(path, "--user", "show", "cumora.service",
			"cumora-go.service", "cumora-sidecar.service", "-p", "LoadState", "--value").Output()
		if err == nil && strings.Contains(string(out), "loaded") {
			t.Skip("本机装有 cumora unit — 跳过(单测不真重启栈)")
		}
	}
	if code := cmdRestart(nil); code != 1 {
		t.Fatalf("无 unit 形态应退 1: %d", code)
	}
}

// 安全门 fail-closed(评审 P2):当前侧 migrations 不可读 = 基线未知
// 必须拦;current 缺失同样拒。
func TestRollbackFailsClosedOnUnreadableBaseline(t *testing.T) {
	releasesDir, currentLink := setupConsole(t, "0.4.0")
	makeRelease(t, releasesDir, "0.3.0", 2)
	// 当前版本 migrations 位堵成非目录(ENOTDIR:容器内 root 无视权限
	// 位,chmod 000 拦不住;文件形态对 ReadDir 恒错)。
	os.RemoveAll(filepath.Join(releasesDir, "0.4.0", "migrations"))
	if err := os.WriteFile(filepath.Join(releasesDir, "0.4.0", "migrations"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := cmdRollback([]string{
		"--releases-dir", releasesDir, "--current-dir", currentLink,
		"--no-restart", "0.3.0",
	}); code != 1 {
		t.Fatalf("基线不可读应拒: %d", code)
	}
	link, _ := os.Readlink(currentLink)
	if filepath.Base(link) != "0.4.0" {
		t.Fatal("拒绝时 current 不得变")
	}
}

func TestRollbackRefusesMissingCurrent(t *testing.T) {
	releasesDir, currentLink := setupConsole(t, "")
	_ = currentLink
	os.Remove(currentLink)
	makeRelease(t, releasesDir, "0.3.0", 2)
	if code := cmdRollback([]string{
		"--releases-dir", releasesDir, "--current-dir", currentLink,
		"--no-restart", "0.3.0",
	}); code != 1 {
		t.Fatalf("current 缺失应拒: %d", code)
	}
}

// 排序:数字感知(0.10.0 > 0.9.0;v 前缀剥除)。
func TestReleaseOrderingNumeric(t *testing.T) {
	releasesDir, _ := setupConsole(t, "")
	makeRelease(t, releasesDir, "0.9.0", 1)
	makeRelease(t, releasesDir, "0.10.0", 1)
	entries, err := listReleases(releasesDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Version != "0.10.0" || entries[1].Version != "0.9.0" {
		t.Fatalf("数字序: %v %v", entries[0].Version, entries[1].Version)
	}
}
