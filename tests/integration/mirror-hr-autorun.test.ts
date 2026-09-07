/**
 * 验收镜像 · hr 自动运行(#350 刀 6)—— 周期例行 + 事件钩子 + 去抖合并。
 *
 * 驱动方式:集成 SUT 的 worker 已关(run.mjs HR_AUTORUN_INTERVAL_MS=0),
 * 全部经 POST /api/hr/autorun/tick 强制到期端点 + DB 回填/种子(看板卡
 * 停更、llm spend、错误率)注入事件,不等任何真实计时器。
 *
 * 覆盖:配置读写闸与校验、周期到期入队(brief 携 ref)且走同一 CLI
 * 执行/报告面收轮、周期去抖/在飞跳过/未到期不烧、interval=0 关、
 * 三钩子各自命中(卡停更/spend 超阈/错误率)+最小样本闸、同目标去抖
 * 不烧重、每 tick 单轮排队排空、未指派机器跳过。
 */
import { test, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { pool } from './harness/db/pool.js'
import { signAgentToken } from './harness/agents/runtime/jwt.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror,
} from './_helpers.js'

const USER = 'u-mirror-hr-ar'
const ADMIN = 'u-mirror-hr-ar-adm'
const MEMBER = 'u-mirror-hr-ar-mem'
const COMPANY = 'c-mirror-hr-ar'
const HR_AGENT_ID = `hr-${COMPANY}`

