// llm_ledger —— /api/admin/observability/llm(+/calls)(#124):全局 LLM
// 观测面,逐段对齐 server/src/agents/llm-ledger.ts 的六个聚合查询
// (summary/rollup/trend/topAgents/tenants/daemonVersions + calls 明细)
// 与 admin-router.ts 的端点组装(30s 响应缓存、fresh=1 跳过但仍回填、
// topPurpose/savableUsd 从 rollup 派生不加表扫)。读预聚合
// llm_calls_rollup(~30k 小时桶)而非原始 llm_calls(~470k 行)。
// savableUsd 用 runtime.PriceFor 的模型价差——价格表不复制(漂移=错价)。
package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/MaskedKM/cumora/apps/server-go/internal/runtime"
)

/* ───────────── 30s 响应缓存(admin-router.ts cachedAgg) ───────────── */

type aggEntry struct {
	at    time.Time
	ttl   time.Duration
	value any
}

var (
	aggMu    sync.Mutex
	aggCache = map[string]aggEntry{}
)

// cachedAgg:观测台每次滤镜切换都会整扇重拉,同一聚合几秒内反复重算;
// 短 TTL 把重复塌成一次 DB 往返(开销面板不需要秒级新鲜度)。键低基数
// (sinceDays × model × 租户的组合),到期自清,无界风险为零。fresh
// (手动/自动刷新)跳过缓存但照常回填。
func cachedAgg(key string, ttl time.Duration, force bool, compute func() (any, error)) (any, error) {
	aggMu.Lock()
	hit := aggCache[key]
	aggMu.Unlock()
	if !force && time.Since(hit.at) < hit.ttl {
		return hit.value, nil
	}
	v, err := compute()
	if err != nil {
		return nil, err
	}
	aggMu.Lock()
	aggCache[key] = aggEntry{at: time.Now(), ttl: ttl, value: v}
	aggMu.Unlock()
	return v, nil
}

/* ───────────── 参数语义(TS Number()/?? 精确复刻) ───────────── */

// sinceDays:缺参 → 30;空串 → Number(”)=0 → clamp 1;坏值 NaN → 30。
func sinceDaysParam(q url.Values) int {
	n := 30
	if q.Has("sinceDays") {
		if s := q.Get("sinceDays"); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				n = v
			}
		} else {
			n = 0
		}
	}
	return clampInt(n, 1, 365)
}

// callsLimit:TS `Math.max(1, Math.min(200, isFinite(raw) ? raw : 50))`
// —— 空串是 finite 的 0 → 收到 1;"abc" → 50;缺参 → 50。
func callsLimitParam(q url.Values) int {
	n := 50
	if q.Has("limit") {
		if s := q.Get("limit"); s != "" {
			if v, err := strconv.Atoi(s); err == nil {
				n = v
			}
		} else {
			n = 0
		}
	}
	return clampInt(n, 1, 200)
}

