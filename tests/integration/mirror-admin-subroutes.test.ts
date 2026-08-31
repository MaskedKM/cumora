/**
 * 验收镜像 · admin 子面九路由(#124,#117-e):用户管理(列表/详情/
 * PATCH 三态)、等待名单(列表/approve 全链/reject)、快速计数、LLM
 * 观察面(summary 扇出 + 钻取)。requireAdmin 的真实 is_admin 检查在
 * 403 门用例里过真闸。llm_calls_rollup/waitlist 不在公共 wipe 表,
 * 本文件 beforeEach 自清。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror, MIRROR_BASE,
} from './_helpers.js'
import { pool } from './harness/db/pool.js'

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(`DELETE FROM llm_calls_rollup`)
  await pool.query(`DELETE FROM waitlist`)
  await pool.query(`DELETE FROM company_invitations`)
})

after(async () => { await teardownAll() })

async function seedAdmin(): Promise<string> {
  const userId = `u-${randomUUID().slice(0, 8)}`
  const companyId = `c-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Admin Co', $2, $3) ON CONFLICT DO NOTHING`,
    [companyId, companyId, userId],
  )
  await seedUserMembership(userId, companyId)
  await pool.query(`UPDATE users SET is_admin = TRUE WHERE id = $1`, [userId])
  return userId
}

function client(userId: string) {
  const m = startMirror(userId, 'c-admin-sub')
  return {
    call: m.call,
    close: m.close,
  }
}

async function seedPlainUser(id: string, email: string, tier = 'free'): Promise<void> {
  await pool.query(
    `INSERT INTO users (id, email, display_name, email_verified_at, tier)
     VALUES ($1, $2, $3, NOW(), $4)`,
    [id, email, id, tier],
  )
}

/* ───────── 门 ───────── */

test('[mirror-admin] 非 admin 全子面 403', async () => {
  const userId = `u-${randomUUID().slice(0, 8)}`
  const companyId = `c-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Plain Co', $2, $3) ON CONFLICT DO NOTHING`,
    [companyId, companyId, userId],
  )
  await seedUserMembership(userId, companyId)
  const c = client(userId)
  for (const [path, init] of [
    ['/admin/users', undefined],
    ['/admin/stats', undefined],
    ['/admin/waitlist', undefined],
    ['/admin/observability/llm', undefined],
    ['/admin/observability/llm/calls', undefined],
    ['/admin/users/u-x', { method: 'PATCH', body: '{"tier":"pro"}' }],
    ['/admin/waitlist/wl-x/approve', { method: 'POST' }],
  ] as const) {
    const r = await c.call(path, init as any)
    assert.equal(r.status, 403, `${path} 应 403`)
  }
  await c.close()
})

/* ───────── users ───────── */

test('[mirror-admin] users 列表:分页/q 搜索/tier 过滤/gravatar 兜底', async () => {
  const admin = await seedAdmin()
  await seedPlainUser('u-list-1', 'alice@example.com')
  await seedPlainUser('u-list-2', 'bob@example.com', 'pro')
  const c = client(admin)
  const all = await c.call('/admin/users?limit=50')
  assert.equal(all.status, 200)
  assert.equal(all.json.total, 3) // admin 自己 + 两个种子
  assert.ok(all.json.items.length >= 3)
  assert.ok(all.json.items.every((i: any) => typeof i.avatarUrl === 'string' && i.avatarUrl.startsWith('http')))

  const q = await c.call('/admin/users?q=ALICE')
  assert.equal(q.json.total, 1)
  assert.equal(q.json.items[0].email, 'alice@example.com')
  assert.equal(q.json.items[0].suspended, false)
  assert.equal(q.json.items[0].sub2apiUserId, null)

  const byTier = await c.call('/admin/users?tier=pro')
  assert.equal(byTier.json.total, 1)
  assert.equal(byTier.json.items[0].id, 'u-list-2')

  const page = await c.call('/admin/users?limit=1&offset=1')
  assert.equal(page.json.items.length, 1)
  assert.equal(page.json.limit, 1)
  assert.equal(page.json.offset, 1)
  await c.close()
})

