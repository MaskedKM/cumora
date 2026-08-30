import { useEffect, useRef, useState } from 'react'
import { useT } from '@/lib/i18n'
import { useResolvedBoardId, useResolvedCalendarId, useResolvedCardId, useResolvedDocumentId } from '@/lib/useArtifactId'
import { parseBlocks, parseBody } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useBoards } from '@/stores/boards'
import { useCalendar } from '@/stores/calendar'
import { useDocuments } from '@/stores/documents'
import { useParticipants } from '@/stores/participants'
import type { Message } from '@/types'
import { ImageViewer } from '../ImageViewer'
import { IBoard, ICalendar, IFigma, IFile } from '../icons'




type ArtifactRef = { type: 'document' | 'board' | 'card' | 'calendar'; id: string }


export function artifactKey(ref: ArtifactRef): string {
  return `${ref.type}:${ref.id}`
}


function addArtifactRef(out: Map<string, ArtifactRef>, ref: ArtifactRef) {
  out.set(artifactKey(ref), ref)
}


function artifactRefsFromBody(body: string): ArtifactRef[] {
  const out = new Map<string, ArtifactRef>()
  for (const block of parseBlocks(body)) {
    if (block.kind !== 'prose') continue
    for (const token of parseBody(block.text)) {
      if (token.kind === 'document' || token.kind === 'board' || token.kind === 'card' || token.kind === 'calendar') {
        addArtifactRef(out, { type: token.kind, id: token.id })
      }
    }
  }
  return Array.from(out.values())
}


