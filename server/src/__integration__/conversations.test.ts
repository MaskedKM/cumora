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
