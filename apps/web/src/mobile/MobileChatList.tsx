import { useEffect, useRef, useState } from 'react'
import { AnimatePresence, motion } from 'framer-motion'
import { api, type ApiInboxItem } from '@/api/client'
import { useApp } from '@/stores/app'
import { useAuth, useMe } from '@/stores/auth'
import { useConversations, isMuted } from '@/stores/conversations'
import { useInbox } from '@/stores/inbox'
import { useMessages } from '@/stores/messages'
import { useParticipants } from '@/stores/participants'
import { useWhispers } from '@/stores/whispers'
import { Avatar } from '@/components/Avatar'
import { PreviewText } from '@/components/PreviewText'
import { HiveAvatar } from '@/components/HiveAvatar'
import { HumanBadge } from '@/components/HumanBadge'
import { IAgent, IAgents, IBoard, IDoc, IExit, ISearch, IShip } from '@/components/icons'
import { GroupCreator } from '@/components/GroupCreator'
import { cn } from '@/lib/utils'
import { Pressable } from './Pressable'
import { useLongPress } from './useLongPress'
import { MobileContextMenu, type ContextMenuItem } from './MobileContextMenu'
import { SwipeableRow, type SwipeAction } from './SwipeableRow'
import { PullToRefresh } from './PullToRefresh'
import { Virtuoso } from 'react-virtuoso'
import type { Conversation, Participant } from '@/types'
import { translate, useLocaleStore, useT, type MessageKey } from '@/lib/i18n'

// #370 刀3(ADR 0009):「私聊」滤片随 whispers 视图退役 —— 纯 agent 会话
// 由「Agent 对话」列表分区承载(数据源 /peek/agent-chats),不再是一个
// 要切进去的过滤器(服务端 /conversations 从不返回 whisper 行,该滤片
// 本就空转)。
const filters: ReadonlyArray<'All' | 'Agents' | 'Humans'> = ['All', 'Agents', 'Humans']
const filterKey = (f: typeof filters[number]) =>
  f === 'All' ? 'mclist.filterAll'
  : f === 'Agents' ? 'mclist.filterAgents'
  : 'mclist.filterHumans'
type Filter = (typeof filters)[number]

function TeamFallback() {
  return (
    <div className="w-12 h-12 rounded-full grid place-items-center text-white font-display font-medium shrink-0 relative"
      style={{ background: 'linear-gradient(135deg, var(--skype), var(--skype-ink))' }}>
      <span style={{ letterSpacing: '-0.02em' }}>⌘</span>
      <span className="absolute rounded-full" style={{ width: 12, height: 12, background: 'var(--avail)', boxShadow: '0 0 0 2.5px var(--paper)', bottom: -1, right: -1 }} />
    </div>
  )
}

// Module-level throttle so a list full of not-yet-resolved rows fires at
// most one roster refetch per window, not one per row.
let lastRosterBackfillAt = 0
function backfillRosterOnce() {
  const now = Date.now()
  if (now - lastRosterBackfillAt < 8000) return
  lastRosterBackfillAt = now
  void useParticipants.getState().refresh()
}

function ConvoAvatar({ c, size = 48 }: { c: Conversation; size?: number }) {
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  // Self-heal a stale roster. A freshly-created group (especially one created
  // on another client or pulled by an agent) can arrive in the conversation
  // list before its members land in `byId`. With zero members resolved the
  // honeycomb has nothing to draw and we'd fall back to the default ⌘ tile —
  // which is exactly the "new group shows the default avatar instead of the
  // member honeycomb" bug. Detect the all-unresolved case and kick a one-shot
  // (throttled) participants refetch; once the roster fills in, the HiveAvatar
  // honeycomb renders on the next pass.
  const noneResolved = c.members.length > 0 && c.members.every((id) => !byId[id])
  useEffect(() => {
    if (noneResolved) backfillRosterOnce()
  }, [noneResolved])
  if (c.tag === 'fresh-pulled') {
    return (
      <div className="rounded-full grid place-items-center text-white font-display font-medium text-base shrink-0"
        style={{
          width: size, height: size,
          background: 'conic-gradient(from 0deg, #B57BFF 0% 25%, var(--coral) 25% 50%, #6B7BE6 50% 75%, var(--gold) 75% 100%)',
          boxShadow: 'inset 0 0 0 3px var(--cloud)',
        }}>⌘</div>
    )
  }
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
    if (ordered.length === 0) return <TeamFallback />
    return <HiveAvatar ps={ordered} size={size} ringColor="var(--paper)" />
  }
  if (c.kind === 'whisper') {
    const ps = c.members.map((m) => byId[m]).filter((p): p is Participant => Boolean(p))
    if (ps.length === 0) return <TeamFallback />
    return <HiveAvatar ps={ps} size={size} ringColor="var(--paper)" />
  }
  const member = c.members.find((m) => m !== meId) ?? c.members[0]
  const p = member ? byId[member] : undefined
  if (!p) return null
  return <Avatar p={p} size={size} ringColor="var(--paper)" />
}

