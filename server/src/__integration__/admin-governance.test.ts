/**
 * Integration tests for the #124 admin governance surface:
 * GET/PATCH /api/admin/users(±detail)、GET /api/admin/waitlist +
 * approve/reject、GET /api/admin/stats、GET /api/admin/observability/llm
 * (±/calls)。对齐 TS 基线 749863e admin-router.ts + admin.ts +
 * agents/llm-ledger.ts;观测台是切换日起的桌面实伤(14 个前端文件消费)。
 *
 * requireAdmin 的 users.is_admin 检查真实生效(不 mock),403 门两侧覆盖;
 * approve 走 core.ApproveWaitlist 全入伙机器(FOR UPDATE/邮箱查重/邀请嗅
 * 探/SAVEPOINT slug 重试/封 session 的 suspend 事务)。
 * resetAllTables 不含 waitlist/llm_calls_rollup,beforeEach 补 TRUNCATE。
 */

import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { after, before, beforeEach, test } from 'node:test'
import { pool } from '../db/pool.js'
import {
  ensureSchemaOnce, MIRROR_BASE,resetAllTables, seedUserMembership, startMirror, teardownAll, 
} from './_helpers.js'

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(`TRUNCATE TABLE waitlist, llm_calls_rollup CASCADE`)
})

after(async () => {
  await teardownAll()
})

type Client = {
  get: (p: string) => Promise<{ status: number; body: any }>
  post: (p: string, b?: unknown) => Promise<{ status: number; body: any }>
  patch: (p: string, b: unknown) => Promise<{ status: number; body: any }>
}

/** 每用例独立身份:fake-auth 盖章(x-test-user)随 user 走,路径不带 /api 前缀。 */
async function withClient<T>(userId: string, fn: (c: Client) => Promise<T>): Promise<T> {
  const m = startMirror(userId, 'c-admin-probe')
  try {
    const wrap = (r: { status: number; json: any }) => ({ status: r.status, body: r.json })
    return await fn({
      get: async (p) => wrap(await m.call(p)),
      post: async (p, b) => wrap(await m.call(p, { method: 'POST', body: JSON.stringify(b ?? {}) })),
      patch: async (p, b) => wrap(await m.call(p, { method: 'PATCH', body: JSON.stringify(b) })),
    })
  } finally {
    await m.close()
  }
}

