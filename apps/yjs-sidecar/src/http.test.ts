/**
 * Sidecar 内表面边界测试(#50):真 pg + 真 Redis,直连 http 层驱动
 * 协议契约(subscribe → update → 状态收敛 → read-text → agent-edit)。
 * 运行环境与 server 单测一致(DATABASE_URL/REDIS_URL + 测试库)。
 */
// dotenv:#142 后 sidecar 不再经 server/src/env 间接吃根 .env,裸跑
// `npm test` 时由这里显式加载(CI 由 runner 直接注 env,不受影响)。
import 'dotenv/config'
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'

// env 模块在 import 时捕获 YJS_SIDECAR_TOKEN —— 必须在首个静态导入
// infra/env 前设好(ESM import 提升),故 infra 模块走动态导入。
process.env.YJS_SIDECAR_TOKEN = process.env.YJS_SIDECAR_TOKEN ?? 'test-sidecar-token'

const { pool } = await import('./infra/pool.js')
const { env } = await import('./infra/env.js')
const { redis, sub } = await import('./infra/redis.js')
const { startSidecarHttp } = await import('./http.js')

const COMPANY = 'c-sidecar-test'
const DOC_ID = 'doc-sidecar-test'
const BASE = 'http://127.0.0.1:5199'

async function call(path: string, body: unknown): Promise<{ status: number; json: any }> {
  const res = await fetch(`${BASE}${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${env.YJS_SIDECAR_TOKEN}` },
    body: JSON.stringify(body),
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

let closeHttp: () => void

before(async () => {
  closeHttp = await startSidecarHttp(5199)
  await pool.query(`DELETE FROM documents WHERE id = $1`, [DOC_ID])
  await pool.query(
    `INSERT INTO documents (id, company_id, title, created_by) VALUES ($1, $2, 'sidecar test', 'u-test')`,
    [DOC_ID, COMPANY],
  )
})

after(async () => {
  closeHttp()
  await pool.query(`DELETE FROM documents WHERE id = $1`, [DOC_ID])
  await pool.end()
  sub.disconnect()
  redis.disconnect()
})

test('unauthorized without bearer token', async () => {
  const res = await fetch(`${BASE}/internal/healthz`)
  assert.equal(res.status, 401)
})

test('healthz ok with token', async () => {
  const res = await fetch(`${BASE}/internal/healthz`, { headers: { authorization: `Bearer ${env.YJS_SIDECAR_TOKEN}` } })
  assert.equal(res.status, 200)
})

test('subscribe → update → resubscribe sees converged state; read-text & agent-edit work', async () => {
  // 1. 首订阅:空文档初始状态
  const s1 = await call('/internal/doc/subscribe', { documentId: DOC_ID, companyId: COMPANY, subscriberId: 'instance:test-a' })
  assert.equal(s1.status, 200)
  assert.ok(typeof s1.json.stateB64 === 'string')

  // 2. 客户端式编辑:本地 Y.Doc 写入 → 增量 update 送内表面
  const local = new Y.Doc()
  Y.applyUpdate(local, new Uint8Array(Buffer.from(s1.json.stateB64, 'base64')))
  const p = new Y.XmlElement('paragraph')
p.insert(0, [new Y.XmlText('hello sidecar')])
local.getXmlFragment('default').insert(0, [p])
  const update = Y.encodeStateAsUpdate(local)
  const u = await call('/internal/doc/update', {
    documentId: DOC_ID, companyId: COMPANY, originId: 'client-x', userId: 'u-test',
    updateB64: Buffer.from(update).toString('base64'),
  })
  assert.equal(u.status, 200)

  // 3. 新订阅者(冷路径)拿到含上述编辑的全量状态
  const s2 = await call('/internal/doc/subscribe', { documentId: DOC_ID, companyId: COMPANY, subscriberId: 'instance:test-b' })
  const remote = new Y.Doc()
  Y.applyUpdate(remote, new Uint8Array(Buffer.from(s2.json.stateB64, 'base64')))
  const remoteXml = remote.getXmlFragment('default').toJSON()
  assert.ok(JSON.stringify(remoteXml).includes('hello sidecar'), `remote=${JSON.stringify(remoteXml)}`)

  // 4. read-text(经 markdown 序列化路径)
  const t = await call('/internal/doc/read-text', { documentId: DOC_ID, companyId: COMPANY })
  assert.equal(t.status, 200)
  assert.ok(t.json.text.includes('hello sidecar'), `text=${t.json.text}`)

  // 5. agent-edit append(纯 prose 路径)后文本可见
  const e = await call('/internal/doc/agent-edit', {
    documentId: DOC_ID, companyId: COMPANY, agentId: 'a-test',
    ops: [{ kind: 'append', text: 'AGENT LINE' }],
  })
  assert.equal(e.status, 200)
  assert.equal(typeof e.json.replaced, 'number')
  const t2 = await call('/internal/doc/read-text', { documentId: DOC_ID, companyId: COMPANY })
  assert.ok(t2.json.text.includes('AGENT LINE'))

  // 6. awareness 端点(仅扇出不落库)
  const aw = await call('/internal/doc/awareness', {
    documentId: DOC_ID, companyId: COMPANY, originId: 'client-x',
    updateB64: Buffer.from(new Uint8Array([1, 0, 1])).toString('base64'),
  })
  assert.equal(aw.status, 200)

  // 7. 注销幂等
  const un = await call('/internal/doc/unsubscribe', { documentId: DOC_ID, subscriberId: 'instance:test-a' })
  assert.equal(un.status, 200)
  const un2 = await call('/internal/doc/unsubscribe', { documentId: DOC_ID, subscriberId: 'instance:test-a' })
  assert.equal(un2.status, 200)
})

test('update on unknown document → lazily accepted (no existence check), sidecar survives', async () => {
  const u = await call('/internal/doc/update', {
    documentId: 'doc-nope', companyId: COMPANY, originId: 'x', userId: 'u',
    updateB64: Buffer.from(Y.encodeStateAsUpdate(new Y.Doc())).toString('base64'),
  })
  assert.equal(u.status, 200)
  const h = await fetch(`${BASE}/internal/healthz`, { headers: { authorization: `Bearer ${env.YJS_SIDECAR_TOKEN}` } })
  assert.equal(h.status, 200, 'sidecar must survive bad requests')
})
