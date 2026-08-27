/**
 * Mirror test: 长尾路由补覆盖(#77)——TS in-process 与 Go(MIRROR)双跑。
 * 头像全链用例在文件内起最小 OpenAI 形桩(/v1/responses 性别分类 +
 * /v1/images/generations 1×1 PNG)——CI 无外部 mock,TS SDK 读
 * process.env.OPENAI_BASE_URL(legacy client 构造时),before() 钉到桩。
 * 覆盖:uploads(presign 本地 501 / refresh-url 键解析)、devtools 门禁
 * (x-cumora-dev-mode + 角色)、agent workspace 文件读、run 事件流、
 * 纯 agent 房偷看(owner-only + 混合房 404)、admin 头像生成(非 agent
 * 400 / 不存在 404 / mock 全链 200)、computers 面(pair/heartbeat/
 * DELETE)、assign、runtime-token 铸造、DELETE /me/account 效应、
 * /runtime/inbox 的附件 URL freshen(#94 延期项)。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { startMirror, seedUserMembership, ensureSchemaOnce, resetAllTables, teardownAll, MIRROR_BASE } from './_helpers.js'
import { signAgentToken } from '../agents/runtime/jwt.js'
import { pool } from '../db/pool.js'

const USER = `u-tail-${randomUUID().slice(0, 6)}`
const COMPANY = `co-tail-${randomUUID().slice(0, 6)}`
const memberMirror = startMirror(USER, COMPANY)

// 非属主观察者(peek 的 403 面):member 角色用户。
const MEMBER_USER = `u-tailm-${randomUUID().slice(0, 6)}`
const memberOnlyMirror = startMirror(MEMBER_USER, COMPANY)

let dualBase = ''
let dualServer: import('node:http').Server | null = null
let mockLLM: import('node:http').Server | null = null

// 1×1 透明 PNG(cli_mocks.py 同款)。
const PNG_B64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=='

before(async () => {
  await ensureSchemaOnce()
  // CI 无外部 mock:文件内桩接管 TS 侧的性别分类与图像生成。
  // (MIRROR 形态该桩闲置——头像路径由 Go 进程自己的 env 指向其桩。)
  const http = await import('node:http')
  mockLLM = http.createServer((req, res) => {
    const chunks: Buffer[] = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => {
      if (req.url?.endsWith('/responses')) {
        res.setHeader('content-type', 'application/json')
        res.end(JSON.stringify({
          id: 'resp-mock', object: 'response', status: 'completed',
          output: [{ type: 'message', content: [{ type: 'output_text', text: 'feminine' }] }],
        }))
        return
      }
      if (req.url?.endsWith('/images/generations')) {
        res.setHeader('content-type', 'application/json')
        res.end(JSON.stringify({ data: [{ b64_json: PNG_B64 }] }))
        return
      }
      res.statusCode = 404
      res.end('{}')
    })
  })
  await new Promise<void>((resolve) => mockLLM!.listen(0, '127.0.0.1', resolve))
  const mockAddr = mockLLM!.address()
  if (mockAddr && typeof mockAddr === 'object') {
    // OpenAI SDK 在 client 构造时读 OPENAI_BASE_URL——本文件进程内
    // 首次 LLM 调用发生在头像用例,时序安全。
    process.env.OPENAI_BASE_URL = `http://127.0.0.1:${mockAddr.port}/v1`
  }
  if (MIRROR_BASE) {
    dualBase = MIRROR_BASE // Go 候选本就同时挂 /api 与 /runtime
    return
  }
  // freshen 用例需要 /runtime 面 —— 自组 /api+/runtime 双挂进程
  // (mirror-boards-wake 同款;fake auth 盖章给 /api 面)。
  const expressMod = await import('express')
  const express = expressMod.default
  const { runtimeRouter } = await import('../agents/runtime/server.js')
  const app = express()
  app.use(express.json({ limit: '34mb' }))
  app.use((req, _res, next) => {
    ;(req as unknown as { authUserId: string }).authUserId = USER
    next()
  })
  const { api } = await import('../api/router.js')
  app.use('/api', api)
  app.use('/runtime', runtimeRouter)
  const { createServer } = await import('node:http')
  dualServer = createServer(app)
  await new Promise<void>((resolve) => {
    dualServer!.listen(0, () => {
      const a = dualServer!.address()
      if (a && typeof a === 'object') dualBase = `http://127.0.0.1:${a.port}`
      resolve()
    })
  })
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(`INSERT INTO companies (id, name, slug, starter_seeded_at, starter_dms_seeded_at)
    VALUES ($1, 'TailCo', 'tailco', NOW(), NOW()) ON CONFLICT DO NOTHING`, [COMPANY])
  await seedUserMembership(USER, COMPANY)
})

after(async () => {
  if (mockLLM?.listening) {
    await new Promise<void>((resolve) => mockLLM!.close(() => resolve()))
  }
  if (dualServer?.listening) {
    await new Promise<void>((resolve) => dualServer!.close(() => resolve()))
  }
  await teardownAll()
  await memberOnlyMirror.close()
  await memberMirror.close()
})

// 注:两测试形态都在 NODE_ENV≠production 下跑(devtools 门 localDev 恒
// 开),DEV 头在这两形态里是装饰性的——403 'not enabled' 分支仅在
// NODE_ENV=production 的部署可达,镜像无法覆盖(#107 评审 NIT6 留档)。
const DEV = { 'x-cumora-dev-mode': '1' }

async function seedAgentRow(id: string): Promise<void> {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status, system_prompt)
       VALUES ($1, $2, 'agent', $1, 'A', '#123456', 'avail', 'test persona')`,
    [id, COMPANY],
  )
}

/* ───────── uploads ───────── */

