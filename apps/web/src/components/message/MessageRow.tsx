import { memo, useMemo } from 'react'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useMe } from '@/stores/auth'
import { discardFailedMessage, retryFailedMessage } from '@/stores/messages'
import { useParticipants } from '@/stores/participants'
import type { Message, Participant } from '@/types'
import { Avatar } from '../Avatar'
import { CalendarLink } from '../CalendarLink'
import { HumanBadge } from '../HumanBadge'
import { firstUrlInBody, LinkPreview } from '../LinkPreview'
import { PollBubble } from '../PollBubble'
import { AttachmentCard, artifactKey, artifactRefsForMessage, BoardArtifactCard, CalendarArtifactCard, CardArtifactCard, DocumentArtifactCard, ToolCard } from './artifactCards'
import { QUICK_REACTIONS, QuickReactionButton, ReactionPill } from './reactions'
import { RichBody } from './richBody'




export function WhisperLink({ msg }: { msg: Message }) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  if (!msg.whisperLink) return null
  const w = msg.whisperLink
  const a = byId[w.pair[0]]
  const b = byId[w.pair[1]]
  return (
    <div
      className="my-2 ml-[50px] max-w-[min(calc(100%-50px),540px)] py-2.5 px-3.5 rounded-xl border border-dashed text-[12.5px] flex items-center gap-2.5 cursor-pointer relative overflow-hidden"
      style={{
        background: 'linear-gradient(135deg, rgba(123, 108, 176, 0.09), rgba(123, 108, 176, 0.03))',
        borderColor: 'rgba(123, 108, 176, 0.42)',
        color: 'var(--whisper-deep)',
      }}
    >
      <div className="shrink-0 w-[30px] h-[30px] rounded-full grid place-items-center text-whisper" style={{ background: 'rgba(123, 108, 176, 0.18)' }}>
        <svg width="14" height="14" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24"><path d="M3 12c0-4 4-8 9-8s9 4 9 8-4 8-9 8a10 10 0 01-3-.5L3 21l1.5-5A8 8 0 013 12z"/></svg>
      </div>
      <div className="min-w-0 flex-1">
        <span className="text-whisper-deep font-bold">{a?.name}</span>{t('msgview.and')}<span className="text-whisper-deep font-bold">{b?.name}</span>{t('msgview.whisperingSuffix')} <em className="font-display italic font-normal text-whisper-deep">{w.snippet}</em> · {w.count} messages
      </div>
      <div className="ml-auto text-[11px] font-semibold py-1 px-2.5 rounded-full bg-[rgba(124,92,255,0.15)] text-whisper-deep shrink-0">{t('msgview.peekArrow')}</div>
    </div>
  )
}


interface MessageRowProps {
  msg: Message
  /** Resolved author. Optional: system / whisper rows render without one
   *  (e.g. the calendar-fired notice is authored by a synthetic system id
   *  that isn't in the participants store). */
  author?: Participant
  delay?: number
  /** Whether to play the rise-in fade animation on mount. Default
   *  `true` for backward compatibility with the desktop view, but
   *  callers that virtualize rows (e.g. the mobile chat's Virtuoso
   *  list) should pass `false` for historical rows — otherwise every
   *  scroll-back remounts an off-screen row and replays its fade,
   *  which reads as flicker. */
  animate?: boolean
}

/**
 * Centered "X joined" row for kind='system' messages. Body is a JSON payload
 * like {"kind":"joined","participantId":"atlas-9af2"}. The participant
 * name resolves from the live participants store, and clicking the chip
 * opens that person's info pane (for agents) or no-ops (for humans).
 *
 * Exported so the Whisper peek pane can reuse the same row instead of
 * dumping raw JSON into a bubble — only `body` is read, so any object
 * shaped `{ body: string }` works (covers both Message and
 * ApiWhisperMessage without a type adapter).
 */
