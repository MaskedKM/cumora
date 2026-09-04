/**
 * 验收镜像 · hr 域(#345 HR Agent 骨架)—— 编外隐形人事代理的配置面与
 * 置备/权限/零泄漏不变量(ADR 0007)。
 *
 * 覆盖:GET 兜底置备(存量公司)/CreateCompany 钩子置备(新公司)、
 * owner/admin 读写 vs member 403、部分更新语义(prompt / computer+engine
 * 解析 / 空串清空)、花名册零泄漏(participants/openDirect/createGroup)、
 * 套餐闸不受 hr_agents 行影响(free 满 10 建第 11 个仍拒)。
 */
import { test, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { pool } from './harness/db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror,
} from './_helpers.js'

const USER = 'u-mirror-hr'
const ADMIN = 'u-mirror-hr-adm'
const MEMBER = 'u-mirror-hr-mem'
const COMPANY = 'c-mirror-hr'

async function seedCompanyAndUsers(roleOverride?: string): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'HR Mirror Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
  await seedUserMembership(ADMIN, COMPANY)
  await seedUserMembership(MEMBER, COMPANY)
  await pool.query(`UPDATE company_members SET role = 'admin' WHERE user_id = $1 AND company_id = $2`, [ADMIN, COMPANY])
  await pool.query(`UPDATE company_members SET role = 'member' WHERE user_id = $1 AND company_id = $2`, [MEMBER, COMPANY])
  if (roleOverride) {
    await pool.query(`UPDATE company_members SET role = $1 WHERE user_id = $2 AND company_id = $3`, [roleOverride, USER, COMPANY])
  }
}

async function seedComputer(id: string, engines: string[]): Promise<void> {
  await pool.query(
    `INSERT INTO computers (id, company_id, name, kind, available_engines, status)
     VALUES ($1, $2, $3, 'local', $4::jsonb, 'online')`,
    [id, COMPANY, `box-${id}`, JSON.stringify(engines)],
  )
}

async function seedAgent(id: string): Promise<void> {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', $3, 'A', '#111111', 'resting')
     ON CONFLICT (id, company_id) DO NOTHING`,
    [id, COMPANY, id],
  )
}

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const adminMirror = startMirror(ADMIN, COMPANY)
const memberMirror = startMirror(MEMBER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUsers()
})

after(async () => {
  await mirror.close(); await adminMirror.close(); await memberMirror.close()
  await teardownAll()
})

test('[mirror] hr: GET 兜底置备 — 默认 prompt/归因键/未指派(seed 公司无 hr_agents 行)', async () => {
  // seed 公司直接 INSERT,未经 CreateCompany 钩子与迁移回填 —— GET 必须自兜底
  const res = await call('/hr')
  assert.equal(res.status, 200)
  assert.equal(res.json.agentId, `hr-${COMPANY}`)
  assert.equal(res.json.computerId, null)
  assert.equal(res.json.engine, null)
  assert.ok(typeof res.json.systemPrompt === 'string' && res.json.systemPrompt.length > 0)
  // 恰一行
  const { rows } = await pool.query(`SELECT COUNT(*)::int AS n FROM hr_agents WHERE company_id = $1`, [COMPANY])
  assert.equal(rows[0].n, 1)
})

test('[mirror] hr: 读写闸 — member 403 / admin 200 / owner 200', async () => {
  assert.equal((await memberMirror.call('/hr')).status, 403)
  assert.equal((await memberMirror.call('/hr', { method: 'PUT', body: JSON.stringify({ systemPrompt: 'x' }) })).status, 403)
  assert.equal((await adminMirror.call('/hr')).status, 200)
  assert.equal((await call('/hr')).status, 200)
})

test('[mirror] hr: PUT 部分更新 — prompt 持久 / 空 prompt 拒收 / 空体拒收', async () => {
  const put = await call('/hr', { method: 'PUT', body: JSON.stringify({ systemPrompt: 'Be a fair judge.' }) })
  assert.equal(put.status, 200)
  assert.equal(put.json.systemPrompt, 'Be a fair judge.')
  assert.equal((await call('/hr')).json.systemPrompt, 'Be a fair judge.')
  assert.equal((await call('/hr', { method: 'PUT', body: JSON.stringify({ systemPrompt: '   ' }) })).status, 400)
  assert.equal((await call('/hr', { method: 'PUT', body: JSON.stringify({}) })).status, 400)
})

test('[mirror] hr: 指派 — 合法 computer+engine 落库 / 未 advertised 引擎回退首项 / 异机 400 / 空串清空', async () => {
  await seedComputer('cpu-hr-a', ['claude', 'zcode'])
  await seedComputer('cpu-hr-other', ['codex'])

  // 合法指派
  const put = await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-a', engine: 'zcode' }) })
  assert.equal(put.status, 200)
  assert.equal(put.json.computerId, 'cpu-hr-a')
  assert.equal(put.json.engine, 'zcode')

  // 换机不带 engine → 回退新机 advertised 首项
  const put2 = await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-other' }) })
  assert.equal(put2.json.computerId, 'cpu-hr-other')
  assert.equal(put2.json.engine, 'codex')

  // 只换 engine(现行机上校验)
  const put3 = await call('/hr', { method: 'PUT', body: JSON.stringify({ engine: 'codex' }) })
  assert.equal(put3.status, 200)
  assert.equal(put3.json.engine, 'codex')

  // 异司/不存在 computer → 400
  const bad = await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-nope' }) })
  assert.equal(bad.status, 400)

  // 谓词排列:他司机器 / 已吊销 / cloud 形态,一律拒收
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ('c-hr-other', 'Other Co', 'c-hr-other', $1)`, [USER],
  )
  await pool.query(
    `INSERT INTO computers (id, company_id, name, kind, available_engines, status)
     VALUES ('cpu-hr-foreign', 'c-hr-other', 'foreign box', 'local', '["claude"]'::jsonb, 'online'),
            ('cpu-hr-revoked', $1, 'revoked box', 'local', '["claude"]'::jsonb, 'online'),
            ('cpu-hr-cloud', $1, 'cloud box', 'cloud', '["claude"]'::jsonb, 'online')`,
    [COMPANY],
  )
  await pool.query(`UPDATE computers SET revoked_at = NOW() WHERE id = 'cpu-hr-revoked'`)
  for (const cid of ['cpu-hr-foreign', 'cpu-hr-revoked', 'cpu-hr-cloud']) {
    const res = await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: cid }) })
    assert.equal(res.status, 400, `${cid} must be rejected`)
  }

  // 空串 = 清空指派(computer+engine 一并)
  const clear = await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: '' }) })
  assert.equal(clear.status, 200)
  assert.equal(clear.json.computerId, null)
  assert.equal(clear.json.engine, null)

  // 清空后只给 engine → 400(无现行机)
  assert.equal((await call('/hr', { method: 'PUT', body: JSON.stringify({ engine: 'claude' }) })).status, 400)
})

