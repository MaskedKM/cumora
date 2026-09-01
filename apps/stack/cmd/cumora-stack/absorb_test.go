// absorb 单测(#283 PR-B):落盘/切链全绿 + 同版本护栏 + 旧目录替换 + 坏源拒绝。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makePayload —— 造一个带正确 MANIFEST 的制品载荷目录。
func makePayload(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"cumora-server":      "server-bin",
		"cumora-daemon":      "daemon-bin",
		"cumora-sidecar":     "sidecar-bin",
		"cumora-stack":       "stack-bin",
		"cumora-stackd":      "stackd-bin",
		"redis-server":       "redis-bin",
		"pg/bin/postgres":    "pg-bin",
		"migrations/001.sql": "CREATE TABLE t();",
	}
	manifestFiles := map[string]string{}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		manifestFiles[name] = hex.EncodeToString(sum[:])
	}
	m := map[string]any{
		"version": version,
		"files":   manifestFiles,
		"deps":    map[string]any{"redis": map[string]string{"version": "7.2.16", "sourceSha256": "00"}},
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "MANIFEST"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAbsorbHappyPath(t *testing.T) {
	src := makePayload(t, "v9.9.9")
	share := t.TempDir()
	releases := filepath.Join(share, "releases")
	current := filepath.Join(share, "current")

	target, err := Absorb(src, releases, current)
	if err != nil {
		t.Fatalf("absorb 应成功: %v", err)
	}
	if filepath.Base(target) != "v9.9.9" {
		t.Fatalf("落盘目录名 = %s", target)
	}
	// 文件在位 + 可执行位保留
	fi, err := os.Stat(filepath.Join(target, "cumora-server"))
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("二进制应在且可执行: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(target, "pg/bin/postgres")); err != nil {
		t.Fatal("子目录树应完整")
	}
	// VERSION
	vb, err := os.ReadFile(filepath.Join(target, "VERSION"))
	if err != nil || strings.TrimSpace(string(vb)) != "v9.9.9" {
		t.Fatalf("VERSION 内容: %q %v", vb, err)
	}
	// current 原子切到 releases/v9.9.9
	cur, err := os.Readlink(current)
	if err != nil || cur != filepath.Join("releases", "v9.9.9") {
		t.Fatalf("current 指向: %q %v", cur, err)
	}
	// staging 不残留
	ents, _ := os.ReadDir(releases)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".staging") {
			t.Fatalf("staging 残留: %s", e.Name())
		}
	}
}

func TestAbsorbSameVersionGuard(t *testing.T) {
	src := makePayload(t, "v9.9.9")
	share := t.TempDir()
	releases := filepath.Join(share, "releases")
	current := filepath.Join(share, "current")
	if _, err := Absorb(src, releases, current); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Readlink(current)
	_, err := Absorb(src, releases, current)
	if err == nil || !strings.Contains(err.Error(), "同版本重铺") {
		t.Fatalf("同版本重铺应拒绝: %v", err)
	}
	after, _ := os.Readlink(current)
	if before != after {
		t.Fatal("拒绝路径不得动 current")
	}
}

func TestAbsorbReplacesStaleTarget(t *testing.T) {
	src := makePayload(t, "v9.9.9")
	share := t.TempDir()
	releases := filepath.Join(share, "releases")
	current := filepath.Join(share, "current")
	// 旧 v9.9.9 目录存在但 current 指向别的版本 → 原地替换不套娃
	stale := filepath.Join(releases, "v9.9.9")
	if err := os.MkdirAll(filepath.Join(stale, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/v0.1.0", current); err != nil {
		t.Fatal(err)
	}
	target, err := Absorb(src, releases, current)
	if err != nil {
		t.Fatalf("旧目录替换应成功: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "old")); !os.IsNotExist(err) {
		t.Fatal("旧目录残留应清干净(不套娃)")
	}
	if _, err := os.Stat(filepath.Join(target, "cumora-server")); err != nil {
		t.Fatal("新内容应在位")
	}
}

func TestAbsorbTamperedSourceRefused(t *testing.T) {
	src := makePayload(t, "v1.0.0")
	// 落盘后篡改一个已清单化的文件
	if err := os.WriteFile(filepath.Join(src, "cumora-server"), []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	share := t.TempDir()
	_, err := Absorb(src, filepath.Join(share, "releases"), filepath.Join(share, "current"))
	if err == nil || !strings.Contains(err.Error(), "cumora-server") {
		t.Fatalf("篡改源应拒绝且点名: %v", err)
	}
	// 拒绝时不得创建 current
	if _, lerr := os.Lstat(filepath.Join(share, "current")); !os.IsNotExist(lerr) {
		t.Fatal("失败路径不得切 current")
	}
}

func TestAbsorbMissingRequiredPieceRefused(t *testing.T) {
	// 制品契约门(#302 评审 P2-4):缺任一必需可执行件 = 响亮拒绝,
	// 不留给 install 后的半栈。
	src := makePayload(t, "v1.0.0")
	if err := os.Remove(filepath.Join(src, "cumora-daemon")); err != nil {
		t.Fatal(err)
	}
	share := t.TempDir()
	_, err := Absorb(src, filepath.Join(share, "releases"), filepath.Join(share, "current"))
	if err == nil || !strings.Contains(err.Error(), "cumora-daemon") {
		t.Fatalf("缺契约件应点名拒绝: %v", err)
	}
}

func TestAbsorbCurrentIsDirectoryRefused(t *testing.T) {
	// current 已存在且非 symlink:点名拒绝,不覆盖、不留半成品
	//(#302 评审 P2-2)。
	src := makePayload(t, "v2.0.0")
	share := t.TempDir()
	current := filepath.Join(share, "current")
	if err := os.MkdirAll(current, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Absorb(src, filepath.Join(share, "releases"), current)
	if err == nil || !strings.Contains(err.Error(), "不是 symlink") {
		t.Fatalf("current 为目录应点名拒绝: %v", err)
	}
	if _, serr := os.Lstat(filepath.Join(share, "current.new")); !os.IsNotExist(serr) {
		t.Fatal("失败路径不得留 current.new 残留")
	}
}
