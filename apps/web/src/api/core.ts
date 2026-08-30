/**
 * 双 API client(主面 ./client.ts / admin 面 ../admin/api.ts)的共享
 * 骨架(#147 ①):server origin 三层解析、Bearer 注入、401 清 session、
 * 错误 detail 解析。此前两份整段复制且各自漂移(admin 401 恒清无豁免、
 * 错误无 {message} 回退、origin 每次调用重解析)——统一进这里,两面差异
 * 经 fetchJson 的 opts 显式表达。
 */
import { getAuthToken, getActiveCompanyId, useAuth } from '@/stores/auth'

export const SERVER_URL_KEY = 'cumora.serverUrl'
const DEVTOOLS_KEY = 'cumora.devtools.enabled'

/** Resolve the API base. Three layers, highest priority first:
 *    1. localStorage['cumora.serverUrl'] — runtime override, settable
 *       from the dev console: `localStorage.setItem('cumora.serverUrl',
 *       'http://192.168.1.10:5181')`. Lets a packaged build switch
 *       endpoints without rebuilding (e.g. server on another LAN box).
 *    2. import.meta.env.VITE_CUMORA_API_BASE — baked at build time;
 *       .env.production points it at the self-hosted server
 *       (http://127.0.0.1:5181 by default).
 *    3. '' — falls back to relative URLs, which work in Vite dev (the
 *       proxy rewrites /api → CUMORA_DEV_API_TARGET) and in any same-
 *       origin static deploy.
 *  Values should be the origin only, with NO trailing slash and NO
 *  `/api` suffix — the suffix is added on use, so `fetchJson` callers and
 *  the WS / ws-ticket paths stay consistent.
 *
 *  Resolved ONCE at module load: the override contract (setServerOrigin)
 *  requires a reload anyway, and per-call re-resolution would let a
 *  mid-session override race in-flight requests against two origins. */
export function resolveServerOrigin(): string {
  if (typeof localStorage !== 'undefined') {
    const override = localStorage.getItem(SERVER_URL_KEY)
    if (override) return override.replace(/\/+$/, '')
  }
  const baked = import.meta.env.VITE_CUMORA_API_BASE as string | undefined
  if (baked) return baked.replace(/\/+$/, '')
  return ''
}

export const SERVER_ORIGIN = resolveServerOrigin()

/** Public getter for UI surfaces (AuthScreen, Settings). Returns the
 *  origin actually in use this session — same value `fetchJson` callers and
 *  the WS client are built against. Empty string means "relative URLs,
 *  going through the Vite proxy or same-origin." */
export function getServerOrigin(): string {
  return SERVER_ORIGIN
}

/** Persist a new server origin override and clear the existing session.
 *  We don't try to hot-swap the in-memory API/WS — anything pending against
 *  the old origin would race or fail in confusing ways. Callers should
 *  follow up with `location.reload()` so the whole app boots fresh against
 *  the new origin. Pass `null` to drop the override entirely (revert to
 *  build-time default). */
export function setServerOrigin(origin: string | null): void {
  if (origin == null || origin.trim() === '') {
    localStorage.removeItem(SERVER_URL_KEY)
  } else {
    localStorage.setItem(SERVER_URL_KEY, origin.trim().replace(/\/+$/, ''))
  }
  // Auth token is server-scoped — DELETE here so a reload lands on AuthScreen
  // instead of probing /auth/me against the new origin with a stale token.
  useAuth.getState().clear()
}

export function getDevModeEnabled(): boolean {
  if (typeof localStorage === 'undefined') return false
  return localStorage.getItem(DEVTOOLS_KEY) === '1'
}

export function setDevModeEnabled(enabled: boolean): void {
  if (typeof localStorage === 'undefined') return
  if (enabled) localStorage.setItem(DEVTOOLS_KEY, '1')
  else localStorage.removeItem(DEVTOOLS_KEY)
}

export class ApiError extends Error {
  constructor(message: string, readonly status: number) {
    super(message)
    this.name = 'ApiError'
  }
}

/** 共享 fetch 骨架。两个消费面的差异经 opts 表达:
 *  - 主面(client.http):base=`${SERVER_ORIGIN}/api`,带 x-company-id 与
 *    devtools 头,401 除 `/auth/` 前缀外才清 session(匿名 auth 端点
 *    的 401 是"凭证错误"而非"会话过期",不能清掉半登录态);
 *  - admin 面:base=`${SERVER_ORIGIN}/api/admin`,无公司/devtools 头
 *    (admin 是跨租户面),401 恒清(该面无匿名端点)。
 *  错误 detail 解析取两面的并集:`{error}` ?? `{message}` ?? 文本前
 *  200 字;空 body 回退 status 文案。 */
export async function fetchJson<T>(
  path: string,
  init: RequestInit | undefined,
  opts: {
    base: string
    companyHeader?: boolean
    devModeHeader?: boolean
    /** 401 是否清 session;缺省恒清(admin 面)。 */
    clear401?: (path: string) => boolean
  },
): Promise<T> {
  const headers: Record<string, string> = { 'content-type': 'application/json' }
  const token = getAuthToken()
  if (token) headers.authorization = `Bearer ${token}`
  if (opts.companyHeader) {
    const company = getActiveCompanyId()
    if (company) headers['x-company-id'] = company
  }
  if (opts.devModeHeader && getDevModeEnabled()) headers['x-cumora-dev-mode'] = '1'
  const res = await fetch(`${opts.base}${path}`, {
    headers: { ...headers, ...(init?.headers ?? {}) },
    ...init,
  })
  // Auto-clear session on 401 so the AuthGate boots back to the login screen.
  if (res.status === 401 && (opts.clear401 ?? (() => true))(path)) {
    useAuth.getState().clear()
  }
  if (!res.ok) {
    // Surface the server's actual error message — most endpoints return a
    // JSON body like `{error: "..."}` on failure. Falls back to a body text
    // snippet if it isn't JSON, then to status only as last resort.
    let detail: string | null = null
    try {
      const text = await res.text()
      if (text) {
        try {
          const j = JSON.parse(text) as { error?: string; message?: string }
          detail = j.error ?? j.message ?? text.slice(0, 200)
        } catch { detail = text.slice(0, 200) }
      }
    } catch { /* ignore */ }
    throw new ApiError(detail ? `${detail} (${res.status})` : `${res.status} ${res.statusText}`, res.status)
  }
  return res.json() as Promise<T>
}
