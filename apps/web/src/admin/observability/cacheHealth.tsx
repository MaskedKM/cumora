import { useMemo } from 'react'
import {
  cacheHitRate,
  fmtPct,
  fmtTokens,
  fmtUsdCompact as fmtUsd,
} from '@/lib/format'
import type { useT } from '@/lib/i18n'
import type { LlmCallPurpose, LlmObservabilityPayload, LlmRollupRow, LlmTrendBucket } from '../api'
import { cacheToneClass, metaFor, purposeLabel, type Unit } from './shared'

// ─── Cache health card ───────────────────────────────────────────────────
//
// Cache hit rate is the single biggest optimization vector: every input token
// that hits cache costs ~10× less than uncached. This card answers three
// questions at a glance:
//
//   1. "How bad is it?"    — overall hit rate + $ savable, hero stat
//   2. "Is it getting worse?" — daily timeseries line chart (UTC days)
//   3. "Where do I look first?" — per-purpose horizontal bars sorted by
//                                  savable $ desc, so the top of the list is
//                                  always the highest-ROI optimization target.
//
// Tone: same paper palette as the rest of the page, but the bars carry a
// subtle sky→coral gradient through the threshold (>50% sky, <20% coral) so
// the operator's eye finds the bad rows without reading labels.

export function CacheHealthCard({ summary, trend, rollup, unit, loading, t }: {
  summary: LlmObservabilityPayload['summary'] | undefined
  trend: LlmTrendBucket[]
  rollup: LlmRollupRow[]
  unit: Unit
  loading: boolean
  t: ReturnType<typeof useT>
}) {
  // Per-purpose aggregation from the rollup (already source-filtered upstream).
  const perPurpose = useMemo(() => {
    const m = new Map<LlmCallPurpose, {
      uncached: number; cached: number; savable: number; cost: number
    }>()
    for (const r of rollup) {
      const cur = m.get(r.purpose) ?? { uncached: 0, cached: 0, savable: 0, cost: 0 }
      cur.uncached += r.inputTokens
      cur.cached += r.cachedInputTokens
      cur.savable += r.savableUsd
      cur.cost += r.costUsd
      m.set(r.purpose, cur)
    }
    const out = [...m.entries()].map(([purpose, v]) => {
      const hitRate = (v.uncached + v.cached) > 0 ? cacheHitRate(v.uncached, v.cached) : null
      return { purpose, hitRate, savableUsd: v.savable, costUsd: v.cost, uncachedIn: v.uncached, cachedIn: v.cached }
    })
    // Biggest optimization target at the top: by savable $ in USD mode, by
    // uncached (cacheable) input tokens in token mode.
    out.sort((a, b) => unit === 'usd' ? b.savableUsd - a.savableUsd : b.uncachedIn - a.uncachedIn)
    return out
  }, [rollup, unit])

  // Daily hit rate from the trend (sum across purposes per day).
  const daily = useMemo(() => {
    const m = new Map<string, { uncached: number; cached: number }>()
    for (const t of trend) {
      const cur = m.get(t.day) ?? { uncached: 0, cached: 0 }
      cur.uncached += t.inputTokens
      cur.cached += t.cachedInputTokens
      m.set(t.day, cur)
    }
    const days = [...m.entries()].sort(([a], [b]) => a < b ? -1 : 1)
    return days.map(([day, v]) => {
      const hitRate = (v.uncached + v.cached) > 0 ? cacheHitRate(v.uncached, v.cached) : null
      return { day, hitRate }
    })
  }, [trend])

  const hit = summary?.cacheHitRate
  const savable = summary?.savableUsd ?? 0

  return (
    <section className="obs-card obs-cache-card">
      <div className="obs-card-head">
        <div>
          <div className="obs-card-title">{t('adminobs.cacheHealthTitle')}</div>
          <div className="obs-card-sub">
            {t('adminobs.cacheHealthSub')}
          </div>
        </div>
      </div>

      <div className="obs-cache-hero">
        <div className="obs-cache-hero-main">
          <div className="obs-cache-hero-label">{t('adminobs.cacheHitLabel')}</div>
          <div className={`obs-cache-hero-value ${cacheToneClass(hit)}`}>
            {hit != null ? fmtPct(hit, 1) : '—'}
          </div>
          <div className="obs-cache-hero-sub">
            {hit != null
              ? t('adminobs.cacheHeroSub', { n: fmtTokens((summary?.totalInputTokens ?? 0) + (summary?.totalCachedInputTokens ?? 0)) })
              : '—'}
          </div>
        </div>
        <div className="obs-cache-hero-aside">
          <div className="obs-cache-hero-label">{unit === 'usd' ? t('adminobs.moneyOnTable') : t('adminobs.cacheableTokens')}</div>
          <div className="obs-cache-hero-value obs-cache-tone-warn">
            {loading ? '—' : unit === 'usd' ? fmtUsd(savable, savable < 1 ? 4 : 2) : fmtTokens(summary?.totalInputTokens ?? 0)}
          </div>
          <div className="obs-cache-hero-sub">
            {t('adminobs.upperBoundPrefix')}{unit === 'usd' ? t('adminobs.upperBoundUsd') : t('adminobs.upperBoundTokens')}
          </div>
        </div>
      </div>

      <CacheDailyChart days={daily} loading={loading} t={t} />

      <div className="obs-cache-bars">
        <div className="obs-cache-bars-head">{t('adminobs.cacheBarsHead', { k: unit === 'usd' ? t('adminobs.savableUsd') : t('adminobs.cacheableTokens') })}</div>
        {perPurpose.length === 0 && <div className="obs-empty">{t('adminobs.noInputTraffic')}</div>}
        {perPurpose.map((p) => (
          <CachePurposeBar key={p.purpose} {...p} unit={unit} t={t} />
        ))}
      </div>
    </section>
  )
}

