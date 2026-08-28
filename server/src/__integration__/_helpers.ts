/**
 * Helpers shared by integration tests. Imported by every *.test.ts in
 * this directory.
 *
 * #70 TS 退役后本套件是 MIRROR-only:全部请求打向一个外部 Go 服务
 * (CUMORA_MIRROR_BASE,由 server/run-integration-tests.mjs 自建自起),
 * 本文件只保留种行(TRUNCATE/seed)与请求面(call)——不再有 in-process
 * TS app 形态。schema 由 Go 服启动迁移(0001_baseline.sql)保证。
 *
 * Lifecycle: each test file is a separate `node:test` invocation, so the
 * module-load side effects in env.ts / pool.ts / redis.ts run once per
 * file. The runner has already swapped DATABASE_URL to
 * INTEGRATION_DATABASE_URL before spawning, so the pool here lands on
 * the test DB.
 *
 * Isolation strategy: TRUNCATE between tests rather than transaction
 * rollback. Rollback would break SKIP LOCKED tests (the retry worker
 * uses its own connection / transaction lifecycle that we must not
 * subsume).
 */
import { createHmac, randomUUID } from 'node:crypto'
import { pool } from '../db/pool.js'
import { env } from '../env.js'

/** 双跑时代留下的形态开关,如今是硬前提:没有 Go 服就没有 SUT。
 *  缺失时在 startMirror() 抛出可诊断错误(而非模块加载即炸,那会让
 *  --test 的文件级聚合输出变成一片红 import 错)。 */
export const MIRROR_BASE = process.env.CUMORA_MIRROR_BASE ?? ''

let schemaReady: Promise<void> | null = null

/** 等待 Go 服的 schema 就位(其启动时应用 0001_baseline.sql)。幂等;
 *  最多等 15s——直接单跑某个测试文件而忘了先起 Go 服时,这里给出
 *  可诊断的报错而不是一串 relation does not exist。 */
export function ensureSchemaOnce(): Promise<void> {
  if (!schemaReady) {
    schemaReady = (async () => {
      const deadline = Date.now() + 15_000
      while (Date.now() < deadline) {
        const { rows } = await pool.query<{ ok: boolean }>(
          `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'companies') AS ok`,
        ).catch(() => ({ rows: [{ ok: false }] }))
        if (rows[0]?.ok) return
        await new Promise((r) => setTimeout(r, 500))
      }
      throw new Error(
        'test schema not present after 15s — boot the Go server first ' +
        '(server/run-integration-tests.mjs does this for you; it applies 0001_baseline.sql on boot)',
      )
    })()
  }
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

/** Wipe every test tables. Call from beforeEach. The check at the top
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
 *  cloudflare worker's `hmacHex` exactly so a test payload looks like it
 *  came off the wire. */
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
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status, email)
     VALUES ($1, $2, 'agent', $3, 'tester', $4, '#abcdef', 'avail', $5)
     ON CONFLICT DO NOTHING`,
    [agentId, companyId, `Agent ${agentId}`, agentId.slice(0, 1).toUpperCase(), agentEmail],
  )
  return { companyId, agentId, agentEmail }
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
  // lazy-mints `<userId>.<slug>@<EMAIL_DOMAIN>` (matches the production path).
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

/** 镜像测试的公共请求面(#49,MIRROR-only 化于 #70)。
 *
 *  前提:CUMORA_MIRROR_BASE 指向一个与测试共享同一 Postgres/Redis 的
 *  Go 服务(CUMORA_GO_FAKE_AUTH=1 起动,信任 x-test-user 头——等价于
 *  旧 in-process 形态的伪造 auth 盖章)。beforeEach 仍经 TS pool 种行/
 *  TRUNCATE;请求带 content-type+x-company-id+x-test-user。401 路径
 *  不可测(fake-auth 从不拒绝)——这是有意的形态取舍。 */
export function startMirror(user: string, company: string): {
  call: (path: string, init?: RequestInit) => Promise<{ status: number; json: any }>
  baseUrl: () => string
  /** 兼容旧签名:MIRROR-only 形态下无事可关(harness 不再起 server)。 */
  close: () => Promise<void>
} {
  if (!MIRROR_BASE) {
    throw new Error(
      'CUMORA_MIRROR_BASE is not set — the mirror suite is MIRROR-only since the TS retirement. ' +
      'Run via `npm run test:integration` (server/run-integration-tests.mjs boots the Go server for you).',
    )
  }
  return {
    baseUrl: () => MIRROR_BASE,
    call: (path: string, init?: RequestInit) => (async () => {
      const res = await fetch(`${MIRROR_BASE}/api${path}`, {
        ...init,
        headers: {
          'content-type': 'application/json',
          'x-company-id': company,
          'x-test-user': user,
          ...(init?.headers ?? {}),
        },
      })
      return { status: res.status, json: await res.json().catch(() => null) }
    })(),
    close: async () => { /* no in-process server since the TS retirement */ },
  }
}
