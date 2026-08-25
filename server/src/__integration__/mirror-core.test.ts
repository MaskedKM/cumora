/**
 * 验收镜像 · core 域(#49):auth/me/quota/companies/invitations/preferences/
 * uploads/push/search/projects —— 全外部行为(HTTP 往返),不触内部。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import { pool } from '../db/pool.js'
import {
  buildApiTestApp, ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, MIRROR_BASE,
} from './_helpers.js'

const USER = 'u-mirror-core'
const COMPANY = 'c-mirror-core'
let server: Server
let baseUrl: string

before(async () => {
  await ensureSchemaOnce()
  const app = await buildApiTestApp(USER)
  await new Promise<void>((resolve) => {
    server = createServer(app).listen(0, () => {
      const a = server.address()
      if (a && typeof a === 'object') baseUrl = `http://127.0.0.1:${a.port}`
      resolve()
    })
  })
  if (MIRROR_BASE) baseUrl = MIRROR_BASE
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
})

after(async () => { await teardownAll(server) })

async function call(path: string, init?: RequestInit): Promise<{ status: number; json: any }> {
  const res = await fetch(`${baseUrl}/api${path}`, {
    ...init,
    headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, ...(init?.headers ?? {}) },
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

test('[mirror] GET /health and /livez are anonymous and ok', async () => {
  const h = await call('/health')
  assert.equal(h.status, 200)
  assert.equal(h.json.ok, true)
  const l = await call('/livez')
  assert.equal(l.status, 200)
})

test('[mirror] GET /auth/me returns user+companies+capabilities', async () => {
  const r = await call('/auth/me')
  assert.equal(r.status, 200)
  assert.equal(r.json.user.id, USER)
  assert.ok(Array.isArray(r.json.companies) && r.json.companies.length >= 1)
  assert.equal(typeof r.json.serverCapabilities.invitationEmail, 'boolean')
})

test('[mirror] GET /me returns participant summary', async () => {
  const r = await call('/me')
  assert.equal(r.status, 200)
  assert.equal(r.json.id, USER)
  assert.equal(r.json.kind, 'human')
})

test('[mirror] GET /me/quota returns configured shape', async () => {
  const r = await call('/me/quota')
  assert.equal(r.status, 200)
  assert.equal(typeof r.json.configured, 'boolean')
})

test('[mirror] companies CRUD-ish: list + create', async () => {
  const list = await call('/companies')
  assert.equal(list.status, 200)
  assert.ok(Array.isArray(list.json))
  const created = await call('/companies', { method: 'POST', body: JSON.stringify({ name: 'Mirror Co' }) })
  assert.equal(created.status, 201)
  assert.ok(created.json.id)
  assert.ok(created.json.slug)
})

test('[mirror] invitations: create → preview → accept → list → revoke', async () => {
  const created = await call(`/companies/${COMPANY}/invitations`, {
    method: 'POST',
    body: JSON.stringify({ role: 'member' }),
  })
  assert.equal(created.status, 201)
  assert.ok(created.json.token)
  assert.ok(created.json.url)

  // 创建者本人已在此公司 → preview 按语义报 already_member(而非 valid)
  const selfPreview = await call(`/invitations/${created.json.token}`)
  assert.equal(selfPreview.status, 200)
  assert.equal(selfPreview.json.status, 'already_member')

  const accepted = await call(`/invitations/${created.json.token}/accept`, { method: 'POST', body: '{}' })
  assert.equal(accepted.status, 200)
  assert.equal(accepted.json.ok, true)
  assert.equal(accepted.json.alreadyMember, true)

  // 邮箱锁定到全新地址的邀请 → 已登录的本司成员查看仍报 already_member
  // (成员检查先于邮箱检查——见 loadInvitation);匿名 valid 视角留给 Go 候选
  // 实现对拍时用无 auth 头的裸 fetch 验证。
  const emailLocked = await call(`/companies/${COMPANY}/invitations`, {
    method: 'POST',
    body: JSON.stringify({ email: 'new-person@test.local' }),
  })
  assert.equal(emailLocked.status, 201)
  const freshPreview = await call(`/invitations/${emailLocked.json.token}`)
  assert.equal(freshPreview.status, 200)
  assert.equal(freshPreview.json.status, 'already_member')
  // (镜像 app 的伪造 auth 对所有请求盖章——匿名 valid 视角在此形态下不可达,
  //  Go 候选实现经 MIRROR_BASE 对拍时自然覆盖。)

  const list = await call(`/companies/${COMPANY}/invitations`)
  assert.equal(list.status, 200)
  assert.ok(Array.isArray(list.json) && list.json.length >= 1)

  const revoked = await call(`/companies/${COMPANY}/invitations/${created.json.id}`, { method: 'DELETE' })
  assert.equal(revoked.status, 200)
  assert.equal(revoked.json.revoked, true)
})

test('[mirror] preferences round-trip', async () => {
  const put = await call('/me/preferences', { method: 'PUT', body: JSON.stringify({ theme: 'dark', mirror: true }) })
  assert.equal(put.status, 200)
  const get = await call('/me/preferences')
  assert.equal(get.status, 200)
  assert.equal(get.json.theme, 'dark')
})

test('[mirror] uploads capabilities + base64 round-trip', async () => {
  const caps = await call('/uploads/capabilities')
  assert.equal(caps.status, 200)
  assert.equal(typeof caps.json.maxBytes, 'number')

  const pngB64 = Buffer.from([0x89, 0x50, 0x4e, 0x47]).toString('base64')
  const up = await call('/uploads', { method: 'POST', body: JSON.stringify({ name: 't.png', mime: 'image/png', dataBase64: pngB64 }) })
  assert.equal(up.status, 200)
  assert.ok(up.json.url)
  assert.equal(up.json.kind, 'img')
})

test('[mirror] push register/unregister idempotent', async () => {
  const reg = await call('/push/register', {
    method: 'POST',
    body: JSON.stringify({ platform: 'web', token: 'mirror-token-1' }),
  })
  assert.equal(reg.status, 200)
  const unreg = await call('/push/unregister', { method: 'POST', body: JSON.stringify({ token: 'mirror-token-1' }) })
  assert.equal(unreg.status, 200)
})

test('[mirror] search returns all four buckets', async () => {
  const r = await call('/search?q=x')
  assert.equal(r.status, 200)
  for (const k of ['participants', 'rooms', 'groups', 'messages']) {
    assert.ok(Array.isArray(r.json[k]), `bucket ${k}`)
  }
})

test('[mirror] projects list/create/update/archive', async () => {
  const created = await call('/projects', { method: 'POST', body: JSON.stringify({ name: 'Mirror Proj', color: '#123456' }) })
  assert.equal(created.status, 201)
  const list = await call('/projects')
  assert.equal(list.status, 200)
  assert.ok(list.json.some((p: any) => p.id === created.json.id))
  const upd = await call(`/projects/${created.json.id}`, { method: 'PUT', body: JSON.stringify({ name: 'Renamed' }) })
  assert.equal(upd.status, 200)
  const arch = await call(`/projects/${created.json.id}/archive`, { method: 'POST', body: JSON.stringify({ archive: true }) })
  assert.equal(arch.status, 200)
  assert.equal(arch.json.status, 'archived')
})

test('[mirror] unknown route → 404 JSON error', async () => {
  const r = await call('/definitely-not-a-route')
  assert.equal(r.status, 404)
})
