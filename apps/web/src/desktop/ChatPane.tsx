import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Virtuoso, type VirtuosoHandle } from 'react-virtuoso'
import { useApp } from '@/stores/app'
import { useMe } from '@/stores/auth'
import { useConversations } from '@/stores/conversations'
import { useParticipants } from '@/stores/participants'
import { useMessages, messagesFor, useStreamingFor, VIRTUOSO_FIRST_INDEX_BASE } from '@/stores/messages'
import type { MessagesState } from '@/stores/messages'
import { api } from '@/api/client'
import { cn } from '@/lib/utils'
import { MessageRow } from '@/components/Message'
import { ScrollToLatestButton } from '@/components/ScrollToLatestButton'
import { ISearch } from '@/components/icons'
import { useT } from '@/lib/i18n'

/** Soft "Coming soon" popover anchored beneath the trigger. Auto-dismisses
 *  after a beat; also closes on outside-click or Escape. The sparkle
 *  drifts gently so the bubble feels alive rather than static. */

import { ChatHeader } from './ChatHeader'
import { ThreadLoader, ThreadError, EmptyConversationState } from './chatStates'
import { Composer } from './Composer'

export { Composer } from './Composer'


export function ChatPane() {
  const t = useT()
  const convoId = useApp((s) => s.selectedConversationId)
  const setView = useApp((s) => s.setView)
  // Atomic selectors — primitive / stable refs
  const byConvo = useMessages((s) => (convoId ? s.byConvo[convoId] : undefined))
  const streaming = useStreamingFor(convoId)
  const typingIds = useMessages((s) => (convoId ? s.typing[convoId] ?? null : null))
  const isLoading = useMessages((s) => (convoId ? s.loading.has(convoId) : false))
  // ThreadLoader visibility — the textbook loader-flicker pattern, with
  // BOTH guards in place:
  //   show-delay (400 ms): nothing renders until the load has been in
  //     flight that long. Cached convos / 404s / fast network loads all
  //     finish before this fires, so the loader stays hidden entirely.
  //   min-visible (500 ms): once the loader DOES appear, it must stay
  //     visible at least that long. Without this, a load that crosses
  //     the 400 ms threshold and finishes 80 ms later would flash the
  //     loader for 80 ms — exactly the "appears then immediately
  //     disappears" UX the user reported.
  //   Net: loads < 400 ms never show the loader; loads 400-900 ms show
  //   it for 500-900 ms (smooth, no flicker); loads > 900 ms show it
  //   for the full duration.
  const [showLoader, setShowLoader] = useState(false)
  const loaderTimers = useRef<{ show: number | null; hide: number | null; shownAt: number | null }>({
    show: null, hide: null, shownAt: null,
  })
  useEffect(() => {
    const t = loaderTimers.current
    if (t.show !== null) { window.clearTimeout(t.show); t.show = null }
    if (t.hide !== null) { window.clearTimeout(t.hide); t.hide = null }

    if (isLoading) {
      if (t.shownAt !== null) return  // already visible
      t.show = window.setTimeout(() => {
        setShowLoader(true)
        t.shownAt = Date.now()
        t.show = null
      }, 400)
      return
    }
    // Loading ended.
    if (t.shownAt === null) {
      setShowLoader(false)
      return
    }
    const elapsed = Date.now() - t.shownAt
    const remaining = Math.max(0, 500 - elapsed)
    if (remaining === 0) {
      setShowLoader(false)
      t.shownAt = null
      return
    }
    t.hide = window.setTimeout(() => {
      setShowLoader(false)
      t.shownAt = null
      t.hide = null
    }, remaining)
  }, [isLoading, convoId])
  useEffect(() => () => {
    const t = loaderTimers.current
    if (t.show !== null) window.clearTimeout(t.show)
    if (t.hide !== null) window.clearTimeout(t.hide)
  }, [])
  const loadError = useMessages((s) => (convoId ? s.errors[convoId] ?? null : null))
  const retryLoad = useMessages((s) => s.retryLoad)
  // Compose with memo so the rendered array ref stays stable when inputs do
  const list = useMemo(
    () => messagesFor({ byConvo: byConvo ? { [convoId!]: byConvo } : {}, streaming } as MessagesState, convoId),
    [byConvo, streaming, convoId],
  )
  const conversations = useConversations((s) => s.list)
  const c = useMemo(() => conversations.find((x) => x.id === convoId), [conversations, convoId])
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  const streamRef = useRef<HTMLDivElement>(null)
  const virtuosoRef = useRef<VirtuosoHandle | null>(null)
  // Whether the scroll is currently anchored to the latest message — drives
  // the bottom-right "scroll to latest" pill that appears once the user
  // scrolls up. Default true so the pill stays hidden on first mount.
  const [atBottom, setAtBottom] = useState(true)
  const scrollToLatest = useCallback(() => {
    if (list.length === 0) return
    virtuosoRef.current?.scrollToIndex({ index: list.length - 1, align: 'end', behavior: 'smooth' })
  }, [list.length])

  // Older-history pager — virtualization keeps the DOM small; this fetches
  // the next page upward when the user scrolls past the top.
  const hasMoreOlder = useMessages((s) => (convoId ? s.hasMoreOlder[convoId] ?? false : false))
  const loadingOlder = useMessages((s) => (convoId ? s.loadingOlder.has(convoId) : false))
  const loadOlder = useMessages((s) => s.loadOlder)
  // Anchor for upward pagination — the store decrements this per prepend so
  // Virtuoso holds scroll position when older history pages in.
  const firstItemIndex = useMessages((s) => (convoId ? s.firstItemIndex[convoId] ?? VIRTUOSO_FIRST_INDEX_BASE : VIRTUOSO_FIRST_INDEX_BASE))
  const onStartReached = useCallback(() => {
    if (!convoId) return
    if (!hasMoreOlder || loadingOlder) return
    void loadOlder(convoId)
  }, [convoId, hasMoreOlder, loadingOlder, loadOlder])

  // In-conversation search — opened by the chat-header search icon.
  // We don't filter the thread; we highlight matching rows in place and
  // jump between them with the up/down arrows or Enter / Shift+Enter.
  const [searchOpen, setSearchOpen] = useState(false)
  const [searchQuery, setSearchQuery] = useState('')
  const [matchIdx, setMatchIdx] = useState(0)
  const searchInputRef = useRef<HTMLInputElement | null>(null)
  const matchedIds = useMemo(() => {
    const q = searchQuery.trim().toLowerCase()
    if (!q) return [] as string[]
    return list
      .filter((m) => typeof m.body === 'string' && m.body.toLowerCase().includes(q))
      .map((m) => m.id)
  }, [list, searchQuery])
  // Reset the search when the user navigates to a different conversation.
  useEffect(() => {
    setSearchOpen(false); setSearchQuery(''); setMatchIdx(0)
  }, [convoId])
  // Reset to the first hit whenever the result set changes.
  useEffect(() => { setMatchIdx(0) }, [matchedIds.length])
  // Scroll the current hit into view. We virtualize the message list so a
  // matched row may not even be mounted yet — virtuoso's scrollToIndex
  // mounts and centers it in one go.
  useEffect(() => {
    const id = matchedIds[matchIdx]
    if (!id) return
    const index = list.findIndex((m) => m.id === id)
    if (index < 0) return
    virtuosoRef.current?.scrollToIndex({ index, align: 'center', behavior: 'smooth' })
  }, [matchedIds, matchIdx, list])

  // Centralized "jump to message" — quote clicks and `#N` chips both set
  // useApp.pendingJumpMessageId and we resolve it here. virtuoso.scrollToIndex
  // mounts off-screen rows reliably; previously a quote click that lost its
  // DOM element (Virtuoso recycled it) silently did nothing. Once the target
  // is mounted we briefly flash it like the old quote jump did.
  const pendingJumpId = useApp((s) => s.pendingJumpMessageId)
  const clearPendingJump = useApp((s) => s.clearPendingJump)
  useEffect(() => {
    if (!pendingJumpId) return
    const index = list.findIndex((m) => m.id === pendingJumpId)
    if (index >= 0) {
      virtuosoRef.current?.scrollToIndex({ index, align: 'center', behavior: 'smooth' })
      // Wait for Virtuoso to mount the row (smooth scroll + recycle ≈ 0–500ms),
      // then flash it. Poll briefly because mount timing varies.
      const targetId = pendingJumpId
      const deadline = Date.now() + 800
      const tryFlash = (): void => {
        const el = document.getElementById(`m-${targetId}`)
        if (el) {
          el.classList.add('quote-jump-flash')
          window.setTimeout(() => el.classList.remove('quote-jump-flash'), 1400)
        } else if (Date.now() < deadline) {
          window.setTimeout(tryFlash, 60)
        }
      }
      window.setTimeout(tryFlash, 80)
    }
    // Clear after we've handled it so a repeat click on the same id re-fires.
    clearPendingJump()
  }, [pendingJumpId, list, clearPendingJump])
  // Auto-focus the search input when the bar opens.
  useEffect(() => {
    if (searchOpen) {
      // requestAnimationFrame: wait for the input to mount.
      const h = window.requestAnimationFrame(() => searchInputRef.current?.focus())
      return () => window.cancelAnimationFrame(h)
    }
  }, [searchOpen])

  // Track which message IDs were already present when this conversation
  // first opened — those get the "initial wave" stagger. Anything that lands
  // after that is brand-new and animates immediately (delay 0), so the thread
  // doesn't blink with empty space while the new row waits its turn.
  const initialIdsRef = useRef<Set<string> | null>(null)
  // Messages that have already played their rise-in fade this convo session.
  // Virtuoso unmounts/remounts rows as you scroll or jump to a quote, and a
  // remount replays the fade — that's the "the quoted message reloads / fades
  // back in after the flash" bug. Animate each message at most once per open.
  const animatedIdsRef = useRef<Set<string>>(new Set())
  const lastConvoRef = useRef<string | null>(null)
  // Sticky "first scroll for this convo hasn't happened yet" flag. The effect
  // below can't compare lastConvoRef to convoId because we sync the ref here
  // at render time — by the time the effect runs they're already equal. The
  // flag stays true until messages actually land and we do the instant snap.
  const pendingConvoSwitchRef = useRef(true)
  if (lastConvoRef.current !== convoId) {
    lastConvoRef.current = convoId
    initialIdsRef.current = new Set(list.map((m) => m.id))
    pendingConvoSwitchRef.current = true
    animatedIdsRef.current = new Set()
  } else if (initialIdsRef.current === null) {
    initialIdsRef.current = new Set(list.map((m) => m.id))
  }

  useEffect(() => {
    // Virtuoso's `followOutput` keeps appended messages glued to the bottom;
    // we only need to do an explicit jump when the user switches into a
    // conversation that already had messages loaded (initialTopMostItemIndex
    // only fires on first mount, not on convo switches within the same
    // mounted instance).
    if (list.length === 0) return
    const isConvoSwitch = pendingConvoSwitchRef.current
    pendingConvoSwitchRef.current = false
    if (isConvoSwitch) {
      virtuosoRef.current?.scrollToIndex({ index: list.length - 1, align: 'end', behavior: 'auto' })
    }
  }, [list.length, convoId])

  // IMPORTANT: every hook in this component must run on EVERY render —
  // React enforces a stable hook order. The "no conversation selected"
  // branch lives below the hooks, not in their middle. (Previously this
  // useMemo sat after an early return, so leaving a group / clearing
  // the selection dropped the hook count between renders and crashed
  // the tree with "Rendered fewer hooks than expected".)
  // Drop the local user from the typing list — seeing "you are typing…"
  // while your own composer is right there reads as a UI hiccup. Humans
  // and agents both broadcast on the same channel; the filter keeps the
  // indicator focused on OTHER participants.
  const typingAgents = useMemo(
    () => (typingIds ?? [])
      .filter((id) => id !== meId)
      .map((id) => byId[id])
      .filter((p): p is NonNullable<typeof p> => Boolean(p)),
    [typingIds, byId, meId],
  )

  // Render the empty state until the selected conversation belongs to the
  // current list. During a company switch the old convoId can survive for
  // a render while the new tenant's conversations are loading; requiring
  // `c` here keeps the composer from flashing before the cloud appears.
  if (!convoId || !c) {
    return <EmptyConversationState />
  }

  const onConvene = async () => {
    if (!convoId) return
    try {
      await api.startConvene(convoId, c?.title ?? 'live work session')
      setView('convene')
    } catch (err) { console.warn('start convene failed', err) }
  }

  return (
    <main
      className={cn(
        'grid overflow-hidden',
        searchOpen
          ? 'grid-rows-[auto_auto_minmax(0,1fr)_auto]'
          : 'grid-rows-[auto_minmax(0,1fr)_auto]',
      )}
      style={{
        // Background lives on <main> so the radial washes span the ENTIRE
        // chat surface (header + thread + composer share one continuous
        // background). Putting the gradient on the inner thread div used
        // to clip the coral haze right where the composer started.
        background: 'radial-gradient(ellipse 80% 40% at 0% 0%, rgba(194, 230, 251, 0.3), transparent 60%), radial-gradient(ellipse 60% 40% at 100% 100%, rgba(255, 217, 210, 0.25), transparent 60%), var(--cloud)',
      }}
    >
      <ChatHeader
        convoId={convoId}
        onConvene={onConvene}
        onToggleSearch={() => setSearchOpen((v) => !v)}
        searchOpen={searchOpen}
      />
      {searchOpen && (
        <div className="px-[22px] py-2 border-b border-ink-100 bg-paper/60 backdrop-blur-sm flex items-center gap-2">
          <div className="flex-1 flex items-center gap-2 px-3 py-1.5 bg-cloud border border-ink-100 rounded-[10px] focus-within:border-sky2-300 text-ink-500 text-[13px]">
            <ISearch className="w-3.5 h-3.5" strokeWidth={2} />
            <input
              ref={searchInputRef}
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') { setSearchOpen(false); setSearchQuery(''); return }
                const n = matchedIds.length
                if (n === 0) return
                if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); setMatchIdx((i) => (i + 1) % n); return }
                if ((e.key === 'Enter' && e.shiftKey) || e.key === 'ArrowUp') { e.preventDefault(); setMatchIdx((i) => (i - 1 + n) % n); return }
                if (e.key === 'ArrowDown') { e.preventDefault(); setMatchIdx((i) => (i + 1) % n) }
              }}
              placeholder={t('chat.searchPlaceholder')}
              className="flex-1 min-w-0 bg-transparent outline-none text-ink-900 placeholder:text-ink-300"
            />
            <span className="shrink-0 font-mono text-[11px] tabular-nums text-ink-300">
              {matchedIds.length === 0
                ? (searchQuery.trim() ? t('chat.searchNoMatch') : '')
                : `${matchIdx + 1} / ${matchedIds.length}`}
            </span>
          </div>
          <button
            type="button"
            onClick={() => setMatchIdx((i) => (i - 1 + matchedIds.length) % Math.max(1, matchedIds.length))}
            disabled={matchedIds.length === 0}
            title={t('chat.prevMatch')}
            className="w-8 h-8 rounded-[8px] grid place-items-center text-ink-500 hover:bg-sky2-50 hover:text-skype-deep transition disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-ink-500"
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" className="w-4 h-4">
              <polyline points="18 15 12 9 6 15" />
            </svg>
          </button>
          <button
            type="button"
            onClick={() => setMatchIdx((i) => (i + 1) % Math.max(1, matchedIds.length))}
            disabled={matchedIds.length === 0}
            title={t('chat.nextMatch')}
            className="w-8 h-8 rounded-[8px] grid place-items-center text-ink-500 hover:bg-sky2-50 hover:text-skype-deep transition disabled:opacity-40 disabled:hover:bg-transparent disabled:hover:text-ink-500"
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" className="w-4 h-4">
              <polyline points="6 9 12 15 18 9" />
            </svg>
          </button>
          <button
            type="button"
            onClick={() => { setSearchOpen(false); setSearchQuery('') }}
            title={t('chat.closeEsc')}
            className="w-8 h-8 rounded-[8px] grid place-items-center text-ink-500 hover:bg-sky2-50 transition"
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" className="w-4 h-4">
              <line x1="6" y1="6" x2="18" y2="18" />
              <line x1="18" y1="6" x2="6" y2="18" />
            </svg>
          </button>
        </div>
      )}
      <div ref={streamRef} className="min-h-0 relative">
        {/* Empty-state branches: an error from the initial fetch wins over the
            loader (a stale spinner under an error message would be confusing).
            Both only render when the thread itself is empty — once any messages
            have landed we let the regular list take over so a transient WS
            reconnect blip doesn't yank the conversation out from under the
            user. */}
        {list.length === 0 && loadError ? (
          <div className="px-6 py-6">
            <ThreadError message={loadError} onRetry={() => retryLoad(convoId)} />
          </div>
        ) : list.length === 0 && showLoader ? (
          <div className="px-6 py-6">
            <ThreadLoader />
          </div>
        ) : (
          <Virtuoso
            ref={virtuosoRef}
            className="h-full"
            data={list}
            firstItemIndex={firstItemIndex}
            followOutput="auto"
            initialTopMostItemIndex={Math.max(0, list.length - 1)}
            startReached={onStartReached}
            atBottomStateChange={setAtBottom}
            // Initial-height hint so Virtuoso's first-pass sizing is
            // close to the real per-message height (avatar + 1-2 lines
            // of text). Without it the list starts assuming a tiny
            // default and every ResizeObserver tick pushes content
            // around, which compounds with image / OG-card lazy loads
            // into the scroll jitter users see.
            defaultItemHeight={96}
            increaseViewportBy={{ top: 800, bottom: 800 }}
            components={{
              Header: () => (
                <div className="px-6 pt-6 flex flex-col gap-2">
                  {hasMoreOlder ? (
                    <div className="self-center py-1 px-2.5 rounded-full text-[10.5px] font-medium text-ink-400">
                      {loadingOlder ? t('chat.loadingEarlier') : ' '}
                    </div>
                  ) : (
                    <div className="flex items-center gap-3 text-ink-300 text-[11px] font-bold tracking-[0.08em] uppercase">
                      <span className="flex-1 h-px bg-gradient-to-r from-transparent via-ink-100 to-transparent" />
                      {t('chat.beginning')}
                      <span className="flex-1 h-px bg-gradient-to-r from-transparent via-ink-100 to-transparent" />
                    </div>
                  )}
                </div>
              ),
              Footer: () => <div className="h-3" />,
            }}
            computeItemKey={(_index, m) => m.clientId ?? m.id}
            itemContent={(i, m) => {
              const author = byId[m.authorId]
              // System / whisper rows render without a resolved author (e.g. the
              // calendar-fired notice has a synthetic system author id). Only
              // gate real authored messages on the participant being loaded.
              if (!author && m.kind !== 'system' && m.kind !== 'whisper-link') return <div className="h-0" />
              const wasInitial = initialIdsRef.current?.has(m.id) ?? false
              const delay = wasInitial ? Math.min(i * 30, 200) : 0
              // Animate a message's rise-in at most once per convo session, so a
              // Virtuoso remount (scroll / quote-jump) doesn't replay the fade.
              const firstAnimation = !animatedIdsRef.current.has(m.id)
              if (firstAnimation) animatedIdsRef.current.add(m.id)
              const isMatch = searchOpen && matchedIds.includes(m.id)
              const isCurrent = isMatch && matchedIds[matchIdx] === m.id
              return (
                <div
                  data-msg-id={m.id}
                  className={cn(
                    'px-6 py-[9px] rounded-[10px] transition-shadow',
                    isMatch && 'ring-1 ring-gold/40',
                    isCurrent && 'ring-2 ring-gold shadow-[0_0_24px_-4px_rgba(244,183,64,0.55)]',
                  )}
                >
                  <MessageRow msg={m} author={author} delay={delay} animate={firstAnimation} />
                </div>
              )
            }}
          />
        )}
        {/* Bottom-right "scroll to latest" pill — appears once the user has
            scrolled up off the bottom. Fades in (animate-rise), tucks against
            the composer's top edge so it doesn't fight the typing area. */}
        <ScrollToLatestButton visible={!atBottom} onClick={scrollToLatest} />
      </div>
      <Composer convoId={convoId} typingNames={typingAgents.map((a) => a.name)} />
    </main>
  )
}