test('[mirror-admin] users 详情:companies + agentCount', async () => {
  const admin = await seedAdmin()
  await seedPlainUser('u-detail-1', 'detail@example.com')
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ('c-det-1', 'Det Co', 'det-co', 'u-detail-1')`,
  )
  await pool.query(`INSERT INTO company_members (company_id, user_id, role) VALUES ('c-det-1', 'u-detail-1', 'owner')`)
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ('a-det-1', 'c-det-1', 'agent', 'Det Agent', 'D', '#111', 'resting'),
            ('a-det-2', 'c-det-1', 'agent', 'Gone Agent', 'G', '#111', 'resting')`,
  )
  await pool.query(`UPDATE participants SET departed_at = NOW() WHERE id = 'a-det-2'`)
  const c = client(admin)
  const r = await c.call('/admin/users/u-detail-1')
  assert.equal(r.status, 200)
  assert.equal(r.json.companies.length, 1)
  assert.equal(r.json.companies[0].id, 'c-det-1')
  assert.equal(r.json.companies[0].agentCount, 1) // 离编 agent 不计
  assert.equal((await c.call('/admin/users/nope')).status, 404)
  await c.close()
})

test('[mirror-admin] users PATCH:tier/admin 位/停用三态', async () => {
  const admin = await seedAdmin()
  await seedPlainUser('u-patch-1', 'patch@example.com')
  const c = client(admin)

  // tier
  assert.equal((await c.call('/admin/users/u-patch-1', { method: 'PATCH', body: '{"tier":"pro"}' })).json.tier, 'pro')
  assert.equal((await c.call('/admin/users/u-patch-1', { method: 'PATCH', body: '{"tier":"bogus"}' })).status, 400)
  assert.equal((await c.call('/admin/users/nope', { method: 'PATCH', body: '{"tier":"pro"}' })).status, 404)

  // admin 位:可以升别人,不能自降
  assert.equal((await c.call('/admin/users/u-patch-1', { method: 'PATCH', body: '{"isAdmin":true}' })).json.isAdmin, true)
  const selfDemote = await c.call(`/admin/users/${admin}`, { method: 'PATCH', body: '{"isAdmin":false}' })
  assert.equal(selfDemote.status, 409)
  assert.equal(selfDemote.json.error, 'cannot demote yourself')

  // 停用:自停 409;停别人 → 状态戳 + session 清空;再停 409
  const selfSuspend = await c.call(`/admin/users/${admin}`, { method: 'PATCH', body: '{"suspended":true,"suspensionReason":"x"}' })
  assert.equal(selfSuspend.status, 409)
  await pool.query(
    `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ('sess-patch-1', 'u-patch-1', NOW() + interval '1 day')`,
  )
  const susp = await c.call('/admin/users/u-patch-1', {
    method: 'PATCH',
    body: '{"suspended":true,"suspensionReason":"  test ban  "}',
  })
  assert.equal(susp.status, 200)
  assert.equal(susp.json.suspended, true)
  assert.equal(susp.json.suspensionReason, 'test ban')
  const sess = await pool.query(`SELECT 1 FROM sessions WHERE user_id = 'u-patch-1'`)
  assert.equal(sess.rows.length, 0, '停用即清 session')
  const again = await c.call('/admin/users/u-patch-1', { method: 'PATCH', body: '{"suspended":true}' })
  assert.equal(again.status, 409)
  assert.equal(again.json.error, 'user is already suspended')

  // 解停:字段归 null;再解停幂等 200
  const uns = await c.call('/admin/users/u-patch-1', { method: 'PATCH', body: '{"suspended":false}' })
  assert.equal(uns.json.suspended, false)
  assert.equal(uns.json.suspensionReason, null)
  assert.equal((await c.call('/admin/users/u-patch-1', { method: 'PATCH', body: '{"suspended":false}' })).status, 200)
  await c.close()
})

