/**
 * #145 合帧单测:rooms.ts 的落库合批窗口(真 pg + 真 Redis,同 http.test.ts
 * 约定)。窗口/上限经 env 拉长缩短(YJS_FLUSH_WINDOW_MS=400 /
 * YJS_FLUSH_MAX_PENDING=8),使「窗口未到点不得落库」可以无竞态断言。
 *
 * 覆盖:窗口合批收敛等价、同进程扇出仍逐帧、上限触发、多 origin 分组、
 * flushAllPending 关停冲刷、200 帧后 compaction 在合批下仍正确收敛。
 */
// dotenv:#142 后 sidecar 自持 env,裸跑 `npm test` 时由此加载。
import 'dotenv/config'
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'

// env 模块 import 时捕获 —— 必须先于 infra/env 的动态导入设好(ESM 提升)。
process.env.YJS_SIDECAR_TOKEN = process.env.YJS_SIDECAR_TOKEN ?? 'test-sidecar-token'
process.env.YJS_FLUSH_WINDOW_MS = '400'
process.env.YJS_FLUSH_MAX_PENDING = '8'

const { pool } = await import('./infra/pool.js')
const { subscribe, unsubscribe, applyLocalUpdate, flushAllPending } = await import('./rooms.js')

const COMPANY = 'c-sidecar-coalesce'
const DOC_IDS = ['doc-coalesce-a', 'doc-coalesce-cap', 'doc-coalesce-multi', 'doc-coalesce-shutdown', 'doc-coalesce-compact']

interface UpdateRow { id: string; author_id: string; update_bytes: Buffer }

async function rowsFor(documentId: string): Promise<UpdateRow[]> {
  const { rows } = await pool.query<UpdateRow>(
    `SELECT id, author_id, update_bytes FROM document_updates WHERE document_id = $1 ORDER BY id ASC`,
    [documentId],
  )
  return rows
}

/** 把落库行(按 id 序)回放到全新 Y.Doc —— 冷加载路径的等价物。 */
function replay(rows: UpdateRow[], snapshotBytes?: Buffer | null): string {
  const doc = new Y.Doc()
  if (snapshotBytes) Y.applyUpdate(doc, new Uint8Array(snapshotBytes))
  for (const r of rows) Y.applyUpdate(doc, new Uint8Array(r.update_bytes))
  return doc.getText('content').toString()
}

/** 单写者本地文档:第 k 步 insert 一个字符后编码全量 state。
 *  全量 update 重复 apply 幂等,合并落库的回放收敛性与增量一致。 */
function stepInsert(local: Y.Doc, ch: string): Uint8Array {
  local.getText('content').insert(local.getText('content').length, ch)
  return Y.encodeStateAsUpdate(local)
}

before(async () => {
  for (const id of DOC_IDS) {
    await pool.query(`DELETE FROM documents WHERE id = $1`, [id])
    await pool.query(
      `INSERT INTO documents (id, company_id, title, created_by) VALUES ($1, $2, 'coalesce test', 'u-test')`,
      [id, COMPANY],
    )
  }
})

after(async () => {
  for (const id of DOC_IDS) {
    await pool.query(`DELETE FROM document_updates WHERE document_id = $1`, [id])
    await pool.query(`DELETE FROM document_snapshots WHERE document_id = $1`, [id])
    await pool.query(`DELETE FROM documents WHERE id = $1`, [id])
  }
  await pool.end()
})

test('window: same-origin burst → ONE row after window; per-frame fan-out stays immediate', async () => {
  const DOC = DOC_IDS[0]
  const fanout: Array<{ bytes: number; origin: string }> = []
  await subscribe(DOC, COMPANY, {
    originId: 'sub:observer',
    onUpdate: (u, originId) => { fanout.push({ bytes: u.length, origin: originId }) },
    onAwareness: () => { /* not exercised */ },
  })

  const local = new Y.Doc()
  for (const ch of ['a', 'b', 'c', 'd', 'e']) {
    await applyLocalUpdate(DOC, COMPANY, 'client:w', 'u-w', stepInsert(local, ch))
  }

  // 同进程订阅者不等待窗口 —— 5 帧已即时送达(合的只是 DB/publish 腿)。
  // fan-out 的 originId 语义:rooms 层把 object origin 一律解析为
  // INSTANCE_ORIGIN(本测试未设 INSTANCE_ID → 'instance:'),per-connection
  // originId 只存在于桥接层 —— 如实断言,不做虚构预期。
  assert.equal(fanout.length, 5, `fanout=${JSON.stringify(fanout)}`)
  assert.ok(fanout.every((f) => f.origin === 'instance:'))

  // 窗口(400ms)未到点 → 不得落库。
  assert.equal((await rowsFor(DOC)).length, 0, 'window must not have flushed yet')

  await new Promise((r) => setTimeout(r, 700))
  const rows = await rowsFor(DOC)
  assert.equal(rows.length, 1, `expected ONE coalesced row, got ${rows.length}`)
  assert.equal(rows[0].author_id, 'u-w')
  assert.equal(replay(rows), 'abcde', 'coalesced row must converge to full state')
})

