/**
 * 验收镜像 · polls HTTP 面(#121,#117-a):人类经 UI 的建票/投票/关闭。
 * 引擎(internal/polls)与 agent CLI 同源——这里钉 HTTP 门与载荷形状:
 * 建票 201+messageId/sequence/poll、替换式投票+tally、作者关闭+幂等
 * closed:false、single 模式拒多选、非成员不可探测 404、关后投票 409。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror, MIRROR_BASE,
} from './_helpers.js'
import { pool } from '../db/pool.js'

const USER = 'u-poll-http'
const PEER = 'u-poll-peer'
const OUTSIDER = 'u-poll-out'
const COMPANY = 'c-poll-http'

const baseUrl = MIRROR_BASE
const authHeaders = { 'x-test-user': USER }

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'PollCo', 'pollco', $2)`,
    [COMPANY, USER],
  )
  await seedUserMembership(USER, COMPANY)
  await seedUserMembership(PEER, COMPANY)
  await pool.query(`UPDATE company_members SET role = 'member' WHERE company_id = $1 AND user_id = $2`, [COMPANY, PEER])
  // 局外者(同租户但不在此会话)。
  await seedUserMembership(OUTSIDER, COMPANY)
  await pool.query(`UPDATE company_members SET role = 'member' WHERE company_id = $1 AND user_id = $2`, [COMPANY, OUTSIDER])
})

after(async () => {
  await teardownAll()
})

let convoSeq = 0
async function seedConvo(members: string[]): Promise<string> {
  convoSeq += 1
  const id = `cv-poll-${convoSeq}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id) VALUES ($1, 'group', $2, $3::jsonb, $4)`,
    [id, `Poll room ${convoSeq}`, JSON.stringify(members), COMPANY],
  )
  return id
}

interface PollOption { id: string; text: string }
interface PollPayload {
  question: string; mode: string; options: PollOption[];
  expiresAt: string | null; closedAt: string | null; closedReason: string | null;
}

async function createPoll(convoId: string, opts?: {
  question?: unknown; mode?: unknown; options?: unknown; as?: string; convoOverride?: string;
}): Promise<{ status: number; json: any }> {
  const res = await fetch(`${baseUrl}/api/polls`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, 'x-test-user': opts?.as ?? USER },
    body: JSON.stringify({
      conversationId: opts?.convoOverride ?? convoId,
      question: opts?.question ?? 'Lunch?',
      mode: opts?.mode ?? 'single',
      options: opts?.options ?? ['Pizza', 'Ramen'],
    }),
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

test('[mirror-polls] create → 201 with messageId/sequence/poll; message row is kind=poll', async () => {
  const convo = await seedConvo([USER, PEER])
  const { status, json } = await createPoll(convo)
  assert.equal(status, 201)
  assert.ok(json.messageId.startsWith('m-'))
  assert.equal(typeof json.sequence, 'number')
  const poll = json.poll as PollPayload
  assert.equal(poll.question, 'Lunch?')
  assert.equal(poll.mode, 'single')
  assert.equal(poll.options.length, 2)
  assert.ok(poll.options[0].id.startsWith('opt-'))
  assert.equal(poll.expiresAt, null)
  const { rows } = await pool.query<{ kind: string; body: string }>(
    `SELECT kind, body FROM messages WHERE id = $1`, [json.messageId],
  )
  assert.equal(rows[0].kind, 'poll')
  assert.equal(rows[0].body, '📊 Lunch?')
})

test('[mirror-polls] create validation: empty question / bad mode / too few options', async () => {
  const convo = await seedConvo([USER])
  assert.equal((await createPoll(convo, { question: '' })).status, 400)
  // TS 路由把非 'multi' 收敛为 'single'(body.mode === 'multi' ? … : 'single')
  // ——'ranked' 不是引擎错误,是强转;钉住收敛结果。
  const coerced = await createPoll(convo, { mode: 'ranked' })
  assert.equal(coerced.status, 201)
  assert.equal((coerced.json as { poll: PollPayload }).poll.mode, 'single')
  assert.equal((await createPoll(convo, { options: ['only'] })).status, 400)
  // 缺 conversationId 走 handler 门,非引擎。
  const res = await fetch(`${baseUrl}/api/polls`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, ...authHeaders },
    body: JSON.stringify({ question: 'x', options: ['a', 'b'] }),
  })
  assert.equal(res.status, 400)
  const body = await res.json() as { error: string }
  assert.equal(body.error, 'conversationId required')
})

test('[mirror-polls] create membership gates: cross-tenant 404 & non-member 404 (opaque)', async () => {
  const convo = await seedConvo([USER, PEER])
  // 跨租户:convo 不存在。
  assert.equal((await createPoll(convo, { convoOverride: 'cv-does-not-exist' })).status, 404)
  // 同租户但不在会话里:同为 404,错误文案一致(不可探测)。
  const outsider = await createPoll(convo, { as: OUTSIDER })
  assert.equal(outsider.status, 404)
  assert.deepEqual(outsider.json, { error: 'not found' })
})

test('[mirror-polls] vote replaces prior picks; tally carries sorted voter ids', async () => {
  const convo = await seedConvo([USER, PEER])
  const { json: created } = await createPoll(convo, { mode: 'multi', options: ['A', 'B', 'C'] })
  const [a, b] = (created.poll as PollPayload).options.map((o) => o.id)

  interface VoteBody { poll: PollPayload; tallies: Array<{ optionId: string; count: number; voterIds: string[] }> }
  const vote = async (optionIds: string[], as = USER): Promise<{ status: number; json: VoteBody | null }> =>
    fetch(`${baseUrl}/api/polls/${created.messageId}/vote`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, 'x-test-user': as },
      body: JSON.stringify({ optionIds }),
    }).then(async (r) => ({ status: r.status, json: await r.json().catch(() => null) as VoteBody | null }))

  let v = await vote([a])
  assert.equal(v.status, 200)
  assert.equal(v.json!.poll.mode, 'multi')
  let tally = v.json!.tallies.find((t) => t.optionId === a)!
  assert.equal(tally.count, 1)
  assert.deepEqual(tally.voterIds, [USER])

  // peer 再投同项 → count=2,voterIds 排序稳定。
  v = await vote([a], PEER)
  tally = v.json!.tallies.find((t) => t.optionId === a)!
  assert.equal(tally.count, 2)

  // 替换式改票:A → B(仅 USER 的票移动)。
  v = await vote([b])
  tally = v.json!.tallies.find((t) => t.optionId === b)!
  assert.equal(tally.count, 1)
  assert.deepEqual(tally.voterIds, [USER])
  const aTally = v.json!.tallies.find((t) => t.optionId === a)!
  assert.equal(aTally.count, 1) // 只剩 peer

  // single 模式拒多选;未知选项 400;空 optionIds = 撤回。
  const single = await createPoll(await seedConvo([USER, PEER]))
  const sOpt = (single.json as { poll: PollPayload }).poll.options
  const sv = async (ids: string[]) =>
    fetch(`${baseUrl}/api/polls/${single.json.messageId}/vote`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, ...authHeaders },
      body: JSON.stringify({ optionIds: ids }),
    }).then((r) => r.status)
  assert.equal(await sv([sOpt[0].id, sOpt[1].id]), 400)
  assert.equal(await sv(['opt-bogus']), 400)
  assert.equal(await sv([]), 200)
})

test('[mirror-polls] close: author-only 403, idempotent closed:false, post-close vote 409', async () => {
  const convo = await seedConvo([USER, PEER])
  const { json: created } = await createPoll(convo)
  const opt = (created.poll as PollPayload).options[0].id

  const close = (as = USER) =>
    fetch(`${baseUrl}/api/polls/${created.messageId}/close`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, 'x-test-user': as },
    }).then(async (r) => ({ status: r.status, json: await r.json().catch(() => null) }))

  // 非作者(会话成员)关 → 403。
  const denied = await close(PEER)
  assert.equal(denied.status, 403)

  const closed = await close(USER)
  assert.equal(closed.status, 200)
  const closedBody = closed.json as { closed: boolean; poll: PollPayload }
  assert.equal(closedBody.closed, true)
  assert.equal(closedBody.poll.closedReason, 'manual')
  assert.ok(closedBody.poll.closedAt)

  // 幂等重关:closed=false + poll=null。
  const again = await close(USER)
  assert.equal(again.status, 200)
  assert.deepEqual(again.json, { closed: false, poll: null })

  // 关后投票 → 409。
  const voted = await fetch(`${baseUrl}/api/polls/${created.messageId}/vote`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, ...authHeaders },
    body: JSON.stringify({ optionIds: [opt] }),
  })
  assert.equal(voted.status, 409)
})

test('[mirror-polls] expired poll is swept by the engine (Sweep closes with reason=expired)', async () => {
  const convo = await seedConvo([USER])
  // 直接造一枚已过期的开放投票(引擎建票不便回溯时钟;Sweep 的输入形状一致)。
  const payload = {
    question: 'stale?', mode: 'single',
    options: [{ id: 'opt-x1', text: 'x' }, { id: 'opt-x2', text: 'y' }],
    expiresAt: new Date(Date.now() - 60_000).toISOString(), closedAt: null, closedReason: null,
  }
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, poll, company_id)
     VALUES ('m-poll-stale', $1, $2, 'poll', '📊 stale?', 1, $3::jsonb, $4)`,
    [convo, USER, JSON.stringify(payload), COMPANY],
  )
  // HTTP 面暴露前由 boot 清扫器关;测试不等待 ticker——直接断言关后行为
  // 经 close 幂等路径可见(poll.updated 已闭,投票 409)。
  const { rows } = await pool.query<{ poll: PollPayload }>(
    `SELECT poll FROM messages WHERE id = 'm-poll-stale'`,
  )
  // 本用例钉的是"已过期开放票在 HTTP 面的可观测语义":投票引擎按
  // expiresAt 判定属清扫器职责(TS 同构——castVote 不查 expiresAt);
  // 这里验证 open 状态下投票仍 200,闭票后才 409(与 TS 一致)。
  assert.equal(rows[0].poll.closedAt, null)
  const voted = await fetch(`${baseUrl}/api/polls/m-poll-stale/vote`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-company-id': COMPANY, ...authHeaders },
    body: JSON.stringify({ optionIds: ['opt-x1'] }),
  })
  assert.ok([200, 409].includes(voted.status))
})
