// 子组件分桶(#219 ②):本文件保留 ConversationsPane 壳(筛选/搜索/取数状态、
// ⌘K 聚焦、右键菜单与弹窗的编排、Virtuoso 列表装配),原局部子组件按职责分居
// ./conversations/:
//   ConvoRow(行+ConvoAvatar/TeamAvatar/Tag/MutedGlyph)· search(搜索框
//   SearchInput+结果下拉 SearchResultsPane 族)· convoMenu(右键菜单装配)
//   · modals(AddToGroupPicker/ConfirmLeave/AddMembersPicker 三弹窗)
//   · shared(Translator+静音词表 MUTE_DURATIONS/muteHint/muteTooltip)。
// 会话数据层仍是 @/stores/conversations(#220 ① 的 applyMessageEvent 补丁),本刀未动。
import type React from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Virtuoso } from 'react-virtuoso'
import { type ApiProject, type ApiSearchResults, api, type ApiInboxItem } from '@/api/client'
import { ContextMenu, type ContextMenuItem } from '@/components/ContextMenu'
import { GroupCreator } from '@/components/GroupCreator'
import { IMail, IPlus } from '@/components/icons'
import { ResizeHandle } from '@/components/ResizeHandle'
import { type MessageKey, useLocale, useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useAuth } from '@/stores/auth'
import { isMuted, useConversations } from '@/stores/conversations'
import { useInbox } from '@/stores/inbox'
import { useParticipants } from '@/stores/participants'
import { useWhispers } from '@/stores/whispers'
import type { ApiWhisper } from '@/api/client'
import type { Conversation } from '@/types'
import { ConvoRow } from './conversations/ConvoRow'
import { openConvoContextMenu } from './conversations/convoMenu'
import { AddMembersPicker, AddToGroupPicker, ConfirmLeave } from './conversations/modals'
import { SearchInput, SearchResultsPane } from './conversations/search'
import { WhisperRow } from './conversations/WhisperRow'

const staticFilters = ['All', 'Unread', 'Agents', 'Humans', 'Groups', 'Email'] as const
type StaticFilter = (typeof staticFilters)[number]
/** Display labels for the static filter chips. The enum values in
 *  `staticFilters` stay English so the existing `matches()` comparison
 *  keeps working; this Record maps each enum to its translation key.
 *  (#368 刀1:'Whispers' 滤片随 WhispersView 退役移除——纯 agent 会话如今
 *  常驻主列表的「Agent 对话」分区,不再是一个要切进去的过滤器。) */
const FILTER_LABEL: Record<StaticFilter, MessageKey> = {
  All: 'convo.filterAll',
  Unread: 'convo.filterUnread',
  Agents: 'convo.filterAgents',
  Humans: 'convo.filterHumans',
  Groups: 'convo.filterGroups',
  Email: 'convo.filterEmail',
}
/** A filter is either one of the static labels, or a project chip identified
 *  by `project:<id>`. Keeping it as a string union lets the existing chip
 *  loop iterate uniformly. */
type Filter = StaticFilter | `project:${string}`

function matches(c: Conversation, f: Filter, byId: Record<string, { kind: string }>) {
  if (f.startsWith('project:')) {
    const projectId = f.slice('project:'.length)
    return c.projectId === projectId
  }
  if (f === 'All') return true
  // Muted convos are intentionally hidden from the Unread filter — the
  // whole point of mute is "stop nagging me about this". Their per-row
  // unread badge still shows under "All".
  if (f === 'Unread') return (c.unread ?? 0) > 0 && !isMuted(c)
  if (f === 'Email') return c.kind === 'email'
  if (f === 'Groups') return c.kind === 'group'
  const isHumanChat = c.tag === 'human' || c.members.every((m) => byId[m]?.kind === 'human')
  if (f === 'Humans') return isHumanChat
  if (f === 'Agents') return c.kind === 'direct' && !isHumanChat
  return true
}

/** Items rendered by the conversations Virtuoso — section labels and the
 *  Pinned/Rest divider live in the same flat list so the whole pane scrolls
 *  through one virtualized container. #368 刀1:末尾追加「Agent 对话」分区
 *  (wlabel + wrow),与普通会话同列表虚拟化。 */
type ConvoListItem =
  | { type: 'loading'; key: string }
  | { type: 'label'; key: string; text: string }
  | { type: 'divider'; key: string }
  | { type: 'row'; key: string; c: Conversation }
  | { type: 'wlabel'; key: string }
  | { type: 'wrow'; key: string; w: ApiWhisper }