// useLongPress lives in ./useLongPress.ts — shared with MobileChat's
// message tapback menu.

/** Small bell-off glyph for muted conversation rows. Same shape as
 *  the desktop pane's indicator — Slack / iOS Messages convention. */
function MutedGlyph() {
  const t = useT()
  return (
    <span className="inline-flex items-center justify-center w-3.5 h-3.5 shrink-0 text-ink-300" aria-label={t('mclist.muted')}>
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="w-3.5 h-3.5">
        <path d="M13.73 21a2 2 0 0 1-3.46 0" />
        <path d="M18.63 13A17.9 17.9 0 0 1 18 8" />
        <path d="M6.26 6.26A5.86 5.86 0 0 0 6 8c0 7-3 9-3 9h14" />
        <path d="M18 8a6 6 0 0 0-9.33-5" />
        <line x1="1" y1="1" x2="23" y2="23" />
      </svg>
    </span>
  )
}

function MobileRow({ c, onTap, onLongPress }: {
  c: Conversation
  onTap: () => void
  onLongPress: (coords: { x: number; y: number }) => void
}) {
  const t = useT()
  const isFresh = c.tag === 'fresh-pulled'
  const muted = isMuted(c)
  const typingIds = useMessages((s) => s.typing[c.id])
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  const press = useLongPress(onLongPress, onTap)
  // Same hardening as the chat composer: drop self, and turn a
  // resolved-but-blank name into a graceful label instead of letting it
  // render as a nameless "is typing…". Unknown ids are dropped.
  const typingNames = (typingIds ?? [])
    .filter((id) => id !== meId)
    .map((id) => {
      const p = byId[id]
      if (!p) return null
      const name = p.name?.trim()
      return name || (p.kind === 'agent' ? t('mclist.fallbackAgent') : t('mclist.fallbackSomeone'))
    })
    .filter((n): n is string => Boolean(n))
  return (
    <motion.button
      {...press}
      onClick={(e) => {
        // Touch path uses onTap from useLongPress. This `onClick` is
        // the mouse / external-keyboard fallback (Vite dev, iPad +
        // trackpad). Suppress when a long-press already fired.
        e.preventDefault()
      }}
      // Subtle press-in scale: rows are large so 0.985 reads as a
      // gentle press without making the surrounding rows look
      // shifted. iOS Mail / Messages use ~0.985–0.99 here.
      whileTap={{ scale: 0.985 }}
      transition={{ type: 'spring', stiffness: 600, damping: 30, mass: 0.5 }}
      className={cn(
        'w-full text-left grid grid-cols-[48px_1fr_auto] gap-3 py-3 px-4 active:bg-sky2-50 relative',
        isFresh && 'bg-gradient-to-r from-[rgba(244,183,64,0.08)] to-transparent',
      )}
    >
      <ConvoAvatar c={c} />
      <div className="min-w-0 self-center">
        <div className="flex items-center gap-1.5 mb-0.5">
          <span className={cn(
            'text-[15px] font-semibold truncate',
            // Muted rows get a softer title tone — same affordance the
            // desktop pane uses. The bell-off glyph carries the
            // assertive signal; this dim is the supporting cue.
            muted ? 'text-ink-700 opacity-80' : 'text-ink-900',
          )}>{c.title}</span>
          {muted && <MutedGlyph />}
          {isFresh && (
            <span className="text-[8.5px] font-bold tracking-wider uppercase py-0.5 px-1.5 rounded text-gold-deep bg-[rgba(244,183,64,0.18)] shrink-0">{t('mclist.badgeNew')}</span>
          )}
          {c.tag === 'human' && <HumanBadge />}
        </div>
        {typingNames.length > 0 ? (
          <div className="text-[12.5px] text-skype-deep leading-snug truncate flex items-center gap-1.5">
            <span className="inline-flex gap-[2px] shrink-0">
              <span className="w-[3px] h-[3px] rounded-full bg-skype animate-bounce-dot" />
              <span className="w-[3px] h-[3px] rounded-full bg-skype animate-bounce-dot" style={{ animationDelay: '0.15s' }} />
              <span className="w-[3px] h-[3px] rounded-full bg-skype animate-bounce-dot" style={{ animationDelay: '0.3s' }} />
            </span>
            <span className="truncate">
              {typingNames.length === 1
                ? t('mclist.typingOne', { name: typingNames[0] })
                : typingNames.length === 2
                  ? t('mclist.typingTwo', { a: typingNames[0], b: typingNames[1] })
                  : t('mclist.typingMore', { name: typingNames[0], n: typingNames.length - 1 })}
            </span>
          </div>
        ) : (
          <div className="text-[12.5px] text-ink-500 leading-snug truncate">
            <PreviewText body={c.preview} />
          </div>
        )}
      </div>
      <div className="flex flex-col items-end gap-1 self-center">
        <span className="text-[10.5px] text-ink-300 tabular-nums">{c.lastAt}</span>
        {c.unread !== undefined && c.unread > 0 && (
          <span className="inline-grid place-items-center min-w-[20px] h-5 px-1.5 rounded-full text-[10.5px] font-bold"
            style={{
              // Muted rows: keep the count visible (people still want
              // to know there's unread) but switch to a grey chip —
              // matches the "silent" affordance from the desktop pane.
              background: muted ? 'var(--ink-200)' : (isFresh ? 'var(--gold)' : 'var(--coral)'),
              color: muted ? 'var(--ink-700)' : (isFresh ? 'var(--ink-900)' : 'white'),
            }}>{c.unread}</span>
        )}
      </div>
    </motion.button>
  )
}