export function SystemRow({ msg, delay = 0, animate = true }: { msg: { body: string }; delay?: number; animate?: boolean }) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const openAgentInfo = useApp((s) => s.openAgentInfo)
  // Same animate-once contract as MessageRow: don't replay the rise-in fade on
  // a Virtuoso remount (scroll / quote-jump).
  const riseCls = animate ? 'animate-rise' : ''
  const riseStyle = animate ? { animationDelay: `${delay}ms` } : undefined
  let payload: {
    kind?: string
    participantId?: string
    actorId?: string
    noticeKind?: string
    text?: string
    title?: string
    eventId?: string
  } = {}
  try { payload = JSON.parse(msg.body) }
  catch { /* malformed — skip rendering */ return null }

  // Provider-state notices (e.g. AI quota exhausted, model degraded) ship
  // with `kind: 'notice'` + a free-text `text` body. No participant chip;
  // rendered as a centered italic banner with a ⚠ glyph so it reads as
  // "something stopped working" rather than "someone did something". The
  // server inserts these via runtime.postSystemNotice with per-conversation
  // Redis dedup so the same warning doesn't spam the room when multiple
  // agents hit the condition.
  if (payload.kind === 'notice' && typeof payload.text === 'string') {
    return (
      <div className={cn('flex justify-center my-3', riseCls)} style={riseStyle}>
        <div className="max-w-[min(100%,540px)] flex items-start gap-2 px-3 py-1.5 rounded-md bg-coral-soft/60 border border-coral-soft text-coral-deep text-[11.5px] font-display">
          <span className="leading-[1.4] shrink-0">⚠</span>
          <span className="leading-[1.4]">{payload.text}</span>
        </div>
      </div>
    )
  }

  if (payload.kind === 'calendar_event') {
    const title = typeof payload.title === 'string' && payload.title.trim() ? payload.title.trim() : 'Calendar event'
    return (
      <div className={cn('flex justify-center my-3', riseCls)} style={riseStyle}>
        <div className="max-w-[min(100%,540px)] flex items-center gap-2 px-3 py-1.5 rounded-md bg-skype/10 border border-skype/20 text-skype text-[11.5px] font-display">
          <span className="leading-[1.4] shrink-0">📅</span>
          <span className="leading-[1.4]">Calendar fired: {title}</span>
          {typeof payload.eventId === 'string' && <CalendarLink id={payload.eventId} />}
        </div>
      </div>
    )
  }

  const subjectId = payload.participantId
  if (!subjectId) return null
  const subject = byId[subjectId]
  // If the participant record hasn't loaded yet, omit the system row
  // entirely rather than leaking the raw id. The row will re-render once
  // the participants store catches up.
  if (!subject) return null
  // Open InfoPane for whoever was clicked — works for humans too now.
  const onClick = () => openAgentInfo(subject.id)

  // 'kicked' rows additionally name the actor (who did the kicking).
  // Other kinds are subject-only.
  const actor = payload.kind === 'kicked' && payload.actorId ? byId[payload.actorId] : null

  return (
    <div className={cn('flex justify-center my-3', riseCls)} style={riseStyle}>
      <div className="text-[11.5px] text-ink-300 italic font-display flex items-center gap-1.5 flex-wrap justify-center">
        {payload.kind === 'kicked' && actor ? (
          <>
            <SystemActor p={actor} onClick={() => openAgentInfo(actor.id)} />
            <span>{t('msgview.removedActor')}</span>
            <SystemActor p={subject} onClick={onClick} />
            <span>{t('msgview.removedFromGroup')}</span>
          </>
        ) : (
          <>
            <SystemActor p={subject} onClick={onClick} />
            <span>— {payload.kind === 'joined' ? 'joined the group'
              : payload.kind === 'left'     ? 'left the group'
              : payload.kind ?? 'updated the group'}</span>
          </>
        )}
      </div>
    </div>
  )
}

/** Small subject pill — avatar + name, clickable when it's an agent (opens
 *  the info pane). Centralized so SystemRow's variants stay readable. */
function SystemActor({ p, onClick }: { p: Participant; onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      disabled={p.kind !== 'agent'}
      className="inline-flex items-center gap-1.5 not-italic font-semibold text-ink-500 hover:text-skype-deep transition disabled:cursor-default disabled:hover:text-ink-500"
    >
      <Avatar p={p} size={16} ringColor="var(--paper)" showStatus={false} />
      {p.name}
    </button>
  )
}

/** Inline citation card rendered above a reply's body. Click jumps to the
 *  quoted-original via the global hash trick (`#m-${id}`) — Message rows
 *  carry that id on the wrapper div so it works as a CSS scroll target. */
