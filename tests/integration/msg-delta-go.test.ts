/**
 * 验收 · #210 msg.delta 激活 —— daemon 上报 → server-go 广播 → WS 收到:
 *   1. POST /runtime/message-delta(agent-runtime JWT)→ message.delta 经
 *      Redis 桥按租户转发到成员 WS(载荷=契约 MessageDeltaEvent,
 *      companyId 打标、authorId 取 JWT 非请求体);
 *   2. 租户隔离:非成员公司连接收不到 message.delta;
 *   3. auth 闸:无 token 401;跨租户 agent 对非本会话成员上报 403;
 *   4. 终局收口:同一 agent 随后 /runtime/cli reply → message.new(真消息
 *      落库广播)——delta 先到、final 收口,前端按 (convo, author) 幂等
 *      替换(store 单测覆盖语义,此处锁定协议顺序)。
 * 形态对齐 ws-push-go.test.ts(CUMORA_MIRROR_BASE 由 runner 必供,Go SUT
 * 与测试共享同一 Postgres/Redis,桥激活)。
 */
import assert from 'node:assert/strict'
import { after, test } from 'node:test'
import { randomUUID } from 'node:crypto'
import WebSocket from 'ws'
import { pool } from './harness/db/pool.js'
import { signAgentToken } from './harness/agents/runtime/jwt.js'
import {
  ensureSchemaOnce, MIRROR_BASE, resetAllTables, seedUserMembership, startMirror, teardownAll,
} from './_helpers.js'

const CO_A = 'c-msg-delta-a'
const CO_B = 'c-msg-delta-b'
const USER_A = 'u-msg-delta-a'
const USER_B = 'u-msg-delta-b'
const AGENT_A = 'a-msg-delta-agent'
const AGENT_B = 'a-msg-delta-b'

