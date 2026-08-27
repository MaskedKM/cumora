/**
 * 验收镜像 · OAuth 登录流(#109)—— /api/auth/start|callback 全分支。
 * provider 指到本文件起的本地桩(定端口 18994,两侧同源:TS 直读
 * process.env、Go 走服务器 env 的 CUMORA_OAUTH_GITHUB_BASE)。302 的
 * Location 用 node:http 直取(fetch 的 manual redirect 拿不到头)。
 */
import { test, beforeEach, after, before } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, request as httpReq, type IncomingMessage } from 'node:http'
import { pool } from '../db/pool.js'
import { redis } from '../redis.js'
import { ensureSchemaOnce, resetAllTables, teardownAll, startMirror } from './_helpers.js'

const STUB_PORT = 18994
const STUB_BASE = `http://127.0.0.1:${STUB_PORT}`
const RETURN_OK = 'http://localhost:5180/after-login'

// 桩可变形状:各用例按需改写后再发起回调。
const stub = {
  user: { id: 4242, login: 'ghuser', name: 'GH User', avatar_url: `${STUB_BASE}/pic.jpg` },
  emails: [{ email: 'GHUser@Example.com', primary: true, verified: true }],
  failToken: false,
}

const JPEG = Buffer.from([
  0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01,
  0xff, 0xd9,
])

let stubSrv: ReturnType<typeof createServer> | null = null

function rawGet(base: string, path: string): Promise<{ status: number; location?: string; body: string }> {
  return new Promise((resolve, reject) => {
    const req = httpReq(new URL(path, base), { method: 'GET' }, (res: IncomingMessage) => {
      let b = ''
      res.on('data', (d: Buffer) => { b += d.toString() })
      res.on('end', () => resolve({ status: res.statusCode ?? 0, location: res.headers.location, body: b }))
    })
    req.on('error', reject)
    req.end()
  })
}

async function pollSQL<T>(q: string, args: unknown[]): Promise<T | null> {
  for (let i = 0; i < 20; i++) {
    const r = await pool.query(q, args)
    if (r.rows.length > 0) return r.rows[0] as T
    await new Promise((r2) => setTimeout(r2, 100))
  }
  return null
}

await ensureSchemaOnce()
const mirror = startMirror('u-mirror-oauth', 'c-mirror-oauth')
const base = mirror.baseUrl

async function startAndGrabState(query = ''): Promise<{ status: number; location: string; state: string }> {
  const r = await rawGet(base(), `/api/auth/start/github${query}`)
  assert.equal(r.status, 302)
  const loc = r.location!
  const state = new URL(loc).searchParams.get('state')!
  assert.ok(state.length >= 40, 'state is base64url(32B)')
  return { status: r.status, location: loc, state }
}

before(async () => {
  process.env.GITHUB_CLIENT_ID = 'stub-id'
  process.env.GITHUB_CLIENT_SECRET = 'stub-secret'
  process.env.CUMORA_OAUTH_GITHUB_BASE = STUB_BASE
  process.env.CUMORA_AUTH_RETURN_ALLOWLIST = RETURN_OK.slice(0, RETURN_OK.lastIndexOf('/')) + '/'
  delete process.env.GOOGLE_CLIENT_ID
  delete process.env.GOOGLE_CLIENT_SECRET
  stubSrv = createServer((req, res) => {
    const u = req.url ?? ''
    if (u === '/access_token') {
      if (stub.failToken) { res.writeHead(500).end('boom'); return }
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ access_token: 'stub-access-token' }))
      return
    }
    if (u === '/user') {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify(stub.user))
      return
    }
    if (u === '/user/emails') {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify(stub.emails))
      return
    }
    if (u === '/pic.jpg') {
      res.writeHead(200, { 'content-type': 'image/jpeg' })
      res.end(JPEG)
      return
    }
    res.writeHead(404).end('nope')
  })
  await new Promise<void>((resolve, reject) => {
    stubSrv!.once('error', reject)
    stubSrv!.listen(STUB_PORT, '127.0.0.1', () => resolve())
  })
})

beforeEach(async () => {
  await resetAllTables()
  const keys = await redis.keys('oauth:state:*')
  if (keys.length > 0) await redis.del(...keys)
  stub.user = { id: 4242, login: 'ghuser', name: 'GH User', avatar_url: `${STUB_BASE}/pic.jpg` }
  stub.emails = [{ email: 'GHUser@Example.com', primary: true, verified: true }]
  stub.failToken = false
})

after(async () => {
  await mirror.close()
  await new Promise<void>((resolve) => stubSrv?.close(() => resolve()))
  await teardownAll()
})

