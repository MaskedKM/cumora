/**
 * 验收镜像 · conversations 深面 + convene + reactions + email(#49 评审 MUST:
 * 补齐 convene/reactions/email-send 等零覆盖域)。
 */
import { test, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { pool } from './harness/db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror,
} from './_helpers.js'

const USER = 'u-mirror-conv'
const COMPANY = 'c-mirror-conv'

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

async function seedAgent(id: string): Promise<string> {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status, system_prompt)
     VALUES ($1, $2, 'agent', $3, 'A', '#111111', 'resting', 'You are terse.')`,
    [id, COMPANY, id],
  )
  return id
}

test('[mirror] reactions: send message → toggle → array shape', async () => {
  const peer = await seedAgent('a-react-peer')
  const conv = await call('/conversations', {
    method: 'POST',
    body: JSON.stringify({ title: 'React Room', members: [USER, peer] }),
  })
  assert.equal(conv.status, 201)
  const sent = await call(`/conversations/${conv.json.id}/messages`, {
    method: 'POST',
    body: JSON.stringify({ body: 'react to me' }),
  })
  assert.ok([200, 202].includes(sent.status), `send=${sent.status}`)
  const reacted = await call(`/messages/${sent.json.id}/reactions`, {
    method: 'POST',
    body: JSON.stringify({ emoji: '🎉' }),
  })
  assert.equal(reacted.status, 200)
  assert.ok(Array.isArray(reacted.json.reactions))
  assert.equal(reacted.json.reactions[0].emoji, '🎉')
  assert.equal(reacted.json.reactions[0].count, 1)
})

test('[mirror] convene: start → active → transcript(list shape)', async () => {
  const agentA = await seedAgent('a-conv-1')
  await seedAgent('a-conv-2')
  const conv = await call('/conversations', {
    method: 'POST',
    body: JSON.stringify({ title: 'Convene Room', members: [USER, agentA, 'a-conv-2'] }),
  })
  assert.equal(conv.status, 201)
  const started = await call(`/conversations/${conv.json.id}/convene`, {
    method: 'POST',
    body: JSON.stringify({ topic: 'mirror topic' }),
  })
  assert.ok([200, 201].includes(started.status), `convene start=${started.status}`)
  assert.ok(started.json.id)
  assert.equal(started.json.state, 'live')
  const active = await call(`/conversations/${conv.json.id}/convene`)
  assert.equal(active.status, 200)
  const transcript = await call(`/convene/${started.json.id}/transcript`)
  assert.equal(transcript.status, 200)
  assert.ok(Array.isArray(transcript.json))
})

test('[mirror] convene: numeric topic coerces via String semantics (F16)', async () => {
  const agentA = await seedAgent('a-conv-num')
  const conv = await call('/conversations', {
    method: 'POST',
    body: JSON.stringify({ title: 'Coerce Room', members: [USER, agentA] }),
  })
  assert.equal(conv.status, 201)
  const started = await call(`/conversations/${conv.json.id}/convene`, {
    method: 'POST',
    body: JSON.stringify({ topic: 123 }),
  })
  assert.ok([200, 201].includes(started.status), `start=${started.status}`)
  // TS String(123) === '123' —— flair 即 topic 截 80。
  assert.equal(started.json.flair, '123')
})

test('[mirror] convene: restart supersedes prior live session', async () => {
  const agentA = await seedAgent('a-conv-sup')
  const conv = await call('/conversations', {
    method: 'POST',
    body: JSON.stringify({ title: 'Supersede Room', members: [USER, agentA] }),
  })
  assert.equal(conv.status, 201)
  const s1 = await call(`/conversations/${conv.json.id}/convene`, {
    method: 'POST',
    body: JSON.stringify({ topic: 'first' }),
  })
  assert.ok([200, 201].includes(s1.status))
  const s2 = await call(`/conversations/${conv.json.id}/convene`, {
    method: 'POST',
    body: JSON.stringify({ topic: 'second' }),
  })
  assert.ok([200, 201].includes(s2.status))
  const active = await call(`/conversations/${conv.json.id}/convene`)
  assert.equal(active.status, 200)
  assert.equal(active.json.id, s2.json.id)
  assert.equal(active.json.state, 'live')
})

test('[mirror] convene: TTL sweep reaps zombie live sessions (F6)', async () => {
  const agentA = await seedAgent('a-conv-ttl')
  const conv = await call('/conversations', {
    method: 'POST',
    body: JSON.stringify({ title: 'TTL Room', members: [USER, agentA] }),
  })
  assert.equal(conv.status, 201)
  const s1 = await call(`/conversations/${conv.json.id}/convene`, {
    method: 'POST',
    body: JSON.stringify({ topic: 'stale' }),
  })
  assert.ok([200, 201].includes(s1.status))
  await pool.query(
    `UPDATE convene_sessions SET started_at = NOW() - INTERVAL '31 minutes' WHERE id = $1`,
    [s1.json.id],
  )
  const active = await call(`/conversations/${conv.json.id}/convene`)
  assert.equal(active.status, 200)
  assert.equal(active.json, null)
})

test('[mirror] conversation create/title-set coerce non-string values (F16)', async () => {
  const peer = await seedAgent('a-conv-coerce')
  // TS String(42) === '42':非串标题不再 400。
  const conv = await call('/conversations', {
    method: 'POST',
    body: JSON.stringify({ title: 42, members: [USER, peer] }),
  })
  assert.equal(conv.status, 201)
  const renamed = await call(`/conversations/${conv.json.id}/title`, {
    method: 'POST',
    body: JSON.stringify({ title: 7 }),
  })
  assert.equal(renamed.status, 200)
  assert.equal(renamed.json.title, '7')
})

test('[mirror] projects create coerces non-string name/description/color (F16)', async () => {
  const res = await call('/projects', {
    method: 'POST',
    body: JSON.stringify({ name: 123, description: true, color: 456 }),
  })
  assert.equal(res.status, 201)
  assert.equal(res.json.name, '123')
  assert.equal(res.json.description, 'true')
  assert.equal(res.json.color, '456')
  // JS truthy 门:0/''/null → color null。
  const falsy = await call('/projects', {
    method: 'POST',
    body: JSON.stringify({ name: 'plain', color: 0 }),
  })
  assert.equal(falsy.status, 201)
  assert.equal(falsy.json.color, null)
})

test('[mirror] invitation create with sendEmail reports emailDelivery (F11, mock)', async () => {
  const res = await call(`/companies/${COMPANY}/invitations`, {
    method: 'POST',
    body: JSON.stringify({ email: 'invitee@example.com', sendEmail: true }),
  })
  assert.equal(res.status, 201)
  // EMAIL_DOMAIN 已配 + RESEND_API_KEY 空 → mock 发送成功。
  assert.deepEqual(res.json.emailDelivery, {
    attempted: true, ok: true, error: null, skipped: null,
  })
})

test('[mirror] email send returns transport envelope (mock mode)', async () => {
  const sent = await call('/email/send', {
    method: 'POST',
    body: JSON.stringify({ to: ['someone@example.com'], subject: 'Mirror test', body: 'hello' }),
  })
  assert.equal(sent.status, 200)
  assert.ok(sent.json.messageId)
  assert.ok(sent.json.conversationId)
  assert.equal(typeof sent.json.transportStatus, 'string')
})

test('[mirror] email reply on the sent thread', async () => {
  const sent = await call('/email/send', {
    method: 'POST',
    body: JSON.stringify({ to: ['someone@example.com'], subject: 'Reply thread', body: 'first' }),
  })
  assert.equal(sent.status, 200)
  const reply = await call(`/email/reply/${sent.json.messageId}`, {
    method: 'POST',
    body: JSON.stringify({ body: 'a reply' }),
  })
  // 无配置 EMAIL_DOMAIN/发件地址时 baseline 报 400 'no email address…';
  // 配置了则 200 envelope。两者都是合法线上行为,镜像接受并校验形状。
  if (reply.status === 200) {
    assert.equal(reply.json.conversationId, sent.json.conversationId)
  } else {
    assert.equal(reply.status, 400)
    assert.ok(typeof reply.json.error === 'string')
  }
})

test('[mirror] board comments lifecycle', async () => {
  const board = await call('/boards', { method: 'POST', body: JSON.stringify({ title: 'Cmt Board' }) })
  const col = await call(`/boards/${board.json.id}/columns`, { method: 'POST', body: JSON.stringify({ title: 'C1' }) })
  const card = await call(`/boards/${board.json.id}/cards`, {
    method: 'POST',
    body: JSON.stringify({ columnId: col.json.id, title: 'Cmt card' }),
  })
  const added = await call(`/boards/${board.json.id}/cards/${card.json.id}/comments`, {
    method: 'POST',
    body: JSON.stringify({ body: 'a comment' }),
  })
  assert.equal(added.status, 200)
  assert.ok(added.json.id)
  const list = await call(`/boards/${board.json.id}/cards/${card.json.id}/comments`)
  assert.equal(list.status, 200)
  assert.ok(Array.isArray(list.json) && list.json.length === 1)
  const del = await call(`/boards/${board.json.id}/cards/${card.json.id}/comments/${added.json.id}`, { method: 'DELETE' })
  assert.equal(del.status, 200)
})

test('[mirror] calendar run-now returns dispatch envelope', async () => {
  const ev = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({
      title: 'Mirror run', kind: 'personal', startAt: new Date(Date.now() + 3600_000).toISOString(),
    }),
  })
  assert.equal(ev.status, 201)
  const run = await call(`/calendar/events/${ev.json.event.id}/run-now`, { method: 'POST' })
  assert.equal(run.status, 200)
  assert.equal(typeof run.json.status, 'string')
})

test('[mirror] auth ws-ticket mints one-shot ticket', async () => {
  const r = await call('/auth/ws-ticket', { method: 'POST', body: '{}' })
  assert.equal(r.status, 200)
  assert.ok(typeof r.json.ticket === 'string' && r.json.ticket.length > 0)
})
