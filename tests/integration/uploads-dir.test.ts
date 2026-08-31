/**
 * 验收镜像 · uploads 根统一认 CUMORA_UPLOADS_DIR(#208)。
 *
 * 背景:此前 env 只被写侧认(domains/uploads、agent/cli_storage),读侧
 * (webapp 静态服务、core/oauth 头像镜像)、email 域(入站附件、GC 根)
 * 与 workspaces 默认区仍指 cwd 相对的 server/uploads —— 设 env 会精神
 * 分裂:上传成功但不可见、永不被 GC。
 *
 * 形态:runner(run.mjs)把 CUMORA_UPLOADS_DIR 钉到每-run 临时目录后再
 * 起 Go 服/sidecar,本文件据此断言四条链路全部命中该目录:
 *   1. POST /api/uploads(base64)落盘 + GET /uploads/<key> 静态可取回;
 *   2. OAuth 首登头像镜像(auth/start→callback)落 avatars/<uid>.<ext>;
 *   3. email 入站附件(webhook)落 email-attachments/ 且静态可取回;
 *   4. email GC tick 扫描该目录(伪造 >1h 孤儿被删、新鲜文件幸存;
 *      GC 间隔由 runner 压到 1s)。
 * 并断言 git 工作树内的旧默认目录(repo/server/uploads)【没有】收到
 * 任何新文件 —— env 生效的负向证据。
 *
 * OAuth 桩端口 18994 与 mirror-oauth.test.ts 同款:node --test 每文件
 * 独立进程且 runner 串行(--test-concurrency=1),不并发抢端口。
 */
import { test, beforeEach, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, request as httpReq, type IncomingMessage } from 'node:http'
import { createHmac } from 'node:crypto'
import { mkdir, readFile, stat, utimes, writeFile } from 'node:fs/promises'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { pool } from './harness/db/pool.js'
import { redis } from './harness/redis.js'
import { ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror, teardownAll } from './_helpers.js'

const here = dirname(fileURLToPath(import.meta.url))
const repo = join(here, '..', '..')

// runner 注入的每-run uploads 根;缺失说明没经 run.mjs 起 SUT,直接炸出
// 可诊断错误(所有断言都围绕这个目录展开,静默跳过等于没验)。
const UPLOADS_DIR = process.env.CUMORA_UPLOADS_DIR ?? ''
if (!UPLOADS_DIR) {
  throw new Error(
    'CUMORA_UPLOADS_DIR is not set — this suite must run via tests/integration/run.mjs, ' +
    'which pins it to a per-run temp dir before booting the Go server (#208).',
  )
}
// 旧默认(git 工作树内,相对仓库根):只用于"没写到这里"的负向断言。
const LEGACY_DIR = resolve(repo, 'server/uploads')

const USER = 'u-updir'
const COMPANY = 'c-updir'
const STUB_PORT = 18994
const STUB_BASE = `http://127.0.0.1:${STUB_PORT}`
const RETURN_OK = 'http://localhost:5180/after-login'
const INBOUND_SECRET = process.env.EMAIL_INBOUND_HMAC_SECRET || 'integration-test-secret'

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const base = mirror.baseUrl

const JPEG = Buffer.from([
  0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0x00, 0x01,
  0xff, 0xd9,
])

function rawGet(path: string): Promise<{ status: number; location?: string }> {
  return new Promise((resolveP, reject) => {
    const req = httpReq(new URL(path, base()), { method: 'GET' }, (res: IncomingMessage) => {
      res.resume() // 只要头(302 Location),体丢弃
      res.on('end', () => resolveP({ status: res.statusCode ?? 0, location: res.headers.location }))
    })
    req.on('error', reject)
    req.end()
  })
}

/** OAuth 桩:mirror-oauth.test.ts 的最小切片(access_token/user/emails/
 * pic.jpg 四路由,头像指向桩自身的 JPEG)。 */
let stubSrv: ReturnType<typeof createServer> | null = null

