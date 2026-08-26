/**
 * Mirror test: /runtime/* agent-runner 面(#60)——TS(runtime-server.ts +
 * inproc-client + wake-bus)与 Go(internal/runtime + internal/wakebus)双跑。
 *
 * 覆盖:auth 闸(401 家族)、JWT 身份钉死(runs/events 落库取 sub 不取
 * body)、读面(persona/roster/system-prompt/inbox/context/climate/skills/
 * faces/memory)、triage 载荷(空箱/仅系统/人类 DM 短路)、agenda 轮询对
 * (byoa 载荷 / verdict 终判)、观测面(runs/events/finish/triage/llm-calls)、
 * 在场(status/typing/thinking/worklog/busy)、mark-read、notices(成员门 +
 * 去重)、wake-stream SSE(ready 帧 + Redis 发布 → wake 帧)。
 *
 * 已知拆票:/runtime/cli 的世界动作命令面(cli.ts ≈6100 行)未随 #60 迁
 * 移——MIRROR 形态断言 501+可识别错误,TS 形态维持原 400 行为。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import { randomUUID } from 'node:crypto'
import { ensureSchemaOnce, resetAllTables, teardownAll, MIRROR_BASE } from './_helpers.js'
import { signAgentToken } from '../agents/runtime/jwt.js'
import { pool } from '../db/pool.js'
import { redis } from '../redis.js'

let server: Server | null = null
let baseUrl = ''

before(async () => {
  await ensureSchemaOnce()
  if (MIRROR_BASE) {
    baseUrl = MIRROR_BASE
    return
  }
  const expressMod = await import('express')
  const express = expressMod.default
  const { runtimeRouter } = await import('../agents/runtime/server.js')
  const app = express()
  app.use(express.json({ limit: '4mb' }))
  app.use('/runtime', runtimeRouter)
  await new Promise<void>((resolve) => {
    server = createServer(app).listen(0, () => {
      const addr = server!.address()
      if (addr && typeof addr === 'object') baseUrl = `http://127.0.0.1:${addr.port}`
      resolve()
    })
  })
})

beforeEach(async () => {
  await resetAllTables()
})

after(async () => {
  await teardownAll(server)
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

async function seedAgent(extra?: { style?: string }): Promise<{ agentId: string; companyId: string; token: string }> {
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
    [companyId, `Co ${companyId}`, companyId, 'test-owner'],
  )
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status, system_prompt)
       VALUES ($1, $2, 'agent', $3, 'tester', $4, '#abcdef', 'avail', $5)`,
    [agentId, companyId, agentId, agentId.slice(0, 1).toUpperCase(), extra?.style ?? null],
  )
  const token = signAgentToken({ agentId, companyId })
  return { agentId, companyId, token }
}

async function seedHuman(companyId: string): Promise<string> {
  const humanId = `u-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
       VALUES ($1, $2, 'human', 'Alice', 'PM', 'A', '#112233', 'avail')`,
    [humanId, companyId],
  )
  return humanId
}

/** 会话 + 双成员;返回手写插消息的辅助。 */
async function seedConversation(companyId: string, members: string[], kind: 'direct' | 'group' = 'group') {
  const convId = `cv-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, $2, 'Test convo', $3::jsonb, $4)`,
    [convId, kind, JSON.stringify(members), companyId],
  )
  let seq = 0
  return {
    convId,
    insertMessage: async (authorId: string, body: string, mkind = 'text') => {
      seq += 1
      const id = `m-${randomUUID().slice(0, 8)}`
      await pool.query(
        `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
         VALUES ($1, $2, $3, $4, $5, $6, $7)`,
        [id, convId, authorId, mkind, body, seq, companyId],
      )
      return id
    },
  }
}

// ── auth 闸 ────────────────────────────────────────────────────────────

test('[mirror-runtime] missing Authorization → 401', async () => {
  const r = await call('/runtime/inbox', { method: 'GET' })
  assert.equal(r.status, 401)
  assert.match(String(r.body?.error ?? ''), /bearer/i)
})

test('[mirror-runtime] wrong scheme → 401', async () => {
  const res = await fetch(`${baseUrl}/runtime/inbox`, {
    headers: { authorization: 'Basic dXNlcjpwYXNz' },
  })
  assert.equal(res.status, 401)
  await res.text()
})

test('[mirror-runtime] bad signature → 401', async () => {
  const { token } = await seedAgent()
  const tampered = token.replace(/\.[^.]*$/, '.AAAA')
  const r = await call('/runtime/inbox', { method: 'GET', token: tampered })
  assert.equal(r.status, 401)
})

test('[mirror-runtime] malformed / expired token → 401', async () => {
  assert.equal((await call('/runtime/inbox', { method: 'GET', token: 'not-a-jwt' })).status, 401)
  const { agentId, companyId } = await seedAgent()
  const expired = signAgentToken({ agentId, companyId, ttlSeconds: -1 })
  assert.equal((await call('/runtime/inbox', { method: 'GET', token: expired })).status, 401)
})

