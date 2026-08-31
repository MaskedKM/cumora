// Store-level tests for the #220 message.new surgical patch — the sidebar
// row update that replaced one full-list HTTP refetch per incoming message.
// Covers the parts a regression would bite hardest: unread accounting
// (active vs background, own messages), recency re-ordering with the pinned
// block left intact, and the stale/replay guard.
import { beforeEach, expect, test, vi } from 'vitest'

vi.stubGlobal('window', {
  setTimeout: (fn: () => void, _ms?: number) => { queueMicrotask(fn); return 0 },
  clearTimeout: () => {},
})
vi.stubGlobal('localStorage', {
  getItem: () => null,
  setItem: () => {},
  removeItem: () => {},
})

import type { ApiMessage } from '@/api/client'
import type { Conversation } from '@/types'

const { useConversations, applyMessageEvent } = await import('./conversations')
const { useAuth } = await import('./auth')
const { useParticipants } = await import('./participants')

function msg(id: string, createdAt: string, body = 'hello', authorId = 'agent-1'): ApiMessage {
  return { id, createdAt, body, authorId, kind: 'text' } as ApiMessage
}

function row(id: string, lastAtIso: string, extra: Partial<Conversation> = {}): Conversation {
  return {
    id, kind: 'group', title: id, members: [], lastAt: lastAtIso, lastAtIso,
    preview: '', ...extra,
  } as Conversation
}

function seed(list: Conversation[]): void {
  useConversations.setState({ list })
}

beforeEach(() => {
  useAuth.setState({ user: { id: 'me-1' } as never })
  useParticipants.setState({ byId: {
    'agent-1': { id: 'agent-1', name: 'Iris' },
  } as never })
})

test('背景会话新消息:本地 bump unread + 更新预览与位置,零网络', () => {
  seed([
    row('c-hot', '2026-08-31T10:00:00Z'),
    row('c-target', '2026-08-31T09:00:00Z', { unread: 2 }),
    row('c-old', '2026-08-31T08:00:00Z'),
  ])
  applyMessageEvent('c-target', msg('m-9', '2026-08-31T09:30:00Z', 'fresh body'), false)
  const list = useConversations.getState().list
  // No reload fired — list identity is a fresh array but length and ids prove
  // the in-place path (a reload would need api mock and produce one entry per
  // server shape; here the untouched rows pass through by reference).
  expect(list.map((c) => c.id)).toEqual(['c-hot', 'c-target', 'c-old'])
  const target = list[1]
  expect(target.unread).toBe(3)
  expect(target.lastMessageId).toBe('m-9')
  expect(target.preview).toContain('Iris')
  expect(target.preview).toContain('fresh body')
})

test('活跃会话新消息:unread 清空(视为已读),不重复计数', () => {
  seed([row('c-active', '2026-08-31T09:00:00Z', { unread: 1 })])
  applyMessageEvent('c-active', msg('m-2', '2026-08-31T09:10:00Z'), true)
  expect(useConversations.getState().list[0].unread).toBeUndefined()
})

test('自己的消息不自我计数未读', () => {
  seed([row('c-me', '2026-08-31T09:00:00Z')])
  applyMessageEvent('c-me', msg('m-3', '2026-08-31T09:10:00Z', 'mine', 'me-1'), false)
  expect(useConversations.getState().list[0].unread).toBeUndefined()
})

test('时间序重排:新消息把行顶到未置顶区首;置顶块不动', () => {
  seed([
    row('c-pin', '2026-08-30T09:00:00Z', { pinned: true }),
    row('c-a', '2026-08-31T10:00:00Z'),
    row('c-b', '2026-08-31T08:00:00Z'),
  ])
  applyMessageEvent('c-b', msg('m-4', '2026-08-31T10:30:00Z'), false)
  const ids = useConversations.getState().list.map((c) => c.id)
  expect(ids).toEqual(['c-pin', 'c-b', 'c-a'])
  // pinned 行保持区块首,即便它更旧
  expect(useConversations.getState().list[0].pinned).toBe(true)
})

test('置顶会话收到消息:回到置顶块尾,不落进未置顶区', () => {
  seed([
    row('c-pin', '2026-08-30T09:00:00Z', { pinned: true }),
    row('c-a', '2026-08-31T10:00:00Z'),
  ])
  applyMessageEvent('c-pin', msg('m-5', '2026-08-31T11:00:00Z'), false)
  expect(useConversations.getState().list.map((c) => c.id)).toEqual(['c-pin', 'c-a'])
})

test('RFC3339Nano 尾零形态不破比较:.5Z 早于 .500000001Z(数值比,非字典序)', () => {
  seed([row('c-x', '2026-08-31T10:00:00.500000001Z', { lastMessageId: 'm-a' })])
  applyMessageEvent('c-x', msg('m-b', '2026-08-31T10:00:00.5Z'), false)
  // 字典序会误判 ".5Z" > ".500000001Z" 而放行;数值比较应识别为陈旧丢弃
  expect(useConversations.getState().list[0].lastMessageId).toBe('m-a')
})

test('陈旧/重放帧被丢弃:不回退行,不重复 bump', () => {
  seed([row('c-x', '2026-08-31T10:00:00Z', { unread: 1, lastMessageId: 'm-new' })])
  const before = useConversations.getState().list
  applyMessageEvent('c-x', msg('m-old', '2026-08-31T09:59:59Z'), false)
  expect(useConversations.getState().list).toBe(before) // 同引用 = 未动
  // 同时间戳的重放同样丢弃
  applyMessageEvent('c-x', msg('m-replay', '2026-08-31T10:00:00Z'), false)
  expect(useConversations.getState().list).toBe(before)
  expect(useConversations.getState().list[0].unread).toBe(1)
})

test('行未加载(他端新建会话竞态):回退一次 reload', async () => {
  const reloadCalls: unknown[][] = []
  useConversations.setState({
    list: [row('c-known', '2026-08-31T09:00:00Z')],
    reload: (async () => { reloadCalls.push([]) }) as never,
  })
  applyMessageEvent('c-unknown', msg('m-6', '2026-08-31T09:05:00Z'), false)
  await Promise.resolve()
  expect(reloadCalls.length).toBe(1)
  expect(useConversations.getState().list.map((c) => c.id)).toEqual(['c-known'])
})