async function postInbound(payload: Record<string, unknown>): Promise<{ status: number; json: any }> {
  const raw = JSON.stringify(payload)
  const signature = createHmac('sha256', INBOUND_SECRET).update(raw).digest('hex')
  const res = await fetch(`${base()}/webhooks/email/inbound`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-cumora-signature': signature },
    body: raw,
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

async function exists(p: string): Promise<boolean> {
  try { await stat(p); return true } catch { return false }
}

async function pollUntil(pred: () => Promise<boolean>, timeoutMs: number): Promise<boolean> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await pred()) return true
    await new Promise((r) => setTimeout(r, 200))
  }
  return await pred()
}

before(async () => {
  stubSrv = createServer((req, res) => {
    const u = req.url ?? ''
    if (u === '/access_token') {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ access_token: 'stub-access-token' }))
      return
    }
    if (u === '/user') {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ id: 7777, login: 'updir', name: 'Up Dir', avatar_url: `${STUB_BASE}/pic.jpg` }))
      return
    }
    if (u === '/user/emails') {
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify([{ email: 'updir@example.com', primary: true, verified: true }]))
      return
    }
    if (u === '/pic.jpg') {
      res.writeHead(200, { 'content-type': 'image/jpeg' })
      res.end(JPEG)
      return
    }
    res.writeHead(404).end('nope')
  })
  await new Promise<void>((resolveP, reject) => {
    stubSrv!.once('error', reject)
    stubSrv!.listen(STUB_PORT, '127.0.0.1', () => resolveP())
  })
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Uploads Dir Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
  const keys = await redis.keys('oauth:state:*')
  if (keys.length > 0) await redis.del(...keys)
})

after(async () => {
  await mirror.close()
  await new Promise<void>((resolveP) => stubSrv?.close(() => resolveP()))
  await teardownAll()
})

/* ───────── 1. 上传写侧 + /uploads 静态读侧 ───────── */

test('[uploads-dir] base64 上传落 env 目录,静态服务可取回,旧默认目录不沾', async () => {
  const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00])
  const up = await mirror.call('/uploads', {
    method: 'POST',
    body: JSON.stringify({ name: 't.png', mime: 'image/png', dataBase64: png.toString('base64') }),
  })
  assert.equal(up.status, 200, JSON.stringify(up.json))
  const key = String(up.json.key)
  assert.match(key, /^attachments\/[0-9a-f]{32}\.png$/)
  assert.equal(up.json.url, `/uploads/${key}`)

  // 落盘命中 env 目录且字节一致。
  const onDisk = await readFile(join(UPLOADS_DIR, key))
  assert.ok(onDisk.equals(png), 'uploaded bytes round-trip in CUMORA_UPLOADS_DIR')

  // 静态服务(webapp /uploads handler)从 env 目录取回 —— 读侧同源。
  const res = await fetch(`${base()}${up.json.url}`)
  assert.equal(res.status, 200)
  assert.equal(res.headers.get('x-content-type-options'), 'nosniff')
  const body = Buffer.from(await res.arrayBuffer())
  assert.ok(body.equals(png), 'static /uploads serves the same bytes')

  // 负向:git 工作树内旧默认目录没收到该文件。
  assert.equal(await exists(join(LEGACY_DIR, key)), false, 'legacy worktree dir must not receive uploads')
})

/* ───────── 2. OAuth 首登头像镜像(oauthMirrorAvatar)───────── */

test('[uploads-dir] OAuth 头像镜像落 env 目录 avatars/<uid>.jpg', async () => {
  const start = await rawGet(`/api/auth/start/github?return=${encodeURIComponent(RETURN_OK)}`)
  assert.equal(start.status, 302)
  const state = new URL(start.location!).searchParams.get('state')!

  const cb = await rawGet(`/api/auth/callback/github?code=stub-code&state=${encodeURIComponent(state)}`)
  assert.equal(cb.status, 302)
  assert.ok(cb.location!.startsWith(`${RETURN_OK}#`), `fragment target: ${cb.location}`)

  const row = (await pool.query<{ id: string; avatar_url: string }>(
    `SELECT id, avatar_url FROM users WHERE email = 'updir@example.com'`)).rows[0]
  assert.ok(row, 'user created by oauth first login')
  assert.equal(row.avatar_url, `/uploads/avatars/${row.id}.jpg`)

  // 写侧:镜像文件在 env 目录;读侧:静态可取(且位图内联渲染)。
  const onDisk = await readFile(join(UPLOADS_DIR, 'avatars', `${row.id}.jpg`))
  assert.ok(onDisk.equals(JPEG), 'mirrored avatar bytes in CUMORA_UPLOADS_DIR')
  const res = await fetch(`${base()}${row.avatar_url}`)
  assert.equal(res.status, 200)
  assert.equal(res.headers.get('content-disposition'), null, 'previewable bitmap stays inline')
  assert.ok(Buffer.from(await res.arrayBuffer()).equals(JPEG))
  assert.equal(await exists(join(LEGACY_DIR, 'avatars', `${row.id}.jpg`)), false)
})

