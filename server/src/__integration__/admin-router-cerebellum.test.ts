/**
 * Integration tests for the Cerebellum Route surface added to
 * `GET/PUT /api/admin/settings` plus the new
 * `GET /api/admin/computers/available-engines` endpoint (ticket #22).
 *
 * Mirrors the existing admin-router integration test pattern: a real
 * Express app (`buildApiTestApp`) + real Postgres, a fake auth middleware
 * that stamps `authUserId`, and `requireAdmin`'s real `users.is_admin`
 * check (not mocked) so the 403 gate is exercised for real. Each test
 * needs its own identity, so each spins up (and tears down) its own
 * listening server rather than sharing one across the file.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import { randomUUID } from 'node:crypto'
import { ensureSchemaOnce, resetAllTables, seedUserMembership, buildApiTestApp, teardownAll } from './_helpers.js'
import { pool } from '../db/pool.js'

before(async () => {
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

/** Boot a listening server with the fake-auth app stamped as `userId`.
 *  Caller is responsible for closing it. */
async function listen(userId: string): Promise<{ baseUrl: string; server: Server }> {
  const app = await buildApiTestApp(userId)
  const server = createServer(app)
  const baseUrl = await new Promise<string>((resolve) => {
    server.listen(0, () => {
      const addr = server.address()
      resolve(addr && typeof addr === 'object' ? `http://127.0.0.1:${addr.port}` : '')
    })
  })
  return { baseUrl, server }
}

function close(server: Server): Promise<void> {
  return new Promise((resolve) => server.close(() => resolve()))
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

test('[integration] GET /api/admin/settings is 403 for a non-admin user', async () => {
  const { userId } = await seedNonAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    const res = await fetch(`${baseUrl}/api/admin/settings`)
    assert.equal(res.status, 403)
  } finally {
    await close(server)
  }
})

test('[integration] PUT /api/admin/settings is 403 for a non-admin user', async () => {
  const { userId } = await seedNonAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    const res = await fetch(`${baseUrl}/api/admin/settings`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ cerebellum_route: 'byoa' }),
    })
    assert.equal(res.status, 403)
  } finally {
    await close(server)
  }
})

test('[integration] GET /api/admin/settings includes Cerebellum defaults and never leaks the API key', async () => {
  const { userId } = await seedAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    const res = await fetch(`${baseUrl}/api/admin/settings`)
    assert.equal(res.status, 200)
    const body = await res.json() as Record<string, unknown>
    assert.equal(body.cerebellum_route, 'remote')
    assert.equal(body.cerebellum_local_engine, 'claude')
    assert.equal(body.cerebellum_provider, '')
    assert.equal(body.cerebellum_base_url, '')
    assert.equal(body.cerebellum_model, '')
    assert.equal(body.cerebellum_api_key_configured, false)
    assert.equal(body.cerebellum_api_key_suffix, null)
    assert.equal(JSON.stringify(body).includes('sk-'), false)
  } finally {
    await close(server)
  }
})

test('[integration] PUT then GET round-trips all six Cerebellum fields; the API key is never echoed back', async () => {
  const { userId } = await seedAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    const putRes = await fetch(`${baseUrl}/api/admin/settings`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        cerebellum_route: 'byoa',
        cerebellum_local_engine: 'codex',
        cerebellum_provider: 'deepseek',
        cerebellum_base_url: 'https://api.deepseek.com/v1',
        cerebellum_model: 'deepseek-chat',
        cerebellum_api_key: 'sk-integration-test-9999',
      }),
    })
    assert.equal(putRes.status, 200)
    const putBody = await putRes.json() as Record<string, unknown>
    assert.equal(putBody.cerebellum_api_key_configured, true)
    assert.equal(putBody.cerebellum_api_key_suffix, '9999')
    assert.equal(JSON.stringify(putBody).includes('sk-integration-test-9999'), false)

    const getRes = await fetch(`${baseUrl}/api/admin/settings`)
    assert.equal(getRes.status, 200)
    const getBody = await getRes.json() as Record<string, unknown>
    assert.equal(getBody.cerebellum_route, 'byoa')
    assert.equal(getBody.cerebellum_local_engine, 'codex')
    assert.equal(getBody.cerebellum_provider, 'deepseek')
    assert.equal(getBody.cerebellum_base_url, 'https://api.deepseek.com/v1')
    assert.equal(getBody.cerebellum_model, 'deepseek-chat')
    assert.equal(getBody.cerebellum_api_key_configured, true)
    assert.equal(getBody.cerebellum_api_key_suffix, '9999')
    assert.equal(JSON.stringify(getBody).includes('sk-integration-test-9999'), false)
  } finally {
    await close(server)
  }
})

