// workspaces 域 worktree —— #265 worktree-per-task 的落盘半边(Workspace
// 战役刀 4)。
//
// 母仓 = 挂载的 workspace folder(ADR 0006);任务 worktree 落
// .cumora/worktrees/<cardId>/(平台内部:CLI 读写拒、文件树不展示、
// watcher 不上报 —— server 侧 RejectReserved 与 walk 扫描都排除,这里
// 不再重复),分支 cumora/<cardId> 可追溯。agent 经挂载点
// team/<wsId>/.cumora/worktrees/<cardId>/ 以 native git/build/test 直干。
//
// 失败保分支:平台不提供任何删除路径 —— 分支与 worktree 永久保留,
// 任务失败时的进度就是分支本身;清理由 operator 以 git 语义人工处理
// (git worktree remove / branch -d)。
package workspaces

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// WorktreeBranch:任务分支名(cardId 形如 card-<hex>,git 分支字符集安全)。
func WorktreeBranch(cardID string) string { return "cumora/" + cardID }

// WorktreeRelDir:任务 worktree 的 folder 相对路径(slash 形态,与 CLI 面
// 的 rel 惯例一致)。
func WorktreeRelDir(cardID string) string { return ".cumora/worktrees/" + cardID }

// gitTimeout:单次 git 调用硬帽(worktree add 需写全量 checkout,大仓给足
// 余量;探测类调用远快于此)。
const gitTimeout = 60 * time.Second

func runGit(ctx context.Context, folder string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", append([]string{"-C", folder}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		msg := out.String()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.String(), nil
}

// worktreeReady:worktree 目录的标志是其 .git 是文件(gitdir: 指针)——
// 普通仓库根的 .git 是目录,以此区分"已物化"与"残留半成品"。
func worktreeReady(abs string) bool {
	st, err := os.Lstat(filepath.Join(abs, ".git"))
	return err == nil && !st.IsDir()
}

// MaterializeWorktree:为 cardID 幂等物化任务 worktree。返回 (分支名,
// folder 相对路径 slash 形态, 是否已存在)。errMsg 非空 = 不可物化
// (宿主无 git / folder 非 git repo / git 报错),文案面向 agent。
func MaterializeWorktree(ctx context.Context, folder, cardID string) (branch, relDir string, already bool, errMsg string) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", "", false, "git is not available on the server host — worktree isolation unavailable"
	}
	if _, err := runGit(ctx, folder, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", "", false,
			"the workspace folder is not a git repository — worktree isolation needs one (ask the operator to bind a git repo)"
	}
	branch, relDir = WorktreeBranch(cardID), WorktreeRelDir(cardID)
	abs := filepath.Join(folder, filepath.FromSlash(relDir))
	if worktreeReady(abs) {
		return branch, relDir, true, ""
	}
	// 陈尸注册(目录已删、.git/worktrees 条目还在)会让 add 报 already
	// registered —— prune 一次再走;并发场景下另一调用刚建完则后续 add
	// 失败,末尾复查兜底。
	_, _ = runGit(ctx, folder, "worktree", "prune")
	branchExists := false
	if _, err := runGit(ctx, folder, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		branchExists = true
	}
	var err error
	if branchExists {
		_, err = runGit(ctx, folder, "worktree", "add", "--quiet", abs, branch)
	} else {
		_, err = runGit(ctx, folder, "worktree", "add", "--quiet", "-b", branch, abs)
	}
	if err != nil {
		if worktreeReady(abs) {
			return branch, relDir, true, ""
		}
		return "", "", false, "worktree creation failed: " + err.Error()
	}
	return branch, relDir, false, ""
}
