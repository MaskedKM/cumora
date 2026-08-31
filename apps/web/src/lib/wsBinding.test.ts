// #220 ② — bindWsEvents 的门闸/连接/分发语义单测(评审 P3 补)。
// ws 模块整体 mock:本面只关心 connect/on 被怎么调,不关心真连接。
import { beforeEach, expect, test, vi } from 'vitest'

const connect = vi.fn()
const on = vi.fn()
vi.mock('@/api/client', () => ({ ws: { connect, on } }))

const { bindWsEvents } = await import('./wsBinding')

function listener(): (e: unknown) => void {
  expect(on).toHaveBeenCalled()
  return on.mock.calls[on.mock.calls.length - 1][0] as (e: unknown) => void
}

beforeEach(() => {
  connect.mockClear()
  on.mockClear()
})

test('首绑:connect 先于 on,返回 true;token 置位后二调直接 false 且零副作用', () => {
  const token = { bound: false }
  const first = bindWsEvents(token, {})
  expect(first).toBe(true)
  expect(connect).toHaveBeenCalledTimes(1)
  expect(on).toHaveBeenCalledTimes(1)
  const second = bindWsEvents(token, {})
  expect(second).toBe(false)
  expect(connect).toHaveBeenCalledTimes(1) // 未增
  expect(on).toHaveBeenCalledTimes(1) // 关键不变量:注册次数不变
})

test('connect:false 不发起连接(module 顶层三件的保真形态)', () => {
  bindWsEvents({ bound: false }, {}, { connect: false })
  expect(connect).not.toHaveBeenCalled()
  expect(on).toHaveBeenCalledTimes(1)
})

test('按 type 查表分发;未登记类型静默;fallback 兜底未登记帧', () => {
  const seen: string[] = []
  bindWsEvents({ bound: false }, {
    'participants.status': () => seen.push('status'),
  }, {
    fallback: () => seen.push('fallback'),
  })
  const l = listener()
  l({ type: 'participants.status' })
  expect(seen).toEqual(['status'])
  l({ type: 'boards' }) // 未登记且无对应 handler
  expect(seen).toEqual(['status', 'fallback'])
})

test('畸形 __proto__ 帧不炸 listener:静默走 fallback,不取 Object.prototype', () => {
  const seen: string[] = []
  bindWsEvents({ bound: false }, {}, { fallback: () => seen.push('fallback') })
  const l = listener()
  expect(() => l({ type: '__proto__' })).not.toThrow()
  expect(seen).toEqual(['fallback'])
})
