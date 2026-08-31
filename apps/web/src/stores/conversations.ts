import { create } from 'zustand'
import { api, ws, type ApiConversation, type ApiMessage } from '@/api/client'
import type { Conversation } from '@/types'
import { useApp } from '@/stores/app'
import { commitIfContextCurrent, useAuth } from '@/stores/auth'
import { useMessages } from '@/stores/messages'
import { useParticipants } from '@/stores/participants'

interface ConversationsState {
  list: Conversation[]
  loaded: boolean
  load: () => Promise<void>
  reload: () => Promise<void>
}

function timeFromIso(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  const now = new Date()
  if (d.toDateString() === now.toDateString()) {
    const h = String(d.getHours()).padStart(2, '0')
    const m = String(d.getMinutes()).padStart(2, '0')
    return `${h}:${m}`
  }
  if ((now.getTime() - d.getTime()) < 86400e3 * 2) return 'Yest'
  return `${d.getMonth() + 1}/${d.getDate()}`
}

/** Render a system-row JSON payload as a human-readable preview line.
 *  Returns null when the payload is unparseable so the caller can fall
 *  back to a generic '(system)' label instead of leaking raw JSON. */
function renderSystemPreview(
  raw: string,
  resolveName: (id: string) => string | null,
): string | null {
  try {
    const p = JSON.parse(raw) as { kind?: string; participantId?: string; actorId?: string; title?: string }
    if (p.kind === 'calendar_event') {
      const title = typeof p.title === 'string' && p.title.trim() ? p.title.trim() : 'Calendar event'
      return `Calendar fired: ${title}`
    }
    if (!p.kind || !p.participantId) return null
    const subject = resolveName(p.participantId) ?? p.participantId
    const actor = p.actorId ? (resolveName(p.actorId) ?? p.actorId) : null
    switch (p.kind) {
      case 'joined':
        return actor && actor !== subject
          ? `${actor} added ${subject} to the group`
          : `${subject} joined the group`
      case 'left':
        return `${subject} left the group`
      case 'kicked':
        return actor
          ? `${actor} removed ${subject} from the group`
          : `${subject} was removed from the group`
      default:
        return `${subject} — ${p.kind}`
    }
  } catch {
    return null
  }
}

/** previewForMessage 的入参面:事件载荷 ApiMessage(message.new)与列表
 *  端点 lastMessage 的共同结构子集——两者字段兼容但类型不同形
 *  (lastMessage 无 conversationId/sequence,事件侧 createdAt 可选)。 */
interface PreviewMessage {
  authorId: string
  kind: string
  body: string
  tool?: unknown
  email?: { direction?: string; subject?: string | null } | null
  attachment?: { name?: string | null; kind?: string } | null
}

/** Sidebar preview line for a message — the single derivation shared by
 *  fromApi (full reload) and the message.new surgical patch (#220), so a
 *  patched row is byte-identical to what a reload would have produced.
 *  Empty string when there's no message yet — the row renderer skips the
 *  preview line entirely so an empty conversation doesn't show a stray "—". */
function previewForMessage(last: PreviewMessage): string {
  let preview = ''
  {
    // Resolve the author's DISPLAY NAME from the participants store. If the
    // participant isn't loaded yet, omit the author prefix entirely — never
    // fall back to the raw id (which may carry a server-side collision
    // suffix like `iris-a0ab` that we shouldn't be exposing to UI).
    const meId = useAuth.getState().user?.id
    const byId = useParticipants.getState().byId
    const resolveName = (id: string): string | null => {
      if (id === meId) return 'You'
      return byId[id]?.name ?? null
    }
    const authorName = resolveName(last.authorId)
    // Rewrite raw `@<id>` mention tokens to `@<DisplayName>` so previews
    // don't leak participant ids like `@u-27c9522d-3b7`. Mirror the regex
    // used by parseBody() so we tokenize exactly what the chat renderer
    // treats as a mention. `@all` is preserved as-is.
    const humanizeMentions = (s: string): string =>
      s.replace(/@[A-Za-z][\w-]*/g, (m) => {
        const id = m.slice(1)
        if (id === 'all') return m
        const name = resolveName(id)
        return name ? `@${name}` : m
      })
    const trimmedBody = humanizeMentions(last.body?.trim() ?? '')
    if (last.kind === 'tool' && last.tool) {
      const t = last.tool as { name?: string; arg?: string }
      preview = authorName
        ? `${authorName}: used ${t.name ?? 'tool'}`
        : `used ${t.name ?? 'tool'}`
    } else if (last.kind === 'email' && last.email) {
      // Subject leads — that's how mailbox apps preview a thread. Body
      // excerpt follows in muted text only when there's room.
      const arrow = last.email.direction === 'in' ? '↓' : '↑'
      const subject = last.email.subject || '(no subject)'
      const snippet = trimmedBody ? ` — ${trimmedBody.slice(0, 60)}` : ''
      preview = `${arrow} ${subject}${snippet}`
    } else if (last.kind === 'system') {
      // System rows ship as JSON bodies — translate to a short
      // human-readable line ("Bram joined the group" / "Scout removed
      // Iris") instead of leaking raw `{"kind":"left",...}` payloads.
      preview = renderSystemPreview(last.body, resolveName) ?? '(system)'
    } else if (last.attachment) {
      // Attachment messages (user uploads) — the row stores attachment
      // metadata in a separate jsonb column and `body` carries only the
      // optional caption. Prefix with a 📎 + filename so the row reads
      // as "shared a file" instead of going blank when the user uploads
      // without a caption.
      const a = last.attachment
      const verb = a.kind === 'img' ? '📷' : '📎'
      const filename = a.name ?? (a.kind === 'img' ? 'image' : 'file')
      const label = trimmedBody
        ? `${verb} ${filename} — ${trimmedBody.slice(0, 80)}`
        : `${verb} ${filename}`
      preview = authorName ? `${authorName}: ${label}` : label
    } else if (trimmedBody) {
      preview = authorName ? `${authorName}: ${trimmedBody.slice(0, 100)}` : trimmedBody.slice(0, 100)
    }
  }
  return preview
}

