/**
 * Integration test: chat-style replies in an email conversation get
 * auto-promoted into real email replies (sendViaProvider path).
 *
 * The bug this guards: before round 15, typing into the chat input of
 * an email thread — or an agent calling `cumora reply` from its CLI —
 * just wrote a kind='text' row. The external recipient never saw the
 * reply, so users assumed the email feature was half-broken. Now both
 * paths converge on replyInEmailConversation, which builds reply
 * headers from the latest email_messages row in the convo and routes
 * the body through Resend (or mock).
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { ensureSchemaOnce, resetAllTables, seedCompanyWithAgent,
  seedUserMembership, teardownAll, MIRROR_BASE,
} from './_helpers.js'
import { pool } from './harness/db/pool.js'
import { findOrCreateEmailConversation, persistEmailMessage } from './_email-seeds.js'

const ME_USER_ID = 'u-test-promote'
const baseUrl = MIRROR_BASE
const authHeaders = { 'x-test-user': ME_USER_ID }

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
})

after(async () => {
  await teardownAll()
})

/** Stand up an email conversation with one inbound row from an external
 *  sender to ME_USER_ID, so a reply has somewhere to thread under. */
async function seedEmailConvoWithInbound(): Promise<{ companyId: string; conversationId: string; agentId: string }> {
  const { companyId, agentId, agentEmail } = await seedCompanyWithAgent()
  await seedUserMembership(ME_USER_ID, companyId)
  const conv = await findOrCreateEmailConversation({
    companyId, inReplyTo: null, references: [],
    subject: 'project status', memberIds: [ME_USER_ID, agentId],
  })
  await persistEmailMessage({
    conversationId: conv.conversationId, companyId, authorId: `external:alice@example.com`,
    direction: 'in', transportStatus: 'received',
    smtpMessageId: `alice-original-${Date.now()}@example.com`,
    inReplyTo: null, references: [],
    subject: 'project status',
    fromAddr: 'Alice <alice@example.com>',
    toAddrs: [agentEmail],
    body: 'how is it going?',
  })
  return { companyId, conversationId: conv.conversationId, agentId }
}

test('[integration] POST /conversations/:id/messages in an email convo auto-promotes to a real send', async () => {
  const { conversationId, companyId } = await seedEmailConvoWithInbound()
  const res = await fetch(`${baseUrl}/api/conversations/${encodeURIComponent(conversationId)}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': companyId },
    body: JSON.stringify({ body: 'going great — full status attached below.' }),
  })
  assert.equal(res.status, 202)
  const payload = await res.json() as { id: string; transportStatus: string; mock: boolean }
  assert.equal(payload.transportStatus, 'sent')
  assert.equal(payload.mock, true, 'expect mock mode in default integration env')

  // The new row must be kind='email', direction='out', author=ME, and
  // its from_addr must derive from a minted participants.email — NOT a
  // text message.
  const { rows } = await pool.query<{
    kind: string; direction: string; from_addr: string; auto_submitted: boolean; conversation_id: string;
  }>(
    `SELECT m.kind, em.direction, em.from_addr, em.auto_submitted, em.conversation_id
       FROM messages m JOIN email_messages em ON em.message_id = m.id
      WHERE m.id = $1`,
    [payload.id],
  )
  assert.equal(rows.length, 1)
  assert.equal(rows[0].kind, 'email', 'auto-promote must write a kind=email row, not kind=text')
  assert.equal(rows[0].direction, 'out')
  assert.equal(rows[0].auto_submitted, false, 'human-driven HTTP reply: autoSubmitted should be false')
  assert.equal(rows[0].conversation_id, conversationId)
})

test('[integration] HTTP reply targets the external sender, not self', async () => {
  // The reply-all split must put the external original sender in TO and
  // drop ME_USER_ID from the recipients (otherwise we'd be emailing
  // ourselves on every reply).
  const { conversationId, companyId } = await seedEmailConvoWithInbound()
  const res = await fetch(`${baseUrl}/api/conversations/${encodeURIComponent(conversationId)}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': companyId },
    body: JSON.stringify({ body: 'reply body' }),
  })
  assert.equal(res.status, 202)
  const payload = await res.json() as { id: string }
  const { rows } = await pool.query<{ to_addrs: string[]; subject: string; in_reply_to: string | null }>(
    `SELECT to_addrs, subject, in_reply_to FROM email_messages WHERE message_id = $1`,
    [payload.id],
  )
  assert.ok(rows[0].to_addrs.some((addr) => addr.includes('alice@example.com')),
    `reply must address the original sender, got: ${JSON.stringify(rows[0].to_addrs)}`)
  assert.match(rows[0].subject, /^Re: /, 'subject must gain the Re: prefix')
  assert.ok(rows[0].in_reply_to, 'in_reply_to must be set to thread the reply')
})

