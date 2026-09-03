// daemon 包 workspace_sync —— #336 团队工作区挂点物化:每个 agent 在
// 同步周期拉 GET /runtime/workspaces(runtime JWT),为 local computer
// 的 agent 在 home/team/<wsId> 建挂点 symlink → 服务器侧文件夹(同
// inode 直写,native 工具链全解锁)。stamp 记 wsId→target:无变化零写
// 盘、变更重建、清单消失回收(只删 stamp 记录的 symlink,不碰 agent
// 自建的普通文件);拉取失败保陈旧(nil 跳过本轮防误回收)。
//
// symlink 而非真 bind mount:unprivileged systemd user service 无
// CAP_SYS_ADMIN,mount(2) 不可用;symlink 达成 ADR 0006 Mountpoint 的
// 定义("the same folder, not a copy or a sync")。
package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

// workspaceMountRef:/runtime/workspaces 清单行(folderPath 仅 local
// computer 返回;vps 零值 → 不建挂点,agent 落 CLI 形态)。
type workspaceMountRef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IsDefault  bool   `json:"isDefault"`
	FolderPath string `json:"folderPath"`
}

// teamMountsStamp:物化状态名(点前缀,平台元数据;agent 经 ls -a 可见
// 亦可改,被改坏时按空 stamp 处理、按清单重物化自愈)。
const teamMountsStamp = ".cumora-team-mounts.json"

// teamMountsDir:home 下挂点父目录。#265 worktree 留位:母仓即本
// symlink 指向的 workspace repo,任务级 worktree 将来另行挂载(不占
// 此目录的 <wsId> 名字空间)。
func teamMountsDir(home string) string { return filepath.Join(home, "team") }

// listAgentWorkspaces:拉 agent 可达工作区清单(失败返回 nil,调用方
// 按"无变化"处理——整轮跳过,不误回收)。
func listAgentWorkspaces(ctx context.Context, cfg *DaemonConfig, token string) []workspaceMountRef {
	var refs []workspaceMountRef
	if err := apiCall(ctx, cfg.ServerURL, http.MethodGet, "/runtime/workspaces", token, nil, &refs); err != nil {
		return nil
	}
	return refs
}

// syncTeamMounts:一个 runner 的挂点同步(sync 周期逐 runner 调用)。
// 失败只记日志——挂点缺失时 persona 的双态文案自然引导 CLI 回退,
// 不许影响唤醒主路径。
func syncTeamMounts(ctx context.Context, cfg *DaemonConfig, r *AgentRunner) {
	token, err := r.ensureToken()
	if err != nil {
		slog.Warn("[computer] team workspace mounts skipped — no runtime token", "agent", r.agent.ID, "err", err)
		return
	}
	refs := listAgentWorkspaces(ctx, cfg, token)
	if refs == nil {
		return
	}
	if err := materializeTeamMounts(teamMountsDir(r.home), refs); err != nil {
		slog.Warn("[computer] team workspace mounts sync failed", "agent", r.agent.ID, "err", err)
	}
}

// materializeTeamMounts:目录级物化(与 runner 解耦,便于单测直驱)。
//   - FolderPath 空(vps)→ 不建,自然 CLI 回退;stamp 既有项回收。
//   - 目标瞬时不可达(盘 flake/维护/ESTALE)→ 保陈旧挂点不回收
//     (skills_sync"陈旧保命"同款);回收只由清单真消失触发。
//   - 挂点位置已有非 symlink 对象(agent 自建文件)→ 不动不记,不与
//     agent 抢名字空间。
//   - 清单消失(成员移除/解绑/转 vps)→ 只回收 stamp 记录且 target 未被
//     改动的 symlink;被手改过的视为"不再归我们",保守留下。
func materializeTeamMounts(dir string, refs []workspaceMountRef) error {
	stamps, err := readTeamMountStamps(dir)
	if err != nil {
		return err
	}
	next := map[string]string{}
	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}
		link := filepath.Join(dir, ref.ID)
		if ref.FolderPath == "" {
			continue // vps 清单行:本就不该有挂点,既有项由回收分支处理
		}
		if st, serr := os.Stat(ref.FolderPath); serr != nil || !st.IsDir() {
			if cur, lerr := os.Readlink(link); lerr == nil && cur == stamps[ref.ID] {
				next[ref.ID] = stamps[ref.ID] // 陈旧保命:不因 stat 抖动误删在用挂点
			}
			continue
		}
		if cur, lerr := os.Readlink(link); lerr == nil && cur == ref.FolderPath {
			next[ref.ID] = ref.FolderPath // 已正确:零写盘
			continue
		}
		if fi, fierr := os.Lstat(link); fierr == nil && fi.Mode()&os.ModeSymlink == 0 {
			continue // agent 自建同名对象:不抢
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		_ = os.Remove(link)
		if err := os.Symlink(ref.FolderPath, link); err != nil {
			continue // 单点失败不阻断其余挂点
		}
		next[ref.ID] = ref.FolderPath
	}
	for id, target := range stamps {
		if _, ok := next[id]; !ok {
			if cur, lerr := os.Readlink(filepath.Join(dir, id)); lerr == nil && cur == target {
				_ = os.Remove(filepath.Join(dir, id))
			}
		}
	}
	if stampsEqual(stamps, next) {
		return nil
	}
	if len(next) == 0 {
		// 无挂点:stamp 清掉(team/ 目录保留——agent 可能自建了东西;
		// 空目录对 persona 双态文案同样可判)。
		return os.Remove(filepath.Join(dir, teamMountsStamp))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeTeamMountStamps(dir, next)
}

func readTeamMountStamps(dir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, teamMountsStamp))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var stamps map[string]string
	if err := json.Unmarshal(b, &stamps); err != nil {
		// 坏 stamp:当空处理,按清单重物化自愈。
		return map[string]string{}, nil
	}
	return stamps, nil
}

func writeTeamMountStamps(dir string, stamps map[string]string) error {
	// encoding/json 对 map 键按字典序输出——落盘字节天然稳定。
	b, err := json.MarshalIndent(stamps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, teamMountsStamp), b, 0o644)
}
