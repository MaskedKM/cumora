import { lazy, Suspense, useEffect, useState } from 'react'
import { AnimatePresence, motion, useTransform, type MotionValue } from 'framer-motion'
import { useApp } from '@/stores/app'
import { useT } from '@/lib/i18n'
import { useWhispers } from '@/stores/whispers'
import { MobileChatList } from './MobileChatList'
import { MobileChat, MobileChatInfo } from './MobileChat'
import { MobileWhisperRoom } from './MobileWhispers'
import { MobileConvene } from './MobileConvene'
import { MobileLibrary, type LibTab } from './MobileLibrary'
import { MobileAgents } from './MobileAgents'
import { MobileMe } from './MobileMe'
import { BoardPeekContent, CalendarEventPeekContent } from '@/components/ArtifactPeekContent'
import { IBack } from '@/components/icons'
import { useDocuments } from '@/stores/documents'
import { IDoc } from '@/components/icons'
import { initPushNotifications } from '@/lib/push'
import { MobileParticipantInfo } from './MobileParticipantInfo'
import { useSwipeBackProps } from './useSwipeBack'
import { ViewBoundary } from './ViewBoundary'

const ShippingWorkspace = lazy(() => import('@/components/ShippingWorkspace').then((module) => ({ default: module.ShippingWorkspace })))
// #144b: tiptap+yjs+prosemirror ride along in the editor's own lazy chunk.
const DocumentEditor = lazy(() =>
  import('@/components/DocumentEditor').then((m) => ({ default: m.DocumentEditor })))

/** iOS UINavigationController push/pop spring. CRITICALLY DAMPED:
 *  damping ratio ζ = damping / (2·√(k·m)) = 38 / (2·√320) ≈ 1.06,
 *  i.e. juuust on the overdamped side of critical. The earlier
 *  "softer" attempt at 320/30 had ζ ≈ 0.84 — visibly underdamped —
 *  which made the entry slide overshoot ~3% past zero before
 *  settling, reading as "the page bounces left after it lands".
 *  Critical damping is what UIKit's default push uses; we match
 *  that here so there is no overshoot. Spring (not tween) is still
 *  the right transition type because the swipe-back commit reuses
 *  the same curve with the user's release velocity carried over. */
const slideTransition = { type: 'spring' as const, stiffness: 320, damping: 38, mass: 1 }
/** Cross-tab fades. iOS's UITabBarController doesn't animate the
 *  swap at all (just a hard cut on selection), but a hard cut on a
 *  hybrid app where each tab has loading time / network state reads
 *  as "jank". 140ms is just long enough to mask the layout pop. */
const fadeTransition = { duration: 0.14, ease: [0.4, 0, 0.2, 1] as const }

function MobileDocumentPeek({ documentId, onClose }: { documentId: string; onClose: () => void }) {
  const t = useT()
  const loaded = useDocuments((s) => s.loaded)
  const load = useDocuments((s) => s.load)
  const doc = useDocuments((s) => s.list.find((d) => d.id === documentId) ?? null)

  useEffect(() => {
    if (!loaded) void load()
  }, [load, loaded])

  if (!loaded) {
    return (
      <div className="h-full bg-cloud grid place-items-center">
        <div className="flex flex-col items-center gap-3 text-ink-400">
          <div className="w-12 h-12 rounded-[12px] grid place-items-center bg-sky2-50 text-skype-deep">
            <IDoc className="w-5 h-5" />
          </div>
          <div className="text-[12.5px] font-display italic">{t('mapp.openingDoc')}</div>
        </div>
      </div>
    )
  }

  if (!doc) {
    return (
      <div className="h-full bg-cloud grid place-items-center px-8 text-center">
        <div className="max-w-[260px]">
          <div className="mx-auto w-12 h-12 rounded-[12px] grid place-items-center bg-coral-soft/45 text-coral-deep">
            <IDoc className="w-5 h-5" />
          </div>
          <div className="mt-3 text-[14px] font-semibold text-ink-900">{t('mapp.docUnavailable')}</div>
          <div className="mt-1 text-[12px] text-ink-500 leading-relaxed">
            {t('mapp.docUnavailableBody')}
          </div>
          <button
            type="button"
            onClick={onClose}
            className="mt-4 h-8 px-3 rounded-[8px] text-[12px] font-semibold text-ink-600 border border-ink-100 hover:bg-sky2-50 transition"
          >
            {t('mapp.docUnavailableClose')}
          </button>
        </div>
      </div>
    )
  }

  return (
    <Suspense fallback={<div className="h-full" />}>
      <DocumentEditor
        documentId={documentId}
        variant="peek"
        onClose={onClose}
      />
    </Suspense>
  )
}

