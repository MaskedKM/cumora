// worktree 与保留路径的内部测试(#265):真 git 树走 MaterializeWorktree
// 全生命周期(全新切分支 → 幂等 → 目录被清后复挂既有分支 = 失败保分支),
// CI alpine go job 无 git 时跳过(真断言在集成侧,bookworm 有 git)。
package workspaces

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectReserved(t *testing.T) {
	reject := map[string]bool{
		".cumora/versions/x": true,
		".CUMORA/anything":   true,
		".cumora":            true,
		".git/config":        true,
		".GIT/objects/xx":    true,
		".gitignore":         false, // 首段是文件名 .gitignore,不是 .git
		"src/.git/config":    false, // 首段语义:只看根级(与 .cumora 一致)
		"a.txt":              false,
		"cumora/x":           false,
	}
	for rel, want := range reject {
		if got := RejectReserved(rel) != ""; got != want {
			t.Errorf("RejectReserved(%q) rejected=%v, want %v", rel, got, want)
		}
	}
}

func TestWorktreeNames(t *testing.T) {
	if got := WorktreeBranch("card-abc123"); got != "cumora/card-abc123" {
		t.Errorf("branch = %q", got)
	}
	if got := WorktreeRelDir("card-abc123"); got != ".cumora/worktrees/card-abc123" {
		t.Errorf("relDir = %q", got)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestMaterializeWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available (skipped on CI alpine go job; integration covers it on bookworm)")
	}
	ctx := context.Background()
	folder := t.TempDir()

	// 非 git repo:诚实文案,不建任何东西。
	if _, _, _, msg := MaterializeWorktree(ctx, folder, "card-abc"); msg == "" || !strings.Contains(msg, "not a git repository") {
		t.Fatalf("non-repo errMsg = %q", msg)
	}

	gitIn(t, folder, "init")
	gitIn(t, folder, "config", "user.email", "test@cumora.local")
	gitIn(t, folder, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(folder, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, folder, "add", ".")
	gitIn(t, folder, "commit", "-m", "init")

	// 全新:切分支 + 落 worktree。
	branch, relDir, already, msg := MaterializeWorktree(ctx, folder, "card-abc")
	if msg != "" || branch != "cumora/card-abc" || relDir != ".cumora/worktrees/card-abc" || already {
		t.Fatalf("first materialize: branch=%q relDir=%q already=%v msg=%q", branch, relDir, already, msg)
	}
	wt := filepath.Join(folder, filepath.FromSlash(relDir))
	if st, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || st.IsDir() {
		t.Fatalf("worktree .git should be a pointer file, err=%v", err)
	}
	if !strings.Contains(gitOut(t, folder, "branch", "--list", "cumora/card-abc"), "cumora/card-abc") {
		t.Fatal("branch cumora/card-abc not created")
	}

	// 幂等:已物化直接返回 already。
	if _, _, again, msg := MaterializeWorktree(ctx, folder, "card-abc"); msg != "" || !again {
		t.Fatalf("idempotent re-materialize: again=%v msg=%q", again, msg)
	}

	// 失败保分支:worktree 目录被清(operator/agent 侧误删)→ 复挂既有
	// 分支,分支不重建不丢失,提交历史仍在。
	os.RemoveAll(wt)
	gitIn(t, folder, "worktree", "prune")
	if b, _, _, msg := MaterializeWorktree(ctx, folder, "card-abc"); msg != "" || b != "cumora/card-abc" {
		t.Fatalf("re-attach after removal: msg=%q", msg)
	}
	if got := strings.TrimSpace(gitOut(t, folder, "branch", "--list", "cumora/card-abc")); !strings.Contains(got, "cumora/card-abc") {
		t.Fatalf("branch lost after re-attach: %q", got)
	}

	// worktree 是活的 checkout:写文件落在分支工作区。
	if err := os.WriteFile(filepath.Join(wt, "done.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := gitOut(t, wt, "status", "--porcelain"); !strings.Contains(out, "done.txt") {
		t.Fatalf("worktree status missing new file: %q", out)
	}
}
