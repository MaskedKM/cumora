import { lazy, Suspense, useEffect } from 'react'
import type { ReactNode } from 'react'
import { EmailComposer } from '@/components/EmailComposer'
import { useT } from '@/lib/i18n'
import { isElectron } from '@/lib/runtime'
import { useResizableWidth } from '@/lib/useResizableWidth'
import { useApp } from '@/stores/app'
import { useDevtools } from '@/stores/devtools'
import { useWhispers } from '@/stores/whispers'
import { ChatPane } from './ChatPane'
import { ConversationsPane } from './ConversationsPane'
import { NavMenu } from './NavMenu'
import { TitleBar } from './TitleBar'

// Thread + info stay eager-lazy in the right slot — they are chat-side
// information panes, NOT secondary navigation (ADR 0009: the artifact peek
// family retired with #369; chat artifact cards now open the full views).
const InfoPane = lazy(() => import('./InfoPane').then((m) => ({ default: m.InfoPane })))
const ThreadDrawer = lazy(() => import('./ThreadDrawer').then((m) => ({ default: m.ThreadDrawer })))
// WhisperRoom —— 纯 agent 会话的聊天面(原 WhispersView 的右栏,#368 刀1 起
// 由会话列表「Agent 对话」分区直选、占用与 ChatPane 同一个中栏槽位)。
const WhisperRoom = lazy(() => import('@/components/WhisperRoom').then((m) => ({ default: m.WhisperRoom })))

// Menu-launched views load as their own chunks (#218). Conversations is
// the landing view and stays eager — ChatPane + ConversationsPane ARE the
// first paint, so deferring them would trade a real flash for nothing.
// Every other view (BoardsView, CalendarView, MeView…) is fetched on first
// launch from the NavMenu and then cached; a one-time centered placeholder
// (ViewFallback) covers the fetch.
const ConveneView = lazy(() => import('./ConveneView').then((m) => ({ default: m.ConveneView })))
const AgentsView = lazy(() => import('./AgentsView').then((m) => ({ default: m.AgentsView })))
const HrView = lazy(() => import('./HrView').then((m) => ({ default: m.HrView }))) // #345 HR Agent 配置面
const BoardsView = lazy(() => import('./BoardsView').then((m) => ({ default: m.BoardsView })))
const CalendarView = lazy(() => import('./CalendarView').then((m) => ({ default: m.CalendarView })))
const DocumentsView = lazy(() => import('./DocumentsView').then((m) => ({ default: m.DocumentsView })))
const ProjectsView = lazy(() => import('./ProjectsView').then((m) => ({ default: m.ProjectsView })))
const SkillsView = lazy(() => import('./SkillsView').then((m) => ({ default: m.SkillsView }))) // #261 公司 Skills 库
const ObservabilityView = lazy(() => import('./ObservabilityView').then((m) => ({ default: m.ObservabilityView })))
const MeView = lazy(() => import('./MeView').then((m) => ({ default: m.MeView })))
const ShippingView = lazy(() => import('./ShippingView').then((m) => ({ default: m.ShippingView })))

/** Centered placeholder while a view chunk loads — same shape the
 *  shipping view already used, now shared by every lazy view. */
function ViewFallback() {
  const t = useT()
  return <div className="h-full grid place-items-center text-sm text-ink-400">{t('common.loading')}</div>
}

/** #369 刀2:全屏视图壳 —— 所有非聊天视图统一「‹ 返回对话」返回语义
 *  (ADR 0009:点谁谁接管内容区)。取代刀1 的 TitleBar 临时返回钮。 */
function ViewShell({ children }: { children: ReactNode }) {
  const t = useT()
  const setView = useApp((s) => s.setView)
  return (
    <div className="grid h-full min-h-0 overflow-hidden" style={{ gridTemplateRows: 'auto minmax(0, 1fr)' }}>
      <div className="flex items-center gap-2 border-b border-ink-100 bg-cloud px-3 py-2">
        <button
          type="button"
          onClick={() => setView('conversations')}
          className="inline-flex h-7 items-center gap-1.5 rounded-lg border border-ink-200 bg-cloud px-2.5 text-[12.5px] text-ink-700 transition-colors hover:border-skype hover:bg-sky2-50 hover:text-skype-deep"
          aria-label={t('menu.backToChats')}
          title={`${t('menu.backToChats')} (Esc)`}
        >
          ‹ {t('menu.backToChats')}
        </button>
      </div>
      <div className="min-h-0 overflow-hidden">{children}</div>
    </div>
  )
}

