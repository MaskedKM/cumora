// obs 包 api —— /api/agents/observability 三面(#68):
// runs 列表、triage 经济学(缓存感知真账)、llm-spend rollup。挂在
// coreRouter(authMiddleware 链内),门禁=devtools 头+角色(devtools 域
// 同语义;本包内联实现避免跨域依赖)。价表复用 costing 包
// (#140 自 runtime 拆出,纯移动——方法挂到 API{DB} 小接收器上)。
package obs

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/costing"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// API:observability 三面的接收器——只带 DB,方法体与拆包前逐字对齐。
type API struct {
	DB *sql.DB
}

const stalledInterval = "5 minutes"

var agentRunStatuses = map[string]bool{
	"running": true, "completed": true, "failed": true, "skipped": true, "stalled": true,
}

// obsRequireDevtools:NODE_ENV≠production 恒开;否则 dev 头 + owner/admin。
func (s *API) obsRequireDevtools(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, ok := httpx.RequireAuth(w, r)
	if !ok {
		return "", false
	}
	companyID, ok := httpx.ResolveCompany(w, r, s.DB, uid)
	if !ok {
		return "", false
	}
	var role string
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
		companyID, uid).Scan(&role); err != nil {
		role = "member"
	}
	localDev := os.Getenv("NODE_ENV") != "production"
	h := r.Header.Get("x-cumora-dev-mode")
	requested := h == "1" || h == "true"
	priv := role == "owner" || role == "admin"
	if !(localDev || (requested && priv)) {
		httpx.WriteError(w, http.StatusForbidden, "developer tools are not enabled")
		return "", false
	}
	return companyID, true
}