test('[mirror] hr: 花名册零泄漏 — participants/openDirect/createGroup 均不见 hr', async () => {
  await call('/hr') // 先置备
  await seedAgent('ag-leak-1')

  // 名册只有真 agent + 人,无 hr-*
  const roster = await call('/participants')
  assert.equal(roster.status, 200)
  const ids: string[] = roster.json.map((p: { id: string }) => p.id)
  assert.ok(!ids.some((id) => id.startsWith('hr-')), 'participants must not leak the HR entity')

  // 不能与 HR 开 DM / 拉它入群(它不是 participant)
  const dm = await call('/conversations/direct', { method: 'POST', body: JSON.stringify({ otherId: `hr-${COMPANY}` }) })
  assert.ok(dm.status >= 400, `openDirect to HR must fail, got ${dm.status}`)
  const grp = await call('/conversations', {
    method: 'POST',
    body: JSON.stringify({ title: 'try hr', members: [`hr-${COMPANY}`] }),
  })
  assert.ok(grp.status >= 400, `createGroup with HR must fail, got ${grp.status}`)
})

test('[mirror] hr: 套餐闸不受影响 — free 满 10 建第 11 个仍拒,hr 行在也不占名额', async () => {
  for (let i = 1; i <= 10; i++) await seedAgent(`ag-quota-${i}`)
  await call('/hr') // hr_agents 行存在
  const created = await call('/agents', {
    method: 'POST',
    body: JSON.stringify({ name: 'Number 11', systemPrompt: 'should be rejected by tier gate' }),
  })
  assert.equal(created.status, 403)
  // 计数只看 participants:hr 行在,名册仍是 10 agent(+owner 人类行)
  const { rows } = await pool.query(
    `SELECT COUNT(*)::int AS n FROM participants WHERE company_id = $1 AND kind = 'agent'`, [COMPANY],
  )
  assert.equal(rows[0].n, 10)
})

test('[mirror] hr: CreateCompany 钩子 — 新公司建即置备(不经 GET 兜底)', async () => {
  const created = await call('/companies', { method: 'POST', body: JSON.stringify({ name: 'HR Provision Co' }) })
  assert.equal(created.status, 201)
  const newCo = created.json.id as string
  // 钩子路径应已落行(直接查库证明是钩子而非 GET 兜底)
  const { rows } = await pool.query(`SELECT COUNT(*)::int AS n FROM hr_agents WHERE company_id = $1`, [newCo])
  assert.equal(rows[0].n, 1)
  const m2 = startMirror(USER, newCo)
  const got = await m2.call('/hr')
  assert.equal(got.status, 200)
  assert.equal(got.json.agentId, `hr-${newCo}`)
  await m2.close()
})

test('[mirror] hr: 归因键防撞 — 取名撞 hr-<companyId> 的 agent 改用后缀 id', async () => {
  // COMPANY=c-mirror-hr ⇒ 归因键 hr-c-mirror-hr;slug("hr c-mirror-hr") 恰等于它
  const created = await call('/agents', {
    method: 'POST',
    body: JSON.stringify({ name: 'hr c-mirror-hr', systemPrompt: 'must not steal the attribution key' }),
  })
  assert.equal(created.status, 201)
  // 精确撞形被跳过 → 落到带后缀的候选(仍带 hr- 前缀,但不再等于任何归因键)
  assert.notEqual(created.json.id, `hr-${COMPANY}`)
  assert.match(created.json.id as string, /^hr-c-mirror-hr-/)
  // 普通带 hr 前缀的名字不受影响("HR Assistant" → hr-assistant 是合法 id)
  const normal = await call('/agents', {
    method: 'POST',
    body: JSON.stringify({ name: 'HR Assistant', systemPrompt: 'plain hire' }),
  })
  assert.equal(normal.status, 201)
  assert.equal(normal.json.id, 'hr-assistant')
})