test('[integration] empty body in an email convo POST is rejected with 400', async () => {
  const { conversationId, companyId } = await seedEmailConvoWithInbound()
  const res = await fetch(`${baseUrl}/api/conversations/${encodeURIComponent(conversationId)}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': companyId },
    body: JSON.stringify({ body: '' }),
  })
  assert.equal(res.status, 400)
})

test('[integration] reply continues the thread when the latest row is our own outbound', async () => {
  // Bug fixed in v0.1.14: when the user composes "hello — world!" and then,
  // before anyone replies, types a follow-up in the chat input, the
  // replyInEmailConversation helper would call splitReplyAddresses on
  // OUR OWN outbound (parent.from = self), get an empty TO list, and
  // throw "no remaining recipients". The reply should instead continue
  // the thread to the same recipients we just addressed.
  const { findOrCreateEmailConversation, persistEmailMessage } = await import('./_email-seeds.js')
  const { companyId, agentId, agentEmail } = await seedCompanyWithAgent()
  await seedUserMembership(ME_USER_ID, companyId)

  // Seed an outbound row from ME to the agent — no inbound replies yet.
  const conv = await findOrCreateEmailConversation({
    companyId, inReplyTo: null, references: [],
    subject: 'hello', memberIds: [ME_USER_ID, agentId],
  })
  await persistEmailMessage({
    conversationId: conv.conversationId, companyId, authorId: ME_USER_ID,
    direction: 'out', transportStatus: 'sent',
    smtpMessageId: `out-self-${Date.now()}@host`,
    inReplyTo: null, references: [],
    subject: 'hello',
    fromAddr: `${ME_USER_ID} <${ME_USER_ID}@test.local>`,
    toAddrs: [agentEmail],
    body: 'world!',
  })

  // Now post a follow-up via the chat input. Should NOT 500.
  const res = await fetch(`${baseUrl}/api/conversations/${encodeURIComponent(conv.conversationId)}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': companyId },
    body: JSON.stringify({ body: '?' }),
  })
  assert.equal(res.status, 202, `follow-up to own thread must succeed; got ${res.status}`)
  const payload = await res.json() as { id: string }

  // Verify the follow-up's TO list contains the agent — same target as
  // the parent outbound, not an empty list.
  const { rows } = await pool.query<{ to_addrs: string[] }>(
    `SELECT to_addrs FROM email_messages WHERE message_id = $1`,
    [payload.id],
  )
  assert.ok(rows[0].to_addrs.some((a) => a.includes(agentEmail)),
    `follow-up TO should preserve the parent's recipients, got: ${JSON.stringify(rows[0].to_addrs)}`)
})

// (TS agent-CLI 入口的两条 auto-promote 用例随 #70 运行时退役删除;HTTP 面
// auto-promote 用例保留。Go 侧 /runtime/cli 的 reply-in-email-convo 路径
// (cli_reply.go,autoSubmitted=true 分支)暂无直测——补盖缺口挂在 #118。)

test('[integration] non-email conversation POST still writes a kind=text row (regression)', async () => {
  // Auto-promote must NOT fire on group/direct/whisper conversations —
  // those are chat surfaces. Stand up a plain group convo and verify the
  // POST still lands as a kind='text' message.
  const { companyId } = await seedCompanyWithAgent()
  await seedUserMembership(ME_USER_ID, companyId)
  // Insert a minimal group conversation with the user as a member.
  const convId = `g-test-${Date.now()}`
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id, topic)
     VALUES ($1, 'group', $2, $3::jsonb, $4, $5)`,
    [convId, 'plain group', JSON.stringify([ME_USER_ID]), companyId, 'group'],
  )
  const res = await fetch(`${baseUrl}/api/conversations/${encodeURIComponent(convId)}/messages`, {
    method: 'POST',
    headers: { ...authHeaders, 'content-type': 'application/json', 'x-company-id': companyId },
    body: JSON.stringify({ body: 'just a chat message' }),
  })
  assert.equal(res.status, 202)
  const payload = await res.json() as { id: string }
  const { rows } = await pool.query<{ kind: string }>(
    `SELECT kind FROM messages WHERE id = $1`,
    [payload.id],
  )
  assert.equal(rows[0].kind, 'text', 'group convo POSTs must remain kind=text — only email convos auto-promote')
})
