/**
 * Admin Observability — the operator-facing answer to:
 *
 *   "Of every sub2api token the platform spent this month, where did it go?"
 *
 * Source of truth is the universal `llm_calls` ledger (Go side:
 * internal/runtime/observability_api.go); this page is a presentation layer
 * over four shapes the
 * /api/admin/observability/llm endpoint returns in one round-trip:
 *
 *   - summary     — hero KPIs (total $, total calls, top burner purpose,
 *                   failure rate, active tenants, output/cached token mix)
 *   - rollup      — per-purpose × model × source breakdown, sorted by $ desc
 *   - trend       — daily-bucketed cost × purpose for the area chart
 *   - topAgents   — top spenders by agent, joined to display name
 *
 * Visual goals (per product brief): stylish, refined, rich. We lean on the
 * existing skype/ink/sky/coral palette — no new neon — and add three things
 * the rest of the admin doesn't have:
 *
 *   1. A gradient "spend" hero card that anchors the page.
 *   2. A stacked SVG area chart for the daily trend (no chart library
 *      pulled in — vendored ~80 LOC of inline SVG that matches the brand
 *      colors, no runtime tax for a single page).
 *   3. Purpose-coded swatches so the eye finds the same purpose in the
 *      KPI card, the chart, AND the rollup table.
 *
 * Filters are URL-stateless (in-page only) — the page is admin-only and the
 * audience is the operator, not a sharable dashboard, so deep linking isn't
 * worth the complexity.
 */

// 子组件分桶(#219 ①):本文件保留页面编排(筛选状态/取数/布局),
// 十余个卡片·表格·钻透面板件按职责分居 ./observability/:
//   shared(目的色板+单位/来源词表+cacheTone)· heroCards · trendChart
//   · cacheHealth · tables(Rollup/TopAgents/Daemon)· drillPanel · tenantPicker
// format 层与轮询数据层仍由 #147 ② 的 @/lib/format 与 usePollingRefresh 承担。
import { useEffect, useMemo, useRef, useState } from 'react'
import { Select } from '@/components/Select'
import { fmtInt, fmtPct, fmtTokens } from '@/lib/format'
import { type MessageKey, tLabel, useT } from '@/lib/i18n'
import { usePollingRefresh } from '@/lib/usePollingRefresh'
import { adminApi, type LlmCallPurpose, type LlmObservabilityPayload } from './api'
import { CacheHealthCard } from './observability/cacheHealth'
import { type DrillDown, DrillPanel } from './observability/drillPanel'
import { HeroSpendCard, HeroStatCard } from './observability/heroCards'
import {
  fmtAmount,
  isByoaSource,
  metaFor,
  purposeLabel,
  SOURCE_FILTERS,
  SOURCE_LABEL_KEY,
  type SourceFilter,
  totalTokens,
  UNITS,
  type Unit,
} from './observability/shared'
import { DaemonVersionTable, RollupTable, TopAgentsTable } from './observability/tables'
import { TenantPicker } from './observability/tenantPicker'
import { PurposeLegend, TrendChart } from './observability/trendChart'

// ─── Range pills ─────────────────────────────────────────────────────────

const RANGES: Array<{ label: string; days: number }> = [
  { label: '24h', days: 1 },
  { label: '7d',  days: 7 },
  { label: '30d', days: 30 },
  { label: '90d', days: 90 },
]

// Auto-refresh cadences (ms). 0 = off. The data pipeline itself is at most ~2min
// stale (the server-side rollup worker), so sub-minute polling mostly re-reads
// the same numbers — but it matches the "live dashboard" expectation and the
// drill-down (raw rows) IS live.
const REFRESH_INTERVALS: Array<{ label: string; ms: number }> = [
  { label: 'Auto-refresh: Off', ms: 0 },
  { label: 'Every 30s', ms: 30_000 },
  { label: 'Every 1m',  ms: 60_000 },
  { label: 'Every 5m',  ms: 300_000 },
]
// i18n lookup by ms cadence. English label stays as fallback for any
// future cadence the author adds before we have a key wired up.
const REFRESH_LABEL_KEY: Record<number, MessageKey> = {
  0:       'adminobs.refreshOff',
  30000:  'adminobs.refresh30s',
  60000:  'adminobs.refresh1m',
  300000: 'adminobs.refresh5m',
}