/** Pinned quick-access row — Feishu / Lark style. Each pinned chat
 *  surfaces as a compact circular avatar at the top so the user can
 *  jump back to their most-used rooms without scrolling. Tight
 *  vertical rhythm — Apple's UI guidelines call for ~56px touch
 *  targets and we want this row to feel like a strip, not a section. */
function PinnedRow({ pinned, onSelect, onLongPress }: {
  pinned: Conversation[]
  onSelect: (id: string) => void
  onLongPress: (c: Conversation, coords: { x: number; y: number }) => void
}) {
  if (pinned.length === 0) return null
  return (
    <div className="px-2 pb-1.5 border-b border-ink-100">
      {/* py-2 inside the scroll row reserves vertical space for the
          unread badge that sticks ~4px above each avatar. overflow-x
          implicitly clips overflow-y, so without this padding the top
          of the badge gets shaved off. */}
      <div className="flex gap-2 overflow-x-auto scroll-clean px-1 py-2">
        {pinned.map((c) => (
          <PinnedTile key={c.id} c={c} onSelect={onSelect} onLongPress={(coords) => onLongPress(c, coords)} />
        ))}
      </div>
    </div>
  )
}

function PinnedTile({ c, onSelect, onLongPress }: {
  c: Conversation
  onSelect: (id: string) => void
  onLongPress: (coords: { x: number; y: number }) => void
}) {
  const press = useLongPress(onLongPress, () => onSelect(c.id))
  return (
    <motion.button
      {...press}
      onClick={(e) => e.preventDefault()}
      whileTap={{ scale: 0.92 }}
      transition={{ type: 'spring', stiffness: 600, damping: 30, mass: 0.5 }}
      className="flex flex-col items-center gap-0.5 w-[52px] shrink-0"
    >
      <div className="relative w-[38px] h-[38px] grid place-items-center">
        <ConvoAvatar c={c} size={38} />
        {c.unread !== undefined && c.unread > 0 && (
          <span
            className="absolute -top-1 -right-1.5 min-w-[16px] h-[16px] px-1 rounded-full text-[9.5px] font-bold leading-none grid place-items-center tabular-nums"
            style={{ background: 'var(--coral)', color: 'white', border: '1.5px solid var(--paper)' }}
          >{c.unread > 99 ? '99+' : c.unread}</span>
        )}
      </div>
      <div className="w-full text-[9.5px] font-semibold text-ink-700 truncate leading-tight text-center">{c.title}</div>
    </motion.button>
  )
}

/** Build the iOS Mail-style swipe-left actions for a conversation
 *  row. Mirrors a subset of the context menu items — the ones a user
 *  reaches for most often (pin / mute / mark read / leave). */
