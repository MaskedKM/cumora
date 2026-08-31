import type { ApiTriageEconomics } from '@/api/client'
import { Select } from '@/components/Select'
import {
  fmtPct as fmtPctPlaces,
  fmtTokens,
  fmtUsdPrecise,
} from '@/lib/format'
import { type MessageKey, useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { RefreshButton } from './RefreshButton'
import { clock, DEV_PANELS, type DevPanel, PANEL_LABEL_KEY } from './shared'

const TRIAGE_WINDOWS: { hours: number; key: MessageKey }[] = [
  { hours: 6, key: 'obs.window6h' }, { hours: 24, key: 'obs.window24h' }, { hours: 72, key: 'obs.window3d' }, { hours: 168, key: 'obs.window7d' },
]

// 格式化与相对时间分部共享 @/lib/format(#147 ②);本页 $ 用精确小数语体,
// 百分比固定 0 位 —— 一行委托维持调用点零改动。fmtTok 换共享档位版
// (1.2k → 1.2K,纯字面差)。
const fmtUsd = fmtUsdPrecise
const fmtPct = (n: number): string => fmtPctPlaces(n, 0)
const fmtTok = fmtTokens

function StatCard({ label, value, sub, tone }: {
  label: string; value: string; sub?: string; tone?: 'pos' | 'neg' | 'warn' | 'muted'
}) {
  const cls = tone === 'neg' ? 'text-coral-deep' : tone === 'warn' ? 'text-gold-deep' : tone === 'muted' ? 'text-ink-400' : 'text-ink-900'
  const style = tone === 'pos' ? { color: 'var(--avail)' } : undefined
  return (
    <div className="rounded-[10px] border border-ink-100 bg-paper px-3.5 py-3">
      <div className="text-[9.5px] font-bold uppercase tracking-[0.12em] text-ink-300">{label}</div>
      <div className={cn('mt-1 font-display text-[20px] font-medium tabular-nums', cls)} style={style}>{value}</div>
      {sub && <div className="mt-0.5 text-[10.5px] leading-snug text-ink-400">{sub}</div>}
    </div>
  )
}

/** The full token unit-price menu — small brain vs big brain, always visible so
 *  the user knows the rates the estimates rest on. Role is a heuristic on the id
 *  (haiku/mini = cerebellum/small; everything else = main/big). */
function PriceMenuTable({ rows }: { rows: { model: string; inPer1M: number; cachedInPer1M: number; outPer1M: number; estimated: boolean }[] }) {
  const t = useT()
  if (rows.length === 0) return null
  // Fixed cerebellum aliases: claude→haiku, codex→gpt-5.4-mini,
  // grok→grok-4.5. Cursor has no fixed cheap alias and reports the model its
  // account selected, so it is classified by that model id.
  const isSmall = (m: string) => /haiku|mini|grok-4\.5/i.test(m)
  return (
    <div className="rounded-[10px] border border-ink-100 bg-paper px-3.5 py-3">
      <div className="text-[9.5px] font-bold uppercase tracking-[0.12em] text-ink-300">{t('obs.priceMenu')}</div>
      <table className="mt-2 w-full text-[11.5px]">
        <thead className="text-[9px] font-bold uppercase tracking-[0.1em] text-ink-400">
          <tr>
            <th className="py-1 text-left">{t('obs.colTier')}</th>
            <th className="py-1 text-left">{t('obs.colModel')}</th>
            <th className="py-1 text-right">{t('obs.colInput')}</th>
            <th className="py-1 text-right">{t('obs.colCacheRead')}</th>
            <th className="py-1 text-right">{t('obs.colOutput')}</th>
            <th className="py-1" />
          </tr>
        </thead>
        <tbody className="divide-y divide-ink-100">
          {rows.map((p) => (
            <tr key={p.model}>
              <td className="py-1">
                <span className={cn('rounded-full px-1.5 py-0.5 text-[9px] font-bold uppercase', isSmall(p.model) ? 'bg-ink-100 text-ink-600' : 'bg-sky2-50 text-skype-deep')}>
                  {isSmall(p.model) ? t('obs.tierSmall') : t('obs.tierBig')}
                </span>
              </td>
              <td className="py-1 font-mono text-ink-700">{p.model}</td>
              <td className="py-1 text-right tabular-nums text-ink-600">${p.inPer1M}</td>
              <td className="py-1 text-right tabular-nums text-ink-600">${p.cachedInPer1M}</td>
              <td className="py-1 text-right tabular-nums text-ink-600">${p.outPer1M}</td>
              <td className="py-1 text-right">{p.estimated && <span className="rounded bg-coral-soft px-1.5 py-0.5 text-[9px] font-bold uppercase text-coral-deep">{t('obs.est')}</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** The triage cost-effectiveness ledger — a self-contained full-width panel.
 *  Answers the user's question: is the small-brain gate actually saving money,
 *  given triage is always cold (uncached) while big-brain turns are cache-warm? */
export function TriageEconomicsPanel(props: {
  panel: DevPanel
  setPanel: (p: DevPanel) => void
  agents: { id: string; name: string }[]
  agentId: string
  setAgentId: (v: string) => void
  hours: number
  setHours: (n: number) => void
  data: ApiTriageEconomics | null
  loading: boolean
  err: string | null
  onRefresh: () => void
}) {
  const t = useT()
  const { data } = props
  const net = data?.estimatedNetSavingsUsd ?? 0
  return (
    <main className="flex h-full min-h-0 flex-col overflow-hidden bg-paper">
      <div className="border-b border-ink-100 px-6 py-4">
        <div className="flex items-start justify-between gap-3">
          <div>
            <h1 className="font-display text-[25px] font-medium tracking-tight text-ink-900">{t('obs.triageTitle')}</h1>
            <div className="mt-1 text-[12px] text-ink-500">
              {t('obs.triageSubtitle')}
            </div>
          </div>
          <RefreshButton loading={props.loading} onClick={props.onRefresh} />
        </div>
        <div className="mt-4 flex flex-wrap items-end gap-3">
          <div className="grid grid-cols-3 gap-1 rounded-[11px] border border-ink-100 bg-cloud p-1">
            {DEV_PANELS.map((item) => (
              <button
                key={item}
                onClick={() => props.setPanel(item)}
                className={cn(
                  'rounded-[8px] px-3 py-2 text-[12px] font-semibold transition',
                  props.panel === item ? 'bg-sky2-50 text-skype-deep shadow-soft' : 'text-ink-500 hover:text-ink-700',
                )}
              >
                {t(PANEL_LABEL_KEY[item])}
              </button>
            ))}
          </div>
          <label className="text-[10.5px] font-bold uppercase tracking-[0.12em] text-ink-400">
            {t('obs.agent')}
            <Select
              value={props.agentId}
              onValueChange={props.setAgentId}
              options={[{ value: 'all', label: t('obs.allAgents') }, ...props.agents.map((a) => ({ value: a.id, label: a.name }))]}
              className="mt-1 w-44 normal-case tracking-normal"
            />
          </label>
          <label className="text-[10.5px] font-bold uppercase tracking-[0.12em] text-ink-400">
            {t('obs.window')}
            <Select<string>
              value={String(props.hours)}
              onValueChange={(v) => props.setHours(Number(v))}
              options={TRIAGE_WINDOWS.map((w) => ({ value: String(w.hours), label: t(w.key) }))}
              className="mt-1 w-24 normal-case tracking-normal"
            />
          </label>
        </div>
      </div>

      {props.err && (
        <div className="mx-6 mt-4 rounded-[10px] bg-coral-soft px-3 py-2 text-[12px] text-coral-deep">{props.err}</div>
      )}

      <div className="min-h-0 flex-1 overflow-auto px-6 py-5">
        {!data ? (
          <div className="grid h-full place-items-center text-[13px] text-ink-400">{props.loading ? t('common.loading') : t('common.noData')}</div>
        ) : (
          <div className="mx-auto max-w-[1100px] space-y-5">
            {/* ALWAYS-ON disclaimer: every $ here is an estimate. */}
            <div className="rounded-[10px] border border-coral-soft bg-coral-soft px-3.5 py-2.5 text-[11.5px] leading-[1.6] text-coral-deep">
              {t('obs.triageEstimateWarn')}
            </div>

            {data.triageCount === 0 ? (
              <>
                <PriceMenuTable rows={data.priceTable ?? []} />
                <div className="rounded-[10px] border border-ink-100 bg-cloud px-3.5 py-3 text-[12px] leading-[1.6] text-ink-500">
                  {t('obs.triageNoData')}
                </div>
              </>
            ) : (
            <>
            {/* The headline: estimated net savings = avoided big-brain spend − triage spend. */}
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
              <StatCard
                label={t('obs.statNetSavings')}
                value={`${net >= 0 ? '+' : ''}${fmtUsd(net)}`}
                tone={net >= 0 ? 'pos' : 'neg'}
                sub={net >= 0 ? t('obs.statNetSavingsPos') : t('obs.statNetSavingsNeg')}
              />
              <StatCard
                label={t('obs.statAvoided')}
                value={fmtUsd(data.estimatedAvoidedUsd)}
                tone="pos"
                sub={t('obs.statAvoidedSub', { skips: data.triageSkipCount, cost: fmtUsd(data.avgTurnCostUsd) })}
              />
              <StatCard label={t('obs.statTriageSpend')} value={fmtUsd(data.triageCostUsd)} sub={t('obs.statTriageSpendSub', { n: data.triageCount, measured: data.triageMeasuredCount })} />
              <StatCard label={t('obs.statOverhead')} value={fmtUsd(data.triageOverheadUsd)} tone={data.triageOverheadUsd > 0 ? 'warn' : undefined} sub={t('obs.statOverheadSub', { n: data.triageWakeCount })} />
              <StatCard label={t('obs.statCacheHit')} value={fmtPct(data.turnCacheHitRate)} sub={t('obs.statCacheHitSub', { turns: data.turnCount, cost: fmtUsd(data.avgTurnCostUsd) })} />
              <StatCard label={t('obs.statSkipRate')} value={data.triageCount > 0 ? fmtPct(data.triageSkipCount / data.triageCount) : '—'} sub={t('obs.statSkipRateSub', { tokens: fmtTok(data.triageInputTokens ?? 0) })} />
            </div>

            {/* The actual per-token unit prices the estimates rest on (table). */}
            {(data.unitPrices?.length ?? 0) > 0 && (
              <div className="rounded-[10px] border border-ink-100 bg-paper px-3.5 py-3">
                <div className="text-[9.5px] font-bold uppercase tracking-[0.12em] text-ink-300">{t('obs.priceMenuShort')}</div>
                <table className="mt-2 w-full text-[11.5px]">
                  <thead className="text-[9px] font-bold uppercase tracking-[0.1em] text-ink-400">
                    <tr>
                      <th className="py-1 text-left">{t('obs.colBrain')}</th>
                      <th className="py-1 text-left">{t('obs.colModel')}</th>
                      <th className="py-1 text-right">{t('obs.colInput')}</th>
                      <th className="py-1 text-right">{t('obs.colCacheRead')}</th>
                      <th className="py-1 text-right">{t('obs.colOutput')}</th>
                      <th className="py-1" />
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-ink-100">
                    {(data.unitPrices ?? []).map((p) => (
                      <tr key={`${p.role}:${p.model}`}>
                        <td className="py-1">
                          <span className={cn(
                            'rounded-full px-1.5 py-0.5 text-[9px] font-bold uppercase',
                            p.role === 'triage' ? 'bg-ink-100 text-ink-600' : 'bg-sky2-50 text-skype-deep',
                          )}>{p.role === 'triage' ? t('obs.tierSmall') : t('obs.tierBig')}</span>
                        </td>
                        <td className="py-1 font-mono text-ink-700">{p.model}</td>
                        <td className="py-1 text-right tabular-nums text-ink-600">${p.inPer1M}</td>
                        <td className="py-1 text-right tabular-nums text-ink-600">${p.cachedInPer1M}</td>
                        <td className="py-1 text-right tabular-nums text-ink-600">${p.outPer1M}</td>
                        <td className="py-1 text-right">{p.estimated && <span className="rounded bg-coral-soft px-1.5 py-0.5 text-[9px] font-bold uppercase text-coral-deep">{t('obs.est')}</span>}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            {data.byoaShare > 0 && (
              <div className="rounded-[10px] border border-gold bg-cloud px-3.5 py-2.5 text-[11px] leading-[1.6] text-gold-deep">
                {t('obs.byoaNote', { share: fmtPct(data.byoaShare) })}
              </div>
            )}

            <div className="rounded-[10px] border border-ink-100 bg-cloud px-3.5 py-2.5 text-[11px] leading-[1.6] text-ink-500">
              <b className="text-ink-700">{t('obs.savedExplainHead')}</b> {t('obs.savedExplainBody')}
              {data.costEstimated && t('obs.pricesListEstimate')}
            </div>

            {/* Per-agent breakdown. */}
            {props.agentId === 'all' && (data.perAgent?.length ?? 0) > 0 && (
              <div className="overflow-hidden rounded-[10px] border border-ink-100">
                <table className="w-full text-[11.5px]">
                  <thead className="bg-cloud text-[9.5px] font-bold uppercase tracking-[0.1em] text-ink-400">
                    <tr>
                      <th className="px-3 py-2 text-left">{t('obs.colAgent')}</th>
                      <th className="px-3 py-2 text-right">{t('obs.colTriages')}</th>
                      <th className="px-3 py-2 text-right">{t('obs.colSkipPct')}</th>
                      <th className="px-3 py-2 text-right">{t('obs.colTriageDollar')}</th>
                      <th className="px-3 py-2 text-right">{t('obs.colAvgTurn')}</th>
                      <th className="px-3 py-2 text-right">{t('obs.colCacheHit')}</th>
                      <th className="px-3 py-2 text-right">{t('obs.colEstNet')}</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-ink-100">
                    {(data.perAgent ?? []).map((a) => (
                      <tr key={a.agentId}>
                        <td className="px-3 py-1.5 text-ink-800">{a.agentName}</td>
                        <td className="px-3 py-1.5 text-right tabular-nums text-ink-600">{a.triageCount}</td>
                        <td className="px-3 py-1.5 text-right tabular-nums text-ink-600">{a.triageCount > 0 ? fmtPct(a.skipCount / a.triageCount) : '—'}</td>
                        <td className="px-3 py-1.5 text-right tabular-nums text-ink-600">{fmtUsd(a.triageCostUsd)}</td>
                        <td className="px-3 py-1.5 text-right tabular-nums text-ink-600">{fmtUsd(a.avgTurnCostUsd)}</td>
                        <td className="px-3 py-1.5 text-right tabular-nums text-ink-600">{fmtPct(a.turnCacheHitRate)}</td>
                        <td className="px-3 py-1.5 text-right tabular-nums font-semibold" style={a.estimatedNetSavingsUsd >= 0 ? { color: 'var(--avail)' } : undefined}>
                          <span className={a.estimatedNetSavingsUsd < 0 ? 'text-coral-deep' : undefined}>{a.estimatedNetSavingsUsd >= 0 ? '+' : ''}{fmtUsd(a.estimatedNetSavingsUsd)}</span>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}

            {/* Recent per-triage ledger. */}
            <div>
              <div className="mb-2 text-[10px] font-bold uppercase tracking-[0.12em] text-ink-400">{t('obs.recentTriages')}</div>
              <div className="space-y-1">
                                {(data.recent ?? []).map((row) => (
                  <div key={row.id} className="flex items-center gap-3 rounded-[8px] border border-ink-100 bg-paper px-3 py-1.5 text-[11.5px]">
                    <span className="w-[58px] shrink-0 tabular-nums text-ink-400">{clock(row.createdAt)}</span>
                    <span className="w-[90px] shrink-0 truncate text-ink-700">{row.agentName}</span>
                    <span className={cn(
                      'w-[52px] shrink-0 rounded-full px-1.5 py-0.5 text-center text-[9.5px] font-bold uppercase',
                      row.actionable ? 'bg-sky2-50 text-skype-deep' : 'bg-ink-100 text-ink-500',
                    )}>{row.actionable ? t('obs.actionWake') : t('obs.actionSkip')}</span>
                    <span className="w-[88px] shrink-0 truncate text-[10px] text-ink-400">{row.source}</span>
                    <span className="w-[80px] shrink-0 text-right tabular-nums text-ink-600">{row.measured ? fmtUsd(row.costUsd) : '—'}</span>
                    <span className="w-[120px] shrink-0 text-right text-[10px] tabular-nums text-ink-400">
                      {row.measured ? t('obs.triageTokenDetail', { uncached: fmtTok(row.inputTokens), cache: fmtTok(row.cachedInputTokens) }) : t('obs.unmeasured')}
                    </span>
                    <span className="flex-1 truncate text-right text-[10px] text-ink-400">
                      {!row.actionable && row.estSavingUsd != null
                        ? <span style={row.estSavingUsd >= 0 ? { color: 'var(--avail)' } : undefined} className={row.estSavingUsd < 0 ? 'text-coral-deep' : undefined}>
                            {(row.estSavingUsd >= 0 ? t('obs.estSaved', { amount: fmtUsd(Math.abs(row.estSavingUsd)) }) : t('obs.estLost', { amount: fmtUsd(Math.abs(row.estSavingUsd)) }))}
                          </span>
                        : <span className="truncate">{row.reason ?? ''}</span>}
                    </span>
                  </div>
                ))}              </div>
            </div>
            </>
            )}
          </div>
        )}
      </div>
    </main>
  )
}
