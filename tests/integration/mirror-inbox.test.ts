/**
 * 验收镜像 · 人侧 Inbox 分级(#264)—— 消费面(/api/inbox*)+ 生成面
 * (run 失败/看板流转 → 分级条目)。WS inbox.new 帧与推送纪律由契约
 * 生成物 + NotificationToasts 分级消费(前端);此处钉住服务端语义:
 * 只有"需要人裁决"落 action_required,静音不影响落账。
 */
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { after, beforeEach, test } from 'node:test'
import { pool } from './harness/db/pool.js'
import { signAgentToken } from './harness/agents/runtime/jwt.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror, teardownAll,
} from './_helpers.js'

const USER = 'u-mirror-inbox'
const COMPANY = 'c-mirror-inbox'

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Inbox Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
})

after(async () => { await mirror.close(); await teardownAll() })

async function finishRunFailed(agentId: string): Promise<void> {
  const token = signAgentToken({ agentId, companyId: COMPANY })
  const res = await fetch(`${mirror.baseUrl()}/runtime/runs/run-${randomUUID().slice(0, 6)}/finish`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify({ status: 'failed', error: 'engine exploded' }),
  })
  assert.equal(res.status, 200)
}

test('[mirror-inbox] starts empty with zero counts and no mutes', async () => {
  const r = await call('/inbox')
  assert.equal(r.status, 200)
  assert.deepEqual(r.json.items, [])
  assert.deepEqual(r.json.counts, { actionRequired: 0, attention: 0, info: 0 })
  assert.deepEqual(r.json.mutedTypes, [])
})

test('[mirror-inbox] run failed → action_required to the company owner', async () => {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ('a-inbox-1', $1, 'agent', 'Atlas', 'tester', 'A', '#abcdef', 'avail')`,
    [COMPANY],
  )
  await finishRunFailed('a-inbox-1')
  const r = await call('/inbox')
  assert.equal(r.json.items.length, 1)
  const it = r.json.items[0]
  assert.equal(it.severity, 'action_required')
  assert.equal(it.type, 'run.failed')
  assert.equal(it.read, false)
  assert.match(it.title, /Atlas/)
  assert.match(it.body, /engine exploded/)
  assert.equal(it.linkKind, 'observability')
  assert.equal(r.json.counts.actionRequired, 1)
})

test('[mirror-inbox] run completed → no item (quiet path stays quiet)', async () => {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ('a-inbox-2', $1, 'agent', 'Iris', 'tester', 'I', '#abcdef', 'avail')`,
    [COMPANY],
  )
  const token = signAgentToken({ agentId: 'a-inbox-2', companyId: COMPANY })
  const res = await fetch(`${mirror.baseUrl()}/runtime/runs/run-ok/finish`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify({ status: 'completed' }),
  })
  assert.equal(res.status, 200)
  assert.deepEqual((await call('/inbox')).json.items, [])
})

test('[mirror-inbox] card assigned to human → attention to the assignee', async () => {
  const human = `u-assignee-${randomUUID().slice(0, 6)}`
  await seedUserMembership(human, COMPANY)
  const boardId = `bd-${randomUUID().slice(0, 6)}`
  const colId = `col-${randomUUID().slice(0, 6)}`
  await pool.query(`INSERT INTO boards (id, company_id, title, created_by) VALUES ($1, $2, 'Ops', $3)`, [boardId, COMPANY, USER])
  await pool.query(`INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, 'To Do', 0)`, [colId, boardId])
  const card = (await call(`/boards/${boardId}/cards`, {
    method: 'POST', body: JSON.stringify({ title: 'Ship it', columnId: colId }),
  })).body
  const moved = await call(`/boards/${boardId}/cards/${card.id}`, {
    method: 'PATCH', body: JSON.stringify({ assigneeId: human }),
  })
  assert.equal(moved.status, 200)
  const r = await call('/inbox')
  assert.equal(r.json.items.length, 1)
  assert.equal(r.json.items[0].severity, 'attention')
  assert.equal(r.json.items[0].type, 'card.assigned')
  assert.match(r.json.items[0].title, /Ship it/)
})

test('[mirror-inbox] card into a ready-for-human column → action_required to owner', async () => {
  const boardId = `bd-${randomUUID().slice(0, 6)}`
  const src1 = `col-${randomUUID().slice(0, 6)}`
  const dst = `col-${randomUUID().slice(0, 6)}`
  await pool.query(`INSERT INTO boards (id, company_id, title, created_by) VALUES ($1, $2, 'Triage', $3)`, [boardId, COMPANY, USER])
  await pool.query(`INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, 'Doing', 0)`, [src1, boardId])
  await pool.query(`INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, 'Ready for Human', 1)`, [dst, boardId])
  const card = (await call(`/boards/${boardId}/cards`, {
    method: 'POST', body: JSON.stringify({ title: 'Decide the pricing', columnId: src1 }),
  })).body
  const moved = await call(`/boards/${boardId}/cards/${card.id}`, {
    method: 'PATCH', body: JSON.stringify({ columnId: dst }),
  })
  assert.equal(moved.status, 200)
  const r = await call('/inbox')
  assert.equal(r.json.items.length, 1)
  assert.equal(r.json.items[0].severity, 'action_required')
  assert.equal(r.json.items[0].type, 'card.needs-human')
})

test('[mirror-inbox] mutes round-trip and do not suppress ledger rows', async () => {
  const set = await call('/inbox/mutes', { method: 'PUT', body: JSON.stringify({ types: ['run.failed', 'run.failed', ''] }) })
  assert.equal(set.status, 200)
  const got = await call('/inbox/mutes')
  assert.deepEqual(got.json.types, ['run.failed']) // 去重 + 空串清洗

  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ('a-inbox-3', $1, 'agent', 'Nova', 'tester', 'N', '#abcdef', 'avail')`,
    [COMPANY],
  )
  await finishRunFailed('a-inbox-3')
  // 静音只拦推送/弹条,不拦落账 —— 列表仍有一条(透明可查)。
  const r = await call('/inbox')
  assert.equal(r.json.items.length, 1)
  assert.deepEqual(r.json.mutedTypes, ['run.failed'])
})

test('[mirror-inbox] mark read: single + read-all, idempotent 404 on foreign id', async () => {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ('a-inbox-4', $1, 'agent', 'Echo', 'tester', 'E', '#abcdef', 'avail')`,
    [COMPANY],
  )
  await finishRunFailed('a-inbox-4')
  const list = await call('/inbox')
  const id = list.json.items[0].id
  assert.equal((await call(`/inbox/${id}/read`, { method: 'POST' })).status, 200)
  assert.equal((await call(`/inbox/${id}/read`, { method: 'POST' })).status, 404) // 已读幂等 → 404
  const after1 = await call('/inbox')
  assert.equal(after1.json.items[0].read, true)
  assert.equal(after1.json.counts.actionRequired, 0)

  await finishRunFailed('a-inbox-4')
  await finishRunFailed('a-inbox-4')
  assert.equal((await call('/inbox/read-all', { method: 'POST' })).status, 200)
  const after2 = await call('/inbox')
  assert.ok(after2.json.items.every((it: any) => it.read))
})
