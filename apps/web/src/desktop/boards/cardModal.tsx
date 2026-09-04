// 卡片详情弹窗件(#219 ③)—— 从 BoardsView.tsx 原样搬移:
// CardDetailModal(标题/描述/列/经办人/评论 编辑与展示)、
// AssigneePicker(经办人下拉)与 formatTime 助手。开关状态由 canvas 持有。
import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { AvatarMini } from '@/components/Avatar'
import { Select } from '@/components/Select'
import { WorkspaceLinkModal } from '@/components/WorkspaceLinkModal'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useAuth, useMe } from '@/stores/auth'
import { useBoards } from '@/stores/boards'
import { useParticipants } from '@/stores/participants'
import type { BoardCard, BoardCardComment, BoardColumn, Participant } from '@/types'
import { hasLinkedReference, MentionedText, MentionInput } from './mentions'

/** Module-level constant so the "no comments yet" fallback is referentially
 *  stable across renders — see the comment in CardDetailModal where it's used. */
const EMPTY_COMMENTS: BoardCardComment[] = []

export function CardDetailModal({ boardId, card, columns, onClose }: {
  boardId: string; card: BoardCard; columns: BoardColumn[]; onClose: () => void
}) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  // board_card 关联服务端要求 owner/admin(AddWorkspaceAssociation 分层),
  // 前端同门(#338)。
  const role = useAuth((st) => st.companies.find((c) => c.id === st.activeCompanyId)?.role)
  const canManage = role === 'owner' || role === 'admin'
  const patchCard = useBoards((s) => s.patchCard)
  const deleteCard = useBoards((s) => s.deleteCard)
  const loadComments = useBoards((s) => s.loadComments)
  const addComment = useBoards((s) => s.addComment)
  // Select the raw entry (may be undefined), then fall back OUTSIDE the
  // selector — `?? []` inside would mint a new array literal on every
  // call, fail Object.is, and cycle render → reselect → render forever.
  const commentsRaw = useBoards((s) => s.comments[card.id])
  const comments: BoardCardComment[] = commentsRaw ?? EMPTY_COMMENTS
  const [title, setTitle] = useState(card.title)
  const [description, setDescription] = useState(card.description ?? '')
  const [draftComment, setDraftComment] = useState('')
  const [posting, setPosting] = useState(false)
  const [linkingWs, setLinkingWs] = useState(false)

  useEffect(() => {
    setTitle(card.title)
    setDescription(card.description ?? '')
  }, [card.id, card.title, card.description])

  useEffect(() => {
    void loadComments(boardId, card.id)
  }, [loadComments, boardId, card.id])

  async function saveTitle() {
    const next = title.trim()
    if (!next || next === card.title) return
    try { await patchCard(boardId, card.id, { title: next }) } catch (e) { console.warn(e) }
  }
  async function saveDescription() {
    const next = description.trim()
    if (next === (card.description ?? '')) return
    try { await patchCard(boardId, card.id, { description: next }) } catch (e) { console.warn(e) }
  }
  async function moveToColumn(columnId: string) {
    if (columnId === card.columnId) return
    try { await patchCard(boardId, card.id, { columnId }) } catch (e) { console.warn(e) }
  }
  async function setAssignee(id: string | null) {
    try { await patchCard(boardId, card.id, { assigneeId: id }) } catch (e) { console.warn(e) }
  }
  async function postComment() {
    const body = draftComment.trim()
    if (!body || posting) return
    setPosting(true)
    try {
      await addComment(boardId, card.id, body)
      setDraftComment('')
    } catch (e) {
      console.warn(e)
    } finally {
      setPosting(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-ink-900/40 p-6" onClick={onClose}>
      <div
        onClick={(e) => e.stopPropagation()}
        className="bg-cloud w-full max-w-2xl max-h-[85vh] rounded-xl shadow-xl flex flex-col"
      >
        <header className="px-5 py-4 border-b border-ink-100 flex items-start gap-3">
          <div className="min-w-0 flex-1">
            <MentionInput
              value={title}
              onChange={setTitle}
              onSubmit={() => void saveTitle()}
              placeholder={t('boards.cardTitleEditPlaceholder')}
              className="-ml-2 w-full border-transparent bg-transparent px-2 py-1.5 text-[19px] font-semibold leading-7 text-ink-900 placeholder:text-ink-300 focus:border-skype/30 focus:bg-white focus:ring-2 focus:ring-skype/15"
            />
          </div>
          {canManage && (
            <button
              type="button"
              onClick={() => setLinkingWs(true)}
              className="shrink-0 rounded-md px-2.5 py-1.5 text-sm text-ink-500 hover:bg-sky2-50 hover:text-skype-deep"
              title={t('wsLink.title')}
            >⌗</button>
          )}
          <button
            type="button"
            onClick={() => { void saveTitle().then(onClose) }}
            className="shrink-0 rounded-md px-2.5 py-1.5 text-sm text-ink-500 hover:bg-sky2-50 hover:text-skype-deep"
          >{t('common.close')}</button>
        </header>
        {linkingWs && (
          <WorkspaceLinkModal kind="board_card" targetId={card.id} onClose={() => setLinkingWs(false)} />
        )}

        <div className="flex-1 overflow-y-auto px-5 py-4 space-y-5">
          <section className="grid grid-cols-2 gap-4">
            <div>
              <div className="text-[11px] uppercase tracking-wide text-ink-400 mb-1">{t('boards.column')}</div>
              <Select
                value={card.columnId}
                onValueChange={(columnId) => void moveToColumn(columnId)}
                options={columns.map((c) => ({ value: c.id, label: c.title }))}
                ariaLabel={t('boards.ariaColumn')}
              />
            </div>
            <div>
              <div className="text-[11px] uppercase tracking-wide text-ink-400 mb-1">{t('boards.assignee')}</div>
              <AssigneePicker
                value={card.assigneeId}
                onChange={(id) => void setAssignee(id)}
                meId={meId ?? null}
              />
            </div>
          </section>

          <section>
            <div className="text-[11px] uppercase tracking-wide text-ink-400 mb-1">{t('boards.description')}</div>
            <MentionInput
              value={description}
              onChange={setDescription}
              onSubmit={() => void saveDescription()}
              placeholder={t('boards.descPlaceholder')}
              multiline
              rows={4}
            />
            {hasLinkedReference(description) && (
              <div className="mt-2 text-sm text-ink-700 whitespace-pre-wrap">
                <MentionedText text={description} byId={byId} />
              </div>
            )}
            <div className="mt-1 flex justify-end">
              <button
                onClick={() => void saveDescription()}
                className="text-xs text-ink-500 hover:text-skype-deep px-2 py-1"
              >{t('boards.saveDescription')}</button>
            </div>
          </section>

          {(card.deliveries?.length ?? 0) > 0 && (
            <section>
              <div className="text-[11px] uppercase tracking-wide text-ink-400 mb-1.5">
                {t('boards.delivery')}<span className="text-ink-500">· {card.deliveries!.length}</span>
              </div>
              <ul className="space-y-1.5">
                {card.deliveries!.map((d) => (
                  <li key={d.id} className="flex flex-wrap items-center gap-2 rounded-[10px] border border-ink-100 bg-white/70 px-3 py-2">
                    <code className="min-w-0 truncate text-[12.5px] font-semibold text-ink-800">{d.branch}</code>
                    {d.prState && (
                      <span className={cn(
                        'shrink-0 rounded-full border px-2.5 py-1 text-[10px] font-bold uppercase tracking-wide',
                        d.prState === 'open' && 'border-sky2-200 bg-sky2-50 text-skype-deep',
                        d.prState === 'merged' && 'border-emerald-200 bg-emerald-50 text-emerald-700',
                        d.prState === 'closed' && 'border-ink-200 bg-ink-50 text-ink-500',
                      )}>{d.prState}</span>
                    )}
                    {d.prUrl && (
                      <a
                        href={d.prUrl}
                        target="_blank"
                        rel="noreferrer"
                        className="shrink-0 text-xs font-semibold text-skype hover:underline"
                      >PR ↗</a>
                    )}
                    <span className="ml-auto shrink-0 text-[11px] text-ink-400">{formatTime(d.createdAt)}</span>
                  </li>
                ))}
              </ul>
            </section>
          )}

          <section>
            <div className="text-[11px] uppercase tracking-wide text-ink-400 mb-1">
              {t('boards.comments')}{comments.length > 0 && <span className="text-ink-500">· {comments.length}</span>}
            </div>
            <ul className="space-y-2">
              {comments.map((c) => {
                const author = byId[c.authorId]
                return (
                  <li key={c.id} className="flex items-start gap-2.5">
                    {author ? <AvatarMini p={author} size={26} /> : <span className="w-6 h-6 rounded-full bg-ink-200" />}
                    <div className="flex-1 min-w-0">
                      <div className="flex items-baseline gap-2">
                        <span className="text-sm font-medium text-ink-800">{author?.name ?? c.authorId}</span>
                        <span className="text-[11px] text-ink-400">{formatTime(c.createdAt)}</span>
                      </div>
                      <div className="text-sm text-ink-700 whitespace-pre-wrap">
                        <MentionedText text={c.body} byId={byId} />
                      </div>
                    </div>
                  </li>
                )
              })}
              {comments.length === 0 && (
                <li className="text-xs text-ink-400">{t('boards.noCommentsYet')}</li>
              )}
            </ul>
            <div className="mt-3">
              <MentionInput
                value={draftComment}
                onChange={setDraftComment}
                onSubmit={() => void postComment()}
                placeholder={t('boards.commentPlaceholder')}
                multiline
                rows={2}
              />
              <div className="mt-1 flex justify-end gap-2">
                <button
                  onClick={() => void postComment()}
                  disabled={!draftComment.trim() || posting}
                  className="px-3 py-1.5 text-sm rounded-md bg-skype text-white hover:bg-skype-deep disabled:opacity-40 disabled:hover:bg-skype"
                >{t('boards.postComment')}</button>
              </div>
            </div>
          </section>
        </div>

        <footer className="px-5 py-3 border-t border-ink-100 flex items-center justify-between">
          <div className="text-[11px] text-ink-400">
            {t('boards.createdByAt', { time: formatTime(card.createdAt), author: byId[card.createdBy]?.name ?? card.createdBy })}
          </div>
          <button
            onClick={async () => {
              if (!confirm(t('boards.deleteCardConfirm'))) return
              try { await deleteCard(boardId, card.id); onClose() } catch (e) { console.warn(e) }
            }}
            className="text-xs text-coral-deep hover:underline"
          >{t('boards.deleteCard')}</button>
        </footer>
      </div>
    </div>
  )
}

function AssigneePicker({ value, onChange, meId }: {
  value: string | null; onChange: (id: string | null) => void; meId: string | null
}) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const id = useId()
  const rootRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [activeIndex, setActiveIndex] = useState(0)
  const everyone = useMemo(() =>
    Object.values(byId).filter((p) => !p.departedAt)
      .sort((a, b) => {
        // Me first, then humans, then agents, then alphabetical.
        if (a.id === meId) return -1
        if (b.id === meId) return 1
        if (a.kind !== b.kind) return a.kind === 'human' ? -1 : 1
        return a.name.localeCompare(b.name)
      }),
    [byId, meId],
  )
  const selected = value ? everyone.find((p) => p.id === value) ?? null : null
  const options = useMemo(() => [
    { id: null, label: t('boards.unassigned'), meta: '', participant: null as Participant | null },
    ...everyone.map((p) => ({
      id: p.id,
      label: p.name,
      meta: p.kind === 'agent' ? t('common.agent') : (p.id === meId ? t('common.you') : t('common.human')),
      participant: p,
    })),
  ], [everyone, meId, t])
  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase()
    if (!needle) return options
    return options.filter((option) =>
      option.label.toLowerCase().includes(needle) ||
      option.meta.toLowerCase().includes(needle) ||
      (option.id ?? '').toLowerCase().includes(needle)
    )
  }, [options, query])

  useEffect(() => {
    if (!open) return
    const idx = filtered.findIndex((option) => option.id === value)
    setActiveIndex(Math.max(0, idx))
  }, [filtered, open, value])

  useEffect(() => {
    if (!open) return
    const onDown = (event: MouseEvent) => {
      const target = event.target as Node | null
      if (target && rootRef.current?.contains(target)) return
      setOpen(false)
      setQuery('')
    }
    window.addEventListener('mousedown', onDown, true)
    return () => window.removeEventListener('mousedown', onDown, true)
  }, [open])

  function openMenu() {
    setOpen(true)
    setQuery('')
    queueMicrotask(() => inputRef.current?.focus())
  }

  function commit(option: (typeof options)[number] | undefined) {
    if (!option) return
    onChange(option.id)
    setOpen(false)
    setQuery('')
    inputRef.current?.blur()
  }

  const displayValue = open ? query : (selected ? `${selected.name}${selected.kind === 'agent' ? ` · ${t('common.agent')}` : ''}` : t('boards.unassigned'))

  return (
    <div ref={rootRef} className="relative">
      <div
        className={cn(
          'group relative flex h-11 w-full items-center rounded-[14px] border border-ink-100 bg-cloud text-left text-[13px] font-semibold text-ink-900 outline-none transition',
          'shadow-[0_1px_0_rgba(255,255,255,0.92)_inset,0_10px_24px_-24px_rgba(26,78,120,0.55)]',
          'hover:border-sky2-200 hover:bg-sky2-50/60',
          open && 'border-sky2-300 bg-white ring-4 ring-sky2-100',
        )}
        style={{
          backgroundImage: 'linear-gradient(180deg, rgba(255,255,255,0.98), rgba(246,250,253,0.94))',
        }}
      >
        {!open && (
          <span className="pointer-events-none absolute left-3 top-1/2 grid h-7 w-7 -translate-y-1/2 place-items-center">
            {selected
              ? <AvatarMini p={selected} size={26} />
              : <span className="grid h-[26px] w-[26px] place-items-center rounded-full bg-ink-100 text-[12px] text-ink-400">-</span>}
          </span>
        )}
        <input
          ref={inputRef}
          id={id}
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={open}
          aria-controls={`${id}-listbox`}
          aria-activedescendant={open && filtered[activeIndex] ? `${id}-option-${activeIndex}` : undefined}
          value={displayValue}
          placeholder={t('boards.searchAssignees')}
          onFocus={openMenu}
          onMouseDown={() => {
            if (!open) openMenu()
          }}
          onChange={(event) => {
            if (!open) setOpen(true)
            setQuery(event.target.value)
          }}
          onKeyDown={(event) => {
            if (event.nativeEvent.isComposing) return
            if (event.key === 'ArrowDown') {
              event.preventDefault()
              if (!open) { openMenu(); return }
              setActiveIndex((idx) => Math.min(filtered.length - 1, idx + 1))
              return
            }
            if (event.key === 'ArrowUp') {
              event.preventDefault()
              if (!open) { openMenu(); return }
              setActiveIndex((idx) => Math.max(0, idx - 1))
              return
            }
            if (event.key === 'Enter') {
              event.preventDefault()
              commit(filtered[activeIndex])
              return
            }
            if (event.key === 'Escape') {
              event.preventDefault()
              setOpen(false)
              setQuery('')
            }
          }}
          className={cn(
            'h-full min-w-0 flex-1 rounded-[14px] bg-transparent px-3.5 pr-[76px] text-[13px] font-semibold text-ink-900 outline-none placeholder:text-ink-300',
            !open && 'pl-11',
          )}
        />
        {value && (
          <button
            type="button"
            aria-label={t('boards.clearAssignee')}
            title={t('boards.clearAssignee')}
            onMouseDown={(event) => event.preventDefault()}
            onClick={() => onChange(null)}
            className="absolute right-[45px] grid h-7 w-7 place-items-center rounded-[9px] text-ink-300 transition hover:bg-sky2-50 hover:text-ink-600"
          >
            <span aria-hidden="true" className="text-base leading-none">×</span>
          </button>
        )}
        <button
          type="button"
          aria-label={t('boards.openAssigneeMenu')}
          onMouseDown={(event) => event.preventDefault()}
          onClick={() => openMenu()}
          className={cn(
            'absolute right-2 grid h-7 w-7 place-items-center rounded-[9px] border border-sky2-100 bg-sky2-50 text-skype-deep transition',
            'group-hover:bg-white group-focus-within:bg-sky2-50',
            open && 'border-sky2-200 bg-sky2-50',
          )}
        >
          <svg viewBox="0 0 14 14" className={cn('h-3.5 w-3.5 transition-transform', open && 'rotate-180')} fill="none">
            <path d="M3.5 5.5 7 9l3.5-3.5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
        </button>
      </div>
      {open && (
        <div
          id={`${id}-listbox`}
          role="listbox"
          className="absolute left-0 right-0 top-full z-[70] mt-2 max-h-72 overflow-auto rounded-[16px] border border-sky2-100 bg-cloud p-2.5 shadow-[0_22px_55px_-24px_rgba(10,30,60,0.38),0_8px_18px_-12px_rgba(10,30,60,0.2),0_0_0_1px_rgba(255,255,255,0.72)_inset] animate-rise"
        >
          {filtered.map((option, idx) => {
            const active = idx === activeIndex
            const selectedOption = option.id === value
            return (
              <button
                key={option.id ?? 'unassigned'}
                id={`${id}-option-${idx}`}
                type="button"
                role="option"
                aria-selected={selectedOption}
                onMouseDown={(event) => event.preventDefault()}
                onMouseEnter={() => setActiveIndex(idx)}
                onClick={() => commit(option)}
                className={cn(
                  'flex h-9 w-full items-center gap-2.5 rounded-[10px] px-3 text-left text-[12.5px] font-semibold transition',
                  selectedOption
                    ? 'bg-skype text-white shadow-[0_10px_22px_-16px_rgba(0,120,200,0.82)]'
                    : active
                      ? 'bg-sky2-50 text-skype-deep'
                      : 'text-ink-700 hover:bg-sky2-50 hover:text-skype-deep',
                )}
              >
                {option.participant
                  ? <AvatarMini p={option.participant} size={22} />
                  : <span className="grid h-[22px] w-[22px] place-items-center rounded-full bg-ink-100 text-[11px] text-ink-400">-</span>}
                <span className="min-w-0 flex-1 truncate font-medium">{option.label}</span>
                {option.meta && (
                  <span className={cn(
                    'shrink-0 text-[10px] uppercase tracking-wide',
                    selectedOption ? 'text-white/70' : 'text-ink-300',
                  )}>{option.meta}</span>
                )}
              </button>
            )
          })}
          {filtered.length === 0 && (
            <div className="px-3 py-3 text-[12.5px] font-semibold text-ink-400">{t('boards.noMatchingTeammate')}</div>
          )}
        </div>
      )}
    </div>
  )
}

function formatTime(iso: string): string {
  try {
    const d = new Date(iso)
    return d.toLocaleString(undefined, {
      month: 'short', day: 'numeric',
      hour: '2-digit', minute: '2-digit',
    })
  } catch { return iso }
}