func (s *API) HandleObsRuns(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.obsRequireDevtools(w, r)
	if !ok {
		return
	}
	clauses := []string{"r.company_id = $1"}
	params := []any{tenant}
	if agentID := strings.TrimSpace(r.URL.Query().Get("agentId")); agentID != "" {
		params = append(params, agentID)
		clauses = append(clauses, "r.agent_id = $"+strconv.Itoa(len(params)))
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" && agentRunStatuses[status] {
		if status == "stalled" {
			clauses = append(clauses, "r.status = 'running' AND r.updated_at < NOW() - INTERVAL '"+stalledInterval+"'")
		} else if status == "running" {
			clauses = append(clauses, "r.status = 'running' AND r.updated_at >= NOW() - INTERVAL '"+stalledInterval+"'")
		} else {
			params = append(params, status)
			clauses = append(clauses, "r.status = $"+strconv.Itoa(len(params)))
		}
	}
	rawLimit, _ := strconv.ParseFloat(r.URL.Query().Get("limit"), 64)
	if r.URL.Query().Get("limit") == "" {
		rawLimit = 50
	}
	limit := int(rawLimit)
	if limit < 10 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	params = append(params, limit)
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT
		    r.id, r.agent_id,
		    COALESCE(p.name, r.agent_id), p.role, p.avatar_url,
		    r.company_id,
		    CASE
		      WHEN r.status = 'running' AND r.updated_at < NOW() - INTERVAL '`+stalledInterval+`' THEN 'stalled'
		      ELSE r.status
		    END,
		    r.stage, r.summary, r.error, r.trigger,
		    r.input_message_ids, r.inbox_count, r.tool_call_count, r.token_count,
		    r.fingerprint, r.started_at, r.updated_at, r.finished_at,
		    ROUND(EXTRACT(EPOCH FROM (COALESCE(r.finished_at, NOW()) - r.started_at)) * 1000)::int
		  FROM agent_runs r
		  LEFT JOIN participants p ON p.id = r.agent_id AND p.company_id = r.company_id
		 WHERE `+strings.Join(clauses, " AND ")+`
		 ORDER BY r.started_at DESC
		 LIMIT $`+strconv.Itoa(len(params)), params...)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, agentID, agentName, status string
		var agentRole, agentAvatar, summary, errMsg sql.NullString
		var stage sql.NullString
		var companyID sql.NullString
		var trigger, inputIDs []byte
		var inboxCount, toolCount, tokenCount sql.NullInt64
		var fingerprint sql.NullString
		var startedAt, updatedAt time.Time
		var finishedAt sql.NullTime
		var durationMs sql.NullInt64
		if err := rows.Scan(&id, &agentID, &agentName, &agentRole, &agentAvatar, &companyID,
			&status, &stage, &summary, &errMsg, &trigger,
			&inputIDs, &inboxCount, &toolCount, &tokenCount,
			&fingerprint, &startedAt, &updatedAt, &finishedAt, &durationMs); err != nil {
			slog.Warn("[obs] runs scan failed", "err", err)
			continue
		}
		var triggerAny, inputAny any
		_ = jsonUnmarshalToAny(trigger, &triggerAny)
		_ = jsonUnmarshalToAny(inputIDs, &inputAny)
		out = append(out, map[string]any{
			"id": id, "agentId": agentID, "agentName": agentName,
			"agentRole": nullStrAny(agentRole), "agentAvatarUrl": nullStrAny(agentAvatar),
			"companyId": nullStrAny(companyID), "status": status,
			"stage": nullStrAny(stage), "summary": nullStrAny(summary), "error": nullStrAny(errMsg),
			"trigger": triggerAny, "inputMessageIds": inputAny,
			"inboxCount": nullIntAny(inboxCount), "toolCallCount": nullIntAny(toolCount),
			"tokenCount": nullIntAny(tokenCount), "fingerprint": nullStrAny(fingerprint),
			"startedAt": httpx.ISOms(startedAt), "updatedAt": httpx.ISOms(updatedAt),
			"finishedAt": nullTimeAny(finishedAt), "durationMs": nullIntAny(durationMs),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

/* ───────── triage 经济学 ───────── */

type triAcc struct {
	agentName                         string
	triageCount, skipCount, wakeCount int
	measuredCount, byoaCount          int
	triageCostUsd, triageOverheadUsd  float64
	turnCount                         int
	turnCostUsd                       float64
	cacheRead, totalInput             int64
}

func (s *API) HandleObsTriage(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.obsRequireDevtools(w, r)
	if !ok {
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agentId"))
	rawHours, _ := strconv.ParseFloat(r.URL.Query().Get("sinceHours"), 64)
	if r.URL.Query().Get("sinceHours") == "" {
		rawHours = 24
	}
	sinceHours := int(rawHours)
	if sinceHours < 1 {
		sinceHours = 1
	}
	if sinceHours > 720 {
		sinceHours = 720
	}
	body, err := s.buildTriageEconomics(r.Context(), tenant, agentID, sinceHours)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (s *API) buildTriageEconomics(ctx context.Context, tenant, agentID string, sinceHours int) (map[string]any, error) {
	ms := float64(sinceHours) * 3_600_000
	agentFilter := ""
	runAgentFilter := ""
	params := []any{tenant, ms}
	if agentID != "" {
		params = append(params, agentID)
		agentFilter = " AND t.agent_id = $3"
		runAgentFilter = " AND r.agent_id = $3"
	}

	type triAggRow struct {
		agentID, agentName                             string
		model                                          sql.NullString
		actionable                                     bool
		n, measuredN, byoaN                            int
		inputTok, cachedTok, cacheCreateTok, outputTok int64
	}
	triRows := []triAggRow{}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.agent_id, COALESCE(p.name, t.agent_id) AS agent_name, t.model, t.actionable,
		       count(*)::int,
		       count(*) FILTER (WHERE t.measured)::int,
		       count(*) FILTER (WHERE t.source LIKE 'byoa%')::int,
		       COALESCE(sum(t.input_tokens), 0),
		       COALESCE(sum(t.cached_input_tokens), 0),
		       COALESCE(sum(t.cache_creation_tokens), 0),
		       COALESCE(sum(t.output_tokens), 0)
		  FROM agent_triages t
		  LEFT JOIN participants p ON p.id = t.agent_id AND p.company_id = t.company_id
		 WHERE t.company_id = $1 AND t.created_at > NOW() - ($2::double precision * INTERVAL '1 millisecond')`+agentFilter+`
		 GROUP BY t.agent_id, agent_name, t.model, t.actionable`, params...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t triAggRow
		if err := rows.Scan(&t.agentID, &t.agentName, &t.model, &t.actionable,
			&t.n, &t.measuredN, &t.byoaN, &t.inputTok, &t.cachedTok, &t.cacheCreateTok, &t.outputTok); err == nil {
			triRows = append(triRows, t)
		}
	}
	rows.Close()

	type runAggRow struct {
		agentID                                        string
		model                                          sql.NullString
		turnN                                          int
		inputTok, cachedTok, cacheCreateTok, outputTok int64
	}
	runRows := []runAggRow{}
	rows, err = s.DB.QueryContext(ctx, `
		SELECT r.agent_id, r.model,
		       count(*)::int,
		       COALESCE(sum(r.input_tokens), 0),
		       COALESCE(sum(r.cached_input_tokens), 0),
		       COALESCE(sum(r.cache_creation_tokens), 0),
		       COALESCE(sum(r.output_tokens), 0)
		  FROM agent_runs r
		 WHERE r.company_id = $1 AND r.started_at > NOW() - ($2::double precision * INTERVAL '1 millisecond')`+runAgentFilter+`
		   AND (r.input_tokens + r.cached_input_tokens + r.output_tokens) > 0
		 GROUP BY r.agent_id, r.model`, params...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rr runAggRow
		if err := rows.Scan(&rr.agentID, &rr.model, &rr.turnN, &rr.inputTok, &rr.cachedTok, &rr.cacheCreateTok, &rr.outputTok); err == nil {
			runRows = append(runRows, rr)
		}
	}
	rows.Close()

	acc := map[string]*triAcc{}
	accOf := func(id, name string) *triAcc {
		a, ok := acc[id]
		if !ok {
			a = &triAcc{agentName: name}
			acc[id] = a
		} else if name != id && a.agentName == id {
			a.agentName = name
		}
		return a
	}
	modelsSeen := map[string]bool{}
	for _, t := range triRows {
		a := accOf(t.agentID, t.agentName)
		if t.model.Valid {
			modelsSeen[t.model.String] = true
		}
		cost, _ := costing.EffectiveCostUsd(t.model.String, costing.TokenUsage{
			InputTokens: t.inputTok, CachedInputTokens: t.cachedTok,
			CacheCreationTokens: t.cacheCreateTok, OutputTokens: t.outputTok,
		})
		a.triageCount += t.n
		a.measuredCount += t.measuredN
		a.byoaCount += t.byoaN
		a.triageCostUsd += cost
		if t.actionable {
			a.wakeCount += t.n
			a.triageOverheadUsd += cost
		} else {
			a.skipCount += t.n
		}
	}
	for _, rr := range runRows {
		a := accOf(rr.agentID, rr.agentID)
		if rr.model.Valid {
			modelsSeen[rr.model.String] = true
		}
		u := costing.TokenUsage{
			InputTokens: rr.inputTok, CachedInputTokens: rr.cachedTok,
			CacheCreationTokens: rr.cacheCreateTok, OutputTokens: rr.outputTok,
		}
		cost, _ := costing.EffectiveCostUsd(rr.model.String, u)
		a.turnCostUsd += cost
		a.turnCount += rr.turnN
		a.cacheRead += u.CachedInputTokens
		a.totalInput += u.InputTokens + u.CachedInputTokens
	}

	type perAgentRow struct {
		agentID, agentName                string
		triageCount, skipCount, wakeCount int
		triageCostUsd, triageOverheadUsd  float64
		turnCount                         int
		avgTurnCostUsd, turnCacheHitRate  float64
		estimatedNetSavingsUsd            float64
	}
	perAgent := []perAgentRow{}
	avgByAgent := map[string]float64{}
	for id, a := range acc {
		if a.triageCount <= 0 {
			continue
		}
		avg := 0.0
		if a.turnCount > 0 {
			avg = a.turnCostUsd / float64(a.turnCount)
		}
		avgByAgent[id] = avg
		perAgent = append(perAgent, perAgentRow{
			agentID: id, agentName: a.agentName,
			triageCount: a.triageCount, skipCount: a.skipCount, wakeCount: a.wakeCount,
			triageCostUsd: a.triageCostUsd, triageOverheadUsd: a.triageOverheadUsd,
			turnCount: a.turnCount, avgTurnCostUsd: avg,
			turnCacheHitRate:       ratio(float64(a.cacheRead), float64(a.totalInput)),
			estimatedNetSavingsUsd: float64(a.skipCount)*avg - a.triageCostUsd,
		})
	}
	// 排序:triageCount 降序(TS .sort((a,b)=>b-a))——插入序不稳定,排序后
	// 同值次序无关紧要(数值聚合,无并列断言)。
	for i := 1; i < len(perAgent); i++ {
		for j := i; j > 0 && perAgent[j].triageCount > perAgent[j-1].triageCount; j-- {
			perAgent[j], perAgent[j-1] = perAgent[j-1], perAgent[j]
		}
	}

	type recentRow struct {
		id, agentID, agentName, source                 string
		model                                          sql.NullString
		actionable                                     bool
		reason                                         sql.NullString
		inputTok, cachedTok, cacheCreateTok, outputTok int64
		measured                                       bool
		createdAt                                      time.Time
	}
	recent := []map[string]any{}
	rows, err = s.DB.QueryContext(ctx, `
		SELECT t.id, t.agent_id, COALESCE(p.name, t.agent_id), t.source, t.model,
		       t.actionable, t.reason, t.input_tokens, t.cached_input_tokens,
		       t.cache_creation_tokens, t.output_tokens, t.measured, t.created_at
		  FROM agent_triages t
		  LEFT JOIN participants p ON p.id = t.agent_id AND p.company_id = t.company_id
		 WHERE t.company_id = $1 AND t.created_at > NOW() - ($2::double precision * INTERVAL '1 millisecond')`+agentFilter+`
		 ORDER BY t.created_at DESC
		 LIMIT 200`, params...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rr recentRow
		if err := rows.Scan(&rr.id, &rr.agentID, &rr.agentName, &rr.source, &rr.model,
			&rr.actionable, &rr.reason, &rr.inputTok, &rr.cachedTok, &rr.cacheCreateTok,
			&rr.outputTok, &rr.measured, &rr.createdAt); err == nil {
			u := costing.TokenUsage{
				InputTokens: rr.inputTok, CachedInputTokens: rr.cachedTok,
				CacheCreationTokens: rr.cacheCreateTok, OutputTokens: rr.outputTok,
			}
			priced, est := costing.EffectiveCostUsd(rr.model.String, u)
			cost := 0.0
			if rr.measured {
				cost = priced
			}
			avg := avgByAgent[rr.agentID]
			var estSaving any
			if rr.actionable {
				estSaving = nil
			} else {
				estSaving = avg - cost
			}
			recent = append(recent, map[string]any{
				"id": rr.id, "agentId": rr.agentID, "agentName": rr.agentName,
				"source": rr.source, "model": nullStrAny(rr.model),
				"actionable": rr.actionable, "reason": nullStrAny(rr.reason),
				"inputTokens": rr.inputTok, "cachedInputTokens": rr.cachedTok, "outputTokens": rr.outputTok,
				"costUsd": cost, "costEstimated": est, "measured": rr.measured,
				"estSavingUsd": estSaving, "createdAt": httpx.ISOms(rr.createdAt),
			})
		}
	}
	rows.Close()

	unitPrices := []map[string]any{}
	seenUnit := map[string]bool{}
	pushUnit := func(role, model string) {
		if model == "" {
			return
		}
		key := role + ":" + model
		if seenUnit[key] {
			return
		}
		seenUnit[key] = true
		pr := costing.PriceFor(model)
		unitPrices = append(unitPrices, map[string]any{
			"role": role, "model": model,
			"inPer1M": pr.InPer1M, "cachedInPer1M": pr.CachedInPer1M, "outPer1M": pr.OutPer1M,
			"estimated": !pr.Verified,
		})
	}
	for _, t := range triRows {
		pushUnit("triage", t.model.String)
	}
	for _, rr := range runRows {
		pushUnit("turn", rr.model.String)
	}

	var triageCostUsd, estimatedAvoidedUsd float64
	var triageCount, triageSkip, triageWake, triageMeasured int
	var triageOverhead float64
	var byoaTri int
	var turnCostUsd float64
	var turnCount int
	var cacheRead, totalInput int64
	for _, a := range acc {
		turnCostUsd += a.turnCostUsd
		turnCount += a.turnCount
		cacheRead += a.cacheRead
		totalInput += a.totalInput
		if a.triageCount > 0 {
			triageCostUsd += a.triageCostUsd
			triageCount += a.triageCount
			triageSkip += a.skipCount
			triageWake += a.wakeCount
			triageMeasured += a.measuredCount
			triageOverhead += a.triageOverheadUsd
			byoaTri += a.byoaCount
			avg := 0.0
			if a.turnCount > 0 {
				avg = a.turnCostUsd / float64(a.turnCount)
			}
			estimatedAvoidedUsd += float64(a.skipCount) * avg
		}
	}
	var triageInputTok, triageCachedTok, triageOutputTok int64
	for _, t := range triRows {
		triageInputTok += t.inputTok
		triageCachedTok += t.cachedTok
		triageOutputTok += t.outputTok
	}
	costEstimated := false
	for m := range modelsSeen {
		if !costing.PriceFor(m).Verified {
			costEstimated = true
		}
	}
	perAgentAny := make([]map[string]any, 0, len(perAgent))
	for _, p := range perAgent {
		perAgentAny = append(perAgentAny, map[string]any{
			"agentId": p.agentID, "agentName": p.agentName,
			"triageCount": p.triageCount, "skipCount": p.skipCount, "wakeCount": p.wakeCount,
			"triageCostUsd": p.triageCostUsd, "triageOverheadUsd": p.triageOverheadUsd,
			"turnCount": p.turnCount, "avgTurnCostUsd": p.avgTurnCostUsd,
			"turnCacheHitRate":       p.turnCacheHitRate,
			"estimatedNetSavingsUsd": p.estimatedNetSavingsUsd,
		})
	}
	return map[string]any{
		"sinceHours":  sinceHours,
		"triageCount": triageCount, "triageSkipCount": triageSkip, "triageWakeCount": triageWake,
		"triageMeasuredCount": triageMeasured, "triageCostUsd": triageCostUsd,
		"triageOverheadUsd": triageOverhead,
		"triageInputTokens": triageInputTok, "triageCachedInputTokens": triageCachedTok,
		"triageOutputTokens": triageOutputTok,
		"turnCount":          turnCount, "turnCostUsd": turnCostUsd,
		"avgTurnCostUsd":         ratio(turnCostUsd, float64(turnCount)),
		"turnCacheHitRate":       ratio(float64(cacheRead), float64(totalInput)),
		"estimatedAvoidedUsd":    estimatedAvoidedUsd,
		"estimatedNetSavingsUsd": estimatedAvoidedUsd - triageCostUsd,
		"costEstimated":          costEstimated,
		"byoaShare":              ratio(float64(byoaTri), float64(triageCount)),
		"unitPrices":             unitPrices,
		"priceTable":             costing.ModelPriceTableRows(),
		"perAgent":               perAgentAny,
		"recent":                 recent,
	}, nil
}

