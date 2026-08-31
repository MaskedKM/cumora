import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'

export function RefreshButton({ loading, onClick }: { loading: boolean; onClick: () => void }) {
  const t = useT()
  return (
    <button
      onClick={onClick}
      className={cn(
        'inline-flex h-[46px] min-w-[96px] items-center justify-center gap-2 rounded-[13px] border border-ink-100 bg-cloud px-4 text-[12.5px] font-semibold text-skype-deep transition',
        'shadow-[0_1px_0_rgba(255,255,255,0.95)_inset,0_12px_28px_-24px_rgba(26,78,120,0.58)]',
        'hover:border-sky2-200 hover:bg-sky2-50/70 active:scale-[0.985]',
        'focus:outline-none focus:ring-4 focus:ring-sky2-100',
        loading && 'text-skype-deep/75',
      )}
      aria-busy={loading}
    >
      <span
        aria-hidden="true"
        className={cn(
          'grid h-4 w-4 place-items-center rounded-full border transition',
          loading
            ? 'animate-spin border-sky2-200 border-t-skype'
            : 'border-sky2-200 bg-sky2-50 shadow-[inset_0_0_0_4px_rgba(255,255,255,0.72)]',
        )}
      />
      <span>{t('obs.refresh')}</span>
    </button>
  )
}