async function seedCompany(company: string, user: string): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $1, $2, $3)`,
    [company, company.replace(/[^a-z0-9]/g, '-'), user],
  )
  await seedUserMembership(user, company)
}

function connect(ticket: string, name: string) {
  const ws = new WebSocket(`${MIRROR_BASE.replace('http', 'ws')}/ws?t=${encodeURIComponent(ticket)}`)
  const frames: any[] = []
  const waiters: Array<{ m: (f: any) => boolean; r: (f: any) => void }> = []
  ws.on('message', (raw: WebSocket.RawData) => {
    const f = JSON.parse(raw.toString())
    frames.push(f)
    for (let i = waiters.length - 1; i >= 0; i--) {
      if (waiters[i].m(f)) { waiters[i].r(f); waiters.splice(i, 1) }
    }
  })
  const opened = new Promise<void>((res, rej) => {
    ws.once('open', () => res())
    ws.once('error', (e) => rej(e))
    ws.once('close', (c, reason) => rej(new Error(`${name} closed ${c} ${reason}`)))
    setTimeout(() => rej(new Error(`${name} open timeout`)), 8000)
  })
  const wait = (m: (f: any) => boolean, label: string, ms = 8000) => new Promise<any>((resolve, reject) => {
    const hit = frames.find(m)
    if (hit) return resolve(hit)
    const t = setTimeout(() => reject(new Error(`${name}: timeout waiting ${label}; got [${frames.map((f) => f.type)}]`)), ms)
    waiters.push({ m: (f) => { if (m(f)) { clearTimeout(t); return true } return false }, r: resolve })
  })
  return { ws, opened, wait, frames, name }
}

async function postDelta(token: string, body: unknown): Promise<{ status: number; json: any }> {
  const res = await fetch(`${MIRROR_BASE}/runtime/message-delta`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify(body),
  })
  const text = await res.text()
  return { status: res.status, json: text ? JSON.parse(text) : null }
}

await ensureSchemaOnce()

test('delta report → WS message.delta fanout + tenant isolation + terminal message.new', async () => {
  const mirrorA = startMirror(USER_A, CO_A)
  const mirrorB = startMirror(USER_B, CO_B)
  const callA = mirrorA.call
  try {
    await resetAllTables()
    await seedCompany(CO_A, USER_A)
    await seedCompany(CO_B, USER_B)
    // agent A 属 CO_A;agent B 属 CO_B(跨租户 403 用)。
    for (const [id, co] of [[AGENT_A, CO_A], [AGENT_B, CO_B]] as const) {
      await pool.query(
        `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
         VALUES ($1, $2, 'agent', $1, 'A', '#222222', 'avail')`,
        [id, co],
      )
    }

    const conv = await callA('/conversations', {
      method: 'POST',
      body: JSON.stringify({ title: 'Delta Room', members: [AGENT_A] }),
    })
    assert.equal(conv.status, 201)
    const convId = conv.json.id as string

    const tokenA = signAgentToken({ agentId: AGENT_A, companyId: CO_A })
    const tokenB = signAgentToken({ agentId: AGENT_B, companyId: CO_B })

    // ── auth 闸 ──
    assert.equal((await postDelta('', { conversationId: convId, messageId: 'ds-1', delta: 'x', sequence: 1, done: false })).status, 401)
    // 跨租户:agent B(token 租户 CO_B)对 CO_A 会话上报 → 成员门 403。
    assert.equal((await postDelta(tokenB, { conversationId: convId, messageId: 'ds-1', delta: 'x', sequence: 1, done: false })).status, 403)

    // ── 成员连接就位(双租户,隔离断言用)──
    const tA = (await callA('/auth/ws-ticket', { method: 'POST', body: '{}' })).json.ticket
    const tB = (await mirrorB.call('/auth/ws-ticket', { method: 'POST', body: '{}' })).json.ticket
    const A = connect(tA, 'A'), B = connect(tB, 'B')
    try {
      await Promise.all([A.opened, B.opened])
      await A.wait((f: any) => f.type === 'hello', 'A hello')

      // ── 1) delta 帧上报 → 桥扇出 ──
      const streamId = `ds-${randomUUID().slice(0, 8)}`
      const r1 = await postDelta(tokenA, { conversationId: convId, messageId: streamId, delta: 'Thinking about ', sequence: 1, done: false })
      assert.equal(r1.status, 200, `report=${r1.status} ${JSON.stringify(r1.json)}`)
      const d1 = await A.wait((f: any) => f.type === 'message.delta' && f.messageId === streamId && f.sequence === 1, 'A delta seq1')
      assert.equal(d1.companyId, CO_A)
      assert.equal(d1.conversationId, convId)
      assert.equal(d1.authorId, AGENT_A, 'authorId must come from the JWT, not the body')
      assert.equal(d1.delta, 'Thinking about ')
      assert.equal(d1.done, false)

      await postDelta(tokenA, { conversationId: convId, messageId: streamId, delta: 'the answer…', sequence: 2, done: false })
      await A.wait((f: any) => f.type === 'message.delta' && f.messageId === streamId && f.sequence === 2, 'A delta seq2')

      // ── 2) 租户隔离:CO_B 连接一条 message.delta 都收不到 ──
      await new Promise((r) => setTimeout(r, 1200))
      assert.ok(
        !B.frames.some((f: any) => f.type === 'message.delta'),
        `foreign-company connection must not receive message.delta, got [${B.frames.map((f: any) => f.type)}]`,
      )

      // ── 3) 终局收口:cli reply(真消息落库)→ message.new 同作者到达 ──
      //     DM(2 成员)豁免反独白/新鲜度预检;body 唯一不触发逐字门。
      const finalBody = `delta 终局收口 ${Date.now()}`
      const reply = await fetch(`${MIRROR_BASE}/runtime/cli`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: `Bearer ${tokenA}` },
        body: JSON.stringify({ argv: ['reply', convId, finalBody] }),
      })
      assert.equal(reply.status, 200)
      const replyJson: any = await reply.json()
      assert.equal(replyJson.exitCode, 0, `reply failed: ${replyJson.text}`)
      const fin = await A.wait(
        (f: any) => f.type === 'message.new' && f.message?.authorId === AGENT_A && f.message?.body === finalBody,
        'A final message.new',
      )
      assert.equal(fin.companyId, CO_A)
      assert.equal(fin.conversationId, convId)

      // ── 4) done 兜底帧(agent 未回帖的 turn 退场路径)照常可上报 ──
      const rDone = await postDelta(tokenA, { conversationId: convId, messageId: streamId, delta: '', sequence: 3, done: true })
      assert.equal(rDone.status, 200)
      await A.wait((f: any) => f.type === 'message.delta' && f.messageId === streamId && f.sequence === 3 && f.done === true, 'A delta done')
    } finally {
      A.ws.close()
      B.ws.close()
    }
  } finally {
    await mirrorA.close()
    await mirrorB.close()
  }
})

after(async () => { await teardownAll() })