function QuoteCard({ msg }: { msg: Message }) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const jumpToMessage = useApp((s) => s.jumpToMessage)
  if (!msg.quotedMessageId) return null
  const summary = msg.quoted
  // Always go through useApp.jumpToMessage — ChatPane resolves via
  // virtuoso.scrollToIndex, which mounts a row that's been recycled OR was
  // never mounted. The old getElementById path silently no-op'd in those
  // cases — the bug the user hit.
  const targetId = msg.quotedMessageId
  const jump = () => { jumpToMessage(targetId) }
  if (!summary) {
    return (
      <button
        onClick={jump}
        className="mb-1 max-w-[min(100%,580px)] flex items-stretch gap-2 text-left rounded-md bg-cloud/60 border border-ink-100 hover:border-ink-200 px-2 py-1.5 transition-colors"
      >
        <span className="w-[3px] rounded bg-ink-200" />
        <span className="min-w-0 text-[11.5px] text-ink-400 italic">{t('msgview.messageDeleted')}</span>
      </button>
    )
  }
  const authorName = summary.authorName ?? byId[summary.authorId]?.name ?? summary.authorId
  const bodyPreview = summary.kind === 'tool'
    ? '[tool call]'
    : summary.body.slice(0, 140).replace(/\n/g, ' ')
  return (
    <button
      onClick={jump}
      className="mb-1 max-w-[min(100%,580px)] flex items-stretch gap-2 text-left rounded-md bg-cloud/60 border border-ink-100 hover:border-ink-200 hover:bg-cloud px-2 py-1.5 transition-colors"
      title={t('msgview.jumpToOriginal')}
    >
      <span className="w-[3px] rounded bg-skype" />
      <span className="min-w-0 flex flex-col gap-0.5">
        <span className="text-[11.5px] font-semibold text-skype-deep truncate">{authorName}</span>
        <span className="text-[12px] text-ink-500 truncate">{bodyPreview}</span>
      </span>
    </button>
  )
}

/** Tiny reply icon — appears on hover, sets app.replyingTo so the composer
 *  picks up the quote draft. Composers are responsible for wiring this into
 *  the actual sendUserMessage call. */
function ReplyIconButton({ msg }: { msg: Message }) {
  const t = useT()
  const setReplyingTo = useApp((s) => s.setReplyingTo)
  return (
    <button
      onClick={() => setReplyingTo(msg.conversationId, msg.id)}
      className="w-6 h-6 rounded-full hover:bg-sky2-50 grid place-items-center text-ink-400 hover:text-skype-deep"
      title={t('chat.reply')}
      aria-label={t('chat.replyToMessage')}
    >
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
        <polyline points="9 17 4 12 9 7" />
        <path d="M20 18v-2a4 4 0 0 0-4-4H4" />
      </svg>
    </button>
  )
}