async function seedCompany(companyId: string): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, 'test-owner') ON CONFLICT DO NOTHING`,
    [companyId, `Test ${companyId}`, companyId],
  )
}

async function seedAdmin(): Promise<{ userId: string; companyId: string; email: string }> {
  const userId = `u-${randomUUID().slice(0, 8)}`
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const email = `${userId}@admin.test`
  await seedCompany(companyId)
  await seedUserMembership(userId, companyId, { email })
  await pool.query(`UPDATE users SET is_admin = TRUE WHERE id = $1`, [userId])
  return { userId, companyId, email }
}

async function seedMember(): Promise<{ userId: string; companyId: string; email: string }> {
  const userId = `u-${randomUUID().slice(0, 8)}`
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const email = `${userId}@member.test`
  await seedCompany(companyId)
  await seedUserMembership(userId, companyId, { email })
  return { userId, companyId, email }
}

async function seedWaitlistRow(overrides: Partial<{
  id: string; email: string; displayName: string; status: string
}> = {}): Promise<string> {
  const id = overrides.id ?? `wl-${randomUUID().slice(0, 12)}`
  await pool.query(
    `INSERT INTO waitlist (id, provider, provider_id, email, display_name)
     VALUES ($1, 'google', $2, $3, $4)`,
    [id, `g-${randomUUID().slice(0, 10)}`,
      overrides.email ?? `${id}@wait.test`, overrides.displayName ?? `Waiter ${id}`],
  )
  if (overrides.status) {
    await pool.query(`UPDATE waitlist SET status = $2 WHERE id = $1`, [id, overrides.status])
  }
  return id
}

/* ============== 门 ============== */

test('[mirror] non-admin gets 403 on users/waitlist/stats/observability', async () => {
  const m = await seedMember()
  await withClient(m.userId, async (c) => {
    for (const p of ['/admin/users', '/admin/waitlist', '/admin/stats', '/admin/observability/llm']) {
      const res = await c.get(p)
      assert.equal(res.status, 403, `${p} should be admin-only`)
      assert.equal(res.body.error, 'admin only')
    }
  })
})

/* ============== users ============== */

test('[mirror] GET /admin/users lists, searches, filters and paginates', async () => {
  const admin = await seedAdmin()
  const other = await seedMember()
  await withClient(admin.userId, async (c) => {
    const res = await c.get('/admin/users')
    assert.equal(res.status, 200)
    assert.equal(res.body.total, 2)
    assert.equal(res.body.limit, 50)
    assert.equal(res.body.offset, 0)
    assert.ok(Array.isArray(res.body.items) && res.body.items.length === 2)
    const me = res.body.items.find((u: any) => u.id === admin.userId)
    assert.equal(me.isAdmin, true)
    assert.equal(me.companyCount, 1)
    assert.equal(me.sub2apiUserId, null)
    assert.equal(me.suspended, false)
    assert.ok(me.avatarUrl.startsWith('https://www.gravatar.com/avatar/')) // NULL 头像兜底

    const byEmail = await c.get(`/admin/users?q=${encodeURIComponent(other.email)}`)
    assert.equal(byEmail.status, 200)
    assert.equal(byEmail.body.total, 1)
    assert.equal(byEmail.body.items[0].id, other.userId)

    await pool.query(`UPDATE users SET tier = 'pro' WHERE id = $1`, [other.userId])
    const byTier = await c.get('/admin/users?tier=pro')
    assert.equal(byTier.body.total, 1)
    assert.equal(byTier.body.items[0].id, other.userId)

    const page = await c.get('/admin/users?limit=1&offset=1')
    assert.equal(page.body.total, 2)
    assert.equal(page.body.items.length, 1)
    assert.equal(page.body.limit, 1)
    assert.equal(page.body.offset, 1)
  })
})

test('[mirror] GET /admin/users/:id returns detail with companies and agent counts', async () => {
  const admin = await seedAdmin()
  const other = await seedMember()
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Obs Agent', 'tester', 'O', '#abcdef', 'avail')`,
    [agentId, other.companyId],
  )
  await withClient(admin.userId, async (c) => {
    const res = await c.get(`/admin/users/${other.userId}`)
    assert.equal(res.status, 200)
    assert.equal(res.body.id, other.userId)
    assert.equal(res.body.companies.length, 1)
    assert.equal(res.body.companies[0].id, other.companyId)
    assert.equal(res.body.companies[0].agentCount, 1)

    const missing = await c.get('/admin/users/u-does-not-exist')
    assert.equal(missing.status, 404)
    assert.equal(missing.body.error, 'user not found')
  })
})

test('[mirror] PATCH /admin/users/:id tier gate + change', async () => {
  const admin = await seedAdmin()
  const other = await seedMember()
  await withClient(admin.userId, async (c) => {
    const bad = await c.patch(`/admin/users/${other.userId}`, { tier: 'enterprise' })
    assert.equal(bad.status, 400)
    assert.equal(bad.body.error, 'invalid tier')

    const ok = await c.patch(`/admin/users/${other.userId}`, { tier: 'pro' })
    assert.equal(ok.status, 200)
    assert.equal(ok.body.tier, 'pro')

    const missing = await c.patch('/admin/users/u-does-not-exist', { tier: 'pro' })
    assert.equal(missing.status, 404)
    assert.equal(missing.body.error, 'user not found')
  })
})

test('[mirror] PATCH /admin/users/:id isAdmin: self-demote refused, other flips', async () => {
  const admin = await seedAdmin()
  const other = await seedMember()
  await withClient(admin.userId, async (c) => {
    const self = await c.patch(`/admin/users/${admin.userId}`, { isAdmin: false })
    assert.equal(self.status, 409)
    assert.equal(self.body.error, 'cannot demote yourself')

    const promote = await c.patch(`/admin/users/${other.userId}`, { isAdmin: true })
    assert.equal(promote.status, 200)
    assert.equal(promote.body.isAdmin, true)
    const demote = await c.patch(`/admin/users/${other.userId}`, { isAdmin: false })
    assert.equal(demote.status, 200)
    assert.equal(demote.body.isAdmin, false)
  })
})