/* ───────── waitlist ───────── */

test('[mirror-admin] waitlist 列表:status/q 过滤 + avatar 兜底', async () => {
  const admin = await seedAdmin()
  await pool.query(
    `INSERT INTO waitlist (id, provider, provider_id, email, display_name)
     VALUES ('wl-1', 'github', 'gh1', 'wait1@example.com', 'Wait One'),
            ('wl-2', 'google', 'g2', 'wait2@example.com', 'Wait Two')`,
  )
  await pool.query(`UPDATE waitlist SET status = 'approved', decided_at = NOW() WHERE id = 'wl-2'`)
  const c = client(admin)
  const pending = await c.call('/admin/waitlist?status=pending')
  assert.equal(pending.status, 200)
  assert.equal(pending.json.total, 1)
  assert.equal(pending.json.items[0].id, 'wl-1')
  assert.ok(pending.json.items[0].avatarUrl.startsWith('https://www.gravatar.com/'))
  assert.equal((await c.call('/admin/waitlist?q=wait2')).json.total, 1)
  assert.equal((await c.call('/admin/waitlist?status=approved')).json.items[0].providerId, 'g2')
  await c.close()
})

test('[mirror-admin] waitlist approve 全链:建号建区/participant/盖章;重复批 409', async () => {
  const admin = await seedAdmin()
  await pool.query(
    `INSERT INTO waitlist (id, provider, provider_id, email, display_name)
     VALUES ('wl-approve-1', 'github', 'gh-app-1', 'NewPerson@example.com', 'New Person')`,
  )
  const c = client(admin)
  const r = await c.call('/admin/waitlist/wl-approve-1/approve', { method: 'POST' })
  assert.equal(r.status, 200)
  const userId = r.json.userId as string
  assert.ok(userId.startsWith('u-'))
  assert.ok(r.json.companyId, '无邀请 → 自建个人区')

  const ident = await pool.query(`SELECT * FROM user_identities WHERE provider = 'github' AND provider_id = 'gh-app-1'`)
  assert.equal(ident.rows[0].user_id, userId)
  assert.equal(ident.rows[0].email_lower, 'newperson@example.com')
  const part = await pool.query(`SELECT * FROM participants WHERE id = $1 AND company_id = $2`, [userId, r.json.companyId])
  assert.equal(part.rows[0].kind, 'human')
  assert.equal(part.rows[0].initial, 'N')
  const wl = await pool.query(`SELECT status, decided_by FROM waitlist WHERE id = 'wl-approve-1'`)
  assert.equal(wl.rows[0].status, 'approved')
  assert.equal(wl.rows[0].decided_by, admin)

  // 再批 → 409 already approved
  const dup = await c.call('/admin/waitlist/wl-approve-1/approve', { method: 'POST' })
  assert.equal(dup.status, 409)
  assert.match(dup.json.error, /already approved/)
  await c.close()
})

