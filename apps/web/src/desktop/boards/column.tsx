// 看板列/卡片件(#219 ③)—— 从 BoardsView.tsx 原样搬移:
// ColumnView(列容器:改名/删除/拖放落点/加卡)与 CardTile(卡片砖)。
import { useState } from 'react'
import { AvatarMini } from '@/components/Avatar'
import { IAt, IMore } from '@/components/icons'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useBoards } from '@/stores/boards'
import { useParticipants } from '@/stores/participants'
import type { BoardCard, BoardColumn } from '@/types'
import { MentionedText, MentionInput } from './mentions'

export function ColumnView({ boardId, column, cards, onOpenCard }: {
  boardId: string; column: BoardColumn; cards: BoardCard[]
  onOpenCard: (id: string) => void
}) {
  const t = useT()
  const addCard = useBoards((s) => s.addCard)
  const moveCardOptimistic = useBoards((s) => s.moveCardOptimistic)
  const renameColumn = useBoards((s) => s.renameColumn)
  const deleteColumn = useBoards((s) => s.deleteColumn)
  const [adding, setAdding] = useState(false)
  const [draft, setDraft] = useState('')
  const [editingTitle, setEditingTitle] = useState(false)
  const [titleDraft, setTitleDraft] = useState('')
  const [dragOver, setDragOver] = useState(false)

  async function submit() {
    const title = draft.trim()
    setAdding(false)
    setDraft('')
    if (!title) return
    try {
      await addCard(boardId, { columnId: column.id, title })
    } catch (e) { console.warn('[boards] add card failed', e) }
  }

  async function submitTitle() {
    const t = titleDraft.trim()
    setEditingTitle(false)
    if (!t || t === column.title) return
    try { await renameColumn(boardId, column.id, t) } catch (e) { console.warn('[boards] rename col failed', e) }
  }

  return (
    <div
      className={cn(
        'w-72 flex-shrink-0 h-full flex flex-col rounded-lg bg-cloud/60 transition-colors',
        dragOver && 'ring-2 ring-skype/40 bg-skype/5',
      )}
      onDragOver={(e) => { e.preventDefault(); setDragOver(true) }}
      onDragLeave={() => setDragOver(false)}
      onDrop={(e) => {
        e.preventDefault()
        setDragOver(false)
        const cardId = e.dataTransfer.getData('text/cumora-card')
        if (!cardId) return
        // Drop at the end of this column. Per-card drop targets would be
        // nicer for fine-grained ordering, but end-of-column covers the
        // common "move to Done" gesture.
        void moveCardOptimistic(boardId, cardId, column.id, cards.length)
      }}
    >
      <div className="px-3 pt-3 pb-2 flex items-center justify-between gap-2">
        {editingTitle ? (
          <input
            autoFocus
            value={titleDraft}
            onChange={(e) => setTitleDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submitTitle()
              if (e.key === 'Escape') setEditingTitle(false)
            }}
            onBlur={() => void submitTitle()}
            className="flex-1 px-1.5 py-0.5 text-sm font-medium rounded-md border border-skype/50 bg-white focus:outline-none"
          />
        ) : (
          <button
            onClick={() => { setTitleDraft(column.title); setEditingTitle(true) }}
            className="text-sm font-medium text-ink-700 hover:text-skype-deep flex-1 text-left truncate"
          >
            {column.title}
          </button>
        )}
        <span className="text-xs text-ink-400">{cards.length}</span>
        <button
          onClick={async () => {
            if (cards.length > 0 && !confirm(t('boards.deleteColumnConfirm', { title: column.title, count: cards.length }))) return
            try { await deleteColumn(boardId, column.id) } catch (e) { console.warn('[boards] delete col failed', e) }
          }}
          className="w-5 h-5 rounded grid place-items-center text-ink-300 hover:text-coral-deep"
          title={t('boards.deleteColumn')}
          aria-label={t('boards.deleteColumn')}
        >
          <IMore className="w-3.5 h-3.5" />
        </button>
      </div>
      <div className="flex-1 overflow-y-auto px-2 pb-2 space-y-2">
        {cards.map((c) => (
          <CardTile key={c.id} card={c} onOpen={() => onOpenCard(c.id)} />
        ))}
        {adding ? (
          <MentionInput
            autoFocus
            value={draft}
            onChange={setDraft}
            onSubmit={() => void submit()}
            onEscape={() => { setAdding(false); setDraft('') }}
            onBlur={() => void submit()}
            placeholder={t('boards.cardTitlePlaceholder')}
            multiline
            submitOnEnter
            rows={2}
            className="w-full px-2.5 py-2 text-sm rounded-md border border-ink-200 bg-white focus:outline-none focus:border-skype resize-none"
          />
        ) : (
          <button
            onClick={() => setAdding(true)}
            className="w-full text-left text-xs text-ink-400 px-2.5 py-1.5 rounded-md hover:bg-white hover:text-ink-600 transition-colors"
          >
            {t('boards.addCard')}
          </button>
        )}
      </div>
    </div>
  )
}

function CardTile({ card, onOpen }: { card: BoardCard; onOpen: () => void }) {
  const byId = useParticipants((s) => s.byId)
  const assignee = card.assigneeId ? byId[card.assigneeId] : null
  return (
    <article
      role="button"
      tabIndex={0}
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData('text/cumora-card', card.id)
        e.dataTransfer.effectAllowed = 'move'
      }}
      onClick={onOpen}
      onKeyDown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onOpen() } }}
      className="px-3 py-2.5 rounded-md bg-white border border-ink-100 shadow-soft text-left cursor-pointer hover:border-skype/40 transition-colors"
    >
      <div className="text-sm text-ink-800 leading-snug">
        <MentionedText text={card.title} byId={byId} />
      </div>
      {(card.assigneeId || card.mentions.length > 0 || card.commentCount > 0) && (
        <div className="mt-2 flex items-center gap-2">
          {assignee && (
            <span className="flex items-center gap-1 text-[11px] text-ink-500">
              <AvatarMini p={assignee} size={18} />
              <span>{assignee.name}</span>
            </span>
          )}
          {card.mentions.length > 0 && !card.assigneeId && (
            <span className="flex items-center gap-0.5 text-[11px] text-ink-400">
              <IAt className="w-3 h-3" />
              {card.mentions.slice(0, 3).map((m) => '@' + (byId[m]?.name ?? m)).join(' ')}
            </span>
          )}
          {card.commentCount > 0 && (
            <span className="ml-auto text-[11px] text-ink-400">{card.commentCount} 💬</span>
          )}
        </div>
      )}
    </article>
  )
}
