/** yjs-sidecar 自持 redis 面(#142:自退役 TS server 的 redis.ts 裁剪内联)。
 *
 * 只保留 sidecar 消费的:双客户端(命令 + 阻塞订阅)、文档协同两通道、
 * publish(doc.update / doc.awareness)。全套通道表与事件类型留在
 * server 侧(集成测试 harness 仍消费那一份)。 */
import IORedis from 'ioredis'
import { env } from './env.js'

const lazyConnect = process.env.CUMORA_RUNTIME_CLIENT === 'http'

/** Single shared client for normal commands. */
export const redis = new IORedis(env.REDIS_URL, {
  maxRetriesPerRequest: null,
  enableReadyCheck: true,
  lazyConnect,
})

/** Separate connection for blocking SUBSCRIBE — required by the Redis protocol. */
export const sub = new IORedis(env.REDIS_URL, {
  maxRetriesPerRequest: null,
  enableReadyCheck: true,
  lazyConnect,
})

redis.on('error', (e) => console.error('[redis]', e))
sub.on('error', (e) => console.error('[redis sub]', e))

/* === Collaborative documents (CRDT) ===
 *
 * Yjs binary updates are base64-encoded into the JSON envelope so they
 * fan out through the same Redis bus + WS path as every other event.
 * `originId` is the WS client (or agent) that produced the update so the
 * fan-out can echo-suppress on the sender's own socket. */
export const CH_DOC_UPDATE = 'cumora:doc.update'
export const CH_DOC_AWARENESS = 'cumora:doc.awareness'

interface TenantTagged { companyId?: string }

export interface DocUpdateEvent extends TenantTagged {
  type: 'doc.update'
  documentId: string
  /** Base64-encoded Y.js update bytes (incremental, not full state). */
  updateB64: string
  /** Stable id of whatever produced this update. WS subscribers ignore
   *  events whose originId matches the id their socket carries. */
  originId: string
  /** Free-form author for activity / "agent just edited" notices. Usually
   *  a user id or agent id; may be the same as originId. */
  authorId: string
}

/** Awareness (cursors, selection, presence info) — ephemeral, not
 *  persisted. Same fan-out path as updates. */
export interface DocAwarenessEvent extends TenantTagged {
  type: 'doc.awareness'
  documentId: string
  updateB64: string
  originId: string
}

export async function publish(channel: string, event: DocUpdateEvent | DocAwarenessEvent): Promise<void> {
  await redis.publish(channel, JSON.stringify(event))
}
