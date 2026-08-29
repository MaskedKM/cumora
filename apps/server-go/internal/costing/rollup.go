// costing 包 rollup —— llm_calls_rollup 预聚合刷新(#62;#140 自
// runtime 拆出,纯移动——Service 方法改吃 *sql.DB):llm-rollup.ts 的
// Go 等价。仪表盘聚合原本全表扫 ~47 万行 5–25s;rollup 折叠到小时桶
// (~15×小)。刷新模型 = 整窗重算 + UPSERT(幂等自愈:漏 tick、双跑、
// 迟到行下一轮全部收敛);advisory lock 单写者;启动即首轮回填。
package costing

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// 与 migrate 的 SCHEMA_LOCK_KEY(7_643_178_926_104)区分。
const rollupLockKey int64 = 7_643_178_926_211

// 稳态尾窗:当前不完整小时 + 时钟漂移/迟到写余量。
const rollupSteadyWindowHours = 3

// 首轮回填/最大追窗口(仪表盘最宽 90d,略余量;封顶防新表无界扫描)。
const rollupMaxBackfillHours = 95 * 24

// 保留窗:更旧的桶定期清,rollup 随历史增长有界。
const rollupRetentionHours = 95 * 24

// envIntRaw:符号感知的环境整数(0/-1 原样返回);缺键/非数 → ok=false。
// 与 envIntOr(0→默认)相反——间隔类 env 的 TS 语义是"0=禁用",
// 必须让 0 活着到达调用方的禁用分支。(自 runtime/scheduler.go 平移;
// #141 横切统一票若收敛 env 助手则届时合一。)
func envIntRaw(name string) (int64, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// envIntRaw 语义:LLM_ROLLUP_INTERVAL_MS=0 = 禁用(TS 文档化 kill-switch)。
func rollupIntervalMS() int64 {
	if n, ok := envIntRaw("LLM_ROLLUP_INTERVAL_MS"); ok {
		return n
	}
	return 120_000
}

// RefreshLlmRollup: 把 created_at 新于 sinceHours 的小时桶整块重算并
// UPSERT;返回写入(插入或更新)的桶数。
func RefreshLlmRollup(ctx context.Context, db *sql.DB, sinceHours int) (int64, error) {
	if sinceHours < 1 {
		sinceHours = 1
	}
	res, err := db.ExecContext(ctx, `
		INSERT INTO llm_calls_rollup (
		   bucket_hour, company_id, agent_id, purpose, model, source, daemon_version,
		   calls, ok_calls, failed_calls, rate_limited_calls,
		   input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens, reasoning_tokens,
		   cost_usd, cost_estimated)
		 SELECT date_trunc('hour', created_at), company_id, agent_id, purpose, model, source, daemon_version,
		        COUNT(*),
		        COUNT(*) FILTER (WHERE status = 'ok'),
		        COUNT(*) FILTER (WHERE status != 'ok'),
		        COUNT(*) FILTER (WHERE status = 'rate_limited'),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(cached_input_tokens), 0),
		        COALESCE(SUM(cache_creation_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(SUM(reasoning_tokens), 0),
		        COALESCE(SUM(cost_usd), 0),
		        BOOL_OR(cost_estimated)
		   FROM llm_calls
		  WHERE created_at >= date_trunc('hour', NOW()) - ($1::int * INTERVAL '1 hour')
		  GROUP BY 1, 2, 3, 4, 5, 6, 7
		 ON CONFLICT (bucket_hour, company_id, agent_id, purpose, model, source, daemon_version)
		 DO UPDATE SET
		   calls = EXCLUDED.calls,
		   ok_calls = EXCLUDED.ok_calls,
		   failed_calls = EXCLUDED.failed_calls,
		   rate_limited_calls = EXCLUDED.rate_limited_calls,
		   input_tokens = EXCLUDED.input_tokens,
		   cached_input_tokens = EXCLUDED.cached_input_tokens,
		   cache_creation_tokens = EXCLUDED.cache_creation_tokens,
		   output_tokens = EXCLUDED.output_tokens,
		   reasoning_tokens = EXCLUDED.reasoning_tokens,
		   cost_usd = EXCLUDED.cost_usd,
		   cost_estimated = EXCLUDED.cost_estimated`, sinceHours)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RunLlmRollupTick: 一轮——取锁、选窗(首跑按最新桶间隙回填)、UPSERT、
// 清老桶。尽力而为,失败下一 tick 重试。锁拿不到 = 他副本在刷新,跳过。
func RunLlmRollupTick(ctx context.Context, db *sql.DB) (buckets int64, sinceHours int, ok bool) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, 0, false
	}
	defer conn.Close()
	var locked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1) AS ok`, rollupLockKey).Scan(&locked); err != nil || !locked {
		return 0, 0, false
	}
	defer func() {
		lockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(lockCtx, `SELECT pg_advisory_unlock($1)`, rollupLockKey)
	}()
	// 窗 = max(稳态, 距最新桶的间隙),封顶回填上限;NULL(空表)→ 全量回填。
	// 扫描失败 = TS 的 throw → 本 tick 失败重试(不得静默全量回填)。
	var gap sql.NullInt64
	if err := conn.QueryRowContext(ctx, `
		SELECT CEIL(EXTRACT(EPOCH FROM (NOW() - MAX(bucket_hour))) / 3600.0)::int AS gap_hours
		  FROM llm_calls_rollup`).Scan(&gap); err != nil {
		slog.Warn("[llm-rollup] gap scan failed — tick aborted", "err", err)
		return 0, 0, false
	}
	sinceHours = rollupMaxBackfillHours
	if gap.Valid {
		gapHours := int(gap.Int64) + 1
		if gapHours < rollupSteadyWindowHours {
			gapHours = rollupSteadyWindowHours
		}
		if gapHours < rollupMaxBackfillHours {
			sinceHours = gapHours
		}
	}
	buckets, err = RefreshLlmRollup(ctx, db, sinceHours)
	if err != nil {
		slog.Warn("[llm-rollup] refresh failed", "err", err)
		return 0, sinceHours, false
	}
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM llm_calls_rollup WHERE bucket_hour < NOW() - ($1::int * INTERVAL '1 hour')`,
		rollupRetentionHours); err != nil {
		slog.Warn("[llm-rollup] retention prune failed", "err", err)
	}
	return buckets, sinceHours, true
}

