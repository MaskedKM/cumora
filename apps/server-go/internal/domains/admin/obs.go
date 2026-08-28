// domains/admin/obs —— #124(#117-e):LLM 观察面(admin)。GET
// /observability/llm = 六查询扇出 + 汇总派生(topPurpose/savableUsd 从
// rollup 免二次扫表);GET /observability/llm/calls = 桶/运行/agent 钻
// 取原始行。30s 进程内 TTL 缓存吸收面板每次过滤切换的全量重取;fresh=1
// 跳过缓存但仍回填。SQL 逐段对齐 agents/llm-ledger.ts(读预聚合
// llm_calls_rollup,不扫原始表)。对齐 api/admin-router.ts 407–496。
package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/runtime"
)

/* ───────── 30s TTL 聚合缓存(admin-router.ts cachedAgg 同型) ───────── */

type aggCacheEntry struct {
	at    time.Time
	ttl   time.Duration
	value any
}

var (
	aggCacheMu sync.Mutex
	aggCache   = map[string]aggCacheEntry{}
)

// cachedAgg:force(手动/自动刷新)跳过缓存但仍回填 —— 随后的常规加载
// 依然温热。键低基数(sinceDays×model×租户组合个位数),自过期不膨胀。
func cachedAgg(key string, ttl time.Duration, force bool, compute func() (any, error)) (any, error) {
	aggCacheMu.Lock()
	if hit, ok := aggCache[key]; ok && !force && time.Since(hit.at) < hit.ttl {
		v := hit.value
		aggCacheMu.Unlock()
		return v, nil
	}
	aggCacheMu.Unlock()
	v, err := compute()
	if err != nil {
		return nil, err
	}
	aggCacheMu.Lock()
	aggCache[key] = aggCacheEntry{at: time.Now(), ttl: ttl, value: v}
	aggCacheMu.Unlock()
	return v, nil
}

/* ───────── 查询参数小件 ───────── */

// obsSinceDays:1..365,默认 30(非数落默认)。
func obsSinceDays(raw string) int {
	days := int(numOrDefault(raw, 30))
	return int(math.Min(365, math.Max(1, float64(days))))
}

func trimmedNonEmpty(raw string) string {
	t := strings.TrimSpace(raw)
	return t
}

/* ───────── 聚合(llm-ledger.ts 同 SQL) ───────── */

