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
import { signAgentToken } from './harness/agents/runtime/jwt.js'
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

/* ───────── #346 评估全链(直铸 hr JWT 模拟 daemon 侧) ───────── */

const HR_AGENT_ID = `hr-${COMPANY}`

function hrToken(agentId = HR_AGENT_ID, companyId: string | null = COMPANY): string {
  return signAgentToken({ agentId, companyId })
}

async function hrCli(argv: string[], token = hrToken()): Promise<{ status: number; json: any }> {
  // runtime 面挂 /runtime/*(daemon 直连,无 /api 前缀)
  const res = await fetch(`${mirror.baseUrl()}/runtime/cli`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify({ argv }),
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

/** 收集窗口内全部 SSE 帧;deadline 后返回(不断言;mirror-scheduler 同款)。 */
async function collectSSE(url: string, token: string, ms: number): Promise<string[]> {
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), ms)
  const frames: string[] = []
  try {
    const res = await fetch(url, {
      headers: { authorization: `Bearer ${token}`, accept: 'text/event-stream' },
      signal: ctrl.signal,
    })
    assert.equal(res.status, 200)
    const reader = (res.body as any).getReader() as { read(): Promise<{ done: boolean; value?: Uint8Array }> }
    const decoder = new TextDecoder()
    let buf = ''
    try {
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        buf += decoder.decode(value, { stream: true })
        for (;;) {
          const idx = buf.indexOf('\n\n')
          if (idx < 0) break
          frames.push(buf.slice(0, idx))
          buf = buf.slice(idx + 2)
        }
      }
    } catch {
      // 窗口到点 abort —— 已收帧照常返回
    }
  } finally {
    clearTimeout(timer)
    ctrl.abort()
  }
  return frames
}

/** 保持一条 wake-stream 订阅(评估触发要求接收者>0,否则 503 收轮)。
 * 确定性等订阅建好:拿到服务端连上即发的 ready/ping 首帧才返回(裸 sleep
 * 赌时序会抖 —— 503!==201 的来源)。 */
async function holdWake(): Promise<() => void> {
  const ac = new AbortController()
  const res = await fetch(`${mirror.baseUrl()}/runtime/wake-stream`, {
    headers: { authorization: `Bearer ${hrToken()}`, accept: 'text/event-stream' },
    signal: ac.signal,
  })
  assert.equal(res.status, 200)
  const reader = (res.body as any).getReader() as { read(): Promise<{ done: boolean; value?: Uint8Array }> }
  await reader.read() // 首帧(ready/ping)= Redis 通道已订阅
  void (async () => {
    try { for (;;) { const { done } = await reader.read(); if (done) break } } catch { /* aborted */ }
  })()
  return () => ac.abort()
}

test('[mirror] hr: 评估全链 — 触发→wake 携 brief→CLI 拉输入/交报告→读面', async () => {
  await seedComputer('cpu-hr-eval', ['claude'])
  await seedAgent('ag-eval-1')
  // 观测面种子:runs/llm 各一行 —— 咬住聚合查询非零(评审 P0:列名错曾
  // 让 runs 路静默恒零)
  await pool.query(
    `INSERT INTO agent_runs (id, agent_id, company_id, status, token_count, started_at)
     VALUES ('run-eval-1', 'ag-eval-1', $1, 'completed', 1200, NOW())`, [COMPANY],
  )
  await pool.query(
    `INSERT INTO llm_calls (id, company_id, agent_id, purpose, model, cost_usd)
     VALUES ('llm-eval-1', $1, 'ag-eval-1', 'agent-turn', 'test-model', 0.01)`, [COMPANY],
  )
  const assigned = await call('/hr', {
    method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-eval', engine: 'claude' }),
  })
  assert.equal(assigned.status, 200)

  // 先订阅 wake-stream,再触发(订阅建好后 300ms 触发,窗口 3s)
  const framesPromise = collectSSE(`${mirror.baseUrl()}/runtime/wake-stream`, hrToken(), 3000)
  await new Promise((r) => setTimeout(r, 300))
  const created = await call('/hr/evaluations', { method: 'POST', body: '{}' })
  assert.equal(created.status, 201)
  assert.equal(created.json.status, 'pending')
  const evalId = created.json.id as string

  const frames = await framesPromise
  const wakeFrame = frames.find((f) => f.includes('event: wake') && f.includes('hr-eval'))
  assert.ok(wakeFrame, `wake frame with hr-eval expected, got: ${frames.join('||').slice(0, 300)}`)
  assert.ok(wakeFrame.includes(HR_AGENT_ID) || wakeFrame.includes('hr-eval'))
  const briefJSON = wakeFrame.slice(wakeFrame.indexOf('data:'))
  assert.ok(briefJSON.includes('backgroundBrief'), 'wake payload carries backgroundBrief')
  assert.ok(briefJSON.includes(evalId), 'brief carries the evaluation ref')

  // CLI:拉输入快照(目标含种子 agent;runs/llm 聚合非零 = 聚合查询咬合)
  const ctx = await hrCli(['hr', 'context', evalId])
  assert.equal(ctx.status, 200)
  assert.equal(ctx.json.ok, true)
  const snapshot = JSON.parse(ctx.json.text)
  const lane = snapshot.targets.find((t: any) => t.agentId === 'ag-eval-1')
  assert.ok(lane, 'seeded agent is in the snapshot targets')
  assert.equal(lane.runs.total, 1)
  assert.equal(lane.runs.tokens, 1200)
  assert.equal(lane.llm.calls, 1)

  // CLI:交报告 → done
  const report = {
    rounds: [{ agentId: 'ag-eval-1', score: 4, findings: ['solid delivery'], suggestion: 'keep' }],
  }
  const submitted = await hrCli(['hr', 'report', evalId, JSON.stringify(report)])
  assert.equal(submitted.status, 200)
  assert.equal(submitted.json.ok, true)
  assert.match(submitted.json.text, /recorded.*done/)

  // 读面:owner 列表+详情可见;member 403
  const list = await call('/hr/evaluations')
  assert.equal(list.status, 200)
  assert.equal(list.json.rows[0].id, evalId)
  assert.equal(list.json.rows[0].status, 'done')
  assert.equal((await memberMirror.call('/hr/evaluations')).status, 403)
  const detail = await call(`/hr/evaluations/${evalId}`)
  assert.equal(detail.status, 200)
  assert.deepEqual(detail.json.payload?.rounds?.[0]?.agentId, 'ag-eval-1')
  assert.ok(detail.json.inputSnapshot, 'detail carries the input snapshot')

  // 收轮后再交 → 拒
  const again = await hrCli(['hr', 'report', evalId, '{"x":1}'])
  assert.equal(again.json.ok, false)
  assert.match(again.json.text, /not open/)
})

