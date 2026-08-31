// 看板侧栏件(#219 ③)—— 从 BoardsView.tsx 原样搬移:
// BoardsSidebar(板列表+新建板输入)与 EmptyBoardsState(空态/未选态)。
import { useState } from 'react'
import { IBoard, IPlus } from '@/components/icons'
import { ResizeHandle } from '@/components/ResizeHandle'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useBoards } from '@/stores/boards'

export function BoardsSidebar({ onResizeStart }: { onResizeStart: (e: React.MouseEvent) => void }) {
  const t = useT()
  const list = useBoards((s) => s.list)
  const selectedId = useBoards((s) => s.selectedId)
  const selectBoard = useBoards((s) => s.selectBoard)
  const createBoard = useBoards((s) => s.createBoard)
  const [creating, setCreating] = useState(false)
  const [draft, setDraft] = useState('')

  async function submit() {
    const title = draft.trim()
    if (!title) { setCreating(false); return }
    setCreating(false)
    setDraft('')
    try {
      await createBoard(title)
    } catch (e) {
      console.warn('[boards] create failed', e)
    }
  }

  return (
    <aside className="h-full overflow-y-auto border-r border-ink-100 bg-cloud/40 relative">
      <ResizeHandle onMouseDown={onResizeStart} />
      <div className="px-4 py-4 flex items-center justify-between">
        <h2 className="text-lg font-semibold text-ink-900">{t('boards.title')}</h2>
        <button
          onClick={() => setCreating(true)}
          className="w-7 h-7 rounded-md grid place-items-center text-ink-500 hover:bg-ink-50 hover:text-skype-deep"
          title={t('boards.newBoard')}
          aria-label={t('boards.newBoard')}
        >
          <IPlus className="w-4 h-4" />
        </button>
      </div>
      {creating && (
        <div className="px-4 pb-2">
          <input
            autoFocus
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submit()
              if (e.key === 'Escape') { setCreating(false); setDraft('') }
            }}
            onBlur={() => void submit()}
            placeholder={t('boards.boardTitlePlaceholder')}
            className="w-full px-2.5 py-1.5 text-sm rounded-md border border-ink-200 bg-white focus:outline-none focus:border-skype"
          />
        </div>
      )}
      <ul className="pb-4">
        {list.map((b) => {
          const active = b.id === selectedId
          return (
            <li key={b.id}>
              <button
                onClick={() => selectBoard(b.id)}
                className={cn(
                  'w-full text-left px-4 py-2.5 flex items-center gap-2.5 transition-colors',
                  active ? 'bg-skype/10 text-skype-deep' : 'text-ink-700 hover:bg-ink-50',
                )}
              >
                <IBoard className="w-4 h-4 flex-shrink-0" />
                <span className="text-sm truncate">{b.title}</span>
              </button>
            </li>
          )
        })}
        {list.length === 0 && !creating && (
          <li className="px-4 py-3 text-xs text-ink-400">
            {t('boards.emptyHint')}
          </li>
        )}
      </ul>
    </aside>
  )
}

export function EmptyBoardsState({ empty }: { empty: boolean }) {
  const t = useT()
  return (
    <div className="h-full grid place-items-center text-ink-400">
      <div className="text-center">
        <IBoard className="w-12 h-12 mx-auto mb-3 opacity-50" />
        <p className="text-sm">
          {empty ? t('boards.emptyCta') : t('boards.pickOne')}
        </p>
      </div>
    </div>
  )
}
