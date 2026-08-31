/**
 * Frontend "Whispers" tab — the user's peek view into agent-to-agent
 * private 1-on-1 chats. On the server these are just regular
 * `kind='direct'` conversations between two agents; the user isn't a
 * member, so they don't appear in /conversations. This store fetches
 * the peek view (/peek/agent-chats) and listens for new messages on
 * those conversations.
 *
 * "Whisper" is purely a frontend concept — naming, tab, UI. The server
 * has no notion of it.
 */
import { create } from 'zustand'
import { api, type ApiWhisper, type ApiWhisperMessage } from '@/api/client'
import { commitIfContextCurrent } from '@/stores/auth'
import { bindWsEvents } from '@/lib/wsBinding'

export interface WhispersState {
  list: ApiWhisper[]
  /** message arrays keyed by conversation id (the "whisper id" in this UI). */
  byId: Record<string, ApiWhisperMessage[]>
  /** Reserved for future inline streaming; kept for backward compat with
   *  `whisperMessages` helper. Currently always empty. */
  streaming: Record<string, { body: string; whisperId: string; authorId: string }>
  loaded: boolean
  /** Hard load — clears state first. Used at boot / workspace switch. */
  loadList: () => Promise<void>
  /** Quiet refetch — keeps the per-conversation message cache so the
   *  conversation a user is currently reading doesn't flash empty. */
  refreshList: () => Promise<void>
  loadMessages: (id: string) => Promise<void>
}

export type WhispersStateLike = Pick<WhispersState, 'byId' | 'streaming'>

export const useWhispers = create<WhispersState>((set) => ({
  list: [],
  byId: {},
  streaming: {},
  loaded: false,

  async loadList() {
    // Reset on every call — a workspace switch must drop the previous
    // tenant's peek list + per-pair message cache before fetching fresh.
    set({ list: [], byId: {}, streaming: {}, loaded: false })
    try {
      await commitIfContextCurrent(
        () => api.getWhispers(),
        (list) => set({ list, loaded: true }),
      )
    } catch (err) {
      console.warn('[whispers] loadList failed', err)
    }
  },

  async refreshList() {
    // No state clear — fetches the list and replaces it in place. WS
    // handlers call this so the per-conversation `byId` cache doesn't
    // get wiped (which would flash the open thread empty).
    try {
      await commitIfContextCurrent(
        () => api.getWhispers(),
        (list) => set({ list, loaded: true }),
      )
    } catch (err) {
      console.warn('[whispers] refreshList failed', err)
    }
  },

  async loadMessages(id) {
    try {
      await commitIfContextCurrent(
        () => api.getWhisperMessages(id),
        (msgs) => set((s) => ({ byId: { ...s.byId, [id]: msgs } })),
      )
    } catch (err) {
      console.warn('[whispers] loadMessages failed', err)
    }
  },
}))

// Data reload runs on every call; WS binding only attaches once.
/** WS 绑定 token —— 与旧 `wsBound` 布尔同一守护语义(#220 ②)。 */
const wsToken = { bound: false }
export function bootWhispers() {
  void useWhispers.getState().loadList()
  bindWsEvents(wsToken, {
    hello: () => {
      // WS (re)connected — Redis pubsub didn't queue events for the gap,
      // so any `message.new` on a peeked agent↔agent thread is gone.
      // Refresh the list (covers msgCount / updatedAt) and any per-thread
      // message caches we've already opened.
      void useWhispers.getState().refreshList()
      const opened = Object.keys(useWhispers.getState().byId)
      for (const id of opened) void useWhispers.getState().loadMessages(id)
    },
    'message.new': (e) => {
      // Only react if this new message belongs to a conversation we're
      // already tracking as a "whisper" (agent ↔ agent). If yes, append it
      // and refresh the list so msgCount / updatedAt stay current.
      const list = useWhispers.getState().list
      const isPeekable = list.some((w) => w.id === e.conversationId)
      if (!isPeekable) {
        // New conversation might have just been created (an agent kicked
        // off a fresh DM with another agent). Refetch the list so it
        // appears in the peek tab on the next read.
        void useWhispers.getState().refreshList()
        return
      }
      // Normalize timestamp shape — WS payload uses `at`, the HTTP shape
      // (and the Bubble renderer) reads `createdAt`.
      const m = e.message
      const rawAt = (m as { at?: string; createdAt?: string }).at
      const wm: ApiWhisperMessage = {
        id: m.id,
        conversationId: e.conversationId,
        authorId: m.authorId,
        kind: m.kind,
        body: m.body,
        sequence: m.sequence,
        tool: (m as { tool?: ApiWhisperMessage['tool'] }).tool ?? null,
        createdAt: (m as { createdAt?: string }).createdAt ?? rawAt ?? new Date().toISOString(),
      }
      useWhispers.setState((s) => {
        const existing = s.byId[e.conversationId] ?? []
        if (existing.some((x) => x.id === wm.id)) return {}
        const next = [...existing, wm].sort((a, b) => a.sequence - b.sequence)
        return { byId: { ...s.byId, [e.conversationId]: next } }
      })
      // Refresh the list so msgCount + updated_at order are accurate.
      // Use refreshList (not loadList) — loadList wipes byId, which would
      // flash the open thread empty until the refetch completes.
      void useWhispers.getState().refreshList()
    },
  })
}

export function whisperMessages(s: WhispersStateLike, whisperId: string): ApiWhisperMessage[] {
  // streaming was used by the old per-token whisper LLM loop; the unified
  // message path is atomic so this returns the base list verbatim.
  return s.byId[whisperId] ?? []
}
