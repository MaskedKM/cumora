/**
 * #137 conversation_members 影子表同步不变量。
 *
 * 迁移 0002 建立 conversation_members(规范化 membership),由触发器
 * 在 conversations.members 的每次写入时重建该会话的成员行。本文件
 * 从四面驱动写入——直接 SQL 种行、HTTP createGroup/openDirect/
 * addMember/leave、以及 SQL 侧数组 union(email 线程的修补机制形态)
 * ——断言两种表示始终一致,且会话删除后子行清空。读路径切换(下一
 * 张 PR)依赖此不变量成立。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, MIRROR_BASE,
} from './_helpers.js'
import { pool } from '../db/pool.js'

const ME = 'u-mm-me'
const OTHER = 'u-mm-ada'
const THIRD = 'u-mm-bram'
const COMPANY = 'c-members-sync'

const authHeaders = {
  'content-type': 'application/json',
  'x-company-id': COMPANY,
  'x-test-user': ME,
}

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id)
     VALUES ($1, 'Members Sync Co', 'members-sync-co', $2)`,
    [COMPANY, ME],
  )
  for (const u of [ME, OTHER, THIRD]) {
    await seedUserMembership(u, COMPANY)
  }
})

after(async () => {
  await teardownAll()
})

/** 两种表示必须严格同集(jsonb 允许重复 id,影子表 PK 去重)。会话
 * 不存在时直接炸——两边皆空的"同集"是空洞通过,不能当绿。 */
async function assertInSync(conversationId: string): Promise<void> {
  const jsonb = await pool.query(
    `SELECT jsonb_array_elements_text(members) AS m FROM conversations WHERE id = $1`,
    [conversationId],
  )
  const exists = await pool.query(
    `SELECT 1 FROM conversations WHERE id = $1`,
    [conversationId],
  )
  assert.equal(exists.rowCount, 1, `conversation ${conversationId} not found`)
  const shadow = await pool.query(
    `SELECT participant_id FROM conversation_members WHERE conversation_id = $1`,
    [conversationId],
  )
  const a = jsonb.rows.map((r: { m: string }) => r.m).sort()
  const b = shadow.rows.map((r: { participant_id: string }) => r.participant_id).sort()
  assert.deepEqual(b, [...new Set(a)], `shadow table out of sync for ${conversationId}`)
}

test('[mirror] #137 direct SQL seed row syncs to shadow table', async () => {
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id)
     VALUES ('mm-sql', 'group', 'sql seeded', $1::jsonb, $2)`,
    [JSON.stringify([ME, OTHER, ME]), COMPANY],  // 重复 id 防御:jsonb 允许,PK 去重
  )
  await assertInSync('mm-sql')
})

test('[mirror] #137 HTTP createGroup / addMember / leave keep shadow in sync', async () => {
  const create = await fetch(`${MIRROR_BASE}/api/conversations`, {
    method: 'POST', headers: authHeaders,
    body: JSON.stringify({ title: 'sync group', members: [OTHER] }),
  })
  const createText = await create.text()
  assert.equal(create.status, 201, createText)
  const { id } = JSON.parse(createText) as { id: string }
  await assertInSync(id)

  const add = await fetch(`${MIRROR_BASE}/api/conversations/${id}/members`, {
    method: 'POST', headers: authHeaders,
    body: JSON.stringify({ id: THIRD }),
  })
  assert.equal(add.status, 200, await add.text())
  await assertInSync(id)

  const leave = await fetch(`${MIRROR_BASE}/api/conversations/${id}/leave`, {
    method: 'POST', headers: authHeaders,
  })
  assert.equal(leave.status, 200, await leave.text())
  await assertInSync(id)
})

test('[mirror] #137 openDirect keeps shadow in sync', async () => {
  const res = await fetch(`${MIRROR_BASE}/api/conversations/direct`, {
    method: 'POST', headers: authHeaders,
    body: JSON.stringify({ otherId: OTHER }),
  })
  const resText = await res.text()
  assert.ok(res.status === 200 || res.status === 201, resText)
  const body = JSON.parse(resText) as { id: string }
  await assertInSync(body.id)
})

test('[mirror] #137 SQL-side members union (email thread shape) syncs', async () => {
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id)
     VALUES ('mm-email', 'email', 'thread', $1::jsonb, $2)`,
    [JSON.stringify([ME]), COMPANY],
  )
  // 与 email.go FindOrCreateEmailConversation 相同形状的 SQL 侧 union。
  await pool.query(`
    UPDATE conversations SET members = (
      SELECT to_jsonb(ARRAY(
        SELECT DISTINCT m FROM (
          SELECT jsonb_array_elements_text(members) AS m
          UNION
          SELECT unnest($2::text[]) AS m
        ) u
      ))
    ) WHERE id = $1`,
    ['mm-email', [OTHER, THIRD]],
  )
  await assertInSync('mm-email')
})

test('[mirror] #137 deleting the conversation drops shadow rows', async () => {
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id)
     VALUES ('mm-del', 'group', 'doomed', $1::jsonb, $2)`,
    [JSON.stringify([ME, OTHER]), COMPANY],
  )
  await pool.query(`DELETE FROM conversations WHERE id = 'mm-del'`)
  const left = await pool.query(
    `SELECT count(*)::int AS n FROM conversation_members WHERE conversation_id = 'mm-del'`,
  )
  assert.equal(left.rows[0].n, 0)
})
