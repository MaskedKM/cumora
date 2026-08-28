/**
 * Mirror test: sendMessage / postSystemMessage 写段事务(#138)。
 *
 * 故障注入手段:messages 表无 (conversation_id, sequence) 唯一约束、
 * PRIMARY KEY id 是随机 token——都打不出"第二步失败"。故由测试自建
 * BEFORE INSERT 触发器按 body/kind 精确抛异常,命中"counter 已预占、
 * INSERT 失败"的第二步:
 *   - sendMessage:500 后 conversation_counters 必须整体回滚(旧行为:
 *     counter 孤儿行 + 序号断档),恢复后序号连续;
 *   - postSystemMessage(addMember 的 joined):成员变更 200 照旧(礼节
 *     写不阻断主路径),但 counter 不被半写推进(旧行为:`_,_=` 吞错 +
 *     断号),恢复后系统消息序号连续。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import {
  ensureSchemaOnce, resetAllTables, teardownAll, startMirror, seedUserMembership,
} from './_helpers.js'
import { pool } from '../db/pool.js'

const uid = 'u-tx'
const company = 'c-tx'
let call: ReturnType<typeof startMirror>['call']
let convId = ''

before(async () => {
  ;({ call } = startMirror(uid, company))
  await ensureSchemaOnce()
})

beforeEach(async () => {
  // 评审 P2:上一下游用例断言红时走不到测试体内的 dropBooms,残留触发器
  // 会让下一个 install 撞 42710 级联失败——TRUNCATE 不清触发器,这里幂等兜底。
  await dropBooms()
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Tx Co', $2, $3)`,
    [company, company, uid],
  )
  await seedUserMembership(uid, company)
  convId = `cv-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, 'group', 'tx probe', $2::jsonb, $3)`,
    [convId, JSON.stringify([uid]), company],
  )
})

after(async () => {
  await dropBooms()
  await teardownAll()
})

async function nextSeq(): Promise<number | null> {
  const { rows } = await pool.query<{ next_sequence: number }>(
    `SELECT next_sequence FROM conversation_counters WHERE conversation_id = $1`, [convId])
  return rows[0]?.next_sequence ?? null
}

async function installTextBoom(): Promise<void> {
  await pool.query(`CREATE OR REPLACE FUNCTION tx_probe_boom() RETURNS trigger AS $fn$
    BEGIN RAISE EXCEPTION 'tx probe: injected text insert failure'; END $fn$ LANGUAGE plpgsql`)
  await pool.query(`CREATE TRIGGER tx_probe_boom BEFORE INSERT ON messages
    FOR EACH ROW WHEN (NEW.kind = 'text' AND NEW.body LIKE 'boom-%')
    EXECUTE FUNCTION tx_probe_boom()`)
}

async function installSysBoom(): Promise<void> {
  await pool.query(`CREATE OR REPLACE FUNCTION tx_probe_boom() RETURNS trigger AS $fn$
    BEGIN RAISE EXCEPTION 'tx probe: injected system insert failure'; END $fn$ LANGUAGE plpgsql`)
  // convId 是本测试生成的 `cv-<uuid前8>`,内联无注入面;WHEN 子句不吃绑定参数
  await pool.query(`CREATE TRIGGER tx_probe_boom BEFORE INSERT ON messages
    FOR EACH ROW WHEN (NEW.kind = 'system' AND NEW.conversation_id = '${convId}')
    EXECUTE FUNCTION tx_probe_boom()`)
}

async function dropBooms(): Promise<void> {
  await pool.query(`DROP TRIGGER IF EXISTS tx_probe_boom ON messages`)
  await pool.query(`DROP FUNCTION IF EXISTS tx_probe_boom()`)
}

async function seedHumanParticipant(): Promise<string> {
  const id = `u-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'human', $3, 'member', 'X', '#abcdef', 'avail')`,
    [id, company, id],
  )
  return id
}

test('sendMessage 第二步失败 → 事务整体回滚,counter 无孤儿行', async () => {
  await installTextBoom()
  const boom = await call(`/conversations/${convId}/messages`, {
    method: 'POST', body: JSON.stringify({ body: 'boom-1' }),
  })
  assert.equal(boom.status, 500)            // insert failed
  assert.equal(boom.json.error, 'insert failed')
  // counter 行整体回滚(旧行为:counter 孤儿行残留 next=2,序号从此断档)
  assert.equal(await nextSeq(), null)
  await dropBooms()

  // 恢复后从 1 连续编号,boom 请求没留下任何痕迹
  const ok = await call(`/conversations/${convId}/messages`, {
    method: 'POST', body: JSON.stringify({ body: 'after recovery' }),
  })
  assert.equal(ok.status, 202)
  assert.equal(ok.json.sequence, 1)
  const { rows } = await pool.query(
    `SELECT body FROM messages WHERE conversation_id = $1 ORDER BY sequence`, [convId])
  assert.deepEqual(rows.map((r: any) => r.body), ['after recovery'])
})

test('sendMessage 正常路径:序号连续,counter 与消息一致推进', async () => {
  for (let i = 1; i <= 3; i++) {
    const res = await call(`/conversations/${convId}/messages`, {
      method: 'POST', body: JSON.stringify({ body: `m${i}` }),
    })
    assert.equal(res.status, 202)
    assert.equal(res.json.sequence, i)
  }
  assert.equal(await nextSeq(), 4)
})

test('postSystemMessage 失败(addMember joined):成员变更不受阻,counter 不半写', async () => {
  // 先发一条正常消息把 counter 立在 next=2,断言点更锋利
  const seed = await call(`/conversations/${convId}/messages`, {
    method: 'POST', body: JSON.stringify({ body: 'seed' }),
  })
  assert.equal(seed.status, 202)
  const other = await seedHumanParticipant()

  await installSysBoom()
  const res = await call(`/conversations/${convId}/members`, {
    method: 'POST', body: JSON.stringify({ id: other }),
  })
  // 礼节性系统消息失败不阻断成员变更主路径
  assert.equal(res.status, 200)
  assert.ok(res.json.members.includes(other))
  // 但 counter 停在 2:事务回滚(旧行为:半写推进到 3 且错误被吞)
  assert.equal(await nextSeq(), 2)
  const sys = await pool.query<{ n: number }>(
    `SELECT COUNT(*)::int AS n FROM messages WHERE conversation_id = $1 AND kind = 'system'`, [convId])
  assert.equal(sys.rows[0].n, 0)

  // 恢复后 joined 系统消息以序号 2 落地——连续无断号
  await dropBooms()
  const other2 = await seedHumanParticipant()
  const res2 = await call(`/conversations/${convId}/members`, {
    method: 'POST', body: JSON.stringify({ id: other2 }),
  })
  assert.equal(res2.status, 200)
  const seqRow = await pool.query<{ sequence: number; body: string }>(
    `SELECT sequence, body FROM messages WHERE conversation_id = $1 AND kind = 'system' ORDER BY sequence`, [convId])
  assert.equal(seqRow.rows.length, 1)
  assert.equal(seqRow.rows[0].sequence, 2)
  assert.ok((seqRow.rows[0] as any).body.includes('"joined"'))
})