test('[integration] PUT without cerebellum_api_key leaves a previously-saved key untouched', async () => {
  const { userId } = await seedAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    await fetch(`${baseUrl}/api/admin/settings`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ cerebellum_api_key: 'sk-keep-me-1234' }),
    })
    const res2 = await fetch(`${baseUrl}/api/admin/settings`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ cerebellum_provider: 'novita' }),
    })
    assert.equal(res2.status, 200)
    const body2 = await res2.json() as Record<string, unknown>
    assert.equal(body2.cerebellum_provider, 'novita')
    assert.equal(body2.cerebellum_api_key_configured, true)
    assert.equal(body2.cerebellum_api_key_suffix, '1234')
  } finally {
    await close(server)
  }
})

test('[integration] PUT with an empty cerebellum_api_key clears a previously-saved key', async () => {
  const { userId } = await seedAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    await fetch(`${baseUrl}/api/admin/settings`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ cerebellum_api_key: 'sk-clear-me-5678' }),
    })
    const cleared = await fetch(`${baseUrl}/api/admin/settings`, {
      method: 'PUT',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ cerebellum_api_key: '' }),
    })
    assert.equal(cleared.status, 200)
    const body = await cleared.json() as Record<string, unknown>
    assert.equal(body.cerebellum_api_key_configured, false)
    assert.equal(body.cerebellum_api_key_suffix, null)
  } finally {
    await close(server)
  }
})

test('[integration] GET /api/admin/computers/available-engines is 403 for a non-admin user', async () => {
  const { userId } = await seedNonAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    const res = await fetch(`${baseUrl}/api/admin/computers/available-engines`)
    assert.equal(res.status, 403)
  } finally {
    await close(server)
  }
})

test('[integration] GET /api/admin/computers/available-engines unions online Computers, excludes offline ones, empty when none online', async () => {
  const { userId, companyId } = await seedAdmin()
  const { baseUrl, server } = await listen(userId)
  try {
    const empty = await fetch(`${baseUrl}/api/admin/computers/available-engines`)
    assert.equal(empty.status, 200)
    assert.deepEqual((await empty.json() as { engines: string[] }).engines, [])

    await pool.query(
      `INSERT INTO computers (id, company_id, name, kind, available_engines, status)
       VALUES
         ($1, $2, 'MacBook', 'local', '["claude","codex"]'::jsonb, 'online'),
         ($3, $2, 'VPS', 'vps', '["codex","grok"]'::jsonb, 'online'),
         ($4, $2, 'Old Laptop', 'local', '["cursor"]'::jsonb, 'offline')`,
      [`comp-${randomUUID().slice(0, 8)}`, companyId, `comp-${randomUUID().slice(0, 8)}`, `comp-${randomUUID().slice(0, 8)}`],
    )

    const res = await fetch(`${baseUrl}/api/admin/computers/available-engines`)
    assert.equal(res.status, 200)
    assert.deepEqual((await res.json() as { engines: string[] }).engines, ['claude', 'codex', 'grok'])
  } finally {
    await close(server)
  }
})
