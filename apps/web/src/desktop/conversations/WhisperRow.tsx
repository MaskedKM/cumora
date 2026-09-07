// WhisperRow —— #368 刀1:会话列表「Agent 对话」分区的行(WhispersView 退役
// 后纯 agent 会话从独立视图并入主列表)。呈现沿用 WhispersView 旧行的语义:
// HiveAvatar + 成员名(1对1 用 A ↔ B,群用标题/“A, B & N more”)+ msgCount。
import { HiveAvatar } from '@/components/HiveAvatar'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useParticipants } from '@/stores/participants'
import type { ApiWhisper } from '@/api/client'
import type { Participant } from '@/types'

export function WhisperRow({ w, selected, onClick }: {
  w: ApiWhisper
  selected: boolean
  onClick: () => void
}) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  // Resolve every member to a participant record; skip rows where any member
  // hasn't loaded yet (they reappear on the next refresh) — same contract as
  // the old WhispersView list.
  const ms = w.members.map((id) => byId[id]).filter((p): p is Participant => Boolean(p))
  if (ms.length < 2) return null
  const isGroup = w.kind === 'group' || ms.length > 2
  const namesLabel: string | null = ms.length <= 2
    ? null
    : ms.length === 3
      ? t('whispers.andOneMore', { a: ms[0].name, b: ms[1].name })
      : t('whispers.andNMore', { a: ms[0].name, b: ms[1].name, n: ms.length - 2 })
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'mb-0.5 grid w-full grid-cols-[44px_1fr_auto] items-center gap-[11px] rounded-[12px] px-3 py-2.5 text-left transition',
        !selected && 'hover:bg-whisper-50',
      )}
      style={selected ? {
        background: 'var(--whisper-50)',
        boxShadow: 'inset 0 0 0 1px var(--whisper-100)',
      } : undefined}
    >
      <HiveAvatar ps={ms} size={44} ringColor="var(--paper)" />
      <span className="min-w-0">
        <span className="mb-0.5 flex items-center gap-1.5 truncate text-[13.5px] font-semibold text-ink-900">
          {isGroup ? (
            <span className="truncate">{w.title || namesLabel}</span>
          ) : (
            <>
              <span className="truncate">{ms[0].name}</span>
              <span className="shrink-0 text-[11px] text-whisper">↔</span>
              <span className="truncate">{ms[1].name}</span>
            </>
          )}
        </span>
        <span className="block truncate font-display text-[11.5px] italic leading-[1.4] text-ink-500">
          {(isGroup && namesLabel ? namesLabel : (w.about ?? t('whispers.privateThread')))}
          <span className="not-italic text-ink-300"> · {w.msgCount}</span>
        </span>
      </span>
      <span className="text-[10.5px] tabular-nums text-ink-300">
        {new Date(w.createdAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
      </span>
    </button>
  )
}

