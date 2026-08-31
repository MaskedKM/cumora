// Store-level tests for the #220 message.new surgical patch — the sidebar
// row update that replaced one full-list HTTP refetch per incoming message.
// Event payloads use the REAL WS shape: the time key is `at` (contract:
// Message.at — "仅 WS message.new 事件携带;REST 列表用 createdAt"), which
// the first version of the guard got wrong — these tests now pin that.
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

// 真实 WS 事件形状:时间键是 at,无 createdAt。
function msg(id: string, at: string, body = 'hello', authorId = 'agent-1'): ApiMessage {
  return { id, at, body, authorId, kind: 'text' } as ApiMessage
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
  expect(list.map((c) => c.id)).toEqual(['c-hot', 'c-target', 'c-old'])
  const target = list[1]
  expect(target.unread).toBe(3)
  expect(target.lastMessageId).toBe('m-9')
  expect(target.preview).toContain('Iris')
  expect(target.preview).toContain('fresh body')
  // 时间键来自事件的 at(而非本地时钟)
  expect(target.lastAtIso).toBe('2026-08-31T09:30:00Z')
})

test('活跃会话新消息:unread 清空(视为已读)', () => {
  seed([row('c-active', '2026-08-31T09:00:00Z', { unread: 1 })])
  applyMessageEvent('c-active', msg('m-2', '2026-08-31T09:10:00Z'), true)
  expect(useConversations.getState().list[0].unread).toBeUndefined()
})

test('本人消息不自我计数,但保留此前他人的未读', () => {
  seed([row('c-me', '2026-08-31T09:00:00Z', { unread: 3 })])
  applyMessageEvent('c-me', msg('m-3', '2026-08-31T09:10:00Z', 'mine', 'me-1'), false)
  expect(useConversations.getState().list[0].unread).toBe(3)
})

test('时间序重排:新消息把行顶到未置顶区首;置顶块在前', () => {
  seed([
    row('c-pin', '2026-08-30T09:00:00Z', { pinned: true }),
    row('c-a', '2026-08-31T10:00:00Z'),
    row('c-b', '2026-08-31T08:00:00Z'),
  ])
  applyMessageEvent('c-b', msg('m-4', '2026-08-31T10:30:00Z'), false)
  expect(useConversations.getState().list.map((c) => c.id)).toEqual(['c-pin', 'c-b', 'c-a'])
  expect(useConversations.getState().list[0].pinned).toBe(true)
})

test('置顶会话收到消息:按 updated_at DESC 提到置顶块首(对齐服务端第二键)', () => {
  seed([
    row('c-pin-a', '2026-08-30T09:00:00Z', { pinned: true }),
    row('c-pin-b', '2026-08-30T08:00:00Z', { pinned: true }),
    row('c-x', '2026-08-31T10:00:00Z'),
  ])
  applyMessageEvent('c-pin-b', msg('m-5', '2026-08-31T11:00:00Z'), false)
  expect(useConversations.getState().list.map((c) => c.id)).toEqual(['c-pin-b', 'c-pin-a', 'c-x'])
})

test('RFC3339Nano 尾零形态安全:亚毫秒差异塌缩为同毫秒,不回退不错序', () => {
  seed([row('c-x', '2026-08-31T10:00:00.500000001Z', { unread: 1, lastMessageId: 'm-a' })])
  applyMessageEvent('c-x', msg('m-b', '2026-08-31T10:00:00.5Z'), false)
  // Date.parse 只有毫秒精度:.5 与 .500000001 塌缩相等,不同 id ⇒ 当真消息
  // 应用(纯字符串字典序会把 ".5Z" 判成更新而放行、把 ".500000001Z" 判成
  // 更旧——数值比较避免的是这种跨可见量级的判反;亚毫秒内的先后本就
  // 不可分辨,应用无害,下一帧会纠正)。
  const c = useConversations.getState().list[0]
  expect(c.lastMessageId).toBe('m-b')
  expect(c.unread).toBe(2)
  // 塌缩相等 ⇒ 不重排(同毫秒保持既位)
  expect(useConversations.getState().list.map((r) => r.id)).toEqual(['c-x'])
})

test('同毫秒不同消息不是重放:照常补丁与计数', () => {
  seed([row('c-x', '2026-08-31T10:00:00.123Z', { unread: 1, lastMessageId: 'm-first', preview: 'old' })])
  applyMessageEvent('c-x', msg('m-second', '2026-08-31T10:00:00.123Z', 'same ms'), false)
  const c = useConversations.getState().list[0]
  expect(c.lastMessageId).toBe('m-second')
  expect(c.unread).toBe(2)
  expect(c.preview).toContain('same ms')
})

test('同 id 同时间戳才是重放:丢弃不重复 bump', () => {
  seed([row('c-x', '2026-08-31T10:00:00Z', { unread: 1, lastMessageId: 'm-new' })])
  const before = useConversations.getState().list
  applyMessageEvent('c-x', msg('m-old', '2026-08-31T09:59:59Z'), false)
  expect(useConversations.getState().list).toBe(before) // 陈旧:同引用 = 未动
  applyMessageEvent('c-x', msg('m-new', '2026-08-31T10:00:00Z'), false) // 同 id 同时刻 = 重放
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
