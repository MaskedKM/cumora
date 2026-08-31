// 会话列表行件(#219 ②)—— 从 ConversationsPane.tsx 原样搬移:
// ConvoRow + 它的展示原子(ConvoAvatar/TeamAvatar/Tag/MutedGlyph)。
import type React from 'react'
import { Avatar } from '@/components/Avatar'
import { HiveAvatar } from '@/components/HiveAvatar'
import { IMail } from '@/components/icons'
import { PreviewText } from '@/components/PreviewText'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useMe } from '@/stores/auth'
import { isMuted } from '@/stores/conversations'
import { useMessages } from '@/stores/messages'
import { useParticipants } from '@/stores/participants'
import type { Conversation, Participant } from '@/types'
import { muteTooltip } from './shared'

function TeamAvatar() {
  return (
    <div className="w-11 h-11 rounded-full relative grid place-items-center text-white font-display font-medium text-base"
      style={{ background: 'linear-gradient(135deg, var(--skype), var(--skype-ink))', boxShadow: 'inset 0 0 0 2px rgba(255,255,255,0.2)' }}>
      <span style={{ letterSpacing: '-0.02em' }}>⌘</span>
      <span className="absolute rounded-full" style={{ width: 12, height: 12, background: 'var(--avail)', boxShadow: '0 0 0 2.5px var(--paper)', bottom: -1, right: -1 }} />
    </div>
  )
}

function ConvoAvatar({ c }: { c: Conversation }) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()

  // Direct chats: show the other participant's portrait.
  if (c.kind === 'direct') {
    const member = c.members.find((m) => m !== meId) ?? c.members[0]
    const p = member ? byId[member] : undefined
    if (p) return <Avatar p={p} size={44} ringColor="var(--paper)" />
    return <TeamAvatar />
  }

  // Groups + freshly-pulled groups: cluster member portraits in a honeycomb so
  // you can see who's in there at a glance. The current user moves to the
  // front so "you're in this group" reads instantly.
  if (c.kind === 'group') {
    const others: Participant[] = []
    let me: Participant | undefined
    for (const id of c.members) {
      const p = byId[id]
      if (!p) continue
      if (p.id === meId) me = p
      else others.push(p)
    }
    const ordered = me ? [me, ...others] : others
    if (ordered.length === 0) return <TeamAvatar />
    return <HiveAvatar ps={ordered} size={44} ringColor="var(--paper)" />
  }

  // Whispers: 2-person hive (the two whisperers).
  if (c.kind === 'whisper') {
    const ps = c.members.map((m) => byId[m]).filter((p): p is Participant => Boolean(p))
    if (ps.length === 0) return <TeamAvatar />
    return <HiveAvatar ps={ps} size={44} ringColor="var(--paper)" />
  }

  // Email: members hive with a small envelope badge in the corner so
  // an email thread is recognizable at a glance even before scanning
  // the title / preview.
  if (c.kind === 'email') {
    const others: Participant[] = []
    let me: Participant | undefined
    for (const id of c.members) {
      const p = byId[id]
      if (!p) continue
      if (p.id === meId) me = p
      else others.push(p)
    }
    const ordered = (me ? [me, ...others] : others).slice(0, 4)
    const inner = ordered.length > 0
      ? <HiveAvatar ps={ordered} size={44} ringColor="var(--paper)" />
      : <TeamAvatar />
    return (
      <div className="relative">
        {inner}
        <span
          className="absolute -bottom-1 -right-1 w-[18px] h-[18px] rounded-full grid place-items-center"
          style={{
            background: 'linear-gradient(135deg, #FBF8F0, #E8DEC0)',
            border: '1.5px solid var(--paper)',
            color: '#7A6A3F',
          }}
          title={t('convo.emailThread')}
        >
          <IMail className="w-2.5 h-2.5" strokeWidth={2.5} />
        </span>
      </div>
    )
  }

  return <TeamAvatar />
}

function Tag({ c }: { c: Conversation }) {
  const t = useT()
  // Note: c.subtitle (auto-generated as "cross-project · N" / "team · N")
  // intentionally NOT rendered — it duplicates the member count already
  // visible in the hive avatar and crowded the row layout. Only meaningful
  // user-facing tags surface here.
  if (c.tag === 'human') return (
    // No chip background — matches the agent role label treatment in
    // MessageRow (small uppercase tracking, no fill). Skype-deep tints
    // the label so the brand palette carries the "this is a human"
    // signal, instead of a coral accent that fights the rest of the row.
    <span
      className="text-[10px] font-semibold tracking-wider uppercase whitespace-nowrap"
      style={{ color: 'var(--skype-deep)' }}
    >{t('convo.teammateTag')}</span>
  )
  return null
}

interface RowProps {
  c: Conversation
  selected: boolean
  onClick: () => void
  onContextMenu?: (e: React.MouseEvent) => void
}

