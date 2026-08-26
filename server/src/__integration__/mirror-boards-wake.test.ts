/**
 * Mirror test: 看板 @提及/指派的 agent 唤醒(#82)——TS(router.ts
 * wakeMentionedAgents 5 调用点 → scheduler.wakeAgent('manual') →
 * wake-bus SSE)与 Go(internal/runtime WakeMentionedAgents + wakebus)双跑。
 *
 * 端到端断言:建卡(@提及)、重指派、评论(@提及)各触发被提及 agent 的
 * wake-stream 收到 reason='manual' 的 wake 事件;发起者自己不被唤醒。
 * buildApiTestApp 不挂 /runtime——本文件自组 app(/api + /runtime 同进程,
 * Redis wake-bus 打通)。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import { randomUUID } from 'node:crypto'
import { ensureSchemaOnce, resetAllTables, teardownAll, MIRROR_BASE } from './_helpers.js'
import { signAgentToken } from '../agents/runtime/jwt.js'
import { pool } from '../db/pool.js'

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
  app.use(express.json({ limit: '34mb' }))
  app.use((req, _res, next) => {
    ;(req as unknown as { authUserId: string }).authUserId = 'wake-user'
    next()
  })
  const { api } = await import('../api/router.js')
  app.use('/api', api)
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
  await teardownAll(server ?? undefined)
})

let fixture: { companyId: string; agentId: string; agent2Id: string; token: string; boardId: string; columnId: string }

async function seed(): Promise<void> {
  const companyId = `c-${randomUUID().slice(0, 8)}`
  const agentId = `a-${randomUUID().slice(0, 8)}`
  const agent2Id = `a-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Co', $1, 'wake-user')`, [companyId])
  await pool.query(
    `INSERT INTO users (id, email, display_name) VALUES ('wake-user', 'w@t.local', 'Waker') ON CONFLICT DO NOTHING`)
  await pool.query(
    `INSERT INTO company_members (company_id, user_id, role) VALUES ($1, 'wake-user', 'owner')`, [companyId])
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Wakee', 'tester', 'W', '#000', 'avail'),
            ($3, $2, 'agent', 'Wakee2', 'tester', 'W', '#111', 'avail')`,
    [agentId, companyId, agent2Id])
  const boardId = `b-${randomUUID().slice(0, 8)}`
  const columnId = `col-${randomUUID().slice(0, 8)}`
  await pool.query(`INSERT INTO boards (id, company_id, title, created_by) VALUES ($1, $2, 'Board', 'wake-user')`, [boardId, companyId])
  await pool.query(`INSERT INTO board_columns (id, board_id, title, position) VALUES ($1, $2, 'To Do', 0)`, [columnId, boardId])
  fixture = { companyId, agentId, agent2Id, token: signAgentToken({ agentId, companyId }), boardId, columnId }
}

async function call(path: string, init: RequestInit = {}): Promise<{ status: number; body: any }> {
  const res = await fetch(`${baseUrl}/api${path}`, {
    ...init,
    headers: { 'content-type': 'application/json', 'x-company-id': fixture.companyId,
      ...(MIRROR_BASE ? { 'x-test-user': 'wake-user' } : {}), ...(init.headers ?? {}) },
  })
  return { status: res.status, body: await res.json().catch(() => null) }
}

/** 订阅 agent 的 wake-stream,收集 wake 事件直到谓词满足或超时。 */
async function collectWakes(token: string, until: (reasons: string[]) => boolean, timeoutMS = 6000): Promise<any[]> {
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), timeoutMS)
  const events: any[] = []
  try {
    const res = await fetch(`${baseUrl}/runtime/wake-stream`, {
      headers: { authorization: `Bearer ${token}` },
      signal: ctrl.signal,
    })
    assert.equal(res.status, 200)
    const reader = (res.body as any).getReader() as { read(): Promise<{ done: boolean; value?: Uint8Array }> }
    const dec = new TextDecoder()
    let buf = ''
    const deadline = Date.now() + timeoutMS
    for (;;) {
      const result = await Promise.race([
        reader.read(),
        new Promise<{ done: true }>((resolve) => setTimeout(() => resolve({ done: true } as const), Math.max(1, deadline - Date.now()))),
      ])
      if (result.done) break
      buf += dec.decode(result.value, { stream: true })
      for (;;) {
        const idx = buf.indexOf('\n\n')
        if (idx < 0) break
        const frame = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        if (!frame.startsWith('event: wake')) continue
        const dataLine = frame.split('\n').find((l) => l.startsWith('data: '))
        if (dataLine) events.push(JSON.parse(dataLine.slice(6)))
        if (until(events.map((e) => e.reason))) return events
      }
      if (Date.now() >= deadline) break
    }
    return events
  } finally {
    clearTimeout(timer)
    ctrl.abort()
  }
}

