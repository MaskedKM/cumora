/**
 * 验收镜像 · computers 半边(#60):配对/心跳/发现/治理 + starter 种子。
 * 双跑:CUMORA_MIRROR_BASE 指向 Go 候选。wake-stream/runtime API 面随
 * 同票后续 commit 补套件。
 */

import assert from 'node:assert/strict'
import { after, beforeEach, test } from 'node:test'
import { pool } from './harness/db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror,teardownAll, 
} from './_helpers.js'

const USER = 'u-mirror-comp'
const COMPANY = 'c-mirror-comp'

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUser()
})

async function seedCompanyAndUser(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Comp Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
}

after(async () => { await mirror.close(); await teardownAll() })

test('[mirror] issue pairing code (role-gated, persistent)', async () => {
  const r = await call('/computers', { method: 'POST', body: '{}' })
  assert.equal(r.status, 201)
  assert.ok(r.json.code.length > 20)
  assert.equal(r.json.expiresInSeconds, null)
  const again = await call('/computers', { method: 'POST', body: '{}' })
  assert.equal(again.json.code, r.json.code) // 幂等:同公司同令牌
})

test('[mirror] pair: redeem + starter team seeding + discovery', async () => {
  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const paired = await call('/computers/pair', {
    method: 'POST',
    body: JSON.stringify({ code, hostName: 'lab-machine', engines: ['codex', 'claude'], version: '1.2.3' }),
  })
  assert.equal(paired.status, 200)
  assert.ok(paired.json.computerId.startsWith('comp-'))
  assert.equal(paired.json.companyId, COMPANY)
  assert.ok(paired.json.deviceToken.length > 20)

  // starter team:4 agents 落到该机,默认引擎 = engines[0]
  const agents = await pool.query(
    `SELECT id, name, computer_id, engine FROM participants WHERE company_id = $1 AND kind = 'agent'`,
    [COMPANY],
  )
  assert.equal(agents.rows.length, 4)
  for (const a of agents.rows) {
    assert.equal(a.computer_id, paired.json.computerId)
    assert.equal(a.engine, 'codex')
  }
  assert.ok(agents.rows.some((a: any) => a.id.startsWith('atlas')))
  // Everyone 群 + owner DM
  const everyone = await pool.query(
    `SELECT members FROM conversations WHERE company_id = $1 AND tag = 'team' AND title = 'Everyone'`,
    [COMPANY],
  )
  assert.equal(everyone.rows.length, 1)
  assert.equal(everyone.rows[0].members.length, 5)
  const dms = await pool.query(
    `SELECT count(*)::int AS n FROM conversations WHERE company_id = $1 AND kind = 'direct'`,
    [COMPANY],
  )
  assert.equal(dms.rows[0].n, 4)
  // one-shot 时间戳
  const stamps = await pool.query(`SELECT starter_seeded_at, starter_dms_seeded_at, all_hands_seeded_at FROM companies WHERE id = $1`, [COMPANY])
  assert.ok(stamps.rows[0].starter_seeded_at)
  assert.ok(stamps.rows[0].all_hands_seeded_at)

  // 发现列表(设备令牌)
  const baseUrl = mirror.baseUrl()
  const discovered = await fetch(`${baseUrl}/api/computers/me/agents`, {
    headers: { authorization: `Bearer ${paired.json.deviceToken}` },
  })
  assert.equal(discovered.status, 200)
  const list = (await discovered.json()) as any[]
  assert.equal(list.length, 4)
  // 坏令牌 401
  const bad = await fetch(`${baseUrl}/api/computers/me/agents`, {
    headers: { authorization: 'Bearer nope' },
  })
  assert.equal(bad.status, 401)
})

