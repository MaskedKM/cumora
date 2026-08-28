/**
 * Email 种子切片(#70 TS 退役 harness 保留件)——原 server/src/email.ts
 * 运行时已删,这里保留测试造数所需的四个函数,行为逐字对齐原实现:
 *   findOrCreateEmailConversation / persistEmailMessage / mintMessageId /
 *   findEmailConversationByMessageIds(内部依赖)。
 * 唯一降级:persistEmailMessage 的 wake 事件里附件 url 恒 null(原实现经
 * storage.publicUrl 解析;本机模式下无测试断言该 url,而 storage 运行时
 * 已随退役删除)。DB 行为(落行形状)完全保留——Go 服读的就是这些行。
 */
import { randomUUID } from 'node:crypto'
import { pool } from '../db/pool.js'
import { env } from '../env.js'
import { CH_MESSAGE_NEW, publish } from '../redis.js'

function mintLocalMessageId(): string {
  return `${Date.now().toString(36)}-${randomUUID().replace(/-/g, '').slice(0, 22)}`
}

export function mintMessageId(): string {
  const dom = env.EMAIL_DOMAIN || 'cumora.local'
  return `${mintLocalMessageId()}@${dom}`
}

export function normalizeMessageId(raw: string | null | undefined): string | null {
  if (!raw) return null
  const trimmed = String(raw).trim().replace(/^<+|>+$/g, '').trim()
  if (!trimmed) return null
  return trimmed.toLowerCase()
}

export async function findEmailConversationByMessageIds(
  messageIds: string[],
  companyId: string,
): Promise<string | null> {
  if (messageIds.length === 0) return null
  const norm = messageIds.map(normalizeMessageId).filter((x): x is string => Boolean(x))
  if (norm.length === 0) return null
  const { rows } = await pool.query<{ conversation_id: string }>(
    `SELECT conversation_id FROM email_messages
      WHERE company_id = $1
        AND LOWER(smtp_message_id) = ANY($2::text[])
      ORDER BY created_at DESC
      LIMIT 1`,
    [companyId, norm],
  )
  return rows[0]?.conversation_id ?? null
}

export async function findOrCreateEmailConversation(args: {
  companyId: string
  inReplyTo: string | null
  references: string[]
  subject: string
  /** Participant ids that should be on the conversation. Includes the
   *  sender. Order is preserved for the title fallback ("A ↔ B"). */
  memberIds: string[]
}): Promise<{ conversationId: string; created: boolean }> {
  const candidates = [args.inReplyTo, ...args.references]
    .map((x) => normalizeMessageId(x))
    .filter((x): x is string => Boolean(x))
  const existing = await findEmailConversationByMessageIds(candidates, args.companyId)
  if (existing) {
    // Membership repair: a thread can pick up new participants over time
    // (someone CC'd later). Add anyone who isn't already a member.
    await pool.query(
      `UPDATE conversations
          SET members = (
            SELECT to_jsonb(ARRAY(
              SELECT DISTINCT m FROM (
                SELECT jsonb_array_elements_text(members) AS m
                UNION
                SELECT unnest($2::text[]) AS m
              ) u
            ))
          )
        WHERE id = $1`,
      [existing, args.memberIds],
    )
    return { conversationId: existing, created: false }
  }
  // Strip leading Re:/Fwd: for the title — easier to scan in the
  // conversation list. The full subject is preserved on each message.
  const cleanSubject = args.subject.replace(/^\s*((re|fwd|fw)\s*:\s*)+/i, '').trim() || '(no subject)'
  const id = `email-${randomUUID().slice(0, 12)}`
  const uniqueMembers = Array.from(new Set(args.memberIds))
  await pool.query(
    `INSERT INTO conversations (id, kind, title, members, company_id, topic)
     VALUES ($1, 'email', $2, $3::jsonb, $4, $5)`,
    [id, cleanSubject.slice(0, 200), JSON.stringify(uniqueMembers), args.companyId, cleanSubject.slice(0, 200)],
  )
  return { conversationId: id, created: true }
}