test('[mirror-oauth] unknown provider → 404 json', async () => {
  const r = await rawGet(base(), '/api/auth/start/gitlab')
  assert.equal(r.status, 404)
  assert.equal(JSON.parse(r.body).error, 'unknown provider')
})

test('[mirror-oauth] unconfigured provider(google)→ 503', async () => {
  const r = await rawGet(base(), '/api/auth/start/google')
  assert.equal(r.status, 503)
  assert.equal(JSON.parse(r.body).error, 'google oauth not configured')
})

test('[mirror-oauth] start: return URL 不在白名单 → 400', async () => {
  const r = await rawGet(base(), '/api/auth/start/github?return=http://evil.example/')
  assert.equal(r.status, 400)
  assert.equal(JSON.parse(r.body).error, 'return URL not allowed')
})

test('[mirror-oauth] start → 302 到桩 authorize,state 形状+redirect_uri', async () => {
  const { location } = await startAndGrabState()
  const u = new URL(location)
  assert.equal(u.origin + u.pathname, `${STUB_BASE}/authorize`)
  assert.equal(u.searchParams.get('client_id'), 'stub-id')
  assert.equal(u.searchParams.get('response_type'), 'code')
  assert.equal(u.searchParams.get('scope'), 'read:user user:email')
  assert.equal(u.searchParams.get('redirect_uri'), 'http://localhost:5181/api/auth/callback/github')
})

test('[mirror-oauth] happy path:首登建号建区,token 走 fragment,me 可用', async () => {
  const { state } = await startAndGrabState(`?return=${encodeURIComponent(RETURN_OK)}`)
  const cb = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(cb.status, 302)
  const loc = cb.location!
  assert.ok(loc.startsWith(`${RETURN_OK}#`), `fragment target: ${loc}`)
  const frag = new URLSearchParams(loc.slice(loc.indexOf('#') + 1))
  const token = frag.get('token')
  assert.ok(token && token.length >= 20, 'token in fragment')
  const companyId = frag.get('companyId')
  assert.ok(companyId && companyId.startsWith('co-'))

  // 建号断言:email 落小写、身份已链、avatar 已镜像到本地。
  const row = await pollSQL<{ id: string; email: string; avatar_url: string }>(
    `SELECT id, email, avatar_url FROM users WHERE email = 'ghuser@example.com'`, [])
  assert.ok(row, 'user created with lowercased email')
  const ident = await pool.query(`SELECT 1 FROM user_identities WHERE provider='github' AND provider_id='4242' AND user_id=$1`, [row!.id])
  assert.equal(ident.rowCount, 1)
  assert.equal(row!.avatar_url, `/uploads/avatars/${row!.id}.jpg`)

  // 建区断言:owner 成员 + participants 落行。
  const comp = await pool.query<{ name: string; role: string }>(
    `SELECT c.name, cm.role FROM companies c JOIN company_members cm ON cm.company_id=c.id WHERE c.id=$1`, [companyId])
  assert.equal(comp.rows[0]!.name, `GH User's team`)
  assert.equal(comp.rows[0]!.role, 'owner')
  const part = await pool.query(`SELECT 1 FROM participants WHERE id=$1 AND company_id=$2 AND kind='human'`, [row!.id, companyId])
  assert.equal(part.rowCount, 1)

  // 会话可用:token 走 /auth/me。
  const me = await fetch(`${base()}/api/auth/me`, { headers: { authorization: `Bearer ${token}` } })
  assert.equal(me.status, 200)
  const meJson = (await me.json()) as { user: { email: string } }
  assert.equal(meJson.user.email, 'ghuser@example.com')

  // 审计(异步 fire-and-forget,轮询)
  const auditRow = await pollSQL<{ kind: string }>(`SELECT kind FROM audit_events WHERE kind='login'`, [])
  assert.ok(auditRow, 'login audit row')
})

test('[mirror-oauth] state 单次消费:重放 → #error=bad_state', async () => {
  const { state } = await startAndGrabState()
  const first = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(first.status, 302)
  const replay = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(replay.status, 302)
  assert.ok(replay.location!.startsWith('http://localhost:5173/#'))
  assert.ok(replay.location!.includes('error=bad_state'))
})

test('[mirror-oauth] callback 缺 code → #error=missing_code_or_state', async () => {
  const r = await rawGet(base(), '/api/auth/callback/github?state=whatever')
  assert.equal(r.status, 302)
  assert.ok(r.location!.includes('error=missing_code_or_state'))
})

