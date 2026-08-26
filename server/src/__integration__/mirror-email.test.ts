/**
 * 验收镜像 · 邮件域出站半边(#58):send / html / reply。
 * 双跑:CUMORA_MIRROR_BASE 指向 Go 候选(须 CUMORA_GO_FAKE_AUTH=1)。
 * 运行环境须 RESEND_API_KEY 为空(mock 模式,集成 runner 默认强制)。
 * 入站 webhook(HMAC)与重试/GC 任务随同票后续面补测。
 */

import assert from 'node:assert/strict'
import { after, beforeEach, test } from 'node:test'
import { pool } from '../db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror,teardownAll, 
} from './_helpers.js'

const USER = 'u-mirror-email'
const COMPANY = 'c-mirror-email'

async function seedCompanyAndUser(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Email Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
}

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUser()
})

after(async () => { await mirror.close(); await teardownAll() })

test('[mirror] email send (mock): external recipient, thread row + wake shape', async () => {
  const r = await call('/email/send', {
    method: 'POST',
    body: JSON.stringify({ to: ['wey@example.com'], subject: 'Hello 世界', body: 'first body' }),
  })
  assert.equal(r.status, 200)
  assert.equal(r.json.transportStatus, 'sent')
  assert.equal(r.json.mock, true)
  assert.ok(r.json.conversationId.startsWith('email-'))

  const conv = await pool.query(`SELECT kind, title, members FROM conversations WHERE id = $1`, [r.json.conversationId])
  assert.equal(conv.rows[0].kind, 'email')
  assert.equal(conv.rows[0].title, 'Hello 世界')
  assert.deepEqual(conv.rows[0].members, [USER])

  const em = await pool.query(
    `SELECT direction, transport_status, from_addr, to_addrs, subject FROM email_messages WHERE message_id = $1`,
    [r.json.messageId],
  )
  assert.equal(em.rows[0].direction, 'out')
  assert.equal(em.rows[0].transport_status, 'sent')
  assert.equal(em.rows[0].from_addr, `${USER} <${USER}.${COMPANY}@cumora.local>`)
  assert.deepEqual(em.rows[0].to_addrs, ['wey@example.com'])
  const msg = await pool.query(`SELECT kind, author_id FROM messages WHERE id = $1`, [r.json.messageId])
  assert.equal(msg.rows[0].kind, 'email')
  assert.equal(msg.rows[0].author_id, USER)
})

test('[mirror] email send: agent participant resolves + joins conversation', async () => {
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, initial, avatar_bg, status)
     VALUES ('ag-email-1', $1, 'agent', 'Aurora', 'A', '#123456', 'offline')`,
    [COMPANY],
  )
  const r = await call('/email/send', {
    method: 'POST',
    body: JSON.stringify({ to: ['ag-email-1'], subject: 'in-house', body: 'b' }),
  })
  assert.equal(r.status, 200)
  const conv = await pool.query(`SELECT members FROM conversations WHERE id = $1`, [r.json.conversationId])
  assert.ok(conv.rows[0].members.includes(USER))
  assert.ok(conv.rows[0].members.includes('ag-email-1'))
  const em = await pool.query(`SELECT to_addrs FROM email_messages WHERE message_id = $1`, [r.json.messageId])
  // 惰铸地址:<safeLocal(id)>.<slug>@EMAIL_DOMAIN
  assert.deepEqual(em.rows[0].to_addrs, ['Aurora <ag-email-1.c-mirror-email@cumora.local>'])
})

test('[mirror] email send validation matrix', async () => {
  assert.equal((await call('/email/send', { method: 'POST', body: JSON.stringify({ subject: 's', body: 'b' }) })).status, 400)
  assert.equal((await call('/email/send', { method: 'POST', body: JSON.stringify({ to: ['x@y.z'], body: 'b' }) })).status, 400)
  assert.equal((await call('/email/send', { method: 'POST', body: JSON.stringify({ to: ['x@y.z'], subject: 's' }) })).status, 400)
  const unres = await call('/email/send', {
    method: 'POST', body: JSON.stringify({ to: ['external:x@y.z'], subject: 's', body: 'b' }),
  })
  assert.equal(unres.status, 400)
  assert.match(unres.json.error, /unresolved recipient/)
  const tooMany = await call('/email/send', {
    method: 'POST',
    body: JSON.stringify({
      to: ['x@y.z'], subject: 's', body: 'b',
      attachments: Array.from({ length: 17 }, () => ({ key: 'k', filename: 'f' })),
    }),
  })
  assert.equal(tooMany.status, 400)
  assert.match(tooMany.json.error, /too many attachments/)
  const noKey = await call('/email/send', {
    method: 'POST',
    body: JSON.stringify({ to: ['x@y.z'], subject: 's', body: 'b', attachments: [{ filename: 'f' }] }),
  })
  assert.equal(noKey.status, 400)
  assert.match(noKey.json.error, /key \+ filename/)
})

test('[mirror] email html: 404 unknown / 204 no html / sanitized html + CSP / 403 non-member', async () => {
  assert.equal((await call('/email/m-none/html')).status, 404)

  // 直接种一行带 html 的 email_messages(入站形态)
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members) VALUES ('conv-em-1',$1,'email','t',$2::jsonb)`,
    [COMPANY, JSON.stringify([USER])],
  )
  const html = '<p>hi</p><script>alert(1)</script><img src="javascript:alert(2)"><a href="jAvAsCrIpT:x">x</a>'
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
     VALUES ('m-em-html','conv-em-1',$1,'email','plain',1,$2)`,
    [USER, COMPANY],
  )
  await pool.query(
    `INSERT INTO email_messages (message_id, conversation_id, company_id, direction, transport_status, subject, from_addr, to_addrs, html)
     VALUES ('m-em-html','conv-em-1',$1,'in','received','s','ext@example.com','[]'::jsonb,$2)`,
    [COMPANY, html],
  )
  const direct = await fetch(`${mirror.baseUrl()}/api/email/m-em-html/html`, {
    headers: { 'x-test-user': USER, 'x-company-id': COMPANY },
  })
  const text = await direct.text()
  assert.match(direct.headers.get('content-type') ?? '', /text\/html/)
  assert.match(direct.headers.get('content-security-policy') ?? '', /default-src 'none'/)
  assert.ok(!text.includes('<script'), 'script tag must be stripped')
  assert.ok(!/javascript:/i.test(text.replace(/jAvAsCrIpT:x/, '')) === false || true)
  assert.ok(!text.toLowerCase().includes('javascript:'), 'javascript: scheme must be neutralized')

  const noHtml = await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
     VALUES ('m-em-plain','conv-em-1',$1,'email','p',2,$2)`,
    [USER, COMPANY],
  )
  assert.ok(noHtml)
  await pool.query(
    `INSERT INTO email_messages (message_id, conversation_id, company_id, direction, transport_status, subject, from_addr, to_addrs)
     VALUES ('m-em-plain','conv-em-1',$1,'in','received','s','ext@example.com','[]'::jsonb)`,
    [COMPANY],
  )
  assert.equal((await call('/email/m-em-plain/html')).status, 204)

  // 非成员 403
  const other = 'u-mirror-email-2'
  await pool.query(`INSERT INTO users (id, email, display_name) VALUES ($1,'e2@test.local','E2')`, [other])
  await pool.query(`INSERT INTO company_members (company_id, user_id, role) VALUES ($1,$2,'member')`, [COMPANY, other])
  const mirror2 = startMirror(other, COMPANY)
  try {
    await mirror2.call('/email/m-none/html') // 预热:startMirror 的 ready 是异步的
    const forbidden = await fetch(`${mirror2.baseUrl()}/api/email/m-em-html/html`, {
      headers: { 'x-test-user': other, 'x-company-id': COMPANY },
    })
    assert.equal(forbidden.status, 403)
  } finally {
    await mirror2.close()
  }
})

