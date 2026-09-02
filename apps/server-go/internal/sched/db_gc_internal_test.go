// db_gc 纯函数单测(同族审计 P1-2):env → 扫删参数的映射面在无 DB 的
// 纯单测钉住——六键的 0 透传(0=禁用 kill-switch,绝不能被吞成默认,
// #62 教训)、五表清单/列名(SQL 形状的锚,防手滑改坏列名)、启动日志
// 窗口串形状。真库扫删行为由一次性 pg 容器验证(不入库)。
package sched

import (
	"strings"
	"testing"
)

// wantGcTargets:TS targets() 的期望面——顺序(ws_tickets 先、agent_events
// 先于 agent_runs 同窗扫以缩小 FK 级联面)、PK/时间列(删批与保留窗的
// SQL 由它们拼出,错了即扫不动/扫错列)。
var wantGcTargets = []gcTarget{
	{table: "ws_tickets", pkCol: "token_hash", timeCol: "expires_at", days: 1},
	{table: "agent_log", pkCol: "id", timeCol: "created_at", days: 30},
	{table: "agent_events", pkCol: "id", timeCol: "created_at", days: 30},
	{table: "agent_transcript", pkCol: "id", timeCol: "created_at", days: 30},
	{table: "agent_runs", pkCol: "id", timeCol: "started_at", days: 30},
	{table: "llm_calls", pkCol: "id", timeCol: "created_at", days: 90},
}

// TestGcTargetsDefaults:env 全缺省 → 六表清单与保留窗默认值逐一等于
// TS env.ts 默认(1/30/30/30/90)。
func TestGcTargetsDefaults(t *testing.T) {
	got := gcTargets()
	if len(got) != len(wantGcTargets) {
		t.Fatalf("gcTargets() 长度 = %d, want %d", len(got), len(wantGcTargets))
	}
	for i, w := range wantGcTargets {
		if got[i] != w {
			t.Errorf("gcTargets()[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestGcTargetsZeroDisablesPerTable:单表窗口 env=0 必须原样透传(0=
// 该表清扫关闭),不得落回默认;负数同(tick 的 days<=0 跳过分支吃它)。
func TestGcTargetsZeroDisablesPerTable(t *testing.T) {
	for _, tc := range []struct {
		key  string
		val  string
		want int64
	}{
		{"DB_GC_AGENT_LOG_DAYS", "0", 0},
		{"DB_GC_AGENT_EVENTS_DAYS", "0", 0},
		{"DB_GC_AGENT_RUNS_DAYS", "0", 0},
		{"DB_GC_LLM_CALLS_DAYS", "0", 0},
		{"DB_GC_WS_TICKETS_DAYS", "0", 0},
		{"DB_GC_AGENT_LOG_DAYS", "-1", -1},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			for _, g := range gcTargets() {
				if g.table == tableOf(tc.key) && g.days != tc.want {
					t.Fatalf("%s=%s → %s.days = %d, want %d(0 被吞成默认即 kill-switch 失效)",
						tc.key, tc.val, g.table, g.days, tc.want)
				}
			}
		})
	}
}

// tableOf:窗口 env 键 → 表名(测试辅助)。
func tableOf(key string) string {
	return map[string]string{
		"DB_GC_AGENT_LOG_DAYS":    "agent_log",
		"DB_GC_AGENT_EVENTS_DAYS": "agent_events",
		"DB_GC_AGENT_RUNS_DAYS":   "agent_runs",
		"DB_GC_LLM_CALLS_DAYS":    "llm_calls",
		"DB_GC_WS_TICKETS_DAYS":   "ws_tickets",
	}[key]
}

// TestDbGcIntervalAndBatchEnv:间隔与批大小——缺省落 TS 默认(5min/10000),
// 显式 0 原样透传(间隔 0=整只 worker 禁用;批 0 是调用方的事,不由
// 读取层代为归一)。
func TestDbGcIntervalAndBatchEnv(t *testing.T) {
	if got := dbGcIntervalMS(); got != 300_000 {
		t.Fatalf("缺省 dbGcIntervalMS() = %d, want 300000", got)
	}
	if got := dbGcBatch(); got != 10_000 {
		t.Fatalf("缺省 dbGcBatch() = %d, want 10000", got)
	}
	t.Setenv("DB_GC_INTERVAL_MS", "0")
	if got := dbGcIntervalMS(); got != 0 {
		t.Fatalf("DB_GC_INTERVAL_MS=0 → %d, want 0(被吞成默认即禁用失效)", got)
	}
	t.Setenv("DB_GC_INTERVAL_MS", "120000")
	if got := dbGcIntervalMS(); got != 120_000 {
		t.Fatalf("DB_GC_INTERVAL_MS=120000 → %d, want 120000", got)
	}
	t.Setenv("DB_GC_BATCH", "0")
	if got := dbGcBatch(); got != 0 {
		t.Fatalf("DB_GC_BATCH=0 → %d, want 0", got)
	}
	t.Setenv("DB_GC_BATCH", "500")
	if got := dbGcBatch(); got != 500 {
		t.Fatalf("DB_GC_BATCH=500 → %d, want 500", got)
	}
}

// TestGcWindows:启动日志窗口串形状对齐 TS(`table=Nd` 空格连接,含 0d
// 的已关闭表——六键生效态一眼可读)。
func TestGcWindows(t *testing.T) {
	if got, want := gcWindows(wantGcTargets),
		"ws_tickets=1d agent_log=30d agent_events=30d agent_transcript=30d agent_runs=30d llm_calls=90d"; got != want {
		t.Fatalf("gcWindows() = %q, want %q", got, want)
	}
	t.Setenv("DB_GC_AGENT_LOG_DAYS", "0")
	if got, want := gcWindows(gcTargets()),
		"ws_tickets=1d agent_log=0d agent_events=30d agent_transcript=30d agent_runs=30d llm_calls=90d"; got != want {
		t.Fatalf("gcWindows()(agent_log=0) = %q, want %q", got, want)
	}
	if strings.Contains(gcWindows(nil), "=") {
		t.Fatal("gcWindows(nil) 不应产出任何 table=Nd 片段")
	}
}