test('[mirror] hr: 评估触发闸 — 未指派 400 / 在飞 409 / member 403 / 未知目标 400', async () => {
  assert.equal((await memberMirror.call('/hr/evaluations', { method: 'POST', body: '{}' })).status, 403)
  // 未指派 computer → 400
  const noComputer = await call('/hr/evaluations', { method: 'POST', body: '{}' })
  assert.equal(noComputer.status, 400)
  await seedComputer('cpu-hr-eval2', ['claude'])
  await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-eval2' }) })
  // 未知目标 → 400
  assert.equal(
    (await call('/hr/evaluations', { method: 'POST', body: JSON.stringify({ targetAgentId: 'nope' }) })).status, 400,
  )
  // 201/409 需要在线接收者(brief 一次性投递,0 接收者走 503 收轮)
  const release = await holdWake()
  // 在飞互斥:第一轮 pending 未收 → 409;failed 形报告收轮后可再触发
  const first = await call('/hr/evaluations', { method: 'POST', body: '{}' })
  assert.equal(first.status, 201)
  const second = await call('/hr/evaluations', { method: 'POST', body: '{}' })
  assert.equal(second.status, 409)
  const fail = await hrCli(['hr', 'report', first.json.id, '{"failed":true,"error":"engine unavailable"}'])
  assert.equal(fail.json.ok, true)
  const detail = await call(`/hr/evaluations/${first.json.id}`)
  assert.equal(detail.json.status, 'failed')
  assert.equal(detail.json.error, 'engine unavailable')
  const third = await call('/hr/evaluations', { method: 'POST', body: '{}' })
  assert.equal(third.status, 201)
  release()
})

test('[mirror] hr: daemon 离线触发 — 503 + 轮次即 failed + 订阅后重触发放行', async () => {
  await seedComputer('cpu-hr-offline', ['claude'])
  await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-offline' }) })
  // 无订阅者:brief 一次性投递即丢 → 503,行直接 failed(不留 30min 悬置锁)
  const res = await call('/hr/evaluations', { method: 'POST', body: '{}' })
  assert.equal(res.status, 503)
  const list = await call('/hr/evaluations')
  assert.equal(list.json.rows[0].status, 'failed')
  assert.match(list.json.rows[0].error, /daemon offline/)
  // 订阅在线后重触发 → 201
  const release = await holdWake()
  assert.equal((await call('/hr/evaluations', { method: 'POST', body: '{}' })).status, 201)
  release()
})

