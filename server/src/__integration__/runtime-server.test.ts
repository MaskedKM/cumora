/**
 * Integration test: the `/runtime/*` HTTP surface that agent-runner pods
 * talk to (server/src/agents/runtime/server.ts).
 *
 * The unit tests in __tests__ cover the argv-strip helper and the JWT
 * sign/verify in isolation; this file glues them together with a real
 * Express mount + real Postgres so we can assert the *security-critical*
 * end-to-end behaviour:
 *
 *  - Auth gate: missing / wrong-scheme / bad-sig / expired tokens are
 *    rejected with 401 BEFORE any handler logic runs.
 *  - Identity pin: when a request body claims `agentId: <spoof>` while
 *    the JWT pins someone else, the row that lands in Postgres uses the
 *    JWT subject, not the body claim.
 *  - Payload validation: 400s for missing required fields, without ever
 *    touching the DB.
 *
 * The full /cli endpoint (which actually spawns runCli) is intentionally
 * not exercised here — booting the CLI requires the whole agent stack.
 * Its security-critical bit (argv strip + JWT-sub injection) lives in
 * agents-runtime-cli-argv.test.ts as a unit test.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import { randomUUID } from 'node:crypto'
import { ensureSchemaOnce, resetAllTables, teardownAll } from './_helpers.js'
import { signAgentToken } from '../agents/runtime/jwt.js'
import { pool } from '../db/pool.js'

let server: Server
let baseUrl = ''

before(async () => {
  await ensureSchemaOnce()
  // Mount the runtime router on a minimal Express app. We can't use the
  // real index.ts entrypoint because it boots schedulers + cron + redis
  // subscribers we don't want in this test process.
  const expressMod = await import('express')
  const express = expressMod.default
  const { runtimeRouter } = await import('../agents/runtime/server.js')
  const app = express()
  app.use(express.json({ limit: '4mb' }))
  app.use('/runtime', runtimeRouter)
  await new Promise<void>((resolve) => {
    server = createServer(app).listen(0, () => {
      const addr = server.address()
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
  opts: { method?: string; token?: string; body?: unknown; headers?: Record<string, string> } = {},
): Promise<{ status: number; body: any }> {
  const headers: Record<string, string> = { 'content-type': 'application/json', ...(opts.headers ?? {}) }
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

async function seedAgent(): Promise<{ agentId: string; companyId: string; token: string }> {
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
    [companyId, `Co ${companyId}`, companyId, 'test-owner'],
  )
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
       VALUES ($1, $2, 'agent', $3, 'tester', $4, '#abcdef', 'avail')`,
    [agentId, companyId, agentId, agentId.slice(0, 1).toUpperCase()],
  )
  const token = signAgentToken({ agentId, companyId })
  return { agentId, companyId, token }
}

// ── auth gate ──────────────────────────────────────────────────────────

test('[integration] runtime: missing Authorization → 401', async () => {
  const r = await call('/runtime/inbox', { method: 'GET' })
  assert.equal(r.status, 401)
  assert.match(String(r.body?.error ?? ''), /bearer/i)
})

test('[integration] runtime: wrong scheme (Basic instead of Bearer) → 401', async () => {
  const r = await call('/runtime/inbox', {
    method: 'GET',
    headers: { authorization: 'Basic dXNlcjpwYXNz' },
  })
  assert.equal(r.status, 401)
})

test('[integration] runtime: bad signature → 401', async () => {
  // Mint a valid token, then tamper with the signature segment.
  const { token } = await seedAgent()
  const tampered = token.replace(/\.[^.]*$/, '.AAAA')
  const r = await call('/runtime/inbox', { method: 'GET', token: tampered })
  assert.equal(r.status, 401)
  assert.match(String(r.body?.error ?? ''), /signature|malformed/i)
})

test('[integration] runtime: malformed token (not three segments) → 401', async () => {
  const r = await call('/runtime/inbox', { method: 'GET', token: 'not-a-jwt' })
  assert.equal(r.status, 401)
  assert.match(String(r.body?.error ?? ''), /malformed/i)
})

test('[integration] runtime: expired token → 401', async () => {
  const { agentId, companyId } = await seedAgent()
  // ttlSeconds = -1 → exp = now - 1s, definitely expired.
  const tok = signAgentToken({ agentId, companyId, ttlSeconds: -1 })
  const r = await call('/runtime/inbox', { method: 'GET', token: tok })
  assert.equal(r.status, 401)
  assert.match(String(r.body?.error ?? ''), /expired/i)
})

// ── identity pin: agentId comes from JWT, not request body ────────────

test('[integration] runtime: /runs records the JWT subject, not whatever the body claims', async () => {
  const { agentId, companyId, token } = await seedAgent()
  // Try to spoof: body claims a different agentId. The endpoint shape
  // doesn't actually read agentId from body (the server takes it from
  // c.sub), but we send the spoof anyway to catch any regression that
  // adds a body.agentId read.
  const r = await call('/runtime/runs', {
    token,
    body: { trigger: { kind: 'test' }, inboxCount: 0, agentId: 'someone-else' },
  })
  assert.equal(r.status, 200)
  const runId = String(r.body?.runId ?? '')
  assert.ok(runId.length > 0, 'runId returned')
  const { rows } = await pool.query<{ agent_id: string; company_id: string }>(
    `SELECT agent_id, company_id FROM agent_runs WHERE id = $1`,
    [runId],
  )
  assert.equal(rows.length, 1)
  assert.equal(rows[0].agent_id, agentId, 'JWT subject pins agent_id')
  assert.equal(rows[0].company_id, companyId, 'JWT companyId pins company_id')
})

test('[integration] runtime: /events row uses JWT subject for agent_id', async () => {
  const { agentId, companyId, token } = await seedAgent()
  // First create a run to attach an event to (events.run_id FK → agent_runs.id).
  const runRes = await call('/runtime/runs', {
    token, body: { trigger: { kind: 'test' }, inboxCount: 0 },
  })
  const runId = String(runRes.body.runId)
  const r = await call('/runtime/events', {
    token,
    body: {
      runId,
      kind: 'test.event',
      title: 'pinning check',
      data: { hi: 1 },
      // Body claims someone else's id — must be ignored.
      agentId: 'someone-else',
      companyId: 'wrong-co',
    },
  })
  assert.equal(r.status, 200)
  const { rows } = await pool.query<{ agent_id: string; company_id: string; kind: string }>(
    `SELECT agent_id, company_id, kind FROM agent_events WHERE run_id = $1 AND kind = 'test.event'`,
    [runId],
  )
  assert.equal(rows.length, 1)
  assert.equal(rows[0].agent_id, agentId)
  assert.equal(rows[0].company_id, companyId)
})

// ── payload validation ────────────────────────────────────────────────

test('[integration] runtime: /events 400 when runId / kind / title is missing', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/events', { token, body: { kind: 'x', title: 'y' } })
  assert.equal(r.status, 400)
  assert.match(String(r.body?.error ?? ''), /runId|kind|title/i)
})

test('[integration] runtime: /status 400 when status field missing', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/status', { token, body: {} })
  assert.equal(r.status, 400)
})

test('[integration] runtime: /typing 400 when conversationId missing', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/typing', { token, body: { done: false } })
  assert.equal(r.status, 400)
})

test('[integration] runtime: /runs/:runId/finish 400 when status missing', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/runs/some-run/finish', { token, body: {} })
  assert.equal(r.status, 400)
})

test('[integration] runtime: /notices 400 when any of the required fields missing', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/notices', { token, body: { conversationId: 'c-1' } })
  assert.equal(r.status, 400)
  assert.match(String(r.body?.error ?? ''), /required/i)
})

// ── happy-path smokes (round-trips through inproc-client into PG) ─────

test('[integration] runtime: /runs + /runs/:runId/finish persists status transition', async () => {
  const { agentId, token } = await seedAgent()
  const created = await call('/runtime/runs', { token, body: { trigger: { kind: 't' }, inboxCount: 0 } })
  assert.equal(created.status, 200)
  const runId = String(created.body.runId)

  const done = await call(`/runtime/runs/${runId}/finish`, {
    token, body: { status: 'completed', summary: 'ok', toolCallCount: 3, tokenCount: 1234 },
  })
  assert.equal(done.status, 200)

  const { rows } = await pool.query<{ status: string; summary: string; tool_call_count: number; token_count: number; agent_id: string }>(
    `SELECT status, summary, tool_call_count, token_count, agent_id FROM agent_runs WHERE id = $1`,
    [runId],
  )
  assert.equal(rows.length, 1)
  assert.equal(rows[0].status, 'completed')
  assert.equal(rows[0].summary, 'ok')
  assert.equal(rows[0].tool_call_count, 3)
  assert.equal(rows[0].token_count, 1234)
  assert.equal(rows[0].agent_id, agentId)
})

// ── ticket #20: BYOA agenda-triage daemon-poll path ────────────────────
//
// Mirrors the `/inbox-triage/payload` conventions this file doesn't (yet)
// cover directly, but matches `agent-idle-agenda.test.ts`'s seeding style.
// The daemon-side EngineAdapter.classify() invocation is out of scope here
// (covered by the daemon's own --doctor probe per docs/BYOA.md) — these
// tests only assert the server's half of the payload/verdict contract.

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

test('[integration] runtime: GET /agenda for a byoa-routed agent returns the classifier payload, not a verdict', async () => {
  const { env } = await import('../env.js')
  const { agentId, companyId, token } = await seedAgent()
  await seedAgendaCard(agentId, companyId)
  const computerId = `comp-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO computers (id, company_id, name, kind, available_engines, status) VALUES ($1,$2,'MacBook','local','["claude"]'::jsonb,'online')`,
    [computerId, companyId],
  )
  await pool.query(`UPDATE participants SET computer_id = $1 WHERE id = $2`, [computerId, agentId])

  const savedRoute = env.CEREBELLUM_ROUTE
  const savedEngine = env.CEREBELLUM_LOCAL_ENGINE
  env.CEREBELLUM_ROUTE = 'byoa'
  env.CEREBELLUM_LOCAL_ENGINE = 'claude'
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
    env.CEREBELLUM_ROUTE = savedRoute
    env.CEREBELLUM_LOCAL_ENGINE = savedEngine
  }
})

test('[integration] runtime: GET /agenda for a remote-routed agent (default, no computer) still returns a final decision synchronously', async () => {
  const { __setLlmClientOverrideForTesting } = await import('../llm.js')
  const { agentId, companyId, token } = await seedAgent()
  await seedAgendaCard(agentId, companyId)
  __setLlmClientOverrideForTesting((async () => ({
    responses: { create: async () => ({ output_text: JSON.stringify({ actionable: true, focus: 'Ship it', reason: 'due' }) }) },
  })) as never)
  try {
    const r = await call('/runtime/agenda', { method: 'GET', token })
    assert.equal(r.status, 200)
    assert.equal(r.body.instructions, undefined, 'remote route must not hand back a daemon-classify payload')
    assert.equal(r.body.actionable, true)
    assert.match(String(r.body.brief), /Ship it/)
  } finally {
    __setLlmClientOverrideForTesting(null)
  }
})

test('[integration] runtime: POST /agenda/verdict finalizes a locally-classified actionable verdict into a brief', async () => {
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

test('[integration] runtime: POST /agenda/verdict with actionable=false returns the fail-closed shape', async () => {
  const { token } = await seedAgent()
  const r = await call('/runtime/agenda/verdict', {
    token, body: { actionable: false, reason: 'classifier error' },
  })
  assert.equal(r.status, 200)
  assert.equal(r.body.actionable, false)
  assert.equal(r.body.brief, undefined)
})
