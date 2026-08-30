// #147 ① 共享骨架的行为钉:两面差异(头注入/401 语义)与统一件(origin
// 三层、错误 detail 解析、base 前缀、setServerOrigin 清 session)。
import { expect, test, vi, beforeEach } from 'vitest'

const h = vi.hoisted(() => ({
  token: null as string | null,
  company: null as string | null,
  clear: vi.fn(),
}))
vi.mock('@/stores/auth', () => ({
  getAuthToken: () => h.token,
  getActiveCompanyId: () => h.company,
  useAuth: { getState: () => ({ clear: h.clear }) },
}))

const store = vi.hoisted(() => new Map<string, string>())
vi.stubGlobal('localStorage', {
  getItem: (k: string) => store.get(k) ?? null,
  setItem: (k: string, v: string) => void store.set(k, v),
  removeItem: (k: string) => void store.delete(k),
})

const { fetchJson, setServerOrigin, resolveServerOrigin, ApiError } = await import('./core')

/** fetch 桩:记录 (url, headers),按脚本回 Response。 */
function stubFetch(status: number, body: string, headers?: Record<string, string>) {
  const calls: Array<{ url: string; headers: Record<string, string> }> = []
  vi.stubGlobal('fetch', vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, headers: (init?.headers ?? {}) as Record<string, string> })
    return new Response(body, { status, headers })
  }))
  return calls
}

beforeEach(() => {
  h.token = null
  h.company = null
  h.clear.mockClear()
  store.clear()
  // fetch 桩由各测试的 stubFetch 覆盖式重装;localStorage 桩是模块级的,
  // 不能 unstubAllGlobals(会把 localStorage 一起拆掉)。
})

test('origin: localStorage override wins, trailing slash trimmed', () => {
  store.set('cumora.serverUrl', 'http://10.0.0.5:5181///')
  expect(resolveServerOrigin()).toBe('http://10.0.0.5:5181')
})

test('origin: falls back to "" when nothing set (relative URLs)', () => {
  expect(resolveServerOrigin()).toBe('')
})

test('headers: Bearer + x-company-id + devtools only on the main face', async () => {
  h.token = 'tk'
  h.company = 'co1'
  store.set('cumora.devtools.enabled', '1')
  const calls = stubFetch(200, '{"ok":true}')
  await fetchJson('/x', undefined, { base: '/api', companyHeader: true, devModeHeader: true })
  expect(calls[0].headers.authorization).toBe('Bearer tk')
  expect(calls[0].headers['x-company-id']).toBe('co1')
  expect(calls[0].headers['x-cumora-dev-mode']).toBe('1')

  // admin 面:无公司/devtools 头(devtools 开着也不带)
  const calls2 = stubFetch(200, '{"ok":true}')
  await fetchJson('/x', undefined, { base: '/api/admin' })
  expect(calls2[0].headers.authorization).toBe('Bearer tk')
  expect(calls2[0].headers['x-company-id']).toBeUndefined()
  expect(calls2[0].headers['x-cumora-dev-mode']).toBeUndefined()
})

test('base prefix: each face hits its own path space', async () => {
  const calls = stubFetch(200, '{}')
  await fetchJson('/stats', undefined, { base: '/api/admin' })
  expect(calls[0].url).toBe('/api/admin/stats')
})

test('401: main face exempts /auth/ paths, admin face always clears', async () => {
  stubFetch(401, '{"error":"bad credentials"}')
  await expect(
    fetchJson('/auth/me', undefined, { base: '/api', clear401: (p) => !p.startsWith('/auth/') }),
  ).rejects.toThrow(ApiError)
  expect(h.clear).not.toHaveBeenCalled()

  stubFetch(401, '{"error":"unauthorized"}')
  await expect(fetchJson('/me', undefined, { base: '/api/admin' })).rejects.toThrow(ApiError)
  expect(h.clear).toHaveBeenCalledTimes(1)

  // 主面非 auth 路径(最高频默认路径):401 必须清 session 回登录屏。
  stubFetch(401, '{"error":"expired"}')
  await expect(
    fetchJson('/conversations', undefined, { base: '/api', clear401: (p) => !p.startsWith('/auth/') }),
  ).rejects.toThrow(ApiError)
  expect(h.clear).toHaveBeenCalledTimes(2)
})

test('error detail: {error} first, {message} fallback, text snippet, then status-only', async () => {
  stubFetch(400, '{"error":"title required"}')
  await expect(fetchJson('/x', undefined, { base: '/api' })).rejects.toMatchObject({
    message: 'title required (400)',
    status: 400,
  })

  stubFetch(502, '{"message":"upstream down"}')
  await expect(fetchJson('/x', undefined, { base: '/api' })).rejects.toThrow('upstream down (502)')

  stubFetch(500, 'plain text oops')
  await expect(fetchJson('/x', undefined, { base: '/api' })).rejects.toThrow('plain text oops (500)')

  stubFetch(418, '')
  await expect(fetchJson('/x', undefined, { base: '/api' })).rejects.toMatchObject({ status: 418 })
})

test('setServerOrigin: persists trimmed origin / clears session / null drops override', () => {
  setServerOrigin('http://lan-box:5181/')
  expect(store.get('cumora.serverUrl')).toBe('http://lan-box:5181')
  expect(h.clear).toHaveBeenCalledTimes(1)
  setServerOrigin(null)
  expect(store.has('cumora.serverUrl')).toBe(false)
  expect(h.clear).toHaveBeenCalledTimes(2)
})