test('[mirror] PATCH /admin/users/:id suspend flips state and revokes sessions atomically', async () => {
  const admin = await seedAdmin()
  const other = await seedMember()
  await pool.query(
    `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, NOW() + INTERVAL '1 day')`,
    [`tok-${randomUUID().slice(0, 12)}`, other.userId],
  )
  await withClient(admin.userId, async (c) => {
    const selfSuspend = await c.patch(`/admin/users/${admin.userId}`, { suspended: true })
    assert.equal(selfSuspend.status, 409)
    assert.equal(selfSuspend.body.error, 'cannot suspend yourself')

    const susp = await c.patch(`/admin/users/${other.userId}`, { suspended: true, suspensionReason: '  abuse  ' })
    assert.equal(susp.status, 200)
    assert.equal(susp.body.suspended, true)
    assert.equal(susp.body.suspensionReason, 'abuse')
    assert.ok(susp.body.suspendedAt)

    const { rows: live } = await pool.query(`SELECT COUNT(*)::int AS n FROM sessions WHERE user_id = $1`, [other.userId])
    assert.equal(live[0].n, 0, 'suspend must delete every live session in the same tx')

    const again = await c.patch(`/admin/users/${other.userId}`, { suspended: true })
    assert.equal(again.status, 409)
    assert.equal(again.body.error, 'user is already suspended')

    const unsusp = await c.patch(`/admin/users/${other.userId}`, { suspended: false })
    assert.equal(unsusp.status, 200)
    assert.equal(unsusp.body.suspended, false)
    assert.equal(unsusp.body.suspensionReason, null)

    const unsuspAgain = await c.patch(`/admin/users/${other.userId}`, { suspended: false })
    assert.equal(unsuspAgain.status, 200, 'unsuspend on an active account is a no-op')
  })
})

/* ============== waitlist ============== */

test('[mirror] GET /admin/waitlist lists with mapping, filters and pagination', async () => {
  const admin = await seedAdmin()
  const idA = await seedWaitlistRow({ displayName: 'Alpha Waiter' })
  const idB = await seedWaitlistRow({ status: 'rejected' })
  await withClient(admin.userId, async (c) => {
    const res = await c.get('/admin/waitlist')
    assert.equal(res.status, 200)
    assert.equal(res.body.total, 2)
    const a = res.body.items.find((e: any) => e.id === idA)
    assert.equal(a.provider, 'google')
    assert.ok(a.providerId.startsWith('g-'))
    assert.equal(a.status, 'pending')
    assert.equal(a.note, null)
    assert.ok(a.avatarUrl.startsWith('https://www.gravatar.com/avatar/'))

    const pendingOnly = await c.get('/admin/waitlist?status=pending')
    assert.equal(pendingOnly.body.total, 1)
    assert.equal(pendingOnly.body.items[0].id, idA)

    const search = await c.get(`/admin/waitlist?q=${encodeURIComponent('alpha')}`)
    assert.equal(search.body.total, 1)
    assert.equal(search.body.items[0].id, idA)
    void idB

    const page = await c.get('/admin/waitlist?limit=1&offset=1')
    assert.equal(page.body.total, 2)
    assert.equal(page.body.items.length, 1)
  })
})

test('[mirror] POST /admin/waitlist/:id/approve mints the full user+company stack', async () => {
  const admin = await seedAdmin()
  const wlId = await seedWaitlistRow({ email: `newbie-${randomUUID().slice(0, 6)}@approve.test`, displayName: 'New Bee' })
  await withClient(admin.userId, async (c) => {
    const res = await c.post(`/admin/waitlist/${wlId}/approve`)
    assert.equal(res.status, 200)
    assert.ok(res.body.userId.startsWith('u-'))
    assert.ok(res.body.companyId && res.body.companyId.startsWith('co-'))

    const { rows: users } = await pool.query(`SELECT email, display_name, avatar_url FROM users WHERE id = $1`, [res.body.userId])
    assert.equal(users.length, 1)
    assert.equal(users[0].display_name, 'New Bee')
    assert.ok(users[0].avatar_url.startsWith('https://www.gravatar.com/avatar/'), 'empty provider avatar falls back to gravatar')

    const { rows: cos } = await pool.query(`SELECT owner_user_id FROM companies WHERE id = $1`, [res.body.companyId])
    assert.equal(cos[0].owner_user_id, res.body.userId)

    const { rows: cm } = await pool.query(`SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2`, [res.body.companyId, res.body.userId])
    assert.equal(cm[0].role, 'owner')

    const { rows: part } = await pool.query(`SELECT kind, name FROM participants WHERE id = $1 AND company_id = $2`, [res.body.userId, res.body.companyId])
    assert.equal(part[0].kind, 'human')

    const { rows: wl } = await pool.query(`SELECT status, decided_by FROM waitlist WHERE id = $1`, [wlId])
    assert.equal(wl[0].status, 'approved')
    assert.equal(wl[0].decided_by, admin.userId)

    const again = await c.post(`/admin/waitlist/${wlId}/approve`)
    assert.equal(again.status, 409)
    assert.equal(again.body.error, 'already approved')
  })
})

