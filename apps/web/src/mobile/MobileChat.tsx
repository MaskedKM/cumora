// 子组件分桶(#219 ⑤):本文件保留 MobileChat 壳(状态/取数/编排 —— 消息流
// 订阅与分页、@提及/表情/附件/回复/Tapback 状态、全部副作用、Virtuoso 装配
// 与发送/提及/插入处理器),原局部子组件与 JSX 大块按职责分居 ./chat/:
//   header(顶栏+更多菜单)· stream(StreamCtx/StreamHeader/StreamFooter/
//   STREAM_COMPONENTS/MessageRowMobileShell)· composer(MentionEntry+输入区)
//   · tapback(长按菜单动作装配)· info(MobileChatInfo 详情页+Stat)。
// header/composer 两件的 JSX 体仅去一级缩进,状态全部留在壳经 props 透传。
// 消费面不变:MobileApp 仍 `import { MobileChat, MobileChatInfo } from
// './MobileChat'`(MobileChatInfo 经下方 re-export 转口)。store 消费面未动
// (zustand v5 全部单值 selector,无新对象返回,无需 useShallow)。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Virtuoso, type VirtuosoHandle } from 'react-virtuoso'
import { type ApiAttachment, api } from '@/api/client'
import type { RichInputHandle } from '@/components/RichInput'
import { ScrollToLatestButton } from '@/components/ScrollToLatestButton'
import { useT } from '@/lib/i18n'
import { isImeComposing } from '@/lib/keyboard'
import { tapHaptic } from '@/lib/native'
import { findSkypeByShortcode } from '@/lib/skypeEmojis'
import { useApp } from '@/stores/app'
import { useMe } from '@/stores/auth'
import { isMuted, useConversations } from '@/stores/conversations'
import type { MessagesState } from '@/stores/messages'
import { messagesFor, sendUserMessage, toggleReaction, useMessages, useStreamingFor, VIRTUOSO_FIRST_INDEX_BASE } from '@/stores/messages'
import { useParticipants } from '@/stores/participants'
import type { Message, Participant } from '@/types'
import { Composer, type MentionEntry } from './chat/composer'
import { ChatHeader } from './chat/header'
import { MessageRowMobileShell, STREAM_COMPONENTS, type StreamCtx } from './chat/stream'
import { buildMessageTapbackActions } from './chat/tapback'
import { MobileMessageTapback } from './MobileMessageTapback'

export { MobileChatInfo } from './chat/info'

