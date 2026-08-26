/**
 * Helpers shared by integration tests. Imported by every *.test.ts in
 * this directory.
 *
 * Lifecycle: each test file is a separate `node:test` invocation, so the
 * module-load side effects in env.ts / pool.ts / redis.ts run once per
 * file. The runner (server/run-integration-tests.mjs) has already
 * swapped DATABASE_URL to INTEGRATION_DATABASE_URL before spawning, so
 * the pool here lands on the test DB.
 *
 * Isolation strategy: TRUNCATE between tests rather than transaction
 * rollback. Rollback would break SKIP LOCKED tests (the retry worker
 * uses its own connection / transaction lifecycle that we must not
 * subsume).
 */
import { createHmac, randomUUID } from 'node:crypto'
import { ensureSchema } from '../db/migrate.js'
import { pool } from '../db/pool.js'
import { env } from '../env.js'

let schemaReady: Promise<void> | null = null

/** Run the schema migrator exactly once per test process. Idempotent —
 *  ensureSchema is itself `IF NOT EXISTS` throughout. */
export function ensureSchemaOnce(): Promise<void> {
  if (!schemaReady) schemaReady = ensureSchema()
  return schemaReady
}

/** Tables we wipe between tests. Order matters when there are FK
 *  constraints; CASCADE on the parents handles it but listing explicitly
 *  keeps the intent visible + lets us spot-check leakage. */
const TABLES_TO_WIPE: readonly string[] = [
  'user_preferences',
  'agent_autonomy',
  'shipping_events',
  'shipping_regressions',
  'shipping_friction_reports',
  'shipping_releases',
  'computers',
  'shipping_verifications',
  'shipping_invariants',
  'shipping_features',
  'document_mentions',
  'document_snapshots',
  'document_updates',
  'documents',
  'board_mention_reads',
  'board_card_comments',
  'board_cards',
  'board_columns',
  'boards',
  'calendar_reminders',
  'calendar_dispatches',
  'calendar_events',
  'email_attachments',
  'email_messages',
  'email_contacts',
  'message_reactions',
  'conversation_reads',
  'conversation_mutes',
  'conversation_counters',
  'messages',
  'conversations',
  'agent_climate',
  'agent_workspace',
  'agent_runs',
  'agent_events',
  'agent_triages',
  'llm_calls',
  'agent_tasks',
  'agent_log',
  'workspace_associations',
  'workspace_members',
  'workspaces',
  'projects',
  'company_members',
  'participants',
  'users',
  'companies',
  'app_settings',
]

/** Wipe every test table. Call from beforeEach. The check at the top
 *  refuses to run if DATABASE_URL doesn't include the substring "test"
 *  — last line of defense against a misconfigured runner pointing at a
 *  real DB. */
export async function resetAllTables(): Promise<void> {
  if (!/test/i.test(env.DATABASE_URL)) {
    throw new Error(`refusing to TRUNCATE — DATABASE_URL doesn't look like a test DB: ${env.DATABASE_URL}`)
  }
  await ensureSchemaOnce()
  for (const t of TABLES_TO_WIPE) {
    await pool.query(`TRUNCATE TABLE ${t} CASCADE`).catch(() => { /* table may not exist on partial schemas */ })
  }
}

/** Compute the HMAC signature the inbound webhook expects. Mirrors the
 *  cloudflare worker's `hmacHex` exactly so a test payload looks like
 *  it came off the wire. */
export function signInboundPayload(body: string): string {
  const secret = env.EMAIL_INBOUND_HMAC_SECRET
  if (!secret) throw new Error('EMAIL_INBOUND_HMAC_SECRET not set in test env')
  return `sha256=${createHmac('sha256', secret).update(body).digest('hex')}`
}

/** Insert the minimum scaffolding an email row needs: one company + one
 *  agent participant whose participants.email is pre-minted. Returns the
 *  ids the caller will use as recipient / sender. */
export async function seedCompanyWithAgent(opts?: {
  companyId?: string; agentId?: string; agentEmail?: string
}): Promise<{ companyId: string; agentId: string; agentEmail: string }> {
  const companyId = opts?.companyId ?? `c-${randomUUID().slice(0, 8)}`
  const agentId = opts?.agentId ?? `a-${randomUUID().slice(0, 8)}`
  const dom = env.EMAIL_DOMAIN || 'cumora.local'
  const agentEmail = opts?.agentEmail ?? `${agentId}.${companyId}@${dom}`
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id)
     VALUES ($1, $2, $3, $4)
     ON CONFLICT DO NOTHING`,
    [companyId, `Test ${companyId}`, companyId, 'test-owner'],
  )
  // participants composite PK is (id, company_id) — see migrate.ts.
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status, email)
     VALUES ($1, $2, 'agent', $3, 'tester', $4, '#abcdef', 'avail', $5)
     ON CONFLICT DO NOTHING`,
    [agentId, companyId, `Agent ${agentId}`, agentId.slice(0, 1).toUpperCase(), agentEmail],
  )
  return { companyId, agentId, agentEmail }
}

/** Build a minimum-viable Express app that mounts only the routes under
 *  test. Avoids booting the full server (auth middleware, schedulers,
 *  etc.) — slow, more failure modes. */
export async function buildTestApp(): Promise<import('express').Express> {
  const expressMod = await import('express')
  const express = expressMod.default
  const app = express()
  const { inboundEmailRouter } = await import('../api/inbound-email.js')
  // Match the production mount path: index.ts mounts inboundEmailRouter
  // at /webhooks/email — see server/src/index.ts.
  app.use('/webhooks/email', inboundEmailRouter)
  return app
}

