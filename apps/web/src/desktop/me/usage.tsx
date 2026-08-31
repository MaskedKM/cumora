// 我的视图桶(#219 ④)usage 件 —— 从 MeView.tsx 原样搬移:UsageTab(配额三卡
// +加载/错误/未配置/无订阅四态)+ QuotaCard 与其纯函数(fmtUsd/resetsHint/
// PERIOD_META/PeriodKey)。
import { useEffect, useState } from 'react'
import { type ApiQuotaSnapshot, type ApiQuotaWindow, api } from '@/api/client'
import { type MessageKey, useT } from '@/lib/i18n'
import { Section } from './shared'

/* ============================ Usage / Quota ============================
 * Three rounded "weather cards" — daily / weekly / monthly — mirroring
 * the cumora cloud-and-paper feel. The bars use --skype on a faint
 * sky2-100 track until usage crosses 75% (turns coral) and 95% (deep
 * coral with a quiet pulse). Numbers come from sub2api's subscription
 * summary; everything is best-effort — missing data renders a soft
 * "unavailable" card rather than a hard error. */

type PeriodKey = 'daily' | 'weekly' | 'monthly'

const PERIOD_META: Array<{ key: PeriodKey; label: MessageKey; sub: MessageKey }> = [
  { key: 'daily',   label: 'me.periodDaily',   sub: 'me.rolloverDaily' },
  { key: 'weekly',  label: 'me.periodWeekly',  sub: 'me.rolloverWeekly' },
  { key: 'monthly', label: 'me.periodMonthly', sub: 'me.rolloverMonthly' },
]

function fmtUsd(n: number): string {
  if (!Number.isFinite(n)) return '—'
  if (n === 0) return '$0.00'
  if (n < 0.01) return '<$0.01'
  if (n < 10) return `$${n.toFixed(2)}`
  if (n < 1000) return `$${n.toFixed(2)}`
  return `$${Math.round(n).toLocaleString()}`
}

/** Best-effort "resets in 3h" / "resets in 2d" string. Falls back to a
 *  blank string when sub2api didn't hand back a window start (older
 *  rows). The period length is fixed (24h / 7d / ~30d) — sub2api uses
 *  rolling windows, so we count forward from window_start. */
function resetsHint(t: (k: MessageKey, params?: Record<string, string | number>) => string, period: PeriodKey, windowStart: string | null): string {
  if (!windowStart) return ''
  const start = new Date(windowStart).getTime()
  if (Number.isNaN(start)) return ''
  const lenMs = period === 'daily' ? 86_400_000
              : period === 'weekly' ? 7 * 86_400_000
              : 30 * 86_400_000
  const remaining = start + lenMs - Date.now()
  if (remaining <= 0) return t('me.resetsSoon')
  const h = Math.floor(remaining / 3_600_000)
  if (h < 1) {
    const m = Math.max(1, Math.floor(remaining / 60_000))
    return t('me.resetsInMinutes', { n: m })
  }
  if (h < 48) return t('me.resetsInHours', { n: h })
  const d = Math.floor(h / 24)
  return t('me.resetsInDays', { n: d })
}

function QuotaCard({ period, label, sub, window }: {
  period: PeriodKey
  label: MessageKey
  sub: MessageKey
  window: ApiQuotaWindow | null
}) {
  const t = useT()
  const used = window?.usedUsd ?? 0
  const limit = window?.limitUsd ?? null
  const pct = limit != null && limit > 0 ? Math.min(100, (used / limit) * 100) : 0
  // Tone shifts as the user gets close to the cap. Default is the brand
  // skype blue; coral takes over past the 75% mark so a glance at the
  // cards still tells the user "you're fine" vs "slow down".
  const tone = limit == null ? 'neutral'
             : pct >= 95 ? 'danger'
             : pct >= 75 ? 'warn'
             : 'ok'
  const barColor = tone === 'danger' ? 'var(--coral-deep, #C84E3F)'
                 : tone === 'warn'   ? 'var(--coral, #FF7A6B)'
                 : tone === 'ok'     ? 'var(--skype, #00A8F0)'
                 : 'var(--ink-300, #94A8BC)'
  const resets = window ? resetsHint(t, period, window.windowStart) : ''
  return (
    <div className="bg-cloud rounded-[14px] p-5 flex flex-col gap-3"
      style={{ border: '1px solid var(--ink-100)' }}>
      <div className="flex items-baseline justify-between gap-3">
        <div className="font-display font-semibold text-[14px] text-ink-900">{t(label)}</div>
        {limit != null
          ? <div className="font-mono text-[11px] font-semibold text-ink-500">{pct.toFixed(0)}%</div>
          : <div className="font-mono text-[10px] tracking-wider uppercase text-ink-300">{t('me.unlimited')}</div>}
      </div>
      <div className="font-display tabular-nums text-[22px] tracking-tight text-ink-900" style={{ letterSpacing: '-0.02em' }}>
        {fmtUsd(used)}
        <span className="text-ink-300 text-[15px] font-normal"> / {limit != null ? fmtUsd(limit) : '∞'}</span>
      </div>
      <div className="h-2 rounded-full overflow-hidden" style={{ background: 'var(--sky2-100, #E1F3FD)' }}>
        <div
          className="h-full rounded-full transition-[width,background-color,opacity] duration-500"
          style={{
            width: limit != null ? `${Math.max(2, pct)}%` : '100%',
            background: barColor,
            opacity: limit != null ? 1 : 0.35,
          }}
        />
      </div>
      <div className="flex items-center justify-between text-[11px]">
        <span className="font-display italic text-ink-400">{t(sub)}</span>
        {resets && <span className="font-mono text-ink-500">{resets}</span>}
      </div>
    </div>
  )
}

