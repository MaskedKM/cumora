// 子组件分桶(#219 ③):本文件保留 BoardsView 壳(板列表取数、首板自动选中、
// 侧栏宽度装配与画布/空态编排),原局部子组件按职责分居 ./boards/:
//   sidebar(BoardsSidebar 板列表+新建 · EmptyBoardsState 空态)
//   · canvas(BoardCanvas 板头/列装配/加列/卡片弹窗编排)
//   · column(ColumnView 列容器+拖放落点 · CardTile 卡片砖)
//   · cardModal(CardDetailModal 卡片详情 · AssigneePicker 经办人下拉 · formatTime)
//   · mentions(mention/引用正则与边界判定、MentionedText、MentionInput)。
// 看板数据层仍是 @/stores/boards,本刀未动。
import { useEffect } from 'react'
import { useResizableWidth } from '@/lib/useResizableWidth'
import { useBoards } from '@/stores/boards'
import { BoardCanvas } from './boards/canvas'
import { BoardsSidebar, EmptyBoardsState } from './boards/sidebar'

/**
 * Boards view — Kanban for both humans and agents.
 *
 * The same boards/cards an agent manipulates via `cumora board ...` show
 * up here. Card titles, descriptions, and comments accept `@<id>` tokens;
 * mentions get chipped inline and broadcast on the boards channel so the
 * recipient (human or agent) is reachable from anywhere.
 */
export function BoardsView() {
  const list = useBoards((s) => s.list)
  const loadingList = useBoards((s) => s.loadingList)
  const selectedId = useBoards((s) => s.selectedId)
  const loadList = useBoards((s) => s.loadList)
  const selectBoard = useBoards((s) => s.selectBoard)

  useEffect(() => { void loadList() }, [loadList])

  // Auto-pick the first board when one becomes available — matches the
  // conversations pane's behaviour and avoids a perpetual "Pick a board"
  // empty state for users who only have one.
  useEffect(() => {
    if (!selectedId && list.length > 0) selectBoard(list[0].id)
  }, [list, selectedId, selectBoard])

  // Same shape as ConversationsLayout — `minmax(0, 1fr)` is critical:
  // a plain `1fr` track expands to fit its widest child, which would
  // make the canvas's horizontal overflow never trigger.
  const { width, onResizeStart } = useResizableWidth('sidebar:boards', 280, { min: 220, max: 480 })

  return (
    <div
      className="h-full grid"
      style={{ gridTemplateColumns: `${width}px minmax(0, 1fr)` }}
    >
      <BoardsSidebar onResizeStart={onResizeStart} />
      {selectedId
        ? <BoardCanvas boardId={selectedId} />
        : <EmptyBoardsState empty={!loadingList && list.length === 0} />}
    </div>
  )
}
