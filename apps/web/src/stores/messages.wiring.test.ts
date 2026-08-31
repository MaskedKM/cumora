// Store-level wiring tests for the #143 streaming coalescing — the glue
// between StreamingDeltaBatch and useMessages that the pure-module tests
// can't see (drop/clear on terminal events, the 64KB cap valve, and
// messagesFor's bubble identity cache). Runs in node; the only browser
// global the touched paths use is window.setTimeout for typing expiry,
// stubbed below.
import { expect, test, vi } from 'vitest'

vi.stubGlobal('window', {
  setTimeout: (fn: () => void, _ms?: number) => { queueMicrotask(fn); return 0 },
  clearTimeout: () => {},
})
// stores/auth reads the token at module load; a bare storage shim keeps
// the import graph alive in node.
vi.stubGlobal('localStorage', {
  getItem: () => null,
  setItem: () => {},
  removeItem: () => {},
})

// messages.ts pulls @/api/client; the WsClient constructor is inert until
// connect(), but the module reads import.meta.env — keep vitest's default
// node environment and import the store after the window stub.
import type { WsEvent } from '@/api/client'

const { useMessages, flushStreamingDeltas, messagesFor } = await import('./messages')

function delta(messageId: string, text: string, sequence = 1, done = false): WsEvent {
  return { type: 'message.delta', conversationId: 'c1', messageId, authorId: 'a1', delta: text, sequence, done } as WsEvent
}

function reset() {
  useMessages.setState({ byConvo: {}, streaming: {}, typing: {}, loaded: new Set(), loading: new Set(), errors: {} })
}

test('deltas buffer and flush as one entry with accumulated body', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('m1', 'Hel', 1))
  apply(delta('m1', 'lo ', 2))
  apply(delta('m1', 'world', 3))
  // Nothing applied before the flush — the whole point of the coalescer.
  expect(useMessages.getState().streaming).toEqual({})
  flushStreamingDeltas()
  expect(useMessages.getState().streaming.m1).toEqual({
    body: 'Hello world', conversationId: 'c1', authorId: 'a1', sequence: 3,
  })
})

test('message.new drops the pending tail — no resurrection on later flush', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('m1', 'partial'))
  apply({
    type: 'message.new', conversationId: 'c1',
    message: { id: 'm1', conversationId: 'c1', authorId: 'a1', kind: 'text', body: 'full final body' },
  } as unknown as WsEvent)
  flushStreamingDeltas()
  expect(useMessages.getState().streaming).toEqual({})
  expect(useMessages.getState().byConvo.c1.map((m) => m.body)).toEqual(['full final body'])
})

test('done=true drops the tail and retires the streaming entry', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('m1', 'abc'))
  flushStreamingDeltas()
  expect(useMessages.getState().streaming.m1).toBeDefined()
  apply(delta('m1', 'tail', 9, true))
  flushStreamingDeltas()
  expect(useMessages.getState().streaming).toEqual({})
})

test('oversized buffer trips the cap valve without an explicit flush', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('m1', 'x'.repeat(65 * 1024)))
  expect(useMessages.getState().streaming.m1.body).toHaveLength(65 * 1024)
})

test('messagesFor hands back the SAME bubble object until a flush replaces the entry', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('m1', 'one'))
  flushStreamingDeltas()
  const state = useMessages.getState()
  const first = messagesFor(state, 'c1').find((m) => m.id === 'm1')
  const again = messagesFor(useMessages.getState(), 'c1').find((m) => m.id === 'm1')
  expect(again).toBe(first)
  apply(delta('m1', 'two'))
  flushStreamingDeltas()
  const grown = messagesFor(useMessages.getState(), 'c1').find((m) => m.id === 'm1')
  expect(grown).not.toBe(first)
  expect(grown?.body).toBe('onetwo')
})

// ── #210:daemon 铸的流 id 与终局消息 id 不配对——收口必须按
// (conversationId, authorId),否则终局后残留一条重复的瞬态气泡。──
test('message.new from the same author retires a daemon-id transient (id never matches)', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('ds-abc123', 'composing the reply…'))
  flushStreamingDeltas()
  expect(useMessages.getState().streaming['ds-abc123']).toBeDefined()
  apply({
    type: 'message.new', conversationId: 'c1',
    message: { id: 'm-real', conversationId: 'c1', authorId: 'a1', kind: 'text', body: 'final body' },
  } as unknown as WsEvent)
  flushStreamingDeltas()
  expect(useMessages.getState().streaming).toEqual({})
  const list = messagesFor(useMessages.getState(), 'c1')
  expect(list.map((m) => m.body)).toEqual(['final body'])
})

test('message.new from a DIFFERENT author leaves the transient alone', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('ds-abc123', 'agent is composing'))
  flushStreamingDeltas()
  apply({
    type: 'message.new', conversationId: 'c1',
    message: { id: 'm-other', conversationId: 'c1', authorId: 'someone-else', kind: 'text', body: 'not my stream' },
  } as unknown as WsEvent)
  flushStreamingDeltas()
  expect(useMessages.getState().streaming['ds-abc123']).toBeDefined()
})

test('synthesized streaming bubbles carry the streaming render flag', () => {
  reset()
  const apply = useMessages.getState().applyEvent
  apply(delta('ds-flag', 'live prefix'))
  flushStreamingDeltas()
  const bubble = messagesFor(useMessages.getState(), 'c1').find((m) => m.id === 'ds-flag')
  expect(bubble?.streaming).toBe(true)
})

test('a completed message in byConvo hides a same-id streaming entry', () => {
  reset()
  useMessages.setState({
    byConvo: { c1: [{ id: 'm1', conversationId: 'c1', authorId: 'a1', kind: 'text', body: 'done', at: '12:00' } as never] },
    streaming: { m1: { body: 'zombie', conversationId: 'c1', authorId: 'a1', sequence: 1 } },
  })
  const list = messagesFor(useMessages.getState(), 'c1')
  expect(list.filter((m) => m.id === 'm1')).toHaveLength(1)
  expect(list.find((m) => m.id === 'm1')?.body).toBe('done')
})
