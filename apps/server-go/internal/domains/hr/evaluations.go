// evaluations —— #346 手动评估全链的 server 半边。
//
// 触发(POST /api/hr/evaluations):在飞互斥(部分唯一索引兜底)→ 客观
// 观测快照装配落 input_snapshot → 唤醒 hr-<companyId>(brief 即任务书,
// 不另设取件面)→ pending。daemon 侧 Brain 经 CLI 拉输入(`hr context`)
// 与交报告(`hr report`);报告落库即终态(done/failed)。daemon 整机死亡
// 的悬置轮由下一次触发的陈旧收尸(在飞 >30min 自动 failed)兜底,零新增
// worker。读面(列表/详情)仅 owner/admin —— 评估结果只向老板汇报。
package hr

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
	"github.com/MaskedKM/cumora/apps/server-go/internal/authn"
	"github.com/MaskedKM/cumora/apps/server-go/internal/db"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/sched"
)

func (s *Server) CreateHrEvaluation(w http.ResponseWriter, r *http.Request) {
	uid, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	var body struct {
		TargetAgentID *string `json:"targetAgentId"`
	}
	// 坏体 400;空体(EOF)容忍 = 全员轮
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	target := ""
	if body.TargetAgentID != nil {
		target = strings.TrimSpace(*body.TargetAgentID)
	}

	cfgRow, ok := loadOrProvision(r.Context(), s.DB, companyID)
	if !ok {
		httpx.WriteInternalError(w, r, fmt.Errorf("hr_agents row missing for company %s", companyID))
		return
	}
	if !cfgRow.computerID.Valid || !cfgRow.engine.Valid {
		httpx.WriteError(w, http.StatusBadRequest, "assign a computer and engine to the HR Agent before triggering evaluations")
		return
	}
	if target != "" {
		var exists bool
		_ = s.DB.QueryRowContext(r.Context(),
			`SELECT 1 FROM participants WHERE id = $1 AND company_id = $2 AND kind = 'agent' AND departed_at IS NULL LIMIT 1`,
			target, companyID).Scan(&exists)
		if !exists {
			httpx.WriteError(w, http.StatusBadRequest, "unknown target agent")
			return
		}
	}
	// 陈旧在飞收尸:daemon 整机死亡留下的悬置轮(>30min)自动 failed,
	// 让本次触发可通过;新鲜在飞仍由唯一索引拦成 409。
	_, _ = s.DB.ExecContext(r.Context(), `
		UPDATE hr_reports
		   SET status = 'failed', error = 'superseded: in-flight round stale over 30min',
		       updated_at = NOW(), finished_at = NOW()
		 WHERE company_id = $1 AND status IN ('pending', 'running')
		   AND updated_at < NOW() - INTERVAL '30 minutes'`, companyID)

	snapshot, err := s.assembleInputs(r.Context(), companyID, target)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	snapJSON, err := json.Marshal(snapshot)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	id := "hre-" + authn.NewToken()[:10]
	_, err = s.DB.ExecContext(r.Context(), `
		INSERT INTO hr_reports (id, company_id, target_agent_id, trigger_kind, status, input_snapshot, created_by)
		VALUES ($1, $2, NULLIF($3, ''), 'manual', 'pending', $4, NULLIF($5, ''))`,
		id, companyID, target, snapJSON, uid)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			httpx.WriteError(w, http.StatusConflict, "an evaluation round is already in flight for this team")
			return
		}
		httpx.WriteInternalError(w, r, err)
		return
	}
	if s.Wake != nil {
		delivered := s.Wake("hr-"+companyID, "hr-eval", &sched.BackgroundBrief{
			Source: "hr-eval",
			Title:  "HR evaluation round " + id,
			Ref:    id,
			Body: fmt.Sprintf(
				"An HR evaluation round (%s) has been triggered by the owner. Steps: "+
					"(1) fetch your inputs: `cumora hr context %s` "+
					"(2) evaluate the target agent(s) per your standing instructions "+
					"(3) submit the structured report as a single JSON object: "+
					"`cumora hr report %s '<json>'`. The round closes when the report lands.",
				id, id, id),
		})
		// brief 走 Redis PUBLISH 一次性投递,HR 无 inbox 持久兜底(非
		// participant)——0 接收者 = daemon 离线,任务书已丢。本轮直接
		// failed 放行重触发,不留 30min 悬置锁。
		if delivered == 0 {
			_, _ = s.DB.ExecContext(r.Context(), `
				UPDATE hr_reports SET status = 'failed',
				       error = 'HR daemon offline — reconnect its computer, then retrigger',
				       updated_at = NOW(), finished_at = NOW()
				 WHERE id = $1 AND status = 'pending'`, id)
			httpx.WriteError(w, http.StatusServiceUnavailable,
				"HR daemon offline — reconnect its computer, then retrigger")
			return
		}
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"id": id, "trigger": "manual", "status": "pending",
		"targetAgentId": nilIfEmpty(target), "createdAt": time.Now().UTC(), "updatedAt": time.Now().UTC(),
	})
}

