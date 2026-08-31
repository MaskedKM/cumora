/**
 * Mirror test: 唤醒调度(#62)——TS(scheduler.ts 订阅环,in-process)与
 * Go(internal/runtime scheduler 循环,boot 已起)同一套 wire 断言。
 *
 * 覆盖:CH_MESSAGE_NEW → claimAndWake 去重 → 成员唤醒(作者排除)、
 * 静音豁免三口(direct / 精确 @提及 / 引用本人)、agent 作者的
 * turn-rate 地板(超 30/min 丢)、busy 租约 → steer 帧同流补发、
 * CH_POLLS → 投票发起者唤醒(pollBrief 携带 / 投票去重 / 自操作跳过)。
 *
 * TS 形态在 before() 显式 startScheduler()(对齐 Go boot 行为);两侧
 * 共享同一 Redis(REDIS_URL),SSE 帧从 /runtime/wake-stream 读回。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { ensureSchemaOnce, resetAllTables, teardownAll, MIRROR_BASE } from './_helpers.js'
import { signAgentToken } from './harness/agents/runtime/jwt.js'
import { pool } from './harness/db/pool.js'
import { redis } from './harness/redis.js'
import { CH_MESSAGE_NEW, CH_POLLS } from './harness/redis.js'

let baseUrl = ''

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  baseUrl = MIRROR_BASE
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
})

after(async () => {
  await teardownAll()
})

/* ── 种子与工具(与 mirror-runtime 同形,文件内自持) ───────────── */

async function seedAgent(): Promise<{ agentId: string; companyId: string; token: string }> {
  const agentId = `ag-${randomUUID().slice(0, 8)}`
  const companyId = `co-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
       VALUES ($1, $2, 'agent', 'Robo', 'helper', 'R', '#005577', 'avail')`,
    [agentId, companyId],
  )
  const token = signAgentToken({ agentId, companyId })
  return { agentId, companyId, token }
}

async function seedSecondAgent(companyId: string): Promise<string> {
  const agentId = `ag-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
       VALUES ($1, $2, 'agent', 'Bram', 'peer', 'B', '#775500', 'avail')`,
    [agentId, companyId],
  )
  return agentId
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

async function seedConversation(
  companyId: string, members: string[], kind: 'direct' | 'group' = 'group',
): Promise<{ convId: string; insertMessage: (authorId: string, body: string) => Promise<string> }> {
  const convId = `cv-${randomUUID().slice(0, 8)}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, $2, 'Test convo', $3::jsonb, $4)`,
    [convId, kind, JSON.stringify(members), companyId],
  )
  let seq = 0
  return {
    convId,
    insertMessage: async (authorId: string, body: string) => {
      seq += 1
      const id = `m-${randomUUID().slice(0, 8)}`
      await pool.query(
        `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
         VALUES ($1, $2, $3, 'text', $4, $5, $6)`,
        [id, convId, authorId, body, seq, companyId],
      )
      return id
    },
  }
}

function messageNewEvent(args: {
  convId: string; companyId: string; messageId: string; authorId: string; body: string
  quotedMessageId?: string
}) {
  return {
    type: 'message.new',
    conversationId: args.convId,
    companyId: args.companyId,
    message: {
      id: args.messageId,
      conversationId: args.convId,
      authorId: args.authorId,
      kind: 'text',
      body: args.body,
      sequence: 1,
      at: new Date().toISOString(),
      ...(args.quotedMessageId ? { quotedMessageId: args.quotedMessageId } : {}),
    },
  }
}

/** 收集窗口内全部 SSE 帧;deadline 后返回(不断言)。 */
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
  } catch { /* abort 收尾 */ } finally {
    clearTimeout(timer)
    ctrl.abort()
  }
  return frames
}

function wakeFrames(frames: string[]): any[] {
  return frames
    .filter((f) => f.startsWith('event: wake'))
    .map((f) => JSON.parse(f.split('\n').find((l) => l.startsWith('data: '))!.slice('data: '.length)))
}

