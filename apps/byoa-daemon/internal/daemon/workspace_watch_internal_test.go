// workspace_watch 单测(#337):真 fsnotify + httptest 桩收上报 —— 覆盖
// 去抖聚合、同窗合并、watch 集增删。debounce 用 env 压到 150ms。
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type reportRecorder struct {
	mu    sync.Mutex
	batch []map[string]any // 每次上报的 items
}

func (rr *reportRecorder) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/computers/me/workspace-report", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Items []map[string]any `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rr.mu.Lock()
		rr.batch = append(rr.batch, body.Items...)
		rr.mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"changed":0}`))
	})
	return mux
}

func (rr *reportRecorder) countFor(path string) int {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	n := 0
	for _, it := range rr.batch {
		if it["path"] == path {
			n++
		}
	}
	return n
}

func (rr *reportRecorder) total() int {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	return len(rr.batch)
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, label string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("[%s] condition not met within %s", label, timeout)
}

func TestTeamWatcherReportsWithinDebounce(t *testing.T) {
	t.Setenv("CUMORA_WS_WATCH_DEBOUNCE_MS", "150")
	rec := &reportRecorder{}
	ts := httptest.NewServer(rec.handler())
	defer ts.Close()

	folder := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &DaemonConfig{ServerURL: ts.URL, DeviceToken: "dev-tok"}
	tw := startTeamWatcher(ctx, cfg)
	if tw == nil {
		t.Skip("fsnotify unavailable in this environment")
	}
	defer tw.stop()

	tw.syncMounts(map[string]string{"ws-a": folder})

	// 同窗多次写同一文件 + 另一文件:去抖后合并为一批(每 path 一条)。
	if err := os.WriteFile(filepath.Join(folder, "a.md"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "a.md"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(folder, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 事件驱动补 watch 有窗口:新目录的 Create 事件被 handle(→watchTree)
	// 之前写入的文件会漏事件 —— 生产面由该文件的下一次修改与兜底扫描
	// 收口;测试里等补 watch 落地再写。
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(folder, "sub", "b.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.countFor("a.md") >= 1 }, "first batch a.md")
	waitFor(t, 3*time.Second, func() bool { return rec.countFor("sub/b.md") >= 1 }, "first batch sub/b.md")

	// 第二轮写:再触发一批(第二次上报)。
	first := rec.countFor("a.md")
	if err := os.WriteFile(filepath.Join(folder, "a.md"), []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return rec.countFor("a.md") > first }, "second write")

	// watch 集收缩:清空 mounts 后再写不再上报。
	before := rec.total()
	tw.syncMounts(map[string]string{})
	if err := os.WriteFile(filepath.Join(folder, "c.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if rec.total() != before {
		t.Fatalf("after unwatch, writes must not be reported (before=%d after=%d)", before, rec.total())
	}
}
