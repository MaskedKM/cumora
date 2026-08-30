/**
 * 验收 · 聊天 WS 推送面(#202)+ hello 握手帧(#198):
 *   1. 连接鉴权后收到 hello(instanceId/ts 形状)——重连(新连接)同样发;
 *   2. REST 发消息 → msg.new 经 Redis 桥按租户成员资格转发到 WS(双连接,
 *      发送方自己的连接也收到,前端按 message id 去重——TS 同款语义);
 *   3. 租户隔离:非成员公司的连接收不到 message.new。
 * 形态对齐 doc-collab-go.test.ts:CUMORA_MIRROR_BASE 由 runner 必供,
 * Go SUT 与测试共享同一 Postgres/Redis(runner 起 SUT 时带 REDIS_URL,
 * 桥激活)。在场翻转(participants.status)是连接副产物,不断言其时序。
 */
import assert from 'node:assert/strict'
import { after, test } from 'node:test'
import WebSocket from 'ws'
import { pool } from '../db/pool.js'
import {
  ensureSchemaOnce, MIRROR_BASE, resetAllTables, seedUserMembership, startMirror, teardownAll,
} from './_helpers.js'

const CO_A = 'c-ws-push-a'
const CO_B = 'c-ws-push-b'
const USER_A = 'u-ws-push-a'
const USER_B = 'u-ws-push-b'

async function seedCompany(company: string, user: string): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $1, $2, $3)`,
    [company, company.replace(/[^a-z0-9]/g, '-'), user],
  )
  // seedUserMembership 顺带种 users/company_members/human participants 行。
  await seedUserMembership(user, company)
}

const PEER_A = 'a-ws-push-peer'

/** doc-collab 同款连接助手:帧收集 + 谓词等待。 */
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

await ensureSchemaOnce()

test('hello on connect + chat push fanout + tenant isolation', async () => {
  const mirrorA = startMirror(USER_A, CO_A)
  const mirrorB = startMirror(USER_B, CO_B)
  const callA = mirrorA.call
  try {
    await resetAllTables()
    await seedCompany(CO_A, USER_A)
    await seedCompany(CO_B, USER_B)
    await pool.query(
      `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
       VALUES ($1, $2, 'agent', $1, 'P', '#222222', 'resting')`,
      [PEER_A, CO_A],
    )

    const conv = await callA('/conversations', {
      method: 'POST',
      body: JSON.stringify({ title: 'Push Room', members: [PEER_A] }), // 调用者自动并入
    })
    assert.equal(conv.status, 201)

    const tA = (await callA('/auth/ws-ticket', { method: 'POST', body: '{}' })).json.ticket
    const tB = (await mirrorB.call('/auth/ws-ticket', { method: 'POST', body: '{}' })).json.ticket
    assert.ok(tA && tB)

    // ── 1) hello 帧(#198):鉴权后即发,形状对齐 TS ws.ts ──
    const A = connect(tA, 'A'), B = connect(tB, 'B')
    try {
      await Promise.all([A.opened, B.opened])
      const helloA = await A.wait((f: any) => f.type === 'hello', 'A hello')
      const helloB = await B.wait((f: any) => f.type === 'hello', 'B hello')
      for (const h of [helloA, helloB]) {
        assert.equal(typeof h.instanceId, 'string')
        assert.ok(h.instanceId.length > 0, 'instanceId must be non-empty')
        assert.equal(typeof h.ts, 'number')
        assert.ok(h.ts > 0)
      }

      // ── 2) REST 发消息 → 桥扇出:成员连接收到 message.new(#202)──
      const body = `ws push ping ${Date.now()}`
      const sent = await callA(`/conversations/${conv.json.id}/messages`, {
        method: 'POST',
        body: JSON.stringify({ body }),
      })
      assert.ok([200, 202].includes(sent.status), `send=${sent.status}`)
      const pushed = await A.wait(
        (f: any) => f.type === 'message.new' && f.message?.id === sent.json.id,
        'A message.new',
      )
      assert.equal(pushed.conversationId, conv.json.id)
      assert.equal(pushed.message.body, body)
      assert.equal(pushed.companyId, CO_A)

      // ── 3) 租户隔离:CO_B 成员收不到 CO_A 的事件 ──
      await new Promise((r) => setTimeout(r, 1500))
      assert.ok(
        !B.frames.some((f: any) => f.type === 'message.new'),
        `foreign-company connection must not receive message.new, got [${B.frames.map((f: any) => f.type)}]`,
      )

      // ── 4) 重连发新 hello(#198 验收面:服务端对每个新连接都发)──
      A.ws.close()
      await new Promise((r) => setTimeout(r, 300))
      const tA2 = (await callA('/auth/ws-ticket', { method: 'POST', body: '{}' })).json.ticket
      const A2 = connect(tA2, 'A2')
      try {
        await A2.opened
        const hello2 = await A2.wait((f: any) => f.type === 'hello', 'A2 hello after reconnect')
        assert.equal(typeof hello2.ts, 'number')
      } finally {
        A2.ws.close()
      }
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