/** Single-line area chart of daily cache hit rate. 0–100% Y axis (fixed), so
 *  a long flat low line reads as "you're not caching, do something". Soft
 *  area fill under the line + a faint 50% midline as a target. */
function CacheDailyChart({ days, loading, t }: {
  days: Array<{ day: string; hitRate: number | null }>
  loading: boolean
  t: ReturnType<typeof useT>
}) {
  const W = 1000, H = 160, PADL = 56, PADR = 16, PADT = 16, PADB = 32
  const innerW = W - PADL - PADR
  const innerH = H - PADT - PADB

  if (loading) return <div className="obs-chart-empty obs-cache-chart-empty">{t('adminobs.loading')}</div>
  if (days.length === 0) return <div className="obs-chart-empty obs-cache-chart-empty">{t('adminobs.noDailyData')}</div>

  // Points; skip nulls (no traffic) so the line interpolates instead of dropping to 0.
  const points = days
    .map((d, i) => ({ i, day: d.day, y: d.hitRate }))
    .filter((p) => p.y != null) as Array<{ i: number; day: string; y: number }>

  const xFor = (i: number) => days.length === 1
    ? PADL + innerW / 2
    : PADL + (i / (days.length - 1)) * innerW
  const yFor = (rate: number) => PADT + (1 - rate) * innerH

  const linePath = points.map((p, idx) => `${idx === 0 ? 'M' : 'L'}${xFor(p.i).toFixed(1)},${yFor(p.y).toFixed(1)}`).join(' ')
  const areaPath = points.length > 0
    ? `${linePath} L${xFor(points[points.length - 1]!.i).toFixed(1)},${(H - PADB).toFixed(1)} L${xFor(points[0]!.i).toFixed(1)},${(H - PADB).toFixed(1)} Z`
    : ''

  // Y ticks at 0/50/100%.
  const yTicks = [0, 0.5, 1].map((r) => ({ y: yFor(r), label: `${Math.round(r * 100)}%` }))
  // Sparse X labels (first/middle/last).
  const xLabels = days.length <= 4
    ? days.map((d, i) => ({ x: xFor(i), label: d.day.slice(5) }))
    : [
        { x: xFor(0),                                  label: days[0]!.day.slice(5) },
        { x: xFor(Math.floor((days.length - 1) / 2)),  label: days[Math.floor((days.length - 1) / 2)]!.day.slice(5) },
        { x: xFor(days.length - 1),                    label: days[days.length - 1]!.day.slice(5) },
      ]

  return (
    <div className="obs-cache-chart">
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label="Daily cache hit rate">
        <defs>
          <linearGradient id="obsCacheGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--skype-deep)" stopOpacity={0.18} />
            <stop offset="100%" stopColor="var(--skype-deep)" stopOpacity={0} />
          </linearGradient>
        </defs>
        {/* Grid + the 50% target line */}
        {yTicks.map((t, i) => (
          <g key={i}>
            <line x1={PADL} x2={W - PADR} y1={t.y} y2={t.y} stroke="var(--ink-100)" strokeWidth={1} strokeDasharray={i === 1 ? '4 4' : ''} />
            <text x={PADL - 8} y={t.y + 4} textAnchor="end" fontSize={10} fill="var(--ink-500)">{t.label}</text>
          </g>
        ))}
        {/* Area + line */}
        {areaPath && <path d={areaPath} fill="url(#obsCacheGrad)" />}
        {linePath && <path d={linePath} fill="none" stroke="var(--skype-deep)" strokeWidth={2} strokeLinejoin="round" strokeLinecap="round" />}
        {/* Data points */}
        {points.map((p, idx) => (
          <circle key={idx} cx={xFor(p.i)} cy={yFor(p.y)} r={2.5} fill="var(--skype-deep)">
            <title>{`${p.day}: ${fmtPct(p.y, 1)}`}</title>
          </circle>
        ))}
        {/* X labels */}
        {xLabels.map((l, i) => (
          <text key={i} x={l.x} y={H - 10} textAnchor="middle" fontSize={10} fill="var(--ink-500)">{l.label}</text>
        ))}
      </svg>
    </div>
  )
}

