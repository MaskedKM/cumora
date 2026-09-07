// autorun —— #350 HR Agent 自动运行:周期例行 + 事件钩子。
//
// 周期例行:每公司 auto_interval_hours(默认 168=每周,0=关),到期即
// 入队全员轮(trigger_kind='periodic')。事件钩子(阈值即开关,置 0 关):
// ①已指派看板卡停更超 overdue_days;②近 24h LLM spend 超 spend_usd;
// ③近 24h 错误率超 error_rate(需 ≥5 次运行样本,免 1/1=100% 假警报)→
// 入队目标轮('event')。入队共用 startRound(无第二套管线),在飞互斥
// 由 0009 部分唯一索引兜底,去抖 = 同目标(或全员)轮创建于 24h 窗内
// 不重复入队(含失败轮 —— 防 Brain 连败时的每 tick 重试热循环)。
//
// 驱动面:生产由 60s worker(StartAutoRunScheduler,HR_AUTORUN_INTERVAL_MS
// 可调/0 关,ENABLE_HR_AUTORUN=false 总闸)扫可运行公司;测试与急性子
// owner 走 POST /api/hr/autorun/tick 同一核心(强制到期不等计时器,
// calendar run-now 同款形态)。
package hr

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/config"
	"github.com/MaskedKM/cumora/apps/server-go/internal/contract"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/sched"
)

const (
	// autorunCooldownHours:同目标去抖窗 —— 目标轮被「指向该 agent 的轮
	// 或全员轮」在窗内创建过即不再入队;全员(周期)轮被任意轮挡。
	autorunCooldownHours = 24
	// hookWindowHours:spend/error 两钩子的回看窗。
	hookWindowHours = 24
	// errorRateMinRuns:错误率钩子最小样本数。
	errorRateMinRuns = 5
	// hookScanLimit:单钩子单公司扫描行上限(防御性,异常 agent 多于
	// 此数也只逐 tick 排队,不一次打爆)。
	hookScanLimit = 200
)

/* ───────── 配置面(owner/admin;阈值与周期读写闸与其余 HR 面一致)───────── */

type autorunRow struct {
	intervalHours int
	lastRunAt     time.Time
	overdueDays   int
	spendUsd      float64
	errorRate     float64
	updatedAt     time.Time
}

func loadAutorun(ctx context.Context, db *sql.DB, companyID string) (autorunRow, bool) {
	var row autorunRow
	err := db.QueryRowContext(ctx, `
		SELECT auto_interval_hours, auto_last_run_at, event_overdue_days,
		       event_spend_usd, event_error_rate, updated_at
		  FROM hr_agents WHERE company_id = $1`, companyID).
		Scan(&row.intervalHours, &row.lastRunAt, &row.overdueDays,
			&row.spendUsd, &row.errorRate, &row.updatedAt)
	return row, err == nil
}

// payload:HrAutoRunStatus 契约形。nextPeriodicAt = 上次例行 + 周期
// (interval=0 关闭时 null);实际入队另受在飞互斥与去抖约束(契约注释)。
func (row autorunRow) payload() map[string]any {
	var next any
	if row.intervalHours > 0 {
		t := row.lastRunAt.Add(time.Duration(row.intervalHours) * time.Hour)
		next = t.UTC()
	}
	return map[string]any{
		"intervalHours":  row.intervalHours,
		"overdueDays":    row.overdueDays,
		"spendUsd":       row.spendUsd,
		"errorRate":      row.errorRate,
		"lastPeriodicAt": row.lastRunAt.UTC(),
		"nextPeriodicAt": next,
		"updatedAt":      row.updatedAt.UTC(),
	}
}

func (s *Server) GetHrAutoRun(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	if _, ok := loadOrProvision(r.Context(), s.DB, companyID); !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("%w: %s", errHrRowMissing, companyID))
		return
	}
	row, ok := loadAutorun(r.Context(), s.DB, companyID)
	if !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("%w: %s", errHrRowMissing, companyID))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row.payload())
}

