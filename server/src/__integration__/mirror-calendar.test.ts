/**
 * 验收镜像 · 日历域(#57)—— CRUD + 隐私可见性 + 手动 dispatch + 历史。
 * 双跑:CUMORA_MIRROR_BASE 指向 Go 候选(须 CUMORA_GO_FAKE_AUTH=1 起动)。
 * 调度 tick(到期扫描/reminder)归 #60,不在本套件。
 */

import assert from 'node:assert/strict'
import { after, beforeEach, test } from 'node:test'
import { pool } from '../db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror,teardownAll, 
} from './_helpers.js'

const USER = 'u-mirror-cal'
const COMPANY = 'c-mirror-cal'

async function seedCompanyAndUser(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Cal Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
}

async function seedAgent(id: string): Promise<void> {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', $3, 'A', '#123456', 'offline')`,
    [id, COMPANY, id],
  )
}

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUser()
})

after(async () => { await mirror.close(); await teardownAll() })

test('[mirror] calendar list starts empty', async () => {
  const r = await call('/calendar/events')
  assert.equal(r.status, 200)
  assert.deepEqual(r.json.events, [])
})

test('[mirror] create defaults + full validation matrix', async () => {
  const ok = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: '  Standup  ', startAt: '2026-09-01T01:00:00Z' }),
  })
  assert.equal(ok.status, 201)
  const ev = ok.json.event
  assert.equal(ev.title, 'Standup')
  assert.equal(ev.kind, 'personal')
  assert.equal(ev.status, 'active')
  assert.equal(ev.allDay, false)
  assert.equal(ev.recurrence, null)
  assert.equal(ev.reminderMinutesBefore, null)
  assert.equal(ev.isPrivate, false)
  assert.equal(ev.createdBy, USER)

  assert.equal((await call('/calendar/events', { method: 'POST', body: JSON.stringify({ startAt: '2026-09-01T01:00:00Z' }) })).status, 400)
  assert.equal((await call('/calendar/events', { method: 'POST', body: JSON.stringify({ title: 'x' }) })).status, 400)
  assert.equal((await call('/calendar/events', { method: 'POST', body: JSON.stringify({ title: 'x', startAt: 'not-a-date' }) })).status, 400)
  assert.equal((await call('/calendar/events', { method: 'POST', body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', kind: 'agent_task' }) })).status, 400)
  assert.equal((await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', assigneeId: 'p-ghost' }),
  })).status, 400)
  assert.equal((await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', targetConversationId: 'conv-ghost' }),
  })).status, 400)
  assert.equal((await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', reminderMinutesBefore: 10 }),
  })).status, 400)
  assert.equal((await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', reminderChannel: 'toast' }),
  })).status, 400)
  assert.equal((await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', reminderChannel: 'sms' }),
  })).status, 400)
  assert.equal((await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', recurrence: { freq: 'hourly' } }),
  })).status, 400)
  assert.equal((await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'x', startAt: '2026-09-01T01:00:00Z', recurrence: { freq: 'daily', count: 0 } }),
  })).status, 400)
  const rec = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({
      title: 'weekly', startAt: '2026-09-01T01:00:00Z',
      recurrence: { freq: 'weekly', interval: 2, byweekday: [1, 3, 99] },
      reminderMinutesBefore: 30, reminderChannel: 'both',
    }),
  })
  assert.equal(rec.status, 201)
  // TS parseRecurrence 恒含 until/count 键(缺省 null);byweekday 仅非空时出现
  assert.deepEqual(rec.json.event.recurrence, {
    freq: 'weekly', interval: 2, byweekday: [1, 3], until: null, count: null,
  })
  assert.equal(rec.json.event.reminderChannel, 'both')

  // 显式 null 语义:title:null → 400;description/agentPrompt:null → NULL;reminder 单字段 null → 跳过
  assert.equal((await call('/calendar/events', {
    method: 'POST', body: JSON.stringify({ title: null, startAt: '2026-09-01T01:00:00Z' }),
  })).status, 400)
  const nulled = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'n', startAt: '2026-09-01T01:00:00Z', description: null, agentPrompt: null, reminderChannel: null, reminderMinutesBefore: null }),
  })
  assert.equal(nulled.status, 201)
  assert.equal(nulled.json.event.description, null)
  assert.equal(nulled.json.event.agentPrompt, null)
  assert.equal(nulled.json.event.reminderChannel, null)
  // until 宽限:date-only
  const untilDate = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'u', startAt: '2026-09-01T01:00:00Z', recurrence: { freq: 'daily', until: '2026-12-31' } }),
  })
  assert.equal(untilDate.status, 201)
  assert.equal(untilDate.json.event.recurrence.until, '2026-12-31T00:00:00.000Z')
  // 字符串数字:interval/count
  const strNums = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 's', startAt: '2026-09-01T01:00:00Z', recurrence: { freq: 'daily', interval: '2', count: '3' } }),
  })
  assert.equal(strNums.status, 201)
  assert.equal(strNums.json.event.recurrence.interval, 2)
  assert.equal(strNums.json.event.recurrence.count, 3)

  // 空白保留:description/agentPrompt 不 trim(String.slice 语义)
  const spaced = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'sp', startAt: '2026-09-01T01:00:00Z', description: ' hello ', agentPrompt: ' pm ' }),
  })
  assert.equal(spaced.status, 201)
  assert.equal(spaced.json.event.description, ' hello ')
  assert.equal(spaced.json.event.agentPrompt, ' pm ')

  // TS 强转语义:数值 floor、字符串数字、标量标题、date-only
  const coerced = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({
      title: 42, startAt: '2026-09-01',
      recurrence: { freq: 'weekly', byweekday: ['1', '3'], count: 2.7 },
      reminderMinutesBefore: 10.5, reminderChannel: 'toast',
    }),
  })
  assert.equal(coerced.status, 201)
  assert.equal(coerced.json.event.title, '42')
  assert.equal(coerced.json.event.startAt, '2026-09-01T00:00:00.000Z')
  assert.deepEqual(coerced.json.event.recurrence.byweekday, [1, 3])
  assert.equal(coerced.json.event.recurrence.count, 2)
  assert.equal(coerced.json.event.reminderMinutesBefore, 10)
  // 列表窗口:date-only 亦生效;无循环的过去事件被排除,循环+active 穿窗
  const plain = await call('/calendar/events', {
    method: 'POST', body: JSON.stringify({ title: 'plain', startAt: '2026-09-01T01:00:00Z' }),
  })
  assert.equal(plain.status, 201)
  const windowed = await call(`/calendar/events?from=${encodeURIComponent('2026-09-02')}`)
  assert.equal(windowed.status, 200)
  assert.ok(!windowed.json.events.some((e: any) => e.id === plain.json.event.id))
  assert.ok(windowed.json.events.some((e: any) => e.id === coerced.json.event.id))
})

