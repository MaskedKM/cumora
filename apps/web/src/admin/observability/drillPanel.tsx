import { useEffect, useState } from 'react'
import {
  cacheHitRate,
  fmtInt,
  fmtPct,
  fmtTokens,
  fmtUsdCompact as fmtUsd,
} from '@/lib/format'
import type { MessageKey, useT } from '@/lib/i18n'
import { adminApi, type LlmCallPurpose, type LlmCallRow, type LlmCallSource } from '../api'
import { isByoaSource, metaFor, purposeLabel, totalTokens, type Unit } from './shared'

/** What the drill-down panel is currently showing. `null` = closed; a value
 *  is the filter set that defines which rows the panel queries (and shows in
 *  its header chip). The page owns this state so the panel can be re-opened
 *  with a different bucket without unmounting (state inside the panel — sort
 *  / refresh — stays put on bucket switch). */
export type DrillDown =
  | { kind: 'bucket'; purpose: LlmCallPurpose; model: string; source: LlmCallSource }
  | { kind: 'run'; runId: string }
  | { kind: 'agent'; agentId: string; agentName: string | null }

// ─── Drill-down panel ────────────────────────────────────────────────────
//
// The "no SQL needed" view. Opens from any rollup row click; shows the actual
// llm_calls rows that drove that bucket, each with its extras JSONB expanded.
// Three open shapes:
//
//   - bucket   — clicked a rollup row → narrowed to one (purpose, model, source)
//   - run      — clicked "view this run's trail" inside an open panel → all
//                hops for ONE run_id, sorted by hopIndex / created_at
//   - agent    — clicked an agent in the Top Spenders table → all calls by
//                that agent_id in the window
//
// The panel owns its own local sort + refresh state. Switching `drill`
// objects from outside refetches but keeps the sort UI sticky.

type CallSort = 'cost' | 'latency' | 'hop' | 'created'
const SORT_LABEL: Record<CallSort, MessageKey> = {
  cost: 'adminobs.sortCost',
  latency: 'adminobs.sortLatency',
  hop: 'adminobs.sortHop',
  created: 'adminobs.sortCreated',
}