test('[mirror-tail] POST /uploads/presign: local mode → 501 exact text', async () => {
  const r = await memberMirror.call('/uploads/presign', {
    method: 'POST',
    body: JSON.stringify({ name: 'a.png', mime: 'image/png', size: 10 }),
  })
  assert.equal(r.status, 501)
  assert.equal(r.json.error, 'presign not available in local mode — POST /uploads instead')
})

test('[mirror-tail] POST /uploads/refresh-url: key/url 解析 + 400 兜底', async () => {
  const byKey = await memberMirror.call('/uploads/refresh-url', {
    method: 'POST',
    body: JSON.stringify({ key: 'attachments/abc.png' }),
  })
  assert.equal(byKey.status, 200)
  assert.deepEqual(byKey.json, { key: 'attachments/abc.png', url: '/uploads/attachments/abc.png' })

  const byUrl = await memberMirror.call('/uploads/refresh-url', {
    method: 'POST',
    body: JSON.stringify({ url: '/uploads/avatars/x.png' }),
  })
  assert.equal(byUrl.status, 200)
  assert.deepEqual(byUrl.json, { key: 'avatars/x.png', url: '/uploads/avatars/x.png' })

  // 带空白与 query 的 key 也要归一。
  const sloppy = await memberMirror.call('/uploads/refresh-url', {
    method: 'POST',
    body: JSON.stringify({ key: '  attachments/q.png?sig=1 ' }),
  })
  assert.deepEqual(sloppy.json, { key: 'attachments/q.png', url: '/uploads/attachments/q.png' })

  const bad = await memberMirror.call('/uploads/refresh-url', {
    method: 'POST',
    body: JSON.stringify({ url: 'https://evil.example/x.png' }),
  })
  assert.equal(bad.status, 400)
  assert.equal(bad.json.error, 'not a Cumora storage URL')
})

/* ───────── devtools 门禁 + workspace 文件 ───────── */

