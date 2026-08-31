import { useEffect, useMemo, useState } from 'react'
import {
  cacheHitRate,
  fmtInt,
  fmtPct,
  fmtTokens,
  fmtUsdCompact as fmtUsd,
  relativeTimeParts,
} from '@/lib/format'
import type { useT } from '@/lib/i18n'
import type { LlmDaemonVersionRow, LlmObservabilityPayload, LlmRollupRow, LlmTopAgentRow } from '../api'
import {
  cacheToneClass,
  isByoaSource,
  metaFor,
  purposeBlurb,
  purposeLabel,
  totalTokens,
  type Unit,
} from './shared'

// ─── Rollup table ────────────────────────────────────────────────────────

type SortKey = 'costUsd' | 'totalTok' | 'calls' | 'inputTokens' | 'outputTokens' | 'failureRate' | 'cacheHitRate'

export function RollupTable({ rows, unit, loading, onDrill, t }: { rows: LlmRollupRow[]; unit: Unit; loading: boolean; onDrill: (r: LlmRollupRow) => void; t: ReturnType<typeof useT> }) {
  // The headline column re-keys with the unit so the default sort always
  // matches what's on screen ($ desc in USD, total tokens desc in tokens).
  const headlineKey: SortKey = unit === 'usd' ? 'costUsd' : 'totalTok'
  const [sort, setSort] = useState<{ key: SortKey; dir: 'desc' | 'asc' }>({ key: 'costUsd', dir: 'desc' })
  // Keep the active sort pinned to the headline column when the unit flips, so
  // toggling $↔tokens doesn't leave the list sorted by a now-hidden metric.
  useEffect(() => { setSort((s) => (s.key === 'costUsd' || s.key === 'totalTok') ? { key: headlineKey, dir: 'desc' } : s) }, [headlineKey])
  const enriched = useMemo(() => rows.map((r) => {
    const failureRate = r.calls > 0 ? (r.calls - r.okCalls) / r.calls : 0
    return { ...r, failureRate, cacheHitRate: cacheHitRate(r.inputTokens, r.cachedInputTokens), totalTok: totalTokens(r) }
  }), [rows])
  const sorted = useMemo(() => {
    const out = [...enriched]
    out.sort((a, b) => (b[sort.key] as number) - (a[sort.key] as number))
    if (sort.dir === 'asc') out.reverse()
    return out
  }, [enriched, sort])

  const head = (label: string, key: SortKey, align: 'left' | 'right' = 'right') => (
    <button
      type="button"
      className={`obs-th-btn obs-th-${align}${sort.key === key ? ' is-active' : ''}`}
      onClick={() => setSort((s) => s.key === key ? { key, dir: s.dir === 'desc' ? 'asc' : 'desc' } : { key, dir: 'desc' })}
    >
      {label}{sort.key === key && <span className="obs-sort-arrow">{sort.dir === 'desc' ? '↓' : '↑'}</span>}
    </button>
  )

  if (loading && rows.length === 0) return <div className="obs-empty">{t('adminobs.loading')}</div>
  if (!loading && rows.length === 0) return <div className="obs-empty">{t('adminobs.noSpendWindow')}</div>

  return (
    <div className="obs-table">
      <div className="obs-thead">
        <div className="obs-th-left">{t('adminobs.colPurpose')}</div>
        <div className="obs-th-left">{t('adminobs.colModelSource')}</div>
        <div>{unit === 'usd' ? head(t('adminobs.colCost'), 'costUsd') : head(t('adminobs.tokensLabel'), 'totalTok')}</div>
        <div>{head(t('adminobs.colCalls'), 'calls')}</div>
        <div>{head(t('adminobs.colInput'), 'inputTokens')}</div>
        <div>{head(t('adminobs.colOutput'), 'outputTokens')}</div>
        <div>{head(t('adminobs.colCacheHit'), 'cacheHitRate')}</div>
        <div>{head(t('adminobs.colFailed'), 'failureRate')}</div>
      </div>
      {sorted.map((r, i) => {
        const m = metaFor(r.purpose)
        return (
          <button
            type="button"
            className="obs-row obs-row-button"
            key={`${r.purpose}-${r.model}-${r.source}-${i}`}
            onClick={() => onDrill(r)}
            title={t('adminobs.rollupRowTitle')}
          >
            <div className="obs-cell-purpose">
              <span className="obs-dot" style={{ background: m.swatch }} aria-hidden />
              <div className="obs-cell-purpose-text">
                <div className="obs-cell-purpose-label">{purposeLabel(t, r.purpose)}</div>
                <div className="obs-cell-purpose-blurb">{purposeBlurb(t, r.purpose)}</div>
              </div>
            </div>
            <div className="obs-cell-model">
              <div className="obs-mono">{r.model}</div>
              <div className="obs-cell-source">
                {r.source}
                {/* BYOA cost is meter-equivalent — what the same tokens WOULD
                    cost on the metered API. The operator's actual bill is a
                    flat subscription. Flagging this on the row keeps the $
                    column honest without hiding the comparable signal. */}
                {unit === 'usd' && isByoaSource(r.source) && <span className="obs-meter-flag" title={t('adminobs.meterFlagTitle')}>·meter</span>}
                {unit === 'usd' && r.costEstimated && <span className="obs-est-flag" title={t('adminobs.estFlagTitle')}>·est</span>}
              </div>
            </div>
            <div className="obs-cell-num obs-cell-cost">{unit === 'usd' ? fmtUsd(r.costUsd, r.costUsd < 1 ? 4 : 2) : fmtTokens(r.totalTok)}</div>
            <div className="obs-cell-num">{fmtInt(r.calls)}</div>
            <div className="obs-cell-num">
              {fmtTokens(r.inputTokens + r.cachedInputTokens)}
              {r.cachedInputTokens > 0 && <span className="obs-cell-sub"> · {fmtTokens(r.cachedInputTokens)} {t('adminobs.cachedSuffix')}</span>}
            </div>
            <div className="obs-cell-num">{fmtTokens(r.outputTokens)}</div>
            <div className="obs-cell-num">{r.cacheHitRate > 0 ? fmtPct(r.cacheHitRate, 0) : '—'}</div>
            <div className="obs-cell-num">
              {r.failureRate > 0
                ? <span style={{ color: r.failureRate > 0.1 ? 'var(--coral-deep)' : 'var(--ink-700)' }}>{fmtPct(r.failureRate, 1)}</span>
                : '—'}
              {r.rateLimitedCalls > 0 && <span className="obs-cell-sub"> · {r.rateLimitedCalls} {t('adminobs.rlSuffix')}</span>}
            </div>
          </button>
        )
      })}
    </div>
  )
}