// ctxBG:fire-and-forget 后台写共用的父上下文。
var ctxBG = context.Background()

var rollupStop chan struct{}

// StartLlmRollupRefresher: 幂等启动;LLM_ROLLUP_INTERVAL_MS=0 关闭。
// 启动即首轮(新部署立刻回填,不等整周期)。
func StartLlmRollupRefresher(db *sql.DB) {
	interval := rollupIntervalMS()
	if interval <= 0 {
		slog.Info("[llm-rollup] disabled (LLM_ROLLUP_INTERVAL_MS=0)")
		return
	}
	if rollupStop != nil {
		return
	}
	slog.Info("[llm-rollup] starting", "interval_ms", interval)
	tick := func() {
		start := time.Now()
		buckets, _, ok := RunLlmRollupTick(ctxBG, db)
		if ok {
			slog.Info("[llm-rollup] refreshed", "buckets", buckets, "ms", time.Since(start).Milliseconds())
		}
	}
	// 首轮即跑(新部署立刻回填);与周期 tick 同样的 panic 隔离。
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("[llm-rollup] first tick panicked", "recover", rec)
			}
		}()
		tick()
	}()
	rollupStop = make(chan struct{})
	go func(stop <-chan struct{}) {
		ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							slog.Error("[llm-rollup] tick panicked", "recover", rec)
						}
					}()
					tick()
				}()
			}
		}
	}(rollupStop)
}
