/**
 * Yjs sidecar 内表面 —— server↔sidecar 进程边界协议(#50 · ADR 0004)。
 *
 * 仅绑 127.0.0.1;Bearer YJS_SIDECAR_TOKEN 鉴权(与 server 共享,env 下发)。
 * 协议契约(变更须同步 packages/contract 的 info 与本注释):
 *   GET  /internal/healthz                                  → {ok}
 *   POST /internal/doc/subscribe   {documentId, companyId, subscriberId}
 *                                  → {stateB64}(幂等;subscriberId 计数引用)
 *   POST /internal/doc/unsubscribe {documentId, subscriberId}          → {ok}
 *   POST /internal/doc/update      {documentId, companyId, originId, userId, updateB64}
 *                                  → {ok}(sidecar 应用→持久化→Redis 扇出)
 *   POST /internal/doc/awareness   {documentId, companyId, originId, updateB64}
 *                                  → {ok}(仅 Redis 扇出,不落库)
 *   POST /internal/doc/read-text   {documentId, companyId}             → {text}
 *   POST /internal/doc/agent-edit  {documentId, companyId, agentId, ops[]}
 *                                  → {replaced, imagePlaced, imagesDeleted, blocksReplaced}
 *
 * 重启语义:sidecar 无状态(状态=Y.Doc 内存房间,冷加载自 pg 快照+增量,
 * 由 rooms.ts 既有逻辑承担)——server 端遇 5xx/ECONNREFUSED 应回 doc.error
 * 帧并可在 sidecar 恢复后重订阅;server 侧 relay 不缓存状态。
 */
import http from 'node:http'
import { env } from '../../../server/src/env.js'
import {
  subscribe,
  unsubscribe,
  applyLocalUpdate,
  broadcastAwareness,
  readDocumentText,
  applyAgentEdit,
} from './rooms.js'

interface SubBody {
  documentId: string
  companyId: string
  subscriberId: string
}

/** sidecar 侧订阅代理表:rooms.unsubscribe 按对象寻址,subscribe 时按
 *  (documentId, subscriberId) 记下惰性代理对象(空回调——扇出走 Redis),
 *  unsubscribe 用同一对象归还引用。 */
const proxies = new Map<string, { originId: string; onUpdate: () => void; onAwareness: () => void }>()
const proxyKey = (d: string, s2: string): string => `${d}\u0000${s2}`

function forgetProxy(documentId: string, subscriberId: string): boolean {
  const proxy = proxies.get(proxyKey(documentId, subscriberId))
  if (!proxy) return true // 幂等:不存在的订阅视为已注销
  proxies.delete(proxyKey(documentId, subscriberId))
  unsubscribe(documentId, proxy)
  return true
}

export function startSidecarHttp(port: number, host = '127.0.0.1'): Promise<() => void> {
  return new Promise((resolve) => {
    const server = http.createServer(async (req, res) => {
      const send = (code: number, body: unknown): void => {
        res.writeHead(code, { 'content-type': 'application/json' })
        res.end(JSON.stringify(body))
      }
      try {
        if (!env.YJS_SIDECAR_TOKEN) { send(503, { error: 'sidecar token not configured' }); return }
        const auth = req.headers.authorization ?? ''
        if (auth !== `Bearer ${env.YJS_SIDECAR_TOKEN}`) { send(401, { error: 'unauthorized' }); return }

        if (req.method === 'GET' && req.url === '/internal/healthz') { send(200, { ok: true }); return }

        if (req.method !== 'POST' || !req.url?.startsWith('/internal/doc/')) {
          send(404, { error: 'not found' }); return
        }
        const chunks: Buffer[] = []
        for await (const c of req) chunks.push(c as Buffer)
        const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString('utf8')) : {}

        switch (req.url) {
          case '/internal/doc/subscribe': {
            const b = body as SubBody
            // 幂等:同 (doc, subscriberId) 复用同一 proxy——rooms.subs 按
            // 对象地址去重,重复 subscribe 不增计数;unsubscribe 才真归还。
            let proxy = proxies.get(proxyKey(b.documentId, b.subscriberId))
            if (!proxy) {
              proxy = {
                originId: b.subscriberId,
                onUpdate: () => { /* fanout happens via Redis CH_DOC_UPDATE */ },
                onAwareness: () => { /* via CH_DOC_AWARENESS */ },
              }
              proxies.set(proxyKey(b.documentId, b.subscriberId), proxy)
            }
            const { initialState } = await subscribe(b.documentId, b.companyId, proxy)
            send(200, { stateB64: Buffer.from(initialState).toString('base64') })
            return
          }
          case '/internal/doc/unsubscribe': {
            const b = body as { documentId: string; subscriberId: string }
            // rooms.unsubscribe 按订阅对象寻址;sidecar 侧按 subscriberId 维持一份
            // 惰性代理(空回调)即可——unsubscribe 需要同一对象,这里查表。
            send(200, { ok: forgetProxy(b.documentId, b.subscriberId) })
            return
          }
          case '/internal/doc/update': {
            const b = body as { documentId: string; companyId: string; originId: string; userId: string; updateB64: string }
            const buf = Buffer.from(b.updateB64, 'base64')
            const update = new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength)
            await applyLocalUpdate(b.documentId, b.companyId, b.originId, b.userId, update)
            send(200, { ok: true })
            return
          }
          case '/internal/doc/awareness': {
            const b = body as { documentId: string; companyId: string; originId: string; updateB64: string }
            const buf = Buffer.from(b.updateB64, 'base64')
            const update = new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength)
            await broadcastAwareness(b.documentId, b.companyId, b.originId, update)
            send(200, { ok: true })
            return
          }
          case '/internal/doc/read-text': {
            const b = body as { documentId: string; companyId: string }
            send(200, { text: await readDocumentText(b.documentId, b.companyId) })
            return
          }
          case '/internal/doc/agent-edit': {
            const b = body as Parameters<typeof applyAgentEdit>[3] extends (infer _O)[] | undefined
              ? { documentId: string; companyId: string; agentId: string; ops: Array<Record<string, unknown>> }
              : never
            send(200, await applyAgentEdit(b.documentId, b.companyId, b.agentId, b.ops as never))
            return
          }
          default:
            send(404, { error: 'not found' })
        }
      } catch (e) {
        send(e instanceof Error && (e as { status?: number }).status === 404 ? 404 : 500, {
          error: e instanceof Error ? e.message : String(e),
        })
      }
    })
    server.listen(port, host, () => {
      console.log(`[yjs-sidecar] internal http on ${host}:${port} · rooms bus active`)
      resolve(() => new Promise<void>((res) => server.close(() => res())))
    })
  })
}