// ── 读面 ────────────────────────────────────────────────────────────────

test('[mirror-runtime] /persona returns the PersonaRow shape', async () => {
  const { agentId, companyId, token } = await seedAgent({ style: 'terse and warm' })
  const r = await call('/runtime/persona', { method: 'GET', token })
  assert.equal(r.status, 200)
  const p = r.body?.persona
  assert.ok(p, 'persona object')
  assert.equal(p.id, agentId)
  assert.equal(p.companyId, companyId)
  assert.equal(p.role, 'tester')
  assert.equal(p.style, 'terse and warm')
  assert.equal(p.model, null)
})

test('[mirror-runtime] /conversation/company-id: 400 + happy path', async () => {
  const { companyId, token } = await seedAgent()
  const missing = await call('/runtime/conversation/company-id', { token, body: {} })
  assert.equal(missing.status, 400)
  const { convId } = await seedConversation(companyId, [])
  const r = await call('/runtime/conversation/company-id', { token, body: { conversationId: convId } })
  assert.equal(r.status, 200)
  assert.equal(r.body.companyId, companyId)
})

test('[mirror-runtime] /inbox surfaces unread rows with the InboxRow shape', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId])
  await convo.insertMessage(humanId, 'hello agent')
  const r = await call('/runtime/inbox?probe=1', { method: 'GET', token })
  assert.equal(r.status, 200)
  const rows = r.body?.rows
  assert.ok(Array.isArray(rows) && rows.length === 1, 'one unread row')
  const row = rows[0]
  assert.equal(row.conversation_id, convo.convId)
  assert.equal(row.author_id, humanId)
  assert.equal(row.author_kind, 'human')
  assert.equal(row.body, 'hello agent')
  assert.equal(row.sequence, 1)
  assert.equal(row.attachment, null)
  assert.equal(row.quoted, null)
  assert.equal(row.quoted_message_id, null)
  assert.equal(row.conversation_kind, 'group')
  assert.ok(typeof row.created_at === 'string' && row.created_at.endsWith('Z'))
  // probe=1 不推进边界:再取一次仍在(该路径不写 conversation_reads)。
  const again = await call('/runtime/inbox?probe=1', { method: 'GET', token })
  assert.equal(again.body.rows.length, 1)
})

test('[mirror-runtime] /context marks unread/self and aggregates reactions', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId])
  await convo.insertMessage(humanId, 'hi')
  const selfId = await convo.insertMessage(agentId, 'my reply')
  const r = await call('/runtime/context', { token, body: { conversationIds: [convo.convId] } })
  assert.equal(r.status, 200)
  const rows = r.body?.rows
  assert.equal(rows.length, 2)
  const byId = new Map(rows.map((x: any) => [x.id, x]))
  assert.equal(byId.get(selfId).is_self, true)
  assert.equal(byId.get(selfId).is_unread, false)
  assert.equal(rows[0].is_unread, true, 'human message unread (no read cursor)')
  assert.deepEqual(rows[0].reactions, [])
})

test('[mirror-runtime] /climate + /faces + /skills read surfaces', async () => {
  const { agentId, companyId, token } = await seedAgent()
  await pool.query(
    `INSERT INTO agent_climate (agent_id, about_id, affinity, trust, last_note) VALUES ($1, $2, 0.7, 2, 'warming')`,
    [agentId, 'someone'],
  )
  const humanId = await seedHuman(companyId)
  await pool.query(`UPDATE participants SET avatar_url = 'https://x.test/a.png' WHERE id = $1`, [humanId])
  await pool.query(
    `INSERT INTO agent_workspace (agent_id, path, body, company_id) VALUES ($1, 'skills/pdf/SKILL.md', $2, $3)`,
    [agentId, `---\nname: pdf\ndescription: Work with PDF files\n---\n\nBody here`, companyId],
  )
  const climate = await call('/runtime/climate', { method: 'GET', token })
  assert.equal(climate.status, 200)
  assert.equal(climate.body.rows.length, 1)
  assert.equal(climate.body.rows[0].about_id, 'someone')
  assert.equal(climate.body.rows[0].affinity, 0.7)

  const faces = await call('/runtime/faces', { token, body: { participantIds: [humanId] } })
  assert.equal(faces.status, 200)
  assert.equal(faces.body.rows.length, 1)
  assert.equal(faces.body.rows[0].id, humanId)
  assert.equal(faces.body.rows[0].avatar_url, 'https://x.test/a.png')

  const skills = await call('/runtime/skills', { method: 'GET', token })
  assert.equal(skills.status, 200)
  assert.equal(skills.body.rows.length, 1)
  assert.equal(skills.body.rows[0].name, 'pdf')
  assert.equal(skills.body.rows[0].description, 'Work with PDF files')
  assert.equal(skills.body.rows[0].path, 'skills/pdf/SKILL.md')
})

