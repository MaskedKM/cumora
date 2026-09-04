// workspace_sync 物化单测(#336):直驱 materializeTeamMounts,覆盖
// 建/迁移/回收/自愈/保守不抢名四类语义 —— 与 skills_sync_internal_test
// 同款(无 HTTP,不依赖 runner)。
package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func readLink(t *testing.T, p string) string {
	t.Helper()
	target, err := os.Readlink(p)
	if err != nil {
		t.Fatalf("readlink %s: %v", p, err)
	}
	return target
}

func assertLink(t *testing.T, dir, id, want string) {
	t.Helper()
	target := readLink(t, filepath.Join(dir, id))
	if target != want {
		t.Fatalf("link %s → %s, want %s", id, target, want)
	}
}

func assertNoLink(t *testing.T, dir, id string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, id)); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, err=%v", id, err)
	}
}

func TestMaterializeTeamMountsLifecycle(t *testing.T) {
	realA, realB, realC := t.TempDir(), t.TempDir(), t.TempDir()
	dir := filepath.Join(t.TempDir(), "team")

	// 初次物化:两个挂点,stamp 两键。
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", IsDefault: true, FolderPath: realA},
		{ID: "ws-b", Name: "B", FolderPath: realB},
	}); err != nil {
		t.Fatal(err)
	}
	assertLink(t, dir, "ws-a", realA)
	assertLink(t, dir, "ws-b", realB)
	stamps, err := readTeamMountStamps(dir)
	if err != nil || len(stamps) != 2 {
		t.Fatalf("stamps after first pass: %v %d (err %v)", stamps, len(stamps), err)
	}

	// ws-a 服务器迁移(同 id 换 folder)→ 重建指向新 folder;ws-b 成员
	// 移除 → 回收;ws-c 新入区 → 新建。
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: realC},
		{ID: "ws-c", Name: "C", FolderPath: realC},
	}); err != nil {
		t.Fatal(err)
	}
	assertLink(t, dir, "ws-a", realC)
	assertNoLink(t, dir, "ws-b")
	assertLink(t, dir, "ws-c", realC)

	// symlink 被手删 → 同清单再物化自愈。
	if err := os.Remove(filepath.Join(dir, "ws-c")); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: realC},
		{ID: "ws-c", Name: "C", FolderPath: realC},
	}); err != nil {
		t.Fatal(err)
	}
	assertLink(t, dir, "ws-c", realC)
}

func TestMaterializeTeamMountsVpsAndUnreachable(t *testing.T) {
	realA := t.TempDir()
	dir := filepath.Join(t.TempDir(), "team")

	// FolderPath 空(vps computer 的清单行)→ 不建。
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A"}, // 无 folderPath
	}); err != nil {
		t.Fatal(err)
	}
	assertNoLink(t, dir, "ws-a")

	// FolderPath 不可达(server 迁移后旧路径)→ 不建,自然 CLI 回退。
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: filepath.Join(t.TempDir(), "gone")},
	}); err != nil {
		t.Fatal(err)
	}
	assertNoLink(t, dir, "ws-a")

	// 已有挂点后清单转 vps(全空 folderPath)→ 既有回收 + stamp 清除。
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: realA},
	}); err != nil {
		t.Fatal(err)
	}
	assertLink(t, dir, "ws-a", realA)
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A"},
	}); err != nil {
		t.Fatal(err)
	}
	assertNoLink(t, dir, "ws-a")
	if _, err := os.Lstat(filepath.Join(dir, teamMountsStamp)); !os.IsNotExist(err) {
		t.Fatalf("stamp should be removed when no mounts remain, err=%v", err)
	}
}

func TestMaterializeTeamMountsKeepsStaleOnTransientUnreachable(t *testing.T) {
	// 评审 P1-1:ref 仍在清单但目标瞬时不可达(盘 flake/ESTALE)时,
	// 不得误回收在用挂点 —— 保 stamp 记录的陈旧 target,下轮可达再重建。
	realA := t.TempDir()
	dir := filepath.Join(t.TempDir(), "team")
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: realA},
	}); err != nil {
		t.Fatal(err)
	}
	assertLink(t, dir, "ws-a", realA)

	// 下轮:同 id 换到不可达路径(stat 必失败)→ 旧挂点保留。
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: filepath.Join(t.TempDir(), "unreachable-now")},
	}); err != nil {
		t.Fatal(err)
	}
	assertLink(t, dir, "ws-a", realA)
	stamps, err := readTeamMountStamps(dir)
	if err != nil || stamps["ws-a"] != realA {
		t.Fatalf("stale target must stay recorded, stamps=%v err=%v", stamps, err)
	}

	// 再下轮:目标恢复可达(真迁移到新 folder)→ 重建指向新 folder。
	realB := t.TempDir()
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: realB},
	}); err != nil {
		t.Fatal(err)
	}
	assertLink(t, dir, "ws-a", realB)
}

func TestMaterializeTeamMountsDoesNotStealNames(t *testing.T) {
	realA := t.TempDir()
	dir := filepath.Join(t.TempDir(), "team")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// agent 自建普通文件占了未来挂点名 → 不动不记(不与 agent 抢名字空间)。
	own := filepath.Join(dir, "ws-a")
	if err := os.WriteFile(own, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := materializeTeamMounts(dir, []workspaceMountRef{
		{ID: "ws-a", Name: "A", FolderPath: realA},
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(own)
	if err != nil || string(body) != "mine" {
		t.Fatalf("agent-owned file must be left alone, body=%q err=%v", body, err)
	}
	stamps, err := readTeamMountStamps(dir)
	if err != nil || len(stamps) != 0 {
		t.Fatalf("stolen name must not be recorded, stamps=%v err=%v", stamps, err)
	}

	// 手改过 target 的挂点(不再等于 stamp 记录)→ 回收时保守留下。
	link := filepath.Join(dir, "ws-b")
	if err := os.Symlink(realA, link); err != nil {
		t.Fatal(err)
	}
	if err := writeTeamMountStamps(dir, map[string]string{"ws-b": realA}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.Symlink(other, link); err != nil { // 手改成别的目标
		t.Fatal(err)
	}
	if _, err := materializeTeamMounts(dir, nil); err != nil {
		t.Fatal(err)
	}
	if target := readLink(t, link); target != other {
		t.Fatalf("hand-modified link must be preserved, got %s", target)
	}
}