test('[mirror] privacy: private rows hidden from others, visible to creator/assignee; owner sees agent-private', async () => {
  await seedAgent('ag-cal-1')
  const mine = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'secret', startAt: '2026-09-01T01:00:00Z', isPrivate: true }),
  })
  assert.equal(mine.status, 201)
  const id = mine.json.event.id
  assert.equal((await call(`/calendar/events/${id}`)).status, 200)

  const agentPrivate = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({
      title: 'agent secret', startAt: '2026-09-01T01:00:00Z', isPrivate: true,
      kind: 'agent_task', assigneeId: 'ag-cal-1',
    }),
  })
  assert.equal(agentPrivate.status, 201)

  // 另一个普通成员视角
  const other = 'u-mirror-cal-2'
  await pool.query(`INSERT INTO users (id, email, display_name) VALUES ($1,'m2@test.local','M2')`, [other])
  await pool.query(`INSERT INTO company_members (company_id, user_id, role) VALUES ($1,$2,'member')`, [COMPANY, other])
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status) VALUES ($1,$2,'human','M2','M','#654321','offline')`,
    [other, COMPANY],
  )
  const mirror2 = startMirror(other, COMPANY)
  const call2 = mirror2.call
  try {
    assert.equal((await call2(`/calendar/events/${id}`)).status, 404)
    assert.equal((await call2(`/calendar/events/${agentPrivate.json.event.id}`)).status, 404)
    const list2 = await call2('/calendar/events')
    assert.equal(list2.json.events.length, 0)
    // 修改/删除同样 404
    assert.equal((await call2(`/calendar/events/${id}`, { method: 'PATCH', body: JSON.stringify({ title: 'hijack' }) })).status, 404)
    assert.equal((await call2(`/calendar/events/${id}`, { method: 'DELETE' })).status, 404)
  } finally {
    await mirror2.close()
  }
  // creator(= owner)能看到 agent 私密行
  assert.equal((await call(`/calendar/events/${agentPrivate.json.event.id}`)).status, 200)
})

test('[mirror] patch partial fields; bad values 400; empty patch 400', async () => {
  const created = await call('/calendar/events', {
    method: 'POST', body: JSON.stringify({ title: 't', startAt: '2026-09-01T01:00:00Z' }),
  })
  const id = created.json.event.id
  const p1 = await call(`/calendar/events/${id}`, {
    method: 'PATCH', body: JSON.stringify({ title: 'renamed', status: 'paused' }),
  })
  assert.equal(p1.status, 200)
  assert.equal(p1.json.event.title, 'renamed')
  assert.equal(p1.json.event.status, 'paused')

  assert.equal((await call(`/calendar/events/${id}`, { method: 'PATCH', body: JSON.stringify({ title: '  ' }) })).status, 400)
  assert.equal((await call(`/calendar/events/${id}`, { method: 'PATCH', body: JSON.stringify({ status: 'zzz' }) })).status, 400)
  assert.equal((await call(`/calendar/events/${id}`, { method: 'PATCH', body: JSON.stringify({ startAt: 'nope' }) })).status, 400)
  assert.equal((await call(`/calendar/events/${id}`, { method: 'PATCH', body: JSON.stringify({}) })).status, 400)

  const cleared = await call(`/calendar/events/${id}`, {
    method: 'PATCH', body: JSON.stringify({ recurrence: null, reminderChannel: null }),
  })
  assert.equal(cleared.status, 200)
  assert.equal(cleared.json.event.recurrence, null)
})

test('[mirror] delete removes; second delete 404', async () => {
  const created = await call('/calendar/events', {
    method: 'POST', body: JSON.stringify({ title: 't', startAt: '2026-09-01T01:00:00Z' }),
  })
  const id = created.json.event.id
  assert.equal((await call(`/calendar/events/${id}`, { method: 'DELETE' })).status, 200)
  assert.equal((await call(`/calendar/events/${id}`)).status, 404)
  assert.equal((await call(`/calendar/events/${id}`, { method: 'DELETE' })).status, 404)
})

test('[mirror] run-now: personal skips; agent_task dispatches into pinned convo; history recorded', async () => {
  await seedAgent('ag-cal-2')
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members) VALUES ('conv-cal-1',$1,'group','Room',$2::jsonb)`,
    [COMPANY, JSON.stringify([USER, 'ag-cal-2'])],
  )
  const personal = await call('/calendar/events', {
    method: 'POST', body: JSON.stringify({ title: 'mine', startAt: '2026-09-01T01:00:00Z' }),
  })
  const r1 = await call(`/calendar/events/${personal.json.event.id}/run-now`, { method: 'POST' })
  assert.equal(r1.status, 200)
  assert.equal(r1.json.status, 'skipped')

  const task = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({
      title: 'daily report', description: ' summarize ', agentPrompt: 'do it',
      startAt: '2026-09-01T01:00:00Z', kind: 'agent_task',
      assigneeId: 'ag-cal-2', targetConversationId: 'conv-cal-1',
    }),
  })
  assert.equal(task.status, 201)
  const r2 = await call(`/calendar/events/${task.json.event.id}/run-now`, { method: 'POST' })
  assert.equal(r2.status, 200)
  assert.equal(r2.json.status, 'dispatched')
  assert.equal(r2.json.conversationId, 'conv-cal-1')
  assert.ok(r2.json.messageId)

  const msg = await pool.query(`SELECT author_id, kind, body FROM messages WHERE id = $1`, [r2.json.messageId])
  assert.equal(msg.rows[0].author_id, 'calendar')
  assert.equal(msg.rows[0].kind, 'system')
  const body = JSON.parse(msg.rows[0].body)
  assert.equal(body.kind, 'calendar_event')
  assert.equal(body.eventId, task.json.event.id)
  assert.equal(body.title, 'daily report')
  assert.equal(body.description, 'summarize')

  const hist = await call(`/calendar/events/${task.json.event.id}/dispatches`)
  assert.equal(hist.status, 200)
  const dispatchedRow = hist.json.dispatches.find((d: any) => d.status === 'dispatched')
  assert.ok(dispatchedRow)
  assert.equal(dispatchedRow.conversationId, 'conv-cal-1')
  assert.equal(dispatchedRow.messageId, r2.json.messageId)
  // personal 事件的 skipped 历史在其自己的 dispatches 里
  const histPersonal = await call(`/calendar/events/${personal.json.event.id}/dispatches`)
  assert.ok(histPersonal.json.dispatches.some((d: any) => d.status === 'skipped' && d.error === 'personal event'))
})