test('[mirror-runtime] /memory/query: pinned + project visibility filter', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const projectId = `pj-${randomUUID().slice(0, 8)}`
  await pool.query(`INSERT INTO projects (id, company_id, name) VALUES ($1, $2, 'P1')`, [projectId, companyId])
  await pool.query(
    `INSERT INTO agent_workspace (agent_id, path, body, meta, company_id) VALUES
       ($1, 'memory/notes/global.md', 'global note', '{"type":"memory","kind":"notes","pinned":false}'::jsonb, $2),
       ($1, 'memory/notes/pinned.md', 'pinned note', '{"type":"memory","kind":"notes","pinned":true}'::jsonb, $2),
       ($1, 'memory/projects/${projectId}/notes/secret.md', 'project note', '{"type":"memory","kind":"notes","pinned":false}'::jsonb, $2)`,
    [agentId, companyId],
  )
  // 无 scope:全局 + pinned,项目行不可见。
  const noScope = await call('/runtime/memory/query', { token, body: { queryText: '' } })
  assert.equal(noScope.status, 200)
  const bodies = noScope.body.rows.map((r: any) => r.body).sort()
  assert.deepEqual(bodies, ['global note', 'pinned note'])
  // 项目 scope:全见。
  const inScope = await call('/runtime/memory/query', { token, body: { queryText: '', projectIds: [projectId] } })
  assert.equal(inScope.body.rows.length, 3)
  // 行形状。
  const row = noScope.body.rows.find((r: any) => r.body === 'pinned note')
  assert.equal(row.pinned, true)
  assert.equal(row.kind, 'notes')
  assert.ok(typeof row.created_at === 'string')
})

test('[mirror-runtime] /system-prompt composes identity/soul/rules/roster', async () => {
  const { agentId, companyId, token } = await seedAgent({ style: 'spiky but kind' })
  const humanId = await seedHuman(companyId)
  await pool.query(
    `INSERT INTO agent_workspace (agent_id, path, body, company_id) VALUES
       ($1, 'IDENTITY.md', 'I am the test agent.', $2),
       ($1, 'SOUL.md', 'Slow to trust, quick to ship.', $2)`,
    [agentId, companyId],
  )
  const r = await call('/runtime/system-prompt', { method: 'GET', token })
  assert.equal(r.status, 200)
  const prompt = String(r.body.prompt)
  assert.match(prompt, /## YOUR IDENTITY \(from your workspace's IDENTITY\.md/)
  assert.match(prompt, /I am the test agent\./)
  assert.match(prompt, /## YOUR SOUL/)
  assert.match(prompt, /Slow to trust, quick to ship\./)
  assert.match(prompt, /Your style: spiky but kind/)
  assert.match(prompt, /HOW YOU EXIST IN CUMORA:/)
  assert.match(prompt, /YOUR TEAMMATES/)
  assert.match(prompt, new RegExp(`- ${humanId} — Alice, PM`))
})

test('[mirror-runtime] /roster: 404 unknown agent; text for known', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const unknown = signAgentToken({ agentId: 'a-nope', companyId })
  assert.equal((await call('/runtime/roster', { method: 'GET', token: unknown })).status, 404)
  const r = await call('/runtime/roster', { method: 'GET', token })
  assert.equal(r.status, 200)
  assert.match(String(r.body.roster), new RegExp(`- ${humanId} — Alice, PM`))
  assert.ok(!String(r.body.roster).includes(agentId), 'self excluded')
})

// ── triage 载荷 ─────────────────────────────────────────────────────────

test('[mirror-runtime] /inbox-triage/payload: empty inbox → empty-inbox verdict', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/inbox-triage/payload', { method: 'GET', token })
  assert.equal(r.status, 200)
  assert.equal(r.body.verdict.actionable, false)
  assert.equal(r.body.verdict.source, 'empty-inbox')
  assert.equal(r.body.instructions, undefined)
})

test('[mirror-runtime] /inbox-triage/payload: system-only inbox → no model call', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId])
  await convo.insertMessage(humanId, JSON.stringify({ kind: 'relay' }), 'system')
  const r = await call('/runtime/inbox-triage/payload', { method: 'GET', token })
  assert.equal(r.status, 200)
  assert.equal(r.body.verdict.source, 'system-only')
})

test('[mirror-runtime] /inbox-triage/payload: human DM bypasses the gate entirely', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId], 'direct')
  await convo.insertMessage(humanId, 'ping')
  const r = await call('/runtime/inbox-triage/payload', { method: 'GET', token })
  assert.equal(r.status, 200)
  assert.equal(r.body.verdict.actionable, true)
  assert.equal(r.body.verdict.source, 'human-dm')
})

