import { lazy, Suspense, useEffect } from 'react'
import { EmailComposer } from '@/components/EmailComposer'
import { useT } from '@/lib/i18n'
import { isElectron } from '@/lib/runtime'
import { useResizableWidth } from '@/lib/useResizableWidth'
import { useApp } from '@/stores/app'
import { useDevtools } from '@/stores/devtools'
import { ChatPane } from './ChatPane'
import { ConversationsPane } from './ConversationsPane'
import { Rail } from './Rail'
import { TitleBar } from './TitleBar'

// Right-slot panes (thread / info / artifact peeks) open only on user
// interaction, so they lazy-load exactly like rail views; the fallback
// keeps the slot sized (h-full) so the grid doesn't reflow when the chunk
// lands. ConversationsPane + ChatPane stay eager — see below.
const InfoPane = lazy(() => import('./InfoPane').then((m) => ({ default: m.InfoPane })))
const ThreadDrawer = lazy(() => import('./ThreadDrawer').then((m) => ({ default: m.ThreadDrawer })))
const DocumentPeekPane = lazy(() => import('./DocumentPeekPane').then((m) => ({ default: m.DocumentPeekPane })))
const BoardPeekPane = lazy(() => import('./BoardPeekPane').then((m) => ({ default: m.BoardPeekPane })))
const CalendarPeekPane = lazy(() => import('./CalendarPeekPane').then((m) => ({ default: m.CalendarPeekPane })))

// Rail-switchable views load as their own chunks (#218). Conversations is
// the landing view and stays eager — ChatPane + ConversationsPane ARE the
// first paint, so deferring them would trade a real flash for nothing.
// Every other view (BoardsView, ObservabilityView, MeView…) is fetched on
// first rail switch and then cached; a one-time centered placeholder
// (ViewFallback) covers the fetch.
const WhispersView = lazy(() => import('./WhispersView').then((m) => ({ default: m.WhispersView })))
const InboxView = lazy(() => import('./InboxView').then((m) => ({ default: m.InboxView }))) // #264 人侧 Inbox
const ConveneView = lazy(() => import('./ConveneView').then((m) => ({ default: m.ConveneView })))
const AgentsView = lazy(() => import('./AgentsView').then((m) => ({ default: m.AgentsView })))
const HrView = lazy(() => import('./HrView').then((m) => ({ default: m.HrView }))) // #345 HR Agent 配置面
const BoardsView = lazy(() => import('./BoardsView').then((m) => ({ default: m.BoardsView })))
const CalendarView = lazy(() => import('./CalendarView').then((m) => ({ default: m.CalendarView })))
const DocumentsView = lazy(() => import('./DocumentsView').then((m) => ({ default: m.DocumentsView })))
const WorkspacesView = lazy(() => import('./WorkspacesView').then((m) => ({ default: m.WorkspacesView })))
const SkillsView = lazy(() => import('./SkillsView').then((m) => ({ default: m.SkillsView }))) // #261 公司 Skills 库
const ObservabilityView = lazy(() => import('./ObservabilityView').then((m) => ({ default: m.ObservabilityView })))
const MeView = lazy(() => import('./MeView').then((m) => ({ default: m.MeView })))
const ShippingView = lazy(() => import('./ShippingView').then((m) => ({ default: m.ShippingView })))

/** Centered placeholder while a rail-view chunk loads — same shape the
 *  shipping view already used, now shared by every lazy view. */
function ViewFallback() {
  const t = useT()
  return <div className="h-full grid place-items-center text-sm text-ink-400">{t('common.loading')}</div>
}