export function ConversationsPane({ onResizeStart }: { onResizeStart?: (e: React.MouseEvent) => void }) {
  const t = useT()
  const locale = useLocale()
  const selected = useApp((s) => s.selectedConversationId)
  const select = useApp((s) => s.selectConversation)
  const setView = useApp((s) => s.setView)
  const list = useConversations((s) => s.list)
  const loaded = useConversations((s) => s.loaded)
  const byId = useParticipants((s) => s.byId)
  const [filter, setFilter] = useState<Filter>('All')
  const [query, setQuery] = useState('')
  const searchRef = useRef<HTMLInputElement | null>(null)
  // ⌘K / Ctrl+K — focus the search input from anywhere in the app.
  // We register globally rather than on the input so the user can trigger
  // it without first clicking the sidebar.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        searchRef.current?.focus()
        searchRef.current?.select()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  // Backend search: debounced API call with abort. We must not load every
  // message into the client — the universal search hits SQL on each
  // keystroke (after debounce) and returns four ranked buckets.
  const [results, setResults] = useState<ApiSearchResults | null>(null)
  const [searching, setSearching] = useState(false)
  useEffect(() => {
    const q = query.trim()
    if (!q) { setResults(null); setSearching(false); return }
    const ctl = new AbortController()
    setSearching(true)
    const handle = window.setTimeout(() => {
      api.search(q, ctl.signal)
        .then((r) => { setResults(r); setSearching(false) })
        .catch((err) => {
          // AbortError fires every time we cancel a stale request — silent.
          if ((err as { name?: string })?.name === 'AbortError') return
          console.warn('[search] failed', err)
          setSearching(false)
        })
    }, 150)
    return () => { window.clearTimeout(handle); ctl.abort() }
  }, [query])

  // Activating a search hit clears the search and jumps to the convo.
  // Pulled out as callbacks so the keyboard-Enter path and the click path
  // share a single implementation.
  const onSelectFromSearch = useCallback((id: string) => {
    setQuery('')
    select(id)
  }, [select])
  const onOpenDirectFromSearch = useCallback(async (pid: string) => {
    try {
      const { id } = await api.openDirect(pid)
      await useConversations.getState().reload()
      setQuery('')
      select(id)
    } catch (err) { console.warn('[search] openDirect failed', err) }
  }, [select])

  // Keyboard nav over the flat result list. The actions array mirrors the
  // visual order rendered by SearchResultsPane (people → rooms → groups →
  // messages); selectedIdx is an index into it. Enter activates; ↑/↓ and
  // Ctrl+P/Ctrl+N move; Esc clears (handled on the input itself).
  const searchActions = useMemo(() => {
    if (!results) return [] as Array<() => void>
    const out: Array<() => void> = []
    for (const p of results.participants) out.push(() => onOpenDirectFromSearch(p.id))
    for (const r of results.rooms)        out.push(() => onSelectFromSearch(r.id))
    for (const g of results.groups)       out.push(() => onSelectFromSearch(g.id))
    for (const m of results.messages)     out.push(() => onSelectFromSearch(m.conversationId))
    return out
  }, [results, onSelectFromSearch, onOpenDirectFromSearch])
  const [selectedIdx, setSelectedIdx] = useState(0)
  // Reset to the top item every time a new result set lands — otherwise
  // a stale index might point past the new (shorter) list.
  useEffect(() => { setSelectedIdx(0) }, [results])
  const [projects, setProjects] = useState<ApiProject[]>([])
  useEffect(() => {
    void api.listProjects()
      .then((list) => setProjects(list.filter((p) => p.status === 'active' && p.conversationCount > 0)))
      .catch(() => { /* ignore — chips just hide */ })
  }, [list.length])  // reload when conversations change so a new project group appears
  const [creating, setCreating] = useState(false)
  const [creatingWithMember, setCreatingWithMember] = useState<string | null>(null)
  const [addingToGroup, setAddingToGroup] = useState<{ participantId: string; name: string } | null>(null)
  const [addingMembersTo, setAddingMembersTo] = useState<Conversation | null>(null)
  const [menu, setMenu] = useState<{ x: number; y: number; items: ContextMenuItem[] } | null>(null)
  const [confirmLeave, setConfirmLeave] = useState<Conversation | null>(null)
  const meId = useAuth((s) => s.user?.id ?? null)

  // ── #368 刀1:两分区 + 菜单触发 ──────────────────────────────
  // ☰ 触发左侧滑出菜单(NavMenu 渲染在 DesktopApp,状态在 app store)。
  const toggleNavMenu = useApp((s) => s.toggleNavMenu)
  // 「需要你行动」分区(InboxView 退役):action_required + attention 进
  // 分区,info 不进——纯落账不占注意力(沿用收件箱徽标语义)。store 由
  // NotificationToasts 在启动/WS 时刷新,这里再兜一次拉取。
  const inboxItems = useInbox((s) => s.items)
  const inboxCounts = useInbox((s) => s.counts)
  const inboxMutedTypes = useInbox((s) => s.mutedTypes)
  const navMenuOpen = useApp((s) => s.navMenuOpen)
  const [actionCollapsed, setActionCollapsed] = useState(false)
  const [actionExpanded, setActionExpanded] = useState(false)
  const [mutesOpen, setMutesOpen] = useState(false)
  useEffect(() => { void useInbox.getState().load() }, [])
  // 「Agent 对话」分区(WhispersView 退役):owner 专属,数据源
  // /peek/agent-chats(bootWhispers 已常驻拉取,WS 维护新鲜度)。
  const isOwner = useAuth((s) => s.companies.find((c) => c.id === s.activeCompanyId)?.role === 'owner')
  const whispers = useWhispers((s) => s.list)
  const actionCount = inboxCounts.actionRequired + inboxCounts.attention
  const actionRows = useMemo(
    () => inboxItems.filter((it) => it.severity !== 'info')
      .sort((a, b) => Number(a.read) - Number(b.read) || b.createdAt.localeCompare(a.createdAt)),
    [inboxItems],
  )
  const openActionItem = (it: ApiInboxItem) => {
    if (!it.read) void useInbox.getState().markRead(it.id)
    if (it.linkKind === 'conversation' && it.linkId) {
      setView('conversations')
      select(it.linkId)
    } else if (it.linkKind === 'board') {
      setView('boards')
    } else if (it.linkKind === 'calendar') {
      setView('calendar')
    } else if (it.linkKind === 'observability') {
      setView('observability')
    }
  }

  const otherMember = (c: Conversation): string | null => {
    return c.members.find((m) => m !== meId) ?? null
  }

  const togglePin = async (c: Conversation) => {
    try {
      await api.togglePin(c.id, !c.pinned)
      await useConversations.getState().reload()
    } catch (err) { console.warn('[pin] failed', err) }
  }

  const setMute = async (c: Conversation, mute: boolean, until: Date | null) => {
    try {
      await api.setMute(c.id, mute, until ? until.toISOString() : null)
      await useConversations.getState().reload()
    } catch (err) { console.warn('[mute] failed', err) }
  }

  const openContextMenu = (c: Conversation, e: React.MouseEvent) => {
    openConvoContextMenu(c, e, {
      t, byId, togglePin, setMute, otherMember,
      setAddingMembersTo, setConfirmLeave, setCreatingWithMember, setAddingToGroup, setMenu,
    })
  }

  const filtered = useMemo(
    () => list.filter((c) => c.kind !== 'whisper' && matches(c, filter, byId)),
    [list, filter, byId],
  )
  // Pinned floats to the top. Everything else (groups, direct chats with
  // agents, direct chats with humans) goes into one flat list — the row
  // itself already shows whether it's a group (hive avatar) or a DM
  // (single avatar), so section headers were redundant.
  const pinned = filtered.filter((c) => c.pinned)
  const rest = filtered.filter((c) => !c.pinned)

  // Flat list for virtualization — single Virtuoso renders every row, label,
  // and divider via itemContent dispatch. Search-results branch stays on its
  // own (search lists are short + show a different layout).
  const items = useMemo<ConvoListItem[]>(() => {
    const out: ConvoListItem[] = []
    if (!loaded) out.push({ type: 'loading', key: 'loading' })
    if (pinned.length > 0) {
      out.push({ type: 'label', key: 'label:pinned', text: t('convo.pinned') })
      for (const c of pinned) out.push({ type: 'row', key: `p:${c.id}`, c })
      if (rest.length > 0) out.push({ type: 'divider', key: 'divider' })
    }
    for (const c of rest) out.push({ type: 'row', key: `r:${c.id}`, c })
    // 「Agent 对话」分区:仅 owner、仅在无滤片时随列表滚(分区不是过滤器,
    // 搜索/滤片态下隐藏与既有 chips 语义一致)。
    if (isOwner && filter === 'All' && whispers.length > 0) {
      out.push({ type: 'wlabel', key: 'wlabel' })
      for (const w of whispers) out.push({ type: 'wrow', key: `w:${w.id}`, w })
    }
    return out
    // locale in the deps so the baked-in section label re-renders on a
    // language switch (t itself is identity-unstable, locale is not).
  }, [loaded, pinned, rest, isOwner, filter, whispers, locale])

  const sectionLabel = (label: string, hint?: string) => (
    <div className="px-2 pt-3 pb-1.5 text-[10px] font-bold text-ink-300 tracking-[0.12em] uppercase flex items-center justify-between">
      <span>{label}</span>
      {hint && (
        <span className="text-coral text-[10px] not-italic font-semibold tracking-wide normal-case flex items-center gap-1.5">
          <span className="w-1.5 h-1.5 rounded-full bg-coral animate-pulse-soft" />
          {hint}
        </span>
      )}
    </div>
  )

  return (
    <aside className="relative flex flex-col overflow-hidden border-r border-ink-100 bg-paper">
      <div className="pt-3 px-[18px] pb-2 flex items-center gap-2">
        {/* #368 刀1:☰ = 左侧全高滑出菜单的触发钮(工作面/公司/我/退出
            都在里面;rail 已退役)。 */}
        <button
          type="button"
          onClick={toggleNavMenu}
          className="inline-flex items-center p-1.5 text-ink-700 bg-cloud border border-ink-100 rounded-[7px] hover:border-sky2-200 hover:text-skype-deep transition shrink-0"
          title={t('nav.menu')}
          aria-label={t('nav.menu')}
          aria-expanded={navMenuOpen}
        >
          <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
            <path d="M4 7h16M4 12h16M4 17h10" />
          </svg>
        </button>
        {/* 评审 P2-1(#372):行动计数聚合胶囊挂列表头 —— 分区块头的计数只
            在列表顶部可见,头部胶囊保证任何滚动/滤片态下待办信号不丢
            (对话未读总数仍由 Unread 滤片徽标承载)。点按 = 展开分区。 */}
        {actionCount > 0 && (
          <button
            type="button"
            onClick={() => setActionCollapsed(false)}
            className="inline-flex h-7 shrink-0 items-center gap-1.5 rounded-full border px-2.5 text-[11.5px] font-semibold transition-colors hover:bg-[#FFEFEA]"
            style={{ background: '#FFF6F5', borderColor: 'var(--coral-soft)', color: 'var(--coral-deep)' }}
            title={t('convo.actionSection')}
            aria-label={t('convo.actionSection')}
          >
            <span
              className="grid h-4 min-w-4 place-items-center rounded-full px-1 text-[9.5px] font-bold"
              style={{ background: 'var(--coral)', color: 'white' }}
            >{actionCount}</span>
            <span className="hidden sm:inline">{t('convo.actionSection')}</span>
          </button>
        )}
        <h1 className="font-display font-medium text-[20px] tracking-tight text-ink-900 leading-none flex-1 min-w-0 truncate whitespace-nowrap">
          {t('convo.title')}
          <svg
            viewBox="0 0 24 24"
            width="17" height="17"
            className="inline-block ml-2 text-skype-deep align-[-0.18em]"
            aria-hidden="true"
          >
            {/* Two soft overlapping chat bubbles — Cumora's mark of "this is where talking happens". */}
            <path d="M3.2 5.5a3 3 0 0 1 3-3h8.6a3 3 0 0 1 3 3v5a3 3 0 0 1-3 3h-2.4l-3.6 3.4v-3.4H6.2a3 3 0 0 1-3-3v-5z" fill="currentColor" opacity="0.95"/>
            <path d="M14 11.4h3.5a3 3 0 0 1 3 3v3.4a3 3 0 0 1-3 3h-1.1v2.6l-2.7-2.6h-1.6a3 3 0 0 1-3-3" fill="currentColor" opacity="0.42"/>
          </svg>
        </h1>
        {/* Compose new email — opens the EmailComposer drawer. Lives in
            the header so it's reachable regardless of which filter is
            active; mail isn't a "filter you have to be in" feature. */}
        {/* Header actions are icon-only (labels live in the tooltips) so the
            pane title never truncates or wraps at narrow pane widths. */}
        <button
          type="button"
          onClick={() => setCreating(true)}
          className="inline-flex items-center p-1.5 text-ink-700 bg-cloud border border-ink-100 rounded-[7px] hover:border-sky2-200 hover:text-skype-deep transition shrink-0"
          title={t('convo.newGroup')}
          aria-label={t('convo.newGroup')}
        >
          <IPlus className="w-3.5 h-3.5" strokeWidth={2.5} />
        </button>
        <button
          type="button"
          onClick={useApp.getState().openComposeNew}
          className="inline-flex items-center p-1.5 text-ink-700 bg-cloud border border-ink-100 rounded-[7px] hover:border-sky2-200 hover:text-skype-deep transition shrink-0"
          title={t('convo.newEmail')}
          aria-label={t('convo.newEmail')}
        >
          <IMail className="w-3.5 h-3.5" strokeWidth={2.5} />
        </button>
      </div>

      <SearchInput
        searchRef={searchRef}
        query={query}
        setQuery={setQuery}
        searchActions={searchActions}
        selectedIdx={selectedIdx}
        setSelectedIdx={setSelectedIdx}
      />

      {/* Filter chips are hidden while a search is active — the backend
          search has its own categorization and the chips would just add
          noise / conflict with the result buckets. */}
      {!query.trim() && (
      <div
        className="px-[18px] py-1 flex gap-1.5 overflow-x-auto scroll-clean"
        style={{
          // Belt-and-suspenders scrollbar hide — covers macOS users who
          // have "Always show scrollbars" enabled in System Settings,
          // which otherwise forces a track to render despite our CSS.
          scrollbarWidth: 'none',
          msOverflowStyle: 'none',
        } as React.CSSProperties}
      >
        {staticFilters.map((f) => {
          // Muted convos are excluded — mute means "don't tug at me". A
          // muted unread still shows its own per-row count, but it doesn't
          // pile onto this top-level "Unread" badge.
          const unreadTotal = list.reduce((s, c) => s + (isMuted(c) ? 0 : (c.unread ?? 0)), 0)
          const showBadge = f === 'Unread' && unreadTotal > 0
          const isActive = filter === f
          return (
            <button
              key={f}
              onClick={() => setFilter(f)}
              className={cn(
                'py-[5px] px-[11px] text-[11px] font-semibold rounded-full whitespace-nowrap border transition inline-flex items-center gap-1.5',
                !isActive && 'bg-cloud border-ink-100 text-ink-500 hover:border-ink-200',
              )}
              style={isActive ? {
                background: 'var(--sky-100)',
                color: 'var(--skype-deep)',
                borderColor: 'var(--sky-200)',
                boxShadow: '0 1px 2px -1px rgba(0, 120, 200, 0.12)',
              } : undefined}
            >
              {t(FILTER_LABEL[f])}
              {showBadge && (
                <span
                  className="inline-grid place-items-center min-w-[16px] h-4 px-1 rounded-full text-[9.5px] font-bold"
                  style={{
                    background: isActive ? 'var(--skype)' : 'var(--coral)',
                    color: 'white',
                  }}
                >{unreadTotal}</span>
              )}
            </button>
          )
        })}
        {projects.length > 0 && (
          <span className="self-center mx-0.5 text-ink-200 text-[14px] leading-none select-none">·</span>
        )}
        {projects.map((p) => {
          const filterKey = `project:${p.id}` as const
          const isActive = filter === filterKey
          return (
            <button
              key={p.id}
              onClick={() => setFilter(filterKey)}
              className={cn(
                'py-[5px] px-[11px] text-[11px] font-semibold rounded-full whitespace-nowrap border transition inline-flex items-center gap-1.5',
                !isActive && 'bg-cloud border-ink-100 text-ink-500 hover:border-ink-200',
              )}
              style={isActive ? {
                background: p.color ?? 'var(--sky-100)',
                color: p.color ? 'white' : 'var(--skype-deep)',
                borderColor: p.color ?? 'var(--sky-200)',
                boxShadow: '0 1px 2px -1px rgba(0, 120, 200, 0.12)',
              } : undefined}
              title={p.description || p.name}
            >
              <span
                className="w-2 h-2 rounded-full shrink-0"
                style={{ background: isActive ? 'rgba(255,255,255,0.85)' : (p.color ?? 'var(--ink-200)') }}
              />
              {p.name}
            </button>
          )
        })}
      </div>
      )}

      {/* ── 「需要你行动」高优分区(#368 刀1,InboxView 退役)────────────
          非虚拟化固定块:条目量级天然有界(action_required + attention),
          常驻列表顶、可折叠;搜索态隐藏(与滤片同语义)。 */}
      {!query.trim() && actionRows.length > 0 && (
        <div className="px-2.5 pt-1">
          <div className="relative flex items-center gap-1 px-1 pb-1">
            <button
              type="button"
              onClick={() => setActionCollapsed((v) => !v)}
              className="flex flex-1 items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300 hover:text-ink-500 transition-colors text-left"
              aria-expanded={!actionCollapsed}
            >
              <span className={cn('inline-block transition-transform', actionCollapsed && '-rotate-90')}>▾</span>
              {t('convo.actionSection')}
              {actionCount > 0 && (
                <span
                  className="ml-0.5 grid h-[16px] min-w-[16px] place-items-center rounded-full px-1 text-[9.5px] font-bold"
                  style={{ background: 'var(--coral)', color: 'white' }}
                >{actionCount}</span>
              )}
            </button>
            {/* 收件箱时代的两枚操作随视图退役迁入分区头:全部已读 + 按 type 静音。 */}
            <button
              type="button"
              onClick={() => { void useInbox.getState().markAllRead() }}
              className="text-[10px] font-semibold text-ink-300 hover:text-skype-deep transition-colors"
              title={t('inbox.readAll')}
            >✓</button>
            <button
              type="button"
              onClick={() => setMutesOpen((v) => !v)}
              className="text-[11px] font-bold text-ink-300 hover:text-skype-deep transition-colors px-0.5"
              title={t('inbox.mutes')}
              aria-label={t('inbox.mutes')}
            >⋯</button>
            {mutesOpen && (
              <div className="absolute right-1 top-6 z-20 rounded-xl border border-ink-100 bg-cloud p-2.5 shadow-lg max-w-[240px]">
                <p className="mb-1.5 text-[10.5px] font-semibold text-ink-500">{t('inbox.mutesHint')}</p>
                <div className="flex flex-wrap gap-1.5">
                  {Array.from(new Set(inboxItems.map((it) => it.type))).sort().map((type) => {
                    const muted = inboxMutedTypes.includes(type)
                    return (
                      <button
                        key={type}
                        type="button"
                        onClick={() => {
                          void useInbox.getState().setMutes(muted ? inboxMutedTypes.filter((x) => x !== type) : [...inboxMutedTypes, type])
                        }}
                        className={cn(
                          'rounded-full px-2 py-0.5 font-mono text-[10px]',
                          muted ? 'bg-ink text-cloud' : 'bg-ink/5 text-ink-700',
                        )}
                      >{type}{muted ? ' 🔇' : ''}</button>
                    )
                  })}
                </div>
              </div>
            )}
          </div>
          {!actionCollapsed && (actionExpanded ? actionRows : actionRows.slice(0, 6)).map((it) => (
            <button
              key={it.id}
              type="button"
              onClick={() => openActionItem(it)}
              className={cn(
                'mb-0.5 flex w-full items-start gap-2 rounded-[10px] border-l-[3px] px-2.5 py-2 text-left transition-colors',
                it.read ? 'border-transparent opacity-60 hover:bg-ink/[0.03]' : 'border-transparent bg-white/60 hover:bg-white',
              )}
              style={{ borderLeftColor: it.read ? 'transparent' : it.severity === 'action_required' ? 'var(--coral)' : 'var(--skype)' }}
            >
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[12.5px] font-semibold text-ink-900">{it.title}</span>
                {it.body && <span className="block truncate text-[11px] text-ink-500">{it.body}</span>}
              </span>
              <span className="shrink-0 text-[9.5px] tabular-nums text-ink-300">
                {new Date(it.createdAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
              </span>
            </button>
          ))}
          {!actionCollapsed && actionRows.length > 6 && (
            // #372 评审 P3-1:截断行从纯展示改为可展开 —— 第 7+ 条不再无路径。
            <button
              type="button"
              onClick={() => setActionExpanded((v) => !v)}
              aria-expanded={actionExpanded}
              className="w-full px-2.5 py-1 text-left text-[10.5px] italic text-ink-300 font-display hover:text-skype-deep transition-colors"
            >
              {actionExpanded
                ? t('convo.actionCollapseMore')
                : t('convo.actionMore', { n: actionRows.length - 6 })}
            </button>
          )}
        </div>
      )}

      {query.trim() ? (
        // Search results — short list with its own layout; non-virtualized.
        <div className="flex-1 overflow-y-auto px-2.5 pb-[18px]">
          <SearchResultsPane
            query={query}
            results={results}
            loading={searching}
            selectedIdx={selectedIdx}
            onHover={setSelectedIdx}
            onSelectConversation={onSelectFromSearch}
            onOpenDirect={onOpenDirectFromSearch}
          />
        </div>
      ) : (
        // The real conversation list — virtualized so workspaces with hundreds
        // of rooms stay snappy. The flat `items` array mixes section labels,
        // the Pinned/Rest divider, and rows; itemContent dispatches by type.
        <div className="flex-1 min-h-0 px-2.5">
          <Virtuoso
            className="h-full"
            data={items}
            computeItemKey={(_, item) => item.key}
            // A typical row is ~62px (avatar + 2 lines). Labels/dividers are
            // smaller; Virtuoso measures everything anyway, this is just the
            // first-pass estimate so the initial scroll isn't jumpy.
            defaultItemHeight={62}
            increaseViewportBy={{ top: 600, bottom: 600 }}
            components={{ Footer: () => <div style={{ height: 18 }} /> }}
            itemContent={(_, item) => {
              if (item.type === 'loading') {
                return <div className="px-3 py-4 text-[12px] text-ink-300 italic font-display">{t('convo.loading')}</div>
              }
              if (item.type === 'label') return sectionLabel(item.text)
              if (item.type === 'wlabel') {
                // 「Agent 对话」分区头 —— whisper 紫寄存器 + owner 锁语义。
                return (
                  <div className="flex items-center gap-1.5 px-2 pt-4 pb-1.5 text-[10px] font-bold tracking-[0.12em] uppercase text-whisper">
                    {t('convo.agentChats')}
                    <span className="text-[9px] opacity-70" title={t('convo.agentChatsOwnerOnly')}>🔒</span>
                  </div>
                )
              }
              if (item.type === 'wrow') {
                return (
                  <WhisperRow
                    w={item.w}
                    selected={selected === item.w.id}
                    onClick={() => select(item.w.id)}
                  />
                )
              }
              if (item.type === 'divider') {
                // Hairline divider — fading double rule between Pinned + Rest.
                return (
                  <div className="px-3 my-2" aria-hidden="true">
                    <div
                      style={{
                        height: 1,
                        background: 'linear-gradient(90deg, transparent 0%, var(--ink-100) 22%, var(--ink-100) 78%, transparent 100%)',
                      }}
                    />
                  </div>
                )
              }
              const c = item.c
              return (
                <ConvoRow
                  c={c}
                  selected={selected === c.id}
                  onClick={() => select(c.id)}
                  onContextMenu={(e) => openContextMenu(c, e)}
                />
              )
            }}
          />
        </div>
      )}

      {menu && (
        <ContextMenu x={menu.x} y={menu.y} items={menu.items} onClose={() => setMenu(null)} />
      )}
      {creating && <GroupCreator onClose={() => setCreating(false)} />}
      {creatingWithMember && (
        <GroupCreator
          initialPicked={[creatingWithMember]}
          onClose={() => setCreatingWithMember(null)}
        />
      )}
      {addingToGroup && (
        <AddToGroupPicker
          participantId={addingToGroup.participantId}
          participantName={addingToGroup.name}
          groups={list.filter((c) => c.kind === 'group' && !c.members.includes(addingToGroup.participantId))}
          onClose={() => setAddingToGroup(null)}
        />
      )}
      {addingMembersTo && (
        <AddMembersPicker
          group={addingMembersTo}
          candidates={Object.values(byId).filter((p) => !addingMembersTo.members.includes(p.id) && p.id !== meId)}
          onClose={() => setAddingMembersTo(null)}
        />
      )}
      {confirmLeave && (
        <ConfirmLeave
          c={confirmLeave}
          onCancel={() => setConfirmLeave(null)}
          onLeft={async () => {
            try { await api.leaveConversation(confirmLeave.id) } catch (e) { console.warn('leave failed', e) }
            await useConversations.getState().reload()
            if (selected === confirmLeave.id) select(null)
            setConfirmLeave(null)
          }}
        />
      )}
      {onResizeStart && <ResizeHandle onMouseDown={onResizeStart} />}
    </aside>
  )
}