export function MobileChat() {
  const t = useT()
  const convoId = useApp((s) => s.selectedConversationId)
  const select = useApp((s) => s.selectConversation)
  const pushStack = useApp((s) => s.pushMobileStack)
  const setView = useApp((s) => s.setView)
  const byConvo = useMessages((s) => (convoId ? s.byConvo[convoId] : undefined))
  const streaming = useStreamingFor(convoId)
  const typingIds = useMessages((s) => (convoId ? s.typing[convoId] ?? null : null))
  const [menuOpen, setMenuOpen] = useState(false)
  const [conveneStarting, setConveneStarting] = useState(false)
  const [emojiOpen, setEmojiOpen] = useState(false)
  const [emojiTab, setEmojiTab] = useState<'std' | 'skype'>('std')
  // Older-history pager — virtualization handles the runtime cost of mounting
  // long threads; we still fetch in pages of 80 so a 5k-message room doesn't
  // pay a single huge SQL + JSON parse on cold open. See messages store
  // (`MESSAGES_PAGE_SIZE`) for the server's matching default.
  const hasMoreOlder = useMessages((s) => (convoId ? s.hasMoreOlder[convoId] ?? false : false))
  const loadingOlder = useMessages((s) => (convoId ? s.loadingOlder.has(convoId) : false))
  const loadOlder = useMessages((s) => s.loadOlder)
  // Anchor for upward pagination — the store decrements this per prepend so
  // Virtuoso holds scroll position when older history pages in.
  const firstItemIndex = useMessages((s) => (convoId ? s.firstItemIndex[convoId] ?? VIRTUOSO_FIRST_INDEX_BASE : VIRTUOSO_FIRST_INDEX_BASE))
  const virtuosoRef = useRef<VirtuosoHandle | null>(null)
  const list = useMemo(
    () => messagesFor({ byConvo: byConvo ? { [convoId!]: byConvo } : {}, streaming } as MessagesState, convoId),
    [byConvo, streaming, convoId],
  )
  // Drives the bottom-right "scroll to latest" pill — true means we're pinned
  // at the bottom and the pill stays hidden. Note: there's also an existing
  // `scrollToLatest` (declared below) that snaps with `behavior:'auto'` for the
  // iOS-keyboard path; explicit user clicks want a smooth animation, so we
  // keep a separate handler instead of reusing it.
  const [atBottom, setAtBottom] = useState(true)
  const smoothScrollToLatest = useCallback(() => {
    if (list.length === 0) return
    virtuosoRef.current?.scrollToIndex({ index: list.length - 1, align: 'end', behavior: 'smooth' })
  }, [list.length])
  const conversations = useConversations((s) => s.list)
  const c = useMemo(() => conversations.find((x) => x.id === convoId), [conversations, convoId])
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  // Per-convo composer draft, persisted in the global store so the text
  // survives MobileChat unmounts (chat → list → chat keeps the half-typed
  // message). Send + convo-switch clears the entry; the cleanup happens
  // inside `send()` and the convoId effect below.
  const draft = useApp((s) => (convoId ? s.composerDrafts[convoId] ?? '' : ''))
  const setComposerDraft = useApp((s) => s.setComposerDraft)
  const setDraft = useCallback((text: string) => {
    if (!convoId) return
    setComposerDraft(convoId, text)
  }, [convoId, setComposerDraft])
  const [attachment, setAttachment] = useState<ApiAttachment | null>(null)
  const [uploading, setUploading] = useState(false)
  const [uploadError, setUploadError] = useState<string | null>(null)
  // Per-convo "replying to" pointer, lifted from the global app store so
  // it persists across mounts and matches the desktop composer's UX.
  const replyingToId = useApp((s) => convoId ? s.replyingTo[convoId] : undefined)
  const setReplyingTo = useApp((s) => s.setReplyingTo)
  const replyingToMsg = useMemo(
    () => (replyingToId && byConvo ? byConvo.find((m) => m.id === replyingToId) : undefined),
    [byConvo, replyingToId],
  )
  // @-mention picker state. Hoisted above the early-return below because
  // every hook must run on every render — see ChatPane.tsx note for the
  // same rule. `mention` is null when the picker is closed.
  const [mention, setMention] = useState<{ start: number; query: string } | null>(null)
  const [mentionIndex, setMentionIndex] = useState(0)
  const editorRef = useRef<RichInputHandle>(null)
  const fileRef = useRef<HTMLInputElement>(null)
  const streamRef = useRef<HTMLDivElement>(null)

  // Members the user can @-mention. Excludes self. Safe when `c` is
  // undefined (convo not yet loaded) — picker just stays empty.
  const memberPool = useMemo<Participant[]>(() => {
    if (!c) return []
    return c.members
      .map((id) => byId[id])
      .filter((p): p is Participant => Boolean(p) && p.id !== meId)
  }, [c, byId, meId])

  const filteredMentions = useMemo<MentionEntry[]>(() => {
    if (!mention) return []
    const q = mention.query.toLowerCase()
    const out: MentionEntry[] = []
    if (memberPool.length > 0 && (q === '' || 'all'.startsWith(q))) {
      out.push({ kind: 'all' })
    }
    for (const p of memberPool) {
      if (p.id.toLowerCase().includes(q) || p.name.toLowerCase().includes(q)) {
        out.push({ kind: 'participant', p })
        if (out.length >= 7) break
      }
    }
    return out
  }, [mention, memberPool])

  // Virtuoso auto-sticks to the bottom on append via `followOutput`; the
  // legacy manual `el.scrollTop = el.scrollHeight` effect is gone with the
  // <div> stream container it depended on.

  // Reset attachment / mention / emoji-picker side state on convo
  // switch. The composer text itself is reset by REMOUNTING RichInput
  // via `key={convoId}` on the JSX below — that path reads the right
  // draft from the store via defaultValue without going through
  // setValue/moveCaretToEnd, which on iOS implicitly focuses the
  // contenteditable in-gesture and pops the soft keyboard.
  useEffect(() => {
    if (!convoId) return
    setAttachment(null)
    setUploadError(null)
    setMention(null)
    setEmojiOpen(false)
  }, [convoId])

  const onStartReached = useCallback(() => {
    if (!convoId) return
    if (!hasMoreOlder || loadingOlder) return
    void loadOlder(convoId)
  }, [convoId, hasMoreOlder, loadingOlder, loadOlder])

  // Dynamic flags for the stream Header, handed to Virtuoso via `context` so
  // the Header component identity stays stable (see StreamHeader). Memoized so
  // a fresh object only appears when a flag actually changes — not on every
  // streaming tick.
  const streamCtx = useMemo<StreamCtx>(
    () => ({ hasMoreOlder, loadingOlder }),
    [hasMoreOlder, loadingOlder],
  )

  // Snapshot of when this convo opened. Only messages created AFTER
  // this timestamp get the rise-in fade — older history that scrolls
  // back into view via Virtuoso virtualization stays static.
  // Re-snapshot on convo switch so the new convo gets the same
  // "fresh messages only animate" treatment.
  const convoOpenedAtRef = useRef<number>(Date.now())
  useEffect(() => {
    convoOpenedAtRef.current = Date.now()
  }, [convoId])

  // Centralized "jump to message" (quote / `#N`) — virtuoso.scrollToIndex
  // mounts off-screen rows reliably, unlike the old getElementById path that
  // silently no-op'd when Virtuoso had recycled the row. Flashes once mounted.
  const pendingJumpId = useApp((s) => s.pendingJumpMessageId)
  const clearPendingJump = useApp((s) => s.clearPendingJump)
  useEffect(() => {
    if (!pendingJumpId) return
    const index = list.findIndex((m) => m.id === pendingJumpId)
    if (index >= 0) {
      virtuosoRef.current?.scrollToIndex({ index, align: 'center', behavior: 'smooth' })
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
    clearPendingJump()
  }, [pendingJumpId, list, clearPendingJump])

  // Long-press tapback state. When set, MobileMessageTapback renders
  // an iOS Messages-style reaction strip + action menu anchored to
  // the touch coords. Cleared on dismiss / action / convo switch.
  const [tapback, setTapback] = useState<{ msg: Message; coords: { x: number; y: number } } | null>(null)
  useEffect(() => { setTapback(null) }, [convoId])

  // Keep the latest message visible when the soft keyboard pops up.
  // iOS fires `focusin` on the contenteditable AND a `visualViewport`
  // resize as the keyboard animates in (~300ms). We listen to both
  // so the snap-to-bottom happens at the right moment regardless of
  // which fires first / when the layout settles.
  const scrollToLatest = useCallback(() => {
    const n = list.length
    if (n === 0) return
    virtuosoRef.current?.scrollToIndex({ index: n - 1, align: 'end', behavior: 'auto' })
  }, [list.length])
  useEffect(() => {
    const vv = window.visualViewport
    if (!vv) return
    const onResize = () => {
      // Only scroll if the editor is focused — otherwise a keyboard
      // resize from some other element shouldn't yank this list.
      const active = document.activeElement
      if (active && active.closest('.rich-input')) scrollToLatest()
    }
    vv.addEventListener('resize', onResize)
    return () => vv.removeEventListener('resize', onResize)
  }, [scrollToLatest])

  // Tap (or scroll) on the message stream while the composer is focused
  // dismisses the soft keyboard — like a native chat. We listen in the CAPTURE
  // phase on the stream container so we run BEFORE the message row's own touch
  // handlers (the long-press/tap-back lives on the rows): a genuine TAP gets
  // swallowed so the first touch only dismisses the keyboard instead of also
  // popping the reaction strip (the "need to tap twice" bug). A scroll-drag
  // still dismisses but is NOT swallowed, so the list scrolls normally.
  useEffect(() => {
    const el = streamRef.current
    if (!el) return
    let armed = false
    const composerFocused = () => {
      const a = document.activeElement as HTMLElement | null
      return !!(a && a.closest('.rich-input'))
    }
    const onStart = (e: TouchEvent) => {
      if (!composerFocused()) { armed = false; return }
      armed = true
      ;(document.activeElement as HTMLElement | null)?.blur() // dismiss keyboard
      // Stop the touch reaching the message row. useLongPress arms its 450ms
      // timer on touchstart and cancels it on touchend; if we only swallowed
      // touchend the timer would still fire and pop the "Reaction / Copy"
      // tap-back. Cutting it at touchstart means the row never engages at all.
      // We do NOT preventDefault, so the list still scrolls under the finger.
      e.stopPropagation()
    }
    const onEnd = (e: TouchEvent) => {
      if (!armed) return
      armed = false
      e.stopPropagation()
      e.preventDefault() // block the synthesized click so nothing else fires
    }
    el.addEventListener('touchstart', onStart, { capture: true })
    el.addEventListener('touchend', onEnd, { capture: true })
    return () => {
      el.removeEventListener('touchstart', onStart, { capture: true })
      el.removeEventListener('touchend', onEnd, { capture: true })
    }
  }, [])

  // Reply tap → focus composer. On mobile this lifts the soft keyboard,
  // which is the whole point: hitting reply without the keyboard
  // appearing feels broken. BUT only on user-initiated reply changes —
  // if the user just navigated INTO this convo and there happens to be
  // a leftover replyingToId in the store, focusing on mount would pop
  // the keyboard the user didn't ask for. Guard with a ref that
  // distinguishes the initial value from in-session changes.
  const replyingToInitializedRef = useRef(false)
  useEffect(() => {
    if (!replyingToInitializedRef.current) {
      replyingToInitializedRef.current = true
      return
    }
    if (!replyingToId) return
    requestAnimationFrame(() => editorRef.current?.focus())
  }, [replyingToId])

  if (!convoId || !c) return null

  // Resolve typing ids → display names. Three hardenings over the naive
  // `byId[id]?.name`:
  //   1. Drop the local user (`id !== meId`) — "you are typing…" with your
  //      own composer right there reads as a glitch (matches desktop + the
  //      conversation list, which already filter self; the chat didn't).
  //   2. `.trim()` + fall back to a label: a resolved participant whose name
  //      is blank/whitespace would otherwise slip past a plain `Boolean(n)`
  //      filter and render "<b></b> is typing…" — i.e. the nameless
  //      "is typing…" bug. Now it shows the real name, or a graceful
  //      "An agent / Someone" when the roster row has no usable name.
  //   3. Genuinely-unknown ids (not in byId yet) are still dropped so a
  //      stale/cross-room typing ping can't surface a phantom indicator.
  const typingNames = (typingIds ?? [])
    .filter((id) => id !== meId)
    .map((id) => {
      const p = byId[id]
      if (!p) return null
      const name = p.name?.trim()
      return name || (p.kind === 'agent' ? t('mclist.fallbackAgent') : t('mclist.fallbackSomeone'))
    })
    .filter((n): n is string => Boolean(n))
  const muted = isMuted(c)

  const onConvene = async () => {
    if (!convoId || conveneStarting) return
    setConveneStarting(true)
    try {
      await api.startConvene(convoId, c.title || t('convene.sessionTitleFallback'))
      setView('convene')
    } catch (err) {
      console.warn('start convene failed', err)
    } finally {
      setConveneStarting(false)
    }
  }

  const toggleMute = async () => {
    if (!convoId) return
    try {
      await api.setMute(convoId, !muted)
      await useConversations.getState().reload()
    } catch (err) { console.warn('mute toggle failed', err) }
    setMenuOpen(false)
  }

  const leaveConvo = async () => {
    if (!convoId) return
    if (!confirm(t('mobchat.confirmLeaveBody'))) {
      setMenuOpen(false)
      return
    }
    try {
      await api.leaveConversation(convoId)
      select(null)
      await useConversations.getState().reload()
    } catch (err) { console.warn('leave failed', err) }
    setMenuOpen(false)
  }

  const upload = async (file: File) => {
    setUploading(true); setUploadError(null)
    try {
      const a = await api.uploadFile(file)
      setAttachment(a)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      setUploadError(msg)
      window.setTimeout(() => setUploadError(null), 4500)
    } finally {
      setUploading(false)
    }
  }
  const onPickFile = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0]
    e.target.value = ''
    if (f) await upload(f)
  }

  // Recompute mention state when the draft or caret moves. Mirrors the
  // desktop logic in ChatPane.tsx so the behaviour stays consistent:
  // - find the latest `@` before the caret
  // - require it sit at the start or after whitespace (not mid-word)
  // - bail if a space has already been typed after `@`
  // - bail if the query is suspiciously long (>30 chars)
  const updateMention = (text: string, caret: number) => {
    const slice = text.slice(0, caret)
    const at = slice.lastIndexOf('@')
    if (at < 0) { setMention(null); return }
    const before = at === 0 ? '' : text[at - 1]
    if (before && !/\s/.test(before)) { setMention(null); return }
    const after = slice.slice(at + 1)
    if (/\s/.test(after)) { setMention(null); return }
    if (after.length > 30) { setMention(null); return }
    setMention({ start: at, query: after })
    setMentionIndex(0)
  }

  /** Splice the partially-typed `@query` out of the serialized draft and
   *  replace it with `@<id> `. Then push the new value into RichInput,
   *  which re-serializes and lets `resolveMention` inflate the token
   *  into an avatar+name chip. Caret lands at the end (acceptable
   *  trade-off, matches desktop ChatPane behaviour). */
  const insertMention = (entry: MentionEntry) => {
    if (!mention) return
    const before = draft.slice(0, mention.start)
    const after = draft.slice(mention.start + 1 + mention.query.length)
    const token = entry.kind === 'all' ? 'all' : entry.p.id
    // Smart separator: if `before` doesn't already end in whitespace,
    // prepend one — happens when the new mention immediately follows
    // a previous mention chip (the typed `@` ends up between the chip
    // and its trailing space, leaving `before` as "@previd"). Without
    // this we'd serialize "@alice@bob" and inflate to two chips
    // squashed together.
    const sep = before && !/\s$/.test(before) ? ' ' : ''
    const insert = `${sep}@${token} `
    const next = `${before}${insert}${after}`.replace(/ {2,}$/, ' ')
    setDraft(next)
    setMention(null)
    editorRef.current?.setValue(next)
    requestAnimationFrame(() => editorRef.current?.focus())
  }

  /** Insert any string at the current caret position (used by the
   *  emoji picker). For Skype shortcodes we route through insertSkype
   *  so the editor renders the animated image inline; everything else
   *  is plain text. */
  const insertAtCursor = (s: string) => {
    const editor = editorRef.current
    if (!editor) return
    const skype = findSkypeByShortcode(s)
    if (skype) editor.insertSkype(skype.key)
    else editor.insertText(s)
    requestAnimationFrame(() => editor.focus())
  }

  /** Tap "@" toolbar button — insert an "@" at the caret to open the
   *  picker. RichInput's onChange will fire `updateMention` for us. */
  const openMentionByButton = () => {
    const editor = editorRef.current
    if (!editor) return
    const caret = editor.getCaretOffset()
    const value = editor.getValue()
    const prevChar = caret > 0 ? value[caret - 1] : ''
    // If we're not at the start of a word, inject a space first so the
    // mention rule (`@` must follow whitespace) actually triggers.
    const prefix = prevChar && !/\s/.test(prevChar) ? ' @' : '@'
    editor.insertText(prefix)
    requestAnimationFrame(() => editor.focus())
  }

  const send = () => {
    const v = draft.trim()
    if (!v && !attachment) return
    if (!convoId) return
    sendUserMessage(convoId, v, attachment, replyingToId ?? null)
    setDraft('')
    editorRef.current?.setValue('')
    setAttachment(null)
    setMention(null)
    setReplyingTo(convoId, null)
    void tapHaptic()
    editorRef.current?.focus()
    // Force-scroll to the new bottom. `followOutput="auto"` only
    // pins to the bottom when the user is already AT the bottom —
    // if they scrolled up to read history before sending, the auto-
    // follow won't fire and the user's own message lands off-screen.
    // Run on the next two frames so Virtuoso has measured the
    // newly-appended row (frame 1) before we scroll to it (frame 2).
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        const n = useMessages.getState().byConvo?.[convoId]?.length ?? 0
        if (n > 0) {
          virtuosoRef.current?.scrollToIndex({ index: n - 1, align: 'end', behavior: 'smooth' })
        }
      })
    })
  }
  const onKey = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (isImeComposing(e)) return

    // Mention picker keyboard nav. On mobile the picker is primarily
    // tap-driven, but external keyboards (iPad, Bluetooth) hit these.
    if (mention && filteredMentions.length > 0) {
      if (e.key === 'ArrowDown') { e.preventDefault(); setMentionIndex((i) => (i + 1) % filteredMentions.length); return }
      if (e.key === 'ArrowUp') { e.preventDefault(); setMentionIndex((i) => (i - 1 + filteredMentions.length) % filteredMentions.length); return }
      if (e.key === 'Enter' || e.key === 'Tab') {
        e.preventDefault()
        insertMention(filteredMentions[mentionIndex])
        return
      }
      if (e.key === 'Escape') { e.preventDefault(); setMention(null); return }
    }
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
    }
  }
  const canSend = (draft.trim().length > 0 || attachment !== null) && !uploading
  const memberPs = c.members
    .map((m) => byId[m])
    .filter((p): p is Participant => Boolean(p))
  const agents = memberPs.filter((p) => p.kind === 'agent')

  return (
    <section className="flex flex-col h-full bg-cloud overflow-x-hidden">
      {/* Header */}
      <ChatHeader
        c={c}
        agents={agents}
        select={select}
        pushStack={pushStack}
        onConvene={onConvene}
        conveneStarting={conveneStarting}
        menuOpen={menuOpen}
        setMenuOpen={setMenuOpen}
        toggleMute={toggleMute}
        muted={muted}
        leaveConvo={leaveConvo}
      />

      {/* Stream — virtualized via react-virtuoso. Long conversations no
          longer mount every bubble at once; rows outside the viewport
          stay un-rendered, and the older-history pager fires when the
          user scrolls past the top. */}
      <div
        ref={streamRef}
        className="flex-1 relative"
        style={{
          background: 'radial-gradient(ellipse 80% 40% at 0% 0%, rgba(194, 230, 251, 0.3), transparent 60%), radial-gradient(ellipse 60% 40% at 100% 100%, rgba(255, 217, 210, 0.25), transparent 60%), var(--cloud)',
        }}
      >
        <Virtuoso
          ref={virtuosoRef}
          className="h-full"
          data={list}
          firstItemIndex={firstItemIndex}
          // Pin to the bottom on first mount + every append. 'smooth' would
          // animate every WS streaming chunk and feel laggy; 'auto' jumps
          // for new messages while leaving the user free to scroll up.
          followOutput="auto"
          initialTopMostItemIndex={Math.max(0, list.length - 1)}
          startReached={onStartReached}
          // Padding lives inside Header / Footer so virtuoso can measure the
          // first/last items without a wrapping flex container fighting it.
          // STREAM_COMPONENTS is a module-level constant (stable identity) and
          // the dynamic header flags ride along on `context` — recreating this
          // object per render remounted the top-of-list node on every stream
          // tick and was a primary cause of scroll-up jitter. See StreamHeader.
          components={STREAM_COMPONENTS}
          context={streamCtx}
          itemContent={(_index, m) => {
            const author = byId[m.authorId]
            // System / whisper rows render without a resolved author (e.g. the
            // calendar-fired notice has a synthetic system author id).
            if (!author && m.kind !== 'system' && m.kind !== 'whisper-link') return <div className="h-0" />
            // Animate only freshly-arrived messages, not historical
            // rows being remounted as Virtuoso virtualizes the
            // scrollback. Without this gate, scrolling up replays a
            // fade on every row that re-enters the viewport.
            const createdAt = m.at ? new Date(m.at).getTime() : 0
            const animate = createdAt > convoOpenedAtRef.current
            return (
              <MessageRowMobileShell
                msg={m}
                author={author}
                animate={animate}
                onLongPress={(coords) => setTapback({ msg: m, coords })}
              />
            )
          }}
          computeItemKey={(_index, m) => m.clientId ?? m.id}
          // First-pass height estimate for rows Virtuoso hasn't measured yet.
          // A real mobile row (avatar + author line + a few lines of body, and
          // often a card) lands well above the old 88px guess, so every
          // unmeasured row used to "correct" from 88→real as it scrolled into
          // view — on iOS momentum scroll those corrections read as violent
          // jitter. 120 is closer to the median row, shrinking each correction.
          defaultItemHeight={120}
          // Mount + measure rows generously OFF-SCREEN before they're visible.
          // The bigger the TOP cushion, the more upward rows are already at
          // their true height by the time the user scrolls to them, so the
          // size correction settles out of view instead of shoving the row the
          // user is reading. This is the single most effective lever against
          // scroll-up jitter in a variable-height virtual list.
          increaseViewportBy={{ top: 2000, bottom: 800 }}
          atBottomStateChange={setAtBottom}
        />
        {/* Bottom-right "scroll to latest" pill — only when the user has
            scrolled up off the bottom. Sits just above the composer with a
            larger inset than desktop so it clears the soft-keyboard safe area. */}
        <ScrollToLatestButton visible={!atBottom} onClick={smoothScrollToLatest} bottomOffset={20} />
      </div>
      {/* Composer */}
      <Composer
        convoId={convoId}
        draft={draft}
        typingNames={typingNames}
        attachment={attachment}
        setAttachment={setAttachment}
        uploading={uploading}
        uploadError={uploadError}
        replyingToId={replyingToId}
        replyingToMsg={replyingToMsg}
        setReplyingTo={setReplyingTo}
        byId={byId}
        meId={meId}
        mention={mention}
        filteredMentions={filteredMentions}
        mentionIndex={mentionIndex}
        insertMention={insertMention}
        fileRef={fileRef}
        onPickFile={onPickFile}
        editorRef={editorRef}
        setDraft={setDraft}
        updateMention={updateMention}
        scrollToLatest={scrollToLatest}
        setMention={setMention}
        onKey={onKey}
        canSend={canSend}
        send={send}
        openMentionByButton={openMentionByButton}
        emojiOpen={emojiOpen}
        setEmojiOpen={setEmojiOpen}
        emojiTab={emojiTab}
        setEmojiTab={setEmojiTab}
        insertAtCursor={insertAtCursor}
      />
      <MobileMessageTapback
        open={tapback !== null}
        anchor={tapback?.coords ?? null}
        myReactions={tapback ? (tapback.msg.reactions ?? []).filter((r) => r.mine).map((r) => r.emoji) : []}
        onReact={(emoji) => {
          if (tapback) void toggleReaction(tapback.msg.id, emoji)
        }}
        actions={tapback ? buildMessageTapbackActions(tapback.msg, convoId, setReplyingTo, meId, t) : []}
        onClose={() => setTapback(null)}
      />
    </section>
  )
}
