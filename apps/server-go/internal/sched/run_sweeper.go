// sched 包 run_sweeper —— #324(#262 里程碑二):服务端失败未决扫描
// 兜底重排。与 daemon 本地重试(#276)互补:本地 attempt 耗尽或 daemon
// 重启/宕机后,失败的 turn 不再只能等周期性 pollLoop/闲时唤醒——本
// sweeper 分钟级定向重排。
//
// 不双跑三闸:①agent busy(thinking/working)不重排(下一 tick 再看);
// ②已被后续成功 run 覆盖的不动;③重排幂等(agent_events kind=
// 'run_requeue' 以 run_id 去重)。分类白名单与 failclass(#276)对齐:
// network / engine-crash / engine-timeout / context-overflow。
package sched

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// sweeperRetryableClasses:#276 classifyTurnFailure 的 retryable 白名单
// 镜像(服务端不引入 daemon 包,分类从 summary 标反解)。
var sweeperRetryableClasses = map[string]bool{
	"network": true, "engine-crash": true, "engine-timeout": true, "context-overflow": true,
}

// sweeperWindowMS:扫描窗——只看近窗失败(陈年失败交给自然唤醒)。
const sweeperWindowMS = 15 * 60_000

// sweeperClassOf:从失败 summary 的 [turn-fail class=X attempts=N] 标
// 反解分类;无标(旧版/非 daemon 路径)按不可重试处理(保守)。
func sweeperClassOf(summary string) string {
	i := strings.Index(summary, "[turn-fail class=")
	if i < 0 {
		return ""
	}
	rest := summary[i+len("[turn-fail class="):]
	if j := strings.IndexByte(rest, ' '); j > 0 {
		return rest[:j]
	}
	return rest
}

// RunRunSweeperTick:一轮扫描重排。返回本轮重排数。
func (s *S) RunRunSweeperTick(ctx context.Context) int {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT r.id, r.agent_id, r.company_id, r.summary
		  FROM agent_runs r
		 WHERE r.status = 'failed'
		   AND r.started_at > $1
		 ORDER BY r.started_at DESC
		 LIMIT 50`, time.Now().Add(-time.Duration(sweeperWindowMS)*time.Millisecond))
	if err != nil {
		slog.Warn("[sweeper] scan failed", "err", err)
		return 0
	}
	type cand struct {
		id, agent, company, summary string
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var company sql.NullString
		if rows.Scan(&c.id, &c.agent, &company, &c.summary) == nil {
			c.company = company.String
			cands = append(cands, c)
		}
	}
	rows.Close()

	requeued := 0
	for _, c := range cands {
		if !sweeperRetryableClasses[sweeperClassOf(c.summary)] {
			continue
		}
		// 闸②:已有后续成功 run 覆盖。
		var superseded int
		if err := s.DB.QueryRowContext(ctx,
			`SELECT 1 FROM agent_runs WHERE agent_id = $1 AND status = 'completed' AND started_at > (SELECT started_at FROM agent_runs WHERE id = $2) LIMIT 1`,
			c.agent, c.id).Scan(&superseded); err == nil {
			continue
		}
		// 闸③:已重排过。
		var requeuedAlready int
		if err := s.DB.QueryRowContext(ctx,
			`SELECT 1 FROM agent_events WHERE kind = 'run_requeue' AND run_id = $1 LIMIT 1`, c.id).
			Scan(&requeuedAlready); err == nil {
			continue
		}
		// 闸①:agent 当前忙——留下一 tick。
		var status string
		if err := s.DB.QueryRowContext(ctx,
			`SELECT status FROM participants WHERE id = $1 LIMIT 1`, c.agent).Scan(&status); err == nil &&
			(status == "thinking" || status == "working") {
			continue
		}
		// 留痕先行(幂等键),再唤醒;唤醒失败下一 tick 由闸③识别为已处理
		// ——唤醒与留痕的间隙由 pollLoop 天然兜底,可接受。
		if _, err := s.DB.ExecContext(ctx, `
			INSERT INTO agent_events (id, run_id, agent_id, company_id, kind, level, title, data)
			VALUES ($1, $2, $3, NULLIF($4, ''), 'run_requeue', 'info', 'server-side requeue of failed run',
			        jsonb_build_object('runId', $2, 'class', $5))`,
			"rev-"+sweeperID(), c.id, c.agent, c.company, sweeperClassOf(c.summary)); err != nil {
			slog.Warn("[sweeper] requeue event insert failed", "run", c.id, "err", err)
			continue
		}
		s.WakeOne(c.agent, "requeue:failed-run", nil, nil, nil)
		requeued++
	}
	return requeued
}

// StartRunSweeperWorker:周期 tick;ENABLE_RUN_SWEEPER='false' 或
// RUN_SWEEPER_INTERVAL_MS<=0 关闭。
func (s *S) StartRunSweeperWorker() {
	if config.Getenv("ENABLE_RUN_SWEEPER") == "false" {
		return
	}
	interval := runSweeperIntervalMS()
	if interval <= 0 {
		slog.Info("[sweeper] disabled (RUN_SWEEPER_INTERVAL_MS=0)")
		return
	}
	RunWorkerLoop(ctxBG, interval, "[sweeper]", func(ctx context.Context) {
		if n := s.RunRunSweeperTick(ctx); n > 0 {
			slog.Info("[sweeper] tick requeued", "count", n)
		}
	})
	slog.Info("[sweeper] running", "interval_ms", interval)
}

func runSweeperIntervalMS() int64 {
	if n, ok := config.EnvIntRaw("RUN_SWEEPER_INTERVAL_MS"); ok {
		return n
	}
	return 60_000
}

// sweeperID:agent_events.id(rev- + hex,与既有行形状同族)。
func sweeperID() string { return httpx.UUIDHex() }
