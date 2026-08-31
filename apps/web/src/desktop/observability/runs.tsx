import type { ApiAgentEvent, ApiAgentRun, ApiAgentRunStatus } from '@/api/client'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { EventDetails } from './eventDetails'
import { clock, elapsed, relative, STATUS_STYLE } from './shared'

const LEVEL_STYLE: Record<ApiAgentEvent['level'], string> = {
  debug: 'border-ink-100 bg-paper text-ink-500',
  info: 'border-sky2-100 bg-sky2-50 text-skype-deep',
  warn: 'border-gold bg-cloud text-gold-deep',
  error: 'border-coral-soft bg-coral-soft text-coral-deep',
}

export function StatusPill({ status }: { status: ApiAgentRunStatus }) {
  const t = useT()
  const tone = STATUS_STYLE[status]
  return (
    <span className={cn('inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[10.5px] font-bold', tone.cls)}>
      <span className="h-1.5 w-1.5 rounded-full" style={{ background: tone.dot }} />
      {t(tone.key)}
    </span>
  )
}

export function AgentDot({ run }: { run: ApiAgentRun }) {
  return (
    <div
      className="grid h-9 w-9 shrink-0 place-items-center rounded-full text-[13px] font-semibold text-white"
      style={{
        background: run.agentAvatarUrl
          ? `center / cover url(${run.agentAvatarUrl})`
          : 'linear-gradient(135deg, var(--skype), var(--whisper))',
      }}
    >
      {!run.agentAvatarUrl && run.agentName.charAt(0).toUpperCase()}
    </div>
  )
}

export function RunRow({ run, active, onClick }: { run: ApiAgentRun; active: boolean; onClick: () => void }) {
  const t = useT()
  return (
    <button
      onClick={onClick}
      className={cn(
        'w-full rounded-[12px] border p-3 text-left transition',
        active
          ? 'border-sky2-200 bg-cloud shadow-soft'
          : 'border-transparent bg-transparent hover:border-ink-100 hover:bg-cloud/70',
      )}
    >
      <div className="flex items-start gap-3">
        <AgentDot run={run} />
        <div className="min-w-0 flex-1">
          <div className="flex items-center justify-between gap-2">
            <div className="truncate text-[13.5px] font-semibold text-ink-900">{run.agentName}</div>
            <StatusPill status={run.status} />
          </div>
          <div className="mt-1 flex items-center gap-2 text-[11px] text-ink-500">
            <span>{clock(run.startedAt)}</span>
            <span className="h-1 w-1 rounded-full bg-ink-200" />
            <span>{elapsed(run.durationMs)}</span>
            {run.stage && (
              <>
                <span className="h-1 w-1 rounded-full bg-ink-200" />
                <span className="truncate font-mono">{run.stage}</span>
              </>
            )}
          </div>
          <div className="mt-2 line-clamp-2 text-[12px] leading-[1.45] text-ink-600">
            {run.error || run.summary || t('obs.unreadInputs', { n: run.inboxCount })}
          </div>
          <div className="mt-2 flex items-center gap-1.5 text-[10.5px] font-semibold text-ink-400">
            <span>{run.inboxCount} {t('obs.inbox')}</span>
            <span>/</span>
            <span>{run.toolCallCount} {t('obs.tools')}</span>
            <span>/</span>
            <span>{run.tokenCount} {t('obs.tokens')}</span>
          </div>
        </div>
      </div>
    </button>
  )
}

export function EventRow({ event }: { event: ApiAgentEvent }) {
  const t = useT()
  return (
    <div className="relative grid grid-cols-[98px_18px_minmax(0,1fr)] gap-3">
      <div className="pt-1 text-right font-mono text-[11px] text-ink-400">{clock(event.createdAt)}</div>
      <div className="relative flex justify-center">
        <span className="absolute top-0 h-full w-px bg-ink-100" />
        <span
          className={cn(
            'relative mt-1 h-3 w-3 rounded-full border-2 bg-cloud',
            event.level === 'error' ? 'border-coral-deep' : event.level === 'warn' ? 'border-gold-deep' : 'border-skype',
          )}
        />
      </div>
      <div className="pb-5">
        <div className={cn('rounded-[12px] border bg-cloud p-3', event.level === 'error' && 'border-coral-soft')}>
          <div className="flex flex-wrap items-center gap-2">
            <span className={cn('rounded-full border px-2 py-0.5 font-mono text-[10px] font-bold', LEVEL_STYLE[event.level])}>
              {event.level}
            </span>
            <span className="font-mono text-[11px] text-ink-400">{event.kind}</span>
            <span className="ml-auto text-[11px] text-ink-400">{relative(event.createdAt, t)}</span>
          </div>
          <div className="mt-2 text-[13px] font-semibold text-ink-900">{event.title}</div>
          {Object.keys(event.data ?? {}).length > 0 && <EventDetails event={event} />}
        </div>
      </div>
    </div>
  )
}