function steerFrames(frames: string[]): any[] {
  return frames
    .filter((f) => f.startsWith('event: steer'))
    .map((f) => JSON.parse(f.split('\n').find((l) => l.startsWith('data: '))!.slice('data: '.length)))
}

async function publish(channel: string, payload: unknown): Promise<void> {
  await redis.publish(channel, JSON.stringify(payload))
}

/* ── message.new → 成员唤醒 ──────────────────────────────────────── */

test('[mirror-scheduler] message.new wakes member agents, excludes the author, dedupes by message id', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const peerId = await seedSecondAgent(companyId)
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, peerId, humanId])
  const messageId = `m-${randomUUID().slice(0, 8)}`

  const frames = collectSSE(`${baseUrl}/runtime/wake-stream`, token, 2500)
  await new Promise((r) => setTimeout(r, 300)) // 等订阅建好(ready 帧已发)
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: convo.convId, companyId, messageId, authorId: humanId, body: 'hello room',
  }))
  // 同一 message id 重放 → claimAndWake 的 SETNX 幂等,不得二次唤醒。
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: convo.convId, companyId, messageId, authorId: humanId, body: 'hello room',
  }))
  const got = await frames
  const wakes = wakeFrames(got)
  assert.equal(wakes.length, 1, `exactly one wake (deduped), got ${wakes.length}`)
  assert.equal(wakes[0].reason, 'message.new')
  assert.equal(wakes[0].conversationId, convo.convId)
  assert.equal(wakes[0].kind, 'wake')
})

test('[mirror-scheduler] agent-authored message still wakes peers (turn-rate floor is generous)', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const peerId = await seedSecondAgent(companyId)
  const convo = await seedConversation(companyId, [agentId, peerId])
  const messageId = `m-${randomUUID().slice(0, 8)}`

  const frames = collectSSE(`${baseUrl}/runtime/wake-stream`, token, 2500)
  await new Promise((r) => setTimeout(r, 300))
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: convo.convId, companyId, messageId, authorId: peerId, body: 'peer ping',
  }))
  const got = await frames
  const wakes = wakeFrames(got)
  assert.equal(wakes.length, 1, 'peer member woken by agent author (under the 30/min floor)')
})

/* ── 静音豁免三口 ───────────────────────────────────────────────── */

test('[mirror-scheduler] muted agent: silent in group, wakes on @mention / direct / quote-reply', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const group = await seedConversation(companyId, [agentId, humanId])
  await pool.query(
    `INSERT INTO conversation_mutes (user_id, conversation_id) VALUES ($1, $2)`,
    [agentId, group.convId],
  )
  const direct = await seedConversation(companyId, [agentId, humanId], 'direct')

  const frames = collectSSE(`${baseUrl}/runtime/wake-stream`, token, 4200)
  await new Promise((r) => setTimeout(r, 300))

  // 静音群 + 无提及 → 不唤醒。
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: group.convId, companyId, messageId: `m-${randomUUID().slice(0, 8)}`,
    authorId: humanId, body: 'plain group chatter',
  }))
  await new Promise((r) => setTimeout(r, 500))

  // 精确 @提及 → 唤醒(边界保护:}@id. 形态也必须算——词边界)。
  const mentionedId = `m-${randomUUID().slice(0, 8)}`
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: group.convId, companyId, messageId: mentionedId,
    authorId: humanId, body: `hey @${agentId} need your input`,
  }))
  await new Promise((r) => setTimeout(r, 500))

  // 引用本人消息的回复 → 唤醒(quotedMessageId → 落库作者回查)。
  const own = await group.insertMessage(agentId, 'my earlier take')
  const quotedId = `m-${randomUUID().slice(0, 8)}`
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: group.convId, companyId, messageId: quotedId,
    authorId: humanId, body: 're: your take', quotedMessageId: own,
  }))
  await new Promise((r) => setTimeout(r, 500))

  // direct → 恒投递(静音不适用)。
  const dmId = `m-${randomUUID().slice(0, 8)}`
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: direct.convId, companyId, messageId: dmId, authorId: humanId, body: 'direct ping',
  }))

  const got = await frames
  const wakes = wakeFrames(got)
  assert.equal(wakes.length, 3, 'mention + quote-reply + direct wake; plain chatter does not')
  const convos = wakes.map((w) => w.conversationId).sort()
  assert.deepEqual(convos, [direct.convId, group.convId, group.convId].sort())
})