func (s *Server) ListHrEvaluations(w http.ResponseWriter, r *http.Request) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT id, target_agent_id, trigger_kind, status, error, created_at, updated_at, finished_at
		  FROM hr_reports WHERE company_id = $1
		 ORDER BY created_at DESC LIMIT 50`, companyID)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, triggerKind, status string
		var target, errMsg sql.NullString
		var createdAt, updatedAt time.Time
		var finishedAt sql.NullTime
		if rows.Scan(&id, &target, &triggerKind, &status, &errMsg, &createdAt, &updatedAt, &finishedAt) != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "trigger": triggerKind, "status": status,
			"targetAgentId": nullStr(target), "error": nullStr(errMsg),
			"createdAt": createdAt.UTC(), "updatedAt": updatedAt.UTC(), "finishedAt": nullTime(finishedAt),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": out})
}

func (s *Server) GetHrEvaluation(w http.ResponseWriter, r *http.Request, id string) {
	_, companyID, ok := requireRole(w, r, s.DB)
	if !ok {
		return
	}
	var target, errMsg sql.NullString
	var triggerKind, status string
	var payload, snapshot []byte
	var createdAt, updatedAt time.Time
	var finishedAt sql.NullTime
	err := s.DB.QueryRowContext(r.Context(), `
		SELECT target_agent_id, trigger_kind, status, payload, error, input_snapshot, created_at, updated_at, finished_at
		  FROM hr_reports WHERE id = $1 AND company_id = $2 LIMIT 1`, id, companyID).
		Scan(&target, &triggerKind, &status, &payload, &errMsg, &snapshot, &createdAt, &updatedAt, &finishedAt)
	if err == sql.ErrNoRows {
		httpx.WriteError(w, http.StatusNotFound, "no such evaluation round")
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": id, "trigger": triggerKind, "status": status,
		"targetAgentId": nullStr(target), "error": nullStr(errMsg),
		"payload": rawJSON(payload), "inputSnapshot": rawJSON(snapshot),
		"createdAt": createdAt.UTC(), "updatedAt": updatedAt.UTC(), "finishedAt": nullTime(finishedAt),
	})
}

/* ───────── CLI 面(daemon 侧 Brain 的拉输入/交报告;JWT 钉身份) ───────── */

// Cli:runtime 接线面(cli_domains case "hr")。调用方身份由 handleCli 从
// JWT sub 钉死注入 --as —— 只有本公司的 hr-<companyId> 实体能用。
//
//	cumora hr context <evaluationId>            → 输入快照 JSON
//	cumora hr report  <evaluationId> '<json>'   → 交报告收轮(done/failed)
func (s *Server) Cli(ctx context.Context, p agent.Parsed) agent.Result {
	caller, err := agent.ResolveAs(p)
	if err != nil {
		return agent.Err("hr: " + err.Error())
	}
	if !strings.HasPrefix(caller, "hr-") {
		return agent.Err("hr commands are reserved for the HR Agent")
	}
	companyID := strings.TrimPrefix(caller, "hr-")
	// 实体闸(评审 P1-2):把租户闭合收回本域,不依赖"公司 id 恒 co- 前缀 +
	// agents 域撞形守卫"两道跨域不变量 —— caller 对应的 hr_agents 行必须
	// 真实存在(顺带挡掉 hr-assistant 这类普通 agent 的前缀穿越)。
	var hrRowExists bool
	if err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM hr_agents WHERE company_id = $1 LIMIT 1`, companyID).Scan(&hrRowExists); err != nil || !hrRowExists {
		return agent.Err("hr commands are reserved for the HR Agent")
	}
	pos := p.Positional()
	if len(pos) == 0 {
		return agent.Err("usage: cumora hr <context|report> <evaluationId> [json]")
	}
	switch pos[0] {
	case "context":
		return s.cliContext(ctx, companyID, pos)
	case "report":
		return s.cliReport(ctx, companyID, pos, p)
	default:
		return agent.Err("unknown hr subcommand: " + pos[0] + " (context|report)")
	}
}