function convoSwipeActions(c: Conversation): SwipeAction[] {
  const reload = () => useConversations.getState().reload()
  const muted = isMuted(c)
  // The translate hook lives inside React — calling useT() here would
  // break the helper's call site. Snapshot the locale once and translate
  // imperatively, same pattern as SuspendedScreen's class counterpart.
  const locale = useLocaleStore.getState().locale
  const t = (k: MessageKey, vars?: Record<string, string | number>) => translate(locale, k, vars)
  const actions: SwipeAction[] = []
  if (c.unread !== undefined && c.unread > 0) {
    actions.push({
      label: t('mclist.swipeRead'),
      background: 'var(--skype)',
      onClick: async () => {
        try { await api.markRead(c.id); await reload() }
        catch (err) { console.warn('markRead failed', err) }
      },
    })
  }
  actions.push({
    label: muted ? t('mclist.swipeUnmute') : t('mclist.swipeMute'),
    background: 'var(--ink-500)',
    onClick: async () => {
      try { await api.setMute(c.id, !muted); await reload() }
      catch (err) { console.warn('setMute failed', err) }
    },
  })
  actions.push({
    label: c.pinned ? t('mclist.swipeUnpin') : t('mclist.swipePin'),
    background: 'var(--gold)',
    color: 'var(--ink-900)',
    onClick: async () => {
      try { await api.togglePin(c.id, !c.pinned); await reload() }
      catch (err) { console.warn('togglePin failed', err) }
    },
  })
  if (c.kind !== 'whisper') {
    actions.push({
      label: t('mclist.swipeLeave'),
      background: 'var(--coral)',
      onClick: async () => {
        if (!confirm(t('mclist.confirmLeave', { title: c.title }))) return
        try { await api.leaveConversation(c.id); await reload() }
        catch (err) { console.warn('leave failed', err) }
      },
    })
  }
  return actions
}

/** Build the iOS-style context menu items for a conversation
 *  long-press. The actual chrome is rendered by <MobileContextMenu>.
 *
 *  For a direct (1:1) chat, prepends "Create group with {otherName}…" —
 *  the mobile equivalent of right-clicking a person in desktop's
 *  ConversationsPane. `onCreateGroupWith` opens GroupCreator pre-seeded
 *  with that person already picked. */
function convoMenuItems(
  c: Conversation,
  ctx: {
    meId: string | null
    byId: Record<string, Participant>
    onCreateGroupWith: (otherId: string) => void
  },
): ContextMenuItem[] {
  const reload = () => useConversations.getState().reload()
  const muted = isMuted(c)
  // Hook order: snapshot locale imperatively — this helper is called from
  // a parent render so we can't call useT() here. Same pattern as
  // convoSwipeActions right above.
  const locale = useLocaleStore.getState().locale
  const t = (k: MessageKey, vars?: Record<string, string | number>) => translate(locale, k, vars)
  const items: ContextMenuItem[] = []

  if (c.kind === 'direct') {
    const otherId = c.members.find((m) => m !== ctx.meId)
    const other = otherId ? ctx.byId[otherId] : undefined
    if (other) {
      items.push({
        label: t('mclist.menuCreateGroupWith', { name: other.name }),
        onClick: () => ctx.onCreateGroupWith(other.id),
      })
    }
  }

  items.push(
    {
      label: t('mclist.menuOpen'),
      onClick: () => useApp.getState().selectConversation(c.id),
    },
    {
      label: c.pinned ? t('mclist.menuUnpinFromTop') : t('mclist.menuPinToTop'),
      onClick: async () => {
        try {
          await api.togglePin(c.id, !c.pinned)
          await reload()
        } catch (err) { console.warn('togglePin failed', err) }
      },
    },
    {
      label: muted ? t('mclist.menuUnmute') : t('mclist.menuMute'),
      onClick: async () => {
        try {
          await api.setMute(c.id, !muted)
          await reload()
        } catch (err) { console.warn('setMute failed', err) }
      },
    },
  )
  if (c.unread !== undefined && c.unread > 0) {
    items.push({
      label: t('mclist.menuMarkRead'),
      onClick: async () => {
        try {
          await api.markRead(c.id)
          await reload()
        } catch (err) { console.warn('markRead failed', err) }
      },
    })
  }
  if (c.kind !== 'whisper') {
    items.push({
      label: t('mclist.menuLeave'),
      destructive: true,
      onClick: async () => {
        if (!confirm(t('mclist.confirmLeave', { title: c.title }))) return
        try {
          await api.leaveConversation(c.id)
          await reload()
        } catch (err) { console.warn('leave failed', err) }
      },
    })
  }
  return items
}

