// 搜索件(#219 ②)—— 从 ConversationsPane.tsx 原样搬移:
// SearchInput(输入框,状态仍由壳持有、经 props 透传)+ 结果下拉
// (SearchResultsPane/SearchConvoButton/SearchSection)与高亮工具。
import type React from 'react'
import { type Dispatch, type RefObject, type SetStateAction, useEffect, useRef } from 'react'
import type { ApiSearchResults } from '@/api/client'
import { Avatar } from '@/components/Avatar'
import { HiveAvatar } from '@/components/HiveAvatar'
import { ISearch } from '@/components/icons'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useMe } from '@/stores/auth'
import { useParticipants } from '@/stores/participants'
import type { Participant } from '@/types'

/** Strip MD/mention sigils so message snippets read as plain text — we
 *  don't render full markdown in the search dropdown, and seeing raw "**"
 *  and "[@id]" makes hits look noisy. */
function plainSnippet(s: string): string {
  return s
    .replace(/\[@[^\]]+\]/g, (m) => m.replace(/^\[@/, '@').replace(/\]$/, ''))
    .replace(/\*\*/g, '')
    .replace(/`([^`]+)`/g, '$1')
}

/** Highlight every case-insensitive occurrence of `needle` inside `text`. */
function highlight(text: string, needle: string): React.ReactNode {
  if (!needle) return text
  const lo = text.toLowerCase()
  const n = needle.toLowerCase()
  const out: React.ReactNode[] = []
  let i = 0
  let k = 0
  while (i < text.length) {
    const j = lo.indexOf(n, i)
    if (j < 0) { out.push(text.slice(i)); break }
    if (j > i) out.push(text.slice(i, j))
    out.push(<mark key={k++} className="bg-gold/40 text-ink-900 rounded-sm px-0.5">{text.slice(j, j + needle.length)}</mark>)
    i = j + needle.length
  }
  return <>{out}</>
}

interface SearchResultsPaneProps {
  query: string
  results: ApiSearchResults | null
  loading: boolean
  selectedIdx: number
  onHover: (idx: number) => void
  onSelectConversation: (id: string) => void
  onOpenDirect: (participantId: string) => void
}

function SearchSection({ label, count, children }: { label: string; count: number; children: React.ReactNode }) {
  if (count === 0) return null
  return (
    <div className="mb-2">
      <div className="px-3 pt-3 pb-1 text-[10px] font-bold text-ink-300 tracking-[0.12em] uppercase flex items-center justify-between">
        <span>{label}</span>
        <span className="text-ink-200 tracking-normal normal-case">{count}</span>
      </div>
      {children}
    </div>
  )
}

/** Shared row classes — `bg-sky2-100` highlights the keyboard-selected row.
 *  The unselected hover state shows on mouse-only nav, but we keep `onHover`
 *  syncing selectedIdx so the highlight tracks the cursor too. */
function rowClass(isSelected: boolean): string {
  return cn(
    'w-full text-left py-2 px-3 rounded-[10px] transition',
    isSelected ? 'bg-sky2-100' : 'hover:bg-sky2-50',
  )
}

export function SearchResultsPane({
  query, results, loading, selectedIdx, onHover, onSelectConversation, onOpenDirect,
}: SearchResultsPaneProps) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const q = query.trim()

  // Auto-scroll the keyboard-selected row into view. We look it up by data
  // attribute so the ref doesn't need to be re-bound on every reorder.
  const containerRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    const el = containerRef.current?.querySelector<HTMLElement>(`[data-search-idx="${selectedIdx}"]`)
    el?.scrollIntoView({ block: 'nearest' })
  }, [selectedIdx])

  if (loading && !results) {
    return <div className="px-4 py-6 text-[12px] text-ink-300 italic font-display">{t('convo.searching')}</div>
  }
  if (!results) return null
  const total = results.participants.length + results.rooms.length + results.groups.length + results.messages.length
  if (total === 0) {
    return (
      <div className="px-4 py-8 text-center text-[12.5px] text-ink-300 italic font-display">
        {t('convo.noMatches')} <span className="text-ink-700 not-italic font-semibold">"{q}"</span>
      </div>
    )
  }

  // Bucket offsets so each row knows its global index — that's what
  // selectedIdx points into, and what onHover reports back.
  const peopleStart = 0
  const roomsStart = peopleStart + results.participants.length
  const groupsStart = roomsStart + results.rooms.length
  const messagesStart = groupsStart + results.groups.length

  return (
    <div ref={containerRef}>
      <SearchSection label={t('convo.searchPeople')} count={results.participants.length}>
        {results.participants.map((p, i) => {
          const idx = peopleStart + i
          const sel = idx === selectedIdx
          return (
            <button
              key={`p-${p.id}`}
              data-search-idx={idx}
              onMouseEnter={() => onHover(idx)}
              onClick={() => onOpenDirect(p.id)}
              className={cn(rowClass(sel), 'grid grid-cols-[36px_1fr_auto] gap-[11px] items-center')}
            >
              <Avatar
                p={{
                  id: p.id, kind: p.kind, name: p.name, role: p.role ?? undefined,
                  initial: p.initial, avatarBg: p.avatarBg, avatarUrl: p.avatarUrl ?? undefined,
                  status: p.status,
                } as Participant}
                size={36}
                ringColor="var(--paper)"
                showStatus={false}
              />
              <div className="min-w-0">
                <div className="text-[13px] font-semibold text-ink-900 truncate">{highlight(p.name, q)}</div>
                <div className="text-[11px] text-ink-500 truncate">
                  {p.role ?? (p.kind === 'agent' ? t('convo.roleAgent') : t('convo.roleHuman'))}
                </div>
              </div>
              <span className="text-[9px] font-bold py-px px-1.5 rounded uppercase tracking-wider whitespace-nowrap"
                style={{
                  background: p.kind === 'agent' ? 'var(--sky-100, #E1F3FD)' : 'var(--coral-soft)',
                  color: p.kind === 'agent' ? 'var(--skype-deep)' : '#B23A2A',
                }}>{p.kind}</span>
            </button>
          )
        })}
      </SearchSection>

      <SearchSection label={t('convo.searchRooms')} count={results.rooms.length}>
        {results.rooms.map((r, i) => {
          const idx = roomsStart + i
          return (
            <SearchConvoButton
              key={`r-${r.id}`}
              id={r.id}
              kind={r.kind}
              title={r.title}
              members={r.members}
              byId={byId}
              query={q}
              index={idx}
              isSelected={idx === selectedIdx}
              onHover={onHover}
              onClick={() => onSelectConversation(r.id)}
            />
          )
        })}
      </SearchSection>

      <SearchSection label={t('convo.searchGroups')} count={results.groups.length}>
        {results.groups.map((g, i) => {
          const idx = groupsStart + i
          return (
            <SearchConvoButton
              key={`g-${g.id}`}
              id={g.id}
              kind="group"
              title={g.title}
              members={g.members}
              byId={byId}
              query={q}
              index={idx}
              isSelected={idx === selectedIdx}
              onHover={onHover}
              onClick={() => onSelectConversation(g.id)}
            />
          )
        })}
      </SearchSection>

      <SearchSection label={t('convo.searchMessages')} count={results.messages.length}>
        {results.messages.map((m, i) => {
          const idx = messagesStart + i
          const sel = idx === selectedIdx
          const when = new Date(m.createdAt)
          const tsLabel = Number.isNaN(when.getTime()) ? '' : when.toLocaleString([], {
            month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit',
          })
          return (
            <button
              key={`m-${m.id}`}
              data-search-idx={idx}
              onMouseEnter={() => onHover(idx)}
              onClick={() => onSelectConversation(m.conversationId)}
              className={rowClass(sel)}
            >
              <div className="flex items-center justify-between gap-2 mb-0.5">
                <div className="text-[12px] font-semibold text-ink-900 truncate">
                  <span className="text-ink-500 font-normal">{t('convo.inConvo')}</span> {m.conversationTitle}
                </div>
                <div className="text-[10px] text-ink-300 tabular-nums shrink-0">{tsLabel}</div>
              </div>
              <div className="text-[11.5px] text-ink-500 leading-[1.45] line-clamp-2">
                <span className="text-ink-700 font-medium">{m.authorName ?? t('convo.unknown')}: </span>
                {highlight(plainSnippet(m.snippet), q)}
              </div>
            </button>
          )
        })}
      </SearchSection>
    </div>
  )
}

function SearchConvoButton({ id, kind, title, members, byId, query, index, isSelected, onHover, onClick }: {
  id: string
  kind: 'direct' | 'whisper' | 'group'
  title: string
  members: string[]
  byId: Record<string, Participant>
  query: string
  index: number
  isSelected: boolean
  onHover: (idx: number) => void
  onClick: () => void
}) {
  const t = useT()
  const meId = useMe()
  const ps = members.map((m) => byId[m]).filter((p): p is Participant => Boolean(p))
  // Build a single-button row that mirrors ConvoAvatar's logic but in
  // miniature so the search dropdown rows are visually compact.
  const avatar = (() => {
    if (kind === 'direct') {
      const other = ps.find((p) => p.id !== meId) ?? ps[0]
      return other ? <Avatar p={other} size={36} ringColor="var(--paper)" showStatus={false} /> : null
    }
    return <HiveAvatar ps={ps} size={36} ringColor="var(--paper)" />
  })()
  return (
    <button
      key={id}
      data-search-idx={index}
      onMouseEnter={() => onHover(index)}
      onClick={onClick}
      className={cn(rowClass(isSelected), 'grid grid-cols-[36px_1fr] gap-[11px] items-center')}
    >
      {avatar}
      <div className="min-w-0">
        <div className="text-[13px] font-semibold text-ink-900 truncate">{highlight(title, query)}</div>
        <div className="text-[11px] text-ink-500 truncate">
          {kind === 'whisper' ? t('convo.kindWhisper') : kind === 'group' ? t('convo.kindGroup') : ''}
          {t(members.length === 1 ? 'convo.member' : 'convo.members', { n: members.length })}
        </div>
      </div>
    </button>
  )
}

interface SearchInputProps {
  searchRef: RefObject<HTMLInputElement>
  query: string
  setQuery: Dispatch<SetStateAction<string>>
  searchActions: Array<() => void>
  selectedIdx: number
  setSelectedIdx: Dispatch<SetStateAction<number>>
}

/** #219 ②: 原样搬自壳的搜索框 JSX(仅去一级缩进)。查询/结果/键盘导航
 *  状态与 ⌘K 聚焦 effect 留在壳里,这里只渲染输入框本身。 */
export function SearchInput({ searchRef, query, setQuery, searchActions, selectedIdx, setSelectedIdx }: SearchInputProps) {
  const t = useT()
  return (
    <div className="mx-[18px] mb-1 flex items-center gap-2.5 py-1 px-3 bg-cloud border border-ink-100 rounded-[10px] text-ink-300 text-[13px] focus-within:border-sky2-300">
      <ISearch className="w-3.5 h-3.5" strokeWidth={2} />
      <input
        ref={searchRef}
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          // Esc: bail out of the search overlay entirely.
          if (e.key === 'Escape') { setQuery(''); searchRef.current?.blur(); return }
          // Navigation only matters while we have a result set to move
          // through. Both Emacs (Ctrl+P/N) and arrow keys are accepted —
          // power users on macOS hit Ctrl+P long before they reach for
          // an arrow key.
          const len = searchActions.length
          if (len === 0) return
          const isDown = e.key === 'ArrowDown' || (e.ctrlKey && e.key.toLowerCase() === 'n')
          const isUp = e.key === 'ArrowUp' || (e.ctrlKey && e.key.toLowerCase() === 'p')
          if (isDown) { e.preventDefault(); setSelectedIdx((i) => Math.min(len - 1, i + 1)); return }
          if (isUp)   { e.preventDefault(); setSelectedIdx((i) => Math.max(0, i - 1)); return }
          if (e.key === 'Enter') {
            e.preventDefault()
            const act = searchActions[Math.max(0, Math.min(len - 1, selectedIdx))]
            if (act) act()
          }
        }}
        className="flex-1 min-w-0 bg-transparent outline-none text-ink-700 placeholder:text-ink-300"
        placeholder={t('convo.searchPlaceholder')}
      />
      {query ? (
        <button
          type="button"
          onClick={() => { setQuery(''); searchRef.current?.focus() }}
          className="shrink-0 grid place-items-center w-4 h-4 rounded-full bg-ink-100 hover:bg-ink-200 text-ink-500 transition"
          aria-label={t('convo.clearSearch')}
        >
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" className="w-2.5 h-2.5">
            <line x1="6" y1="6" x2="18" y2="18" />
            <line x1="18" y1="6" x2="6" y2="18" />
          </svg>
        </button>
      ) : (
        <kbd className="font-mono text-[10px] py-px px-1.5 bg-ink-100 rounded text-ink-500 shrink-0">⌘K</kbd>
      )}
    </div>
  )
}