func (s *Server) cliContext(ctx context.Context, companyID string, pos []string) agent.Result {
	if len(pos) < 2 || strings.TrimSpace(pos[1]) == "" {
		return agent.Err("usage: cumora hr context <evaluationId>")
	}
	var snapshot []byte
	var status string
	err := s.DB.QueryRowContext(ctx,
		`SELECT input_snapshot, status FROM hr_reports WHERE id = $1 AND company_id = $2 LIMIT 1`,
		strings.TrimSpace(pos[1]), companyID).Scan(&snapshot, &status)
	if err != nil {
		return agent.Err("unknown evaluation round")
	}
	if len(snapshot) == 0 {
		return agent.Err("evaluation round has no input snapshot")
	}
	// 生命周期兑现(评审 P1-3):Brain 取输入即视为开跑 —— pending → running
	// (幂等;终态轮不改)。
	_, _ = s.DB.ExecContext(ctx, `
		UPDATE hr_reports SET status = 'running', updated_at = NOW()
		 WHERE id = $1 AND company_id = $2 AND status = 'pending`,
		strings.TrimSpace(pos[1]), companyID)
	return agent.OK(string(snapshot))
}

func (s *Server) cliReport(ctx context.Context, companyID string, pos []string, p agent.Parsed) agent.Result {
	if len(pos) < 2 || strings.TrimSpace(pos[1]) == "" {
		return agent.Err("usage: cumora hr report <evaluationId> '<json>'")
	}
	id := strings.TrimSpace(pos[1])
	body := strings.TrimSpace(p.JoinBodyArgs(2))
	if body == "" {
		return agent.Err("report JSON required: cumora hr report <evaluationId> '<json>'")
	}
	if agent.UTF16Len(body) > 120_000 {
		return agent.Err("report too large (max 120k UTF-16 units)")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return agent.Err("report must be a single JSON object")
	}
	status := "done"
	errText := ""
	if failed, ok := payload["failed"].(bool); ok && failed {
		status = "failed"
	}
	if ev, ok := payload["error"].(string); ok && ev != "" {
		status = "failed"
		errText = ev
	}
	// #348:报告可携带 jobEdits(岗位层修改)—— 与收轮同事务应用;任一条
	// 不合法整体回滚(轮保持打开,半套优化落库比不落库更糟)。失败轮
	// 不许带 edits:failed = 评估未完成,何来优化建议(评审 P1 显式裁定)。
	edits, err := jobEditsFrom(payload)
	if err != nil {
		return agent.Err(err.Error())
	}
	if status == "failed" && len(edits) > 0 {
		return agent.Err("failed rounds cannot carry jobEdits — report without edits, or fix the failure")
	}
	var applied int
	err = db.WithTx(ctx, s.DB, func(tx *sql.Tx) error {
		n, err := applyJobEdits(ctx, tx, companyID, id, edits)
		if err != nil {
			return err
		}
		applied = n
		res, err := tx.ExecContext(ctx, `
			UPDATE hr_reports
			   SET status = $3, payload = $4, error = NULLIF($5, ''),
			       updated_at = NOW(), finished_at = NOW()
			 WHERE id = $1 AND company_id = $2 AND status IN ('pending', 'running')`,
			id, companyID, status, payload, errText)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errRoundClosed
		}
		return nil
	})
	if err == errRoundClosed {
		return agent.Err("evaluation round is not open (already reported or closed)")
	}
	if err != nil {
		return agent.Err(err.Error())
	}
	return agent.OK(fmt.Sprintf("recorded: %s (%s, %d job edit(s) applied)", id, status, applied))
}