test('[mirror] email reply: threading headers, Re: subject, self-dedup', async () => {
  // 原始入站邮件:外部发件人 → 我 + agent
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members) VALUES ('conv-em-2',$1,'email','Original subject',$2::jsonb)`,
    [COMPANY, JSON.stringify([USER])],
  )
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
     VALUES ('m-em-orig','conv-em-2',$1,'email','orig',1,$2)`,
    [USER, COMPANY],
  )
  await pool.query(
    `INSERT INTO email_messages (message_id, conversation_id, company_id, direction, transport_status,
       smtp_message_id, references_chain, subject, from_addr, to_addrs, cc_addrs)
     VALUES ('m-em-orig','conv-em-2',$1,'in','received','orig-smtp-id@x','[]'::jsonb,'Original subject',
       'Wey <wey@example.com>',$2::jsonb,$3::jsonb)`,
    [COMPANY, JSON.stringify([`${USER}.${COMPANY}@cumora.local`]), JSON.stringify(['cc-person@example.com'])],
  )
  const r = await call('/email/reply/m-em-orig', {
    method: 'POST', body: JSON.stringify({ body: 'reply body' }),
  })
  assert.equal(r.status, 200)
  assert.equal(r.json.conversationId, 'conv-em-2')
  const em = await pool.query(
    `SELECT subject, in_reply_to, references_chain, to_addrs, cc_addrs FROM email_messages WHERE message_id = $1`,
    [r.json.messageId],
  )
  assert.equal(em.rows[0].subject, 'Re: Original subject')
  assert.equal(em.rows[0].in_reply_to, 'orig-smtp-id@x')
  assert.deepEqual(em.rows[0].references_chain, ['orig-smtp-id@x'])
  // TO = 原 From;CC = 原 CC(原 To 中的 self 已去重)
  assert.deepEqual(em.rows[0].to_addrs, ['Wey <wey@example.com>'])
  assert.deepEqual(em.rows[0].cc_addrs, ['cc-person@example.com'])

  // reply 无可回对象(原始 From 是自己)→ 400
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members) VALUES ('conv-em-3',$1,'email','s2',$2::jsonb)`,
    [COMPANY, JSON.stringify([USER])],
  )
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
     VALUES ('m-em-self','conv-em-3',$1,'email','x',1,$2)`,
    [USER, COMPANY],
  )
  await pool.query(
    `INSERT INTO email_messages (message_id, conversation_id, company_id, direction, transport_status, subject, from_addr, to_addrs)
     VALUES ('m-em-self','conv-em-3',$1,'in','received','s',$2,$3::jsonb)`,
    [COMPANY, `${USER}.${COMPANY}@cumora.local`, JSON.stringify(['wey@example.com'])],
  )
  const r2 = await call('/email/reply/m-em-self', { method: 'POST', body: JSON.stringify({ body: 'x' }) })
  assert.equal(r2.status, 400)
  assert.equal(r2.json.error, 'no other recipients to reply to')
})
