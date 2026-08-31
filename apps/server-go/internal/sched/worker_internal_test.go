// RunWorkerLoop 生命周期单测(#251):#215 形态的停机语义在无 DB/Redis
// 的纯单测钉住——tick 执行、ctx cancel 后不再执行、tick panic 不打断
// 循环、tick 收到的正是启动时注入的父 ctx。Start* 的接线面(idle)以
// nil-DB tick panic 作 slog 探针各钉一条:注入已死 ctx 则永不 tick;
// 运行中 cancel 后计数冻结(SetBaseContext 注入即生效)。
package sched

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
)

func waitUntil(t *testing.T, what string, cond func() bool) {
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

// TestRunWorkerLoopStopsTickingAfterCancel:cancel 是停机的唯一面——计数
// 先涨,cancel 后静置窗口内不再有任何 tick。
func TestRunWorkerLoopStopsTickingAfterCancel(t *testing.T) {
	var n atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RunWorkerLoop(ctx, 1, "[test]", func(context.Context) { n.Add(1) })
	waitUntil(t, "at least 3 ticks", func() bool { return n.Load() >= 3 })
	cancel()
	assertFreezesAfterCancel(t, n.Load)
}

// TestRunWorkerLoopTickPanicIsolated:tick panic 被循环级 recover 吸收,
// 后续 tick 照常执行(单 tick 故障不拖垮 worker)。
func TestRunWorkerLoopTickPanicIsolated(t *testing.T) {
	restore := discardLogs()
	defer restore()
	var n atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RunWorkerLoop(ctx, 1, "[test]", func(context.Context) {
		n.Add(1)
		panic("boom")
	})
	waitUntil(t, "ticks survive panics", func() bool { return n.Load() >= 3 })
	cancel()
}

// TestRunWorkerLoopRunsTickWithParentCtx:tick 收到的是启动时传入的 ctx
// 本身(SetBaseContext 注入的 ctx 直达 tick,不换壳)。
func TestRunWorkerLoopRunsTickWithParentCtx(t *testing.T) {
	type ctxKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "inst-1"))
	defer cancel()
	got := make(chan string, 1)
	RunWorkerLoop(ctx, 1, "[test]", func(c context.Context) {
		select {
		case got <- c.Value(ctxKey{}).(string):
		default:
		}
	})
	select {
	case v := <-got:
		if v != "inst-1" {
			t.Fatalf("tick ctx value = %q, want inst-1", v)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tick never ran")
	}
}

/* ───────── Start* 接线面(nil-DB 探针) ───────── */

// tickPanics:把默认 slog 换成只计数 "tick panicked" 的 handler(其余
// 日志一并吞掉),返回计数读取与还原函数。nil DB 的 tick 在触库处必
// panic,经循环 recover 落一条 "tick panicked"——以此作为 tick 发生的
// 观测面(无需引入 DB 假件)。
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

// discardLogs:静默吞掉全部日志(panic 隔离测试的噪音面)。
func discardLogs() func() {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return func() { slog.SetDefault(old) }
}

// withBaseCtx:测试期替换包级 ctxBG(SetBaseContext 的等价注入),返回
// 还原函数。生产语义"注入后不再改写"只在 boot 期成立;测试还原以免
// 污染同包其它用例。
func withBaseCtx(ctx context.Context) func() {
	old := ctxBG
	ctxBG = ctx
	return func() { ctxBG = old }
}

// TestStartIdleSchedulerNeverTicksUnderDeadBaseCtx:注入已 cancel 的
// ctx 后启动 idle 调度器——循环立即退出,零 tick。
func TestStartIdleSchedulerNeverTicksUnderDeadBaseCtx(t *testing.T) {
	count, restoreLogs := tickPanics(t)
	defer restoreLogs()
	t.Setenv("ENABLE_IDLE", "true")
	t.Setenv("IDLE_INTERVAL_MS", "1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	restoreCtx := withBaseCtx(ctx)
	defer restoreCtx()

	New(agent.New(nil, nil)).StartIdleScheduler()
	time.Sleep(150 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("dead base ctx must yield zero ticks, got %d", n)
	}
}

// TestStartIdleSchedulerStopsAfterBaseCtxCancel:运行中 cancel 包级 ctx
// → tick 计数冻结(接线面证明 StartIdleScheduler 把 ctxBG 传进了循环)。
func TestStartIdleSchedulerStopsAfterBaseCtxCancel(t *testing.T) {
	count, restoreLogs := tickPanics(t)
	defer restoreLogs()
	t.Setenv("ENABLE_IDLE", "true")
	t.Setenv("IDLE_INTERVAL_MS", "1")
	ctx, cancel := context.WithCancel(context.Background())
	restoreCtx := withBaseCtx(ctx)
	defer func() { restoreCtx(); cancel() }()

	New(agent.New(nil, nil)).StartIdleScheduler() // nil DB:每 tick 触库即 panic,即观测信号
	waitUntil(t, "idle ticks observed", func() bool { return count() >= 2 })
	cancel()
	assertFreezesAfterCancel(t, count)
}
