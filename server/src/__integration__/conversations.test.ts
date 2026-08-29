/**
 * Integration tests for conversation list/search shaping.
 *
 * Direct conversation rows are shared by both participants, so the stored
 * `conversations.title` can only ever be correct for one viewer. The API must
 * return a viewer-specific title based on the other member instead.
 *
 * #70 TS 退役:请求面打向 MIRROR Go 服(x-test-user 伪造 auth 等价旧
 * in-process 盖章;x-company-id 原本就显式带)。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, MIRROR_BASE,
} from './_helpers.js'
import { pool } from '../db/pool.js'

const ME_USER_ID = 'u-me'
const OTHER_USER_ID = 'u-ada'
const baseUrl = MIRROR_BASE
const authHeaders = { 'x-test-user': ME_USER_ID }

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
})

after(async () => {
  await teardownAll()
})

async function seedHumanDirectWithSelfStoredTitle(): Promise<{ companyId: string; conversationId: string }> {
  const companyId = 'c-direct-title'
  const conversationId = 'direct-ada-yetone'
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id)
     VALUES ($1, 'Direct Title Co', 'direct-title-co', $2)`,
    [companyId, ME_USER_ID],
  )
  await seedUserMembership(ME_USER_ID, companyId, {
    email: 'yetone@test.local',
    displayName: 'Yetone',
  })
  await seedUserMembership(OTHER_USER_ID, companyId, {
    email: 'ada@test.local',
    displayName: 'Ada',
  })
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, tag, company_id)
     VALUES ($1, 'direct', 'Yetone', $2::jsonb, 'human', $3)`,
    [conversationId, JSON.stringify([OTHER_USER_ID, ME_USER_ID]), companyId],
  )
  return { companyId, conversationId }
}

test('[integration] GET /conversations returns the other member as a direct title', async () => {
  const { companyId, conversationId } = await seedHumanDirectWithSelfStoredTitle()

  const res = await fetch(`${baseUrl}/api/conversations`, {
    headers: { ...authHeaders, 'x-company-id': companyId },
  })
  assert.equal(res.status, 200)
  const rows = await res.json() as Array<{ id: string; title: string }>
  const direct = rows.find((r) => r.id === conversationId)

  assert.equal(direct?.title, 'Ada')
})

test('[integration] GET /search uses the same perspective-specific direct title', async () => {
  const { companyId, conversationId } = await seedHumanDirectWithSelfStoredTitle()

  const res = await fetch(`${baseUrl}/api/search?q=${encodeURIComponent('Ada')}`, {
    headers: { ...authHeaders, 'x-company-id': companyId },
  })
  assert.equal(res.status, 200)
  const body = await res.json() as { rooms: Array<{ id: string; title: string }> }
  const direct = body.rooms.find((r) => r.id === conversationId)

  assert.equal(direct?.title, 'Ada')
})

test('[mirror] #118 coercion: sendMessage String(body) + clientId gate + createGroup F-03', async () => {
  const companyId = 'c-coerce'
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id)
     VALUES ($1, 'Coerce Co', 'coerce-co', $2)`,
    [companyId, ME_USER_ID],
  )
  await seedUserMembership(ME_USER_ID, companyId, { email: 'me-coerce@test.local', displayName: 'Me' })
  await seedUserMembership(OTHER_USER_ID, companyId, { email: 'ada-coerce@test.local', displayName: 'Ada' })
  const H = { ...authHeaders, 'x-company-id': companyId, 'content-type': 'application/json' }

  // createGroup F-03:projectId truthy-but-trims-empty 落 ''(不再悄悄抬
  // NULL 装成功)——与 TS 同炸:749863e router.ts:2752 就是把 '' 发给
  // INSERT,撞 conversations_project_id_fkey → 500。
  const emojiTitle = '😀'.repeat(45) // 90 UTF-16 码元 → 截 80 → 40 字
  const gBad = await fetch(`${baseUrl}/api/conversations`, {
    method: 'POST', headers: H,
    body: JSON.stringify({ title: emojiTitle, members: [OTHER_USER_ID], projectId: '   ' }),
  })
  assert.equal(gBad.status, 500, 'F-03: empty-string projectId hits the projects FK, same as TS')

  const g = await fetch(`${baseUrl}/api/conversations`, {
    method: 'POST', headers: H,
    body: JSON.stringify({ title: emojiTitle, members: [OTHER_USER_ID] }),
  })
  assert.equal(g.status, 201)
  const gBody = await g.json() as { id: string; projectId: unknown }
  const { rows: convRows } = await pool.query(
    `SELECT title, project_id FROM conversations WHERE id = $1`, [gBody.id])
  assert.equal(convRows[0].project_id, null)
  assert.equal([...convRows[0].title].length, 40)
  assert.equal(gBody.projectId, null)

  // sendMessage:body 123 → "123"(TS String(x ?? '').trim(),#118 主体;
  // struct 解码曾整包丢弃落 'body required')。
  const send = await fetch(`${baseUrl}/api/conversations/${gBody.id}/messages`, {
    method: 'POST', headers: H, body: JSON.stringify({ body: 123 }),
  })
  assert.equal(send.status, 202)
  const sendBody = await send.json() as { id: string }
  const { rows: msgRows } = await pool.query(`SELECT body FROM messages WHERE id = $1`, [sendBody.id])
  assert.equal(msgRows[0].body, '123')

  // clientId 空/超长/非串 → 400 'invalid clientId'(TS router.ts 3295 门)。
  for (const clientId of ['', 'x'.repeat(81), 42]) {
    const bad = await fetch(`${baseUrl}/api/conversations/${gBody.id}/messages`, {
      method: 'POST', headers: H, body: JSON.stringify({ body: 'x', clientId }),
    })
    assert.equal(bad.status, 400)
    assert.equal(((await bad.json()) as { error: string }).error, 'invalid clientId')
  }

  // #141 rider:双空恒 'empty message'(TS router.ts:3284;Go 曾漂移
  // 'body required');畸形附件(缺 url/name)视同无附件,同落此门。
  for (const payload of [{}, { attachment: { url: 1 } }, { body: '   ' }]) {
    const bad = await fetch(`${baseUrl}/api/conversations/${gBody.id}/messages`, {
      method: 'POST', headers: H, body: JSON.stringify(payload),
    })
    assert.equal(bad.status, 400)
    assert.equal(((await bad.json()) as { error: string }).error, 'empty message')
  }
})