export async function persistEmailMessage(args: {
  conversationId: string
  companyId: string
  /** Participant id authoring this message — agent for outbound, agent or
   *  human for inbound from a recognized sender, or a synthetic
   *  `external:<addr>` id for unknown senders. */
  authorId: string
  direction: 'in' | 'out'
  transportStatus: 'queued' | 'sending' | 'sent' | 'failed' | 'received'
  transportError?: string | null
  smtpMessageId: string | null
  inReplyTo: string | null
  references: string[]
  subject: string
  fromAddr: string
  toAddrs: string[]
  ccAddrs?: string[]
  bccAddrs?: string[]
  /** Body the agent / renderer reads. Plain text always, even when the
   *  source was HTML — caller strips tags before passing in. */
  body: string
  html?: string | null
  rawSizeBytes?: number | null
  autoSubmitted?: boolean
  attachments?: Array<{
    filename: string
    mimeType: string
    sizeBytes: number
    storageKey: string | null
    truncated?: boolean
  }>
}): Promise<{ messageId: string; sequence: number }> {
  const messageId = `m-${randomUUID()}`
  // Atomic sequence claim — same pattern as cmdReply / api/router.ts.
  const seqResult = await pool.query<{ seq: number }>(
    `INSERT INTO conversation_counters (conversation_id, next_sequence)
     VALUES ($1, 2)
     ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
     RETURNING next_sequence - 1 AS seq`,
    [args.conversationId],
  )
  const sequence = seqResult.rows[0]?.seq ?? 1
  // No need to stash headers on the messages row — the API joins on
  // email_messages and emits a typed `email` field per row when kind='email'.
  await pool.query(
    `INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
     VALUES ($1, $2, $3, 'email', $4, $5, $6)`,
    [messageId, args.conversationId, args.authorId, args.body, sequence, args.companyId],
  )
  // Schedule the first retry attempt for outbound failures. The retry
  // worker only ever looks at direction='out' + status='failed' rows whose
  // next_retry_at is in the past, so inbound + sent rows trivially get
  // ignored. We use a fresh 60-second backoff for the first try; the
  // worker compounds it on each subsequent miss.
  const initialRetryAt = (args.direction === 'out' && args.transportStatus === 'failed')
    ? new Date(Date.now() + 60_000)
    : null
  await pool.query(
    `INSERT INTO email_messages (
        message_id, conversation_id, company_id, direction, transport_status,
        transport_error, smtp_message_id, in_reply_to, references_chain,
        subject, from_addr, to_addrs, cc_addrs, bcc_addrs, html, raw_size_bytes,
        auto_submitted, next_retry_at
     ) VALUES (
        $1, $2, $3, $4, $5,
        $6, $7, $8, $9::jsonb,
        $10, $11, $12::jsonb, $13::jsonb, $14::jsonb, $15, $16,
        $17, $18
     )`,
    [
      messageId, args.conversationId, args.companyId, args.direction, args.transportStatus,
      args.transportError ?? null,
      normalizeMessageId(args.smtpMessageId),
      normalizeMessageId(args.inReplyTo),
      JSON.stringify(args.references.map((r) => normalizeMessageId(r)).filter(Boolean)),
      args.subject.slice(0, 1000),
      args.fromAddr.slice(0, 320),
      JSON.stringify(args.toAddrs.slice(0, 64)),
      JSON.stringify((args.ccAddrs ?? []).slice(0, 64)),
      JSON.stringify((args.bccAddrs ?? []).slice(0, 64)),
      args.html ?? null,
      args.rawSizeBytes ?? null,
      Boolean(args.autoSubmitted),
      initialRetryAt,
    ],
  )
  const persistedAttachments: Array<{
    id: string; filename: string; mimeType: string; sizeBytes: number;
    storageKey: string | null; truncated: boolean;
  }> = []
  if (args.attachments?.length) {
    for (const a of args.attachments) {
      const attId = `eatt-${randomUUID().slice(0, 12)}`
      const filename = a.filename.slice(0, 200)
      const mimeType = (a.mimeType || 'application/octet-stream').slice(0, 120)
      const truncated = Boolean(a.truncated)
      await pool.query(
        `INSERT INTO email_attachments
           (id, message_id, conversation_id, company_id, filename, mime_type, size_bytes, storage_key, truncated)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
        [
          attId,
          messageId, args.conversationId, args.companyId,
          filename, mimeType, a.sizeBytes,
          a.storageKey, truncated,
        ],
      )
      persistedAttachments.push({
        id: attId, filename, mimeType, sizeBytes: a.sizeBytes,
        storageKey: a.storageKey, truncated,
      })
    }
  }
  await pool.query(`UPDATE conversations SET updated_at = NOW() WHERE id = $1`, [args.conversationId])
  // Wake every member-agent of the conversation. The scheduler dedups
  // across replicas via the wake-claim Redis key. Attachment urls in the
  // wake payload degrade to null (see file header).
  await publish(CH_MESSAGE_NEW, {
    type: 'message.new',
    conversationId: args.conversationId,
    companyId: args.companyId,
    message: {
      id: messageId,
      conversationId: args.conversationId,
      authorId: args.authorId,
      kind: 'email',
      body: args.body,
      sequence,
      at: new Date().toISOString(),
      email: {
        subject: args.subject,
        from: args.fromAddr,
        to: args.toAddrs,
        cc: args.ccAddrs ?? [],
        direction: args.direction,
        transportStatus: args.transportStatus,
        transportError: args.transportError ?? null,
        smtpMessageId: normalizeMessageId(args.smtpMessageId),
        inReplyTo: normalizeMessageId(args.inReplyTo),
        hasHtml: Boolean(args.html),
        autoSubmitted: Boolean(args.autoSubmitted),
        attachments: persistedAttachments.map((a) => ({
          id: a.id, filename: a.filename, mimeType: a.mimeType, sizeBytes: a.sizeBytes,
          url: null, truncated: a.truncated,
        })),
      },
    },
  })
  return { messageId, sequence }
}