async function seedCompanyAndUsers(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'HR Autorun Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
  await seedUserMembership(ADMIN, COMPANY)
  await seedUserMembership(MEMBER, COMPANY)
  await pool.query(`UPDATE company_members SET role = 'admin' WHERE user_id = $1 AND company_id = $2`, [ADMIN, COMPANY])
  await pool.query(`UPDATE company_members SET role = 'member' WHERE user_id = $1 AND company_id = $2`, [MEMBER, COMPANY])
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

/** HR 已指派机器 = 自动轮可入队的前置(指派语义走真端点)。 */
async function assignHrComputer(): Promise<void> {
  await seedComputer('cpu-hr-ar', ['claude'])
  const res = await call('/hr', { method: 'PUT', body: JSON.stringify({ computerId: 'cpu-hr-ar', engine: 'claude' }) })
  assert.equal(res.status, 200)
}

/** 看板卡种子(可回填停更时长):board→column→card 一条链。 */
async function seedOverdueCard(cardId: string, assignee: string, idleDays: number): Promise<void> {
  await pool.query(
    `INSERT INTO boards (id, company_id, title, created_by) VALUES ($1, $2, 'HR board', $3)`,
    [`bd-${cardId}`, COMPANY, USER],
  )
  await pool.query(
    `INSERT INTO board_columns (id, board_id, title) VALUES ($1, $2, 'todo')`,
    [`col-${cardId}`, `bd-${cardId}`],
  )
  await pool.query(
    `INSERT INTO board_cards (id, board_id, column_id, title, created_by, assignee_id, updated_at)
     VALUES ($1, $2, $3, $4, $5, $6, NOW() - ($7 || ' days')::interval)`,
    [cardId, `bd-${cardId}`, `col-${cardId}`, `card ${cardId}`, USER, assignee, String(idleDays)],
  )
}

async function tick(): Promise<{ status: number; json: any }> {
  const res = await call('/hr/autorun/tick', { method: 'POST' })
  return { status: res.status, json: res.json }
}

function hrToken(): string {
  return signAgentToken({ agentId: HR_AGENT_ID, companyId: COMPANY })
}

async function hrCli(argv: string[]): Promise<{ status: number; json: any }> {
  const res = await fetch(`${mirror.baseUrl()}/runtime/cli`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${hrToken()}` },
    body: JSON.stringify({ argv }),
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

/** 收集窗口内全部 SSE 帧;deadline 后返回。 */
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

/** 订阅建好(300ms)后执行触发动作,返回窗口内收到的帧。 */
async function wakeFramesDuring(action: () => Promise<unknown>, ms = 3000): Promise<string[]> {
  const p = collectSSE(`${mirror.baseUrl()}/runtime/wake-stream`, hrToken(), ms)
  await new Promise((r) => setTimeout(r, 300))
  await action()
  return p
}

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const adminMirror = startMirror(ADMIN, COMPANY)
const memberMirror = startMirror(MEMBER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUsers()
  await call('/hr') // 置备 hr_agents 行(GET 兜底,含 0013 默认配置)
})

after(async () => {
  await mirror.close(); await adminMirror.close(); await memberMirror.close()
  await teardownAll()
})

/* ───────── 配置面 ───────── */

test('[autorun] 配置读 — 默认值齐备,nextPeriodicAt=置备+168h', async () => {
  const res = await call('/hr/autorun')
  assert.equal(res.status, 200)
  assert.equal(res.json.intervalHours, 168)
  assert.equal(res.json.overdueDays, 3)
  assert.equal(res.json.spendUsd, 5)
  assert.equal(res.json.errorRate, 0.5)
  assert.ok(res.json.lastPeriodicAt, 'lastPeriodicAt defaults to provisioning time')
  assert.ok(res.json.nextPeriodicAt, 'weekly default → next due is set')
  const deltaH = (new Date(res.json.nextPeriodicAt).getTime() - new Date(res.json.lastPeriodicAt).getTime()) / 3_600_000
  assert.ok(Math.abs(deltaH - 168) < 0.01, `next-last should be 168h, got ${deltaH}`)
})

test('[autorun] 配置写 — 部分更新生效;越界/空体 400;member 403', async () => {
  assert.equal((await memberMirror.call('/hr/autorun')).status, 403)
  assert.equal((await memberMirror.call('/hr/autorun', { method: 'PUT', body: JSON.stringify({ intervalHours: 24 }) })).status, 403)
  assert.equal((await memberMirror.call('/hr/autorun/tick', { method: 'POST' })).status, 403)

  const put = await call('/hr/autorun', { method: 'PUT', body: JSON.stringify({ intervalHours: 24, spendUsd: 9.5 }) })
  assert.equal(put.status, 200)
  assert.equal(put.json.intervalHours, 24)
  assert.equal(put.json.spendUsd, 9.5)
  assert.equal(put.json.overdueDays, 3, 'untouched field keeps default')
  assert.ok((await call('/hr/autorun')).json.spendUsd === 9.5, 'persisted')

  for (const bad of [
    { intervalHours: -1 }, { intervalHours: 99999 },
    { overdueDays: -1 }, { spendUsd: -0.5 }, { errorRate: 1.5 },
  ]) {
    const res = await call('/hr/autorun', { method: 'PUT', body: JSON.stringify(bad) })
    assert.equal(res.status, 400, `${JSON.stringify(bad)} must 400`)
  }
  assert.equal((await call('/hr/autorun', { method: 'PUT', body: JSON.stringify({}) })).status, 400)
})

test('[autorun] interval=0 — nextPeriodicAt 为 null,到期也不入队', async () => {
  await assignHrComputer()
  await seedAgent('ag-off-1')
  const off = await call('/hr/autorun', { method: 'PUT', body: JSON.stringify({ intervalHours: 0 }) })
  assert.equal(off.status, 200)
  assert.equal(off.json.nextPeriodicAt, null)
  await pool.query(`UPDATE hr_agents SET auto_last_run_at = NOW() - interval '30 days' WHERE company_id = $1`, [COMPANY])
  const t = await tick()
  assert.equal(t.status, 200)
  assert.equal(t.json.periodicFired, false)
  const { rows } = await pool.query(`SELECT COUNT(*)::int AS n FROM hr_reports WHERE company_id = $1`, [COMPANY])
  assert.equal(rows[0].n, 0)
})

/* ───────── 周期例行 ───────── */

test('[autorun] 周期到期 — 入队全员轮 + brief 携 ref + 走同一 CLI 收轮面', async () => {
  await assignHrComputer()
  await seedAgent('ag-per-1')
  await pool.query(`UPDATE hr_agents SET auto_last_run_at = NOW() - interval '8 days' WHERE company_id = $1`, [COMPANY])

  const frames = await wakeFramesDuring(() => tick())
  const wakeFrame = frames.find((f) => f.includes('event: wake') && f.includes('hr-eval'))
  assert.ok(wakeFrame, `wake frame with hr-eval expected, got: ${frames.join('||').slice(0, 300)}`)

  const { rows } = await pool.query(
    `SELECT id, target_agent_id, trigger_kind, status, created_by FROM hr_reports WHERE company_id = $1`,
    [COMPANY],
  )
  assert.equal(rows.length, 1)
  const round = rows[0]
  assert.equal(round.trigger_kind, 'periodic')
  assert.equal(round.status, 'pending')
  assert.equal(round.target_agent_id, null)
  assert.equal(round.created_by, null)
  assert.ok(wakeFrame.includes(round.id), 'brief carries the auto round ref')

  // 去抖点前进:tick 后 lastPeriodicAt ≈ now(GET 面可见下次例行时间)
  const cfg = await call('/hr/autorun')
  assert.ok(new Date(cfg.json.lastPeriodicAt).getTime() > Date.now() - 60_000, 'auto_last_run_at advanced on enqueue')

  // 共用执行/报告面:自动轮走同一 CLI context/report(无第二套管线)
  const ctx = await hrCli(['hr', 'context', round.id])
  assert.equal(ctx.status, 200)
  assert.ok(ctx.json.ok, `hr context must succeed: ${JSON.stringify(ctx.json)}`)
  const flip = await pool.query(`SELECT status FROM hr_reports WHERE id = $1`, [round.id])
  assert.equal(flip.rows[0].status, 'running')
  const rep = await hrCli(['hr', 'report', round.id, JSON.stringify({ summary: 'auto round ok', ratings: {} })])
  assert.equal(rep.status, 200)
  const done = await pool.query(`SELECT status FROM hr_reports WHERE id = $1`, [round.id])
  assert.equal(done.rows[0].status, 'done')
})

test('[autorun] 周期去抖 — 轮创建于 24h 窗内则跳过(含已收口的轮)', async () => {
  await assignHrComputer()
  await seedAgent('ag-deb-1')
  await pool.query(`UPDATE hr_agents SET auto_last_run_at = NOW() - interval '9 days' WHERE company_id = $1`, [COMPANY])
  let first: any
  await wakeFramesDuring(async () => { first = await tick() })
  assert.equal(first.json.periodicFired, true)
  // 到期点已前进,但把到期再回填 —— 去抖(近期已评)必须独立挡住
  await pool.query(`UPDATE hr_agents SET auto_last_run_at = NOW() - interval '9 days' WHERE company_id = $1`, [COMPANY])
  await pool.query(`UPDATE hr_reports SET status = 'done', finished_at = NOW() WHERE company_id = $1`, [COMPANY])
  const second = await tick()
  assert.equal(second.json.periodicFired, false)
  assert.ok(second.json.skipped.includes('periodic-cooldown'), `expected periodic-cooldown, got ${JSON.stringify(second.json.skipped)}`)
  const { rows } = await pool.query(`SELECT COUNT(*)::int AS n FROM hr_reports WHERE company_id = $1`, [COMPANY])
  assert.equal(rows[0].n, 1)
})

test('[autorun] 在飞跳过 / 未到期不烧 / 未指派机器跳过', async () => {
  // 未指派机器(仅置备行)
  const unassigned = await tick()
  assert.ok(unassigned.json.skipped.includes('hr-computer-unassigned'))

  // 指派后未到期(默认 lastRunAt=now)
  await assignHrComputer()
  await seedAgent('ag-gate-1')
  const notDue = await tick()
  assert.equal(notDue.json.periodicFired, false)
  assert.ok(notDue.json.skipped.includes('no-hook-hit'), `expected no-hook-hit, got ${JSON.stringify(notDue.json.skipped)}`)

  // 在飞(手种 pending 轮)
  await pool.query(
    `INSERT INTO hr_reports (id, company_id, trigger_kind, status) VALUES ('hre-seeded', $1, 'manual', 'pending')`,
    [COMPANY],
  )
  const inFlight = await tick()
  assert.ok(inFlight.json.skipped.includes('round-in-flight'))
})

/* ───────── 事件钩子 ───────── */

test('[autorun] 钩子①卡停更 — 已指派卡停更超阈入队目标轮(reason=overdue-card)', async () => {
  await assignHrComputer()
  await seedAgent('ag-card-1')
  await seedOverdueCard('card-stale', 'ag-card-1', 4)
  const frames = await wakeFramesDuring(() => tick())
  const wakeFrame = frames.find((f) => f.includes('event: wake') && f.includes('hr-eval'))
  assert.ok(wakeFrame, 'event round wakes HR with a brief')
  assert.ok(wakeFrame.includes('overdue-card'), 'brief body carries the hook reason')

  const { rows } = await pool.query(
    `SELECT id, target_agent_id, trigger_kind, status FROM hr_reports WHERE company_id = $1`,
    [COMPANY],
  )
  assert.equal(rows.length, 1)
  assert.equal(rows[0].trigger_kind, 'event')
  assert.equal(rows[0].target_agent_id, 'ag-card-1')
  assert.equal(rows[0].status, 'pending')
})

test('[autorun] 钩子②spend — 近 24h LLM spend 超阈(reason=spend-over);阈值即开关', async () => {
  await assignHrComputer()
  await seedAgent('ag-spend-1')
  await pool.query(
    `INSERT INTO llm_calls (id, company_id, agent_id, purpose, model, cost_usd)
     VALUES ('lc-1', $1, 'ag-spend-1', 'agent-turn', 'test-model', 4.0),
            ('lc-2', $1, 'ag-spend-1', 'agent-turn', 'test-model', 2.0)`,
    [COMPANY],
  )
  let fired: any
  await wakeFramesDuring(async () => { fired = await tick() })
  assert.equal(fired.json.events.length, 1)
  assert.equal(fired.json.events[0].reason, 'spend-over')
  assert.equal(fired.json.events[0].agentId, 'ag-spend-1')

  // 阈值抬到 10 → 同数据(总额 6)不再命中
  await pool.query(`UPDATE hr_reports SET status = 'done', finished_at = NOW() WHERE company_id = $1`, [COMPANY])
  await call('/hr/autorun', { method: 'PUT', body: JSON.stringify({ spendUsd: 10 }) })
  const quiet = await tick()
  assert.equal(quiet.json.events.length, 0)
  assert.ok(quiet.json.skipped.includes('no-hook-hit'))
})

test('[autorun] 钩子③错误率 — ≥5 样本且超阈命中(reason=error-rate);样本不足不烧', async () => {
  await assignHrComputer()
  await seedAgent('ag-err-1')
  // 6 次 4 败 = 0.67 ≥ 0.5
  for (let i = 0; i < 6; i++) {
    await pool.query(
      `INSERT INTO agent_runs (id, agent_id, company_id, status, token_count, started_at)
       VALUES ($1, 'ag-err-1', $2, $3, 100, NOW())`,
      [`run-err-${i}`, COMPANY, i < 4 ? 'failed' : 'completed'],
    )
  }
  let fired: any
  await wakeFramesDuring(async () => { fired = await tick() })
  assert.equal(fired.json.events.length, 1)
  assert.equal(fired.json.events[0].reason, 'error-rate')

  // 最小样本闸:另一 agent 3 次 3 败(100% 但样本不足)不命中;同时阈值
  // 抬到 1.0,让首轮 agent(4/6≈0.67)也不再命中 —— 两者同证不烧。
  await pool.query(`UPDATE hr_reports SET status = 'done', finished_at = NOW() WHERE company_id = $1`, [COMPANY])
  await call('/hr/autorun', { method: 'PUT', body: JSON.stringify({ errorRate: 1 }) })
  await seedAgent('ag-err-few')
  for (let i = 0; i < 3; i++) {
    await pool.query(
      `INSERT INTO agent_runs (id, agent_id, company_id, status, token_count, started_at)
       VALUES ($1, 'ag-err-few', $2, 'failed', 100, NOW())`,
      [`run-few-${i}`, COMPANY],
    )
  }
  const quiet = await tick()
  assert.equal(quiet.json.events.length, 0)
})

test('[autorun] 同目标去抖 — 条件持续不重复入队;新目标可入队', async () => {
  await assignHrComputer()
  await seedAgent('ag-cd-a')
  await seedAgent('ag-cd-b')
  await seedOverdueCard('card-a', 'ag-cd-a', 5)
  await seedOverdueCard('card-b', 'ag-cd-b', 5)
  await seedOverdueCard('card-b2', 'ag-cd-b', 6)

  // 每 tick 单轮:两个异常目标,先入队 agentID 排序第一者
  let first: any
  await wakeFramesDuring(async () => { first = await tick() })
  assert.equal(first.json.events.length, 1)
  const firstTarget = first.json.events[0].agentId as string
  const secondTarget = firstTarget === 'ag-cd-a' ? 'ag-cd-b' : 'ag-cd-a'
  assert.ok(['ag-cd-a', 'ag-cd-b'].includes(firstTarget))

  // 同轮在飞 → 整公司跳过
  const inFlight = await tick()
  assert.ok(inFlight.json.skipped.includes('round-in-flight'))

  // CLI 真收口(done)——创建时刻仍在 24h 窗内:同目标去抖生效,
  // 条件持续的第一目标不重复,第二目标接力入队
  const roundId = first.json.events[0].id as string
  assert.equal((await hrCli(['hr', 'context', roundId])).status, 200)
  assert.equal((await hrCli(['hr', 'report', roundId, JSON.stringify({ summary: 'ok' })])).status, 200)
  let second: any
  await wakeFramesDuring(async () => { second = await tick() })
  assert.equal(second.json.events.length, 1)
  assert.equal(second.json.events[0].agentId, secondTarget)
  assert.ok(second.json.skipped.includes(`cooldown:${firstTarget}`), `same-target debounce visible, got ${JSON.stringify(second.json.skipped)}`)
  // 三轮:第一目标(done,窗内)+ 第二目标(pending 在飞)= 恰两轮
  const { rows } = await pool.query(`SELECT COUNT(*)::int AS n FROM hr_reports WHERE company_id = $1`, [COMPANY])
  assert.equal(rows[0].n, 2)
})

test('[autorun] 全员轮覆盖各目标 — 近期全员轮后事件钩子按同目标去抖', async () => {
  await assignHrComputer()
  await seedAgent('ag-cover-1')
  await seedOverdueCard('card-cover', 'ag-cover-1', 5)
  // 近期全员轮(手种 23h 前)—— 钩子对 ag-cover-1 应被去抖挡住
  await pool.query(
    `INSERT INTO hr_reports (id, company_id, target_agent_id, trigger_kind, status, created_at)
     VALUES ('hre-full-recent', $1, NULL, 'manual', 'done', NOW() - interval '23 hours')`,
    [COMPANY],
  )
  const t = await tick()
  assert.equal(t.json.events.length, 0)
  assert.ok(t.json.skipped.includes('cooldown:ag-cover-1'), `expected cooldown skip, got ${JSON.stringify(t.json.skipped)}`)
})

test('[autorun] member 触发 tick 403;admin 可读配置', async () => {
  assert.equal((await memberMirror.call('/hr/autorun/tick', { method: 'POST' })).status, 403)
  assert.equal((await adminMirror.call('/hr/autorun')).status, 200)
})