function fromApi(c: ApiConversation): Conversation {
  const last = c.lastMessage
  const preview = last ? previewForMessage(last) : ''
  return {
    id: c.id,
    kind: c.kind,
    title: c.title,
    subtitle: c.subtitle ?? undefined,
    topic: c.topic ?? null,
    members: c.members,
    pinned: c.pinned,
    muted: c.muted,
    mutedUntil: c.mutedUntil,
    unread: c.unreadCount > 0 ? c.unreadCount : undefined,
    lastMessageId: last?.id ?? null,
    lastAt: timeFromIso(last?.createdAt ?? c.updatedAt),
    lastAtIso: last?.createdAt ?? c.updatedAt,
    preview,
    tag: (c.tag ?? undefined) as Conversation['tag'],
    pulledBy: c.pulledBy ?? undefined,
    projectId: c.projectId,
    projectName: c.projectName,
    projectColor: c.projectColor,
  }
}

function refreshActiveMessagesIfSidebarMoved(conversations: Conversation[]): void {
  const active = useApp.getState().selectedConversationId
  if (!active) return
  const activeConvo = conversations.find((c) => c.id === active)
  const lastMessageId = activeConvo?.lastMessageId
  if (!lastMessageId) return

  const messages = useMessages.getState()
  if (messages.loading.has(active)) return
  const cached = messages.byConvo[active]
  if (!cached && !messages.loaded.has(active)) return
  if (cached?.some((m) => m.id === lastMessageId)) return

  void messages.reloadConversation(active)
}

/**
 * "Effective" mute state: server says muted=true, AND any per-row expiry
 * hasn't lapsed since the last list reload. The server already filters
 * expired mutes out, but if the user keeps the app open past the expiry
 * (e.g. muted "for 15 min" and no traffic happens for 20), the local
 * cached row would still claim muted=true. Recompute against `now` so the
 * silence wears off without waiting for the next WS-triggered reload.
 */
export function isMuted(c: Pick<Conversation, 'muted' | 'mutedUntil'>): boolean {
  if (!c.muted) return false
  if (!c.mutedUntil) return true  // muted forever
  return new Date(c.mutedUntil).getTime() > Date.now()
}

export const useConversations = create<ConversationsState>((set) => ({
  list: [],
  loaded: false,
  async load() {
    // Clear stale data immediately so a workspace switch never shows the
    // previous tenant's conversations during the loading window.
    set({ list: [], loaded: false })
    try {
      await commitIfContextCurrent(() => api.getConversations(), (list) => {
        const conversations = list.map(fromApi)
        set({ list: conversations, loaded: true })
        refreshActiveMessagesIfSidebarMoved(conversations)
      })
    } catch (err) {
      console.warn('[conversations] load failed', err)
    }
  },
  async reload() {
    try {
      await commitIfContextCurrent(() => api.getConversations(), (list) => {
        const conversations = list.map(fromApi)
        set({ list: conversations })
        refreshActiveMessagesIfSidebarMoved(conversations)
      })
    } catch (err) {
      console.warn('[conversations] reload failed', err)
    }
  },
}))

