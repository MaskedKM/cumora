// workspace_scan 单测(#337):SyncWorkspaceFileState 的 walk 分支(兜底
// 扫描路径)与快照语义 —— rdb=nil(无缓存降级)+ 未 SetPublisher
// (发布 no-op)即可直驱,无需 DB/Redis。
package sched

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncWorkspaceFileStateWalkSnapshots(t *testing.T) {
	folder := t.TempDir()
	if err := os.MkdirAll(filepath.Join(folder, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "sub", "a.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 全量 walk(paths=nil):变化文件进清单,旧内容留档 versions/。
	changes := SyncWorkspaceFileState(context.Background(), "ws-t", "c-t", folder, nil, nil)
	if len(changes) != 1 || changes[0].Path != "sub/a.md" || changes[0].Removed {
		t.Fatalf("walk changes = %+v", changes)
	}
	vdir := filepath.Join(folder, ".cumora", "versions", "sub", "a.md")
	entries, err := os.ReadDir(vdir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 version snapshot, entries=%v err=%v", entries, err)
	}
	body, _ := os.ReadFile(filepath.Join(vdir, entries[0].Name()))
	if string(body) != "first" {
		t.Fatalf("snapshot must hold pre-change content, got %q", body)
	}

	// .cumora 自身不进感知面(改动 versions 不产生变更条目)。
	changes = SyncWorkspaceFileState(context.Background(), "ws-t", "c-t", folder, nil, nil)
	if len(changes) != 1 || changes[0].Path == ".cumora/versions/sub/a.md" {
		t.Fatalf("reserved dir must be excluded from walk, changes=%+v", changes)
	}
}

func TestSyncWorkspaceFileStateRemoval(t *testing.T) {
	folder := t.TempDir()
	p := filepath.Join(folder, "gone.md")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 指定 paths(watcher 上报形):文件在 → 变化条目。
	changes := SyncWorkspaceFileState(context.Background(), "ws-t", "c-t", folder, []string{"gone.md"}, nil)
	if len(changes) != 1 || changes[0].Removed {
		t.Fatalf("existing file must surface, got %+v", changes)
	}
	// 挂载侧删除后:无 rdb 已知态 → 无 removed 条目(上报噪音过滤;
	// rdb!=nil 的 removed 路径由集成层覆盖)。
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	changes = SyncWorkspaceFileState(context.Background(), "ws-t", "c-t", folder, []string{"gone.md"}, nil)
	if len(changes) != 0 {
		t.Fatalf("unknown missing file must be ignored, got %+v", changes)
	}
}
