import {
  fmtTokens,
  fmtUsdCompact as fmtUsd,
} from '@/lib/format'
import type { useT } from '@/lib/i18n'
import type { LlmObservabilityPayload } from '../api'
import type { Unit } from './shared'

// ─── Hero spend card ─────────────────────────────────────────────────────

export function HeroSpendCard({ summary, unit, loading, t }: { summary: LlmObservabilityPayload['summary'] | undefined; unit: Unit; loading: boolean; t: ReturnType<typeof useT> }) {
  const totalUsd = summary?.totalCostUsd ?? 0
  // Tokens headline counts every billable token (cached input included); the
  // sub then splits it so the cached share — which bills ~10× less — is never
  // hidden inside the total.
  const uncachedIn = summary?.totalInputTokens ?? 0
  const cachedIn = summary?.totalCachedInputTokens ?? 0
  const out = summary?.totalOutputTokens ?? 0
  const totalTok = uncachedIn + cachedIn + out
  return (
    <div className="obs-hero-card obs-hero-spend">
      <div className="obs-hero-spend-shine" aria-hidden />
      <div className="obs-hero-label">{t('adminobs.heroSpendPrefix', { unit: unit === 'usd' ? t('adminobs.spendLabel') : t('adminobs.tokensLabel'), days: summary?.sinceDays ?? 30 })}</div>
      <div className="obs-hero-value">{loading ? '—' : unit === 'usd' ? fmtUsd(totalUsd, totalUsd < 100 ? 4 : 2) : fmtTokens(totalTok)}</div>
      <div className="obs-hero-sub">
        {summary
          ? (unit === 'usd'
              ? t('adminobs.heroSubUsd', { in: fmtTokens(uncachedIn + cachedIn), out: fmtTokens(out) })
              : t('adminobs.heroSubTokens', { in: fmtTokens(uncachedIn), cached: fmtTokens(cachedIn), out: fmtTokens(out) }))
          : <> </>}
      </div>
    </div>
  )
}

export function HeroStatCard({ label, value, sub, accent, loading }: {
  label: string; value: string; sub: string; accent?: string; loading: boolean
}) {
  return (
    <div className="obs-hero-card">
      <div className="obs-hero-label">{label}</div>
      <div className="obs-hero-value" style={accent ? { color: accent } : undefined}>
        {loading ? '—' : value}
      </div>
      <div className="obs-hero-sub">{sub || ' '}</div>
    </div>
  )
}