/** Small round agent avatar: portrait image when present, colored-initial
 *  fallback otherwise (mirrors the app's Avatar fallback, but self-contained so
 *  the admin bundle needn't construct a full Participant). */
function AgentAvatar({ url, initial, bg, size = 26 }: { url: string | null; initial: string | null; bg: string | null; size?: number }) {
  const [broke, setBroke] = useState(false)
  const showImg = !!url && !broke
  return (
    <span className="obs-agent-avatar" style={{ width: size, height: size, background: showImg ? 'transparent' : (bg ?? 'var(--ink-200)') }} aria-hidden>
      {showImg
        ? <img src={url} alt="" width={size} height={size} onError={() => setBroke(true)} />
        : <span>{initial ?? '?'}</span>}
    </span>
  )
}

export function TopAgentsTable({ rows, unit, loading, onDrill, t }: { rows: LlmObservabilityPayload['topAgents']; unit: Unit; loading: boolean; onDrill: (r: LlmTopAgentRow) => void; t: ReturnType<typeof useT> }) {
  // Server ranks by $ desc; in token mode re-rank client-side (only ~20 rows)
  // so the biggest token consumer — not the biggest spender — sits on top.
  const sorted = useMemo(
    () => unit === 'usd' ? rows : [...rows].sort((a, b) => totalTokens(b) - totalTokens(a)),
    [rows, unit],
  )
  if (loading && rows.length === 0) return <div className="obs-empty">{t('adminobs.loading')}</div>
  if (!loading && rows.length === 0) return <div className="obs-empty">{t('adminobs.noAgentSpend')}</div>
  return (
    <div className="obs-table obs-table-compact">
      <div className="obs-thead obs-thead-compact">
        <div className="obs-th-left">{t('adminobs.colAgent')}</div>
        <div className="obs-th-left">{t('adminobs.colCompany')}</div>
        <div className="obs-th-right">{unit === 'usd' ? t('adminobs.colCost') : t('adminobs.tokensLabel')}</div>
        <div className="obs-th-right">{t('adminobs.colCalls')}</div>
      </div>
      {sorted.map((r, i) => (
        <button
          type="button"
          className={`obs-row obs-row-compact${r.agentId ? ' obs-row-button' : ''}`}
          key={`${r.agentId ?? 'anon'}-${i}`}
          onClick={() => r.agentId && onDrill(r)}
          disabled={!r.agentId}
          title={r.agentId ? t('adminobs.agentRowTitle') : t('adminobs.agentRowTitleAnon')}
        >
          <div className="obs-cell-agent">
            {r.agentId && <AgentAvatar url={r.agentAvatarUrl} initial={r.agentInitial} bg={r.agentAvatarBg} />}
            <div className="obs-cell-agent-text">
              <div className="obs-cell-purpose-label">{r.agentName ?? (r.agentId ? <span className="obs-mono">{r.agentId.slice(0, 12)}</span> : <em>—</em>)}</div>
              {r.agentId && <div className="obs-mono obs-cell-sub">{r.agentId}</div>}
            </div>
          </div>
          <div className="obs-cell-sub">
            {r.companyName ?? (r.companyId ? <span className="obs-mono">{r.companyId}</span> : '—')}
          </div>
          <div className="obs-cell-num obs-cell-cost">
            {unit === 'usd' ? fmtUsd(r.costUsd, r.costUsd < 1 ? 4 : 2) : fmtTokens(totalTokens(r))}
            {unit !== 'usd' && r.cachedInputTokens > 0 && <span className="obs-cell-sub"> · {fmtTokens(r.cachedInputTokens)} {t('adminobs.cachedSuffix')}</span>}
          </div>
          <div className="obs-cell-num">{fmtInt(r.calls)}</div>
        </button>
      ))}
    </div>
  )
}