function ConversationsLayout() {
  const infoOpen = useApp((s) => s.infoAgentId !== null)
  const threadOpen = useApp((s) => s.openThread !== null)
  // 选中项若是纯 agent 会话(「Agent 对话」分区),中栏渲染 WhisperRoom 而
  // 非 ChatPane —— 选中 id 复用 selectedConversationId(whisper id 在
  // conversations store 中不存在,App.tsx 的已读标记 effect 自然跳过)。
  const selected = useApp((s) => s.selectedConversationId)
  const whispers = useWhispers((s) => s.list)
  const whisperSelected = selected !== null && whispers.some((w) => w.id === selected)
  // Thread + info compete for the same right slot; opening one closes the
  // other implicitly via the store action. Render thread if both somehow
  // ended up set, since the thread is the more action-oriented pane.
  const rightOpen = threadOpen || infoOpen
  const { width, onResizeStart } = useResizableWidth('sidebar:conversations', 320, { min: 240, max: 520 })
  return (
    <div
      className="grid h-full overflow-hidden"
      style={{ gridTemplateColumns: rightOpen ? `${width}px minmax(0, 1fr) 420px` : `${width}px minmax(0, 1fr)` }}
    >
      <ConversationsPane onResizeStart={onResizeStart} />
      {whisperSelected && selected
        ? <Suspense fallback={<ViewFallback />}><WhisperRoom pairId={selected} /></Suspense>
        : <ChatPane />}
      {threadOpen
        ? <Suspense fallback={<ViewFallback />}><ThreadDrawer /></Suspense>
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

  // #368 刀1:B = 看板 ↔ 对话(唯一常驻非聊天面的快捷键,ADR 0009)。
  // 输入框聚焦/带修饰键时不动 —— 与 ⌘K 同一款全局键纪律。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey) return
      if (e.key !== 'b' && e.key !== 'B') return
      const el = e.target as HTMLElement | null
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return
      e.preventDefault()
      useApp.getState().closeNavMenu()
      setView(useApp.getState().view === 'boards' ? 'conversations' : 'boards')
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [setView])

  // #369 刀2:Esc 在二级视图 = 返回对话(逐层退回:NavMenu 自己消化 Esc,
  // 这里只处理视图层)。闸:菜单开着不管 / 会话视图不管(聊天侧 Esc 语义
  // 丰富:关搜索/关提及/取消回复)/ 输入框 target 不管(视图内编辑态)。
  // 关键时序(#373 评审 P1-2):本监听注册于应用挂载,在一切弹层的 window
  // 监听**之前**执行,此刻 defaultPrevented 恒 false —— 真正的退场必须
  // 延迟到事件分发结束后复查:弹层消费了这发 Esc(preventDefault)就让路,
  // 层级 = 弹层 → 视图。已知余量:嵌套弹层一次 Esc 会同关两层。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      const s = useApp.getState()
      if (s.navMenuOpen || s.view === 'conversations') return
      if (e.defaultPrevented) return
      const el = e.target as HTMLElement | null
      if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return
      setTimeout(() => {
        if (!e.defaultPrevented) useApp.getState().setView('conversations')
      }, 0)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

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
      {/* #368 刀1:rail(72px 图标栏)退役 —— 内容区单列;#369 刀2:二级面
          统一经 ViewShell 全屏接管(‹ 返回对话)。NavMenu 与 scrim 以绝对
          定位覆盖本行。 */}
      <div className="relative grid h-full min-h-0 overflow-hidden" style={{ gridTemplateColumns: 'minmax(0, 1fr)' }}>
        {view === 'conversations' && <ConversationsLayout />}
        {view !== 'conversations' && (
          <ViewShell>
            <Suspense fallback={<ViewFallback />}>
              {view === 'convene' && <ConveneView />}
              {view === 'agents' && <AgentsView />}
              {view === 'hr' && <HrView />}
              {view === 'boards' && <BoardsView />}
              {view === 'calendar' && <CalendarView />}
              {view === 'documents' && <DocumentsView />}
              {view === 'projects' && <ProjectsView />}
              {view === 'skills' && <SkillsView />}
              {view === 'shipping' && <ShippingView />}
              {view === 'observability' && devtoolsEnabled && <ObservabilityView />}
              {view === 'me' && <MeView />}
            </Suspense>
          </ViewShell>
        )}
        <NavMenu />
      </div>
      {/* Email composer drawer — globally rendered so opening it works
          from any view (sidebar Compose CTA, EmailCard reply button). */}
      <EmailComposer />
    </div>
  )
}