function MutedGlyph({ title }: { title: string }) {
  // Slack-style bell-off — small, ink-300, matches the row's secondary tone.
  return (
    <span
      className="inline-flex items-center justify-center w-3 h-3 shrink-0 text-ink-300"
      title={title}
      aria-label={title}
    >
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="w-3 h-3">
        <path d="M13.73 21a2 2 0 0 1-3.46 0" />
        <path d="M18.63 13A17.9 17.9 0 0 1 18 8" />
        <path d="M6.26 6.26A5.86 5.86 0 0 0 6 8c0 7-3 9-3 9h14" />
        <path d="M18 8a6 6 0 0 0-9.33-5" />
        <line x1="1" y1="1" x2="23" y2="23" />
      </svg>
    </span>
  )
}

export function ConvoRow({ c, selected, onClick, onContextMenu }: RowProps) {
  const t = useT()
  const isFreshPulled = c.tag === 'fresh-pulled'
  const muted = isMuted(c)
  const typingIds = useMessages((s) => s.typing[c.id])
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  const pulledByName = c.pulledBy?.agentId
    ? byId[c.pulledBy.agentId]?.name ?? c.pulledBy.agentId
    : null
  const typingNames = (typingIds ?? [])
    .filter((id) => id !== meId)
    .map((id) => byId[id]?.name)
    .filter((n): n is string => Boolean(n))
  return (
    <div
      onClick={onClick}
      onContextMenu={onContextMenu}
      className={cn(
        'grid grid-cols-[44px_1fr_auto] gap-[11px] py-2.5 px-2 rounded-[11px] cursor-pointer items-center transition relative',
        !selected && 'hover:bg-sky2-50',
        selected && !isFreshPulled && 'bg-gradient-to-b from-sky2-100 to-sky2-50 ring-1 ring-inset ring-sky2-200',
        isFreshPulled && selected && 'bg-gradient-to-b from-[rgba(244,183,64,0.13)] to-[rgba(244,183,64,0.04)] ring-1 ring-inset ring-[rgba(244,183,64,0.45)] shadow-[0_0_24px_-4px_rgba(244,183,64,0.3)]',
      )}
    >
      {isFreshPulled && selected && (
        <span className="absolute -top-2 right-3 bg-cloud border border-[rgba(244,183,64,0.5)] text-gold-deep text-[9px] font-extrabold tracking-wider uppercase py-0.5 px-2 rounded-full shadow">
          {pulledByName ? t('convo.newPulledBy', { name: pulledByName }) : t('convo.new')}
        </span>
      )}
      <ConvoAvatar c={c} />
      <div className="min-w-0">
        <div className="flex items-center gap-1.5 mb-0.5">
          {/* Muted titles get a soft de-emphasis (ink-700 not ink-900,
              slightly translucent) so the row reads "I'm silenced"
              without screaming it. The bell-off glyph carries the
              affirmative signal. */}
          <span className={cn(
            'text-[13.5px] font-semibold truncate',
            muted ? 'text-ink-700 opacity-80' : 'text-ink-900',
          )}>{c.title}</span>
          {muted && <MutedGlyph title={muteTooltip(t, c.mutedUntil)} />}
          <Tag c={c} />
        </div>
        {typingNames.length > 0 ? (
          <div className="text-[12px] text-skype-deep leading-[1.4] truncate flex items-center gap-1.5">
            <span className="inline-flex gap-[2px] shrink-0">
              <span className="w-[3px] h-[3px] rounded-full bg-skype animate-bounce-dot" />
              <span className="w-[3px] h-[3px] rounded-full bg-skype animate-bounce-dot" style={{ animationDelay: '0.15s' }} />
              <span className="w-[3px] h-[3px] rounded-full bg-skype animate-bounce-dot" style={{ animationDelay: '0.3s' }} />
            </span>
            <span className="truncate">
              {typingNames.length === 1
                ? t('convo.typingOne', { name: typingNames[0] })
                : typingNames.length === 2
                  ? t('convo.typingTwo', { a: typingNames[0], b: typingNames[1] })
                  : t('convo.typingMore', { a: typingNames[0], n: typingNames.length - 1 })}
            </span>
          </div>
        ) : c.preview && (
          <div className="text-[12px] text-ink-500 leading-[1.4] truncate">
            <PreviewText body={c.preview} />
          </div>
        )}
      </div>
      <div className="text-right pt-0.5">
        <div className="text-[10.5px] text-ink-300 tabular-nums mb-1">{c.lastAt}</div>
        {c.unread !== undefined && c.unread > 0 && (
          <span
            className="inline-grid place-items-center min-w-[18px] h-[18px] px-1.5 rounded-full text-[10px] font-bold"
            style={{
              // Muted convos keep the count visible (per the spec: "still
              // show the per-row unread count") but switch to an ink chip
              // instead of coral — preserves the "silent" affordance.
              background: muted ? 'var(--ink-200)' : (isFreshPulled ? 'var(--gold)' : 'var(--coral)'),
              color: muted ? 'var(--ink-700)' : (isFreshPulled ? 'var(--ink-900)' : 'white'),
            }}
          >{c.unread}</span>
        )}
      </div>
    </div>
  )
}