// errRoundClosed:收轮 UPDATE 零行的哨兵(区别于其它 SQL 错误)。
var errRoundClosed = errors.New("round closed")

/* ───────── 客观观测快照装配(刀 2:五路聚合;#347 再补转录/同侪/评分) ───────── */

const inputWindowDays = 14

// hrInputTarget:单目标聚合累加器;装配即终形(payload 字段名=累加器输出键)。
type hrInputTarget struct {
	agentID, name, role               string
	runsTotal, runsFailed             int
	runTokens                         int64
	runCost, llmCost                  float64
	lastRunAt                         sql.NullTime
	llmCalls                          int
	triageTotal, triageActionable     int
	cardsAssigned, deliveries, merged int
	calActive, calDone, calCancelled  int
	// #347 三路:owner 主观评分(0=未评)+ 同侪信号(双向)+ 近窗转录(有界)
	ratingScore                int
	ratingComment              string
	climateToward, climateFelt []map[string]any
	recentMessages             []map[string]any
}

func (t hrInputTarget) payload() map[string]any {
	return map[string]any{
		"agentId": t.agentID, "name": t.name, "role": t.role,
		"runs":           map[string]any{"total": t.runsTotal, "failed": t.runsFailed, "tokens": t.runTokens, "costUsd": t.runCost, "lastRunAt": nullTime(t.lastRunAt)},
		"llm":            map[string]any{"calls": t.llmCalls, "costUsd": t.llmCost},
		"triage":         map[string]any{"total": t.triageTotal, "actionable": t.triageActionable},
		"cards":          map[string]any{"assigned": t.cardsAssigned, "deliveries": t.deliveries, "mergedDeliveries": t.merged},
		"calendar":       map[string]any{"active": t.calActive, "done": t.calDone, "cancelled": t.calCancelled},
		"rating":         map[string]any{"score": t.ratingScore, "comment": t.ratingComment},
		"climate":        map[string]any{"towardThem": t.climateToward, "theyFeel": t.climateFelt},
		"recentMessages": t.recentMessages,
	}
}

