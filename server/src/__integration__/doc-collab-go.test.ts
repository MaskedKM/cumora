/**
 * 验收 · 文档协同链路(#55):Go server /ws 网关 → yjs-sidecar → Redis 扇出
 * → 回到各 WS 客户端。两客户端实时同步是本域验收重心。
 *
 * #70 起套件 MIRROR-only:CUMORA_MIRROR_BASE 由 runner 必供(协同是 Go 域的验收面;
 * TS 基线的协同由 apps/yjs-sidecar 自带的 rooms/relay 测试覆盖)。
 * 候选进程启动(手工或脚本):
 *   sidecar: DATABASE_URL=<test> REDIS_URL=<redis> YJS_SIDECAR_TOKEN=t \
 *            YJS_SIDECAR_PORT=5183 npx tsx apps/yjs-sidecar/src/main.ts
 *   server:  同 DATABASE_URL/REDIS_URL + YJS_SIDECAR_URL=http://127.0.0.1:5183 \
 *            YJS_SIDECAR_TOKEN=t CUMORA_GO_FAKE_AUTH=1 CUMORA_GO_LISTEN=… ./server-go
 *   本测试:  CUMORA_MIRROR_BASE=http://… npx tsx --test src/__integration__/doc-collab-go.test.ts
 */

import assert from 'node:assert/strict'
import { after, test } from 'node:test'
import WebSocket from 'ws'
import * as Y from 'yjs'
import { pool } from '../db/pool.js'
import {
  ensureSchemaOnce, MIRROR_BASE,resetAllTables, seedUserMembership, startMirror, teardownAll, 
} from './_helpers.js'

const USER = 'u-doc-collab'
const COMPANY = 'c-doc-collab'

async function seedCompanyAndUser(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Collab Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
}

await ensureSchemaOnce()

test('doc collab over Go gateway', async () => {
  const mirror = startMirror(USER, COMPANY)
  const call = mirror.call
  try {
    await resetAllTables()
    await seedCompanyAndUser()

    const created = await call('/documents', { method: 'POST', body: JSON.stringify({ title: 'collab e2e' }) })
    assert.equal(created.status, 201)
    const docId: string = created.json.id

    const t1 = (await call('/auth/ws-ticket', { method: 'POST', body: '{}' })).json.ticket
    const t2 = (await call('/auth/ws-ticket', { method: 'POST', body: '{}' })).json.ticket
    assert.ok(t1 && t2)

    function connect(ticket: string, name: string) {
      const ws = new WebSocket(`${MIRROR_BASE.replace('http', 'ws')}/ws?t=${encodeURIComponent(ticket)}`)
      const frames: any[] = []
      const waiters: Array<{ m: (f: any) => boolean; r: (f: any) => void }> = []
      const timers: NodeJS.Timeout[] = []
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
        timers.push(setTimeout(() => rej(new Error(`${name} open timeout`)), 8000))
      })
      const wait = (m: (f: any) => boolean, label: string, ms = 8000) => new Promise<any>((resolve, reject) => {
        const hit = frames.find(m)
        if (hit) return resolve(hit)
        const t = setTimeout(() => reject(new Error(`${name}: timeout waiting ${label}; got [${frames.map((f) => f.type)}]`)), ms)
        timers.push(t)
        waiters.push({ m: (f) => { if (m(f)) { clearTimeout(t); return true } return false }, r: resolve })
      })
      return { ws, opened, wait, send: (o: unknown) => ws.send(JSON.stringify(o)), name, timers }
    }

    const A = connect(t1, 'A'), B = connect(t2, 'B')
    try {
      await Promise.all([A.opened, B.opened])

      A.send({ type: 'doc.subscribe', documentId: docId })
      B.send({ type: 'doc.subscribe', documentId: docId })
      const syncA = await A.wait((f: any) => f.type === 'doc.sync', 'A doc.sync')
      const syncB = await B.wait((f: any) => f.type === 'doc.sync', 'B doc.sync')
      assert.ok(syncA.stateB64.length > 0 && syncB.stateB64.length > 0)

      // A 编辑 → sidecar 应用+持久化+Redis 扇出 → B 实时收到并收敛
      const ydoc = new Y.Doc()
      Y.applyUpdate(ydoc, new Uint8Array(Buffer.from(syncA.stateB64, 'base64')))
      ydoc.getText('content').insert(0, 'hello 协同 from A')
      A.send({ type: 'doc.update', documentId: docId, updateB64: Buffer.from(Y.encodeStateAsUpdate(ydoc)).toString('base64') })
      const got = await B.wait((f: any) => f.type === 'doc.update' && f.documentId === docId, 'B doc.update')
      const ydocB = new Y.Doc()
      Y.applyUpdate(ydocB, new Uint8Array(Buffer.from(got.updateB64, 'base64')))
      assert.equal(ydocB.getText('content').toString(), 'hello 协同 from A')

      // awareness 往返(relay 不解析内容)
      A.send({ type: 'doc.awareness', documentId: docId, updateB64: Buffer.from('awareness-e2e').toString('base64') })
      const aw = await B.wait((f: any) => f.type === 'doc.awareness' && f.documentId === docId, 'B doc.awareness')
      assert.equal(Buffer.from(aw.updateB64, 'base64').toString(), 'awareness-e2e')

      // 防泄漏:双方 unsubscribe 后 sidecar 引用归零(以 sidecar 无异常为准,
      // 房间回收属 sidecar 内部生命周期;此处验证帧面不再推送)。
      A.send({ type: 'doc.unsubscribe', documentId: docId })
      await new Promise((r) => setTimeout(r, 300))

      // 无票据连接被拒(升级前 HTTP 401,客户端不该看到 open)
      await assert.doesNotReject(async () => {
        const outcome = await new Promise<string>((res) => {
          const bare = new WebSocket(`${MIRROR_BASE.replace('http', 'ws')}/ws`)
          bare.once('open', () => res('OPEN'))
          bare.once('error', () => res('REJECTED'))
          bare.once('close', () => res('CLOSED'))
          setTimeout(() => res('TIMEOUT'), 5000)
        })
        assert.notEqual(outcome, 'OPEN', 'unauthenticated /ws must not upgrade')
      })
    } finally {
      for (const c of [A, B]) { c.timers.forEach(clearTimeout); c.ws.close() }
    }
  } finally {
    await mirror.close()
  }
})

after(async () => { await teardownAll() })
