// daemon 包 workspace_watch —— #337 挂载工作区文件变更感知:进程级
// fsnotify watcher 监听各 agent 挂点指向的真实文件夹(同 ws 多 agent
// 同 inode,watch 一处即全知),per-ws ≈2s 去抖聚合上报
// POST /api/computers/me/workspace-report(device token —— 上报者是
// daemon 计算机而非某 agent)。server 对账已知态→快照→广播
// workspace.files_changed;上报失败丢弃本批(watcher 仍在,下次变更
// 再报;60min 兜底扫描终会追上)。inotify watch 上限/Add 失败只 Warn
// —— 该区感知退化为兜底扫描(Syncthing 同款分层)。
package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// teamWatchDebounce:per-ws 去抖窗(批量聚合突发写;对齐 scheduleWake
// 的 timer 形)。env 可覆盖供测试。
func teamWatchDebounce() time.Duration {
	if v := os.Getenv("CUMORA_WS_WATCH_DEBOUNCE_MS"); v != "" {
		if n := parseIntDefault(v, 0); n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 2 * time.Second
}

func parseIntDefault(s string, def int) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

type teamWatcher struct {
	ctx context.Context
	cfg *DaemonConfig

	fw *fsnotify.Watcher

	mu      sync.Mutex
	targets map[string]string // resolved target → wsId(watch 集)
	pending map[string]map[string]struct{}
	timers  map[string]*time.Timer
}

// startTeamWatcher:进程级 watcher(sync 周期外独立 goroutine;fsnotify
// 初始化失败返回 nil —— 感知退化为 server 兜底扫描,不影响主路径)。
func startTeamWatcher(ctx context.Context, cfg *DaemonConfig) *teamWatcher {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("[computer] workspace watcher unavailable — relying on server fallback scan", "err", err)
		return nil
	}
	t := &teamWatcher{
		ctx: ctx, cfg: cfg, fw: fw,
		targets: map[string]string{},
		pending: map[string]map[string]struct{}{},
		timers:  map[string]*time.Timer{},
	}
	go t.loop()
	return t
}

func (t *teamWatcher) stop() {
	if t != nil && t.fw != nil {
		_ = t.fw.Close()
	}
}

func (t *teamWatcher) loop() {
	for {
		select {
		case <-t.ctx.Done():
			return
		case ev, ok := <-t.fw.Events:
			if !ok {
				return
			}
			t.handle(ev)
		case err, ok := <-t.fw.Errors:
			if !ok {
				return
			}
			slog.Warn("[computer] workspace watcher error", "err", err)
		}
	}
}

// syncMounts:与 materializeTeamMounts 的产物对齐 watch 集(增删)。
// 同 ws 多 agent 同 target → 幂等。
func (t *teamWatcher) syncMounts(mounts map[string]string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	want := map[string]string{}
	for wsID, target := range mounts {
		real, err := filepath.EvalSymlinks(target)
		if err != nil {
			continue
		}
		want[real] = wsID
	}
	for target := range want {
		if _, ok := t.targets[target]; ok {
			continue
		}
		if err := t.watchTree(target); err != nil {
			slog.Warn("[computer] workspace watch add failed — falling back to server scan", "target", target, "err", err)
			continue
		}
		t.targets[target] = want[target]
	}
	for target := range t.targets {
		if _, ok := want[target]; !ok {
			_ = t.fw.Remove(target)
			delete(t.targets, target)
		}
	}
}

// watchTree:递归 watch(target + 全部子目录;新建目录由事件路径补)。
func (t *teamWatcher) watchTree(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单点失败跳过
		}
		if d.IsDir() {
			return t.fw.Add(p)
		}
		return nil
	})
}

func (t *teamWatcher) handle(ev fsnotify.Event) {
	if ev.Op&(fsnotify.Chmod) != 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var wsID, rel string
	for target, id := range t.targets {
		if ev.Name == target || strings.HasPrefix(ev.Name, target+string(filepath.Separator)) {
			r, err := filepath.Rel(target, ev.Name)
			if err != nil {
				continue
			}
			wsID, rel = id, filepath.ToSlash(r)
			break
		}
	}
	if wsID == "" || rel == "" || rel == "." {
		return
	}
	// 新建目录 → 补 watch(递归覆盖)。
	if ev.Op&fsnotify.Create != 0 {
		if st, err := os.Stat(ev.Name); err == nil && st.IsDir() {
			_ = t.watchTree(ev.Name)
		}
	}
	if t.pending[wsID] == nil {
		t.pending[wsID] = map[string]struct{}{}
	}
	t.pending[wsID][rel] = struct{}{}
	if tm, ok := t.timers[wsID]; ok {
		tm.Reset(teamWatchDebounce())
		return
	}
	t.timers[wsID] = time.AfterFunc(teamWatchDebounce(), func() { t.flush(wsID) })
}

// flush:去抖窗到 —— 收集该区 pending 清单批量上报(fire-and-forget;
// 失败丢弃本批,感知由兜底扫描追平)。
func (t *teamWatcher) flush(wsID string) {
	t.mu.Lock()
	rels := t.pending[wsID]
	delete(t.pending, wsID)
	delete(t.timers, wsID)
	t.mu.Unlock()
	if len(rels) == 0 {
		return
	}
	type reportItem struct {
		WorkspaceID string `json:"workspaceId"`
		Path        string `json:"path"`
	}
	items := make([]reportItem, 0, len(rels))
	for rel := range rels {
		items = append(items, reportItem{WorkspaceID: wsID, Path: rel})
	}
	if err := apiCall(t.ctx, t.cfg.ServerURL, http.MethodPost,
		"/api/computers/me/workspace-report", t.cfg.DeviceToken,
		map[string]any{"items": items}, nil); err != nil {
		slog.Warn("[computer] workspace report failed — batch dropped (server scan will catch up)",
			"ws", wsID, "items", len(items), "err", err)
	}
}