test('[mirror] approve refuses when the email already has a user', async () => {
  const admin = await seedAdmin()
  const wlId = await seedWaitlistRow({ email: admin.email })
  await withClient(admin.userId, async (c) => {
    const res = await c.post(`/admin/waitlist/${wlId}/approve`)
    assert.equal(res.status, 409)
    assert.ok(res.body.error.includes('already exists'), res.body.error)
  })
})

test('[mirror] approve 404s on an unknown waitlist id; mirror failure keeps the provider avatar', async () => {
  const admin = await seedAdmin()
  await withClient(admin.userId, async (c) => {
    const missing = await c.post('/admin/waitlist/wl-does-not-exist/approve')
    assert.equal(missing.status, 404)
    assert.equal(missing.body.error, 'waitlist entry not found')
  })
  // TS admin.ts mirrorAvatar:一切失败路径回退原 provider URL(非 null,
  // `?? gravatar` 不触发)——不可达地址必须原样保留,不得换 gravatar。
  const email = `avatar-${randomUUID().slice(0, 6)}@approve.test`
  const wlId = await seedWaitlistRow({ email })
  await pool.query(`UPDATE waitlist SET avatar_url = 'http://127.0.0.1:9/unreachable.png' WHERE id = $1`, [wlId])
  await withClient(admin.userId, async (c) => {
    const res = await c.post(`/admin/waitlist/${wlId}/approve`)
    assert.equal(res.status, 200)
    const { rows } = await pool.query(`SELECT avatar_url FROM users WHERE id = $1`, [res.body.userId])
    assert.equal(rows[0].avatar_url, 'http://127.0.0.1:9/unreachable.png')
  })
})

test('[mirror] approve with a pending invitation skips the personal workspace', async () => {
  const admin = await seedAdmin()
  const email = `invited-${randomUUID().slice(0, 6)}@approve.test`
  const wlId = await seedWaitlistRow({ email })
  await pool.query(
    `INSERT INTO company_invitations (token_hash, company_id, invited_by, email, expires_at)
     VALUES ($1, $2, $3, $4, NOW() + INTERVAL '1 day')`,
    [`th-${randomUUID().slice(0, 12)}`, admin.companyId, admin.userId, email],
  )
  await withClient(admin.userId, async (c) => {
    const res = await c.post(`/admin/waitlist/${wlId}/approve`)
    assert.equal(res.status, 200)
    assert.ok(res.body.userId.startsWith('u-'))
    assert.equal(res.body.companyId, null, 'pending invitation → no personal workspace')
  })
})

test('[mirror] POST /admin/waitlist/:id/reject marks rejected with note; non-pending 404s', async () => {
  const admin = await seedAdmin()
  const wlId = await seedWaitlistRow()
  await withClient(admin.userId, async (c) => {
    const res = await c.post(`/admin/waitlist/${wlId}/reject`, { note: 'not a fit' })
    assert.equal(res.status, 200)
    assert.deepEqual(res.body, { ok: true })
    const { rows } = await pool.query(`SELECT status, note FROM waitlist WHERE id = $1`, [wlId])
    assert.equal(rows[0].status, 'rejected')
    assert.equal(rows[0].note, 'not a fit')

    const again = await c.post(`/admin/waitlist/${wlId}/reject`)
    assert.equal(again.status, 404)
    assert.equal(again.body.error, 'no pending waitlist entry')

    const missing = await c.post('/admin/waitlist/wl-does-not-exist/reject')
    assert.equal(missing.status, 404)
  })
})

/* ============== stats ============== */

