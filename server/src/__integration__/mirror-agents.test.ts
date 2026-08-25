/**
 * 验收镜像 · computers/agents/boards/calendar/documents/observability 域(#49)—— 公共脚手架见 _helpers.startMirror。
 */
import { test, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { pool } from '../db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror,
} from './_helpers.js'

const USER = 'u-mirror-ag'
const COMPANY = 'c-mirror-ag'

async function seedCompanyAndUser(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Co', $2, $3)`,
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

async function seedAgent(id: string): Promise<void> {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', $3, 'A', '#111111', 'resting')
     ON CONFLICT (id, company_id) DO NOTHING`,
    [id, COMPANY, id],
  )
}

test('[mirror] computers: list empty + pairing code mint', async () => {
  const list = await call('/computers')
  assert.equal(list.status, 200)
  assert.deepEqual(list.json, [])
  const pair = await call('/computers', { method: 'POST', body: '{}' })
  assert.equal(pair.status, 201)
  assert.ok(typeof pair.json.code === 'string' && pair.json.code.length > 0)
})

test('[mirror] agents: create → update → list → autonomy → offboard → rehire', async () => {
  const created = await call('/agents', {
    method: 'POST',
    body: JSON.stringify({ name: 'Mirror Bot', role: 'tester', systemPrompt: 'You are a terse test agent.' }),
  })
  assert.equal(created.status, 201)
  const id = created.json.id

  const upd = await call(`/agents/${id}`, { method: 'PUT', body: JSON.stringify({ bio: 'mirror bio' }) })
  assert.equal(upd.status, 200)

  const parts = await call('/participants')
  assert.equal(parts.status, 200)
  assert.ok(parts.json.some((p: any) => p.id === id && p.bio === 'mirror bio'))

  const auto = await call(`/agents/${id}/autonomy`, { method: 'PUT', body: JSON.stringify({ threshold: 7 }) })
  assert.equal(auto.status, 200)
  assert.equal(typeof auto.json.threshold, 'number')
  const allAuto = await call('/agents/autonomy')
  assert.equal(allAuto.status, 200)
  assert.ok(allAuto.json.some((a: any) => a.agentId === id))

  const off = await call(`/agents/${id}`, { method: 'DELETE' })
  assert.equal(off.status, 200)
  assert.ok(off.json.departedAt)
  const rehire = await call(`/agents/${id}/rehire`, { method: 'POST' })
  assert.equal(rehire.status, 200)
})

test('[mirror] boards full lifecycle', async () => {
  const board = await call('/boards', { method: 'POST', body: JSON.stringify({ title: 'Mirror Board' }) })
  assert.equal(board.status, 200)
  const bid = board.json.id
  const col = await call(`/boards/${bid}/columns`, { method: 'POST', body: JSON.stringify({ title: 'Todo' }) })
  assert.equal(col.status, 200)
  const card = await call(`/boards/${bid}/cards`, {
    method: 'POST',
    body: JSON.stringify({ columnId: col.json.id, title: 'First card' }),
  })
  assert.equal(card.status, 200)
  const snap = await call(`/boards/${bid}`)
  assert.equal(snap.status, 200)
  assert.equal(snap.json.cards.length, 1)
  const moved = await call(`/boards/${bid}/cards/${card.json.id}`, {
    method: 'PATCH',
    body: JSON.stringify({ title: 'Renamed card' }),
  })
  assert.equal(moved.status, 200)
  const lookup = await call(`/cards/${card.json.id}`)
  assert.equal(lookup.status, 200)
  assert.equal(lookup.json.card.title, 'Renamed card')
  const del = await call(`/boards/${bid}/cards/${card.json.id}`, { method: 'DELETE' })
  assert.equal(del.status, 200)
})

test('[mirror] calendar: create/get/patch/delete + dispatches shape', async () => {
  const ev = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'Mirror standup', startAt: new Date(Date.now() + 3600_000).toISOString() }),
  })
  assert.equal(ev.status, 201)
  const id = ev.json.event.id
  const got = await call(`/calendar/events/${id}`)
  assert.equal(got.status, 200)
  assert.equal(got.json.event.title, 'Mirror standup')
  const patched = await call(`/calendar/events/${id}`, { method: 'PATCH', body: JSON.stringify({ title: 'Renamed' }) })
  assert.equal(patched.status, 200)
  assert.equal(patched.json.event.title, 'Renamed')
  const list = await call('/calendar/events')
  assert.equal(list.status, 200)
  assert.ok(Array.isArray(list.json.events))
  const disp = await call(`/calendar/events/${id}/dispatches`)
  assert.equal(disp.status, 200)
  assert.ok(Array.isArray(disp.json.dispatches))
  const del = await call(`/calendar/events/${id}`, { method: 'DELETE' })
  assert.equal(del.status, 200)
})

test('[mirror] documents: create/get/rename/collaborators/delete', async () => {
  const doc = await call('/documents', { method: 'POST', body: JSON.stringify({ title: 'Mirror Doc' }) })
  assert.equal(doc.status, 201)
  const id = doc.json.id
  const got = await call(`/documents/${id}`)
  assert.equal(got.status, 200)
  assert.equal(got.json.title, 'Mirror Doc')
  const renamed = await call(`/documents/${id}`, { method: 'PUT', body: JSON.stringify({ title: 'Renamed Doc' }) })
  assert.equal(renamed.status, 200)
  assert.equal(renamed.json.title, 'Renamed Doc')
  const collab = await call(`/documents/${id}/collaborators`, { method: 'PUT', body: JSON.stringify({ participantIds: [] }) })
  assert.equal(collab.status, 200)
  assert.deepEqual(collab.json.collaborators, [])
  const list = await call('/documents')
  assert.equal(list.status, 200)
  assert.ok(list.json.documents.some((d: any) => d.id === id))
  const del = await call(`/documents/${id}`, { method: 'DELETE' })
  assert.equal(del.status, 200)
})

test('[mirror] observability surfaces respond with arrays/objects', async () => {
  await seedAgent('a-mirror-obs')
  const runs = await call('/agents/observability/runs')
  assert.equal(runs.status, 200)
  assert.ok(Array.isArray(runs.json))
  const triage = await call('/agents/observability/triage')
  assert.equal(triage.status, 200)
  assert.equal(typeof triage.json.sinceHours, 'number')
  const spend = await call('/agents/observability/llm-spend')
  assert.equal(spend.status, 200)
  assert.ok(Array.isArray(spend.json))
})

test('[mirror] devtools capabilities + peek agent-chats', async () => {
  const caps = await call('/devtools/capabilities')
  assert.equal(caps.status, 200)
  assert.equal(typeof caps.json.enabled, 'boolean')
  const peek = await call('/peek/agent-chats')
  assert.equal(peek.status, 200)
  assert.ok(Array.isArray(peek.json))
})