func obsLlmRollup(db *sql.DB, sinceDays int, model, companyFilter string) ([]map[string]any, error) {
	where := `bucket_hour > NOW() - ($1::int * INTERVAL '1 day')`
	params := []any{sinceDays}
	if companyFilter != "" {
		params = append(params, companyFilter)
		where += fmt.Sprintf(` AND company_id = $%d`, len(params))
	}
	if model != "" {
		params = append(params, "%"+model+"%")
		where += fmt.Sprintf(` AND model ILIKE $%d`, len(params))
	}
	rows, err := db.Query(`
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
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var purpose, mdl, source string
		var calls, ok, failed, rateLimited, inTok, cachedIn, cacheCre, outTok, reasonTok int64
		var costUsd float64
		var costEstimated bool
		if rows.Scan(&purpose, &mdl, &source, &calls, &ok, &failed, &rateLimited,
			&inTok, &cachedIn, &cacheCre, &outTok, &reasonTok, &costUsd, &costEstimated) != nil {
			continue
		}
		// savable $ = 未命中输入 ×(全价 − 缓存价);价未核验则 savable
		// 同为估计(image 行 inTok=0 → 0)。
		price := runtime.PriceFor(mdl)
		gap := math.Max(0, price.InPer1M-price.CachedInPer1M)
		out = append(out, map[string]any{
			"purpose": purpose, "model": mdl, "source": source,
			"calls": calls, "okCalls": ok, "failedCalls": failed, "rateLimitedCalls": rateLimited,
			"inputTokens": inTok, "cachedInputTokens": cachedIn, "cacheCreationTokens": cacheCre,
			"outputTokens": outTok, "reasoningTokens": reasonTok,
			"costUsd": costUsd, "costEstimated": costEstimated,
			"savableUsd": float64(inTok) * gap / 1_000_000,
		})
	}
	return out, rows.Err()
}

func obsLlmSummary(db *sql.DB, sinceDays int, companyFilter string) (map[string]any, error) {
	scope := ""
	params := []any{sinceDays}
	if companyFilter != "" {
		params = append(params, companyFilter)
		scope = fmt.Sprintf(`AND company_id = $%d`, len(params))
	}
	// GROUPING SETS 一趟:总计行(is_total=1)+ 逐区行(计活跃租户数);
	// GROUPING() 区分超聚合 NULL 与真 NULL(personal-key)。
	rows, err := db.Query(`
		SELECT GROUPING(company_id) AS is_total, company_id,
		       SUM(calls), COALESCE(SUM(cost_usd), 0),
		       SUM(input_tokens), SUM(cached_input_tokens), SUM(output_tokens),
		       SUM(failed_calls), SUM(rate_limited_calls)
		  FROM llm_calls_rollup
		 WHERE bucket_hour > NOW() - ($1::int * INTERVAL '1 day') `+scope+`
		 GROUP BY GROUPING SETS ((), (company_id))`, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var totalCalls, totalIn, totalCachedIn, totalOut, failed, rateLimited int64
	var totalCost float64
	activeTenants := 0
	foundTotal := false
	for rows.Next() {
		var isTotal int
		var companyID sql.NullString
		var calls, inTok, cachedIn, outTok, failN, rateN int64
		var cost float64
		if rows.Scan(&isTotal, &companyID, &calls, &cost, &inTok, &cachedIn, &outTok, &failN, &rateN) != nil {
			continue
		}
		if isTotal == 1 {
			totalCalls, totalCost = calls, cost
			totalIn, totalCachedIn, totalOut = inTok, cachedIn, outTok
			failed, rateLimited = failN, rateN
			foundTotal = true
		} else if companyID.Valid {
			activeTenants++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = foundTotal // 空表 → 全零行,TS Number(undefined ?? 0) 同型
	var failureRate float64
	if totalCalls > 0 {
		failureRate = float64(failed) / float64(totalCalls)
	}
	var cacheHitRate any // null 当分母为 0
	if totalIn+totalCachedIn > 0 {
		cacheHitRate = float64(totalCachedIn) / float64(totalIn+totalCachedIn)
	}
	return map[string]any{
		"sinceDays": sinceDays, "totalCalls": totalCalls, "totalCostUsd": totalCost,
		"totalInputTokens": totalIn, "totalCachedInputTokens": totalCachedIn,
		"totalOutputTokens": totalOut, "failureRate": failureRate,
		"rateLimitedCalls": rateLimited, "activeTenants": activeTenants,
		"cacheHitRate": cacheHitRate,
	}, nil
}

func obsLlmTrend(db *sql.DB, sinceDays int, companyFilter string) ([]map[string]any, error) {
	scope := ""
	params := []any{sinceDays}
	if companyFilter != "" {
		params = append(params, companyFilter)
		scope = fmt.Sprintf(`AND company_id = $%d`, len(params))
	}
	rows, err := db.Query(`
		SELECT to_char(date_trunc('day', bucket_hour AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS day,
		       purpose, COALESCE(SUM(cost_usd), 0), SUM(calls),
		       SUM(input_tokens), SUM(cached_input_tokens)
		  FROM llm_calls_rollup
		 WHERE bucket_hour > NOW() - ($1::int * INTERVAL '1 day') `+scope+`
		 GROUP BY day, purpose
		 ORDER BY day ASC, SUM(cost_usd) DESC`, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var day, purpose string
		var cost float64
		var calls, inTok, cachedIn int64
		if rows.Scan(&day, &purpose, &cost, &calls, &inTok, &cachedIn) != nil {
			continue
		}
		out = append(out, map[string]any{
			"day": day, "purpose": purpose, "costUsd": cost, "calls": calls,
			"inputTokens": inTok, "cachedInputTokens": cachedIn,
		})
	}
	return out, rows.Err()
}

func obsLlmTenants(db *sql.DB, sinceDays, limit int) ([]map[string]any, error) {
	rows, err := db.Query(`
		SELECT l.company_id, c.name, c.slug,
		       COALESCE(SUM(l.cost_usd), 0), SUM(l.calls),
		       SUM(l.input_tokens), SUM(l.cached_input_tokens), SUM(l.output_tokens)
		  FROM llm_calls_rollup l LEFT JOIN companies c ON c.id = l.company_id
		 WHERE l.bucket_hour > NOW() - ($1::int * INTERVAL '1 day')
		   AND l.company_id IS NOT NULL
		 GROUP BY l.company_id, c.name, c.slug
		 ORDER BY SUM(l.cost_usd) DESC NULLS LAST
		 LIMIT $2`, sinceDays, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var companyID, name, slug sql.NullString
		var cost float64
		var calls, inTok, cachedIn, outTok int64
		if rows.Scan(&companyID, &name, &slug, &cost, &calls, &inTok, &cachedIn, &outTok) != nil {
			continue
		}
		out = append(out, map[string]any{
			"companyId": companyID.String, "name": nullStrAny(name), "slug": nullStrAny(slug),
			"costUsd": cost, "calls": calls,
			"inputTokens": inTok, "cachedInputTokens": cachedIn, "outputTokens": outTok,
		})
	}
	return out, rows.Err()
}

func obsLlmTopAgents(db *sql.DB, sinceDays, limit int, companyFilter string) ([]map[string]any, error) {
	scope := ""
	params := []any{sinceDays}
	if companyFilter != "" {
		params = append(params, companyFilter)
		scope = fmt.Sprintf(`AND l.company_id = $%d`, len(params))
	}
	params = append(params, limit)
	rows, err := db.Query(`
		SELECT l.agent_id, l.company_id,
		       p.name, p.avatar_url, p.avatar_bg, p.initial, c.name,
		       COALESCE(SUM(l.cost_usd), 0), SUM(l.calls),
		       SUM(l.input_tokens), SUM(l.cached_input_tokens), SUM(l.output_tokens)
		  FROM llm_calls_rollup l
		  LEFT JOIN participants p
		         ON p.id = l.agent_id
		        AND (p.company_id = l.company_id OR (p.company_id IS NULL AND l.company_id IS NULL))
		  LEFT JOIN companies c ON c.id = l.company_id
		 WHERE l.bucket_hour > NOW() - ($1::int * INTERVAL '1 day') `+scope+`
		 GROUP BY l.agent_id, l.company_id, p.name, p.avatar_url, p.avatar_bg, p.initial, c.name
		 ORDER BY SUM(l.cost_usd) DESC NULLS LAST
		 LIMIT $`+fmt.Sprint(len(params)), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var agentID, companyID, agentName, avatarURL, avatarBg, initial, companyName sql.NullString
		var cost float64
		var calls, inTok, cachedIn, outTok int64
		if rows.Scan(&agentID, &companyID, &agentName, &avatarURL, &avatarBg, &initial, &companyName,
			&cost, &calls, &inTok, &cachedIn, &outTok) != nil {
			continue
		}
		out = append(out, map[string]any{
			"agentId": nullStrAny(agentID), "companyId": nullStrAny(companyID),
			"agentName": nullStrAny(agentName), "agentAvatarUrl": nullStrAny(avatarURL),
			"agentAvatarBg": nullStrAny(avatarBg), "agentInitial": nullStrAny(initial),
			"companyName": nullStrAny(companyName),
			"costUsd": cost, "calls": calls,
			"inputTokens": inTok, "cachedInputTokens": cachedIn, "outputTokens": outTok,
		})
	}
	return out, rows.Err()
}

func obsLlmDaemonVersions(db *sql.DB, sinceDays int, companyFilter string) ([]map[string]any, error) {
	scope := ""
	params := []any{sinceDays}
	if companyFilter != "" {
		params = append(params, companyFilter)
		scope = fmt.Sprintf(`AND company_id = $%d`, len(params))
	}
	rows, err := db.Query(`
		SELECT daemon_version, source,
		       SUM(calls), COALESCE(SUM(cost_usd), 0),
		       SUM(input_tokens), SUM(cached_input_tokens), SUM(output_tokens),
		       SUM(ok_calls), MIN(bucket_hour), MAX(bucket_hour)
		  FROM llm_calls_rollup
		 WHERE bucket_hour > NOW() - ($1::int * INTERVAL '1 day') `+scope+`
		   AND daemon_version IS NOT NULL
		 GROUP BY daemon_version, source
		 ORDER BY MAX(bucket_hour) DESC, SUM(cost_usd) DESC`, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var version, source string
		var calls, inTok, cachedIn, outTok, ok int64
		var cost float64
		var firstSeen, lastSeen time.Time
		if rows.Scan(&version, &source, &calls, &cost, &inTok, &cachedIn, &outTok, &ok, &firstSeen, &lastSeen) != nil {
			continue
		}
		var failureRate float64
		if calls > 0 {
			failureRate = float64(calls-ok) / float64(calls)
		}
		out = append(out, map[string]any{
			"daemonVersion": version, "source": source,
			"calls": calls, "costUsd": cost,
			"inputTokens": inTok, "cachedInputTokens": cachedIn, "outputTokens": outTok,
			"failureRate": failureRate,
			"firstSeen":   isoTime(firstSeen), "lastSeen": isoTime(lastSeen),
		})
	}
	return out, rows.Err()
}

/* ───────── HTTP 面 ───────── */

// llmObservability:GET /api/admin/observability/llm —— 一次挂载取全
// 页四形;summary 的 topPurpose/savableUsd 从已取 rollup 派生(不另扫
// 表,且随 model 过滤联动)。tenants 恒全局(选择器要全量租户清单)。
func llmObservability(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		sinceDays := obsSinceDays(r.URL.Query().Get("sinceDays"))
		model := trimmedNonEmpty(r.URL.Query().Get("model"))
		companyFilter := trimmedNonEmpty(r.URL.Query().Get("companyId"))
		fresh := r.URL.Query().Get("fresh") == "1" || r.URL.Query().Get("fresh") == "true"

		cacheKey := fmt.Sprintf("obs-llm|%d|%s|%s", sinceDays, model, companyFilterOr(companyFilter))
		payload, err := cachedAgg(cacheKey, 30*time.Second, fresh, func() (any, error) {
			rollup, err := obsLlmRollup(db, sinceDays, model, companyFilter)
			if err != nil {
				return nil, err
			}
			summary, err := obsLlmSummary(db, sinceDays, companyFilter)
			if err != nil {
				return nil, err
			}
			trend, err := obsLlmTrend(db, sinceDays, companyFilter)
			if err != nil {
				return nil, err
			}
			topAgents, err := obsLlmTopAgents(db, sinceDays, 20, companyFilter)
			if err != nil {
				return nil, err
			}
			tenants, err := obsLlmTenants(db, sinceDays, 200)
			if err != nil {
				return nil, err
			}
			daemonVersions, err := obsLlmDaemonVersions(db, sinceDays, companyFilter)
			if err != nil {
				return nil, err
			}
			// 派生 topPurpose / savableUsd(映射保插入序,首最大者胜出
			// 同 TS for-of 语义)。
			costByPurpose := map[string]float64{}
			var order []string
			savableUsd := 0.0
			for _, row := range rollup {
				purpose, _ := row["purpose"].(string)
				cost, _ := row["costUsd"].(float64)
				if _, seen := costByPurpose[purpose]; !seen {
					order = append(order, purpose)
				}
				costByPurpose[purpose] += cost
				if sv, ok := row["savableUsd"].(float64); ok {
					savableUsd += sv
				}
			}
			var topPurpose any
			for _, p := range order {
				c := costByPurpose[p]
				if topPurpose == nil {
					topPurpose = map[string]any{"purpose": p, "costUsd": c}
					continue
				}
				if prev, ok := topPurpose.(map[string]any); ok {
					if prevCost, _ := prev["costUsd"].(float64); c > prevCost {
						topPurpose = map[string]any{"purpose": p, "costUsd": c}
					}
				}
			}
			summary["topPurpose"] = topPurpose
			summary["savableUsd"] = savableUsd
			return map[string]any{
				"summary": summary, "rollup": rollup, "trend": trend,
				"topAgents": topAgents, "tenants": tenants, "daemonVersions": daemonVersions,
			}, nil
		})
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, payload)
	}
}

func companyFilterOr(f string) string {
	if f == "" {
		return "*"
	}
	return f
}

// llmCallsDrilldown:GET /api/admin/observability/llm/calls —— 桶(purpose
// /model/source)或跨桶(runId/agentId)钻取;run/agent 路径默认时间正
// 序,桶路径默认花费倒序。
func llmCallsDrilldown(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		q := r.URL.Query()
		sinceDays := obsSinceDays(q.Get("sinceDays"))
		limit := int(math.Min(200, math.Max(1, numOrDefault(q.Get("limit"), 50))))
		purpose := trimmedNonEmpty(q.Get("purpose"))
		model := trimmedNonEmpty(q.Get("model"))
		source := trimmedNonEmpty(q.Get("source"))
		runID := trimmedNonEmpty(q.Get("runId"))
		agentID := trimmedNonEmpty(q.Get("agentId"))
		companyFilter := trimmedNonEmpty(q.Get("companyId"))
		sortBy := ""
		switch q.Get("sortBy") {
		case "cost", "latency", "hop", "created":
			sortBy = q.Get("sortBy")
		}

		params := []any{sinceDays}
		where := []string{`l.created_at > NOW() - ($1::int * INTERVAL '1 day')`}
		add := func(clause string, v any) {
			params = append(params, v)
			where = append(where, strings.Replace(clause, "$$", fmt.Sprintf("$%d", len(params)), 1))
		}
		if purpose != "" {
			add(`l.purpose = $$`, purpose)
		}
		if model != "" {
			add(`l.model = $$`, model)
		}
		if source != "" {
			add(`l.source = $$`, source)
		}
		if runID != "" {
			add(`l.run_id = $$`, runID)
		}
		if agentID != "" {
			add(`l.agent_id = $$`, agentID)
		}
		if companyFilter != "" {
			add(`l.company_id = $$`, companyFilter)
		}
		if sortBy == "" {
			if runID != "" || agentID != "" {
				sortBy = "created"
			} else {
				sortBy = "cost"
			}
		}
		orderBy := map[string]string{
			"cost":    `l.cost_usd DESC NULLS LAST`,
			"latency": `l.latency_ms DESC NULLS LAST`,
			"hop":     `(l.extras->>'hopIndex')::int ASC NULLS LAST, l.created_at ASC`,
			"created": `l.created_at ASC`,
		}[sortBy]
		params = append(params, limit)
		rows, err := db.Query(`
			SELECT l.id, l.created_at, l.company_id, l.agent_id, p.name,
			       l.run_id, l.conversation_id, l.purpose, l.source, l.model,
			       l.input_tokens, l.cached_input_tokens, l.cache_creation_tokens,
			       l.output_tokens, l.reasoning_tokens,
			       l.cost_usd, l.cost_estimated, l.measured,
			       l.latency_ms, l.status, l.error, l.extras, l.daemon_version
			  FROM llm_calls l
			  LEFT JOIN participants p
			         ON p.id = l.agent_id
			        AND (p.company_id = l.company_id OR (p.company_id IS NULL AND l.company_id IS NULL))
			 WHERE `+strings.Join(where, " AND ")+`
			 ORDER BY `+orderBy+`
			 LIMIT $`+fmt.Sprint(len(params)), params...)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, createdAt string
			var companyID, agentID, agentName, runID, convoID sql.NullString
			var purpose, source, mdl string
			var inTok, cachedIn, cacheCre, outTok, reasonTok int64
			var costUsd float64
			var costEstimated, measured bool
			var latency sql.NullInt64
			var status string
			var errStr sql.NullString
			var extras sql.NullString
			var daemonVersion sql.NullString
			if rows.Scan(&id, &createdAt, &companyID, &agentID, &agentName, &runID, &convoID,
				&purpose, &source, &mdl, &inTok, &cachedIn, &cacheCre, &outTok, &reasonTok,
				&costUsd, &costEstimated, &measured, &latency, &status, &errStr, &extras, &daemonVersion) != nil {
				continue
			}
			var extrasWire any
			if extras.Valid && extras.String != "" {
				var v any
				if json.Unmarshal([]byte(extras.String), &v) == nil {
					extrasWire = v
				} else {
					extrasWire = map[string]any{}
				}
			}
			out = append(out, map[string]any{
				"id": id, "createdAt": createdAt,
				"companyId": nullStrAny(companyID), "agentId": nullStrAny(agentID), "agentName": nullStrAny(agentName),
				"runId": nullStrAny(runID), "conversationId": nullStrAny(convoID),
				"purpose": purpose, "source": source, "model": mdl,
				"inputTokens": inTok, "cachedInputTokens": cachedIn, "cacheCreationTokens": cacheCre,
				"outputTokens": outTok, "reasoningTokens": reasonTok,
				"costUsd": costUsd, "costEstimated": costEstimated, "measured": measured,
				"latencyMs": nullIntAny(latency), "status": status, "error": nullStrAny(errStr),
				"extras": extrasWire, "daemonVersion": nullStrAny(daemonVersion),
			})
		}
		if err := rows.Err(); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

func nullIntAny(ni sql.NullInt64) any {
	if !ni.Valid {
		return nil
	}
	return ni.Int64
}