function MessageRowImpl({ msg, author, delay = 0, animate = true }: MessageRowProps) {
  const t = useT()
  const openAgentInfo = useApp((s) => s.openAgentInfo)
  const openThreadView = useApp((s) => s.openThreadView)
  const meId = useMe()
  // System / whisper rows don't need a resolved author — handle them before
  // touching `author` so a synthetic-author system message (the calendar-fired
  // notice, authored by CALENDAR_SYSTEM_AUTHOR_ID) renders instead of being
  // dropped by the missing-author guard.
  if (msg.kind === 'whisper-link') return <WhisperLink msg={msg} />
  if (msg.kind === 'system') return <SystemRow msg={msg} delay={delay} animate={animate} />
  if (!author) return null
  const isHuman = author.kind === 'human'
  const isMine = msg.authorId === meId

  const isToolOnly = msg.kind === 'tool' && !msg.body
  const isAttachOnly = Boolean(msg.attachment) && !msg.body
  const _isEmail = msg.kind === 'email'
  const isPoll = msg.kind === 'poll'
  const artifactRefs = useMemo(
    () => artifactRefsForMessage(msg),
    [msg.body, msg.tool?.arg, msg.tool?.detail],
  )
  // Avatar click opens InfoPane for both kinds — humans now have profile
  // cards (their auth email is the most useful new piece). The "yourself"
  // case is gated below via the disabled prop.
  const onAvatarClick = () => {
    if (!isMine) openAgentInfo(author.id)
  }

  return (
    <div
      id={`m-${msg.id}`}
      className={cn(
        'group grid grid-cols-[38px_1fr] gap-3 items-start scroll-mt-20',
        animate && 'animate-rise',
      )}
      style={animate ? { animationDelay: `${delay}ms` } : undefined}
    >
      <button
        onClick={onAvatarClick}
        disabled={isMine}
        className={cn('rounded-full transition', !isMine && 'hover:opacity-80 active:scale-95 cursor-pointer')}
        title={isMine ? undefined : t('chat.showAuthorInfo', { name: author.name })}
      >
        <Avatar p={author} size={38} ringColor="var(--cloud)" />
      </button>
      <div className="min-w-0">
        <div className="flex items-baseline gap-2 mb-1">
          <span className="font-bold text-[13.5px] text-ink-900">{author.name}</span>
          {author.role && !isHuman && (
            <span className="text-[10.5px] text-ink-300 font-semibold tracking-wider uppercase">{author.role}</span>
          )}
          {isHuman && !isMine && <HumanBadge />}
          <span className="ml-auto text-[10.5px] text-ink-300 tabular-nums">{msg.at}</span>
        </div>

        <QuoteCard msg={msg} />

        {!isToolOnly && !isAttachOnly && !isPoll && (
          <div
            className={cn(
              'inline-block py-2.5 px-3.5 rounded-tl-[4px] rounded-tr-[14px] rounded-br-[14px] rounded-bl-[14px] text-[14px] leading-[1.55] max-w-[min(100%,580px)] break-words',
              isMine
                ? 'border'
                : 'bg-sky2-50 border border-sky2-100 text-ink-700'
            )}
            style={isMine ? {
              background: 'linear-gradient(135deg, #FFE8E1, #FFD9D2)',
              borderColor: 'rgba(255, 122, 107, 0.25)',
              color: '#5A2B22',
            } : undefined}
          >
            <RichBody body={msg.body} conversationId={msg.conversationId} />
          </div>
        )}

        {/* Open-Graph card for the first URL in a chat-style body. Skipped
            for tool / attachment / poll / email kinds — those have their
            own card UIs and a link preview underneath would be visual
            noise. The component itself returns null when there's nothing
            useful to render, so this gate is just to avoid spurious
            network calls for non-text messages. */}
        {!isToolOnly && !isAttachOnly && !isPoll && msg.kind !== 'email' && (() => {
          const linkUrl = firstUrlInBody(msg.body)
          return linkUrl ? <LinkPreview url={linkUrl} /> : null
        })()}

        {isPoll && <PollBubble msg={msg} />}

        {msg.kind === 'tool' && <ToolCard msg={msg} />}
        {artifactRefs.length > 0 && (
          <div className="flex flex-col">
            {artifactRefs.map((ref) => (
              ref.type === 'document'
                ? <DocumentArtifactCard key={artifactKey(ref)} id={ref.id} conversationId={msg.conversationId} />
                : ref.type === 'board'
                  ? <BoardArtifactCard key={artifactKey(ref)} id={ref.id} />
                  : ref.type === 'card'
                    ? <CardArtifactCard key={artifactKey(ref)} id={ref.id} />
                    : <CalendarArtifactCard key={artifactKey(ref)} id={ref.id} />
            ))}
          </div>
        )}
        {msg.attachment && <AttachmentCard msg={msg} />}

        {(msg.failed || msg.unconfirmed) && (
          <div className="mt-1 flex items-center gap-2 text-[11px] text-coral-deep">
            <span>{msg.unconfirmed ? t('chat.deliveryUnconfirmed') : t('chat.failedToSend')}</span>
            <button
              type="button"
              onClick={(e) => { e.stopPropagation(); void retryFailedMessage(msg.conversationId, msg.id) }}
              className="font-semibold underline underline-offset-2 hover:text-coral-700"
            >{t('chat.tryAgain')}</button>
            <span className="text-ink-300">·</span>
            <button
              type="button"
              onClick={(e) => { e.stopPropagation(); discardFailedMessage(msg.conversationId, msg.id) }}
              className="font-semibold underline underline-offset-2 hover:text-coral-700"
            >{t('chat.dismiss')}</button>
          </div>
        )}

        {(msg.replyCount ?? 0) > 0 && (
          <button
            onClick={() => openThreadView(msg.conversationId, msg.id)}
            className="mt-1 text-[11.5px] text-skype-deep hover:underline flex items-center gap-1"
          >
            <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
              <polyline points="9 17 4 12 9 7" />
              <path d="M20 18v-2a4 4 0 0 0-4-4H4" />
            </svg>
            {msg.replyCount} {msg.replyCount === 1 ? 'reply' : 'replies'}
          </button>
        )}

        <div className="mt-2 flex flex-wrap gap-1 items-center">
          {/* Dedup by emoji at the render boundary — last writer wins.
              The store's mergeReactionOrder already keeps the array
              unique, but a defensive Map here guarantees the pill row
              never doubles up even if a future code path slips a
              duplicate through. */}
          {Array.from(
            new Map((msg.reactions ?? []).map((r) => [r.emoji, r])).values(),
          ).map((r) => <ReactionPill key={r.emoji} msgId={msg.id} r={r} />)}
          {/* Quick-reaction popup + reply button, visible on hover. */}
          <div className="reaction-quick-tray opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 transition-opacity flex gap-0.5">
            {QUICK_REACTIONS.filter((e) => !(msg.reactions ?? []).some((r) => r.emoji === e && r.mine)).slice(0, 5).map((e) => (
              <QuickReactionButton key={e} msgId={msg.id} emoji={e} />
            ))}
            <ReplyIconButton msg={msg} />
          </div>
        </div>
      </div>
    </div>
  )
}

