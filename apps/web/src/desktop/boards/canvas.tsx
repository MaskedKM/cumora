// 看板画布件(#219 ③)—— 从 BoardsView.tsx 原样搬移:
// BoardCanvas(板头改名/删除、列装配 cardsByColumn、加列、卡片详情弹窗编排)。
import { useMemo, useState } from 'react'
import { ITrash } from '@/components/icons'
import { useT } from '@/lib/i18n'
import { useBoards } from '@/stores/boards'
import { useParticipants } from '@/stores/participants'
import type { BoardCard } from '@/types'
import { CardDetailModal } from './cardModal'
import { ColumnView } from './column'
import { MentionedText } from './mentions'

export function BoardCanvas({ boardId }: { boardId: string }) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const snap = useBoards((s) => s.snapshots[boardId])
  const loadingBoardId = useBoards((s) => s.loadingBoardId)
  const addColumn = useBoards((s) => s.addColumn)
  const deleteBoard = useBoards((s) => s.deleteBoard)
  const renameBoard = useBoards((s) => s.renameBoard)
  const [addingCol, setAddingCol] = useState(false)
  const [colDraft, setColDraft] = useState('')
  const [openCardId, setOpenCardId] = useState<string | null>(null)
  const [editingTitle, setEditingTitle] = useState(false)
  const [titleDraft, setTitleDraft] = useState('')

  // Hooks must run on every render in the same order — keep useMemo
  // above any conditional early return. When the snapshot hasn't
  // hydrated yet we just memoize over an empty board.
  const cardsByColumn = useMemo(() => {
    const m = new Map<string, BoardCard[]>()
    if (!snap) return m
    for (const col of snap.columns) m.set(col.id, [])
    for (const c of snap.cards) {
      const arr = m.get(c.columnId)
      if (arr) arr.push(c)
    }
    for (const [k, arr] of m) {
      arr.sort((a, b) => a.position - b.position)
      m.set(k, arr)
    }
    return m
  }, [snap])

  if (!snap) {
    return (
      <div className="h-full grid place-items-center text-ink-400 text-sm">
        {loadingBoardId === boardId ? t('common.loading') : t('common.noData')}
      </div>
    )
  }

  const openCard = openCardId ? snap.cards.find((c) => c.id === openCardId) ?? null : null

  async function submitNewColumn() {
    const t = colDraft.trim()
    setAddingCol(false)
    setColDraft('')
    if (!t) return
    try { await addColumn(boardId, t) } catch (e) { console.warn('[boards] add column failed', e) }
  }

  async function submitTitle() {
    const t = titleDraft.trim()
    setEditingTitle(false)
    if (!t || t === snap.title) return
    try { await renameBoard(boardId, t) } catch (e) { console.warn('[boards] rename failed', e) }
  }

  return (
    <div className="h-full flex flex-col min-h-0">
      <header className="flex items-center justify-between px-6 py-4 border-b border-ink-100">
        <div className="min-w-0 flex-1">
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
              className="text-2xl font-semibold text-ink-900 bg-transparent border-b border-skype outline-none"
            />
          ) : (
            <button
              onClick={() => { setTitleDraft(snap.title); setEditingTitle(true) }}
              className="text-2xl font-semibold text-ink-900 hover:text-skype-deep text-left truncate"
            >
              {snap.title}
            </button>
          )}
          {snap.description && (
            <p className="text-sm text-ink-500 mt-1 truncate">
              <MentionedText text={snap.description} byId={byId} />
            </p>
          )}
        </div>
        <button
          onClick={async () => {
            if (!confirm(t('boards.deleteBoardConfirm', { title: snap.title }))) return
            try { await deleteBoard(boardId) } catch (e) { console.warn('[boards] delete failed', e) }
          }}
          className="w-8 h-8 rounded-md grid place-items-center text-ink-400 hover:bg-coral-50 hover:text-coral-deep"
          title={t('boards.deleteBoard')}
          aria-label={t('boards.deleteBoard')}
        >
          <ITrash className="w-4 h-4" />
        </button>
      </header>

      <div className="flex-1 overflow-x-auto overflow-y-hidden min-h-0">
        <div className="h-full flex items-start gap-4 px-6 py-4">
          {snap.columns.map((col) => (
            <ColumnView
              key={col.id}
              boardId={boardId}
              column={col}
              cards={cardsByColumn.get(col.id) ?? []}
              onOpenCard={setOpenCardId}
            />
          ))}
          {addingCol ? (
            <div className="w-72 flex-shrink-0 p-3 rounded-lg bg-cloud/60">
              <input
                autoFocus
                value={colDraft}
                onChange={(e) => setColDraft(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void submitNewColumn()
                  if (e.key === 'Escape') { setAddingCol(false); setColDraft('') }
                }}
                onBlur={() => void submitNewColumn()}
                placeholder={t('boards.columnTitlePlaceholder')}
                className="w-full px-2.5 py-1.5 text-sm rounded-md border border-ink-200 bg-white focus:outline-none focus:border-skype"
              />
            </div>
          ) : (
            <button
              onClick={() => setAddingCol(true)}
              className="w-72 flex-shrink-0 px-3 py-2.5 rounded-lg text-sm text-ink-500 border border-dashed border-ink-200 hover:bg-cloud/40 hover:text-ink-700 transition-colors text-left"
            >
              {t('boards.addColumn')}
            </button>
          )}
        </div>
      </div>

      {openCard && (
        <CardDetailModal
          boardId={boardId}
          card={openCard}
          columns={snap.columns}
          onClose={() => setOpenCardId(null)}
        />
      )}
    </div>
  )
}