test('[mirror-tail] /devtools/agent-workspace/file: 400 / 404 / 200 形状', async () => {
  const missing = await memberMirror.call('/devtools/agent-workspace/file?agentId=&path=', { headers: DEV })
  assert.equal(missing.status, 400)
  assert.equal(missing.json.error, 'agentId and path required')

  const agentId = `ag-tail-${randomUUID().slice(0, 6)}`
  await seedAgentRow(agentId)
  await pool.query(
    `INSERT INTO agent_workspace (agent_id, path, body, company_id) VALUES ($1, 'memory/MEMORY.md', $2, $3)`,
    [agentId, 'line one\nline two\nline three', COMPANY],
  )

  const found = await memberMirror.call(`/devtools/agent-workspace/file?agentId=${agentId}&path=memory/MEMORY.md`, { headers: DEV })
  assert.equal(found.status, 200)
  assert.equal(found.json.path, 'memory/MEMORY.md')
  assert.equal(found.json.body, 'line one\nline two\nline three')
  assert.equal(found.json.size, 'line one\nline two\nline three'.length)
  assert.equal(found.json.lineCount, 3)
  assert.ok(typeof found.json.updatedAt === 'string')

  const nf = await memberMirror.call(`/devtools/agent-workspace/file?agentId=${agentId}&path=nope.md`, { headers: DEV })
  assert.equal(nf.status, 404)
  assert.equal(nf.json.error, 'file not found')
})

test('[mirror-tail] /agents/observability/runs/:id/events: 404 门 + 事件形状', async () => {
  const agentId = `ag-tail-${randomUUID().slice(0, 6)}`
  await seedAgentRow(agentId)
  const nf = await memberMirror.call(`/agents/observability/runs/run-nope/events`, { headers: DEV })
  assert.equal(nf.status, 404)

  const runId = `run-tail-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO agent_runs (id, agent_id, company_id, status, trigger) VALUES ($1, $2, $3, 'completed', '{}'::jsonb)`,
    [runId, agentId, COMPANY],
  )
  await pool.query(
    `INSERT INTO agent_events (id, run_id, agent_id, company_id, kind, level, title, data)
     VALUES ('ev-1', $1, $2, $3, 'tool', 'info', 'tool started', '{"tool":"send"}'::jsonb),
            ('ev-2', $1, $2, $3, 'tool', 'info', 'tool finished', '{"ms":12}'::jsonb)`,
    [runId, agentId, COMPANY],
  )
  const r = await memberMirror.call(`/agents/observability/runs/${runId}/events`, { headers: DEV })
  assert.equal(r.status, 200)
  assert.ok(Array.isArray(r.json) && r.json.length === 2)
  assert.equal(r.json[0].id, 'ev-1')
  assert.equal(r.json[0].runId, runId)
  assert.equal(r.json[0].agentId, agentId)
  assert.deepEqual(r.json[0].data, { tool: 'send' })
  assert.ok(typeof r.json[0].createdAt === 'string')
})

/* ───────── peek:owner-only + 纯 agent 房谓词 ───────── */

