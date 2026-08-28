/**
 * Mirror test: 越权读两修(#129 cli search / #130 /runtime/context)。
 *
 * #129:cliCmdSearch 此前裸 `WHERE m.body ILIKE $1` 全库扫,跨租户正文
 * 直接回给发起 agent。修后 = HTTP search 同形(c.company_id + members
 * containment)。本文件断言:跨租户搜不到、同公司非成员会话也搜不到、
 * 成员正常命中(对照组)。
 *
 * #130:loadContextSQL 此前仅 `c.id = ANY($2)`,无成员校验无上限。修后
 * members containment 恒生效 + company 过滤(JWT claim)+ handler 50
 * 上限。断言:非成员会话回空(跨租户 + 同公司两案)、51 个 id → 400。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { ensureSchemaOnce, resetAllTables, teardownAll, MIRROR_BASE } from './_helpers.js'
import { signAgentToken } from '../agents/runtime/jwt.js'
import { pool } from '../db/pool.js'

let baseUrl = ''

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  baseUrl = MIRROR_BASE
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
})

after(async () => {
  await teardownAll()
})

async function call(
  path: string,
  opts: { method?: string; token?: string; body?: unknown } = {},
): Promise<{ status: number; body: any }> {
  const headers: Record<string, string> = { 'content-type': 'application/json' }
  if (opts.token) headers['authorization'] = `Bearer ${opts.token}`
  const res = await fetch(`${baseUrl}${path}`, {
    method: opts.method ?? 'POST',
    headers,
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
  })
  const text = await res.text()
  let parsed: any = null
  try { parsed = text ? JSON.parse(text) : null } catch { parsed = text }
  return { status: res.status, body: parsed }
}

async function seedAgent(): Promise<{ agentId: string; companyId: string; token: string }> {
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
    [companyId, `Co ${companyId}`, companyId, 'test-owner'],
  )
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
       VALUES ($1, $2, 'agent', $3, 'tester', $4, '#abcdef', 'avail')`,
    [agentId, companyId, agentId, agentId.slice(0, 1).toUpperCase()],
  )
  const token = signAgentToken({ agentId, companyId })
  return { agentId, companyId, token }
}

async function seedSecondAgent(companyId: string): Promise<{ agentId: string; token: string }> {
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
       VALUES ($1, $2, 'agent', $3, 'tester', $4, '#abcdef', 'avail')`,
    [agentId, companyId, agentId, agentId.slice(0, 1).toUpperCase()],
  )
  return { agentId, token: signAgentToken({ agentId, companyId }) }
}

async function seedConversation(companyId: string, members: string[]) {
  const convId = `cv-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, 'group', 'Test convo', $2::jsonb, $3)`,
    [convId, JSON.stringify(members), companyId],
  )
  let seq = 0
  return {
    convId,
    insertMessage: async (authorId: string, body: string) => {
      seq += 1
      const id = `m-${randomUUID().slice(0, 8)}`
      await pool.query(
        `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
         VALUES ($1, $2, $3, 'text', $4, $5, $6)`,
        [id, convId, authorId, body, seq, companyId],
      )
      return id
    },
  }
}

// ── #129: cli search 租户隔离 ────────────────────────────────────────

test('[mirror-tenant] /cli search: 跨租户 + 同公司非成员均不可见,成员命中', async () => {
  const outsider = await seedAgent() // 公司 A 的局外人
  const insider = await seedAgent() // 公司 B
  const secret = `s3cr3t-turnip-${randomUUID().slice(0, 8)}`
  const convo = await seedConversation(insider.companyId, [insider.agentId])
  await convo.insertMessage(insider.agentId, `the ${secret} is in the cellar`)

  // 对照:本公司成员能搜到。
  const hit = await call('/runtime/cli', { token: insider.token, body: { argv: ['search', secret] } })
  assert.equal(hit.status, 200)
  assert.equal(hit.body.ok, true)
  assert.match(hit.body.text, new RegExp(`1 match\\(es\\) for "${secret}"`))

  // 跨租户:别公司 agent 搜同词 → 零命中,正文不回。
  const cross = await call('/runtime/cli', { token: outsider.token, body: { argv: ['search', secret] } })
  assert.equal(cross.status, 200)
  assert.equal(cross.body.ok, true)
  assert.match(cross.body.text, /no matches/)
  assert.doesNotMatch(cross.body.text, /cellar/)

  // 同公司但非会话成员:members containment 挡下(光 company 过滤不够)。
  const colleague = await seedSecondAgent(insider.companyId)
  const nonMember = await call('/runtime/cli', { token: colleague.token, body: { argv: ['search', secret] } })
  assert.equal(nonMember.status, 200)
  assert.match(nonMember.body.text, /no matches/)
})

test('[mirror-tenant] /cli search: --in 限定别公司会话仍搜不到', async () => {
  const outsider = await seedAgent()
  const insider = await seedAgent()
  const secret = `s3cr3t-radish-${randomUUID().slice(0, 8)}`
  const convo = await seedConversation(insider.companyId, [insider.agentId])
  await convo.insertMessage(insider.agentId, `bring the ${secret}`)

  const r = await call('/runtime/cli', {
    token: outsider.token,
    body: { argv: ['search', secret, '--in', convo.convId] },
  })
  assert.equal(r.status, 200)
  assert.match(r.body.text, /no matches/)
})

// ── #130: /runtime/context 成员校验 + 上限 ───────────────────────────

test('[mirror-tenant] /runtime/context: 非成员会话回空(跨租户 + 同公司)', async () => {
  const outsider = await seedAgent()
  const insider = await seedAgent()
  const convo = await seedConversation(insider.companyId, [insider.agentId])
  await convo.insertMessage(insider.agentId, 'quarterly numbers')

  // 对照:成员拉得到。
  const own = await call('/runtime/context', { token: insider.token, body: { conversationIds: [convo.convId] } })
  assert.equal(own.status, 200)
  assert.equal((own.body?.rows ?? []).length, 1)

  // 跨租户:回空不报错(票面验收:403 或空)。
  const cross = await call('/runtime/context', { token: outsider.token, body: { conversationIds: [convo.convId] } })
  assert.equal(cross.status, 200)
  assert.deepEqual(cross.body?.rows, [])

  // 同公司非成员:同样回空。
  const colleague = await seedSecondAgent(insider.companyId)
  const nonMember = await call('/runtime/context', { token: colleague.token, body: { conversationIds: [convo.convId] } })
  assert.equal(nonMember.status, 200)
  assert.deepEqual(nonMember.body?.rows, [])
})

test('[mirror-tenant] /runtime/context: 混入合法 id 时只回成员会话', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const mine = await seedConversation(companyId, [agentId])
  await mine.insertMessage(agentId, 'my own note')
  const other = await seedAgent()
  const theirs = await seedConversation(other.companyId, [other.agentId])
  await theirs.insertMessage(other.agentId, 'their note')

  const r = await call('/runtime/context', { token, body: { conversationIds: [mine.convId, theirs.convId] } })
  assert.equal(r.status, 200)
  const rows = (r.body?.rows ?? []) as any[]
  assert.equal(rows.length, 1)
  assert.equal(rows[0].conversation_id, mine.convId)
})

test('[mirror-tenant] /runtime/context: conversationIds 超 50 → 400', async () => {
  const { token } = await seedAgent()
  const ids = Array.from({ length: 51 }, (_, i) => `cv-pad-${i}`)
  const over = await call('/runtime/context', { token, body: { conversationIds: ids } })
  assert.equal(over.status, 400)
  assert.match(over.body?.error ?? '', /max 50/)

  const atCap = await call('/runtime/context', { token, body: { conversationIds: ids.slice(0, 50) } })
  assert.equal(atCap.status, 200)
  assert.deepEqual(atCap.body?.rows, [])
})