export function MobileApp() {
  const t = useT()
  const view = useApp((s) => s.view)
  const setView = useApp((s) => s.setView)
  const convoId = useApp((s) => s.selectedConversationId)
  const select = useApp((s) => s.selectConversation)
  const stack = useApp((s) => s.mobileStack)
  const pushStack = useApp((s) => s.pushMobileStack)
  const documentId = useApp((s) => s.openDocumentId)
  const closeDocumentPeek = useApp((s) => s.closeDocumentPeek)
  const boardId = useApp((s) => s.openBoardId)
  const boardCardId = useApp((s) => s.openBoardCardId)
  const closeBoardPeek = useApp((s) => s.closeBoardPeek)
  const calendarEventId = useApp((s) => s.openCalendarEventId)
  const closeCalendarEventPeek = useApp((s) => s.closeCalendarEventPeek)
  // Mobile reuses the desktop `infoAgentId` flag — tapping an avatar or
  // mention chip anywhere flips it on, which we surface as a slide-up
  // overlay (MobileParticipantInfo) below.
  const infoParticipantId = useApp((s) => s.infoAgentId)
  const closeAgentInfo = useApp((s) => s.closeAgentInfo)
  // #370 刀3(ADR 0009):纯 agent 会话由主列表「Agent 对话」分区直选,
  // 选中 id 复用 selectedConversationId(与桌面同款语义);聊天覆盖层据
  // 此换渲染 MobileWhisperRoom。
  const whispers = useWhispers((s) => s.list)
  const whisperSelected = convoId !== null && whispers.some((w) => w.id === convoId)

  // 看板/资料库直达:列表头看板钮、菜单资料库项、行动分区 board/calendar
  // 条目共用一条 tab 管线(nonce 让重复点击同目标也能重新生效;每次进库
  // 都走重挂载,initialTab 保证落页正确)。
  const [libraryReq, setLibraryReq] = useState<{ tab: LibTab; n: number } | null>(null)
  const openLibraryTab = (tab: LibTab) => {
    setLibraryReq((r) => ({ tab, n: (r?.n ?? 0) + 1 }))
    setView('library')
  }

  // Edge-swipe-back gestures for deep screens. Hooks are called
  // unconditionally (the per-page motion.div is conditional, but the
  // hooks need to be stable across renders).
  const chatSwipe = useSwipeBackProps(() => pushStack('list'))
  const infoSwipe = useSwipeBackProps(() => pushStack('chat'))
  const participantSwipe = useSwipeBackProps(() => closeAgentInfo())

  // Parallax — when a top layer (chat / info / whisper room) is
  // sliding right under the user's finger, the layer ABOUT TO BE
  // REVEALED needs to peek in from the left at ~30% the speed.
  // iOS uses ~30% parallax, which is what these transforms emulate.
  // The transforms react to the SAME motion value that drives the
  // top layer's drag + AnimatePresence enter/exit, so the
  // background slides in as the foreground slides out, no extra
  // wiring needed.
  const listPeekX = useParallax(chatSwipe.x)
  const artifactKey = documentId
    ? `doc-${documentId}`
    : boardId
      ? `board-${boardId}`
      : calendarEventId
        ? `calendar-${calendarEventId}`
        : null

  useEffect(() => {
    if (!convoId && stack !== 'list') pushStack('list')
  }, [convoId, stack, pushStack])

  // Wire push registration (APNs on iOS, FCM on Android) once the authed
  // mobile shell mounts. Soft no-op on web / Electron. The initializer is
  // idempotent — re-mounts (e.g. on company switch) won't re-prompt because
  // Capacitor caches the decision.
  useEffect(() => {
    void initPushNotifications()
  }, [])

  return (
    <div className="relative z-10 h-[100dvh] w-screen flex flex-col bg-paper">
      <main className="flex-1 relative overflow-hidden">
        {/* Top-level view switcher — was previously
            `<AnimatePresence mode="wait">`, which serializes exits
            before the next enter. Problem: if ANY nested exit got
            stuck (e.g. a MotionValue still being driven by a stale
            drag handler, or framer-motion losing track of a child
            during a fast tab tap), the outer wait blocked forever
            and the new tab never mounted → persistent white screen
            that only `kill app + relaunch` could clear.
            Default mode (sync) lets the old view's fade-out and the
            new view's fade-in overlap for the ~140ms transition.
            They're both `absolute inset-0`, so the overlap reads
            as a smooth crossfade, not jank. Worst case: even if an
            exit really IS stuck, the new view still mounts on top
            and the user can keep working. */}
        <AnimatePresence>
          {/* CONVERSATIONS view — list is always rendered when this
              view is active so it can peek behind the chat / info
              overlays via parallax during swipe-back. */}
          {view === 'conversations' && (
            <motion.div key="conv-root" className="absolute inset-0"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={fadeTransition}>
              <ViewBoundary name="Chats">
              {/* List (parallax background). `isolate` pins the
                  list's internal `z-10` sticky header inside this
                  layer's own stacking context, so it can't paint
                  on top of the chat/info overlays above. */}
              <motion.div
                className="absolute inset-0 isolate"
                // `willChange: transform` promotes the parallax
                // background to its own GPU layer so its translate
                // doesn't repaint the chat list inside on every drag
                // frame.
                style={{
                  x: stack === 'chat' ? listPeekX : 0,
                  zIndex: 0,
                  willChange: 'transform',
                }}
              >
                <MobileChatList onOpenLibraryTab={openLibraryTab} />
              </motion.div>
              {/* Chat / Info overlays. Explicit `zIndex: 1` +
                  inline opaque background guarantees they sit
                  above the parallax list AND fully occlude it
                  regardless of Tailwind's purge / class order. */}
              <AnimatePresence>
                {/* Chat stays MOUNTED while Info is open (note the `|| info`)
                    so it sits directly beneath the info card. Swiping the
                    info card away then reveals the chat that's already
                    there — instead of the old bug where chat unmounted at
                    stack 'info', the swipe uncovered the LIST, and chat then
                    replayed its slide-in entrance on the way back. Keeping
                    it mounted also preserves the chat's scroll position. */}
                {(stack === 'chat' || stack === 'info') && (
                  <motion.div key="conv-chat" className="absolute inset-0"
                    initial={{ x: '100%' }} animate={{ x: 0 }} exit={{ x: '100%' }}
                    transition={slideTransition}
                    {...chatSwipe.props}
                    style={{ ...chatSwipe.props.style, background: 'var(--paper)', zIndex: 1 }}>
                    {whisperSelected && convoId
                      ? <MobileWhisperRoom pairId={convoId} onBack={() => select(null)} />
                      : <MobileChat />}
                  </motion.div>
                )}
                {/* Info card sits ABOVE the chat (zIndex 2). */}
                {stack === 'info' && (
                  <motion.div key="conv-info" className="absolute inset-0"
                    initial={{ x: '100%' }} animate={{ x: 0 }} exit={{ x: '100%' }}
                    transition={slideTransition}
                    {...infoSwipe.props}
                    style={{ ...infoSwipe.props.style, background: 'var(--paper)', zIndex: 2 }}>
                    <MobileChatInfo />
                  </motion.div>
                )}
              </AnimatePresence>
              </ViewBoundary>
            </motion.div>
          )}

          {/* CONVENE view — reachable from chat header, shows empty state on its own */}
          {view === 'convene' && (
            <motion.div key="convene" className="absolute inset-0"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={fadeTransition}>
              <ViewBoundary name="Convene"><MobileConvene /></ViewBoundary>
            </motion.div>
          )}

          {/* LIBRARY view — documents, boards, calendar(#370 刀3:菜单进入,
              顶部返回头与桌面 ViewShell 同语义;flex 列让返回条不挤爆
              子视图的 h-full —— 评审 P1-1) */}
          {view === 'library' && (
            <motion.div key="library" className="absolute inset-0 flex flex-col"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={fadeTransition}>
              <ViewBoundary name="Library">
                <MobileViewBack />
                <div className="min-h-0 flex-1">
                  <MobileLibrary initialTab={libraryReq?.tab ?? 'documents'} tabNonce={libraryReq?.n ?? 0} />
                </div>
              </ViewBoundary>
            </motion.div>
          )}

          {/* SHIP view — end-to-end contract, verification, release, and learning loop */}
          {view === 'shipping' && (
            <motion.div key="shipping" className="absolute inset-0 flex flex-col"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={fadeTransition}>
              <ViewBoundary name="Ship">
                <MobileViewBack />
                <div className="min-h-0 flex-1">
                  <Suspense fallback={<div className="h-full grid place-items-center text-sm text-ink-400">{t('mapp.openingShip')}</div>}><ShippingWorkspace compact /></Suspense>
                </div>
              </ViewBoundary>
            </motion.div>
          )}

          {/* AGENTS view */}
          {view === 'agents' && (
            <motion.div key="agents" className="absolute inset-0 flex flex-col"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={fadeTransition}>
              <ViewBoundary name="Agents">
                <MobileViewBack />
                <div className="min-h-0 flex-1">
                  <MobileAgents />
                </div>
              </ViewBoundary>
            </motion.div>
          )}

          {/* ME view */}
          {view === 'me' && (
            <motion.div key="me" className="absolute inset-0 flex flex-col"
              initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }}
              transition={fadeTransition}>
              <ViewBoundary name="Me">
                <MobileViewBack />
                <div className="min-h-0 flex-1">
                  <MobileMe />
                </div>
              </ViewBoundary>
            </motion.div>
          )}
        </AnimatePresence>
      </main>
      <AnimatePresence>
        {artifactKey && (
          <motion.div
            key={artifactKey}
            className="absolute inset-0 z-40 bg-cloud"
            initial={{ y: '6%', opacity: 0 }}
            animate={{ y: 0, opacity: 1 }}
            exit={{ y: '6%', opacity: 0 }}
            transition={slideTransition}
          >
            {documentId ? (
              <MobileDocumentPeek documentId={documentId} onClose={closeDocumentPeek} />
            ) : boardId ? (
              <BoardPeekContent boardId={boardId} focusCardId={boardCardId} onClose={closeBoardPeek} />
            ) : calendarEventId ? (
              <CalendarEventPeekContent eventId={calendarEventId} onClose={closeCalendarEventPeek} />
            ) : null}
          </motion.div>
        )}
      </AnimatePresence>
      {/* Participant detail — slides in from the right when an avatar /
          mention chip is tapped. Sits on top of artifact peeks so a
          mention click inside a doc peek still navigates the user to
          the profile cleanly. */}
      <AnimatePresence>
        {infoParticipantId && (
          <motion.div
            key={`participant-${infoParticipantId}`}
            className="absolute inset-0 z-50 bg-paper"
            initial={{ x: '100%' }}
            animate={{ x: 0 }}
            exit={{ x: '100%' }}
            transition={slideTransition}
            {...participantSwipe.props}
          >
            <MobileParticipantInfo />
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  )
}