func (s *Server) PutHrAutoRunConfig(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	if _, ok := loadOrProvision(r.Context(), s.DB, companyID); !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("%w: %s", errHrRowMissing, companyID))
		return
	}
	var body contract.HrAutoRunConfigInput
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// 部分更新 + 界校验(与 0013 CHECK 同界,app 层先拦给可读 400)。
	var sets []string
	var args []any
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if body.IntervalHours != nil {
		if *body.IntervalHours < 0 || *body.IntervalHours > 2160 {
			httpx.WriteError(w, http.StatusBadRequest, "intervalHours must be 0–2160 (0 disables the periodic run)")
			return
		}
		add("auto_interval_hours", *body.IntervalHours)
	}
	if body.OverdueDays != nil {
		if *body.OverdueDays < 0 || *body.OverdueDays > 365 {
			httpx.WriteError(w, http.StatusBadRequest, "overdueDays must be 0–365 (0 disables the hook)")
			return
		}
		add("event_overdue_days", *body.OverdueDays)
	}
	if body.SpendUsd != nil {
		if *body.SpendUsd < 0 {
			httpx.WriteError(w, http.StatusBadRequest, "spendUsd must be ≥ 0 (0 disables the hook)")
			return
		}
		add("event_spend_usd", float64(*body.SpendUsd))
	}
	if body.ErrorRate != nil {
		if *body.ErrorRate < 0 || *body.ErrorRate > 1 {
			httpx.WriteError(w, http.StatusBadRequest, "errorRate must be within 0–1 (0 disables the hook)")
			return
		}
		add("event_error_rate", float64(*body.ErrorRate))
	}
	if len(sets) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	args = append(args, companyID)
	if _, err := s.DB.ExecContext(r.Context(), `
		UPDATE hr_agents SET `+strings.Join(sets, ", ")+`, updated_at = NOW()
		 WHERE company_id = $`+strconv.Itoa(len(args)), args...); err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	row, ok := loadAutorun(r.Context(), s.DB, companyID)
	if !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("%w: %s", errHrRowMissing, companyID))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, row.payload())
}

/* ───────── tick 核心(worker 与强制端点共用)───────── */

// roundCoveredRecently:事件目标的同目标去抖 —— 指向该 agent 的轮或
// 全员轮在窗内创建过即算覆盖(全员轮覆盖所有目标)。含失败轮:防 Brain
// 连败/daemon 长离线时每 tick 重试的热循环。
func (s *Server) roundCoveredRecently(ctx context.Context, companyID, target string) bool {
	var one bool
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1 FROM hr_reports
		 WHERE company_id = $1
		   AND created_at > NOW() - ($2 || ' hours')::interval
		   AND ($3 = '' OR target_agent_id IS NULL OR target_agent_id = $3)
		 LIMIT 1`,
		companyID, strconv.Itoa(autorunCooldownHours), target).Scan(&one)
	return err == nil
}

// fullCoveredRecently:周期全员轮的去抖 —— 仅被近期全员轮(target IS
// NULL)挡;单目标轮只覆盖该 agent,不推迟全员例行(评审 P2:否则手动
// 评一人可把例行全员评挡 24h)。含失败轮,理由同上。
func (s *Server) fullCoveredRecently(ctx context.Context, companyID string) bool {
	var one bool
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1 FROM hr_reports
		 WHERE company_id = $1 AND target_agent_id IS NULL
		   AND created_at > NOW() - ($2 || ' hours')::interval
		 LIMIT 1`,
		companyID, strconv.Itoa(autorunCooldownHours)).Scan(&one)
	return err == nil
}

func (s *Server) hasInFlightRound(ctx context.Context, companyID string) bool {
	var one bool
	err := s.DB.QueryRowContext(ctx, `
		SELECT 1 FROM hr_reports WHERE company_id = $1 AND status IN ('pending', 'running') LIMIT 1`,
		companyID).Scan(&one)
	return err == nil
}

type hookHit struct {
	agentID string
	reason  string
}