function artifactRefsFromPlainText(text: string): ArtifactRef[] {
  const out = new Map<string, ArtifactRef>()
  const re = /\b(doc_[A-Za-z0-9]+|board-[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*|card-[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*|ce-[A-Za-z0-9-]+)\b/g
  let m: RegExpExecArray | null
  while ((m = re.exec(text)) !== null) {
    const id = m[0]
    if (id.startsWith('doc_')) addArtifactRef(out, { type: 'document', id })
    else if (id.startsWith('board-')) addArtifactRef(out, { type: 'board', id })
    else if (id.startsWith('card-')) addArtifactRef(out, { type: 'card', id })
    else if (id.startsWith('ce-')) addArtifactRef(out, { type: 'calendar', id })
  }
  return Array.from(out.values())
}

export function artifactRefsForMessage(msg: Message): ArtifactRef[] {
  const out = new Map<string, ArtifactRef>()
  for (const ref of artifactRefsFromBody(msg.body)) addArtifactRef(out, ref)
  if (msg.tool) {
    for (const ref of artifactRefsFromPlainText(`${msg.tool.arg}\n${msg.tool.detail}`)) addArtifactRef(out, ref)
  }
  return Array.from(out.values())
}

function timeAgo(iso: string): string {
  const then = new Date(iso).getTime()
  const ms = Date.now() - then
  if (!Number.isFinite(then)) return 'recently'
  if (ms < 60_000) return 'just now'
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`
  return new Date(iso).toLocaleDateString()
}

export function DocumentArtifactCard({ id: rawId, conversationId }: { id: string; conversationId: string }) {
  const t = useT()
  const id = useResolvedDocumentId(rawId) // git-style short-id → full id
  const loaded = useDocuments((s) => s.loaded)
  const loadDocuments = useDocuments((s) => s.load)
  const selectDocument = useDocuments((s) => s.select)
  const doc = useDocuments((s) => s.list.find((d) => d.id === id) ?? null)
  const byId = useParticipants((s) => s.byId)
  const openDocumentPeek = useApp((s) => s.openDocumentPeek)

  useEffect(() => {
    if (!loaded) void loadDocuments()
  }, [loadDocuments, loaded])

  const title = doc?.title?.trim() || (loaded ? t('docs.unavailable') : t('docs.opening'))
  const author = doc ? byId[doc.createdBy]?.name ?? doc.createdBy : null
  const updated = doc ? timeAgo(doc.updatedAt) : null
  const isPinnedHere = doc?.conversationId === conversationId

  const open = () => {
    selectDocument(id)
    openDocumentPeek(id)
  }

  return (
    <button
      type="button"
      onClick={open}
      className="mt-2 group block w-full max-w-[min(100%,580px)] text-left rounded-[12px] border border-ink-100 bg-cloud overflow-hidden transition hover:border-sky2-200 hover:shadow-[0_16px_34px_-22px_rgba(0,80,140,0.42)] focus-visible:outline focus-visible:outline-2 focus-visible:outline-sky2-300"
      aria-label={`Open document ${title}`}
    >
      <div className="grid grid-cols-[52px_minmax(0,1fr)_auto] gap-3 px-3 py-3 items-center">
        <div
          className="relative w-[52px] h-[64px] rounded-[9px] bg-white border border-sky2-100 shadow-[0_10px_24px_-20px_rgba(0,80,140,0.35)] overflow-hidden"
          aria-hidden
        >
          <div className="h-2 bg-gradient-to-r from-skype via-sky2-200 to-coral-soft" />
          <div className="px-2.5 py-2 space-y-1.5">
            <span className="block h-1.5 rounded-full bg-ink-100 w-7" />
            <span className="block h-1 rounded-full bg-sky2-100 w-8" />
            <span className="block h-1 rounded-full bg-sky2-100 w-5" />
            <span className="block h-1 rounded-full bg-coral-soft/70 w-7" />
          </div>
          <div className="absolute bottom-1.5 right-1.5 w-5 h-5 rounded-md grid place-items-center bg-skype text-white shadow-sm">
            <IFile className="w-3 h-3" strokeWidth={1.8} />
          </div>
        </div>

        <div className="min-w-0">
          <div className="flex items-center gap-2 min-w-0">
            <span className="text-[10px] font-bold uppercase tracking-[0.14em] text-skype-deep">{t('msgview.cardDocument')}</span>
            <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
            <span className="text-[10.5px] text-ink-400 truncate">{id}</span>
          </div>
          <div className="mt-1 text-[14px] font-semibold text-ink-900 truncate">{title}</div>
          <div className="mt-1 flex items-center gap-1.5 min-w-0 text-[11.5px] text-ink-500">
            {author && <span className="truncate">{author}</span>}
            {author && updated && <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />}
            {updated && <span className="shrink-0">Updated {updated}</span>}
            {isPinnedHere && (
              <>
                <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
                <span className="shrink-0 text-gold-deep">{t('chat.inThisConversation')}</span>
              </>
            )}
          </div>
        </div>

        <div className="ml-1 h-8 px-3 rounded-full bg-sky2-50 text-skype-deep text-[11.5px] font-semibold inline-flex items-center gap-1.5 transition group-hover:bg-skype group-hover:text-white">
          {t('common.open')}
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="w-3.5 h-3.5">
            <path d="M9 18l6-6-6-6" />
          </svg>
        </div>
      </div>
    </button>
  )
}

export function BoardArtifactCard({ id: rawId }: { id: string }) {
  const t = useT()
  const id = useResolvedBoardId(rawId) // git-style short-id → full id
  const loadList = useBoards((s) => s.loadList)
  const loadingList = useBoards((s) => s.loadingList)
  const list = useBoards((s) => s.list)
  const loadBoard = useBoards((s) => s.loadBoard)
  const loadingBoardId = useBoards((s) => s.loadingBoardId)
  const snapshot = useBoards((s) => s.snapshots[id])
  const selectBoard = useBoards((s) => s.selectBoard)
  const openBoardPeek = useApp((s) => s.openBoardPeek)
  const summary = list.find((b) => b.id === id) ?? null
  const didRequestList = useRef(false)
  const requestedBoardId = useRef<string | null>(null)

  useEffect(() => {
    if (!summary && !loadingList && !didRequestList.current) {
      didRequestList.current = true
      void loadList().catch(() => { /* stale or missing board reference */ })
    }
  }, [loadList, loadingList, summary])

  useEffect(() => {
    if (!snapshot && loadingBoardId !== id && requestedBoardId.current !== id) {
      requestedBoardId.current = id
      void loadBoard(id).catch(() => { /* handled by unavailable card state */ })
    }
  }, [id, loadBoard, loadingBoardId, snapshot])

  const isBoardPending = !snapshot && (loadingBoardId === id || requestedBoardId.current !== id)
  const title = snapshot?.title?.trim() || summary?.title?.trim() || (isBoardPending ? t('docs.openingBoard') : t('peek.boardUnavailable'))
  const updated = snapshot?.updatedAt || summary?.updatedAt
  const columns = snapshot?.columns.length ?? null
  const cards = snapshot?.cards.length ?? null

  const open = () => {
    selectBoard(id)
    openBoardPeek(id)
  }

  return (
    <button
      type="button"
      onClick={open}
      className="mt-2 group block w-full max-w-[min(100%,580px)] text-left rounded-[12px] border border-ink-100 bg-cloud overflow-hidden transition hover:border-sky2-200 hover:shadow-[0_16px_34px_-24px_rgba(0,80,140,0.24)] focus-visible:outline focus-visible:outline-2 focus-visible:outline-sky2-200"
      aria-label={`Open board ${title}`}
    >
      <div className="grid grid-cols-[52px_minmax(0,1fr)_auto] gap-3 px-3 py-3 items-center">
        <div className="relative w-[52px] h-[64px] rounded-[9px] bg-white border border-sky2-100 shadow-[0_10px_24px_-22px_rgba(0,80,140,0.22)] overflow-hidden" aria-hidden>
          <div className="h-2 bg-gradient-to-r from-sky2-200 via-sky2-100 to-sky2-100" />
          <div className="grid grid-cols-3 gap-1 px-2 py-2 h-[46px]">
            <span className="rounded bg-sky2-100" />
            <span className="rounded bg-sky2-100" />
            <span className="rounded bg-ink-100" />
          </div>
          <div className="absolute bottom-1.5 right-1.5 w-5 h-5 rounded-md grid place-items-center bg-skype-deep text-white shadow-sm">
            <IBoard className="w-3 h-3" strokeWidth={1.8} />
          </div>
        </div>

        <div className="min-w-0">
          <div className="flex items-center gap-2 min-w-0">
            <span className="text-[10px] font-bold uppercase tracking-[0.14em] text-skype-deep">{t('msgview.cardKanban')}</span>
            <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
            <span className="text-[10.5px] text-ink-400 truncate">{id}</span>
          </div>
          <div className="mt-1 text-[14px] font-semibold text-ink-900 truncate">{title}</div>
          <div className="mt-1 flex items-center gap-1.5 min-w-0 text-[11.5px] text-ink-500">
            {columns !== null && <span>{columns} columns</span>}
            {columns !== null && cards !== null && <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />}
            {cards !== null && <span>{cards} cards</span>}
            {updated && (
              <>
                <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
                <span className="shrink-0">Updated {timeAgo(updated)}</span>
              </>
            )}
          </div>
        </div>

        <div className="ml-1 h-8 px-3 rounded-full bg-sky2-50 text-skype-deep text-[11.5px] font-semibold inline-flex items-center gap-1.5 transition group-hover:bg-skype-deep group-hover:text-white">
          {t('common.open')}
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="w-3.5 h-3.5">
            <path d="M9 18l6-6-6-6" />
          </svg>
        </div>
      </div>
    </button>
  )
}

export function CardArtifactCard({ id: rawId }: { id: string }) {
  const t = useT()
  const id = useResolvedCardId(rawId) // git-style short-id → full id (best-effort for cards)
  const lookup = useBoards((s) => s.cardLookups[id])
  const loadingCardId = useBoards((s) => s.loadingCardId)
  const loadCard = useBoards((s) => s.loadCard)
  const selectBoard = useBoards((s) => s.selectBoard)
  const openBoardPeek = useApp((s) => s.openBoardPeek)
  const byId = useParticipants((s) => s.byId)
  const [failed, setFailed] = useState(false)
  const didRequestCard = useRef(false)

  useEffect(() => {
    if (lookup || failed || loadingCardId === id || didRequestCard.current) return
    didRequestCard.current = true
    void loadCard(id).catch(() => setFailed(true))
  }, [failed, id, loadCard, loadingCardId, lookup])

  const card = lookup?.card ?? null
  const assignee = card?.assigneeId ? byId[card.assigneeId]?.name ?? card.assigneeId : null
  const title = card?.title.trim() || (failed ? t('boards.cardUnavailable') : t('boards.openingCard'))
  const updated = card?.updatedAt ? timeAgo(card.updatedAt) : null
  const location = lookup ? `${lookup.board.title} -> ${lookup.column.title}` : id

  const open = () => {
    if (lookup) {
      selectBoard(lookup.board.id)
      openBoardPeek(lookup.board.id, id)
      return
    }
    void loadCard(id)
      .then((resolved) => {
        selectBoard(resolved.board.id)
        openBoardPeek(resolved.board.id, id)
      })
      .catch(() => setFailed(true))
  }

  return (
    <button
      type="button"
      onClick={open}
      className="mt-2 group block w-full max-w-[min(100%,580px)] text-left rounded-[12px] border border-ink-100 bg-cloud overflow-hidden transition hover:border-sky2-200 hover:shadow-[0_16px_34px_-24px_rgba(0,80,140,0.24)] focus-visible:outline focus-visible:outline-2 focus-visible:outline-sky2-200"
      aria-label={`Open card ${title}`}
    >
      <div className="grid grid-cols-[52px_minmax(0,1fr)_auto] gap-3 px-3 py-3 items-center">
        <div className="relative w-[52px] h-[64px] rounded-[9px] bg-white border border-sky2-100 shadow-[0_10px_24px_-22px_rgba(0,80,140,0.22)] overflow-hidden" aria-hidden>
          <div className="h-2 bg-gradient-to-r from-sky2-200 via-sky2-100 to-cloud" />
          <div className="px-2 py-2 space-y-1.5">
            <span className="block h-1.5 rounded-full bg-ink-200 w-8" />
            <span className="block h-1 rounded-full bg-sky2-100 w-7" />
            <span className="block h-1 rounded-full bg-sky2-100 w-6" />
          </div>
          <div className="absolute bottom-1.5 right-1.5 w-5 h-5 rounded-md grid place-items-center bg-skype-deep text-white shadow-sm">
            <IBoard className="w-3 h-3" strokeWidth={1.8} />
          </div>
        </div>

        <div className="min-w-0">
          <div className="flex items-center gap-2 min-w-0">
            <span className="text-[10px] font-bold uppercase tracking-[0.14em] text-skype-deep">{t('msgview.cardKanbanCard')}</span>
            <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
            <span className="text-[10.5px] text-ink-400 truncate">{id}</span>
          </div>
          <div className="mt-1 text-[14px] font-semibold text-ink-900 truncate">{title}</div>
          <div className="mt-1 flex items-center gap-1.5 min-w-0 text-[11.5px] text-ink-500">
            <span className="truncate">{location}</span>
            {assignee && (
              <>
                <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
                <span className="truncate">{assignee}</span>
              </>
            )}
            {updated && (
              <>
                <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
                <span className="shrink-0">Updated {updated}</span>
              </>
            )}
          </div>
        </div>

        <div className="ml-1 h-8 px-3 rounded-full bg-sky2-50 text-skype-deep text-[11.5px] font-semibold inline-flex items-center gap-1.5 transition group-hover:bg-skype-deep group-hover:text-white">
          {t('common.peek')}
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="w-3.5 h-3.5">
            <path d="M9 18l6-6-6-6" />
          </svg>
        </div>
      </div>
    </button>
  )
}

export function CalendarArtifactCard({ id: rawId }: { id: string }) {
  const t = useT()
  const id = useResolvedCalendarId(rawId) // git-style short-id → full event id
  const loadingEventId = useCalendar((s) => s.loadingEventId)
  const loadEvent = useCalendar((s) => s.loadEvent)
  const event = useCalendar((s) => s.events.find((e) => e.id === id) ?? null)
  const byId = useParticipants((s) => s.byId)
  const openCalendarEventPeek = useApp((s) => s.openCalendarEventPeek)
  const [failed, setFailed] = useState(false)
  const didRequestCalendar = useRef(false)

  useEffect(() => {
    if (!event && !failed && loadingEventId !== id && !didRequestCalendar.current) {
      didRequestCalendar.current = true
      void loadEvent(id).catch(() => setFailed(true))
    }
  }, [event, failed, id, loadEvent, loadingEventId])

  const title = event?.title?.trim() || (failed ? t('peek.eventUnavailable') : t('docs.openingEvent'))
  const assignee = event?.assigneeId ? byId[event.assigneeId]?.name ?? event.assigneeId : null
  const start = event ? new Date(event.startAt) : null
  const startLabel = start && Number.isFinite(start.getTime())
    ? `${start.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })} - ${start.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`
    : null

  return (
    <button
      type="button"
      onClick={() => openCalendarEventPeek(id)}
      className="mt-2 group block w-full max-w-[min(100%,580px)] text-left rounded-[12px] border border-ink-100 bg-cloud overflow-hidden transition hover:border-sky2-200 hover:shadow-[0_16px_34px_-24px_rgba(0,168,240,0.20)] focus-visible:outline focus-visible:outline-2 focus-visible:outline-sky2-200"
      aria-label={`Open calendar event ${title}`}
    >
      <div className="grid grid-cols-[52px_minmax(0,1fr)_auto] gap-3 px-3 py-3 items-center">
        <div className="relative w-[52px] h-[64px] rounded-[9px] bg-white border border-sky2-100 shadow-[0_10px_24px_-22px_rgba(0,168,240,0.18)] overflow-hidden" aria-hidden>
          <div className="h-2 bg-gradient-to-r from-sky2-200 via-sky2-100 to-cloud" />
          <div className="px-2 py-2">
            <span className="block text-[18px] leading-none font-semibold text-skype-deep">{start?.getDate() ?? '-'}</span>
            <span className="mt-1 block h-1 rounded-full bg-sky2-100 w-8" />
            <span className="mt-1.5 block h-1 rounded-full bg-ink-100 w-6" />
          </div>
          <div className="absolute bottom-1.5 right-1.5 w-5 h-5 rounded-md grid place-items-center bg-skype text-white shadow-sm">
            <ICalendar className="w-3 h-3" strokeWidth={1.8} />
          </div>
        </div>

        <div className="min-w-0">
          <div className="flex items-center gap-2 min-w-0">
            <span className="text-[10px] font-bold uppercase tracking-[0.14em] text-skype-deep">{t('msgview.cardCalendar')}</span>
            <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
            <span className="text-[10.5px] text-ink-400 truncate">{id}</span>
          </div>
          <div className="mt-1 text-[14px] font-semibold text-ink-900 truncate">{title}</div>
          <div className="mt-1 flex items-center gap-1.5 min-w-0 text-[11.5px] text-ink-500">
            {startLabel && <span className="truncate">{startLabel}</span>}
            {event?.kind === 'agent_task' && assignee && (
              <>
                <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
                <span className="truncate">for {assignee}</span>
              </>
            )}
            {event?.status && (
              <>
                <span className="w-1 h-1 rounded-full bg-ink-200 shrink-0" />
                <span className="shrink-0 capitalize">{event.status}</span>
              </>
            )}
          </div>
        </div>

        <div className="ml-1 h-8 px-3 rounded-full bg-sky2-50 text-skype-deep text-[11.5px] font-semibold inline-flex items-center gap-1.5 transition group-hover:bg-skype group-hover:text-white">
          {t('common.open')}
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" className="w-3.5 h-3.5">
            <path d="M9 18l6-6-6-6" />
          </svg>
        </div>
      </div>
    </button>
  )
}

export function ToolCard({ msg }: { msg: Message }) {
  if (!msg.tool) return null
  const t = msg.tool
  const ico = t.icon ?? 'web'
  const icoBg: Record<string, string> = {
    web: 'linear-gradient(135deg, #4FC2A1, #2D8C72)',
    github: '#1A1A1A',
    figma: 'linear-gradient(135deg, #F24E1E, #A259FF)',
    db: 'linear-gradient(135deg, #4DB8E5, #2380B0)',
  }
  return (
    <div className="mt-2 max-w-[min(100%,580px)] bg-cloud border border-ink-100 rounded-[11px] overflow-hidden shadow-soft">
      <div className="flex items-center gap-2 px-3 py-2.5 bg-gradient-to-b from-[#F8FBFD] to-[#F2F7FB] border-b border-ink-100 text-[11.5px] font-semibold text-ink-700">
        <div
          className="w-[22px] h-[22px] rounded-md grid place-items-center text-white font-mono font-bold text-[11px]"
          style={{ background: icoBg[ico] ?? icoBg.web }}
        >{ico === 'github' ? '▲' : (ico ?? 'W')[0].toUpperCase()}</div>
        <div className="font-mono text-[11.5px] font-medium">
          {t.name} <span className="text-ink-300">· {t.arg}</span>
        </div>
        <div className="ml-auto text-[10.5px] font-bold tracking-wider px-2 py-0.5 rounded bg-[rgba(110,197,106,0.15)] text-[#3D8B3F]">
          {t.status}
        </div>
      </div>
      <div className="px-3.5 py-3 font-mono text-[11.5px] text-ink-500 leading-[1.55] whitespace-pre-line">
        {t.detail}
      </div>
    </div>
  )
}

export function AttachmentCard({ msg }: { msg: Message }) {
  const [viewerOpen, setViewerOpen] = useState(false)
  if (!msg.attachment) return null
  const a = msg.attachment

  // Real image with a URL: render inline; clicking opens the lightbox.
  if (a.kind === 'img' && a.url) {
    return (
      <>
        <button
          type="button"
          onClick={() => setViewerOpen(true)}
          className="block mt-2 max-w-[min(100%,420px)] text-left cursor-zoom-in group"
        >
          {/* The server doesn't store image dimensions today, so we
              don't know the natural aspect when this row first renders.
              Without a reserved box, the <img> renders at 0×0 → grows
              to natural size on load → shifts every message below it
              downward, which inside a virtualized list compounds into
              the visible jitter users complain about while scrolling.
              Wrap in a fixed 4:3 aspect box, contain the image inside,
              and the layout is stable from first paint. Letterboxing
              (with the bubble's bg-cloud showing through) is the price
              of stability; once the upload pipeline records width/height
              this can switch to natural-aspect again. */}
          <div
            className="rounded-[11px] border border-ink-100 bg-cloud overflow-hidden transition group-hover:brightness-95"
            style={{ aspectRatio: '4 / 3', width: '100%', maxHeight: 360 }}
          >
            <img
              src={a.url}
              alt={a.name}
              className="w-full h-full object-contain"
              loading="lazy"
              decoding="async"
              draggable={false}
            />
          </div>
          <div className="mt-1 text-[11px] text-ink-500 truncate">{a.name}{a.size ? ` · ${Math.round(a.size / 1024)}KB` : ''}</div>
        </button>
        {viewerOpen && (
          <ImageViewer src={a.url} name={a.name} onClose={() => setViewerOpen(false)} />
        )}
      </>
    )
  }

  // File card fallback (PDF / docs / archives / text / etc). Renders as a
  // real `<a download>` so clicking actually saves the file — Skype-style.
  // No URL → fall back to a non-clickable div (mock seed data, in-flight
  // optimistic rows). Short extension chip helps the eye in a stack of
  // mixed attachments.
  const ext = (() => {
    const fromMime = a.mime?.split('/')?.[1]?.toLowerCase()
    const fromName = a.name.includes('.') ? a.name.split('.').pop()?.toLowerCase() : null
    return (fromName || fromMime || a.kind).slice(0, 5)
  })()
  const sizeLabel = a.size
    ? a.size > 1024 * 1024
      ? `${(a.size / 1024 / 1024).toFixed(1)} MB`
      : `${Math.max(1, Math.round(a.size / 1024))} KB`
    : null
  const inner = (
    <>
      <div
        className="w-14 h-14 rounded-lg relative grid place-items-center overflow-hidden shrink-0"
        style={{
          background: a.kind === 'fig'
            ? 'radial-gradient(circle at 30% 30%, #FF6B9D, transparent 50%), radial-gradient(circle at 70% 70%, #4FC2F4, transparent 50%), linear-gradient(135deg, #2A2545, #1A1525)'
            : 'linear-gradient(135deg, #2A2A35, #1A1A22)',
        }}
      >
        {a.kind === 'fig' ? <IFigma className="w-5 h-5" stroke="white" strokeWidth={2} /> : <IFile className="w-5 h-5" stroke="white" strokeWidth={1.5} />}
        <span className="absolute bottom-1 right-1 font-mono text-[9px] font-bold text-white bg-black/55 px-1 rounded tracking-wider uppercase">{ext}</span>
      </div>
      <div className="min-w-0">
        <div className="text-[13px] font-semibold text-ink-900 truncate">{a.name}</div>
        <div className="text-[11px] text-ink-500 truncate">
          {a.mime ?? a.meta ?? ''}{sizeLabel ? ` · ${sizeLabel}` : ''}
        </div>
      </div>
    </>
  )

  if (!a.url) {
    return (
      <div className="mt-2 max-w-[min(100%,380px)] grid grid-cols-[56px_1fr] gap-2.5 p-2.5 bg-cloud border border-ink-100 rounded-[11px] items-center">
        {inner}
      </div>
    )
  }

  return (
    <a
      href={a.url}
      download={a.name}
      target="_blank"
      rel="noopener noreferrer"
      className="mt-2 max-w-[min(100%,380px)] grid grid-cols-[56px_1fr] gap-2.5 p-2.5 bg-cloud border border-ink-100 rounded-[11px] items-center cursor-pointer hover:shadow-soft hover:border-sky2-200 transition no-underline"
    >
      {inner}
    </a>
  )
}
