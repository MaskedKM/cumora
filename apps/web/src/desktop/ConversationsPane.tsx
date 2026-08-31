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
import { type ApiProject, type ApiSearchResults, api } from '@/api/client'
import { ContextMenu, type ContextMenuItem } from '@/components/ContextMenu'
import { GroupCreator } from '@/components/GroupCreator'
import { IMail, IPlus } from '@/components/icons'
import { ResizeHandle } from '@/components/ResizeHandle'
import { type MessageKey, useLocale, useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useAuth } from '@/stores/auth'
import { isMuted, useConversations } from '@/stores/conversations'
import { useParticipants } from '@/stores/participants'
import type { Conversation } from '@/types'
import { ConvoRow } from './conversations/ConvoRow'
import { openConvoContextMenu } from './conversations/convoMenu'
import { AddMembersPicker, AddToGroupPicker, ConfirmLeave } from './conversations/modals'
import { SearchInput, SearchResultsPane } from './conversations/search'

const staticFilters = ['All', 'Unread', 'Agents', 'Humans', 'Groups', 'Email', 'Whispers'] as const
type StaticFilter = (typeof staticFilters)[number]
/** Display labels for the static filter chips. The enum values in
 *  `staticFilters` stay English so the existing `matches()` comparison
 *  keeps working; this Record maps each enum to its translation key. */
const FILTER_LABEL: Record<StaticFilter, MessageKey> = {
  All: 'convo.filterAll',
  Unread: 'convo.filterUnread',
  Agents: 'convo.filterAgents',
  Humans: 'convo.filterHumans',
  Groups: 'convo.filterGroups',
  Email: 'convo.filterEmail',
  Whispers: 'convo.filterWhispers',
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
  if (f === 'Whispers') return c.kind === 'whisper'
  if (f === 'Email') return c.kind === 'email'
  if (f === 'Groups') return c.kind === 'group'
  const isHumanChat = c.tag === 'human' || c.members.every((m) => byId[m]?.kind === 'human')
  if (f === 'Humans') return isHumanChat
  if (f === 'Agents') return c.kind === 'direct' && !isHumanChat
  return true
}

/** Items rendered by the conversations Virtuoso — section labels and the
 *  Pinned/Rest divider live in the same flat list so the whole pane scrolls
 *  through one virtualized container. */
type ConvoListItem =
  | { type: 'loading'; key: string }
  | { type: 'label'; key: string; text: string }
  | { type: 'divider'; key: string }
  | { type: 'row'; key: string; c: Conversation }

export function ConversationsPane({ onResizeStart }: { onResizeStart?: (e: React.MouseEvent) => void }) {
  const t = useT()
  const locale = useLocale()
  const selected = useApp((s) => s.selectedConversationId)
  const select = useApp((s) => s.selectConversation)
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
    return out
    // locale in the deps so the baked-in section label re-renders on a
    // language switch (t itself is identity-unstable, locale is not).
  }, [loaded, pinned, rest, locale])

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