test('[mirror] run-now fallback to existing DM; no DM → skipped', async () => {
  await seedAgent('ag-cal-3')
  // 既有 DM
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members) VALUES ('conv-dm-1',$1,'direct','',$2::jsonb)`,
    [COMPANY, JSON.stringify([USER, 'ag-cal-3'])],
  )
  const task = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({ title: 'dm task', startAt: '2026-09-01T01:00:00Z', kind: 'agent_task', assigneeId: 'ag-cal-3' }),
  })
  const r = await call(`/calendar/events/${task.json.event.id}/run-now`, { method: 'POST' })
  assert.equal(r.json.status, 'dispatched')
  assert.equal(r.json.conversationId, 'conv-dm-1')

  // assignee 不在钉定会话 → skipped(assignee 非成员)
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members) VALUES ('conv-cal-2',$1,'group','NoAgent',$2::jsonb)`,
    [COMPANY, JSON.stringify([USER])],
  )
  const pinned = await call('/calendar/events', {
    method: 'POST',
    body: JSON.stringify({
      title: 'pinned', startAt: '2026-09-01T01:00:00Z', kind: 'agent_task',
      assigneeId: 'ag-cal-3', targetConversationId: 'conv-cal-2',
    }),
  })
  const r2 = await call(`/calendar/events/${pinned.json.event.id}/run-now`, { method: 'POST' })
  assert.equal(r2.json.status, 'skipped')
  assert.equal(r2.json.error, 'no target conversation')
})