func (s *Server) assembleInputs(ctx context.Context, companyID, target string) (map[string]any, error) {
	targetRows, err := s.DB.QueryContext(ctx, `
		SELECT id, COALESCE(name, ''), COALESCE(role, '')
		  FROM participants
		 WHERE company_id = $1 AND kind = 'agent' AND departed_at IS NULL
		   AND ($2 = '' OR id = $2)
		 ORDER BY name ASC`, companyID, target)
	if err != nil {
		return nil, err
	}
	byID := map[string]*hrInputTarget{}
	order := []string{}
	for targetRows.Next() {
		t := hrInputTarget{
			climateToward: []map[string]any{}, climateFelt: []map[string]any{}, recentMessages: []map[string]any{},
		}
		if targetRows.Scan(&t.agentID, &t.name, &t.role) == nil {
			byID[t.agentID] = &t
			order = append(order, t.agentID)
		}
	}
	targetRows.Close()
	if len(order) == 0 {
		return map[string]any{"generatedAt": time.Now().UTC(), "windowDays": inputWindowDays, "targets": []any{}}, nil
	}
	ids := make([]string, 0, len(order))
	for _, id := range order {
		ids = append(ids, id)
	}

	// 五路聚合:失败记日志降级(缺表/部分 schema 不阻断触发;快照里缺路=
	// 零值,报告可解释)—— 评审 P0 教训 ×3(全部实测自 standing-stack 日志):
	// ①静默吞错让错误不可见;②ANY($N) 直传 []string 经 database/sql 不绑
	// (库内惯例:数组字面量 + ::text[],conversations.arrayLiteral 同款);
	// ③($N || ' days')::interval 令 PG 把参数推断成 text(OID 25),pgx
	// 编不出 int→text —— 窗口参数以字符串传入。五路全部显式转型绑定。
	idsArr := pgTextArray(ids)
	windowDaysArg := strconv.Itoa(inputWindowDays)
	collect := func(query func(rows *sql.Rows), q string, args ...any) {
		rows, err := s.DB.QueryContext(ctx, q, args...)
		if err != nil {
			slog.Warn("[hr] input lane query failed (lane degrades to zero)", "err", err)
			return
		}
		query(rows)
		rows.Close()
	}
	collect(func(rows *sql.Rows) {
		for rows.Next() {
			var aid string
			var t hrInputTarget
			if rows.Scan(&aid, &t.runsTotal, &t.runsFailed, &t.runTokens, &t.runCost, &t.lastRunAt) == nil {
				if m, ok := byID[aid]; ok {
					m.runsTotal, m.runsFailed, m.runTokens, m.runCost, m.lastRunAt =
						t.runsTotal, t.runsFailed, t.runTokens, t.runCost, t.lastRunAt
				}
			}
		}
	}, `SELECT agent_id, COUNT(*)::int, COUNT(*) FILTER (WHERE status = 'failed')::int,
		       COALESCE(SUM(token_count), 0)::bigint, COALESCE(SUM(cost_usd), 0), MAX(started_at)
		  FROM agent_runs
		 WHERE company_id = $1 AND agent_id = ANY($2::text[]) AND started_at > NOW() - ($3 || ' days')::interval
		 GROUP BY agent_id`, companyID, idsArr, windowDaysArg)

	collect(func(rows *sql.Rows) {
		for rows.Next() {
			var aid string
			var calls int
			var cost float64
			if rows.Scan(&aid, &calls, &cost) == nil {
				if m, ok := byID[aid]; ok {
					m.llmCalls, m.llmCost = calls, cost
				}
			}
		}
	}, `SELECT agent_id, COUNT(*)::int, COALESCE(SUM(cost_usd), 0)
		  FROM llm_calls
		 WHERE company_id = $1 AND agent_id = ANY($2::text[]) AND created_at > NOW() - ($3 || ' days')::interval
		 GROUP BY agent_id`, companyID, idsArr, windowDaysArg)

	collect(func(rows *sql.Rows) {
		for rows.Next() {
			var aid string
			var total, actionable int
			if rows.Scan(&aid, &total, &actionable) == nil {
				if m, ok := byID[aid]; ok {
					m.triageTotal, m.triageActionable = total, actionable
				}
			}
		}
	}, `SELECT agent_id, COUNT(*)::int, COUNT(*) FILTER (WHERE actionable)::int
		  FROM agent_triages
		 WHERE agent_id = ANY($1::text[]) AND created_at > NOW() - ($2 || ' days')::interval
		 GROUP BY agent_id`, idsArr, windowDaysArg)

	collect(func(rows *sql.Rows) {
		for rows.Next() {
			var aid string
			var n int
			if rows.Scan(&aid, &n) == nil {
				if m, ok := byID[aid]; ok {
					m.cardsAssigned = n
				}
			}
		}
	}, `SELECT assignee_id, COUNT(*)::int FROM board_cards
		 WHERE assignee_id = ANY($1::text[]) AND created_at > NOW() - ($2 || ' days')::interval
		 GROUP BY assignee_id`, idsArr, windowDaysArg)

	collect(func(rows *sql.Rows) {
		for rows.Next() {
			var aid string
			var total, merged int
			if rows.Scan(&aid, &total, &merged) == nil {
				if m, ok := byID[aid]; ok {
					m.deliveries, m.merged = total, merged
				}
			}
		}
	}, `SELECT created_by, COUNT(*)::int, COUNT(*) FILTER (WHERE pr_state = 'merged')::int
		  FROM card_deliveries
		 WHERE created_by = ANY($1::text[]) AND created_at > NOW() - ($2 || ' days')::interval
		 GROUP BY created_by`, idsArr, windowDaysArg)

	collect(func(rows *sql.Rows) {
		for rows.Next() {
			var aid, status string
			var n int
			if rows.Scan(&aid, &status, &n) != nil {
				continue
			}
			m, ok := byID[aid]
			if !ok {
				continue
			}
			switch status {
			case "active":
				m.calActive = n
			case "done":
				m.calDone = n
			case "cancelled":
				m.calCancelled = n
			}
		}
	}, `SELECT assignee_id, status, COUNT(*)::int FROM calendar_events
		 WHERE assignee_id = ANY($1::text[]) AND created_at > NOW() - ($2 || ' days')::interval
		 GROUP BY assignee_id, status`, idsArr, windowDaysArg)

	// ── #347 三路:owner 主观评分 / 同侪信号(双向)/ 近窗转录(有界)──

	collect(func(rows *sql.Rows) {
		for rows.Next() {
			var aid string
			var score int
			var comment string
			if rows.Scan(&aid, &score, &comment) == nil {
				if m, ok := byID[aid]; ok {
					m.ratingScore, m.ratingComment = score, comment
				}
			}
		}
	}, `SELECT agent_id, score, comment FROM hr_ratings
		 WHERE company_id = $1 AND agent_id = ANY($2::text[])`, companyID, idsArr)

	// 同侪(评审 P0 修正):agent_climate.company_id 无任何生产写入方(恒
	// DEFAULT 'personal'),按它过滤 = 生产恒空 —— 租户改经 participants
	// 连接推导(自愈,免回填)。每目标查(两方向合计 ≤ climateCap 行),
	// 消除全员轮的 N² 无界快照。
	const climateCap = 40
	for _, tid := range order {
		rows, err := s.DB.QueryContext(ctx, `
			SELECT ac.agent_id, ac.about_id, ac.affinity, ac.trust, ac.last_note
			  FROM agent_climate ac
			  JOIN participants p1 ON p1.id = ac.agent_id AND p1.company_id = $1
			  JOIN participants p2 ON p2.id = ac.about_id AND p2.company_id = $1
			 WHERE (ac.about_id = $2 AND ac.agent_id <> $2) OR (ac.agent_id = $2 AND ac.about_id <> $2)
			 ORDER BY ac.updated_at DESC
			 LIMIT $3`, companyID, tid, climateCap)
		if err != nil {
			slog.Warn("[hr] input lane query failed (lane degrades to zero)", "lane", "climate", "err", err)
			continue
		}
		for rows.Next() {
			var agentID, aboutID, note string
			var affinity, trust float32
			if rows.Scan(&agentID, &aboutID, &affinity, &trust, &note) != nil || agentID == aboutID {
				continue
			}
			if aboutID == tid {
				byID[tid].climateToward = append(byID[tid].climateToward, climateRow("from", agentID, affinity, trust, note))
			} else {
				byID[tid].climateFelt = append(byID[tid].climateFelt, climateRow("about", aboutID, affinity, trust, note))
			}
		}
		rows.Close()
	}

	// 转录(评审 P1 修正):改每目标取数(LIMIT + id tie-breaker)—— 原
	// 全局 800 截断在多目标轮会被话多的目标吃满,安静目标饿死为零。
	// 单条截 400 UTF-16;全轮转录预算 transcriptBudget 单元,先到先得,
	// 触顶即截该目标并在快照上置 transcriptsTruncated(Brain 可见口径)。
	const perTargetMsgCap = 40
	const bodyCap = 400
	const transcriptBudget = 150_000 // UTF-16 单元
	budgetLeft := transcriptBudget
	transcriptsTruncated := false
	for _, tid := range order {
		rows, err := s.DB.QueryContext(ctx, `
			SELECT m.conversation_id, c.kind, m.author_id, m.kind, m.body, m.created_at
			  FROM conversation_members cm
			  JOIN messages m ON m.conversation_id = cm.conversation_id
			  JOIN conversations c ON c.id = m.conversation_id AND c.company_id = $1
			 WHERE cm.participant_id = $2
			   AND m.created_at > NOW() - ($3 || ' days')::interval
			 ORDER BY m.created_at DESC, m.id DESC
			 LIMIT $4`, companyID, tid, windowDaysArg, perTargetMsgCap)
		if err != nil {
			slog.Warn("[hr] input lane query failed (lane degrades to zero)", "lane", "transcripts", "err", err)
			continue
		}
		list := []map[string]any{}
		for rows.Next() {
			var conversationID, convKind, authorID, msgKind, body string
			var createdAt time.Time
			if rows.Scan(&conversationID, &convKind, &authorID, &msgKind, &body, &createdAt) != nil {
				continue
			}
			capped := httpx.UTF16Cap(body, bodyCap)
			units := agent.UTF16Len(capped)
			if units > budgetLeft {
				transcriptsTruncated = true
				break
			}
			budgetLeft -= units
			// 时间正序输出(rows 是 DESC)
			list = append([]map[string]any{{
				"conversationId": conversationID, "conversationKind": convKind,
				"authorId": authorID, "kind": msgKind,
				"body": capped, "at": createdAt.UTC(),
			}}, list...)
		}
		rows.Close()
		byID[tid].recentMessages = list
	}

	targets := make([]map[string]any, 0, len(order))
	for _, id := range order {
		targets = append(targets, byID[id].payload())
	}
	snapshot := map[string]any{
		"generatedAt": time.Now().UTC(),
		"windowDays":  inputWindowDays,
		"note": "four input lanes: objective observation (runs/llm/triage/boards/calendar), owner ratings (score 0 = unrated), " +
			"peer signals (agent_climate both directions, 40 rows/target), bounded recent transcripts " +
			"(40/target, 400 UTF-16 units/message, 150k-unit round budget)",
		"targets": targets,
	}
	if transcriptsTruncated {
		snapshot["transcriptsTruncated"] = true
	}
	return snapshot, nil
}

// climateRow:同侪信号行的两种朝向(towardThem 用 "from",theyFeel 用 "about")。
func climateRow(dirKey, otherID string, affinity, trust float32, note string) map[string]any {
	row := map[string]any{dirKey: otherID, "affinity": affinity, "trust": trust}
	if note != "" {
		row["note"] = note
	}
	return row
}

/* ───────── 小件 ───────── */

// pgTextArray:[]string → PG 数组字面量。database/sql 不直接绑切片参数,
// 库内既有惯例即此形态(conversations.arrayLiteral 同款)。
func pgTextArray(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		escaped := strings.ReplaceAll(id, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		parts = append(parts, `"`+escaped+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func rawJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

func nullTime(nt sql.NullTime) any {
	if nt.Valid {
		return nt.Time.UTC()
	}
	return nil
}