export function DrillPanel({ drill, sinceDays, companyId, unit, refreshSignal, onClose, onJumpToRun, onJumpToAgent, t }: {
  drill: DrillDown | null
  sinceDays: number
  /** Inherits the page's tenant scope so a clicked rollup row stays inside
   *  that tenant. When undefined, the drill ranges over all accounts. */
  companyId?: string
  unit: Unit
  /** Bumped by the page's refresh button / auto-refresh so an open panel
   *  re-fetches its (always-live, raw) rows in lockstep with the dashboard. */
  refreshSignal: number
  onClose: () => void
  onJumpToRun: (runId: string) => void
  onJumpToAgent: (agentId: string, agentName: string | null) => void
  t: ReturnType<typeof useT>
}) {
  const [rows, setRows] = useState<LlmCallRow[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [sortBy, setSortBy] = useState<CallSort>('cost')

  // Re-fetch when the drill target OR the sort changes. sinceDays + the
  // page's tenant scope are also relevant — a tighter window means fewer rows
  // even within one bucket; flipping accounts narrows the rows further.
  useEffect(() => {
    if (!drill) { setRows(null); return }
    let cancelled = false
    setLoading(true); setErr(null)
    const params: Parameters<typeof adminApi.observabilityLlmCalls>[0] = { sinceDays, limit: 100, sortBy }
    if (drill.kind === 'bucket') { params.purpose = drill.purpose; params.model = drill.model; params.source = drill.source }
    else if (drill.kind === 'run')   { params.runId = drill.runId }
    else if (drill.kind === 'agent') { params.agentId = drill.agentId }
    if (companyId) params.companyId = companyId
    adminApi.observabilityLlmCalls(params)
      .then((r) => { if (!cancelled) setRows(r) })
      .catch((e: unknown) => { if (!cancelled) setErr(e instanceof Error ? e.message : String(e)) })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [drill, sinceDays, sortBy, companyId, refreshSignal])

  // ESC closes — common dashboard expectation, free implementation here.
  useEffect(() => {
    if (!drill) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [drill, onClose])

  if (!drill) return null

  const meta = drill.kind === 'bucket' ? metaFor(drill.purpose) : null
  const title =
    drill.kind === 'bucket' ? purposeLabel(t, drill.purpose)
    : drill.kind === 'run'   ? t('adminobs.drillRunTitle')
    : drill.kind === 'agent' ? (drill.agentName ?? t('adminobs.drillAgentTitle'))
    : '—'
  const subtitle =
    drill.kind === 'bucket' ? <><span className="obs-mono">{drill.model}</span> · {drill.source}</>
    : drill.kind === 'run'   ? <span className="obs-mono">{drill.runId}</span>
    : drill.kind === 'agent' ? <span className="obs-mono">{drill.agentId}</span>
    : null

  return (
    <>
      <div className="obs-drill-scrim" onClick={onClose} aria-hidden />
      <aside className="obs-drill" role="dialog" aria-label={`Drill-down: ${title}`}>
        <header className="obs-drill-head">
          <div className="obs-drill-titlebar">
            {meta && <span className="obs-dot" style={{ background: meta.swatch }} aria-hidden />}
            <div>
              <div className="obs-drill-title">{title}</div>
              <div className="obs-drill-sub">{subtitle}</div>
            </div>
          </div>
          <button type="button" className="obs-drill-close" onClick={onClose} aria-label={t('adminobs.closeAria')}>×</button>
        </header>

        <div className="obs-drill-sortbar">
          <span className="obs-drill-sortbar-label">{t('adminobs.sortLabel')}</span>
          {(Object.entries(SORT_LABEL) as [CallSort, MessageKey][]).map(([s, labelKey]) => (
            <button
              key={s}
              type="button"
              className={`obs-pill${sortBy === s ? ' is-active' : ''}`}
              onClick={() => setSortBy(s)}
            >{t(labelKey)}</button>
          ))}
        </div>

        <div className="obs-drill-body">
          {err && <div className="obs-error">{t('adminobs.loadFailed', { error: err })}</div>}
          {loading && !rows && <div className="obs-empty">{t('adminobs.loading')}</div>}
          {rows && rows.length === 0 && <div className="obs-empty">{t('adminobs.noCalls')}</div>}
          {rows && rows.map((c) => (
            <DrillCallCard
              key={c.id}
              call={c}
              unit={unit}
              onJumpToRun={onJumpToRun}
              onJumpToAgent={onJumpToAgent}
              t={t}
            />
          ))}
        </div>
      </aside>
    </>
  )
}

function DrillCallCard({ call, unit, onJumpToRun, onJumpToAgent, t }: {
  call: LlmCallRow
  unit: Unit
  onJumpToRun: (runId: string) => void
  onJumpToAgent: (agentId: string, agentName: string | null) => void
  t: ReturnType<typeof useT>
}) {
  // Cache hit %: cached / (uncached + cached). NaN-safe(共享件,#147 ②)。
  const cacheHit = (call.inputTokens + call.cachedInputTokens) > 0
    ? cacheHitRate(call.inputTokens, call.cachedInputTokens)
    : null
  // The five extras we WANT to surface as first-class chips when present
  // (these are the ones the engine + daemon write). Anything else still
  // shows up via the generic kv list below so nothing's invisible.
  const ex = call.extras ?? {}
  const hopIndex = typeof ex.hopIndex === 'number' ? ex.hopIndex : (typeof ex.hop === 'number' ? ex.hop : null)
  const toolUses = typeof ex.toolUses === 'number' ? ex.toolUses : null
  const textChars = typeof ex.textChars === 'number' ? ex.textChars : null
  const itemsDropped = typeof ex.itemsDropped === 'number' ? ex.itemsDropped : null
  const inputTokensBefore = typeof ex.inputTokensBefore === 'number' ? ex.inputTokensBefore : null
  // Compaction's compression ratio is THE diagnostic: tokens in vs out.
  const compressionRatio = call.purpose === 'compaction' && inputTokensBefore && call.outputTokens > 0
    ? Math.round(inputTokensBefore / call.outputTokens)
    : null

  // Other extras → generic kv list (ones we haven't already chipped).
  const KNOWN_KEYS = new Set(['hopIndex', 'hop', 'toolUses', 'textChars', 'itemsDropped', 'inputTokensBefore'])
  const otherExtras: Array<[string, unknown]> = Object.entries(ex).filter(([k]) => !KNOWN_KEYS.has(k))

  const m = metaFor(call.purpose)
  return (
    <article className="obs-drill-card">
      <div className="obs-drill-card-head">
        <span className="obs-dot" style={{ background: m.swatch }} aria-hidden />
        <div className="obs-drill-card-headtext">
          <div className="obs-drill-card-title">
            {purposeLabel(t, call.purpose)}
            <span className="obs-drill-card-when">· {new Date(call.createdAt).toLocaleString()}</span>
          </div>
          <div className="obs-drill-card-sub">
            <span className="obs-mono">{call.model}</span>
            <span> · {call.source}</span>
            {unit === 'usd' && call.costEstimated && <span className="obs-est-flag">·est</span>}
            {unit === 'usd' && isByoaSource(call.source) && <span className="obs-meter-flag">·meter</span>}
            {call.daemonVersion && (
              <span className="obs-version-flag" title={t('adminobs.daemonVersionTitle', { version: call.daemonVersion })}>
                · {t('adminobs.versionPrefix', { version: call.daemonVersion })}
              </span>
            )}
            <span className={call.status === 'ok' ? '' : 'obs-drill-status-bad'}> · {call.status}</span>
          </div>
        </div>
        <div className="obs-drill-card-cost">{unit === 'usd' ? fmtUsd(call.costUsd, call.costUsd < 1 ? 4 : 2) : fmtTokens(totalTokens(call))}</div>
      </div>

      {/* First-class chips for the high-signal extras. Order is intentional
          (hop index → tool calls → output mix → compaction diagnostic), so
          the eye scans the bottleneck first. */}
      <div className="obs-drill-chips">
        {hopIndex != null  && <Chip label={t('adminobs.chipHop')}      value={`#${hopIndex}`} />}
        {toolUses != null  && <Chip label={t('adminobs.chipTools')}    value={fmtInt(toolUses)} />}
        {textChars != null && <Chip label={t('adminobs.chipText')}     value={`${fmtInt(textChars)}c`} />}
        {compressionRatio  && <Chip label={t('adminobs.chipCompress')} value={`${compressionRatio}×`} title={t('adminobs.compressTitle')} />}
        {itemsDropped != null && <Chip label={t('adminobs.chipDropped')} value={fmtInt(itemsDropped)} />}
        {call.latencyMs && call.latencyMs > 0 && <Chip label={t('adminobs.chipLatency')} value={`${(call.latencyMs / 1000).toFixed(1)}s`} />}
      </div>

      <div className="obs-drill-numgrid">
        <NumCell label={t('adminobs.cellInput')} value={fmtTokens(call.inputTokens)} />
        <NumCell label={t('adminobs.cellCached')} value={fmtTokens(call.cachedInputTokens)} hint={cacheHit != null ? fmtPct(cacheHit, 0) : undefined} />
        <NumCell label={t('adminobs.cellOutput')} value={fmtTokens(call.outputTokens)} />
        <NumCell label={t('adminobs.cellReasoning')} value={call.reasoningTokens > 0 ? fmtTokens(call.reasoningTokens) : '—'} />
      </div>

      {call.error && (
        <div className="obs-drill-err">
          <div className="obs-drill-err-label">{t('adminobs.errorLabel')}</div>
          <div className="obs-drill-err-body">{call.error}</div>
        </div>
      )}

      {otherExtras.length > 0 && (
        <details className="obs-drill-rawextras">
          <summary>{t('adminobs.extrasLabel', { n: otherExtras.length })}</summary>
          <dl>
            {otherExtras.map(([k, v]) => (
              <div key={k} className="obs-drill-kv">
                <dt>{k}</dt>
                <dd>{typeof v === 'object' ? JSON.stringify(v) : String(v)}</dd>
              </div>
            ))}
          </dl>
        </details>
      )}

      <footer className="obs-drill-card-foot">
        {call.runId && (
          <button type="button" className="obs-drill-link" onClick={() => onJumpToRun(call.runId!)}>
            {t('adminobs.runTrailLabel', { id: call.runId.slice(0, 8) + '…' })}
          </button>
        )}
        {call.agentId && (
          <button type="button" className="obs-drill-link" onClick={() => onJumpToAgent(call.agentId!, call.agentName)}>
            {call.agentName ?? t('adminobs.agentLabel', { name: call.agentId.slice(0, 8) + '…' })}
          </button>
        )}
      </footer>
    </article>
  )
}

function Chip({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <span className="obs-drill-chip" title={title}>
      <span className="obs-drill-chip-k">{label}</span>
      <span className="obs-drill-chip-v">{value}</span>
    </span>
  )
}

function NumCell({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="obs-drill-num">
      <div className="obs-drill-num-label">{label}</div>
      <div className="obs-drill-num-value">{value}</div>
      {hint && <div className="obs-drill-num-hint">{hint}</div>}
    </div>
  )
}