test('[mirror-oauth] invite 门控:带 invite 首登不建个人区', async () => {
  stub.emails = [{ email: 'invited@example.com', primary: true, verified: true }]
  const { state } = await startAndGrabState(`?return=${encodeURIComponent(RETURN_OK)}&invite=abcdefgh1234`)
  const cb = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(cb.status, 302)
  const frag = new URLSearchParams(cb.location!.slice(cb.location!.indexOf('#') + 1))
  assert.ok(frag.get('token'), 'session minted')
  assert.equal(frag.get('companyId'), null, 'no auto-created workspace on invite path')
  const comps = await pool.query(`SELECT 1 FROM companies c JOIN company_members cm ON cm.company_id=c.id JOIN users u ON u.id=cm.user_id WHERE u.email='invited@example.com'`)
  assert.equal(comps.rowCount, 0)
})

test('[mirror-oauth] 停用账号:Path B 跨链补绑 → #suspended=1,不发会话', async () => {
  await pool.query(
    `INSERT INTO users (id, email, display_name, email_verified_at, suspended_at, suspension_reason)
       VALUES ('u-susp', 'susp@example.com', 'Suspended One', NOW(), NOW(), 'misconduct')`)
  stub.emails = [{ email: 'susp@example.com', primary: true, verified: true }]
  const { state } = await startAndGrabState()
  const cb = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(cb.status, 302)
  assert.ok(cb.location!.startsWith('http://localhost:5173/#'), 'default done URL')
  const frag = new URLSearchParams(cb.location!.slice(cb.location!.indexOf('#') + 1))
  assert.equal(frag.get('suspended'), '1')
  assert.equal(frag.get('email'), 'susp@example.com')
  assert.equal(frag.get('reason'), 'misconduct')
  const ident = await pool.query(`SELECT 1 FROM user_identities WHERE user_id='u-susp' AND provider='github'`)
  assert.equal(ident.rowCount, 1, 'identity cross-linked')
  const sessions = await pool.query(`SELECT 1 FROM sessions WHERE user_id='u-susp'`)
  assert.equal(sessions.rowCount, 0, 'no session minted')
})

test('[mirror-oauth] 等待名单开门:新邮箱入列 → #waitlist=1', async () => {
  await pool.query(`INSERT INTO app_settings (key, value, updated_by) VALUES ('waitlist_enabled', 'true'::jsonb, 'test')`)
  stub.emails = [{ email: 'waitlist@example.com', primary: true, verified: true }]
  const { state } = await startAndGrabState()
  const cb = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(cb.status, 302)
  assert.ok(cb.location!.includes('waitlist=1'))
  assert.ok(cb.location!.includes('email=waitlist%40example.com'))
  const wl = await pollSQL<{ status: string }>(
    `SELECT status FROM waitlist WHERE email='waitlist@example.com'`, [])
  assert.ok(wl, 'waitlist row')
  assert.equal(wl!.status, 'pending')
  const u = await pool.query(`SELECT 1 FROM users WHERE email='waitlist@example.com'`)
  assert.equal(u.rowCount, 0, 'no user created')
})

test('[mirror-oauth] 换码失败 → #error=… + login_failed 审计', async () => {
  stub.failToken = true
  const { state } = await startAndGrabState(`?return=${encodeURIComponent(RETURN_OK)}`)
  const cb = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(cb.status, 302)
  assert.ok(cb.location!.startsWith(`${RETURN_OK}#`))
  assert.ok(cb.location!.includes('error=github+token+exchange+500'), cb.location)
  const auditRow = await pollSQL<{ kind: string }>(`SELECT kind FROM audit_events WHERE kind='login_failed'`, [])
  assert.ok(auditRow, 'login_failed audit row')
})

test('[mirror-oauth] 二次登录走 Path A:不重复建号,仍发新会话', async () => {
  for (let round = 0; round < 2; round++) {
    const { state } = await startAndGrabState(`?return=${encodeURIComponent(RETURN_OK)}`)
    const cb = await rawGet(base(), `/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
    assert.equal(cb.status, 302)
    const frag = new URLSearchParams(cb.location!.slice(cb.location!.indexOf('#') + 1))
    assert.ok(frag.get('token'), `round${round} loc=${cb.location}`)
  }
  const users = await pool.query(`SELECT id FROM users WHERE email='ghuser@example.com'`)
  assert.equal(users.rowCount, 1, 'single user across two logins')
  const sessions = await pool.query(`SELECT 1 FROM sessions WHERE user_id=$1`, [users.rows[0]!.id])
  assert.equal(sessions.rowCount, 2, 'two sessions minted')
})