/** One horizontal bar — purpose dot + label, then a progress-style bar
 *  whose fill width is the hit rate. The right side shows savable $ — the
 *  actionable number. */
function CachePurposeBar({ purpose, hitRate, savableUsd, costUsd, uncachedIn, cachedIn, unit, t }: {
  purpose: LlmCallPurpose
  hitRate: number | null
  savableUsd: number
  costUsd: number
  uncachedIn: number
  cachedIn: number
  unit: Unit
  t: ReturnType<typeof useT>
}) {
  const m = metaFor(purpose)
  const ratePct = hitRate != null ? Math.max(0, Math.min(1, hitRate)) * 100 : 0
  const total = uncachedIn + cachedIn
  return (
    <div className="obs-cache-bar-row" title={t('adminobs.cacheBarTitle', { cached: fmtTokens(cachedIn), total: fmtTokens(total) })}>
      <div className="obs-cache-bar-purpose">
        <span className="obs-dot" style={{ background: m.swatch }} aria-hidden />
        <span className="obs-cache-bar-label">{purposeLabel(t, purpose)}</span>
      </div>
      <div className="obs-cache-bar-track">
        <div
          className={`obs-cache-bar-fill ${cacheToneClass(hitRate)}`}
          style={{ width: `${ratePct}%` }}
          aria-label={`${ratePct.toFixed(0)}%`}
        />
        <span className="obs-cache-bar-rate">{hitRate != null ? fmtPct(hitRate, 0) : '—'}</span>
      </div>
      <div className="obs-cache-bar-savable">
        {unit === 'usd'
          ? <>
              {savableUsd > 0 ? <span className="obs-cache-bar-savable-amt">{fmtUsd(savableUsd, savableUsd < 1 ? 4 : 2)}</span> : <span className="obs-cache-bar-savable-na">—</span>}
              <span className="obs-cache-bar-savable-sub"> of {fmtUsd(costUsd, costUsd < 1 ? 4 : 2)}</span>
            </>
          : <>
              {uncachedIn > 0 ? <span className="obs-cache-bar-savable-amt">{fmtTokens(uncachedIn)}</span> : <span className="obs-cache-bar-savable-na">—</span>}
              <span className="obs-cache-bar-savable-sub"> of {fmtTokens(uncachedIn + cachedIn)} in</span>
            </>}
      </div>
    </div>
  )
}
