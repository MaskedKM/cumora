/**
 * Server 侧文档 relay —— sidecar 进程边界的对端(#50 · ADR 0004)。
 *
 * 职责:①持有本 server 实例的 WS 订阅表,把 sidecar 经 Redis
 * CH_DOC_UPDATE/CH_DOC_AWARENESS 扇出的事件按 originId 回声抑制后推给
 * 各 WS 客户端;②把客户端/agent CLI 的房间操作经 sidecar 内表面 HTTP
 * 转发(协议契约见 apps/yjs-sidecar/src/http.ts 头注释)。
 *
 * 对外 API 形状刻意对齐原 rooms.ts(ws.ts/cli.ts 的调用面零改动),
 * 仅 subscribe 的 subRec 语义变为"本进程 WS 订阅"而非"Y.Doc 订阅"。
 */
import { env } from '../env.js'
import {
  sub, CH_DOC_UPDATE, CH_DOC_AWARENESS,
  type DocUpdateEvent, type DocAwarenessEvent,
} from '../redis.js'
import type { DocSubscriber } from '../../../apps/yjs-sidecar/src/rooms.js'
export type { DocSubscriber }
export {
  isAnchoredImagePlacement,
  type AgentImagePlacement,
  type AgentImageDeleteMatch,
} from '../../../apps/yjs-sidecar/src/markdown.js'

const wsSubs = new Map<string, Set<DocSubscriber>>()
/** 每 (documentId) 在 sidecar 的订阅引用计数>0 即保持;归零时注销。 */
const docRefcounts = new Map<string, number>()
let relayBootstrapped = false

export function instanceOrigin(): string { return `instance:${env.INSTANCE_ID}` }

async function sidecar<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(`${env.YJS_SIDECAR_URL}${path}`, {
    method: 'POST',
    headers: {
      'content-type': 'application/json',
      authorization: `Bearer ${env.YJS_SIDECAR_TOKEN}`,
    },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    const text = await res.text().catch(() => '')
    throw new Error(`sidecar ${path} → ${res.status} ${text.slice(0, 200)}`)
  }
  return res.json() as Promise<T>
}

export function bootDocRelay(): void {
  if (relayBootstrapped) return
  relayBootstrapped = true
  void sub.subscribe(CH_DOC_UPDATE, CH_DOC_AWARENESS).then(() => {
    console.log(`[docs] relay subscribed(rooms live in yjs-sidecar @ ${env.YJS_SIDECAR_URL})`)
  }).catch((e) => console.warn('[docs] relay redis subscribe failed', e))

  sub.on('message', (channel, payload) => {
    if (channel !== CH_DOC_UPDATE && channel !== CH_DOC_AWARENESS) return
    let parsed: DocUpdateEvent | DocAwarenessEvent
    try { parsed = JSON.parse(payload) } catch { return }
    const bytes = Buffer.from(parsed.updateB64, 'base64')
    const update = new Uint8Array(bytes.buffer, bytes.byteOffset, bytes.byteLength)
    const subs = wsSubs.get(parsed.documentId)
    if (!subs) return
    for (const s of subs) {
      if (s.originId === parsed.originId) continue  // originator echo
      try {
        if (channel === CH_DOC_UPDATE) s.onUpdate(update, parsed.originId)
        else s.onAwareness(update, parsed.originId)
      } catch { /* one dead sub must not break the fanout */ }
    }
  })
}

export async function subscribe(
  documentId: string,
  companyId: string,
  subRec: DocSubscriber,
): Promise<{ initialState: Uint8Array }> {
  let set = wsSubs.get(documentId)
  if (!set) { set = new Set(); wsSubs.set(documentId, set) }
  set.add(subRec)
  const ref = (docRefcounts.get(documentId) ?? 0) + 1
  docRefcounts.set(documentId, ref)
  try {
    await sidecar('/internal/doc/subscribe', {
      documentId, companyId, subscriberId: instanceOrigin(),
    })
    // 每 (doc, instance) 在 sidecar 只登记一次;新订阅者仍要全量初始
    // 状态——sidecar 读路径幂等(房间已在内存),直接再取一次。
    const r = await sidecar<{ stateB64: string }>('/internal/doc/subscribe', {
      documentId, companyId, subscriberId: instanceOrigin(),
    })
    return { initialState: new Uint8Array(Buffer.from(r.stateB64, 'base64')) }
  } catch (e) {
    // 侧失败回滚本地登记,向上抛——ws.ts 会给客户端发 doc.error
    set.delete(subRec)
    const left = (docRefcounts.get(documentId) ?? 1) - 1
    docRefcounts.set(documentId, Math.max(0, left))
    throw e
  }
}

export function unsubscribe(documentId: string, subRec: DocSubscriber): void {
  const set = wsSubs.get(documentId)
  if (!set || !set.delete(subRec)) return
  const left = (docRefcounts.get(documentId) ?? 1) - 1
  docRefcounts.set(documentId, Math.max(0, left))
  if (left === 0) {
    wsSubs.delete(documentId)
    docRefcounts.delete(documentId)
    void sidecar('/internal/doc/unsubscribe', {
      documentId, subscriberId: instanceOrigin(),
    }).catch((e) => console.warn(`[docs] sidecar unsubscribe(${documentId}) failed`, e instanceof Error ? e.message : e))
  }
}

export async function applyLocalUpdate(
  documentId: string,
  companyId: string,
  originId: string,
  userId: string,
  update: Uint8Array,
): Promise<void> {
  await sidecar('/internal/doc/update', {
    documentId, companyId, originId, userId,
    updateB64: Buffer.from(update).toString('base64'),
  })
}

export async function broadcastAwareness(
  documentId: string,
  companyId: string,
  originId: string,
  update: Uint8Array,
): Promise<void> {
  await sidecar('/internal/doc/awareness', {
    documentId, companyId, originId,
    updateB64: Buffer.from(update).toString('base64'),
  })
}

export async function readDocumentText(documentId: string, companyId: string): Promise<string> {
  const r = await sidecar<{ text: string }>('/internal/doc/read-text', { documentId, companyId })
  return r.text
}

export interface AgentEditResult {
  replaced: number
  imagePlaced: 'absolute' | 'anchor' | 'anchor-missed' | null
  imagesDeleted: number
  blocksReplaced: number
}

export async function applyAgentEdit(
  documentId: string,
  companyId: string,
  agentId: string,
  ops: Array<Record<string, unknown>>,
): Promise<AgentEditResult> {
  return sidecar<AgentEditResult>('/internal/doc/agent-edit', { documentId, companyId, agentId, ops })
}