/* ───────── llm-spend rollup ───────── */

func (s *API) HandleObsSpend(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.obsRequireDevtools(w, r)
	if !ok {
		return
	}
	rawDays, _ := strconv.ParseFloat(r.URL.Query().Get("sinceDays"), 64)
	if r.URL.Query().Get("sinceDays") == "" {
		rawDays = 30
	}
	sinceDays := int(rawDays)
	if sinceDays < 1 {
		sinceDays = 1
	}
	if sinceDays > 365 {
		sinceDays = 365
	}
	modelFilter := strings.TrimSpace(r.URL.Query().Get("model"))

	params := []any{sinceDays}
	where := "bucket_hour > NOW() - ($1::int * INTERVAL '1 day')"
	if tenant != "" {
		params = append(params, tenant)
		where += " AND company_id = $" + strconv.Itoa(len(params))
	}
	if modelFilter != "" {
		params = append(params, "%"+modelFilter+"%")
		where += " AND model ILIKE $" + strconv.Itoa(len(params))
	}
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT
		   purpose, model, source,
		   SUM(calls), SUM(ok_calls), SUM(failed_calls), SUM(rate_limited_calls),
		   SUM(input_tokens), SUM(cached_input_tokens), SUM(cache_creation_tokens),
		   SUM(output_tokens), SUM(reasoning_tokens),
		   COALESCE(SUM(cost_usd), 0), BOOL_OR(cost_estimated)
		 FROM llm_calls_rollup
		 WHERE `+where+`
		 GROUP BY purpose, model, source
		 ORDER BY SUM(cost_usd) DESC`, params...)
	if err != nil {
		httpx.WriteInternalError(w, r, err)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var purpose, model, source string
		var calls, okCalls, failedCalls, rateLimited int64
		var inputTok, cachedTok, cacheCreateTok, outputTok, reasoningTok int64
		var costUsd float64
		var costEstimated bool
		if err := rows.Scan(&purpose, &model, &source, &calls, &okCalls, &failedCalls, &rateLimited,
			&inputTok, &cachedTok, &cacheCreateTok, &outputTok, &reasoningTok, &costUsd, &costEstimated); err == nil {
			price := costing.PriceFor(model)
			gap := price.InPer1M - price.CachedInPer1M
			if gap < 0 {
				gap = 0
			}
			out = append(out, map[string]any{
				"purpose": purpose, "model": model, "source": source,
				"calls": calls, "okCalls": okCalls, "failedCalls": failedCalls,
				"rateLimitedCalls": rateLimited,
				"inputTokens":      inputTok, "cachedInputTokens": cachedTok,
				"cacheCreationTokens": cacheCreateTok, "outputTokens": outputTok,
				"reasoningTokens": reasoningTok,
				"costUsd":         costUsd, "costEstimated": costEstimated,
				"savableUsd": float64(inputTok) * gap / 1_000_000,
			})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

/* ───────── 小助手 ───────── */

func ratio(num, den float64) float64 {
	if den <= 0 {
		return 0
	}
	return num / den
}

func nullIntAny(ni sql.NullInt64) any {
	if !ni.Valid {
		return nil
	}
	return ni.Int64
}

func nullTimeAny(nt sql.NullTime) any {
	if !nt.Valid {
		return nil
	}
	return httpx.ISOms(nt.Time)
}

func jsonUnmarshalToAny(b []byte, v *any) error {
	if b == nil {
		return nil
	}
	return json.Unmarshal(b, v) // 自 runtime/agenda.go 的同名包装平移
}

// nullStrAny:sql.NullString → JSON any(runtime/cli_calendar.go 同名助手
// 的本包副本;#141 横切统一票再议合并)。
func nullStrAny(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}
