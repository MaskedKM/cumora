/**
 * Integration tests for the Cerebellum Route surface added to
 * `GET/PUT /api/admin/settings` plus the new
 * `GET /api/admin/computers/available-engines` endpoint (ticket #22;
 * #70 起 MIRROR-only:全部请求打向 runner(run.mjs)自建的
 * Go 服)。
 *
 * requireAdmin's real `users.is_admin` check is exercised for real (not
 * mocked) so the 403 gate is covered on both backends. Each test needs its
 * own identity, so each spins its own context (server in TS form, mirror
 * client in MIRROR form) rather than sharing one across the file.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror, MIRROR_BASE,
} from './_helpers.js'
import { pool } from './harness/db/pool.js'


before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(`DELETE FROM app_settings WHERE key LIKE 'cerebellum_%'`)
  await pool.query(`DELETE FROM computers`)
})

after(async () => {
  await teardownAll()
})

type Client = {
  get: (p: string) => Promise<{ status: number; body: any }>
  put: (p: string, b: unknown) => Promise<{ status: number; body: any }>
}

/** 每用例独立身份:fake-auth 盖章(x-test-user)随 user 走,路径不带 /api 前缀。 */
async function withClient<T>(userId: string, fn: (c: Client) => Promise<T>): Promise<T> {
  const m = startMirror(userId, 'c-admin-probe')
  try {
    const wrap = (r: { status: number; json: any }) => ({ status: r.status, body: r.json })
    return await fn({
      get: async (p) => wrap(await m.call(p)),
      put: async (p, b) => wrap(await m.call(p, { method: 'PUT', body: JSON.stringify(b) })),
    })
  } finally {
    await m.close()
  }
}