// trimParam:非空 trim 才算有值(TS `typeof q === 'string' && q.trim()`)。
func trimParam(q url.Values, k string) string {
	s := strings.TrimSpace(q.Get(k))
	return s
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func atof64(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

/* ───────────── 六查询(llm-ledger.ts 逐字对齐) ───────────── */

type llmRollupRow struct {
	purpose, model, source       string
	calls, okCalls, failed       string
	rateLimited                  string
	input, cached, cacheCreation string
	output, reasoning            string
	costUsd                      string
	costEstimated                bool
}

func (r llmRollupRow) toMap() map[string]any {
	inputTokens := atoi64(r.input)
	price := runtime.PriceFor(r.model)
	gap := price.InPer1M - price.CachedInPer1M
	if gap < 0 {
		gap = 0
	}
	return map[string]any{
		"purpose": r.purpose, "model": r.model, "source": r.source,
		"calls": atoi64(r.calls), "okCalls": atoi64(r.okCalls), "failedCalls": atoi64(r.failed),
		"rateLimitedCalls": atoi64(r.rateLimited),
		"inputTokens":      inputTokens, "cachedInputTokens": atoi64(r.cached),
		"cacheCreationTokens": atoi64(r.cacheCreation),
		"outputTokens":        atoi64(r.output), "reasoningTokens": atoi64(r.reasoning),
		"costUsd": atof64(r.costUsd), "costEstimated": r.costEstimated,
		// 上界口径:全部输入命中缓存能省多少(冷单发做不到,但这是唯一
		// 诚实的"桌上之钱"信号);图片行 input=0 → 0。
		"savableUsd": float64(inputTokens) * gap / 1_000_000,
	}
}

func getLlmSpendRollup(db *sql.DB, sinceDays int, companyFilter string, hasCompany bool, model string) ([]map[string]any, error) {
	params := []any{sinceDays}
	where := `bucket_hour > NOW() - ($1::int * INTERVAL '1 day')`
	if hasCompany {
		params = append(params, companyFilter)
		where += ` AND company_id = $` + strconv.Itoa(len(params))
	}
	if model != "" {
		params = append(params, "%"+model+"%")
		where += ` AND model ILIKE $` + strconv.Itoa(len(params))
	}
	rows, err := db.Query(`
		SELECT
		   purpose, model, source,
		   SUM(calls)::text                                             AS calls,
		   SUM(ok_calls)::text                                          AS ok_calls,
		   SUM(failed_calls)::text                                      AS failed_calls,
		   SUM(rate_limited_calls)::text                                AS rate_limited_calls,
		   SUM(input_tokens)::text                                      AS input_tokens,
		   SUM(cached_input_tokens)::text                               AS cached_input_tokens,
		   SUM(cache_creation_tokens)::text                             AS cache_creation_tokens,
		   SUM(output_tokens)::text                                     AS output_tokens,
		   SUM(reasoning_tokens)::text                                  AS reasoning_tokens,
		   COALESCE(SUM(cost_usd), 0)::text                             AS cost_usd,
		   BOOL_OR(cost_estimated)                                      AS cost_estimated
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
		var r llmRollupRow
		if rows.Scan(&r.purpose, &r.model, &r.source, &r.calls, &r.okCalls, &r.failed,
			&r.rateLimited, &r.input, &r.cached, &r.cacheCreation, &r.output, &r.reasoning,
			&r.costUsd, &r.costEstimated) != nil {
			continue
		}
		out = append(out, r.toMap())
	}
	return out, rows.Err()
}

// getLlmSummary:GROUPING SETS 一趟出总计+活跃租户数(个人键调用不得
// 冒充租户——GROUPING() 区分超聚合 NULL 与真 NULL company_id)。
// 回 map 少 topPurpose/savableUsd 两个派生键(端点从 rollup 派生)。
func getLlmSummary(db *sql.DB, sinceDays int, companyFilter string, hasCompany bool) (map[string]any, error) {
	params := []any{sinceDays}
	scope := ""
	if hasCompany {
		params = append(params, companyFilter)
		scope = `AND company_id = $` + strconv.Itoa(len(params))
	}
	rows, err := db.Query(`
		SELECT GROUPING(company_id)                AS is_total,
		        company_id,
		        SUM(calls)::text                    AS calls,
		        COALESCE(SUM(cost_usd), 0)::text    AS cost_usd,
		        SUM(input_tokens)::text             AS input_tokens,
		        SUM(cached_input_tokens)::text      AS cached_input_tokens,
		        SUM(output_tokens)::text            AS output_tokens,
		        SUM(failed_calls)::text             AS failed_calls,
		        SUM(rate_limited_calls)::text       AS rate_limited_calls
		   FROM llm_calls_rollup
		  WHERE bucket_hour > NOW() - ($1::int * INTERVAL '1 day') `+scope+`
		  GROUP BY GROUPING SETS ((), (company_id))`, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type sumRow struct {
		isTotal               int
		companyID             sql.NullString
		calls, costUsd        string
		input, cached, output string
		failed, rateLimited   string
	}
	var total *sumRow
	activeTenants := 0
	for rows.Next() {
		var s sumRow
		if rows.Scan(&s.isTotal, &s.companyID, &s.calls, &s.costUsd, &s.input, &s.cached,
			&s.output, &s.failed, &s.rateLimited) != nil {
			continue
		}
		if s.isTotal == 1 {
			cp := s
			total = &cp
		} else if s.companyID.Valid {
			activeTenants++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	t := sumRow{}
	if total != nil {
		t = *total
	}
	totalCalls := atoi64(t.calls)
	totalInput := atoi64(t.input)
	totalCached := atoi64(t.cached)
	denom := totalInput + totalCached
	cacheHitRate := any(nil)
	if denom > 0 {
		cacheHitRate = float64(totalCached) / float64(denom)
	}
	failureRate := 0.0
	if totalCalls > 0 {
		failureRate = float64(atoi64(t.failed)) / float64(totalCalls)
	}
	return map[string]any{
		"sinceDays": sinceDays, "totalCalls": totalCalls,
		"totalCostUsd":     atof64(t.costUsd),
		"totalInputTokens": totalInput, "totalCachedInputTokens": totalCached,
		"totalOutputTokens": atoi64(t.output),
		"failureRate":       failureRate, "rateLimitedCalls": atoi64(t.rateLimited),
		"activeTenants": activeTenants, "cacheHitRate": cacheHitRate,
	}, nil
}

func getLlmDailyTrend(db *sql.DB, sinceDays int, companyFilter string, hasCompany bool) ([]map[string]any, error) {
	params := []any{sinceDays}
	scope := ""
	if hasCompany {
		params = append(params, companyFilter)
		scope = `AND company_id = $` + strconv.Itoa(len(params))
	}
	rows, err := db.Query(`
		SELECT to_char(date_trunc('day', bucket_hour AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS day,
		        purpose,
		        COALESCE(SUM(cost_usd), 0)::text             AS cost_usd,
		        SUM(calls)::text                             AS calls,
		        SUM(input_tokens)::text                      AS input_tokens,
		        SUM(cached_input_tokens)::text               AS cached_input_tokens
		   FROM llm_calls_rollup
		  WHERE bucket_hour > NOW() - ($1::int * INTERVAL '1 day') `+scope+`
		  GROUP BY day, purpose
		  ORDER BY day ASC, cost_usd DESC`, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var day, purpose, costUsd, calls, input, cached string
		if rows.Scan(&day, &purpose, &costUsd, &calls, &input, &cached) != nil {
			continue
		}
		out = append(out, map[string]any{
			"day": day, "purpose": purpose, "costUsd": atof64(costUsd),
			"calls": atoi64(calls), "inputTokens": atoi64(input), "cachedInputTokens": atoi64(cached),
		})
	}
	return out, rows.Err()
}

// getLlmTenants:租户选择器的数据源(全局,不吃 companyFilter——选择器
// 永远要能列出所有活跃租户)。
func getLlmTenants(db *sql.DB, sinceDays int, limit int) ([]map[string]any, error) {
	rows, err := db.Query(`
		SELECT l.company_id,
		        c.name, c.slug,
		        COALESCE(SUM(l.cost_usd), 0)::text             AS cost_usd,
		        SUM(l.calls)::text                             AS calls,
		        SUM(l.input_tokens)::text                      AS input_tokens,
		        SUM(l.cached_input_tokens)::text               AS cached_input_tokens,
		        SUM(l.output_tokens)::text                     AS output_tokens
		   FROM llm_calls_rollup l
		   LEFT JOIN companies c ON c.id = l.company_id
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
		var companyID string
		var name, slug sql.NullString
		var costUsd, calls, input, cached, output string
		if rows.Scan(&companyID, &name, &slug, &costUsd, &calls, &input, &cached, &output) != nil {
			continue
		}
		out = append(out, map[string]any{
			"companyId": companyID, "name": nullStrAny(name), "slug": nullStrAny(slug),
			"costUsd": atof64(costUsd), "calls": atoi64(calls),
			"inputTokens": atoi64(input), "cachedInputTokens": atoi64(cached), "outputTokens": atoi64(output),
		})
	}
	return out, rows.Err()
}

func getLlmTopAgents(db *sql.DB, sinceDays int, companyFilter string, hasCompany bool, limit int) ([]map[string]any, error) {
	params := []any{sinceDays}
	scope := ""
	if hasCompany {
		params = append(params, companyFilter)
		scope = `AND l.company_id = $` + strconv.Itoa(len(params))
	}
	params = append(params, limit)
	rows, err := db.Query(`
		SELECT l.agent_id, l.company_id,
		        p.name AS agent_name,
		        p.avatar_url AS agent_avatar_url,
		        p.avatar_bg  AS agent_avatar_bg,
		        p.initial    AS agent_initial,
		        c.name       AS company_name,
		        COALESCE(SUM(l.cost_usd), 0)::text          AS cost_usd,
		        SUM(l.calls)::text                          AS calls,
		        SUM(l.input_tokens)::text                   AS input_tokens,
		        SUM(l.cached_input_tokens)::text            AS cached_input_tokens,
		        SUM(l.output_tokens)::text                  AS output_tokens
		   FROM llm_calls_rollup l
		   LEFT JOIN participants p
		          ON p.id = l.agent_id
		         AND (p.company_id = l.company_id OR (p.company_id IS NULL AND l.company_id IS NULL))
		   LEFT JOIN companies c ON c.id = l.company_id
		  WHERE l.bucket_hour > NOW() - ($1::int * INTERVAL '1 day') `+scope+`
		  GROUP BY l.agent_id, l.company_id, p.name, p.avatar_url, p.avatar_bg, p.initial, c.name
		  ORDER BY SUM(l.cost_usd) DESC NULLS LAST
		  LIMIT $`+strconv.Itoa(len(params)), params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var agentID, companyID sql.NullString
		var agentName, agentAvatar, agentBg, agentInitial, companyName sql.NullString
		var costUsd, calls, input, cached, output string
		if rows.Scan(&agentID, &companyID, &agentName, &agentAvatar, &agentBg, &agentInitial,
			&companyName, &costUsd, &calls, &input, &cached, &output) != nil {
			continue
		}
		out = append(out, map[string]any{
			"agentId": nullStrAny(agentID), "companyId": nullStrAny(companyID),
			"agentName": nullStrAny(agentName), "agentAvatarUrl": nullStrAny(agentAvatar),
			"agentAvatarBg": nullStrAny(agentBg), "agentInitial": nullStrAny(agentInitial),
			"companyName": nullStrAny(companyName),
			"costUsd":     atof64(costUsd), "calls": atoi64(calls),
			"inputTokens": atoi64(input), "cachedInputTokens": atoi64(cached), "outputTokens": atoi64(output),
		})
	}
	return out, rows.Err()
}

func getLlmDaemonVersionRollup(db *sql.DB, sinceDays int, companyFilter string, hasCompany bool) ([]map[string]any, error) {
	params := []any{sinceDays}
	scope := ""
	if hasCompany {
		params = append(params, companyFilter)
		scope = `AND company_id = $` + strconv.Itoa(len(params))
	}
	rows, err := db.Query(`
		SELECT daemon_version, source,
		        SUM(calls)::text                                               AS calls,
		        COALESCE(SUM(cost_usd), 0)::text                               AS cost_usd,
		        SUM(input_tokens)::text                                        AS input_tokens,
		        SUM(cached_input_tokens)::text                                 AS cached_input_tokens,
		        SUM(output_tokens)::text                                       AS output_tokens,
		        SUM(ok_calls)::text                                            AS ok_calls,
		        MIN(bucket_hour)::text                                         AS first_seen,
		        MAX(bucket_hour)::text                                         AS last_seen
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
		var calls, costUsd, input, cached, output, okCalls, firstSeen, lastSeen string
		if rows.Scan(&version, &source, &calls, &costUsd, &input, &cached, &output, &okCalls,
			&firstSeen, &lastSeen) != nil {
			continue
		}
		n := atoi64(calls)
		failureRate := 0.0
		if n > 0 {
			failureRate = float64(n-atoi64(okCalls)) / float64(n)
		}
		out = append(out, map[string]any{
			"daemonVersion": version, "source": source,
			"calls": n, "costUsd": atof64(costUsd),
			"inputTokens": atoi64(input), "cachedInputTokens": atoi64(cached),
			"outputTokens": atoi64(output), "failureRate": failureRate,
			// 桶时戳的小时精度——"2h 前"级展示够用(TS 同款)。
			"firstSeen": firstSeen, "lastSeen": lastSeen,
		})
	}
	return out, rows.Err()
}

/* ───────────── 端点 ───────────── */

func llmObservability(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		q := r.URL.Query()
		sinceDays := sinceDaysParam(q)
		modelFilter := trimParam(q, "model")
		// 逐账户范围:空/缺 = 全部账户;非空收窄全部聚合(选择器数据
		// 源 tenants 除外,永远全局)。
		companyFilter := trimParam(q, "companyId")
		hasCompany := companyFilter != ""
		fresh := q.Get("fresh") == "1" || q.Get("fresh") == "true"

		cacheKey := "obs-llm|" + strconv.Itoa(sinceDays) + "|" + modelFilter + "|"
		if hasCompany {
			cacheKey += companyFilter
		} else {
			cacheKey += "*"
		}
		payload, err := cachedAgg(cacheKey, 30*time.Second, fresh, func() (any, error) {
			// TS Promise.all 并发扇出;此处顺序执行——结果集等价,首访
			// 多几趟串行往返,30s 缓存吃掉重复面。
			summary, err := getLlmSummary(db, sinceDays, companyFilter, hasCompany)
			if err != nil {
				return nil, err
			}
			rollup, err := getLlmSpendRollup(db, sinceDays, companyFilter, hasCompany, modelFilter)
			if err != nil {
				return nil, err
			}
			trend, err := getLlmDailyTrend(db, sinceDays, companyFilter, hasCompany)
			if err != nil {
				return nil, err
			}
			topAgents, err := getLlmTopAgents(db, sinceDays, companyFilter, hasCompany, 20)
			if err != nil {
				return nil, err
			}
			tenants, err := getLlmTenants(db, sinceDays, 200)
			if err != nil {
				return nil, err
			}
			daemonVersions, err := getLlmDaemonVersionRollup(db, sinceDays, companyFilter, hasCompany)
			if err != nil {
				return nil, err
			}
			// topPurpose + savableUsd 从手里的 rollup 派生(不加表扫),
			// 两者随之反映 model 过滤——正是运营者过滤单模型时想要的。
			costByPurpose := map[string]float64{}
			savableTotal := 0.0
			for _, row := range rollup {
				costByPurpose[row["purpose"].(string)] += row["costUsd"].(float64)
				savableTotal += row["savableUsd"].(float64)
			}
			topPurpose := any(nil)
			for _, p := range sortedKeysFloat(costByPurpose) {
				topPurpose = map[string]any{"purpose": p, "costUsd": costByPurpose[p]}
				break
			}
			summary["topPurpose"] = topPurpose
			summary["savableUsd"] = savableTotal
			return map[string]any{
				"summary": summary, "rollup": rollup, "trend": trend,
				"topAgents": topAgents, "tenants": tenants, "daemonVersions": daemonVersions,
			}, nil
		})
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, payload)
	}
}

// sortedKeysFloat:按值降序的键序(Map 迭代无序,TS 的 topPurpose 取
// 严格最大值——同值并列时取先到者,这里用确定序模拟)。
func sortedKeysFloat(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && m[keys[j]] > m[keys[j-1]]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// llmCalls:下钻明细 —— 一个 (purpose×model×source) 桶,或一个 run /
// agent 的轨迹。runId/agentId 路径默认 created ASC,桶路径默认 cost
// DESC,让面板顶部就是最重的那条。
func llmCalls(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireAdmin(w, r, db); !ok {
			return
		}
		q := r.URL.Query()
		sinceDays := sinceDaysParam(q)
		limit := callsLimitParam(q)
		purpose := trimParam(q, "purpose")
		model := trimParam(q, "model")
		source := trimParam(q, "source")
		runID := trimParam(q, "runId")
		agentID := trimParam(q, "agentId")
		companyFilter := trimParam(q, "companyId")
		sortBy := ""
		switch q.Get("sortBy") {
		case "cost", "latency", "hop", "created":
			sortBy = q.Get("sortBy")
		}

		params := []any{sinceDays}
		where := []string{`l.created_at > NOW() - ($1::int * INTERVAL '1 day')`}
		add := func(clause string, value any) {
			params = append(params, value)
			where = append(where, strings.Replace(clause, "$$", "$"+strconv.Itoa(len(params)), 1))
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
		orderBy := `l.cost_usd DESC NULLS LAST`
		switch sortBy {
		case "latency":
			orderBy = `l.latency_ms DESC NULLS LAST`
		case "hop":
			orderBy = `(l.extras->>'hopIndex')::int ASC NULLS LAST, l.created_at ASC`
		case "created":
			orderBy = `l.created_at ASC`
		}
		params = append(params, limit)
		rows, err := db.Query(`
			SELECT
			   l.id, l.created_at::text, l.company_id,
			   l.agent_id, p.name AS agent_name,
			   l.run_id, l.conversation_id,
			   l.purpose, l.source, l.model,
			   l.input_tokens::text, l.cached_input_tokens::text, l.cache_creation_tokens::text,
			   l.output_tokens::text, l.reasoning_tokens::text,
			   l.cost_usd::text, l.cost_estimated, l.measured,
			   l.latency_ms, l.status, l.error,
			   l.extras, l.daemon_version
			 FROM llm_calls l
			 LEFT JOIN participants p
			        ON p.id = l.agent_id
			       AND (p.company_id = l.company_id OR (p.company_id IS NULL AND l.company_id IS NULL))
			 WHERE `+strings.Join(where, " AND ")+`
			 ORDER BY `+orderBy+`
			 LIMIT $`+strconv.Itoa(len(params)), params...)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id, createdAt, purpose2, source2, model2 string
			var companyID, agentID2, agentName, runID2, convID sql.NullString
			var input, cached, cacheCreation, output, reasoning, costUsd string
			var costEstimated, measured bool
			var latency sql.NullInt64
			var status string
			var errStr sql.NullString
			var extras []byte
			var daemonVersion sql.NullString
			if rows.Scan(&id, &createdAt, &companyID, &agentID2, &agentName, &runID2, &convID,
				&purpose2, &source2, &model2, &input, &cached, &cacheCreation, &output, &reasoning,
				&costUsd, &costEstimated, &measured, &latency, &status, &errStr, &extras, &daemonVersion) != nil {
				continue
			}
			extrasAny := any(nil)
			if len(extras) > 0 {
				extrasAny = json.RawMessage(extras)
			}
			latencyAny := any(nil)
			if latency.Valid {
				latencyAny = latency.Int64
			}
			out = append(out, map[string]any{
				"id": id, "createdAt": createdAt, "companyId": nullStrAny(companyID),
				"agentId": nullStrAny(agentID2), "agentName": nullStrAny(agentName),
				"runId": nullStrAny(runID2), "conversationId": nullStrAny(convID),
				"purpose": purpose2, "source": source2, "model": model2,
				"inputTokens": atoi64(input), "cachedInputTokens": atoi64(cached),
				"cacheCreationTokens": atoi64(cacheCreation),
				"outputTokens":        atoi64(output), "reasoningTokens": atoi64(reasoning),
				"costUsd": atof64(costUsd), "costEstimated": costEstimated, "measured": measured,
				"latencyMs": latencyAny, "status": status, "error": nullStrAny(errStr),
				"extras": extrasAny, "daemonVersion": nullStrAny(daemonVersion),
			})
		}
		if err := rows.Err(); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}
