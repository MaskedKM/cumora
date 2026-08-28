/**
 * Integration tests: message delivery idempotency (#70 MIRROR-only 化)。
 *
 * 原 TS 形态经 CUMORA_TEST_MESSAGE_FAULTS + x-cumora-test-message-fault
 * 头在进程内注入"commit 后丢 ack"故障;Go 服未实现该测试门(见 PR 说明)。
 * 保留的可移植语义:同 clientId 重发幂等(重试返回原消息、不双落行)
 * 与并发同 clientId 恰好一条。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, MIRROR_BASE,
} from './_helpers.js'
import { pool } from '../db/pool.js'

const USER_ID = 'u-delivery-test'
const COMPANY_ID = 'c-delivery-test'
const CONVERSATION_ID = 'g-delivery-test'
const baseUrl = MIRROR_BASE
const authHeaders = { 'x-test-user': USER_ID }

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id)
     VALUES ($1, 'Delivery Test', 'delivery-test', $2)`,
    [COMPANY_ID, USER_ID],
  )
  await seedUserMembership(USER_ID, COMPANY_ID)
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id)
     VALUES ($1, 'group', 'Delivery Test', $2::jsonb, $3)`,
    [CONVERSATION_ID, JSON.stringify([USER_ID]), COMPANY_ID],
  )
})

after(async () => {
  await teardownAll()
})

test('[integration] resending with the same clientId returns the original message', async () => {
  const first = await fetch(`${baseUrl}/api/conversations/${CONVERSATION_ID}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': COMPANY_ID },
    body: JSON.stringify({ body: 'delivery probe', clientId: 'retry-probe-1' }),
  })
  assert.equal(first.status, 202)
  const original = await first.json() as { id: string; sequence: number }
  // 复用短路的可观测代理:首发后的 updated_at 在重试后必须原样。
  const { rows: before } = await pool.query<{ updated_at: Date }>(
    `SELECT updated_at FROM conversations WHERE id = $1`,
    [CONVERSATION_ID],
  )

  // A lost-ACK retry replays the exact same request: same clientId, same body.
  const retry = await fetch(`${baseUrl}/api/conversations/${CONVERSATION_ID}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': COMPANY_ID },
    body: JSON.stringify({ body: 'delivery probe', clientId: 'retry-probe-1' }),
  })
  assert.equal(retry.status, 202)
  const retried = await retry.json() as { id: string; sequence: number }
  assert.equal(retried.id, original.id)
  assert.equal(retried.sequence, original.sequence)
  // TS 短路语义:复用路径不 bump updated_at(也不重播 message.new/重推
  // ——副作用面以 updated_at 为可观测代理)。
  await new Promise((r) => setTimeout(r, 150))
  const { rows: after } = await pool.query<{ updated_at: Date }>(
    `SELECT updated_at FROM conversations WHERE id = $1`,
    [CONVERSATION_ID],
  )
  assert.equal(after[0]?.updated_at?.getTime(), before[0]?.updated_at?.getTime(),
    'retry with same clientId must not re-fire conversation side effects')

  const { rows } = await pool.query<{ id: string; author_id: string; body: string; client_id: string }>(
    `SELECT id, author_id, body, client_id FROM messages WHERE conversation_id = $1`,
    [CONVERSATION_ID],
  )
  assert.deepEqual(rows, [{
    id: original.id,
    author_id: USER_ID,
    body: 'delivery probe',
    client_id: 'retry-probe-1',
  }])
})

test('[integration] concurrent requests with the same client id create one message', async () => {
  const send = () => fetch(`${baseUrl}/api/conversations/${CONVERSATION_ID}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': COMPANY_ID },
    body: JSON.stringify({ body: 'concurrent delivery probe', clientId: 'concurrent-probe-1' }),
  })
  const responses = await Promise.all([send(), send()])
  assert.deepEqual(responses.map((response) => response.status), [202, 202])
  const messages = await Promise.all(responses.map((response) => response.json() as Promise<{ id: string }>))
  assert.equal(messages[0]?.id, messages[1]?.id)

  const { rows } = await pool.query<{ count: string }>(
    `SELECT COUNT(*)::text AS count FROM messages WHERE conversation_id = $1`,
    [CONVERSATION_ID],
  )
  assert.equal(rows[0]?.count, '1')
})
