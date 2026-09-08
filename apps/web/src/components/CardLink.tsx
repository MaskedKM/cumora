import { useEffect, useRef } from 'react'
import { useApp } from '@/stores/app'
import { useBoards } from '@/stores/boards'
import { useResolvedCardId } from '@/lib/useArtifactId'
import { useIsMobile } from '@/lib/utils'
import { IBoard } from './icons'
import type { BoardCardLookup } from '@/types'

export function CardLink({ id: rawId }: { id: string }) {
  // Resolve a git-style short id to the full card id (best-effort: cards are
  // only loaded per opened board, so an unopened board's card stays short).
  const id = useResolvedCardId(rawId)
  const setView = useApp((s) => s.setView)
  const view = useApp((s) => s.view)
  const openBoardPeek = useApp((s) => s.openBoardPeek)
  const selectBoard = useBoards((s) => s.selectBoard)
  const loadCard = useBoards((s) => s.loadCard)
  const loadingCardId = useBoards((s) => s.loadingCardId)
  const lookup = useBoards((s) => s.cardLookups[id])
  const isMobile = useIsMobile()
  const didRequestCard = useRef(false)

  useEffect(() => {
    if (!lookup && loadingCardId !== id && !didRequestCard.current) {
      didRequestCard.current = true
      void loadCard(id).catch(() => { /* stale or missing card reference */ })
    }
  }, [id, loadCard, loadingCardId, lookup])

  const label = lookup?.card.title.trim() || id

  const open = (resolved: BoardCardLookup) => {
    selectBoard(resolved.board.id)
    // #369 刀2(ADR 0009):桌面 = 全屏看板(板已选中);移动端 peek 至刀3。
    if (view === 'conversations' && isMobile) openBoardPeek(resolved.board.id, id)
    else setView('boards')
  }

  return (
    <a
      href={`#cards/${id}`}
      onClick={(e) => {
        e.preventDefault()
        e.stopPropagation()
        if (lookup) {
          open(lookup)
          return
        }
        void loadCard(id)
          .then(open)
          .catch(() => {
            // #373 余量收口:死卡不再无响应 —— 桌面落到看板全屏(用户能
            // 看到卡确已不存在);移动端维持原静默(无看板全屏面,历史行为)。
            if (!isMobile && view === 'conversations') setView('boards')
          })
      }}
      className="inline-flex max-w-[260px] items-center gap-1.5 rounded-full border border-sky2-100 bg-sky2-50 px-2 py-0.5 text-[13px] font-semibold text-skype-deep no-underline transition hover:border-sky2-200 hover:bg-[#EAF7FD]"
      style={{ verticalAlign: '-0.16em' }}
      title={`Open card ${id}`}
      aria-label={`Open card ${label}`}
    >
      <IBoard className="h-3.5 w-3.5 shrink-0" />
      <span className="truncate">{label}</span>
    </a>
  )
}
