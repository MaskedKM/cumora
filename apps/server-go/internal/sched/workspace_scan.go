// sched 包 workspace_scan —— #337 文件面感知的服务端半边:
//
//   - SyncWorkspaceFileState:文件已知态对账核心(Redis hash
//     cumora:wsidx:<wsId> path→"nano:size")。挂载 watcher 上报(指定
//     paths)与 60min 兜底扫描(paths=nil 全量 walk)共用:变化项先快照
//     当前内容进 .cumora/versions/(挂载写快照史——symlink 同 inode 下
//     被覆盖的旧版只有这里留底),再更新已知态并返回变更清单供广播。
//   - StartWorkspaceScanWorker:兜底扫描(watcher 丢事件 / inotify watch
//     上限时接管;Syncthing 同款"watcher 启用仍保留低频全量扫描")。
package sched

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/domains/workspaces"
	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
)

// WSFileChange:workspace.files_changed 帧的清单行(与 ws-events 契约
// 的 changes.items 同形)。
type WSFileChange struct {
	Path       string `json:"path"`
	MtimeNanos int64  `json:"mtimeNanos"`
	Size       int64  `json:"size"`
	Removed    bool   `json:"removed"`
}

// wsIdxKey:已知态 hash(扁平 key 惯例 cumora:<purpose>:<id>)。
func wsIdxKey(wsID string) string { return "cumora:wsidx:" + wsID }

// parseKnown:hash value "nano:size"。
func parseKnown(v string) (int64, int64) {
	nano, size, _ := strings.Cut(v, ":")
	n, _ := strconv.ParseInt(nano, 10, 64)
	s, _ := strconv.ParseInt(size, 10, 64)
	return n, s
}

// SyncWorkspaceFileState:对账一个工作区的文件已知态。paths=nil 走
// 全量 walk(兜底扫描);否则逐项 stat(watcher 上报,文件不存在按
// removed 处理)。变化项:快照当前版 → HSet/HDel → 返回清单。Redis
// 不可用(nil client)时退化为"只快照不判重"(每次上报都留档一版,
// 10 版帽兜底),不因缓存故障丢感知。
func SyncWorkspaceFileState(ctx context.Context, wsID, companyID, folder string, paths []string, rdb redis.UniversalClient) []WSFileChange {
	key := wsIdxKey(wsID)
	if paths == nil {
		paths = walkWorkspaceFiles(folder)
	}
	var changes []WSFileChange
	hset := map[string]interface{}{}
	var hdel []string
	for _, rel := range paths {
		if workspaces.RejectReserved(rel) != "" {
			continue // 平台内部目录不进感知面
		}
		abs := filepath.Join(folder, filepath.FromSlash(rel))
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			// 不存在/是目录:已知态里有 → removed;没有 → 忽略(上报噪音)。
			if rdb != nil {
				if cmd := rdb.HGet(ctx, key, rel); cmd.Err() == nil && cmd.Val() != "" {
					changes = append(changes, WSFileChange{Path: rel, Removed: true})
					hdel = append(hdel, rel)
				}
			}
			continue
		}
		nano, size := st.ModTime().UnixNano(), st.Size()
		if rdb != nil {
			if cmd := rdb.HGet(ctx, key, rel); cmd.Err() == nil {
				if on, osz := parseKnown(cmd.Val()); on == nano && osz == size {
					continue // 无变化:去重(watcher 抖动/重复上报)
				}
			}
		}
		workspaces.SnapshotVersion(folder, rel)
		changes = append(changes, WSFileChange{Path: rel, MtimeNanos: nano, Size: size})
		hset[rel] = strconv.FormatInt(nano, 10) + ":" + strconv.FormatInt(size, 10)
	}
	if rdb != nil {
		if len(hset) > 0 {
			_ = rdb.HSet(ctx, key, hset)
		}
		if len(hdel) > 0 {
			_ = rdb.HDel(ctx, key, hdel...)
		}
	}
	if len(changes) > 0 {
		mapped := make([]map[string]any, 0, len(changes))
		for _, c := range changes {
			mapped = append(mapped, map[string]any{
				"path": c.Path, "mtimeNanos": strconv.FormatInt(c.MtimeNanos, 10),
				"size": c.Size, "removed": c.Removed,
			})
		}
		events.WorkspaceFilesChanged(ctx, events.WorkspaceFilesChangedEvent{
			CompanyID:   companyID,
			WorkspaceID: wsID,
			Changes:     mapped,
		})
	}
	return changes
}

// walkWorkspaceFiles:全量 walk(跳 .cumora/.git 与 >2MB 的目录树成员不
// 限制——walk 本身列名不读内容;快照面在 SnapshotVersion 里自有 2MB 帽)。
func walkWorkspaceFiles(folder string) []string {
	var out []string
	_ = filepath.WalkDir(folder, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != folder && (name == ".cumora" || name == ".git") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(folder, p)
		if rerr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}

// workspaceScanIntervalMS:兜底扫描间隔(默认 60min;0 = 禁用 kill-switch
// 走 EnvIntRaw 透传,同 calendar 家族)。
func workspaceScanIntervalMS() int64 {
	if n, ok := config.EnvIntRaw("WORKSPACE_SCAN_INTERVAL_MS"); ok {
		return n
	}
	return 3_600_000
}

// RunWorkspaceScanTick:一轮兜底扫描——全部 bound 工作区全量对账。
// 导出供测试直驱。
func (s *S) RunWorkspaceScanTick(ctx context.Context) int {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, company_id, folder_path FROM workspaces WHERE unbound_at IS NULL`)
	if err != nil {
		slog.Warn("[ws-scan] query failed", "err", err)
		return 0
	}
	defer rows.Close()
	type wsRow struct{ id, company, folder string }
	var all []wsRow
	for rows.Next() {
		var r wsRow
		if err := rows.Scan(&r.id, &r.company, &r.folder); err == nil {
			all = append(all, r)
		}
	}
	n := 0
	for _, r := range all {
		changed := SyncWorkspaceFileState(ctx, r.id, r.company, r.folder, nil, s.RDB)
		n += len(changed)
	}
	return n
}

// StartWorkspaceScanWorker:60min 兜底扫描(watcher 丢事件/inotify 上限
// 时是唯一感知来源,不可裁剪——ADR 0006)。
func (s *S) StartWorkspaceScanWorker() {
	if config.Getenv("ENABLE_WORKSPACE_SCAN") == "false" {
		return
	}
	interval := workspaceScanIntervalMS()
	if interval <= 0 {
		slog.Warn("[ws-scan] disabled (WORKSPACE_SCAN_INTERVAL_MS<=0)")
		return
	}
	slog.Info("[ws-scan] fallback scan armed", "intervalMS", interval)
	RunWorkerLoop(ctxBG, interval, "[ws-scan]", func(ctx context.Context) {
		if n := s.RunWorkspaceScanTick(ctx); n > 0 {
			slog.Info("[ws-scan] reconciled changes", "changes", n)
		}
	})
}