test('[mirror-runtime] /inbox-triage/payload: agent-only group chat → model prompt shape', async () => {
  const { agentId, companyId, token } = await seedAgent({ style: 'quiet' })
  const otherAgent = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status) VALUES ($1, $2, 'agent', 'Bee', 'dev', 'B', '#000', 'avail')`,
    [otherAgent, companyId],
  )
  const convo = await seedConversation(companyId, [agentId, otherAgent])
  await convo.insertMessage(otherAgent, 'what do you think about the plan?')
  const r = await call('/runtime/inbox-triage/payload', { method: 'GET', token })
  assert.equal(r.status, 200)
  assert.equal(r.body.verdict, undefined, 'falls through to the model prompt')
  assert.equal(typeof r.body.instructions, 'string')
  assert.match(r.body.instructions, /inbox triage cerebellum/)
  assert.match(r.body.input, new RegExp(`Agent: ${agentId}`))
  assert.match(r.body.input, /Unread inbox:/)
  assert.match(r.body.input, /Open work state/)
  assert.equal(r.body.failClosed, true, 'purely agent-to-agent wake fails closed locally')
})

// ── agenda 轮询对 ───────────────────────────────────────────────────────

async function seedAgendaCard(agentId: string, companyId: string): Promise<string> {
  const boardId = `b-${randomUUID().slice(0, 8)}`
  const columnId = `col-${randomUUID().slice(0, 8)}`
  const cardId = `card-${randomUUID().slice(0, 8)}`
  await pool.query(`INSERT INTO boards (id, company_id, title, created_by) VALUES ($1,$2,'Board','test')`, [boardId, companyId])
  await pool.query(`INSERT INTO board_columns (id, board_id, title, position) VALUES ($1,$2,'To Do',0)`, [columnId, boardId])
  await pool.query(
    `INSERT INTO board_cards (id, board_id, column_id, title, assignee_id, created_by) VALUES ($1,$2,$3,'Ship it',$4,'test')`,
    [cardId, boardId, columnId, agentId],
  )
  return cardId
}

test('[mirror-runtime] GET /agenda byoa-routed → classifier payload, never a synchronous verdict', async () => {
  const { setCerebellumSettings } = await import('../cerebellum-settings.js')
  const { agentId, companyId, token } = await seedAgent()
  await seedAgendaCard(agentId, companyId)
  const computerId = `comp-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO computers (id, company_id, name, kind, available_engines, status) VALUES ($1,$2,'MacBook','local','["claude"]'::jsonb,'online')`,
    [computerId, companyId],
  )
  await pool.query(`UPDATE participants SET computer_id = $1 WHERE id = $2`, [computerId, agentId])
  await setCerebellumSettings({ route: 'byoa', localEngine: 'claude' }, 'test')
  try {
    const r = await call('/runtime/agenda', { method: 'GET', token })
    assert.equal(r.status, 200)
    assert.equal(typeof r.body.instructions, 'string')
    assert.equal(typeof r.body.input, 'string')
    assert.match(r.body.instructions, /heartbeat agenda triage/i)
    assert.equal(r.body.actionable, undefined, 'byoa route must never get a synchronous remote verdict')
    assert.equal(r.body.agenda.cards.length, 1)
    assert.equal(r.body.agenda.cards[0].title, 'Ship it')
  } finally {
    await setCerebellumSettings({ route: 'remote', localEngine: 'claude' }, 'test')
  }
})

test('[mirror-runtime] GET /agenda remote-routed → synchronous decision shape', async () => {
  const { agentId, companyId, token } = await seedAgent()
  await seedAgendaCard(agentId, companyId)
  // TS 侧默认路由即 remote;LLM 覆盖属 TS 单侧能力,此处只断言形状契约:
  // 有终判(actionable 布尔),绝不回传 daemon-classify 载荷。
  const r = await call('/runtime/agenda', { method: 'GET', token })
  assert.equal(r.status, 200)
  assert.equal(r.body.instructions, undefined, 'remote route must not hand back a daemon-classify payload')
  assert.equal(typeof r.body.actionable, 'boolean')
  assert.equal(typeof r.body.cards, 'number')
})

test('[mirror-runtime] POST /agenda/verdict finalizes an actionable verdict into a brief', async () => {
  const { agentId, companyId, token } = await seedAgent()
  await seedAgendaCard(agentId, companyId)
  const r = await call('/runtime/agenda/verdict', {
    token, body: { actionable: true, focus: 'Ship the migration', reason: 'card is due' },
  })
  assert.equal(r.status, 200)
  assert.equal(r.body.actionable, true)
  assert.equal(r.body.focus, 'Ship the migration')
  assert.equal(r.body.cards, 1)
  assert.match(String(r.body.brief), /Ship it/)
})

test('[mirror-runtime] POST /agenda/verdict actionable=false → fail-closed shape', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/agenda/verdict', { token, body: { actionable: false, reason: 'classifier error' } })
  assert.equal(r.status, 200)
  assert.equal(r.body.actionable, false)
  assert.equal(r.body.brief, undefined)
})

// ── 观测面 ──────────────────────────────────────────────────────────────

test('[mirror-runtime] /runs + /events + /finish: identity pin + persisted transition', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const created = await call('/runtime/runs', {
    token, body: { trigger: { kind: 't' }, inboxCount: 0, agentId: 'someone-else' },
  })
  assert.equal(created.status, 200)
  const runId = String(created.body.runId)
  {
    const { rows } = await pool.query<any>(`SELECT agent_id, company_id, status FROM agent_runs WHERE id = $1`, [runId])
    assert.equal(rows.length, 1)
    assert.equal(rows[0].agent_id, agentId, 'JWT subject pins agent_id')
    assert.equal(rows[0].company_id, companyId)
    assert.equal(rows[0].status, 'running')
  }
  const ev = await call('/runtime/events', {
    token,
    body: { runId, kind: 'test.event', title: 'pinning check', data: { hi: 1 }, agentId: 'someone-else', companyId: 'wrong-co' },
  })
  assert.equal(ev.status, 200)
  {
    const { rows } = await pool.query<any>(
      `SELECT agent_id, company_id, kind, level FROM agent_events WHERE run_id = $1 AND kind = 'test.event'`, [runId])
    assert.equal(rows.length, 1)
    assert.equal(rows[0].agent_id, agentId)
    assert.equal(rows[0].company_id, companyId)
    assert.equal(rows[0].level, 'info')
  }
  const done = await call(`/runtime/runs/${runId}/finish`, {
    token, body: { status: 'completed', summary: 'ok', toolCallCount: 3, tokenCount: 1234 },
  })
  assert.equal(done.status, 200)
  const { rows } = await pool.query<any>(
    `SELECT status, summary, tool_call_count, token_count FROM agent_runs WHERE id = $1`, [runId])
  assert.equal(rows[0].status, 'completed')
  assert.equal(rows[0].summary, 'ok')
  assert.equal(rows[0].tool_call_count, 3)
  assert.equal(rows[0].token_count, 1234)
})

test('[mirror-runtime] 400 family: events/status/typing/finish/notices', async () => {
  const { token } = await seedAgent()
  assert.equal((await call('/runtime/events', { token, body: { kind: 'x', title: 'y' } })).status, 400)
  assert.equal((await call('/runtime/status', { token, body: {} })).status, 400)
  assert.equal((await call('/runtime/typing', { token, body: { done: false } })).status, 400)
  assert.equal((await call('/runtime/runs/some-run/finish', { token, body: {} })).status, 400)
  assert.equal((await call('/runtime/notices', { token, body: { conversationId: 'c-1' } })).status, 400)
})

test('[mirror-runtime] /triage: agent_triages row + llm_calls mirror when usage present', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const r = await call('/runtime/triage', {
    token,
    body: {
      source: 'byoa-claude', model: 'claude-haiku', actionable: false, reason: 'not mine',
      usage: { inputTokens: 100, cachedInputTokens: 20, cacheCreationTokens: 0, outputTokens: 5 },
      daemonVersion: '1.2.3',
    },
  })
  assert.equal(r.status, 200)
  assert.equal(r.body.ok, true)
  {
    const { rows } = await pool.query<any>(`SELECT * FROM agent_triages WHERE agent_id = $1`, [agentId])
    assert.equal(rows.length, 1)
    assert.equal(rows[0].company_id, companyId)
    assert.equal(rows[0].source, 'byoa-claude')
    assert.equal(rows[0].model, 'claude-haiku')
    assert.equal(rows[0].actionable, false)
    assert.equal(rows[0].measured, true)
    assert.equal(rows[0].input_tokens, 100)
  }
  {
    // TS 侧 recordLlmCall 是 fire-and-forget(res.json 不等插入)——立即
    // 查询会输给竞态;轮询短窗等待镜像行落库(Go 侧同步插入,首查即中)。
    let rows: any[] = []
    for (let i = 0; i < 50 && rows.length === 0; i++) {
      const r = await pool.query<any>(`SELECT * FROM llm_calls WHERE agent_id = $1`, [agentId])
      rows = r.rows
      if (rows.length === 0) await new Promise((res) => setTimeout(res, 50))
    }
    assert.equal(rows.length, 1, 'mirrored into the universal ledger')
    assert.equal(rows[0].purpose, 'inbox-triage')
    assert.equal(rows[0].source, 'byoa-claude')
    assert.equal(rows[0].daemon_version, '1.2.3')
    assert.equal(rows[0].input_tokens, 100)
  }
})

test('[mirror-runtime] /llm-calls: hop batch with purpose whitelist', async () => {
  const { agentId, token } = await seedAgent()
  const r = await call('/runtime/llm-calls', {
    token,
    body: {
      source: 'byoa-codex', daemonVersion: '9.9.9',
      hops: [
        { purpose: 'agent-turn', model: 'gpt-5', usage: { inputTokens: 10, cachedInputTokens: 0, cacheCreationTokens: 0, outputTokens: 3 }, latencyMs: 120 },
        { purpose: 'sneaky-new-purpose', model: '', latencyMs: 5 },
      ],
    },
  })
  assert.equal(r.status, 200)
  assert.equal(r.body.inserted, 2)
  const { rows } = await pool.query<any>(`SELECT purpose, model, source, latency_ms FROM llm_calls WHERE agent_id = $1 ORDER BY latency_ms DESC`, [agentId])
  assert.equal(rows.length, 2)
  assert.equal(rows[0].purpose, 'agent-turn')
  assert.equal(rows[0].model, 'gpt-5')
  assert.equal(rows[1].purpose, 'agent-turn', 'unknown purpose coerced')
  assert.equal(rows[1].model, '<unknown>')
  assert.equal(rows[1].source, 'byoa-codex')
})

// ── 状态 + 在场 ─────────────────────────────────────────────────────────

test('[mirror-runtime] /status + /status/heartbeat update participants + broadcast ok', async () => {
  const { agentId, token } = await seedAgent()
  const set = await call('/runtime/status', { token, body: { status: 'working' } })
  assert.equal(set.status, 200)
  assert.equal(set.body.ok, true)
  {
    const { rows } = await pool.query<any>(`SELECT status FROM participants WHERE id = $1`, [agentId])
    assert.equal(rows[0].status, 'working')
  }
  const hb = await call('/runtime/status/heartbeat', { token, body: { status: 'working' } })
  assert.equal(hb.status, 200)
  // TS 语义:只查非空——任意字符串直写(列无 CHECK);heartbeat 的
  // status 不匹配当前值时 UPDATE 零行,同样 200。
  const odd = await call('/runtime/status', { token, body: { status: 'partying' } })
  assert.equal(odd.status, 200)
  {
    const { rows } = await pool.query<any>(`SELECT status FROM participants WHERE id = $1`, [agentId])
    assert.equal(rows[0].status, 'partying', 'TS writes whatever non-empty status it is given')
  }
  const oddHb = await call('/runtime/status/heartbeat', { token, body: { status: 'avail' } })
  assert.equal(oddHb.status, 200)
})

test('[mirror-runtime] /typing returns ok', async () => {
  const { companyId, token } = await seedAgent()
  const { convId } = await seedConversation(companyId, [])
  const r = await call('/runtime/typing', { token, body: { conversationId: convId, done: false } })
  assert.equal(r.status, 200)
  assert.equal(r.body.ok, true)
})

test('[mirror-runtime] /busy/heartbeat + /busy/clear round-trip', async () => {
  const { token } = await seedAgent()
  assert.equal((await call('/runtime/busy/heartbeat', { token, body: { ttlSec: 30 } })).body.ok, true)
  assert.equal((await call('/runtime/busy/clear', { token, body: {} })).body.ok, true)
})

test('[mirror-runtime] thinking mark/peek/unmark', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const { convId } = await seedConversation(companyId, [agentId])
  const mark = await call('/runtime/thinking/mark', { token, body: { conversationIds: [convId], ttlSec: 60 } })
  assert.equal(mark.status, 200)
  const peek = await call(`/runtime/thinking/peek?conversationId=${convId}`, { method: 'GET', token })
  assert.equal(peek.status, 200)
  assert.equal(peek.body.agents.length, 1)
  assert.equal(peek.body.agents[0].agentId, agentId)
  assert.ok(typeof peek.body.agents[0].claimedAt === 'number')
  assert.equal((await call(`/runtime/thinking/peek?conversationId=`, { method: 'GET', token })).status, 400)
  const unmark = await call('/runtime/thinking/unmark', { token, body: { conversationIds: [convId] } })
  assert.equal(unmark.status, 200)
  const cleared = await call(`/runtime/thinking/peek?conversationId=${convId}`, { method: 'GET', token })
  assert.deepEqual(cleared.body.agents, [])
})

test('[mirror-runtime] worklog claim/peek/release', async () => {
  const { agentId, token } = await seedAgent()
  // scope 随机化:Redis 不随库 TRUNCATE,固定 scope 会撞上一轮残留 claim。
  const scope = `convo:t-${randomUUID().slice(0, 8)}`
  const bad = await call('/runtime/worklog/claim', { token, body: { scopeKey: scope } })
  assert.equal(bad.status, 400)
  const first = await call('/runtime/worklog/claim', {
    token, body: { scopeKey: scope, taskType: 'web-search', subject: 'Warm pastels', ttlSec: 120 },
  })
  assert.equal(first.status, 200)
  assert.equal(first.body.accepted, true)
  const peek = await call(`/runtime/worklog/peek?scopeKey=${scope}`, { method: 'GET', token })
  assert.equal(peek.body.entries.length, 1)
  assert.equal(peek.body.entries[0].agentId, agentId)
  assert.equal(peek.body.entries[0].subject, 'Warm pastels')

  // 另一 agent 抢同一(归一化后相同的)主题 → 让位 + 现有持有者详情。
  const other = await seedAgent()
  const second = await call('/runtime/worklog/claim', {
    token: other.token, body: { scopeKey: scope, taskType: 'web-search', subject: 'warm  PASTELS ' },
  })
  assert.equal(second.body.accepted, false)
  assert.equal(second.body.existing.agentId, agentId)

  // 释放(非持有者释放无效)后可重抢。
  await call('/runtime/worklog/release', { token: other.token, body: { scopeKey: scope, taskType: 'web-search', subject: 'Warm pastels' } })
  const stillHeld = await call(`/runtime/worklog/peek?scopeKey=${scope}`, { method: 'GET', token })
  assert.equal(stillHeld.body.entries.length, 1, "someone else's release is a no-op")
  await call('/runtime/worklog/release', { token, body: { scopeKey: scope, taskType: 'web-search', subject: 'Warm pastels' } })
  const reclaim = await call('/runtime/worklog/claim', {
    token: other.token, body: { scopeKey: scope, taskType: 'web-search', subject: 'warm pastels', ttlSec: 120 },
  })
  assert.equal(reclaim.body.accepted, true)
})

// ── mark-read + notices ─────────────────────────────────────────────────

test('[mirror-runtime] /conversation/mark-read advances the cursor monotonically', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId])
  const m1 = await convo.insertMessage(humanId, 'one')
  const m2 = await convo.insertMessage(humanId, 'two')
  const r = await call('/runtime/conversation/mark-read', { token, body: { conversationId: convo.convId, upToMessageId: m2 } })
  assert.equal(r.status, 200)
  {
    const { rows } = await pool.query<any>(
      `SELECT last_read_message_id FROM conversation_reads WHERE user_id = $1 AND conversation_id = $2`,
      [agentId, convo.convId])
    assert.equal(rows.length, 1)
    assert.equal(rows[0].last_read_message_id, m2)
  }
  // 乱序回退不回撤游标。
  await call('/runtime/conversation/mark-read', { token, body: { conversationId: convo.convId, upToMessageId: m1 } })
  const { rows } = await pool.query<any>(
    `SELECT last_read_message_id FROM conversation_reads WHERE user_id = $1 AND conversation_id = $2`,
    [agentId, convo.convId])
  assert.equal(rows[0].last_read_message_id, m2)
})

test('[mirror-runtime] /notices: member gate + dedupe + system row', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId])
  const outsider = await seedAgent()
  // 去重键随机化:Redis 不随库 TRUNCATE,固定键会被上一轮运行残留吞掉。
  const dedupeKey = `k-${randomUUID().slice(0, 8)}`
  const forbidden = await call('/runtime/notices', {
    token: outsider.token,
    body: { conversationId: convo.convId, noticeKind: 'quota_exhausted', text: 'x', dedupeKey: `other-${dedupeKey}` },
  })
  assert.equal(forbidden.status, 403)

  const posted = await call('/runtime/notices', {
    token,
    body: { conversationId: convo.convId, noticeKind: 'quota_exhausted', text: 'quota gone', dedupeKey },
  })
  assert.equal(posted.status, 200)
  assert.equal(posted.body.posted, true)
  const dup = await call('/runtime/notices', {
    token,
    body: { conversationId: convo.convId, noticeKind: 'quota_exhausted', text: 'quota gone', dedupeKey },
  })
  assert.equal(dup.body.posted, false, 'dedupe window swallows the repeat')
  const { rows } = await pool.query<any>(
    `SELECT kind, body, author_id, sequence FROM messages WHERE conversation_id = $1 AND kind = 'system'`, [convo.convId])
  assert.equal(rows.length, 1)
  assert.equal(rows[0].author_id, agentId)
  const payload = JSON.parse(rows[0].body)
  assert.equal(payload.kind, 'notice')
  assert.equal(payload.noticeKind, 'quota_exhausted')
  assert.equal(payload.text, 'quota gone')
})

// ── /cli 出口(#60 拆票:MIRROR 侧 501,TS 侧维持原 400 校验) ─────────

test('[mirror-runtime] /cli outlet: argv validation (TS) / explicit not-yet-migrated (Go)', async () => {
  const { token } = await seedAgent()
  if (MIRROR_BASE) {
    const r = await call('/runtime/cli', { token, body: { argv: ['inbox'] } })
    assert.equal(r.status, 501)
    assert.match(String(r.body?.error ?? ''), /not yet migrated/i)
  } else {
    const bad = await call('/runtime/cli', { token, body: {} })
    assert.equal(bad.status, 400)
  }
})

// ── wake-stream(SSE + Redis 总线) ────────────────────────────────────

/** 读 SSE 流直到 collector 返回真(按帧回调)。超时兜底防挂死。 */
async function readSSE(
  url: string, token: string,
  onFrame: (frame: string) => boolean,
  timeoutMS = 8000,
): Promise<void> {
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), timeoutMS)
  try {
    const res = await fetch(url, {
      headers: { authorization: `Bearer ${token}`, accept: 'text/event-stream' },
      signal: ctrl.signal,
    })
    assert.equal(res.status, 200)
    assert.match(res.headers.get('content-type') ?? '', /text\/event-stream/)
    const reader = (res.body as any).getReader() as { read(): Promise<{ done: boolean; value?: Uint8Array }> }
    const decoder = new TextDecoder()
    let buf = ''
    for (;;) {
      const { done, value } = await reader.read()
      if (done) break
      buf += decoder.decode(value, { stream: true })
      for (;;) {
        const idx = buf.indexOf('\n\n')
        if (idx < 0) break
        const frame = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        if (onFrame(frame)) return
      }
    }
    throw new Error('SSE stream ended before the expected frame')
  } finally {
    clearTimeout(timer)
    ctrl.abort()
  }
}

test('[mirror-runtime] /wake-stream: ready frame, then a published wake event', async () => {
  const { agentId, token } = await seedAgent()
  let resolveWake: () => void = () => {}
  const wakeSeen = new Promise<void>((res) => { resolveWake = res })
  const streamDone = readSSE(`${baseUrl}/runtime/wake-stream`, token, (frame) => {
    if (frame.startsWith('event: ready')) {
      // ready 已到(协议保证 SUBSCRIBE 先于 ready)→ 现在从总线外发布,
      // 就像任何 scheduler 实例 deliver() 那样。
      void redis.publish(`cumora:wake:${agentId}`, JSON.stringify({
        kind: 'wake', id: `wake-test-${randomUUID()}`, at: Date.now(),
        reason: 'message.new', conversationId: 'cv-x',
      })).catch(() => {})
      return false
    }
    if (frame.startsWith('event: wake')) {
      const dataLine = frame.split('\n').find((l) => l.startsWith('data: '))
      const evt = JSON.parse(dataLine!.slice('data: '.length))
      assert.equal(evt.kind, 'wake')
      assert.equal(evt.reason, 'message.new')
      assert.equal(evt.conversationId, 'cv-x')
      resolveWake()
      return true
    }
    return false
  })
  await Promise.all([streamDone, wakeSeen])
})

test('[mirror-runtime] /wake-stream: the stream STAYS OPEN after ready (no early server close)', async () => {
  // 真机 daemon 曾抓到的形态:ready 帧后服务端立刻关流,daemon 退避重连
  // 风暴。镜像测试此前读完首帧即 abort,测不出"早关"——这里在 ready 后
  // 静置 1.5s,断言流仍未 EOF(连接仍活)。
  const { token } = await seedAgent()
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), 6000)
  try {
    const res = await fetch(`${baseUrl}/runtime/wake-stream`, {
      headers: { authorization: `Bearer ${token}` },
      signal: ctrl.signal,
    })
    assert.equal(res.status, 200)
    const reader = (res.body as any).getReader() as { read(): Promise<{ done: boolean; value?: Uint8Array }> }
    const dec = new TextDecoder()
    let buf = ''
    let sawReady = false
    const deadline = Date.now() + 1500
    for (;;) {
      const waitMS = sawReady ? Math.max(1, deadline - Date.now()) : 6000
      const result = await Promise.race([
        reader.read(),
        new Promise<{ done: true }>((resolve) => setTimeout(() => resolve({ done: true } as const), waitMS)),
      ])
      if (result.done) {
        if (!sawReady) throw new Error('stream ended before the ready frame')
        // 超时窗口耗尽而未 EOF = 流仍开。若真 EOF,done 会带着 value 缺失
        // 提前到达——此处到达即说明 ready 后 1.5s 内被服务端关闭。
        const endedEarly = Date.now() < deadline
        assert.ok(!endedEarly, `server closed the stream right after ready (${Date.now()} < ${deadline})`)
        return
      }
      buf += dec.decode(result.value, { stream: true })
      if (!sawReady && buf.includes('event: ready')) sawReady = true
      if (Date.now() >= deadline) return // 静置窗口过完,流仍开 = 通过
    }
  } finally {
    clearTimeout(timer)
    ctrl.abort()
  }
})

test('[mirror-runtime] /wake-stream: steer events ride the same stream', async () => {
  const { agentId, token } = await seedAgent()
  let resolveSeen: () => void = () => {}
  const steerSeen = new Promise<void>((res) => { resolveSeen = res })
  const streamDone = readSSE(`${baseUrl}/runtime/wake-stream`, token, (frame) => {
    if (frame.startsWith('event: ready')) {
      void redis.publish(`cumora:wake:${agentId}`, JSON.stringify({
        kind: 'steer', id: `steer-test-${randomUUID()}`, at: Date.now(),
        conversationId: 'cv-y', messageId: 'm-1', authorName: 'Alice', body: 'mid-turn injection',
      })).catch(() => {})
      return false
    }
    if (frame.startsWith('event: steer')) {
      const dataLine = frame.split('\n').find((l) => l.startsWith('data: '))
      const evt = JSON.parse(dataLine!.slice('data: '.length))
      assert.equal(evt.kind, 'steer')
      assert.equal(evt.body, 'mid-turn injection')
      resolveSeen()
      return true
    }
    return false
  })
  await Promise.all([streamDone, steerSeen])
})