test('[mirror-admin] waitlist approve:同 email 已有号 → 409;有活跃邀请 → 跳个人区', async () => {
  const admin = await seedAdmin()
  await seedPlainUser('u-taken', 'taken@example.com')
  await pool.query(
    `INSERT INTO waitlist (id, provider, provider_id, email, display_name)
     VALUES ('wl-taken-1', 'google', 'g-taken', 'taken@example.com', 'Taken Person'),
            ('wl-inv-1', 'google', 'g-inv', 'invited@example.com', 'Invited Person')`,
  )
  // 给 invited@example.com 一张活跃邀请
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ('c-inv-host', 'Host Co', 'host-co', $1)`,
    [admin],
  )
  await pool.query(
    `INSERT INTO company_invitations (token_hash, company_id, invited_by, email, expires_at, max_uses)
     VALUES ('hash-x', 'c-inv-host', $1, 'invited@example.com', NOW() + interval '1 day', 5)`,
    [admin],
  )
  const c = client(admin)
  const taken = await c.call('/admin/waitlist/wl-taken-1/approve', { method: 'POST' })
  assert.equal(taken.status, 409)
  assert.match(taken.json.error, /already exists/)

  const inv = await c.call('/admin/waitlist/wl-inv-1/approve', { method: 'POST' })
  assert.equal(inv.status, 200)
  assert.equal(inv.json.companyId, null, '待决邀请 → 不自建区')
  const member = await pool.query(`SELECT 1 FROM company_members WHERE user_id = $1`, [inv.json.userId])
  assert.equal(member.rows.length, 0)
  await c.close()
})

test('[mirror-admin] waitlist reject:记 note;非 pending → 404', async () => {
  const admin = await seedAdmin()
  await pool.query(
    `INSERT INTO waitlist (id, provider, provider_id, email, display_name)
     VALUES ('wl-rej-1', 'github', 'gh-rej', 'rej@example.com', 'Rej Person')`,
  )
  const c = client(admin)
  const r = await c.call('/admin/waitlist/wl-rej-1/reject', { method: 'POST', body: '{"note":"not a fit"}' })
  assert.equal(r.status, 200)
  assert.equal(r.json.ok, true)
  const row = await pool.query(`SELECT status, note, decided_by FROM waitlist WHERE id = 'wl-rej-1'`)
  assert.equal(row.rows[0].status, 'rejected')
  assert.equal(row.rows[0].note, 'not a fit')
  assert.equal(row.rows[0].decided_by, admin)
  assert.equal((await c.call('/admin/waitlist/wl-rej-1/reject', { method: 'POST' })).status, 404)
  await c.close()
})

/* ───────── stats ───────── */

test('[mirror-admin] stats:四计数块', async () => {
  const admin = await seedAdmin()
  await seedPlainUser('u-stat-1', 's1@example.com', 'pro')
  await pool.query(
    `INSERT INTO waitlist (id, provider, provider_id, email, display_name)
     VALUES ('wl-stat-1', 'github', 'gh-s', 'sw@example.com', 'SW')`,
  )
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ('a-stat-1', (SELECT company_id FROM company_members WHERE user_id = $1 LIMIT 1), 'agent', 'SA', 'S', '#111', 'resting')`,
    [admin],
  )
  const c = client(admin)
  const r = await c.call('/admin/stats')
  assert.equal(r.status, 200)
  assert.equal(r.json.users.total, 2)
  assert.equal(r.json.users.admins, 1)
  assert.equal(r.json.users.tiers.pro, 1)
  assert.equal(r.json.waitlist.pending, 1)
  assert.equal(r.json.agents, 1)
  assert.ok(r.json.companies >= 1)
  await c.close()
})

/* ───────── observability ───────── */

async function seedRollup(): Promise<void> {
  const now = new Date()
  await pool.query(
    `INSERT INTO llm_calls_rollup
       (bucket_hour, company_id, agent_id, purpose, model, source, daemon_version,
        calls, ok_calls, failed_calls, rate_limited_calls,
        input_tokens, cached_input_tokens, output_tokens, cost_usd, cost_estimated)
     VALUES
       ($1, 'c-obs-1', 'a-obs-1', 'agent_turn', 'gpt-5.4-mini', 'cloud', 'v0.2.2', 10, 9, 1, 0, 1000, 500, 200, 0.5, false),
       ($1, 'c-obs-1', 'a-obs-1', 'triage', 'gpt-5.4', 'cloud', 'v0.2.1', 2, 2, 0, 0, 100, 0, 50, 0.3, false),
       ($1, NULL, NULL, 'avatar', 'img-x', 'cloud', NULL, 1, 1, 0, 0, 0, 0, 0, 0.01, true)`,
    [now],
  )
}