test('cap: FLUSH_MAX_PENDING trips before the window', async () => {
  const DOC = DOC_IDS[1]
  const local = new Y.Doc()
  // cap=8:第 8 帧立刻 flush,第 9 帧留在窗口里。
  for (let i = 0; i < 9; i++) {
    await applyLocalUpdate(DOC, COMPANY, 'client:cap', 'u-cap', stepInsert(local, String(i)))
  }

  // 轮询等 cap 批出现 —— deadline 卡在 400ms 窗口到期前,确保观察到的
  // 1 行来自 cap 腿而非窗口腿。
  const midDeadline = Date.now() + 250
  let mid = await rowsFor(DOC)
  while (mid.length === 0 && Date.now() < midDeadline) {
    await new Promise((r) => setTimeout(r, 25))
    mid = await rowsFor(DOC)
  }
  assert.equal(mid.length, 1, `cap flush expected mid-window, got ${mid.length}`)

  await new Promise((r) => setTimeout(r, 700))
  const rows = await rowsFor(DOC)
  assert.equal(rows.length, 2, `expected cap batch + window batch, got ${rows.length}`)
  assert.equal(replay(rows), '012345678', 'both batches must converge together')
})

test('multi-origin: one row per origin with its own author_id', async () => {
  const DOC = DOC_IDS[2]
  const la = new Y.Doc()
  const lb = new Y.Doc()
  await applyLocalUpdate(DOC, COMPANY, 'client:x', 'u-x', stepInsert(la, 'x'))
  await applyLocalUpdate(DOC, COMPANY, 'client:y', 'u-y', stepInsert(lb, 'y'))
  await applyLocalUpdate(DOC, COMPANY, 'client:x', 'u-x', stepInsert(la, 'x'))
  await applyLocalUpdate(DOC, COMPANY, 'client:y', 'u-y', stepInsert(lb, 'y'))
  await applyLocalUpdate(DOC, COMPANY, 'client:x', 'u-x', stepInsert(la, 'x'))

  await new Promise((r) => setTimeout(r, 700))
  const rows = await rowsFor(DOC)
  assert.equal(rows.length, 2, `expected one row per origin, got ${rows.length}`)
  assert.deepEqual([...new Set(rows.map((r) => r.author_id))].sort(), ['u-x', 'u-y'])
  const merged = replay(rows)
  assert.equal(merged.length, 5)
  assert.equal((merged.match(/x/g) ?? []).length, 3)
  assert.equal((merged.match(/y/g) ?? []).length, 2)
})

test('flushAllPending: shutdown drains without waiting for the window', async () => {
  const DOC = DOC_IDS[3]
  const local = new Y.Doc()
  await applyLocalUpdate(DOC, COMPANY, 'client:s', 'u-s', stepInsert(local, 'z'))
  assert.equal((await rowsFor(DOC)).length, 0, 'still inside the window')

  await flushAllPending()
  const rows = await rowsFor(DOC)
  assert.equal(rows.length, 1, 'shutdown flush must drain pending synchronously')
  assert.equal(replay(rows), 'z')
})

test('compaction: 210 raw updates still trigger snapshot + correct replay', async () => {
  const DOC = DOC_IDS[4]
  const local = new Y.Doc()
  for (let i = 0; i < 210; i++) {
    await applyLocalUpdate(DOC, COMPANY, 'client:c', 'u-c', stepInsert(local, i % 10 === 9 ? '\n' : String(i % 10)))
  }
  await flushAllPending()

  // 200 帧阈值在合批下照样触发 maybeCompact —— 轮询等快照落盘。
  const deadline = Date.now() + 5_000
  let snap: { state_bytes: Buffer; snapshot_at_update_id: string } | undefined
  while (Date.now() < deadline) {
    const { rows } = await pool.query<{ state_bytes: Buffer; snapshot_at_update_id: string }>(
      `SELECT state_bytes, snapshot_at_update_id FROM document_snapshots WHERE document_id = $1`,
      [DOC],
    )
    if (rows[0]) { snap = rows[0]; break }
    await new Promise((r) => setTimeout(r, 200))
  }
  assert.ok(snap, 'snapshot row must exist after 210 updates')

  const tail = (await rowsFor(DOC)).filter((r) => BigInt(r.id) > BigInt(snap!.snapshot_at_update_id))
  const expected = local.getText('content').toString()
  assert.equal(
    replay(tail, snap.state_bytes),
    expected,
    'snapshot + tail replay must converge to the full 210-step state',
  )
})

// 防御:房间不清理会让 eviction 定时器挂着进程 —— force-exit 由 test runner 处理,
// 这里仅确认 unsubscribe 幂等不抛。
test('unsubscribe without subscription is a no-op', () => {
  unsubscribe(DOC_IDS[0], { originId: 'sub:never', onUpdate: () => {}, onAwareness: () => {} })
})