/** Apply a message.new to the sidebar row in place (#220): preview /
 *  lastMessageId / lastAt* refresh, unread bumps only when the user isn't
 *  reading the conversation, and the row re-takes its position under the
 *  server's list order (pinned first, then recency — conversations.go
 *  `ORDER BY c.pinned DESC, c.updated_at DESC`). Falls back to one reload
 *  when the row isn't loaded yet (a patch can't invent the row).
 *  Exported for conversations.patch.test.ts. */
export function applyMessageEvent(conversationId: string, m: ApiMessage, isActive: boolean): void {
  const meId = useAuth.getState().user?.id
  const bumpUnread = !isActive && m.authorId !== meId
  // 事件侧 createdAt 类型可选;实况恒在,缺省回退"现在"——活事件的诚实
  // 时间戳(比丢弃或纪元零好,零值会被守卫误判为陈旧帧)。
  const createdAt = m.createdAt ?? new Date().toISOString()
  useConversations.setState((s) => {
    const idx = s.list.findIndex((c) => c.id === conversationId)
    if (idx === -1) {
      void useConversations.getState().reload()
      return s
    }
    const prev = s.list[idx]
    // Out-of-order or replayed frame (hello backfill races a live event):
    // message ids are random server-side tokens (not orderable), so recency
    // is judged on createdAt — never move the row backwards, and an equal
    // timestamp (the replay) must not double-bump unread.
    if (prev.lastAtIso >= createdAt) return s
    const next: Conversation = {
      ...prev,
      preview: previewForMessage(m),
      lastMessageId: m.id,
      lastAt: timeFromIso(createdAt),
      lastAtIso: createdAt,
      // Reading it live → treated as seen (server markRead already fired);
      // own messages from another surface never self-unread.
      unread: bumpUnread ? (prev.unread ?? 0) + 1 : undefined,
    }
    const list = s.list.filter((c) => c.id !== conversationId)
    let insertAt: number
    if (next.pinned) {
      // Pinned rows keep their top block; a pinned conversation that just
      // received a message returns to the END of that block (recency
      // doesn't reorder inside the block — the server sort doesn't either).
      insertAt = list.findIndex((c) => !c.pinned)
      if (insertAt === -1) insertAt = list.length
    } else {
      // Unpinned: first row (pinned gap aside) that is older-or-equal to
      // this message; fall through to append when everything is newer.
      insertAt = list.findIndex((c) => !c.pinned)
      if (insertAt === -1) insertAt = list.length
      for (let i = insertAt; i < list.length; i++) {
        if (list[i].lastAtIso <= createdAt) { insertAt = i; break }
        insertAt = i + 1
      }
    }
    list.splice(insertAt, 0, next)
    return { list }
  })
}

// WS bindings are attached once for the page lifetime; data reload runs
// on every call so workspace switches (App.tsx remounts the tree on
// companyId change) pick up the new tenant's data.
let wsBound = false
export function bootConversations() {
  void useConversations.getState().load()
  if (wsBound) return
  wsBound = true
  ws.connect()
  ws.on((e) => {
    if (e.type === 'hello') {
      // WS (re)connected — Redis pubsub didn't queue events for the gap,
      // so any `message.new` / `group.pulled` / `conversation.updated`
      // that fired while we were disconnected is gone. Refetch the list
      // so last-message previews and unread badges backfill without a
      // manual page refresh.
      void useConversations.getState().reload()
      return
    }
    if (e.type === 'message.new' || e.type === 'group.pulled') {
      // message.new patches the row in place (#220): with several agents
      // replying concurrently, one HTTP list refetch per message was a
      // request storm with full-list re-renders. The open conversation
      // keeps its server read-state in step via markRead alone (no
      // refetch — the transcript itself lives in the messages store).
      const active = useApp.getState().selectedConversationId
      if (e.type === 'message.new') {
        const isActive = e.conversationId === active
        if (isActive) {
          // Treated as already-seen: mark read BEFORE the patch so the
          // badge never blinks up to 1 just to drop back to 0.
          void api.markRead(e.conversationId).catch(() => { /* offline: next open retries */ })
        }
        applyMessageEvent(e.conversationId, e.message, isActive)
        return
      }
      // group.pulled can introduce a conversation the client has never
      // loaded — a patch can't invent the row, so this stays a reload.
      void useConversations.getState().reload()
    } else if (e.type === 'conversation.updated') {
      // Surgical patch — apply patch fields to the matching conversation in
      // place without a full network reload.
      useConversations.setState((s) => ({
        list: s.list.map((c) => {
          if (c.id !== e.conversationId) return c
          const next: Conversation = { ...c }
          if (e.patch.topic !== undefined) next.topic = e.patch.topic
          if (e.patch.title !== undefined) next.title = e.patch.title
          return next
        }),
      }))
    }
  })
}