test('[mirror-admin] observability/llm:summary 扇出 + model 过滤 + fresh', async () => {
  const admin = await seedAdmin()
  await seedRollup()
  const c = client(admin)
  const r = await c.call('/admin/observability/llm')
  assert.equal(r.status, 200)
  const s = r.json.summary
  assert.equal(s.totalCalls, 13)
  assert.ok(Math.abs(s.totalCostUsd - 0.81) < 1e-9, `浮点和 ${s.totalCostUsd}`)
  assert.equal(s.activeTenants, 1)
  assert.equal(s.failureRate > 0, true)
  assert.equal(s.topPurpose.purpose, 'agent_turn')
  assert.equal(r.json.rollup.length, 3)
  const agentTurn = r.json.rollup.find((x: any) => x.purpose === 'agent_turn')
  assert.ok(agentTurn.savableUsd > 0, '未命中输入 × 价差 > 0')
  assert.equal(r.json.trend.length, 3) // agent_turn + triage + avatar 三 purpose
  assert.equal(r.json.tenants.length, 1)
  assert.equal(r.json.tenants[0].companyId, 'c-obs-1')
  assert.equal(r.json.topAgents.length >= 1, true)
  assert.equal(r.json.daemonVersions.length, 2) // v0.2.2 + v0.2.1

  // model 子串过滤:rollup/trend 缩,summary KPI 仍全局
  const m = await c.call('/admin/observability/llm?model=mini')
  assert.equal(m.json.rollup.length, 1)
  assert.equal(m.json.rollup[0].model, 'gpt-5.4-mini')
  assert.equal(m.json.summary.totalCalls, 13)
  assert.equal(m.json.summary.topPurpose.purpose, 'agent_turn')
  await c.close()
})

test('[mirror-admin] observability/llm/calls:桶钻取 + 排序', async () => {
  const admin = await seedAdmin()
  await pool.query(
    `INSERT INTO llm_calls
       (id, company_id, agent_id, purpose, source, model, input_tokens, output_tokens,
        cost_usd, latency_ms, status, extras, daemon_version)
     VALUES
       ('lc-1', 'c-obs-1', 'a-obs-1', 'agent_turn', 'cloud', 'gpt-5.4-mini', 100, 10, 0.2, 800, 'ok', '{"hopIndex":2}', 'v0.2.2'),
       ('lc-2', 'c-obs-1', 'a-obs-1', 'agent_turn', 'cloud', 'gpt-5.4-mini', 100, 10, 0.5, 300, 'ok', '{"hopIndex":1}', 'v0.2.2'),
       ('lc-3', 'c-obs-1', 'a-obs-1', 'triage', 'cloud', 'gpt-5.4', 50, 5, 0.1, NULL, 'error', NULL, NULL)`,
  )
  const c = client(admin)
  const bucket = await c.call('/admin/observability/llm/calls?purpose=agent_turn')
  assert.equal(bucket.status, 200)
  assert.equal(bucket.json.length, 2)
  assert.equal(bucket.json[0].id, 'lc-2', '默认 cost 倒序')
  assert.equal(bucket.json[0].agentName, null, '无 participants 行 → null')
  assert.equal(bucket.json[0].extras.hopIndex, 1)

  const hop = await c.call('/admin/observability/llm/calls?sortBy=hop')
  assert.equal(hop.json[0].extras.hopIndex, 1)
  assert.equal(hop.json[1].extras.hopIndex, 2)

  const run = await c.call('/admin/observability/llm/calls?agentId=a-obs-1')
  assert.equal(run.json.length, 3)
  assert.equal(run.json[0].createdAt <= run.json[2].createdAt, true, 'run/agent 路径时间正序')
  assert.equal((await c.call('/admin/observability/llm/calls?model=nope')).json.length, 0)
  await c.close()
})