/* ───────── 3. email 入站附件写侧 + 静态读侧 ───────── */

test('[uploads-dir] email 入站附件落 env 目录 email-attachments/', async () => {
  await pool.query(
    `UPDATE participants SET email = $1 WHERE id = $2 AND company_id = $3`,
    [`${USER}.${COMPANY}@cumora.local`, USER, COMPANY],
  )
  const note = Buffer.from('uploads-dir attachment body')
  const r = await postInbound({
    messageId: '<updir-1@ext.example>',
    from: 'Wey <wey@example.com>',
    to: [`${USER}.${COMPANY}@cumora.local`],
    subject: 'attachment hits env dir',
    text: 'body',
    attachments: [{
      filename: 'note.txt', mimeType: 'text/plain',
      sizeBytes: note.length, contentBase64: note.toString('base64'),
    }],
  })
  assert.equal(r.status, 200, JSON.stringify(r.json))
  assert.equal(r.json.ok, true)

  const rows = (await pool.query<{ storage_key: string }>(
    `SELECT storage_key FROM email_attachments WHERE storage_key IS NOT NULL`)).rows
  assert.equal(rows.length, 1, 'one persisted attachment row')
  const key = rows[0].storage_key
  assert.match(key, /^email-attachments\//)

  const onDisk = await readFile(join(UPLOADS_DIR, key))
  assert.ok(onDisk.equals(note), 'inbound attachment bytes in CUMORA_UPLOADS_DIR')

  const res = await fetch(`${base()}/uploads/${key}`)
  assert.equal(res.status, 200)
  assert.equal(res.headers.get('content-disposition'), 'attachment', 'non-bitmap forced to download')
  assert.ok(Buffer.from(await res.arrayBuffer()).equals(note))
  assert.equal(await exists(join(LEGACY_DIR, key)), false)
})

/* ───────── 4. email GC tick 扫描 env 目录 ───────── */

test('[uploads-dir] email GC 删 env 目录内超龄孤儿,新鲜文件幸存', async () => {
  const gcDir = join(UPLOADS_DIR, 'email-attachments')
  await mkdir(gcDir, { recursive: true })
  const orphan = join(gcDir, 'deadbeefdeadbeefdeadbeefdeadbeef.orphan')
  const fresh = join(gcDir, 'cafef00dcafef00dcafef00dcafef00d.txt')
  await writeFile(orphan, 'stale orphan body')
  await writeFile(fresh, 'fresh unreferenced body')
  // 孤儿 mtime 拨回 2h 前(GC 安全窗 1h:窗口内的一律幸免)。
  const stale = new Date(Date.now() - 2 * 60 * 60 * 1000)
  await utimes(orphan, stale, stale)

  // GC 间隔由 runner 压到 1s;给 15s 余量(首 tick 对账 DB 需一轮)。
  const deleted = await pollUntil(async () => !(await exists(orphan)), 15_000)
  assert.ok(deleted, 'orphan older than the 1h safety window got GC-deleted from CUMORA_UPLOADS_DIR')
  // 同一轮 tick 已被观察到(孤儿已删),新鲜孤儿必须仍在:证明删的判据
  // 是安全窗而非"扫到就删"。
  assert.equal(await exists(fresh), true, 'fresh unreferenced file spared by the safety window')
})