function ConversationsLayout() {
  const infoOpen = useApp((s) => s.infoAgentId !== null)
  const threadOpen = useApp((s) => s.openThread !== null)
  const documentOpen = useApp((s) => s.openDocumentId !== null)
  const boardOpen = useApp((s) => s.openBoardId !== null)
  const calendarOpen = useApp((s) => s.openCalendarEventId !== null)
  // Thread + info + artifact peeks compete for the same right slot. Opening one closes the
  // other implicitly via the store action (see openThreadView /
  // openAgentInfo / artifact peek actions). Render thread if both somehow
  // ended up set, since the thread is the more action-oriented pane.
  const artifactOpen = documentOpen || boardOpen || calendarOpen
  const rightOpen = threadOpen || infoOpen || artifactOpen
  const rightColumn = documentOpen || boardOpen ? 'clamp(420px, 42vw, 640px)' : '420px'
  const { width, onResizeStart } = useResizableWidth('sidebar:conversations', 320, { min: 240, max: 520 })
  return (
    <div
      className="grid h-full overflow-hidden"
      style={{ gridTemplateColumns: rightOpen ? `${width}px minmax(0, 1fr) ${rightColumn}` : `${width}px minmax(0, 1fr)` }}
    >
      <ConversationsPane onResizeStart={onResizeStart} />
      <ChatPane />
      {threadOpen
        ? <Suspense fallback={<ViewFallback />}><ThreadDrawer /></Suspense>
        : documentOpen
          ? <Suspense fallback={<ViewFallback />}><DocumentPeekPane /></Suspense>
          : boardOpen
            ? <Suspense fallback={<ViewFallback />}><BoardPeekPane /></Suspense>
            : calendarOpen
              ? <Suspense fallback={<ViewFallback />}><CalendarPeekPane /></Suspense>
              : infoOpen
                ? <Suspense fallback={<ViewFallback />}><InfoPane /></Suspense>
                : null}
    </div>
  )
}

export function DesktopApp() {
  const view = useApp((s) => s.view)
  const setView = useApp((s) => s.setView)
  const devtoolsEnabled = useDevtools((s) => s.enabled)
  const devtoolsLoaded = useDevtools((s) => s.loaded)
  const loadDevtools = useDevtools((s) => s.load)

  useEffect(() => {
    void loadDevtools()
  }, [loadDevtools])

  useEffect(() => {
    if (view === 'observability' && devtoolsLoaded && !devtoolsEnabled) setView('conversations')
  }, [devtoolsEnabled, devtoolsLoaded, setView, view])

  // In Electron, fill the full window. In browser, render as a "windowed app" card.
  const wrap = isElectron
    ? {
        width: '100vw',
        height: '100vh',
        margin: 0,
        borderRadius: 0,
        boxShadow: 'none',
      }
    : {
        width: 'min(1480px, calc(100vw - 48px))',
        height: 'calc(100vh - 48px)',
        boxShadow: '0 50px 100px -20px rgba(10, 30, 60, 0.25), 0 30px 60px -30px rgba(10, 30, 60, 0.3), 0 0 0 1px rgba(0, 80, 140, 0.06)',
      }

  return (
    <div
      className={isElectron ? 'relative z-10 bg-cloud overflow-hidden grid grid-rows-[44px_1fr]' : 'relative z-10 mx-auto my-6 bg-cloud rounded-[18px] overflow-hidden grid grid-rows-[44px_1fr] backdrop-blur'}
      style={wrap}
    >
      <TitleBar />
      <div className="grid h-full min-h-0 overflow-hidden" style={{ gridTemplateColumns: '72px minmax(0, 1fr)' }}>
        <Rail />
        {view === 'conversations' && <ConversationsLayout />}
        {view === 'inbox' && <Suspense fallback={<ViewFallback />}><InboxView /></Suspense>}
        {view === 'whispers' && <Suspense fallback={<ViewFallback />}><WhispersView /></Suspense>}
        {view === 'convene' && <Suspense fallback={<ViewFallback />}><ConveneView /></Suspense>}
        {view === 'agents' && <Suspense fallback={<ViewFallback />}><AgentsView /></Suspense>}
        {view === 'hr' && <Suspense fallback={<ViewFallback />}><HrView /></Suspense>}
        {view === 'boards' && <Suspense fallback={<ViewFallback />}><BoardsView /></Suspense>}
        {view === 'calendar' && <Suspense fallback={<ViewFallback />}><CalendarView /></Suspense>}
        {view === 'documents' && <Suspense fallback={<ViewFallback />}><DocumentsView /></Suspense>}
        {view === 'workspaces' && <Suspense fallback={<ViewFallback />}><WorkspacesView /></Suspense>}
        {view === 'skills' && <Suspense fallback={<ViewFallback />}><SkillsView /></Suspense>}
        {view === 'shipping' && <Suspense fallback={<ViewFallback />}><ShippingView /></Suspense>}
        {view === 'observability' && devtoolsEnabled && <Suspense fallback={<ViewFallback />}><ObservabilityView /></Suspense>}
        {view === 'me' && <Suspense fallback={<ViewFallback />}><MeView /></Suspense>}
      </div>
      {/* Email composer drawer — globally rendered so opening it works
          from any view (sidebar Compose CTA, EmailCard reply button). */}
      <EmailComposer />
    </div>
  )
}