// ─── Daemon-version rollup ────────────────────────────────────────────────
//
// "After v0.1.X shipped, did average cost-per-hop go up?" The single most
// useful release-regression-spotter the page has. Each row is one
// (daemonVersion × source) combo, sorted by lastSeen DESC so the freshest
// version sits at the top. Cache hit % is the headline column — a release
// that broke session caching shows up here as a giant drop.
//
// Same visual shape as TopAgents/Rollup so the page reads as a family. The
// "last seen" column shows a relative timestamp so the operator immediately
// knows whether a version is still live.

export function DaemonVersionTable({ rows, unit, loading, t }: {
  rows: LlmDaemonVersionRow[]
  unit: Unit
  loading: boolean
  t: ReturnType<typeof useT>
}) {
  if (loading && rows.length === 0) return <div className="obs-empty">{t('adminobs.loading')}</div>
  if (!loading && rows.length === 0) return (
    <div className="obs-empty">
      {t('adminobs.noDaemonData')}
    </div>
  )
  return (
    <div className="obs-table obs-table-daemon">
      <div className="obs-thead obs-thead-daemon">
        <div className="obs-th-left">{t('adminobs.colVersion')}</div>
        <div className="obs-th-left">{t('adminobs.colSource')}</div>
        <div className="obs-th-right">{t('adminobs.colCalls')}</div>
        <div className="obs-th-right">{unit === 'usd' ? t('adminobs.colCost') : t('adminobs.tokensLabel')}</div>
        <div className="obs-th-right">{t('adminobs.colCacheHit')}</div>
        <div className="obs-th-right">{t('adminobs.colAvgCall')}</div>
        <div className="obs-th-right">{t('adminobs.colFailed')}</div>
        <div className="obs-th-right">{t('adminobs.colLastSeen')}</div>
      </div>
      {rows.map((r, i) => {
        const hitRate = (r.inputTokens + r.cachedInputTokens) > 0 ? cacheHitRate(r.inputTokens, r.cachedInputTokens) : null
        const totalTok = r.inputTokens + r.cachedInputTokens + r.outputTokens
        const avgCost = r.calls > 0 ? r.costUsd / r.calls : 0
        const avgTok = r.calls > 0 ? totalTok / r.calls : 0
        return (
          <div className="obs-row obs-row-daemon" key={`${r.daemonVersion}-${r.source}-${i}`}>
            <div className="obs-cell-version">
              <span className="obs-cell-purpose-label">{t('adminobs.versionPrefix', { version: r.daemonVersion })}</span>
            </div>
            <div className="obs-cell-source">{r.source}{isByoaSource(r.source) && <span className="obs-meter-flag">·meter</span>}</div>
            <div className="obs-cell-num">{fmtInt(r.calls)}</div>
            <div className="obs-cell-num obs-cell-cost">{unit === 'usd' ? fmtUsd(r.costUsd, r.costUsd < 1 ? 4 : 2) : fmtTokens(totalTok)}</div>
            <div className="obs-cell-num">{hitRate != null ? <span className={cacheToneClass(hitRate)}>{fmtPct(hitRate, 1)}</span> : '—'}</div>
            <div className="obs-cell-num">{unit === 'usd' ? fmtUsd(avgCost, 6) : fmtTokens(avgTok)}</div>
            <div className="obs-cell-num">{r.failureRate > 0 ? <span style={{ color: r.failureRate > 0.1 ? 'var(--coral-deep)' : 'var(--ink-700)' }}>{fmtPct(r.failureRate, 1)}</span> : '—'}</div>
            <div className="obs-cell-num obs-cell-sub-only" title={new Date(r.lastSeen).toLocaleString()}>{relativeTime(r.lastSeen, t)}</div>
          </div>
        )
      })}
    </div>
  )
}

/** Tiny "now-3h" / "2d ago" formatter for the "Last seen" column. The full
 *  timestamp lives in the title attribute. 分部计算在 @/lib/format(#147 ②),
 *  这里只做本页 i18n 键族映射。 */
function relativeTime(iso: string, t: ReturnType<typeof useT>): string {
  const parts = relativeTimeParts(iso)
  if (!parts) return '—'
  switch (parts.unit) {
    case 'sec':  return t('adminobs.timeAgoSec',  { n: parts.n })
    case 'min':  return t('adminobs.timeAgoMin',  { n: parts.n })
    case 'hour': return t('adminobs.timeAgoHour', { n: parts.n })
    default:     return t('adminobs.timeAgoDay',  { n: parts.n })
  }
}
