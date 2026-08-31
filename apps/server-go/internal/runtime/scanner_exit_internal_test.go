// scanner 生命周期单测(#251):StartScanner 的接线面——SetBaseContext
// 注入的 ctx 对循环生效(注入已死 ctx 则零 tick;运行中 cancel 后 tick
// 计数冻结)。tick 触 nil DB 即 panic、经共享循环(sched.RunWorkerLoop)
// recover 落一条 "tick panicked"——以日志计数作 tick 观测面;循环本体的
// 停机/panic 隔离语义在 sched 包钉住,此处只证 runtime 侧接线。
package runtime

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func scannerWaitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

// withBaseCtx:测试期替换包级 ctxBG(SetBaseContext 的等价注入),返回
// 还原函数,避免污染同包其它用例。
func withBaseCtx(ctx context.Context) func() {
	old := ctxBG
	ctxBG = ctx
	return func() { ctxBG = old }
}

func tickPanics(t *testing.T) (count func() int64, restore func()) {
	t.Helper()
	old := slog.Default()
	h := &tickPanicsHandler{}
	slog.SetDefault(slog.New(h))
	return h.n.Load, func() { slog.SetDefault(old) }
}

type tickPanicsHandler struct{ n atomic.Int64 }

func (h *tickPanicsHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "tick panicked") {
		h.n.Add(1)
	}
	return nil
}
func (h *tickPanicsHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *tickPanicsHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *tickPanicsHandler) WithGroup(string) slog.Handler            { return h }

// assertFreezesAfterCancel:cancel 后先给一段宽限让在途 tick 落地(cancel
// 与 select 已决出的最后一次 tick 竞争是合法的),再静置比较——计数必须
// 冻结。
func assertFreezesAfterCancel(t *testing.T, count func() int64) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	settled := count()
	time.Sleep(200 * time.Millisecond)
	if after := count(); after != settled {
		t.Fatalf("ticks continued after cancel: %d → %d", settled, after)
	}
}

// TestStartScannerNeverTicksUnderDeadBaseCtx:注入已 cancel 的 ctx 后启动
// 扫描器——循环立即退出,零 tick。
func TestStartScannerNeverTicksUnderDeadBaseCtx(t *testing.T) {
	count, restoreLogs := tickPanics(t)
	defer restoreLogs()
	t.Setenv("ENABLE_SCANNER", "true")
	t.Setenv("SCANNER_INTERVAL_MS", "1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restoreCtx := withBaseCtx(ctx)
	defer restoreCtx()

	New(nil, nil).StartScanner()
	time.Sleep(150 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("dead base ctx must yield zero ticks, got %d", n)
	}
}

// TestStartScannerStopsAfterBaseCtxCancel:运行中 cancel 包级 ctx →
// tick 计数冻结(#215:原 ticker.Stop 被丢弃、停机对循环无效的回归面)。
func TestStartScannerStopsAfterBaseCtxCancel(t *testing.T) {
	count, restoreLogs := tickPanics(t)
	defer restoreLogs()
	t.Setenv("ENABLE_SCANNER", "true")
	t.Setenv("SCANNER_INTERVAL_MS", "1")
	ctx, cancel := context.WithCancel(context.Background())
	restoreCtx := withBaseCtx(ctx)
	defer func() { restoreCtx(); cancel() }()

	New(nil, nil).StartScanner() // nil DB:每 tick 触库即 panic,即观测信号
	scannerWaitUntil(t, "scanner ticks observed", func() bool { return count() >= 2 })
	cancel()
	assertFreezesAfterCancel(t, count)
}