// ─── Component ───────────────────────────────────────────────────────────

export function ObservabilityPage() {
  const t = useT()
  const [sinceDays, setSinceDays] = useState<number>(30)
  const [modelFilter, setModelFilter] = useState<string>('')
  const [sourceFilter, setSourceFilter] = useState<SourceFilter>('all')
  /** $ vs tokens. Defaults to tokens — the platform-neutral truth (the $
   *  figures are seeded estimates; see the Unit notes above). */
  const [unit, setUnit] = useState<Unit>('tokens')
  /** Per-tenant scope. Empty string = all accounts; a non-empty companyId
   *  narrows EVERY stat on the page to that account. The picker below the
   *  filter bar populates this. */
  const [companyFilter, setCompanyFilter] = useState<string>('')
  const [data, setData] = useState<LlmObservabilityPayload | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  /** Open drill-down view. ESC / overlay click closes it. */
  const [drill, setDrill] = useState<DrillDown | null>(null)
  /** Manual-refresh + auto-refresh. `refreshTick` bumps to force a re-fetch
   *  (the fetch effect depends on it); `freshRef` flags that the *next* fetch
   *  should bypass the server's 30s response cache (set by user/auto refresh,
   *  not by a filter change). `lastUpdated` powers the "updated …" readout and
   *  `refreshing` spins the button without flashing the skeleton. */
  const [refreshTick, setRefreshTick] = useState(0)
  const [autoRefreshMs, setAutoRefreshMs] = useState(0)
  const [lastUpdated, setLastUpdated] = useState<number | null>(null)
  const [refreshing, setRefreshing] = useState(false)
  const freshRef = useRef(false)
  const everLoadedRef = useRef(false)
  const triggerRefresh = () => { freshRef.current = true; setRefreshing(true); setRefreshTick((t) => t + 1) }

  useEffect(() => {
    let cancelled = false
    // Skeleton only on the very first load (no data yet); a refresh or filter
    // change keeps the current data on screen until the new payload lands, so
    // the page never flashes empty.
    if (!everLoadedRef.current) setLoading(true)
    setError(null)
    const fresh = freshRef.current
    freshRef.current = false
    adminApi.observabilityLlm({
      sinceDays,
      ...(modelFilter.trim() ? { model: modelFilter.trim() } : {}),
      ...(companyFilter ? { companyId: companyFilter } : {}),
      ...(fresh ? { fresh: true } : {}),
    })
      .then((r) => { if (!cancelled) { setData(r); setLastUpdated(Date.now()); everLoadedRef.current = true } })
      .catch((e: unknown) => { if (!cancelled) setError(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (!cancelled) { setLoading(false); setRefreshing(false) } })
    return () => { cancelled = true }
  }, [sinceDays, modelFilter, companyFilter, refreshTick])

  // Auto-refresh: while a non-zero interval is selected, force a fresh
  // re-fetch on that cadence. 共享 hook(#147 ②)—— 本页顺带采纳后台 tab
  // 暂停(desktop 侧既有实践:看不见的面板不烧 API/电池)。
  usePollingRefresh(triggerRefresh, autoRefreshMs)

  // Apply source filter CLIENT-SIDE — the payload is small and we want toggle
  // changes to be instant, not another round-trip. The summary KPIs stay
  // GLOBAL (across all sources) so the "Top burner" card still answers "of
  // EVERYTHING, what's the biggest" — that's the question the operator
  // actually wants there. Per-source breakdowns live in the rollup table.
  const filtered = useMemo(() => {
    if (!data) return data
    if (sourceFilter === 'all') return data
    return {
      ...data,
      rollup: data.rollup.filter((r) => sourceFilter === 'byoa' ? isByoaSource(r.source) : r.source === sourceFilter),
      // Trend buckets don't carry source; leaving them as-is is the honest
      // choice — the source-filtered chart would mean re-fetching with a
      // server-side filter, deferred until needed.
    }
  }, [data, sourceFilter])

  // Top burner adapts to the unit: by $ (server's summary.topPurpose, global)
  // for USD; by total tokens (from the source-filtered rollup) for tokens —
  // because the biggest token consumer isn't always the biggest $ one (a cheap
  // high-volume model inverts the ranking, which is exactly what tokens expose).
  const topBurner = useMemo((): { purpose: LlmCallPurpose; usd: number; tokens: number } | null => {
    if (unit === 'usd') {
      const tp = data?.summary?.topPurpose
      return tp ? { purpose: tp.purpose, usd: tp.costUsd, tokens: 0 } : null
    }
    const m = new Map<LlmCallPurpose, { usd: number; tokens: number }>()
    for (const r of filtered?.rollup ?? []) {
      const cur = m.get(r.purpose) ?? { usd: 0, tokens: 0 }
      cur.usd += r.costUsd
      cur.tokens += totalTokens(r)
      m.set(r.purpose, cur)
    }
    let best: { purpose: LlmCallPurpose; usd: number; tokens: number } | null = null
    for (const [purpose, v] of m) if (!best || v.tokens > best.tokens) best = { purpose, ...v }
    return best
  }, [unit, data?.summary?.topPurpose, filtered?.rollup])

  return (
    <div className="admin-page obs-page">
      <header className="admin-page-head">
        <div>
          <h1 className="admin-h1">{t('adminobs.title')}</h1>
          <div className="admin-sub">
            {t('adminobs.subtitle')}
            {data?.summary && (
              <> {t('adminobs.windowSummary', { sinceDays: data.summary.sinceDays, activeTenants: fmtInt(data.summary.activeTenants) })}</>
            )}
          </div>
        </div>
        <div className="admin-filters obs-filters">
          <div className="obs-pills" role="tablist" aria-label={t('adminobs.rangeAria')}>
            {RANGES.map((r) => (
              <button
                key={r.days}
                role="tab"
                aria-selected={sinceDays === r.days}
                className={`obs-pill${sinceDays === r.days ? ' is-active' : ''}`}
                onClick={() => setSinceDays(r.days)}
              >{r.label}</button>
            ))}
          </div>
          <div className="obs-pills" role="tablist" aria-label={t('adminobs.sourceAria')}>
            {SOURCE_FILTERS.map((s) => (
              <button
                key={s.key}
                role="tab"
                aria-selected={sourceFilter === s.key}
                className={`obs-pill${sourceFilter === s.key ? ' is-active' : ''}`}
                onClick={() => setSourceFilter(s.key)}
              >{tLabel(t, SOURCE_LABEL_KEY[s.key], s.label)}</button>
            ))}
          </div>
          {/* Unit toggle — $ are seeded estimates; tokens are the platform-
              neutral truth (the whole reason BYOA can't trust the $). */}
          <div className="obs-pills" role="tablist" aria-label={t('adminobs.unitAria')}>
            {UNITS.map((u) => (
              <button
                key={u.key}
                role="tab"
                aria-selected={unit === u.key}
                className={`obs-pill${unit === u.key ? ' is-active' : ''}`}
                onClick={() => setUnit(u.key)}
                title={u.key === 'usd' ? t('adminobs.unitUsdTitle') : t('adminobs.unitTokensTitle')}
              >{u.key === 'usd' ? tLabel(t, 'adminobs.unitUsd', u.label) : tLabel(t, 'adminobs.unitTokens', u.label)}</button>
            ))}
          </div>
          <input
            className="admin-input obs-model-input"
            placeholder={t('adminobs.modelPh')}
            value={modelFilter}
            onChange={(e) => setModelFilter(e.target.value)}
          />
          <TenantPicker
            tenants={data?.tenants ?? []}
            value={companyFilter}
            unit={unit}
            onChange={setCompanyFilter}
            t={t}
          />
          {/* Refresh controls — manual button + auto-refresh cadence. Both force
              a fresh (cache-bypassing) re-fetch with the CURRENT filters. */}
          <div className="obs-refresh-group">
            <button
              type="button"
              className="obs-refresh-btn"
              onClick={triggerRefresh}
              disabled={refreshing}
              title={lastUpdated ? t('adminobs.refreshedTitle', { time: new Date(lastUpdated).toLocaleTimeString() }) : t('adminobs.refreshTitle')}
              aria-label={t('adminobs.refreshAria')}
            >
              <span className={`obs-refresh-icon${refreshing ? ' is-spinning' : ''}`} aria-hidden>↻</span>
            </button>
            <Select
              value={String(autoRefreshMs)}
              onValueChange={(v) => setAutoRefreshMs(Number(v))}
              options={REFRESH_INTERVALS.map((r) => ({ value: String(r.ms), label: tLabel(t, REFRESH_LABEL_KEY[r.ms], r.label) }))}
              ariaLabel={tLabel(t, 'adminobs.autoRefreshAria', 'Auto-refresh interval')}
              className="min-w-[150px]"
            />
          </div>
        </div>
        {lastUpdated && (
          <div className="obs-updated" aria-live="polite">
            {t('adminobs.updatedAt', { time: new Date(lastUpdated).toLocaleTimeString() })}
            {autoRefreshMs > 0 && <> · {t('adminobs.autoEvery', { label: (() => { const found = REFRESH_INTERVALS.find((r) => r.ms === autoRefreshMs); return found ? tLabel(t, REFRESH_LABEL_KEY[found.ms], found.label).replace(/^(Every |每 )/, '') : '' })() })}</>}
            {refreshing && <> · {t('adminobs.refreshing')}</>}
          </div>
        )}
      </header>

      {error && <div className="obs-error">{t('adminobs.loadFailed', { error })}</div>}

      {/* Hero KPIs — paper-toned, with one richly-treated "Spend" tile as
          the visual anchor. The four cards always render so the page doesn't
          jump around between loading / loaded states. */}
      <section className="obs-hero">
        <HeroSpendCard summary={data?.summary} unit={unit} loading={loading} t={t} />
        <HeroStatCard
          label={t('adminobs.totalCalls')}
          value={data?.summary ? fmtInt(data.summary.totalCalls) : '—'}
          sub={data?.summary
            ? t('adminobs.rateLimitedSub', { n: fmtInt(data.summary.rateLimitedCalls) })
            : ' '}
          loading={loading}
        />
        <HeroStatCard
          label={t('adminobs.topBurner')}
          value={topBurner ? purposeLabel(t, topBurner.purpose) : '—'}
          sub={topBurner
            ? t('adminobs.ofWindow', { amount: fmtAmount(unit, topBurner.usd, topBurner.tokens) })
            : ' '}
          accent={topBurner ? metaFor(topBurner.purpose).swatch : undefined}
          loading={loading}
        />
        <HeroStatCard
          label={t('adminobs.failureRate')}
          value={data?.summary ? fmtPct(data.summary.failureRate) : '—'}
          sub={data?.summary
            ? t('adminobs.failSub', { out: fmtTokens(data.summary.totalOutputTokens), cached: fmtTokens(data.summary.totalCachedInputTokens) })
            : ' '}
          accent={data?.summary && data.summary.failureRate > 0.05 ? 'var(--coral-deep)' : undefined}
          loading={loading}
        />
      </section>

      {/* Stacked area / line chart. SVG-only so we don't import a chart lib
          for one page. The trend is the SECOND-most-important visual after
          the hero: a spike in compaction $ yesterday should jump out. */}
      <section className="obs-card obs-trend-card">
        <div className="obs-card-head">
          <div>
            <div className="obs-card-title">{unit === 'usd' ? t('adminobs.trendCostTitle') : t('adminobs.trendTokensTitle')}</div>
            <div className="obs-card-sub">{t('adminobs.trendSub', { modelPart: modelFilter ? t('adminobs.trendSubModel', { model: modelFilter }) : t('adminobs.trendSubAllModels'), tokenPart: unit !== 'usd' ? t('adminobs.trendSubTokens') : '' })}</div>
          </div>
        </div>
        <TrendChart buckets={data?.trend ?? []} unit={unit} loading={loading} t={t} />
        <PurposeLegend rollup={filtered?.rollup ?? []} unit={unit} />
      </section>

      {/* Cache health — the single biggest optimization vector. Every uncached
          input token costs ~10× the cached rate; this card tells the operator
          how much they're leaving on the table and which purpose to target. */}
      <CacheHealthCard summary={data?.summary} trend={data?.trend ?? []} rollup={filtered?.rollup ?? []} unit={unit} loading={loading} t={t} />

      {/* Per-purpose rollup. The reason this page exists. */}
      <section className="obs-card">
        <div className="obs-card-head">
          <div>
            <div className="obs-card-title">{t('adminobs.rollupTitle', { unit: unit === 'usd' ? t('adminobs.spendLabel') : t('adminobs.tokensLabel') })}</div>
            <div className="obs-card-sub">
              {t('adminobs.rollupSub', { unit: unit === 'usd' ? t('adminobs.spendLabel') : t('adminobs.tokensLabel') })}
              {sourceFilter !== 'all' && <> · {t('adminobs.rollupFiltered', { label: (() => { const found = SOURCE_FILTERS.find((s) => s.key === sourceFilter); return found ? tLabel(t, SOURCE_LABEL_KEY[found.key], found.label) : '' })() })}</>}
            </div>
          </div>
        </div>
        <RollupTable
          rows={filtered?.rollup ?? []}
          unit={unit}
          loading={loading}
          onDrill={(r) => setDrill({ kind: 'bucket', purpose: r.purpose, model: r.model, source: r.source })}
          t={t}
        />
      </section>

      {/* Top spenders — joins agent display name so the operator can SEE
          who's burning the most. Anonymous (no-agent) rows aggregate as
          one bottom row when present (e.g. avatar-image at agent creation). */}
      <section className="obs-card">
        <div className="obs-card-head">
          <div>
            <div className="obs-card-title">{t('adminobs.topSpendersTitle')}</div>
            <div className="obs-card-sub">{t('adminobs.topSpendersSub')}</div>
          </div>
        </div>
        <TopAgentsTable
          rows={data?.topAgents ?? []}
          unit={unit}
          loading={loading}
          onDrill={(r) => r.agentId && setDrill({ kind: 'agent', agentId: r.agentId, agentName: r.agentName })}
          t={t}
        />
      </section>

      {/* By daemon version — correlate spend / cache behaviour with daemon
          releases. When a new version regresses token usage, the per-version
          rollup makes the bad bucket light up. NULL daemon_version rows
          (cloud agent-turn etc.) are excluded server-side; this card is for
          BYOA-version analysis specifically. */}
      <section className="obs-card">
        <div className="obs-card-head">
          <div>
            <div className="obs-card-title">{t('adminobs.daemonTitle')}</div>
            <div className="obs-card-sub">{t('adminobs.daemonSub')}</div>
          </div>
        </div>
        <DaemonVersionTable
          rows={data?.daemonVersions ?? []}
          unit={unit}
          loading={loading}
          t={t}
        />
      </section>

      {/* Slide-in drill-down panel — the "no SQL needed" view. Rendered as a
          sibling of the page chrome so its overlay sits on top of everything;
          the page underneath stays visible (and scrolled) so closing returns
          exactly where the operator was. */}
      <DrillPanel
        drill={drill}
        sinceDays={sinceDays}
        companyId={companyFilter || undefined}
        unit={unit}
        refreshSignal={refreshTick}
        onClose={() => setDrill(null)}
        onJumpToRun={(runId) => setDrill({ kind: 'run', runId })}
        onJumpToAgent={(agentId, agentName) => setDrill({ kind: 'agent', agentId, agentName })}
        t={t}
      />
    </div>
  )
}