test('[mirror] GET /admin/stats aggregates users/waitlist/companies/agents', async () => {
  const admin = await seedAdmin()
  const other = await seedMember()
  await pool.query(`UPDATE users SET tier = 'pro' WHERE id = $1`, [other.userId])
  await seedWaitlistRow()
  await seedWaitlistRow({ status: 'rejected' })
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Stat Agent', 'S', '#abcdef', 'avail')`,
    [agentId, admin.companyId],
  )
  await withClient(admin.userId, async (c) => {
    const res = await c.get('/admin/stats')
    assert.equal(res.status, 200)
    assert.deepEqual(res.body.users, {
      total: 2, admins: 1, tiers: { free: 1, pro: 1, max: 0 },
    })
    assert.deepEqual(res.body.waitlist, { pending: 1, approved: 0, rejected: 1 })
    assert.equal(res.body.companies, 2)
    assert.equal(res.body.agents, 1)
  })
})

/* ============== observability ============== */

test('[mirror] GET /admin/observability/llm returns the six-shape payload on an empty ledger', async () => {
  const admin = await seedAdmin()
  await withClient(admin.userId, async (c) => {
    const res = await c.get('/admin/observability/llm')
    assert.equal(res.status, 200)
    const s = res.body.summary
    assert.equal(s.sinceDays, 30)
    assert.equal(s.totalCalls, 0)
    assert.equal(s.totalCostUsd, 0)
    assert.equal(s.failureRate, 0)
    assert.equal(s.activeTenants, 0)
    assert.equal(s.cacheHitRate, null)
    assert.equal(s.topPurpose, null)
    assert.equal(s.savableUsd, 0)
    assert.deepEqual(res.body.rollup, [])
    assert.deepEqual(res.body.trend, [])
    assert.deepEqual(res.body.topAgents, [])
    assert.deepEqual(res.body.tenants, [])
    assert.deepEqual(res.body.daemonVersions, [])
  })
})

async function seedLedger(companyId: string, agentId: string): Promise<void> {
  await seedCompany(companyId)
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Ledger Agent', 'L', '#abcdef', 'avail')`,
    [agentId, companyId],
  )
  await pool.query(
    `INSERT INTO llm_calls_rollup (
       bucket_hour, company_id, agent_id, purpose, model, source, daemon_version,
       calls, ok_calls, failed_calls, rate_limited_calls,
       input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens, reasoning_tokens,
       cost_usd, cost_estimated)
     VALUES
       (date_trunc('hour', NOW()) - INTERVAL '1 hour', $1, $2, 'agent-turn', 'gpt-5.4-mini', 'cloud', NULL,
        8, 7, 1, 0, 10000, 2000, 0, 3000, 0, 0.123, false),
       (date_trunc('hour', NOW()) - INTERVAL '2 hour', $1, NULL, 'palette', 'gpt-5.4', 'byoa-claude', '0.1.9',
        2, 2, 0, 0, 500, 0, 0, 100, 0, 0.01, true)`,
    [companyId, agentId],
  )
  await pool.query(
    `INSERT INTO llm_calls (
       id, company_id, agent_id, run_id, purpose, source, model,
       input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens, reasoning_tokens,
       cost_usd, cost_estimated, measured, latency_ms, status, extras, daemon_version)
     VALUES
       ('llm-obs-1', $1, $2, 'run-obs-1', 'agent-turn', 'cloud', 'gpt-5.4-mini',
        900, 100, 0, 250, 0, 0.05, false, true, 1500, 'ok', '{"hopIndex": 2}'::jsonb, NULL),
       ('llm-obs-2', $1, NULL, NULL, 'compaction', 'cloud', 'gpt-5.4',
        100, 0, 0, 50, 0, 0.01, false, true, 800, 'failed', NULL, NULL)`,
    [companyId, agentId],
  )
}

