// sched 包 db_gc —— DB 行保留 GC(#70 退役时丢失的 startDbGcWorker 移植,
// 同族审计 P1-2):已退役 TS db-gc.ts 的 Go 平价。高量遥测表无限增长
// (agent_log 每次唤醒一条面包屑,Aug-2026 规模 ~80 万行/天;agent_events
// 镜像每次 run 的可观测轨迹;ws_tickets 过期后无人回收),TS 时代已达
// 50GB/64GB、磁盘 +1.3GB/天——单机自托管慢性填满后静默炸。读端只看近窗
// (`cumora log` 每 agent 末 100 条;可观测下钻有界;成本聚合永久保留在
// llm_calls_rollup),旧行纯死重。
//
// 策略:周期清扫,按表保留窗删过期行,小批短锁(每批独立事务 +
// statement_timeout,病态扫描拖不垮 worker,超时批下轮重来)。victim
// SELECT 走裸时间列索引(idx_*_created/idx_ws_tickets_expires,baseline
// 迁移已带),DELETE 走 PK 的 = ANY——TS 早期 `DELETE WHERE ctid IN
// (子查询)` 单语句形态把外层规划成全堆 seq scan,31GB 表上每轮 55s
// 超时清零行,故 TS 改两段式,此处平价沿用。删 agent_runs 经 FK ON
// DELETE CASCADE 级联其残余 agent_events(agent_events 同窗先扫,级联
// 只处理零星掉队者)。多副本安全:删除幂等,两副本抢同批 = 一方删 0 行。
// 空间只回 free-space map 不回 OS——磁盘不再涨,但不会缩。
package sched

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
)

/* ───────────────────────── 扫描目标(env → 表清单) ───────────────────────── */

// gcTarget:一张待清扫表——PK 列(定址删除批)、时间列(保留窗作用面)、
// 保留天数(0=该表清扫关闭)。列名全是源内建表常量,非用户输入。
type gcTarget struct {
	table   string
	pkCol   string
	timeCol string
	days    int64
}

// gcEnvDay:保留窗 env 读取。EnvIntRaw 家族——0 必须原样透传到
// `days <= 0 → 跳过` 分支(envIntOr 的 0→默认会吞掉单表 kill-switch,
// #62 教训);缺键/非数才落默认。
func gcEnvDay(key string, def int64) int64 {
	if n, ok := config.EnvIntRaw(key); ok {
		return n
	}
	return def
}

// gcTargets:TS targets() 平价。顺序有意义:agent_events 先于 agent_runs
// 同窗扫掉,级联只剩零星掉队者;ws_tickets 按 expires_at(过期即垃圾,
// 多留 1 天纯诊断余量)。
func gcTargets() []gcTarget {
	return []gcTarget{
		{table: "ws_tickets", pkCol: "token_hash", timeCol: "expires_at", days: gcEnvDay("DB_GC_WS_TICKETS_DAYS", 1)},
		{table: "agent_log", pkCol: "id", timeCol: "created_at", days: gcEnvDay("DB_GC_AGENT_LOG_DAYS", 30)},
		{table: "agent_events", pkCol: "id", timeCol: "created_at", days: gcEnvDay("DB_GC_AGENT_EVENTS_DAYS", 30)},
		{table: "agent_runs", pkCol: "id", timeCol: "started_at", days: gcEnvDay("DB_GC_AGENT_RUNS_DAYS", 30)},
		{table: "llm_calls", pkCol: "id", timeCol: "created_at", days: gcEnvDay("DB_GC_LLM_CALLS_DAYS", 90)},
	}
}

// dbGcIntervalMS / dbGcBatch:间隔与批大小 env(EnvIntRaw,0 透传——
// 间隔的 0=整个 worker 禁用)。默认对齐 TS:5min / 10000 行。
func dbGcIntervalMS() int64 {
	if n, ok := config.EnvIntRaw("DB_GC_INTERVAL_MS"); ok {
		return n
	}
	return 5 * 60_000
}

func dbGcBatch() int64 {
	if n, ok := config.EnvIntRaw("DB_GC_BATCH"); ok {
		return n
	}
	return 10_000
}

// gcWindows:启动日志的窗口串(TS 形状 `table=Nd` 空格连接,含 0d 的
// 已关闭表——运维一眼看清六键生效态)。
func gcWindows(ts []gcTarget) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = fmt.Sprintf("%s=%dd", t.table, t.days)
	}
	return strings.Join(parts, " ")
}

/* ───────────────────────── 单批扫删(两段式,索引驱动) ───────────────────────── */