/** 验收镜像的统一请求面(#49)。
 *
 * 双跑前提:设 CUMORA_MIRROR_BASE 即把整套镜像指向 Go 候选——但目标必须
 * 共享同一测试 Postgres 与 Redis(beforeEach 仍经 TS pool 种行/TRUNCATE);
 * 请求只带 content-type+x-company-id,无 Authorization——伪造 auth 在本
 * 进程 app 内盖章,候选实现需在 dev 模式复刻同一约定或配令牌注入;
 * 401 路径在此形态下不可测(盖章中间件从不拒绝)。 */
export const MIRROR_BASE = process.env.CUMORA_MIRROR_BASE ?? ''

/** Full /api router + 伪造 auth 中间件:把每个请求盖章为给定 userId。
 * requireAuth() 只读该字段,handler 无从分辨——代价是 401 路径不可测。 */
export async function buildApiTestApp(userId: string): Promise<import('express').Express> {
  const expressMod = await import('express')
  const express = expressMod.default
  const app = express()
  // 入站邮件门先于通用 JSON 解析挂载(raw body HMAC 捕获依赖其自带
  // parser 的 verify;对齐 index.ts 的挂载顺序)。
  const { inboundEmailRouter } = await import('../api/inbound-email.js')
  app.use('/webhooks/email', inboundEmailRouter)
  app.use(express.json({ limit: '34mb' }))
  // Fake auth middleware: stamp authUserId from the test's choice. Real
  // requireAuth() just reads this field, so handlers can't distinguish.
  app.use((req, _res, next) => {
    (req as unknown as { authUserId: string }).authUserId = userId
    next()
  })
  const { api } = await import('../api/router.js')
  app.use('/api', api)
  return app
}

/** Insert a user + company_members row so requireCompany resolves to the
 *  given tenant. ALSO inserts a corresponding participants row, matching
 *  what production onboarding does — human users get a participants
 *  entry so they can have a minted cumora email, climate signals,
 *  /participants visibility, etc. Without this, ensureParticipantAddress
 *  returns null and email-reply paths 500. */
export async function seedUserMembership(userId: string, companyId: string, opts?: {
  email?: string; displayName?: string;
}): Promise<void> {
  const displayName = opts?.displayName ?? userId
  const authEmail = opts?.email ?? `${userId}@test.local`
  await pool.query(
    `INSERT INTO users (id, email, display_name, tier)
     VALUES ($1, $2, $3, 'free')
     ON CONFLICT (id) DO NOTHING`,
    [userId, authEmail, displayName],
  )
  await pool.query(
    `INSERT INTO company_members (company_id, user_id, role)
     VALUES ($1, $2, 'owner')
     ON CONFLICT DO NOTHING`,
    [companyId, userId],
  )
  // Mirror what production onboarding does: a human is also a participant
  // in the company. We leave participants.email NULL so ensureParticipantAddress
  // lazy-mints `<userId>.<slug>@<EMAIL_DOMAIN>` on first access (matches
  // the production code path).
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'human', $3, 'owner', $4, '#abcdef', 'avail')
     ON CONFLICT DO NOTHING`,
    [userId, companyId, displayName, displayName.slice(0, 1).toUpperCase()],
  )
}

/** Tear down every resource the test harness opened: HTTP server, pg
 *  pool, redis (and the separate sub connection). Call from `after()` in
 *  each test file. Without this, node:test waits 60s+ on dangling event-
 *  loop handles before timing out the whole file. */
export async function teardownAll(server?: import('node:http').Server): Promise<void> {
  if (server && server.listening) {
    await new Promise<void>((resolve) => server.close(() => resolve()))
  }
  // Pool + redis are module-level singletons; ending them is fine because
  // the process is about to exit anyway. Catch swallows reentrant-end
  // errors when multiple test files share the singleton.
  try { await pool.end() } catch { /* ignore */ }
  try {
    const { redis, sub } = await import('../redis.js')
    redis.disconnect()
    sub.disconnect()
  } catch { /* ignore */ }
}

/** 镜像测试的公共脚手架:起 in-process app(或让位于 MIRROR_BASE)、
 * 提供带 x-company-id 的 call()、beforeEach 种公司行。 */
export function startMirror(user: string, company: string): {
  call: (path: string, init?: RequestInit) => Promise<{ status: number; json: any }>
  baseUrl: () => string
  /** after() 里必调:关掉 in-process server(MIRROR_BASE 形态下为 no-op)。 */
  close: () => Promise<void>
} {
  let base = ''
  let server: import('node:http').Server | null = null
  const ready = (async () => {
    const app = await buildApiTestApp(user)
    if (MIRROR_BASE) { base = MIRROR_BASE; return }
    const { createServer } = await import('node:http')
    server = createServer(app)
    await new Promise<void>((resolve) => {
      server!.listen(0, () => {
        const a = server!.address()
        if (a && typeof a === 'object') base = `http://127.0.0.1:${a.port}`
        resolve()
      })
    })
  })()
  return {
    baseUrl: () => base,
    call: (path: string, init?: RequestInit) => (async () => {
      await ready
      // MIRROR 形态:候选 Go 进程须以 CUMORA_GO_FAKE_AUTH=1 启动并信任
      // x-test-user(等价本进程的伪造 auth 盖章);TS in-process 形态下
      // 该头无人消费,带上无害。
      const authHeaders: Record<string, string> = MIRROR_BASE ? { 'x-test-user': user } : {}
      const res = await fetch(`${base}/api${path}`, {
        ...init,
        headers: { 'content-type': 'application/json', 'x-company-id': company, ...authHeaders, ...(init?.headers ?? {}) },
      })
      return { status: res.status, json: await res.json().catch(() => null) }
    })(),
    close: async () => {
      await ready
      if (server?.listening) {
        await new Promise<void>((resolve) => server!.close(() => resolve()))
      }
    },
  }
}