/* ── busy → steer 帧同流补发 ───────────────────────────────────── */

test('[mirror-scheduler] busy agent also receives a steer frame with the message body', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId])
  const messageId = `m-${randomUUID().slice(0, 8)}`

  const hb = await fetch(`${baseUrl}/runtime/busy/heartbeat`, {
    method: 'POST',
    headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json' },
    body: JSON.stringify({ ttlSec: 30 }),
  })
  assert.equal(hb.status, 200)

  const frames = collectSSE(`${baseUrl}/runtime/wake-stream`, token, 2500)
  await new Promise((r) => setTimeout(r, 300))
  await publish(CH_MESSAGE_NEW, messageNewEvent({
    convId: convo.convId, companyId, messageId, authorId: humanId, body: 'mid-turn injection please',
  }))
  const got = await frames
  assert.equal(wakeFrames(got).length, 1, 'wake still fires')
  const steers = steerFrames(got)
  assert.equal(steers.length, 1, 'steer rides the same stream')
  assert.equal(steers[0].kind, 'steer')
  assert.equal(steers[0].messageId, messageId)
  assert.equal(steers[0].body, 'mid-turn injection please')
  assert.equal(steers[0].conversationId, convo.convId)
})

/* ── CH_POLLS → 投票发起者唤醒 ─────────────────────────────────── */

test('[mirror-scheduler] poll.updated wakes the author with a brief; debounced; self-actor skipped', async () => {
  const { agentId, companyId, token } = await seedAgent()
  const humanId = await seedHuman(companyId)
  const convo = await seedConversation(companyId, [agentId, humanId])
  const messageId = await convo.insertMessage(agentId, 'poll: lunch?')

  const pollUpdated = (actorId: string | null) => ({
    type: 'poll.updated',
    companyId,
    conversationId: convo.convId,
    messageId,
    poll: {
      question: 'lunch?',
      mode: 'single',
      options: [{ id: 'opt-a', text: 'noodles' }, { id: 'opt-b', text: 'rice' }],
      expiresAt: null,
      closedAt: null,
      closedReason: null,
    },
    tallies: [{ optionId: 'opt-a', count: 1, voterIds: [humanId] }],
    actorId,
  })

  const frames = collectSSE(`${baseUrl}/runtime/wake-stream`, token, 4200)
  await new Promise((r) => setTimeout(r, 300))

  // 自操作(作者自己投票)→ 不自唤。
  await publish(CH_POLLS, pollUpdated(agentId))
  await new Promise((r) => setTimeout(r, 500))

  // 人类投票 → 唤醒 + pollBrief。
  await publish(CH_POLLS, pollUpdated(humanId))
  await new Promise((r) => setTimeout(r, 500))

  // 8s 去重窗内的第二票 → 不再唤醒。
  await publish(CH_POLLS, pollUpdated(humanId))

  const got = await frames
  const wakes = wakeFrames(got)
  assert.equal(wakes.length, 1, 'author woken once; self-actor and debounce silent')
  assert.equal(wakes[0].reason, 'poll.updated')
  const brief = wakes[0].pollBrief
  assert.ok(brief, 'pollBrief carried on the wake')
  assert.equal(brief.question, 'lunch?')
  assert.equal(brief.totalVotes, 1)
  assert.equal(brief.status, 'open')
  assert.equal(brief.phase, 'vote')
  assert.equal(brief.tallies.length, 2)
  assert.equal(brief.tallies[0].count, 1)
  assert.equal(brief.tallies[0].voters[0].id, humanId)
  assert.ok(Array.isArray(brief.pending), 'pending voters listed')
})