export function UsageTab() {
  const t = useT()
  const [state, setState] = useState<
    | { kind: 'loading' }
    | { kind: 'ready'; configured: boolean; snapshot: ApiQuotaSnapshot | null; error?: string }
    | { kind: 'error'; message: string }
  >({ kind: 'loading' })

  const load = () => {
    setState({ kind: 'loading' })
    api.getQuota()
      .then((r) => setState({ kind: 'ready', configured: r.configured, snapshot: r.snapshot, error: r.error }))
      .catch((e) => setState({ kind: 'error', message: e instanceof Error ? e.message : String(e) }))
  }
  useEffect(load, [])

  if (state.kind === 'loading') {
    return (
      <div className="space-y-6">
        <Section title={t('me.sectionQuota')}>
          <div className="grid grid-cols-3 gap-3">
            {PERIOD_META.map((p) => (
              <div key={p.key} className="bg-cloud rounded-[14px] p-5 h-[140px]"
                style={{ border: '1px solid var(--ink-100)' }}>
                <div className="font-display font-semibold text-[14px] text-ink-300">{t(p.label)}</div>
                <div className="font-display italic text-[12px] text-ink-300 mt-2">{t('common.loading')}</div>
              </div>
            ))}
          </div>
        </Section>
      </div>
    )
  }

  if (state.kind === 'error') {
    return (
      <div className="space-y-6">
        <Section title={t('me.sectionQuota')}>
          <div className="bg-cloud rounded-[14px] p-6 text-center"
            style={{ border: '1px solid var(--ink-100)' }}>
            <div className="font-display text-[14px] text-ink-700 mb-1">{t('me.quotaFetchFailed')}</div>
            <div className="font-display italic text-[12px] text-coral-deep mb-3">{state.message}</div>
            <button onClick={load}
              className="px-4 py-1.5 rounded-[8px] text-[12px] font-semibold text-white"
              style={{ background: 'var(--skype)' }}>
              {t('common.tryAgain')}
            </button>
          </div>
        </Section>
      </div>
    )
  }

  // ready
  const { configured, snapshot, error } = state
  if (!configured) {
    return (
      <div className="space-y-6">
        <Section title={t('me.sectionQuota')}>
          <div className="bg-cloud rounded-[14px] p-6"
            style={{ border: '1px dashed var(--ink-100)' }}>
            <div className="font-display text-[14px] text-ink-700">{t('me.noQuotaGateway')}</div>
            <div className="font-display italic text-[12px] text-ink-500 mt-1 max-w-xl">
              {t('me.noQuotaHint')}
            </div>
          </div>
        </Section>
      </div>
    )
  }

  if (!snapshot) {
    return (
      <div className="space-y-6">
        <Section title={t('me.sectionQuota')}>
          <div className="bg-cloud rounded-[14px] p-6"
            style={{ border: '1px dashed var(--ink-100)' }}>
            <div className="font-display text-[14px] text-ink-700">
              {error ? t('me.quotaUnreachable') : t('me.noActiveSub')}
            </div>
            <div className="font-display italic text-[12px] text-ink-500 mt-1 max-w-xl">
              {error ? t('me.quotaGatewayUnreachHint') : t('me.subNotProvisioned')}
            </div>
            <button onClick={load}
              className="mt-3 px-4 py-1.5 rounded-[8px] text-[12px] font-semibold text-skype-deep bg-cloud hover:bg-sky2-50 transition"
              style={{ border: '1px dashed var(--sky2-300)' }}>
              {t('me.refresh')}
            </button>
          </div>
        </Section>
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <Section title={t('me.sectionQuota')}>
        <div className="text-[13px] text-ink-500 leading-[1.55] mb-4 max-w-2xl font-display italic">
          {t('me.quotaIntro')}
          {snapshot.groupName ? <> {t('me.quotaIntroPlan', { plan: snapshot.groupName })}</> : null}
        </div>
        <div className="grid grid-cols-3 gap-3">
          {PERIOD_META.map((p) => (
            <QuotaCard
              key={p.key}
              period={p.key}
              label={p.label}
              sub={p.sub}
              window={snapshot[p.key]}
            />
          ))}
        </div>
        <div className="mt-4 flex items-center gap-3">
          <button onClick={load}
            className="px-4 py-1.5 rounded-[8px] text-[12px] font-semibold text-skype-deep bg-cloud hover:bg-sky2-50 transition"
            style={{ border: '1px solid var(--ink-100)' }}>
            {t('me.refresh')}
          </button>
          {error && <span className="text-[11.5px] text-coral-deep font-display italic">{t('me.refreshFailed', { msg: error })}</span>}
        </div>
      </Section>
    </div>
  )
}