test('[mirror-tail] /peek/agent-chats/:id/messages: 403/404/200', async () => {
  // member 角色用户(memberOnlyMirror 的 company_members 行先补)。
  await seedUserMembership(MEMBER_USER, COMPANY)
  await pool.query(`UPDATE company_members SET role = 'member' WHERE company_id = $1 AND user_id = $2`, [COMPANY, MEMBER_USER])

  const a1 = `ag-tail-${randomUUID().slice(0, 6)}`
  const a2 = `ag-tail-${randomUUID().slice(0, 6)}`
  await seedAgentRow(a1)
  await seedAgentRow(a2)
  const agentRoom = `cv-tail-${randomUUID().slice(0, 6)}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, 'group', 'Robots', $2::jsonb, $3)`,
    [agentRoom, JSON.stringify([a1, a2]), COMPANY],
  )
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
     VALUES ('pm-1', $1, $2, 'text', 'beep', 1, $3), ('pm-2', $1, $4, 'text', 'boop', 2, $3)`,
    [agentRoom, a1, COMPANY, a2],
  )
  // 混合房:有人类成员 → 谓词不过。
  const mixedRoom = `cv-tail-${randomUUID().slice(0, 6)}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, 'group', 'Mixed', $2::jsonb, $3)`,
    [mixedRoom, JSON.stringify([a1, USER]), COMPANY],
  )

  const forbidden = await memberOnlyMirror.call(`/peek/agent-chats/${agentRoom}/messages`)
  assert.equal(forbidden.status, 403)

  const mixed = await memberMirror.call(`/peek/agent-chats/${mixedRoom}/messages`)
  assert.equal(mixed.status, 404)

  const ok = await memberMirror.call(`/peek/agent-chats/${agentRoom}/messages`)
  assert.equal(ok.status, 200)
  assert.ok(Array.isArray(ok.json) && ok.json.length === 2)
  assert.equal(ok.json[0].id, 'pm-1')
  assert.equal(ok.json[0].conversationId, agentRoom)
  assert.equal(ok.json[0].authorId, a1)
  assert.equal(ok.json[1].body, 'boop')
  assert.ok(typeof ok.json[0].createdAt === 'string')
})

/* ───────── avatar generate(错误面 + mock 全链) ───────── */

test('[mirror-tail] POST /agents/:id/avatar/generate: 404 / 400 / mock 全链 200', async () => {
  const nf = await memberMirror.call(`/agents/u-nope-${randomUUID().slice(0, 4)}/avatar/generate`, { method: 'POST' })
  assert.equal(nf.status, 404)
  assert.equal(nf.json.error, 'not found')

  // 人类参与者 id:存在但 kind 不对 → 400(seedUserMembership 会建人类行)。
  const bad = await memberMirror.call(`/agents/${USER}/avatar/generate`, { method: 'POST' })
  assert.equal(bad.status, 400)
  assert.equal(bad.json.error, 'avatar generation is only for agents')

  // mock 全链:性别分类(/v1/responses)+ 图像(/v1/images/generations)
  // 都走 OPENAI_BASE_URL 指向的桩 —— 双侧同桩,产物 URL 形状断言。
  const agentId = `ag-tail-${randomUUID().slice(0, 6)}`
  await seedAgentRow(agentId)
  const r = await memberMirror.call(`/agents/${agentId}/avatar/generate`, { method: 'POST' })
  assert.equal(r.status, 200)
  assert.match(String(r.json.url), /^\/uploads\/avatars\/avatar-/)
  // 头像已落库 + 落盘键回读一致。
  const { rows } = await pool.query<{ avatar_url: string }>(
    `SELECT avatar_url FROM participants WHERE id = $1`, [agentId])
  assert.equal(rows[0].avatar_url, r.json.url)
})

/* ───────── computers 面(pair / heartbeat / DELETE)+ assign + runtime-token ───────── */

test('[mirror-tail] computers pair→heartbeat→DELETE 与 assign/runtime-token 值断言', async () => {
  const pairToken = `pt-${randomUUID().slice(0, 8)}`
  await pool.query(`UPDATE companies SET pair_token = $1 WHERE id = $2`, [pairToken, COMPANY])

  const paired = await memberMirror.call('/computers/pair', {
    method: 'POST',
    body: JSON.stringify({ code: pairToken, engines: ['claude'], hostName: 'tail-box', version: '0.0.1' }),
  })
  assert.equal(paired.status, 200)
  const computerId: string = paired.json.computerId
  assert.match(computerId, /^comp-/)
  assert.ok(typeof paired.json.deviceToken === 'string' && paired.json.deviceToken.length > 10)

  // device-token 心跳:last_seen_at 推进。
  const beat0 = await pool.query<{ last: string | null }>(
    `SELECT last_seen_at::text AS last FROM computers WHERE id = $1`, [computerId])
  // requireDevice:Authorization Bearer <deviceToken>(与用户会话无关)。
  const hb = await memberMirror.call('/computers/heartbeat', {
    method: 'POST',
    headers: { authorization: `Bearer ${paired.json.deviceToken}` },
    body: JSON.stringify({ version: '0.0.1' }),
  })
  assert.equal(hb.status, 200)
  const beat1 = await pool.query<{ last: string | null }>(
    `SELECT last_seen_at::text AS last FROM computers WHERE id = $1`, [computerId])
  assert.ok(beat0.rows[0].last === null || beat1.rows[0].last >= beat0.rows[0].last)

  // assign:agent → computer。
  const agentId = `ag-tail-${randomUUID().slice(0, 6)}`
  await seedAgentRow(agentId)
  const assign = await memberMirror.call(`/agents/${agentId}/computer`, {
    method: 'POST',
    body: JSON.stringify({ computerId }),
  })
  assert.equal(assign.status, 200)
  const { rows: agRows } = await pool.query<{ computer_id: string }>(
    `SELECT computer_id FROM participants WHERE id = $1`, [agentId])
  assert.equal(agRows[0].computer_id, computerId)

  // runtime-token:requireDevice 认证 + agent 须已 assign 到该机。
  const tok = await memberMirror.call(`/agents/${agentId}/runtime-token`, {
    method: 'POST',
    headers: { authorization: `Bearer ${paired.json.deviceToken}` },
  })
  assert.equal(tok.status, 200)
  assert.ok(typeof tok.json.token === 'string' && tok.json.token.split('.').length === 3)
  const claims = JSON.parse(Buffer.from(tok.json.token.split('.')[1], 'base64url').toString('utf8'))
  assert.equal(claims.sub, agentId)
  assert.equal(claims.scope, 'agent-runner')

  // DELETE computer → 撤销(revoke)。
  const del = await memberMirror.call(`/computers/${computerId}`, { method: 'DELETE' })
  assert.equal(del.status, 200)
  const { rows: revRows } = await pool.query<{ revoked_at: string | null }>(
    `SELECT revoked_at FROM computers WHERE id = $1`, [computerId])
  assert.ok(revRows[0].revoked_at !== null)
})

/* ───────── DELETE /me/account ───────── */

test('[mirror-tail] DELETE /me/account: 哨兵邮箱 + participants departed + sessions 清空', async () => {
  const r = await memberMirror.call('/me/account', { method: 'DELETE' })
  assert.ok(r.status === 200 || r.status === 204)
  const { rows } = await pool.query<{ email: string; departed: string | null }>(
    `SELECT u.email, p.departed_at::text AS departed FROM users u
       LEFT JOIN participants p ON p.id = u.id AND p.company_id = $1
      WHERE u.id = $2`, [COMPANY, USER])
  assert.match(rows[0].email, /^deleted\+.*@cumora\.invalid$/)
  assert.ok(rows[0].departed !== null)
  const { rows: sess } = await pool.query(`SELECT 1 FROM sessions WHERE user_id = $1`, [USER])
  assert.equal(sess.length, 0)
})

/* ───────── /runtime/inbox 附件 URL freshen(#94 延期项) ───────── */

test('[mirror-tail] /runtime/inbox freshens legacy attachment URLs (key backfilled)', async () => {
  const agentId = `ag-tail-${randomUUID().slice(0, 6)}`
  await seedAgentRow(agentId)
  const convo = `cv-tail-${randomUUID().slice(0, 6)}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, 'direct', 'D', $2::jsonb, $3)`,
    [convo, JSON.stringify([agentId, USER]), COMPANY],
  )
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id, attachment)
     VALUES ('fm-1', $1, $2, 'text', 'see attached', 1, $3,
       '{"url":"/uploads/attachments/legacy.png","name":"legacy.png","kind":"img"}'::jsonb)`,
    [convo, USER, COMPANY],
  )
  const token = signAgentToken({ agentId, companyId: COMPANY })
  const res = await fetch(`${dualBase}/runtime/inbox`, { headers: { authorization: `Bearer ${token}` } })
  assert.equal(res.status, 200)
  const body = await res.json() as { rows: Array<{ attachment: Record<string, unknown> }> }
  assert.equal(body.rows.length, 1)
  // freshen:URL 重算 + key 回填(legacy 行原本无 key)。
  assert.equal(body.rows[0].attachment.url, '/uploads/attachments/legacy.png')
  assert.equal(body.rows[0].attachment.key, 'attachments/legacy.png')
})