async function seedCompany(companyId: string): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, 'test-owner') ON CONFLICT DO NOTHING`,
    [companyId, `Test ${companyId}`, companyId],
  )
}

async function seedAdmin(): Promise<{ userId: string; companyId: string }> {
  const userId = `u-${randomUUID().slice(0, 8)}`
  const companyId = `c-${randomUUID().slice(0, 8)}`
  await seedCompany(companyId)
  await seedUserMembership(userId, companyId)
  await pool.query(`UPDATE users SET is_admin = TRUE WHERE id = $1`, [userId])
  return { userId, companyId }
}

async function seedNonAdmin(): Promise<{ userId: string; companyId: string }> {
  const userId = `u-${randomUUID().slice(0, 8)}`
  const companyId = `c-${randomUUID().slice(0, 8)}`
  await seedCompany(companyId)
  await seedUserMembership(userId, companyId)
  return { userId, companyId }
}

test('[mirror] GET /api/admin/settings is 403 for a non-admin user', async () => {
  const { userId } = await seedNonAdmin()
  await withClient(userId, async (c) => {
    const res = await c.get('/admin/settings')
    assert.equal(res.status, 403)
  })
})

test('[mirror] PUT /api/admin/settings is 403 for a non-admin user', async () => {
  const { userId } = await seedNonAdmin()
  await withClient(userId, async (c) => {
    const res = await c.put('/admin/settings', { cerebellum_route: 'byoa' })
    assert.equal(res.status, 403)
  })
})

test('[mirror] GET /api/admin/settings includes Cerebellum defaults and never leaks the API key', async () => {
  const { userId } = await seedAdmin()
  await withClient(userId, async (c) => {
    const res = await c.get('/admin/settings')
    assert.equal(res.status, 200)
    const body = res.body as Record<string, unknown>
    assert.equal(body.cerebellum_route, 'remote')
    assert.equal(body.cerebellum_local_engine, 'claude')
    assert.equal(body.cerebellum_provider, '')
    assert.equal(body.cerebellum_base_url, '')
    assert.equal(body.cerebellum_model, '')
    assert.equal(body.cerebellum_api_key_configured, false)
    assert.equal(body.cerebellum_api_key_suffix, null)
    assert.equal(JSON.stringify(body).includes('sk-'), false)
  })
})

test('[mirror] PUT then GET round-trips all six Cerebellum fields; the API key is never echoed back', async () => {
  const { userId } = await seedAdmin()
  await withClient(userId, async (c) => {
    const putRes = await c.put('/admin/settings', {
      cerebellum_route: 'byoa',
      cerebellum_local_engine: 'codex',
      cerebellum_provider: 'deepseek',
      cerebellum_base_url: 'https://api.deepseek.com/v1',
      cerebellum_model: 'deepseek-chat',
      cerebellum_api_key: 'sk-integration-test-9999',
    })
    assert.equal(putRes.status, 200)
    const putBody = putRes.body as Record<string, unknown>
    assert.equal(putBody.cerebellum_api_key_configured, true)
    assert.equal(putBody.cerebellum_api_key_suffix, '9999')
    assert.equal(JSON.stringify(putBody).includes('sk-integration-test-9999'), false)

    const getRes = await c.get('/admin/settings')
    assert.equal(getRes.status, 200)
    const getBody = getRes.body as Record<string, unknown>
    assert.equal(getBody.cerebellum_route, 'byoa')
    assert.equal(getBody.cerebellum_local_engine, 'codex')
    assert.equal(getBody.cerebellum_provider, 'deepseek')
    assert.equal(getBody.cerebellum_base_url, 'https://api.deepseek.com/v1')
    assert.equal(getBody.cerebellum_model, 'deepseek-chat')
    assert.equal(getBody.cerebellum_api_key_configured, true)
    assert.equal(getBody.cerebellum_api_key_suffix, '9999')
    assert.equal(JSON.stringify(getBody).includes('sk-integration-test-9999'), false)
  })
})

test('[mirror] PUT without cerebellum_api_key leaves a previously-saved key untouched', async () => {
  const { userId } = await seedAdmin()
  await withClient(userId, async (c) => {
    await c.put('/admin/settings', { cerebellum_api_key: 'sk-keep-me-1234' })
    const res2 = await c.put('/admin/settings', { cerebellum_provider: 'novita' })
    assert.equal(res2.status, 200)
    const body2 = res2.body as Record<string, unknown>
    assert.equal(body2.cerebellum_provider, 'novita')
    assert.equal(body2.cerebellum_api_key_configured, true)
    assert.equal(body2.cerebellum_api_key_suffix, '1234')
  })
})

test('[mirror] PUT with an empty cerebellum_api_key clears a previously-saved key', async () => {
  const { userId } = await seedAdmin()
  await withClient(userId, async (c) => {
    await c.put('/admin/settings', { cerebellum_api_key: 'sk-clear-me-5678' })
    const cleared = await c.put('/admin/settings', { cerebellum_api_key: '' })
    assert.equal(cleared.status, 200)
    const body = cleared.body as Record<string, unknown>
    assert.equal(body.cerebellum_api_key_configured, false)
    assert.equal(body.cerebellum_api_key_suffix, null)
  })
})

test('[mirror] PUT with JSON null fields ignores them (typeof gate) and preserves the API key', async () => {
  const { userId } = await seedAdmin()
  await withClient(userId, async (c) => {
    await c.put('/admin/settings', { cerebellum_api_key: 'sk-null-guard-1234' })
    const r = await c.put('/admin/settings', {
      cerebellum_api_key: null,
      waitlist_enabled: null,
      signups_paused: null,
      cerebellum_route: null,
      cerebellum_local_engine: null,
      cerebellum_provider: null,
      cerebellum_base_url: null,
      cerebellum_model: null,
    })
    assert.equal(r.status, 400)
    assert.equal((r.body as { error: string }).error, 'no settings to update')
    const g = await c.get('/admin/settings')
    const body = g.body as Record<string, unknown>
    assert.equal(body.cerebellum_api_key_configured, true)
    assert.equal(body.cerebellum_api_key_suffix, '1234')
    assert.equal(body.waitlist_enabled, false)
    assert.equal(body.cerebellum_provider, '')
  })
})

test('[mirror] GET /api/admin/computers/available-engines is 403 for a non-admin user', async () => {
  const { userId } = await seedNonAdmin()
  await withClient(userId, async (c) => {
    const res = await c.get('/admin/computers/available-engines')
    assert.equal(res.status, 403)
  })
})

test('[mirror] GET /api/admin/computers/available-engines unions online Computers, excludes offline ones, empty when none online', async () => {
  const { userId, companyId } = await seedAdmin()
  await withClient(userId, async (c) => {
    const empty = await c.get('/admin/computers/available-engines')
    assert.equal(empty.status, 200)
    assert.deepEqual((empty.body as { engines: string[] }).engines, [])

    await pool.query(
      `INSERT INTO computers (id, company_id, name, kind, available_engines, status)
       VALUES
         ($1, $2, 'MacBook', 'local', '["claude","codex"]'::jsonb, 'online'),
         ($3, $2, 'VPS', 'vps', '["codex","grok"]'::jsonb, 'online'),
         ($4, $2, 'Old Laptop', 'local', '["cursor"]'::jsonb, 'offline')`,
      [`comp-${randomUUID().slice(0, 8)}`, companyId, `comp-${randomUUID().slice(0, 8)}`, `comp-${randomUUID().slice(0, 8)}`],
    )

    const res = await c.get('/admin/computers/available-engines')
    assert.equal(res.status, 200)
    assert.deepEqual((res.body as { engines: string[] }).engines, ['claude', 'codex', 'grok'])
  })
})