/** Memoized wrapper around MessageRowImpl. The big win for long threads is
 *  that WS streaming events (which fire many times per second during a
 *  chat-completion stream) only mutate ONE row's reference in the store —
 *  with default shallow equality, every OTHER row's memo holds and skips
 *  the re-render. Author updates still bust the memo because `author` is
 *  resolved upstream from a Map that is replaced on participant churn;
 *  that's fine — participant churn is rare. */
export const MessageRow = memo(MessageRowImpl)

/**
 * The typing indicator. Always reserves its own row height so the thread
 * doesn't shift up/down when typers come and go — only the inner content
 * fades in/out. Supports any number of typers ("Iris is typing…",
 * "Iris and Bram are typing…", "Iris, Bram and 2 more are typing…").
 */
export function TypingRow({ names }: { names: string[] }) {
  const t = useT()
  const visible = names.length > 0
  let body: React.ReactNode = null
  if (names.length === 1) {
    body = <><b className="text-ink-700 font-semibold">{names[0]}</b>{t('msgview.typingIs')}</>
  } else if (names.length === 2) {
    body = <><b className="text-ink-700 font-semibold">{names[0]}</b>{t('msgview.and')}<b className="text-ink-700 font-semibold">{names[1]}</b>{t('msgview.typingAre')}</>
  } else if (names.length >= 3) {
    body = (
      <>
        <b className="text-ink-700 font-semibold">{names[0]}</b>{t('msgview.typingSep')}<b className="text-ink-700 font-semibold">{names[1]}</b>
        {names.length === 3
          ? <>{t('msgview.and')}<b className="text-ink-700 font-semibold">{names[2]}</b>{t('msgview.typingAre')}</>
          : <>{t('msgview.and')}<b className="text-ink-700 font-semibold">{t('msgview.nMore', { n: names.length - 2 })}</b>{t('msgview.typingAre')}</>}
      </>
    )
  }

  return (
    <div
      // Fixed height + opacity transition = no layout shift when typers
      // appear/disappear, and the dots/name fade smoothly. No internal
      // left-padding — the parent decides positioning, since this row is
      // used both inside the message stream and right above the composer.
      aria-live="polite"
      className="flex items-center gap-2 text-ink-500 text-[12px] transition-opacity duration-200"
      style={{ height: 18, opacity: visible ? 1 : 0, pointerEvents: visible ? 'auto' : 'none' }}
    >
      <span className="inline-flex gap-[3px]">
        <span className="w-[5px] h-[5px] rounded-full bg-skype animate-bounce-dot" />
        <span className="w-[5px] h-[5px] rounded-full bg-skype animate-bounce-dot" style={{ animationDelay: '0.15s' }} />
        <span className="w-[5px] h-[5px] rounded-full bg-skype animate-bounce-dot" style={{ animationDelay: '0.3s' }} />
      </span>
      <span>{body}</span>
    </div>
  )
}