test('[mirror] GET /admin/observability/llm aggregates the seeded ledger', async () => {
  const admin = await seedAdmin()
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await seedLedger(companyId, agentId)
  await withClient(admin.userId, async (c) => {
    // fresh=1:镜像形态整跑共用一个 Go 服进程,30s 响应缓存里还躺着
    // 空账测试的条目(TS 时代每测新起服,缓存天然隔离)。
    const res = await c.get('/admin/observability/llm?fresh=1')
    assert.equal(res.status, 200)
    const s = res.body.summary
    assert.equal(s.totalCalls, 10)
    assert.equal(s.activeTenants, 1)
    assert.equal(s.rateLimitedCalls, 0)
    assert.ok(Math.abs(s.failureRate - 0.1) < 1e-9, `failureRate ${s.failureRate}`)
    assert.ok(Math.abs(s.totalCostUsd - 0.133) < 1e-9, `totalCostUsd ${s.totalCostUsd}`)
    assert.ok(Math.abs(s.cacheHitRate - 2000 / 12500) < 1e-9, `cacheHitRate ${s.cacheHitRate}`)
    assert.equal(s.topPurpose.purpose, 'agent-turn')

    assert.equal(res.body.rollup.length, 2)
    assert.equal(res.body.rollup[0].purpose, 'agent-turn') // cost DESC
    assert.equal(res.body.rollup[0].okCalls, 7)
    assert.equal(res.body.rollup[0].failedCalls, 1)
    assert.equal(res.body.rollup[0].costEstimated, false)
    assert.equal(typeof res.body.rollup[0].savableUsd, 'number')
    assert.ok(res.body.rollup[0].savableUsd > 0, 'uncached input × rate gap')
    assert.equal(res.body.rollup[1].costEstimated, true) // BOOL_OR

    assert.ok(res.body.trend.length >= 2)
    assert.ok(res.body.trend.every((b: any) => /^\d{4}-\d{2}-\d{2}$/.test(b.day)))

    assert.equal(res.body.tenants.length, 1)
    assert.equal(res.body.tenants[0].companyId, companyId)

    assert.equal(res.body.topAgents.length, 2)
    assert.equal(res.body.topAgents[0].agentId, agentId)

    assert.equal(res.body.daemonVersions.length, 1)
    assert.equal(res.body.daemonVersions[0].daemonVersion, '0.1.9')
    assert.equal(res.body.daemonVersions[0].failureRate, 0)

    // fresh=1 绕过缓存仍回同形状;model 过滤收窄 rollup 不动 summary KPI。
    const filtered = await c.get('/admin/observability/llm?model=mini&fresh=1')
    assert.equal(filtered.status, 200)
    assert.equal(filtered.body.rollup.length, 1)
    assert.equal(filtered.body.rollup[0].model, 'gpt-5.4-mini')
    assert.equal(filtered.body.summary.totalCalls, 10, 'summary KPIs stay global under model filter')

    const scoped = await c.get(`/admin/observability/llm?companyId=${companyId}`)
    assert.equal(scoped.body.summary.totalCalls, 10)
    const wrongCo = await c.get('/admin/observability/llm?companyId=co-none')
    assert.equal(wrongCo.body.summary.totalCalls, 0)
    assert.equal(wrongCo.body.summary.topPurpose, null)
  })
})

test('[mirror] GET /admin/observability/llm/calls drills down with filters and sorts', async () => {
  const admin = await seedAdmin()
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const agentId = `a-${randomUUID().slice(0, 8)}`
  await seedLedger(companyId, agentId)
  await withClient(admin.userId, async (c) => {
    const all = await c.get('/admin/observability/llm/calls')
    assert.equal(all.status, 200)
    assert.equal(all.body.length, 2)
    assert.equal(all.body[0].id, 'llm-obs-1', 'default bucket sort is cost DESC')

    const byPurpose = await c.get('/admin/observability/llm/calls?purpose=agent-turn')
    assert.equal(byPurpose.body.length, 1)
    const row = byPurpose.body[0]
    assert.equal(row.agentName, 'Ledger Agent')
    assert.equal(row.latencyMs, 1500)
    assert.equal(row.extras.hopIndex, 2)
    assert.equal(row.measured, true)

    const byRun = await c.get('/admin/observability/llm/calls?runId=run-obs-1')
    assert.equal(byRun.body.length, 1)
    assert.equal(byRun.body[0].id, 'llm-obs-1')

    const hopSort = await c.get('/admin/observability/llm/calls?sortBy=hop')
    assert.equal(hopSort.body.length, 2)
    assert.equal(hopSort.body[0].id, 'llm-obs-1', 'hopIndex ASC NULLS LAST')

    const none = await c.get('/admin/observability/llm/calls?model=claude-zeta')
    assert.equal(none.body.length, 0)
  })
})
