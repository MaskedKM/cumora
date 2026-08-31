/**
 * 验收镜像 · 文档域(#55)—— CRUD + 协作者治理。
 * 双跑:CUMORA_MIRROR_BASE 指向 Go 候选(须 CUMORA_GO_FAKE_AUTH=1 起动)。
 * 协同链路(WS→sidecar)见 doc-collab-go.test.ts。
 */

import assert from 'node:assert/strict'
import { after, beforeEach, test } from 'node:test'
import { pool } from './harness/db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror,teardownAll, 
} from './_helpers.js'

const USER = 'u-mirror-docs'
const COMPANY = 'c-mirror-docs'

async function seedCompanyAndUser(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Docs Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
}

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUser()
})

after(async () => { await mirror.close(); await teardownAll() })

test('[mirror] documents list starts empty (documents array, never null)', async () => {
  const r = await call('/documents')
  assert.equal(r.status, 200)
  assert.deepEqual(r.json.documents, [])
})

test('[mirror] create defaults title to Untitled, trims+caps custom titles', async () => {
  const a = await call('/documents', { method: 'POST', body: '{}' })
  assert.equal(a.status, 201)
  assert.equal(a.json.title, 'Untitled')
  assert.equal(a.json.createdBy, USER)
  assert.equal(a.json.conversationId, null)
  assert.ok(a.json.id.startsWith('doc_'))
  assert.ok(typeof a.json.createdAt === 'string')

  const long = '标'.repeat(210)
  const b = await call('/documents', { method: 'POST', body: JSON.stringify({ title: `  ${long}  ` }) })
  assert.equal(b.status, 201)
  assert.equal([...b.json.title].length, 200)
})

test('[mirror] create with conversationId validates tenant membership', async () => {
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members) VALUES ('conv-x', $1, 'group', 'X', $2::jsonb)`,
    [COMPANY, JSON.stringify([USER])],
  )
  const ok = await call('/documents', { method: 'POST', body: JSON.stringify({ conversationId: 'conv-x' }) })
  assert.equal(ok.status, 201)
  assert.equal(ok.json.conversationId, 'conv-x')

  const bad = await call('/documents', { method: 'POST', body: JSON.stringify({ conversationId: 'conv-elsewhere' }) })
  assert.equal(bad.status, 404)
  assert.equal(bad.json.error, 'conversation not found')
})

test('[mirror] get by id; cross-tenant 404', async () => {
  const created = await call('/documents', { method: 'POST', body: '{}' })
  const id = created.json.id
  const r = await call(`/documents/${id}`)
  assert.equal(r.status, 200)
  assert.equal(r.json.id, id)

  assert.equal((await call('/documents/doc_none')).status, 404)
})

test('[mirror] put title requires non-empty, updates and reports', async () => {
  const created = await call('/documents', { method: 'POST', body: '{}' })
  const id = created.json.id
  assert.equal((await call(`/documents/${id}`, { method: 'PUT', body: JSON.stringify({ title: '   ' }) })).status, 400)
  assert.equal((await call(`/documents/${id}`, { method: 'PUT', body: JSON.stringify({ title: 'New 标题' }) })).status, 200)
  const got = await call(`/documents/${id}`)
  assert.equal(got.json.title, 'New 标题')
  assert.equal((await call('/documents/doc_none', { method: 'PUT', body: JSON.stringify({ title: 'x' }) })).status, 404)
})

test('[mirror] collaborators: replace set, validate actives, cap 100, dedup+trim', async () => {
  const created = await call('/documents', { method: 'POST', body: '{}' })
  const id = created.json.id
  const pid = `p-docs-1`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Docs Agent', 'D', '#123456', 'offline')`,
    [pid, COMPANY],
  )

  assert.equal(
    (await call(`/documents/${id}/collaborators`, { method: 'PUT', body: JSON.stringify({ participantIds: 'nope' }) })).status,
    400,
  )
  const r = await call(`/documents/${id}/collaborators`, {
    method: 'PUT',
    body: JSON.stringify({ participantIds: [`  ${pid}  `, pid, '  ', 42, pid] }),
  })
  assert.equal(r.status, 200)
  assert.deepEqual(r.json.collaborators, [pid])

  const unknown = await call(`/documents/${id}/collaborators`, {
    method: 'PUT', body: JSON.stringify({ participantIds: ['p-ghost'] }),
  })
  assert.equal(unknown.status, 400)
  assert.match(unknown.json.error, /unknown active participant/)

  const tooMany = await call(`/documents/${id}/collaborators`, {
    method: 'PUT', body: JSON.stringify({ participantIds: Array.from({ length: 101 }, (_, i) => `p-${i}`) }),
  })
  assert.equal(tooMany.status, 400)
  assert.match(tooMany.json.error, /max 100/)

  const cleared = await call(`/documents/${id}/collaborators`, {
    method: 'PUT', body: JSON.stringify({ participantIds: [] }),
  })
  assert.equal(cleared.status, 200)
  assert.deepEqual(cleared.json.collaborators, [])
})

test('[mirror] delete removes doc; second delete 404', async () => {
  const created = await call('/documents', { method: 'POST', body: '{}' })
  const id = created.json.id
  assert.equal((await call(`/documents/${id}`, { method: 'DELETE' })).status, 200)
  assert.equal((await call(`/documents/${id}`)).status, 404)
  assert.equal((await call(`/documents/${id}`, { method: 'DELETE' })).status, 404)
})

test('[mirror] list orders by updated_at DESC', async () => {
  const a = await call('/documents', { method: 'POST', body: JSON.stringify({ title: 'a' }) })
  await new Promise((r) => setTimeout(r, 20))
  const b = await call('/documents', { method: 'POST', body: JSON.stringify({ title: 'b' }) })
  await new Promise((r) => setTimeout(r, 20))
  await call(`/documents/${a.json.id}`, { method: 'PUT', body: JSON.stringify({ title: 'a2' }) })
  const list = await call('/documents')
  assert.equal(list.json.documents.length, 2)
  assert.equal(list.json.documents[0].id, a.json.id)
  assert.equal(list.json.documents[1].id, b.json.id)
})
