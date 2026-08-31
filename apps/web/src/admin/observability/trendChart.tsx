import { useMemo } from 'react'
import {
  fmtTokens,
  fmtUsdCompact as fmtUsd,
} from '@/lib/format'
import { useT } from '@/lib/i18n'
import type { LlmCallPurpose, LlmRollupRow, LlmTrendBucket } from '../api'
import { metaFor, purposeBlurb, purposeLabel, totalTokens, type Unit } from './shared'

// ─── Trend chart (stacked area) ──────────────────────────────────────────

/** Render the trend as a stacked area SVG. We do this by hand (no chart lib)
 *  because:
 *   - The shape is fixed (stacked over time) — no library flexibility needed.
 *   - The page is admin-only; pulling in recharts would be a runtime tax for
 *     a page two people will ever load.
 *   - Inline SVG gives us pixel-perfect control over the paper palette. */
export function TrendChart({ buckets, unit, loading, t }: { buckets: LlmTrendBucket[]; unit: Unit; loading: boolean; t: ReturnType<typeof useT> }) {
  // Trend buckets carry $ and input tokens (cached + uncached) but no output —
  // so the token series is total INPUT tokens, which the card sub spells out.
  const val = (b: LlmTrendBucket): number => unit === 'usd' ? b.costUsd : b.inputTokens + b.cachedInputTokens
  // Width is responsive via viewBox; height fixed. Stack order: largest total
  // cost on the bottom so the eye reads "the big slice is at the floor".
  const W = 1000
  const H = 220
  const PADL = 56, PADR = 16, PADT = 16, PADB = 32

  // Bucket → { day, [purpose]: cost }
  const days = useMemo(() => {
    const set = new Set<string>()
    for (const b of buckets) set.add(b.day)
    return [...set].sort()
  }, [buckets])

  const { purposes, series, maxStack } = useMemo(() => {
    const totalByPurpose = new Map<LlmCallPurpose, number>()
    const byDay = new Map<string, Map<LlmCallPurpose, number>>()
    for (const b of buckets) {
      totalByPurpose.set(b.purpose, (totalByPurpose.get(b.purpose) ?? 0) + val(b))
      const m = byDay.get(b.day) ?? new Map<LlmCallPurpose, number>()
      m.set(b.purpose, (m.get(b.purpose) ?? 0) + val(b))
      byDay.set(b.day, m)
    }
    const purposes = [...totalByPurpose.entries()]
      .sort(([, a], [, b]) => b - a)   // largest contributor at the bottom
      .map(([p]) => p)
    const series = days.map((d) => {
      const m = byDay.get(d) ?? new Map<LlmCallPurpose, number>()
      return purposes.map((p) => m.get(p) ?? 0)
    })
    const totalByDay = series.map((row) => row.reduce((a, b) => a + b, 0))
    const maxStack = Math.max(0.000001, ...totalByDay)
    return { purposes, series, totalByDay, maxStack }
  }, [buckets, days, unit])

  if (loading) return <div className="obs-chart-empty">{t('adminobs.loading')}</div>
  if (days.length === 0) return <div className="obs-chart-empty">{t('adminobs.noData')}</div>

  const innerW = W - PADL - PADR
  const innerH = H - PADT - PADB
  // X for each day; if only one day, center it.
  const xFor = (i: number) => days.length === 1
    ? PADL + innerW / 2
    : PADL + (i / (days.length - 1)) * innerW
  const yFor = (cum: number) => PADT + (1 - cum / maxStack) * innerH

  // Build a polygon per purpose: top edge = cumulative-above (or 0 for floor
  // layer), bottom edge = cumulative-above + this layer's value.
  type Layer = { purpose: LlmCallPurpose; path: string }
  const layers: Layer[] = []
  const cumBelow = new Array(days.length).fill(0)
  // We draw from largest contributor (bottom of stack) to smallest (top).
  for (let li = 0; li < purposes.length; li++) {
    const purpose = purposes[li]!
    const top: Array<[number, number]> = []
    const bot: Array<[number, number]> = []
    for (let i = 0; i < days.length; i++) {
      const v = series[i]![li]!
      const below = cumBelow[i]
      bot.push([xFor(i), yFor(below)])
      top.push([xFor(i), yFor(below + v)])
      cumBelow[i] = below + v
    }
    const path = [
      ...bot.map(([x, y], i) => `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`),
      ...top.reverse().map(([x, y]) => `L${x.toFixed(1)},${y.toFixed(1)}`),
      'Z',
    ].join(' ')
    layers.push({ purpose, path })
  }

  // Y-axis: 3 horizontal grid lines at 0/50/100% of max.
  const ticks = [0, 0.5, 1].map((f) => ({
    y: PADT + (1 - f) * innerH,
    label: unit === 'usd' ? fmtUsd(maxStack * f) : fmtTokens(maxStack * f),
  }))

  // Sparse x-axis labels — first, middle, last so the chart doesn't get a
  // cluttered date strip at the bottom.
  const xLabels = days.length <= 4
    ? days.map((d, i) => ({ x: xFor(i), label: d.slice(5) }))
    : [
        { x: xFor(0),                 label: days[0]!.slice(5) },
        { x: xFor(Math.floor((days.length - 1) / 2)), label: days[Math.floor((days.length - 1) / 2)]!.slice(5) },
        { x: xFor(days.length - 1),   label: days[days.length - 1]!.slice(5) },
      ]

  return (
    <div className="obs-chart">
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label="Daily cost by purpose">
        {/* Grid */}
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={PADL} x2={W - PADR} y1={t.y} y2={t.y} stroke="var(--ink-100)" strokeWidth={1} />
            <text x={PADL - 8} y={t.y + 4} textAnchor="end" fontSize={10} fill="var(--ink-500)">{t.label}</text>
          </g>
        ))}
        {/* Stacks */}
        {layers.map((l) => {
          const m = metaFor(l.purpose)
          return (
            <path key={l.purpose} d={l.path} fill={m.swatch} fillOpacity={0.85} stroke={m.swatch} strokeWidth={0.5}>
              <title>{purposeLabel(t, l.purpose)}</title>
            </path>
          )
        })}
        {/* X labels */}
        {xLabels.map((l, i) => (
          <text key={i} x={l.x} y={H - 10} textAnchor="middle" fontSize={10} fill="var(--ink-500)">{l.label}</text>
        ))}
      </svg>
    </div>
  )
}

export function PurposeLegend({ rollup, unit }: { rollup: LlmRollupRow[]; unit: Unit }) {
  const t = useT()
  // Aggregate by purpose so legend dots show the TOTAL too — answers "which
  // color is the big one?" without re-scanning the chart. Uses the active unit.
  const totals = new Map<LlmCallPurpose, number>()
  for (const r of rollup) totals.set(r.purpose, (totals.get(r.purpose) ?? 0) + (unit === 'usd' ? r.costUsd : totalTokens(r)))
  const sorted = [...totals.entries()].sort(([, a], [, b]) => b - a)
  if (sorted.length === 0) return null
  return (
    <div className="obs-legend">
      {sorted.map(([p, cost]) => {
        const m = metaFor(p)
        return (
          <span key={p} className="obs-legend-chip" title={purposeBlurb(t, p)}>
            <span className="obs-legend-dot" style={{ background: m.swatch }} />
            <span className="obs-legend-label">{purposeLabel(t, p)}</span>
            <span className="obs-legend-cost">{unit === 'usd' ? fmtUsd(cost) : fmtTokens(cost)}</span>
          </span>
        )
      })}
    </div>
  )
}