export function MobileChatList({ onOpenBoards }: { onOpenBoards: () => void }) {
  const t = useT()
  const select = useApp((s) => s.selectConversation)
  const setView = useApp((s) => s.setView)
  const list = useConversations((s) => s.list)
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  const [filter, setFilter] = useState<Filter>('All')
  const [searchOpen, setSearchOpen] = useState(false)
  const [searchQ, setSearchQ] = useState('')
  const [actionFor, setActionFor] = useState<{ c: Conversation; coords: { x: number; y: number } } | null>(null)
  // ── #370 刀3:菜单 sheet + 两分区(对齐桌面 ConversationsPane 语义)──
  const [menuOpen, setMenuOpen] = useState(false)
  const [actionExpanded, setActionExpanded] = useState(false)
  const isOwner = useAuth((s) => s.companies.find((c) => c.id === s.activeCompanyId)?.role === 'owner')
  const whispers = useWhispers((s) => s.list)
  const inboxItems = useInbox((s) => s.items)
  const actionRows = inboxItems
    .filter((it) => it.severity !== 'info')
    .sort((a, b) => Number(a.read) - Number(b.read) || b.createdAt.localeCompare(a.createdAt))
  const openActionItem = (it: ApiInboxItem) => {
    if (!it.read) void useInbox.getState().markRead(it.id)
    if (it.linkKind === 'conversation' && it.linkId) select(it.linkId)
    else if (it.linkKind === 'board') onOpenBoards()
    else if (it.linkKind === 'calendar') setView('library')
    else if (it.linkKind === 'observability') setView('library')
  }
  // GroupCreator is opened from a long-press → "Create group with {name}…"
  // on a direct chat (mirrors the desktop right-click pattern). When
  // non-null, the modal is open; `initialPicked` pre-seeds the other person.
  const [creating, setCreating] = useState<{ initialPicked: string[] } | null>(null)
  // Virtuoso needs an actual scrolling element to measure against; we let
  // PullToRefresh hand us its inner motion.div via onScrollerReady so the
  // virtualizer reads scrollTop from the same node that drives the
  // pull gesture (otherwise the two would fight over who owns the scroll).
  const [scroller, setScroller] = useState<HTMLElement | null>(null)
  const searchRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (searchOpen) requestAnimationFrame(() => searchRef.current?.focus())
  }, [searchOpen])

  // Warm the message cache for the top conversations in the background. The
  // first time you open a convo, loadConversation does a heavy fetch + JSON
  // normalize on the main thread; if that lands while the push-slide is
  // animating (framer-motion's spring runs in JS), the transition stutters /
  // feels rushed — which is exactly why the SECOND open is smooth (cached) and
  // the first isn't. Prefetching during idle time means most taps hit a warm
  // cache → smooth slide + content already in place (also kills the post-slide
  // content flash). loadConversation is idempotent (skips loaded/in-flight).
  useEffect(() => {
    if (list.length === 0) return
    const ids = list.slice(0, 12).map((c) => c.id)
    let cancelled = false
    let i = 0
    const w = window as Window & {
      requestIdleCallback?: (cb: () => void, opts?: { timeout: number }) => number
      cancelIdleCallback?: (h: number) => void
    }
    const schedule = (cb: () => void): number =>
      typeof w.requestIdleCallback === 'function'
        ? w.requestIdleCallback(cb, { timeout: 1500 })
        : window.setTimeout(cb, 200)
    let handle = 0
    const step = () => {
      if (cancelled) return
      const id = ids[i++]
      if (!id) return
      void useMessages.getState().loadConversation(id)
      handle = schedule(step)
    }
    handle = schedule(step)
    return () => {
      cancelled = true
      if (typeof w.cancelIdleCallback === 'function') w.cancelIdleCallback(handle)
    }
  }, [list.length])

  const q = searchQ.trim().toLowerCase()
  const filtered = list.filter((c) => {
    // 服务端 /conversations 从不返回 whisper 行(纯 agent 会话只在
    // /peek/agent-chats);这行是防御性排除,与桌面主列表同款。
    if (c.kind === 'whisper') return false
    if (filter === 'Agents') {
      if (!(c.kind === 'direct' && c.tag !== 'human')) return false
    } else if (filter === 'Humans') {
      if (c.tag !== 'human') return false
    }
    if (!q) return true
    if (c.title.toLowerCase().includes(q)) return true
    if (c.preview && c.preview.toLowerCase().includes(q)) return true
    for (const id of c.members) {
      const p = byId[id]
      if (!p) continue
      if (p.name.toLowerCase().includes(q)) return true
      if (p.id.toLowerCase().includes(q)) return true
    }
    return false
  })

  // Pinned chats already have their own strip at the top, so the main
  // list intentionally ignores `pinned` and sorts purely by recency.
  // The raw `lastAtIso` (real ISO timestamp, NOT the display string)
  // is the sort key — comparing the formatted `lastAt` would be
  // lexicographic and wrong ("22:13" < "5/20" alphabetically).
  const pinned = filtered.filter((c) => c.pinned)
  const byRecency = [...filtered].sort((a, b) => {
    const at = a.lastAtIso ?? ''
    const bt = b.lastAtIso ?? ''
    if (at === bt) return 0
    return at < bt ? 1 : -1
  })

  return (
    <section className="relative flex flex-col h-full bg-paper">
      <div
        className="sticky top-0 z-10 bg-paper/95 backdrop-blur-md"
        style={{ paddingTop: 'max(env(safe-area-inset-top), 12px)' }}
      >
        <div className="px-4 pt-2 pb-3 flex items-center gap-2.5">
          {/* #370 刀3(ADR 0009):☰ = 菜单 sheet(tab 栏退役后的二级面入口)。 */}
          <Pressable
            onClick={() => setMenuOpen(true)}
            className="w-9 h-9 rounded-full grid place-items-center text-ink-700 bg-cloud border border-ink-100"
            aria-label={t('nav.menu')}
          >
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round">
              <path d="M4 7h16M4 12h16M4 17h10" />
            </svg>
          </Pressable>
          <h1 className="font-display font-medium text-[24px] tracking-tight text-ink-900 leading-none">
            Cumora
          </h1>
          {/* 看板 = 唯一常驻非聊天入口(与桌面同款定位),直达资料库看板页。 */}
          <Pressable
            onClick={onOpenBoards}
            className="w-9 h-9 rounded-full grid place-items-center text-ink-700 bg-cloud border border-ink-100"
            aria-label={t('nav.boards')}
          >
            <IBoard className="w-[17px] h-[17px]" />
          </Pressable>
          <Pressable
            onClick={() => { setSearchOpen((v) => !v); if (searchOpen) setSearchQ('') }}
            className={cn(
              'ml-auto w-9 h-9 rounded-full grid place-items-center text-ink-700 border',
              searchOpen ? 'bg-sky2-50 border-sky2-200 text-skype-deep' : 'bg-cloud border-ink-100',
            )}
            aria-label={searchOpen ? t('mclist.closeSearch') : t('mclist.openSearch')}
          >
            <ISearch className="w-[18px] h-[18px]" />
          </Pressable>
        </div>

        {searchOpen && (
          <div className="px-4 pb-2">
            <div className="flex items-center gap-2 bg-cloud rounded-[14px] py-2 px-3"
              style={{ border: '1px solid var(--ink-100)' }}>
              <ISearch className="w-[16px] h-[16px] text-ink-500 shrink-0" />
              <input
                ref={searchRef}
                type="search"
                value={searchQ}
                onChange={(e) => setSearchQ(e.target.value)}
                placeholder={t('mclist.searchPh')}
                className="flex-1 bg-transparent outline-none text-[14px] text-ink-900 placeholder:text-ink-300"
                autoCapitalize="none"
                autoCorrect="off"
                enterKeyHint="search"
              />
              {searchQ && (
                <Pressable
                  onClick={() => setSearchQ('')}
                  className="text-ink-500 px-1.5 text-[18px] leading-none"
                  aria-label={t('mclist.clearSearch')}
                >×</Pressable>
              )}
            </div>
          </div>
        )}

        <div className="px-4 pb-2 flex gap-2 overflow-x-auto scroll-clean">
          {filters.map((f) => {
            const isActive = filter === f
            return (
              <Pressable
                key={f}
                onClick={() => setFilter(f)}
                scale={0.92}
                className={cn(
                  'py-1.5 px-3.5 text-[12px] font-semibold rounded-full whitespace-nowrap border',
                  !isActive && 'bg-cloud border-ink-100 text-ink-500',
                )}
                style={isActive ? {
                  background: 'var(--sky-100)',
                  color: 'var(--skype-deep)',
                  borderColor: 'var(--sky-200)',
                  boxShadow: '0 1px 2px -1px rgba(0, 120, 200, 0.12)',
                } : undefined}
              >{t(filterKey(f))}</Pressable>
            )
          })}
        </div>
      </div>

      <div className="flex-1 min-h-0">
        <PullToRefresh
          onRefresh={() => useConversations.getState().reload()}
          onScrollerReady={setScroller}
        >
          <div className="pb-2">
            {/* 「需要你行动」分区(#370 刀3,对齐桌面;InboxView 家族已无移动面)。 */}
            {actionRows.length > 0 && (
              <div className="px-4 pt-2 pb-1">
                <div className="flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300">
                  {t('convo.actionSection')}
                  {actionRows.length > 0 && (
                    <span
                      className="grid h-[16px] min-w-[16px] place-items-center rounded-full px-1 text-[9.5px] font-bold"
                      style={{ background: 'var(--coral)', color: 'white' }}
                    >{actionRows.length}</span>
                  )}
                </div>
                {(actionExpanded ? actionRows : actionRows.slice(0, 3)).map((it) => (
                  <Pressable
                    key={it.id}
                    onClick={() => openActionItem(it)}
                    className="mt-1.5 flex w-full items-start gap-2 rounded-[10px] border-l-[3px] bg-white/70 px-2.5 py-2 text-left"
                    style={{ borderLeftColor: it.read ? 'transparent' : it.severity === 'action_required' ? 'var(--coral)' : 'var(--skype)' }}
                  >
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-[13px] font-semibold text-ink-900">{it.title}</span>
                      {it.body && <span className="block truncate text-[11px] text-ink-500">{it.body}</span>}
                    </span>
                    <span className="shrink-0 text-[9.5px] tabular-nums text-ink-300">
                      {new Date(it.createdAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
                    </span>
                  </Pressable>
                ))}
                {actionRows.length > 3 && (
                  <Pressable
                    onClick={() => setActionExpanded((v) => !v)}
                    className="mt-1 px-1 text-[11px] italic text-ink-300 font-display"
                  >
                    {actionExpanded ? t('convo.actionCollapseMore') : t('convo.actionMore', { n: actionRows.length - 3 })}
                  </Pressable>
                )}
              </div>
            )}
            <PinnedRow
              pinned={pinned}
              onSelect={select}
              onLongPress={(c, coords) => setActionFor({ c, coords })}
            />
            {/* Pinned shelf scrolls with the list (same scroll parent); the
                virtualizer below recycles recent-conversation rows so a
                workspace with hundreds of chats doesn't pay re-render cost
                for off-screen rows on every store tick. customScrollParent
                hands the scroll element to PullToRefresh so pull-to-refresh
                and the gesture-bound `y` transform keep working. */}
            <div className="px-0 pt-1.5 pb-1">
              {byRecency.length === 0 ? (
                <div className="px-3 py-6 text-center text-[13px] text-ink-300 font-display italic">{t('mclist.empty')}</div>
              ) : scroller ? (
                <Virtuoso
                  customScrollParent={scroller}
                  data={byRecency}
                  computeItemKey={(_, c) => c.id}
                  // Generous viewport overscan so swipe-to-reveal targets
                  // are mounted slightly before they enter view.
                  increaseViewportBy={{ top: 400, bottom: 400 }}
                  itemContent={(_, c) => (
                    <SwipeableRow actions={convoSwipeActions(c)}>
                      <MobileRow
                        c={c}
                        onTap={() => select(c.id)}
                        onLongPress={(coords) => setActionFor({ c, coords })}
                      />
                    </SwipeableRow>
                  )}
                />
              ) : null}
            </div>
            {/* 「Agent 对话」分区(#370 刀3,owner 闸;whispers 视图/tab 退役,
                纯 agent 会话并入主列表,数据源 /peek/agent-chats)。 */}
            {isOwner && whispers.length > 0 && (
              <div className="px-4 pt-2 pb-3">
                <div className="flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.12em] text-whisper">
                  {t('convo.agentChats')}
                  <span className="text-[9px] opacity-70" title={t('convo.agentChatsOwnerOnly')}>🔒</span>
                </div>
                {whispers.slice(0, 20).map((w) => {
                  const ms = w.members.map((id) => byId[id]).filter((p): p is Participant => Boolean(p))
                  if (ms.length < 2) return null
                  const isGroup = w.kind === 'group' || ms.length > 2
                  return (
                    <Pressable
                      key={w.id}
                      onClick={() => select(w.id)}
                      className="mt-1.5 flex w-full items-center gap-2.5 rounded-[12px] px-1.5 py-2 text-left"
                    >
                      <HiveAvatar ps={ms} size={38} ringColor="var(--paper)" />
                      <span className="min-w-0 flex-1">
                        <span className="block truncate text-[13.5px] font-semibold text-ink-900">
                          {isGroup
                            ? (w.title || `${ms[0].name}, ${ms[1].name} …`)
                            : (<>{ms[0].name} <span className="text-[11px] text-whisper">↔</span> {ms[1].name}</>)}
                        </span>
                        <span className="block truncate text-[11.5px] text-ink-500 font-display italic">
                          {w.about ?? t('whispers.privateThread')}
                          <span className="not-italic text-ink-300"> · {w.msgCount}</span>
                        </span>
                      </span>
                    </Pressable>
                  )
                })}
              </div>
            )}
          </div>
        </PullToRefresh>
      </div>

      <MobileContextMenu
        open={actionFor !== null}
        anchor={actionFor?.coords ?? null}
        caption={actionFor ? (
          <span className="flex items-center gap-2">
            <span>{actionFor.c.kind}</span>
            <span className="text-ink-300">·</span>
            <span className="truncate normal-case font-semibold text-ink-700 tracking-normal text-[12px]">{actionFor.c.title}</span>
          </span>
        ) : undefined}
        items={actionFor ? convoMenuItems(actionFor.c, {
          meId,
          byId,
          onCreateGroupWith: (id) => {
            setActionFor(null)
            setCreating({ initialPicked: [id] })
          },
        }) : []}
        onClose={() => setActionFor(null)}
      />
      {creating && (
        <GroupCreator
          initialPicked={creating.initialPicked}
          onClose={() => setCreating(null)}
        />
      )}

      {/* ── 菜单 sheet(#370 刀3,MobileTabBar 退役后的二级面入口)──
          右滑覆盖层与 app 的 push/pop 语言一致;条目 = 有移动面的二级面:
          资料库(文档/看板/日历)/交付/智能体/我 + 退出钉底。hr/观测/
          技能库/项目暂无移动面,不在菜单虚构入口(记 #370 余量)。 */}
      <AnimatePresence>
        {menuOpen && (
          <>
            <motion.div
              key="menu-scrim"
              className="absolute inset-0 z-40"
              style={{ background: 'rgba(10, 27, 46, 0.30)' }}
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              onClick={() => setMenuOpen(false)}
            />
            <motion.nav
              key="menu-panel"
              className="absolute inset-y-0 left-0 z-50 flex w-[82%] max-w-[320px] flex-col bg-cloud"
              style={{ boxShadow: '18px 0 50px -16px rgba(10, 30, 60, 0.35)' }}
              initial={{ x: '-100%' }} animate={{ x: 0 }} exit={{ x: '-100%' }}
              transition={{ type: 'spring', stiffness: 320, damping: 38 }}
              aria-label={t('nav.menu')}
            >
              <div className="flex-1 overflow-y-auto px-3 py-4">
                <div className="px-2.5 pb-1 text-[10.5px] font-bold tracking-[0.05em] text-ink-300">{t('menu.work')}</div>
                <MenuRow label={t('nav.library')} onClick={() => { setView('library'); setMenuOpen(false) }}><IDoc className="w-[17px] h-[17px]" /></MenuRow>
                <MenuRow label={t('nav.boards')} onClick={() => { setMenuOpen(false); onOpenBoards() }}><IBoard className="w-[17px] h-[17px]" /></MenuRow>
                <MenuRow label={t('nav.ship')} onClick={() => { setView('shipping'); setMenuOpen(false) }}><IShip className="w-[17px] h-[17px]" /></MenuRow>
                <div className="mx-2.5 my-2 h-px bg-ink-100" />
                <div className="px-2.5 pb-1 text-[10.5px] font-bold tracking-[0.05em] text-ink-300">{t('menu.company')}</div>
                <MenuRow label={t('nav.agents')} onClick={() => { setView('agents'); setMenuOpen(false) }}><IAgent className="w-[17px] h-[17px]" /></MenuRow>
                <MenuRow label={t('nav.me')} onClick={() => { setView('me'); setMenuOpen(false) }}><IAgents className="w-[17px] h-[17px]" /></MenuRow>
              </div>
              <MenuRow
                label={t('common.signOut')}
                onClick={async () => {
                  try { await api.authLogout() } catch (e) { console.warn('[signout] server call failed', e) }
                  useAuth.getState().clear()
                  location.reload()
                }}
                danger
              >
                <IExit className="w-[15px] h-[15px]" />
              </MenuRow>
            </motion.nav>
          </>
        )}
      </AnimatePresence>
    </section>
  )
}

function MenuRow({ label, onClick, danger, children }: {
  label: string
  onClick: () => void
  danger?: boolean
  children?: React.ReactNode
}) {
  return (
    <Pressable
      onClick={onClick}
      className={cn(
        'flex w-full items-center gap-3 rounded-[12px] px-3 py-3 text-left text-[14px] font-medium',
        danger ? 'text-ink-500' : 'text-ink-900',
      )}
      aria-label={label}
    >
      <span className={cn('grid h-8 w-8 shrink-0 place-items-center rounded-[9px]', danger ? 'bg-paper border border-ink-100 text-ink-500' : 'bg-sky2-100 text-skype-deep')}>
        {children}
      </span>
      <span className="truncate">{label}</span>
    </Pressable>
  )
}