/** #370 刀3:菜单进入的二级面统一返回头 —— 与桌面 ViewShell 的
 *  「‹ 返回对话」同语义(移动端含安全区顶距)。 */
function MobileViewBack() {
  const t = useT()
  const setView = useApp((s) => s.setView)
  return (
    <div
      className="flex shrink-0 items-center gap-2 border-b border-ink-100 bg-cloud px-3 py-2"
      style={{ paddingTop: 'max(env(safe-area-inset-top), 8px)' }}
    >
      <button
        type="button"
        onClick={() => setView('conversations')}
        className="inline-flex h-8 items-center gap-1 rounded-lg border border-ink-200 bg-cloud px-2.5 text-[13px] text-ink-700 active:bg-sky2-50"
        aria-label={t('menu.backToChats')}
      >
        <IBack className="w-4 h-4" />
        {t('menu.backToChats')}
      </button>
    </div>
  )
}

/** Derive a parallax `x` for the layer SITTING UNDERNEATH a swipe-
 *  back top layer. iOS-native push/pop uses ~30% — the background
 *  moves at 30% the speed of the foreground. At top rest (x=0) the
 *  background sits 30%w to the left (just out of view); at top
 *  fully off (x=width) the background is centered (x=0).
 *
 *  Reads `window.innerWidth` ONCE per mount (cached + resized).
 *  Reading it inside the per-frame transform callback caused a layout
 *  flush every drag frame — measurable jank during the swipe. */
function useParallax(topX: MotionValue<number>) {
  const [w, setW] = useState(() => (typeof window !== 'undefined' ? window.innerWidth || 390 : 390))
  useEffect(() => {
    const onResize = () => setW(window.innerWidth || 390)
    window.addEventListener('resize', onResize)
    return () => window.removeEventListener('resize', onResize)
  }, [])
  return useTransform(topX, (v) => -w * 0.3 + (v / w) * (w * 0.3))
}