test('[mirror] heartbeat + runtime-token + revoke lifecycle', async () => {
  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const paired = await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code, hostName: 'host-a', engines: ['claude'] }),
  })
  const device = paired.json.deviceToken
  const baseUrl = mirror.baseUrl()

  const hb = await fetch(`${baseUrl}/api/computers/heartbeat`, {
    method: 'POST',
    headers: { authorization: `Bearer ${device}`, 'content-type': 'application/json' },
    body: JSON.stringify({ version: '2.0.0', supervised: true }),
  })
  assert.equal(hb.status, 200)
  const row = await pool.query(`SELECT daemon_version, daemon_supervised, status FROM computers WHERE id = $1`, [paired.json.computerId])
  assert.equal(row.rows[0].daemon_version, '2.0.0')
  assert.equal(row.rows[0].daemon_supervised, true)

  const atlas = (await pool.query(`SELECT id FROM participants WHERE company_id = $1 AND id LIKE 'atlas%'`, [COMPANY])).rows[0].id
  const minted = await fetch(`${baseUrl}/api/agents/${atlas}/runtime-token`, {
    method: 'POST', headers: { authorization: `Bearer ${device}` },
  })
  assert.equal(minted.status, 200)
  const tok = (await minted.json()) as { token: string; expiresInSeconds: number }
  assert.ok(tok.token.split('.').length === 3)
  assert.equal(tok.expiresInSeconds, 7200)
  // 未分配 agent(另一台机的)→ 403 由场景覆盖:同机 agent 才准

  // 吊销 → 设备令牌失效
  assert.equal((await call(`/computers/${paired.json.computerId}`, { method: 'DELETE' })).status, 200)
  const revoked = await fetch(`${baseUrl}/api/computers/me/agents`, {
    headers: { authorization: `Bearer ${device}` },
  })
  assert.equal(revoked.status, 401)
  assert.equal((await call(`/computers/${paired.json.computerId}`, { method: 'DELETE' })).status, 404)
})

test('[mirror] re-pair by hostname attaches the same computer', async () => {
  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const first = await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code, hostName: 'same-host', engines: ['claude'] }),
  })
  const second = await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code, hostName: 'same-host', engines: ['claude'] }),
  })
  assert.equal(second.status, 200)
  assert.equal(second.json.computerId, first.json.computerId)
  const count = await pool.query(`SELECT count(*)::int AS n FROM computers WHERE company_id = $1`, [COMPANY])
  assert.equal(count.rows[0].n, 1)
})

test('[mirror] invalid pairing token 400; assign agent to computer engine pick', async () => {
  assert.equal((await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code: 'garbage' }),
  })).status, 400)
  assert.equal((await call('/computers/pair', { method: 'POST', body: '{}' })).status, 400)

  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const paired = await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code, hostName: 'h', engines: ['claude', 'grok'] }),
  })
  const nova = (await pool.query(`SELECT id FROM participants WHERE company_id = $1 AND id LIKE 'nova%'`, [COMPANY])).rows[0].id
  // 请求 grok(advertised)→ grok;请求 cursor(未 advertised)→ 回退第一个
  const a1 = await call(`/agents/${nova}/computer`, {
    method: 'POST', body: JSON.stringify({ computerId: paired.json.computerId, engine: 'grok' }),
  })
  assert.equal(a1.status, 200)
  assert.equal(a1.json.engine, 'grok')
  const a2 = await call(`/agents/${nova}/computer`, {
    method: 'POST', body: JSON.stringify({ computerId: paired.json.computerId, engine: 'cursor' }),
  })
  assert.equal(a2.status, 200)
  assert.equal(a2.json.engine, 'claude')
  assert.equal((await call(`/agents/${nova}/computer`, {
    method: 'POST', body: JSON.stringify({ computerId: 'comp-none' }),
  })).status, 400)
  assert.equal((await call(`/agents/${nova}/computer`, {
    method: 'POST', body: JSON.stringify({}),
  })).status, 400)

  // repair:重连令牌绑定该机
  const repair = await call(`/computers/${paired.json.computerId}/repair`, { method: 'POST', body: '{}' })
  assert.equal(repair.status, 200)
  assert.ok(repair.json.code.length > 20)
  const rePaired = await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code: repair.json.code, hostName: 'other-name' }),
  })
  assert.equal(rePaired.status, 200)
  // call() 已把 body 解析进 .json(原断言误取 wrapper 上的 computerId,
  // 恒 undefined 后靠 || 兜底蒙混)——直接取 body。
  const rePairedJson = (rePaired.json ?? {}) as { computerId?: string }
  assert.equal(rePairedJson.computerId, paired.json.computerId)
})

test('[mirror] GET /computers lists with daemon fields', async () => {
  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const paired = await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code, hostName: 'vh', engines: ['claude'], version: '1.0.0' }),
  })
  const list = await call('/computers')
  assert.equal(list.status, 200)
  assert.ok(Array.isArray(list.json))
  const mine = list.json.find((c: any) => c.id === paired.json.computerId)
  assert.ok(mine)
  assert.equal(mine.kind, 'local')
  assert.equal(mine.status, 'online')
  assert.equal(mine.daemon_version, '1.0.0')
  assert.ok('latest_daemon_version' in mine && 'daemon_outdated' in mine)
})