// dbGcStmtTimeout:每批的语句超时(TS 平价 55s)。SET LOCAL 随事务
// COMMIT/ROLLBACK 自动还原,池化连接不带残留的低超时。
const dbGcStmtTimeout = "55s"

// gcDeleteBatch:TS deleteBatch 平价——单事务内先 SELECT victim(裸时间
// 列索引,ORDER BY 时间 ASC LIMIT 批大小),再 DELETE … PK = ANY(数组)
// (PK 索引)。返回 picked(victim 选中数)与 deleted(实际删除数;多
// 副本抢同批时 picked>deleted 合法)。
func (s *S) gcDeleteBatch(ctx context.Context, t gcTarget, batchSize int64) (picked, deleted int64, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() // COMMIT 后是 no-op
	if _, err = tx.ExecContext(ctx, `SET LOCAL statement_timeout = '`+dbGcStmtTimeout+`'`); err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s AS pk FROM %s
		  WHERE %s < NOW() - ($1::int * INTERVAL '1 day')
		  ORDER BY %s ASC
		  LIMIT $2`, t.pkCol, t.table, t.timeCol, t.timeCol),
		int(t.days), int(batchSize))
	if err != nil {
		return 0, 0, err
	}
	var pks []string
	for rows.Next() {
		var pk string
		if err = rows.Scan(&pk); err != nil {
			rows.Close()
			return 0, 0, err
		}
		pks = append(pks, pk)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	if len(pks) > 0 {
		res, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE %s = ANY($1)`, t.table, t.pkCol), pks)
		if err != nil {
			return 0, 0, err
		}
		if n, serr := res.RowsAffected(); serr == nil {
			deleted = n
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return int64(len(pks)), deleted, nil
}

/* ───────────────────────── tick 与调度 ───────────────────────── */

// dbGcMaxBatchesPerTable:每表每 tick 的批数上限——大积压跨多轮 tick
// 渐进烧掉,不搞马拉松(TS 平价 10)。
const dbGcMaxBatchesPerTable = 10

// RunDbGcTick:一轮清扫(导出供测试/手动触发),批大小随 DB_GC_BATCH。
// 返回有删除的表 → 行数(0 删除的表不入 map,TS 平价)。逐表隔离:单表
// 失败记日志后继续下一表。
func (s *S) RunDbGcTick(ctx context.Context) map[string]int64 {
	return s.runDbGcTick(ctx, dbGcBatch())
}

// runDbGcTick:可注入批大小的实现面(验证用)。
func (s *S) runDbGcTick(ctx context.Context, batchSize int64) map[string]int64 {
	deleted := map[string]int64{}
	for _, t := range gcTargets() {
		if t.days <= 0 {
			continue // 该表清扫关闭(0=禁用透传至此)
		}
		var total int64
		for i := 0; i < dbGcMaxBatchesPerTable; i++ {
			picked, n, err := s.gcDeleteBatch(ctx, t, batchSize)
			if err != nil {
				// TS:console.error + inc('db.gc.failed') → Go 无指标族,日志即等价面。
				slog.Warn("[db-gc] "+t.table+" sweep failed", "err", err)
				break
			}
			total += n
			// 短 PICK(而非短 DELETE)= 该表积压已清:多副本抢同批最旧行时,
			// 一批可 pick 1 万活行却只删几行(对端先到),积压仍大。
			if picked < batchSize {
				break
			}
		}
		if total > 0 {
			deleted[t.table] = total
		}
	}
	if len(deleted) > 0 {
		slog.Info("db.gc.tick", "deleted", deleted) // TS: {"evt":"db.gc.tick","deleted":{…}}
	}
	return deleted
}

// StartDbGcWorker:周期清扫 loop。DB_GC_INTERVAL_MS<=0 关闭整个 worker
// (TS 平价 kill-switch;TS 无 ENABLE_DB_GC 键,故不设字面门控)。
// #215 形态:ctx 驱动 + tick 级 panic 隔离,cancelBoot 即停。
func (s *S) StartDbGcWorker() {
	interval := dbGcIntervalMS()
	if interval <= 0 {
		slog.Info("[db-gc] disabled (DB_GC_INTERVAL_MS=0)")
		return
	}
	RunWorkerLoop(ctxBG, interval, "[db-gc]", func(ctx context.Context) { s.RunDbGcTick(ctx) })
	slog.Info("[db-gc] starting", "interval_ms", interval, "batch", dbGcBatch(), "windows", gcWindows(gcTargets()))
}