test('[mirror] hr: CLI 身份闸 — 普通 agent / 异司 HR 均不可用', async () => {
  await seedComputer('cpu-hr-eval3', ['claude'])
  await seedAgent('ag-eval-3')
  await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-eval3' }) })
  // 异司 HR 实体真实存在(实体闸按 hr_agents 行校验,行不存在=前缀穿越)
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ('c-hr-other', 'Other Co', 'c-hr-other', $1)`, [USER],
  )
  await pool.query(`INSERT INTO hr_agents (company_id) VALUES ('c-hr-other')`)
  const release = await holdWake()
  const created = await call('/hr/evaluations', { method: 'POST', body: '{}' })
  assert.equal(created.status, 201)
  const evalId = created.json.id as string
  release()

  // 普通 agent 的 runtime JWT → hr 命令保留给 HR 实体
  const agentTok = signAgentToken({ agentId: 'ag-eval-3', companyId: COMPANY })
  const asAgent = await hrCli(['hr', 'context', evalId], agentTok)
  assert.equal(asAgent.json.ok, false)
  assert.match(asAgent.json.text, /reserved for the HR Agent/)

  // 异司 HR → 找不到本轮(行按公司隔离)
  const foreignTok = signAgentToken({ agentId: 'hr-c-hr-other', companyId: 'c-hr-other' })
  const foreign = await hrCli(['hr', 'context', evalId], foreignTok)
  assert.equal(foreign.json.ok, false)
  assert.match(foreign.json.text, /unknown evaluation round/)

  // 坏 JSON / 非对象 → 拒
  const bad = await hrCli(['hr', 'report', evalId, 'not-json'])
  assert.equal(bad.json.ok, false)
  await hrCli(['hr', 'report', evalId, '{"failed":true,"error":"cleanup"}'])
})

/* ───────── #347 评分 CRUD + 三路装配(转录/同侪/评分)───────── */

test('[mirror] hr: 评分 CRUD — owner upsert / 越界与未知 400 / member 403', async () => {
  await seedAgent('ag-rate-1')
  assert.equal((await memberMirror.call('/hr/ratings')).status, 403)
  assert.equal(
    (await memberMirror.call('/hr/ratings/ag-rate-1', { method: 'PUT', body: JSON.stringify({ score: 4 }) })).status, 403,
  )
  for (const bad of [{ score: 0 }, { score: 6 }, {}]) {
    const res = await call('/hr/ratings/ag-rate-1', { method: 'PUT', body: JSON.stringify(bad) })
    assert.equal(res.status, 400, `score ${JSON.stringify(bad)} must 400`)
  }
  assert.equal(
    (await call('/hr/ratings/nope', { method: 'PUT', body: JSON.stringify({ score: 4 }) })).status, 400,
  )
  const put1 = await call('/hr/ratings/ag-rate-1', { method: 'PUT', body: JSON.stringify({ score: 4, comment: 'solid' }) })
  assert.equal(put1.status, 200)
  assert.equal(put1.json.score, 4)
  assert.equal(put1.json.comment, 'solid')
  // upsert = 替换当前评分
  const put2 = await call('/hr/ratings/ag-rate-1', { method: 'PUT', body: JSON.stringify({ score: 2, comment: '' }) })
  assert.equal(put2.json.score, 2)
  const list = await call('/hr/ratings')
  assert.equal(list.status, 200)
  assert.equal(list.json.rows.length, 1)
  assert.equal(list.json.rows[0].score, 2)
  assert.equal(list.json.rows[0].comment, '')
})

test('[mirror] hr: 输入补全 — 转录/同侪/评分三路进快照(非零咬合)', async () => {
  await seedComputer('cpu-hr-enrich', ['claude'])
  await seedAgent('ag-enrich-1')
  await seedAgent('ag-enrich-2')
  await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-enrich', engine: 'claude' }) })
  // 转录:agent 间私聊一条(members jsonb 触发器自动落 conversation_members)
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id)
     VALUES ('cv-enrich', 'direct', '', $1, $2)`,
    [JSON.stringify(['ag-enrich-1', 'ag-enrich-2']), COMPANY],
  )
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
     VALUES ('m-enrich', 'cv-enrich', 'ag-enrich-2', 'text', 'peer says hi', 1, $1)`,
    [COMPANY],
  )
  // 同侪:peer → target 的 affinity/trust
  await pool.query(
    `INSERT INTO agent_climate (agent_id, about_id, company_id, affinity, trust, last_note)
     VALUES ('ag-enrich-2', 'ag-enrich-1', $1, 0.5, 0.7, 'reliable')`,
    [COMPANY],
  )
  // 评分:owner 打 4
  await call('/hr/ratings/ag-enrich-1', { method: 'PUT', body: JSON.stringify({ score: 4, comment: 'steady' }) })

  const release = await holdWake()
  const created = await call('/hr/evaluations', {
    method: 'POST', body: JSON.stringify({ targetAgentId: 'ag-enrich-1' }),
  })
  assert.equal(created.status, 201)
  const ctx = await hrCli(['hr', 'context', created.json.id])
  assert.equal(ctx.json.ok, true)
  const lane = (JSON.parse(ctx.json.text).targets ?? []).find((t: any) => t.agentId === 'ag-enrich-1')
  assert.ok(lane, 'target lane present')
  // 转录路
  assert.ok(Array.isArray(lane.recentMessages) && lane.recentMessages.length >= 1, 'recentMessages non-empty')
  assert.ok(lane.recentMessages.some((m: any) => String(m.body).includes('peer says hi')))
  // 同侪路(towardThem:别人对目标)
  assert.equal(lane.climate.towardThem.length, 1)
  assert.equal(lane.climate.towardThem[0].from, 'ag-enrich-2')
  assert.equal(lane.climate.towardThem[0].trust, 0.7)
  // 评分路
  assert.equal(lane.rating.score, 4)
  assert.equal(lane.rating.comment, 'steady')
  release()
  await hrCli(['hr', 'report', created.json.id, '{"failed":true,"error":"cleanup"}'])
})
