import { useMemo } from 'react'
import { Combobox } from '@/components/Combobox'
import { tLabel, useLocale, type useT } from '@/lib/i18n'
import type { LlmTenantRow } from '../api'
import { fmtAmount, totalTokens, type Unit } from './shared'

// ─── Tenant picker ───────────────────────────────────────────────────────
//
// Compact dropdown listing every company with ledger activity in the window,
// ranked by cost so the heaviest spenders are at the top. Selecting one
// scopes the entire page (hero, chart, rollup, top spenders, drill-down) to
// that tenant. "All accounts" clears the filter. The list comes from the
// global tenants slice of the payload — it isn't filtered by the page's own
// companyFilter so the picker can always offer every active account.
//
// Uses the shared searchable <Combobox> (200 tenants is too many to scroll —
// the operator types to filter). Options are cost/token-ranked with the spend
// in the right-aligned `hint`, "All accounts" first.

export function TenantPicker({ tenants, value, unit, onChange, t }: {
  tenants: LlmTenantRow[]
  value: string
  unit: Unit
  onChange: (companyId: string) => void
  t: ReturnType<typeof useT>
}) {
  const locale = useLocale()
  // Stable label per tenant so a partially-resolved companies join doesn't
  // surface bare ids in the picker. Format: "Name · $cost" / "Name · NtokT"
  // depending on the active unit; falls back to the company id when unnamed.
  const options = useMemo(() => {
    const name = (t: LlmTenantRow): string => t.name?.trim() || t.slug?.trim() || `(${t.companyId.slice(0, 8)}…)`
    // Sort by the metric that's actually ON SCREEN, descending. The server
    // ranks tenants by $; in token mode that order looks random (a cheap model
    // burns more tokens than an expensive one), so re-sort by tokens here.
    const sorted = [...tenants].sort((a, b) =>
      unit === 'usd' ? b.costUsd - a.costUsd : totalTokens(b) - totalTokens(a))
    return [
      { value: '', label: t('adminobs.allAccountsCount', { n: tenants.length }) },
      // amount goes in `hint` (right-aligned, never truncated) so the per-tenant
      // spend stays visible even when the workspace name is long — the label
      // truncates, the number doesn't.
      ...sorted.map((t) => ({
        value: t.companyId,
        label: name(t),
        hint: fmtAmount(unit, t.costUsd, totalTokens(t)),
      })),
    ]
    // locale in the deps so the baked-in "All accounts" label re-renders
    // on a language switch (t itself is identity-unstable, locale is not).
  }, [tenants, unit, locale])
  return (
    <Combobox
      value={value}
      onValueChange={onChange}
      options={options}
      ariaLabel={tLabel(t, 'adminobs.tenantFilterAria', 'Filter by tenant')}
      placeholder={t('adminobs.allAccountsLabel')}
      searchPlaceholder={t('adminobs.searchWorkspaces')}
      className="w-[320px]"
    />
  )
}