test('[mirror-boards-wake] card create with @mention wakes the agent (reason=manual)', async () => {
  await seed()
  const collecting = collectWakes(fixture.token, (rs) => rs.filter((r) => r === 'manual').length >= 1)
  // 等流就绪:先建一块无提及的卡(不产唤醒),流上只应有 ready。
  await new Promise((r) => setTimeout(r, 300))
  const created = await call(`/boards/${fixture.boardId}/cards`, {
    method: 'POST',
    body: JSON.stringify({ title: `hey @${fixture.agentId} look`, columnId: fixture.columnId }),
  })
  assert.equal(created.status, 200)
  assert.deepEqual(created.body.mentions, [fixture.agentId])
  const events = await collecting
  const manual = events.filter((e) => e.reason === 'manual')
  assert.equal(manual.length, 1, `exactly one manual wake, got ${JSON.stringify(events)}`)
  assert.equal(manual[0].kind, 'wake')
  assert.equal(manual[0].conversationId, null)
})

test('[mirror-boards-wake] assignment without @token wakes the assignee; actor never woken', async () => {
  await seed()
  const token2 = signAgentToken({ agentId: fixture.agent2Id, companyId: fixture.companyId })
  const collecting = collectWakes(token2, (rs) => rs.filter((r) => r === 'manual').length >= 1)
  await new Promise((r) => setTimeout(r, 300))
  const created = await call(`/boards/${fixture.boardId}/cards`, {
    method: 'POST',
    body: JSON.stringify({ title: 'plain card', columnId: fixture.columnId, assigneeId: fixture.agent2Id }),
  })
  assert.equal(created.status, 200)
  const cardId = created.body.id
  const events = await collecting
  assert.equal(events.filter((e) => e.reason === 'manual').length, 1, 'assignee woken exactly once')
  // 发起者(wake-user 是 human,不可能是 agent)——换 agent 发起者验证自过滤:
  // 用 agent2 自己指派自己 → 不唤醒任何人(自提及滤除)。
  const selfAssign = await call(`/boards/${fixture.boardId}/cards/${cardId}`, {
    method: 'PATCH',
    body: JSON.stringify({ assigneeId: fixture.agent2Id }),
  })
  assert.equal(selfAssign.status, 200)
  await new Promise((r) => setTimeout(r, 600))
})

test('[mirror-boards-wake] reassignment wakes the new assignee; comment @mention wakes too', async () => {
  await seed()
  const card = await call(`/boards/${fixture.boardId}/cards`, {
    method: 'POST',
    body: JSON.stringify({ title: 'to move', columnId: fixture.columnId }),
  })
  assert.equal(card.status, 200)
  const cardId = card.body.id

  // 重指派 → 新 assignee 唤醒
  const collecting = collectWakes(fixture.token, (rs) => rs.filter((r) => r === 'manual').length >= 1)
  await new Promise((r) => setTimeout(r, 300))
  const moved = await call(`/boards/${fixture.boardId}/cards/${cardId}`, {
    method: 'PATCH',
    body: JSON.stringify({ assigneeId: fixture.agentId }),
  })
  assert.equal(moved.status, 200)
  let events = await collecting
  assert.equal(events.filter((e) => e.reason === 'manual').length, 1, 'reassignment wakes new assignee')

  // 评论 @提及 → 唤醒
  const collecting2 = collectWakes(fixture.token, (rs) => rs.filter((r) => r === 'manual').length >= 1)
  await new Promise((r) => setTimeout(r, 300))
  const commented = await call(`/boards/${fixture.boardId}/cards/${cardId}/comments`, {
    method: 'POST',
    body: JSON.stringify({ body: `ping @${fixture.agentId}` }),
  })
  assert.equal(commented.status, 200)
  assert.deepEqual(commented.body.mentions, [fixture.agentId])
  events = await collecting2
  assert.equal(events.filter((e) => e.reason === 'manual').length, 1, 'comment mention wakes')
})