// #337 挂载文件感知上报面:device token 调 /api/computers/me/workspace-report,
// server 对账已知态(Redis cumora:wsidx:<wsId>)→ 变化项快照(.cumora/versions/)
// → 广播 workspace.files_changed(cumora:workspace 通道帧 = wsx 桥的输入,
// 桥的成员过滤已由 ws-push-go 覆盖,此处断 Redis 帧即断广播面)。
import { mkdtemp, writeFile, readdir, readFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { redis } from './harness/redis.js'

test('[mirror] workspace-report: dedup, snapshot, files_changed frame', async () => {
  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const paired = (await call('/computers/pair', {
    method: 'POST', body: JSON.stringify({ code, hostName: 'watch-box', engines: ['claude'], version: '9.9' }),
  })).json

  const folder = await mkdtemp(join(tmpdir(), 'ws-report-'))
  const wsId = `ws-rep-${Math.random().toString(36).slice(2, 10)}`
  await pool.query(
    `INSERT INTO workspaces (id, company_id, name, folder_path) VALUES ($1, $2, 'Reported', $3)`,
    [wsId, COMPANY, folder],
  )
  await writeFile(join(folder, 'note.md'), 'v1\n')

  // 订阅广播通道(先订后动,防丢帧)。
  const sub = redis.duplicate()
  const frames: any[] = []
  sub.on('messageBuffer', (_ch: Buffer, msg: Buffer) => {
    try { frames.push(JSON.parse(msg.toString())) } catch { /* 非 JSON 帧(他测余波)忽略 */ }
  })
  await sub.subscribe('cumora:workspace')

  const post = (items: unknown) =>
    fetch(`${mirrorBase()}/api/computers/me/workspace-report`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: `Bearer ${paired.deviceToken}` },
      body: JSON.stringify({ items }),
    })

  // 首报:v1 → 变化 1 条 + 快照 v1 + 帧。
  let res = await post([{ workspaceId: wsId, path: 'note.md' }])
  assert.equal(res.status, 200)
  assert.equal((await res.json() as { changed: number }).changed, 1)
  const vdir = join(folder, '.cumora', 'versions', 'note.md')
  const versions = await readdir(vdir)
  assert.equal(versions.length, 1, 'first report snapshots current content')
  const snap = await readFile(join(vdir, versions[0]), 'utf8')
  assert.ok(snap.includes('v1'), 'snapshot holds the observed content')

  // 改动后重报:v2 → 又一条;未改重报 → changed=0(已知态去重)。
  await writeFile(join(folder, 'note.md'), 'v2\n')
  res = await post([{ workspaceId: wsId, path: 'note.md' }])
  assert.equal((await res.json() as { changed: number }).changed, 1)
  res = await post([{ workspaceId: wsId, path: 'note.md' }])
  assert.equal((await res.json() as { changed: number }).changed, 0, 'unchanged re-report deduped')
  assert.ok((await readdir(vdir)).length >= 2, 'each observed change leaves a version')

  // 广播帧:workspace.files_changed,清单含 note.md。
  await new Promise((r) => setTimeout(r, 300))
  const evt = frames.find((f) => f.type === 'workspace.files_changed' && f.workspaceId === wsId)
  assert.ok(evt, `files_changed frame expected, got ${JSON.stringify(frames.map((f) => f.type))}`)
  assert.equal(evt.companyId, COMPANY)
  assert.ok(evt.changes.some((c: any) => c.path === 'note.md'))

  // 跨租户区:静默跳过(不确认存在性)。
  const otherCo = `c-other-${Math.random().toString(36).slice(2, 8)}`
  await pool.query(`INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Other', $2, 'u-x')`, [otherCo, otherCo])
  const otherWs = `ws-oth-${Math.random().toString(36).slice(2, 8)}`
  await pool.query(`INSERT INTO workspaces (id, company_id, name, folder_path) VALUES ($1, $2, 'Other', '/tmp/x')`, [otherWs, otherCo])
  res = await post([{ workspaceId: otherWs, path: 'secret.md' }])
  assert.equal(res.status, 200)
  assert.equal((await res.json() as { changed: number }).changed, 0, 'cross-tenant report silently ignored')

  // 401:无 device token。
  const noAuth = await fetch(`${mirrorBase()}/api/computers/me/workspace-report`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: '{"items":[]}',
  })
  assert.equal(noAuth.status, 401)

  await sub.disconnect()
})

function mirrorBase(): string {
  const base = process.env.CUMORA_MIRROR_BASE
  if (!base) throw new Error('CUMORA_MIRROR_BASE not set')
  return base
}