// scanEventHooks:三钩子扫描(阈值 0 = 跳过)。同 agent 命中多钩子取
// 先扫到的理由;返回按 agentID 排序的确定性序列。
func (s *Server) scanEventHooks(ctx context.Context, companyID string, cfg autorunRow) []hookHit {
	hits := map[string]string{}
	collect := func(reason string, q string, args ...any) {
		rows, err := s.DB.QueryContext(ctx, q, args...)
		if err != nil {
			slog.Warn("[hr] autorun hook scan failed (hook degrades to skip)", "hook", reason, "err", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var aid string
			if rows.Scan(&aid) == nil {
				if _, ok := hits[aid]; !ok {
					hits[aid] = reason
				}
			}
		}
	}
	if cfg.overdueDays > 0 {
		// 已指派看板卡停更超阈(列无终态语义,以停更时长定义逾期;
		// 指派对象限在职 agent —— HR 只评 agent)。
		collect("overdue-card", `
			SELECT bc.assignee_id
			  FROM board_cards bc
			  JOIN boards b ON b.id = bc.board_id AND b.company_id = $1
			  JOIN participants p ON p.id = bc.assignee_id
			     AND p.company_id = $1 AND p.kind = 'agent' AND p.departed_at IS NULL
			 WHERE bc.updated_at < NOW() - ($2 || ' days')::interval
			 GROUP BY bc.assignee_id
			 LIMIT $3`,
			companyID, strconv.Itoa(cfg.overdueDays), hookScanLimit)
	}
	if cfg.spendUsd > 0 {
		collect("spend-over", `
			SELECT lc.agent_id
			  FROM llm_calls lc
			  JOIN participants p ON p.id = lc.agent_id
			     AND p.company_id = $1 AND p.kind = 'agent' AND p.departed_at IS NULL
			 WHERE lc.company_id = $1 AND lc.created_at > NOW() - ($2 || ' hours')::interval
			 GROUP BY lc.agent_id
			HAVING COALESCE(SUM(lc.cost_usd), 0) > $3
			 LIMIT $4`,
			companyID, strconv.Itoa(hookWindowHours), cfg.spendUsd, hookScanLimit)
	}
	if cfg.errorRate > 0 {
		collect("error-rate", `
			SELECT ar.agent_id
			  FROM agent_runs ar
			  JOIN participants p ON p.id = ar.agent_id
			     AND p.company_id = $1 AND p.kind = 'agent' AND p.departed_at IS NULL
			 WHERE ar.company_id = $1 AND ar.started_at > NOW() - ($2 || ' hours')::interval
			 GROUP BY ar.agent_id
			HAVING COUNT(*) >= $3
			   AND COUNT(*) FILTER (WHERE ar.status = 'failed')::float8 / COUNT(*) >= $4
			 LIMIT $5`,
			companyID, strconv.Itoa(hookWindowHours), errorRateMinRuns, cfg.errorRate, hookScanLimit)
	}
	out := make([]hookHit, 0, len(hits))
	for aid, reason := range hits {
		out = append(out, hookHit{agentID: aid, reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].agentID < out[j].agentID })
	return out
}

// runAutoTick:单公司一轮自动评估扫描。每 tick 至多入队一轮(周期或
// 首个过闸事件目标);多异常 agent 由后续 tick 在去抖约束下自然排队。
// 返回 HrAutoRunTickResult 契约形(强制端点直接回给前端/测试)。
func (s *Server) runAutoTick(ctx context.Context, companyID string) map[string]any {
	skipped := []string{}
	events := []map[string]any{}
	periodicFired := false
	skip := func(why string) map[string]any {
		skipped = append(skipped, why)
		return map[string]any{"periodicFired": periodicFired, "events": events, "skipped": skipped}
	}

	cfgRow, ok := loadHrAgent(ctx, s.DB, companyID)
	if !ok {
		return skip("hr-row-missing")
	}
	if !cfgRow.computerID.Valid || !cfgRow.engine.Valid {
		return skip("hr-computer-unassigned")
	}
	cfg, ok := loadAutorun(ctx, s.DB, companyID)
	if !ok {
		return skip("hr-row-missing")
	}
	var liveAgents int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)::int FROM participants
		 WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL`, companyID).
		Scan(&liveAgents); err != nil || liveAgents == 0 {
		return skip("no-live-agents")
	}
	if s.hasInFlightRound(ctx, companyID) {
		return skip("round-in-flight")
	}

	// 周期例行:到期(上次例行 + 周期)。全员轮只被近期全员轮去抖
	// (fullCoveredRecently,评审 P2);被去抖挡住时只记 skip 不早退 ——
	// 钩子扫描继续,目标级去抖自会防重复(评审 P1:挡周期 ≠ 停扫钩子,
	// 否则例行到期撞上任意近轮可让三钩子停扫最长 24h)。
	now := time.Now()
	if cfg.intervalHours > 0 && !now.Before(cfg.lastRunAt.Add(time.Duration(cfg.intervalHours)*time.Hour)) {
		if s.fullCoveredRecently(ctx, companyID) {
			skipped = append(skipped, "periodic-cooldown")
		} else {
			_, err := s.startRound(ctx, companyID, "", "periodic", "", "")
			switch {
			case err == nil:
				periodicFired = true
				// 到期点前进(auto_last_run_at=NOW):入队成功即算本轮例行
				// 已发生 —— daemon 离线致轮 failed 的场景由 24h 去抖挡住
				// 每 tick 重试,一周后自然再试。
				_, _ = s.DB.ExecContext(ctx,
					`UPDATE hr_agents SET auto_last_run_at = NOW() WHERE company_id = $1`, companyID)
			case err == errRoundInFlight:
				skipped = append(skipped, "round-in-flight")
			case err == errDaemonOffline:
				skipped = append(skipped, "daemon-offline")
			default:
				slog.Warn("[hr] autorun periodic start failed", "company", companyID, "err", err)
				skipped = append(skipped, "periodic-error")
			}
		}
	}

	// 事件钩子:周期轮刚入队则本 tick 收工(在飞互斥天然如此,显式短路)。
	if periodicFired {
		return map[string]any{"periodicFired": periodicFired, "events": events, "skipped": skipped}
	}
	for _, hit := range s.scanEventHooks(ctx, companyID, cfg) {
		if s.roundCoveredRecently(ctx, companyID, hit.agentID) {
			skipped = append(skipped, "cooldown:"+hit.agentID)
			continue
		}
		id, err := s.startRound(ctx, companyID, hit.agentID, "event", hit.reason, "")
		switch {
		case err == nil:
			events = append(events, map[string]any{"id": id, "agentId": hit.agentID, "reason": hit.reason})
			// 每公司每 tick 一轮:公司级在飞互斥下第二起必 409,
			// 明确收工让队列由后续 tick 排空。
			return map[string]any{"periodicFired": periodicFired, "events": events, "skipped": skipped}
		case err == errRoundInFlight:
			return skip("round-in-flight")
		case err == errDaemonOffline:
			return skip("daemon-offline")
		default:
			slog.Warn("[hr] autorun event start failed", "company", companyID, "target", hit.agentID, "err", err)
			return skip("event-error")
		}
	}
	if len(skipped) == 0 && len(events) == 0 && !periodicFired {
		skipped = append(skipped, "no-hook-hit")
	}
	return map[string]any{"periodicFired": periodicFired, "events": events, "skipped": skipped}
}

// TriggerHrAutoRunTick:强制到期端点(测试驱动面 + 急性子 owner 手动
// 扫描)。与 worker 同核心,只作用本公司;不等真实计时器。
func (s *Server) TriggerHrAutoRunTick(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.runAutoTick(r.Context(), companyID))
}

/* ───────── 周期 worker(生产驱动面)───────── */

// RunAutoRunTickAll:扫全部可运行公司(HR 已指派且机器未吊销 + 名册
// 有在职 agent)各跑一轮 tick;单公司 panic 不炸整轮(calendar dispatch
// 同款逐项 recover)。
func (s *Server) RunAutoRunTickAll(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT h.company_id FROM hr_agents h
		 WHERE h.computer_id IS NOT NULL AND h.engine IS NOT NULL
		   AND EXISTS (SELECT 1 FROM computers c
		                WHERE c.id = h.computer_id AND c.company_id = h.company_id
		                  AND c.revoked_at IS NULL)
		   AND EXISTS (SELECT 1 FROM participants p
		                WHERE p.company_id = h.company_id AND p.kind = 'agent'
		                  AND p.departed_at IS NULL)`)
	if err != nil {
		slog.Warn("[hr] autorun scan failed", "err", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		func(companyID string) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Warn("[hr] autorun tick panicked", "company", companyID, "recover", rec)
				}
			}()
			s.runAutoTick(ctx, companyID)
		}(id)
	}
}

// StartAutoRunScheduler:周期 tick;ENABLE_HR_AUTORUN='false' 总关,
// HR_AUTORUN_INTERVAL_MS<=0 关(默认 60s;集成 SUT 置 0 —— 测试走
// 强制端点,与 ENABLE_SCANNER/ENABLE_IDLE 同一哲学)。
func (s *Server) StartAutoRunScheduler(ctx context.Context) {
	if config.Getenv("ENABLE_HR_AUTORUN") == "false" {
		return
	}
	interval := int64(60_000)
	if v, ok := config.EnvIntRaw("HR_AUTORUN_INTERVAL_MS"); ok {
		interval = v
	}
	if interval <= 0 {
		slog.Info("[hr] autorun scheduler disabled (HR_AUTORUN_INTERVAL_MS=0)")
		return
	}
	sched.RunWorkerLoop(ctx, interval, "[hr] autorun", s.RunAutoRunTickAll)
	slog.Info("[hr] autorun scheduler running", "interval_ms", interval)
}
