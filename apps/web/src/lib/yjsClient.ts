/**
 * Browser-side Y.Doc session for a single collaborative document.
 *
 * One YDocSession per opened document. It owns a Y.Doc + Awareness
 * instance and bridges them to the existing WsClient: outbound updates
 * are emitted as `doc.update` envelopes, inbound `doc.update` /
 * `doc.sync` / `doc.awareness` frames are applied locally.
 *
 * We deliberately don't use y-websocket — the existing Cumora WS path
 * already handles auth + reconnect + tenant scoping, so an extra socket
 * would just duplicate state. Yjs binary updates are b64-wrapped so
 * they fit the JSON envelope the rest of the app speaks.
 *
 * #145 合帧:outbound update/awareness 攒 ~40ms 窗口再上送 —— 连续打字
 * 从每按键 1 帧降到每窗口 1 帧(服务端 rooms.ts 另有自己的落库合批
 * 窗口,两层叠加)。Yjs update 是 CRDT,Y.mergeUpdates 合并窗口内帧
 * 语义等价;awareness 状态可覆盖,flush 时对 dirty 客户端集一次性编码。
 */
import * as Y from 'yjs'
import { Awareness, applyAwarenessUpdate, encodeAwarenessUpdate, removeAwarenessStates } from 'y-protocols/awareness'
import { ws, type WsEvent } from '@/api/client'

function bytesToB64(bytes: Uint8Array): string {
  let binary = ''
  const chunk = 0x8000
  for (let i = 0; i < bytes.length; i += chunk) {
    binary += String.fromCharCode(...bytes.subarray(i, i + chunk))
  }
  return btoa(binary)
}

function b64ToBytes(b64: string): Uint8Array {
  const binary = atob(b64)
  const out = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i)
  return out
}

export interface YDocSession {
  doc: Y.Doc
  awareness: Awareness
  /** True after the server has sent its initial state. The UI can wait
   *  on this to avoid showing an empty editor for a brief moment before
   *  the doc loads. */
  synced: Promise<void>
  /** Tear down listeners + flush an awareness-clear so other clients
   *  drop the local cursor immediately. */
  destroy: () => void
}

export interface OpenDocumentOptions {
  documentId: string
  /** Free-form identity stamped on awareness so peers can render
   *  "<name> is editing". Typically the user's display name. */
  user: { id: string; name: string; color: string }
}

export function openDocument(opts: OpenDocumentOptions): YDocSession {
  const { documentId, user } = opts
  const doc = new Y.Doc()
  // The default top-level Y.Text used by the editor binding. Same key
  // the server-side agent tools use so an agent edit shows up here.
  doc.getText('content')

  const awareness = new Awareness(doc)
  awareness.setLocalState({ user: { id: user.id, name: user.name, color: user.color } })

  let resolveSynced: () => void = () => { /* assigned below */ }
  const synced = new Promise<void>((r) => { resolveSynced = r })

  // Outbound: any local update goes upstream. Origin `remote` means the
  // update came in via the WS path — skip echoing it back. Local edits
  // coalesce into a short window (#145); the merged batch is what Yjs
  // semantics make order-independent, so one frame per window is enough.
  const COALESCE_MS = 40
  const RETRY_MS = 1_000

  let pendingUpdate: Uint8Array | null = null
  let updateTimer: ReturnType<typeof setTimeout> | null = null
  let updateRetryTimer: ReturnType<typeof setTimeout> | null = null

  const flushUpdate = (): void => {
    if (updateTimer != null) { clearTimeout(updateTimer); updateTimer = null }
    const merged = pendingUpdate
    if (merged == null) return
    const sent = ws.send({ type: 'doc.update', documentId, updateB64: bytesToB64(merged) })
    if (sent) {
      pendingUpdate = null
      if (updateRetryTimer != null) { clearTimeout(updateRetryTimer); updateRetryTimer = null }
      return
    }
    // WsClient.send drops silently while the socket is closed — KEEP the
    // batch and retry ('hello' re-flushes too), instead of losing edits
    // typed through a reconnect flap.
    if (updateRetryTimer == null) updateRetryTimer = setTimeout(flushUpdate, RETRY_MS)
  }

  const handleUpdate = (update: Uint8Array, origin: unknown) => {
    if (origin === 'remote') return
    pendingUpdate = pendingUpdate ? Y.mergeUpdates([pendingUpdate, update]) : update
    if (updateTimer == null) updateTimer = setTimeout(flushUpdate, COALESCE_MS)
  }
  doc.on('update', handleUpdate)

  // Awareness is ephemeral and last-write-wins per client — collect the
  // dirty client set through the window, encode once at flush time.
  const dirtyClients = new Set<number>()
  let awarenessTimer: ReturnType<typeof setTimeout> | null = null

  const flushAwareness = (): void => {
    if (awarenessTimer != null) { clearTimeout(awarenessTimer); awarenessTimer = null }
    if (dirtyClients.size === 0) return
    const clients = [...dirtyClients]
    dirtyClients.clear()
    const update = encodeAwarenessUpdate(awareness, clients)
    // A dropped frame is fine — peers just keep a stale cursor until the
    // next local change or the 'hello' re-broadcast.
    ws.send({ type: 'doc.awareness', documentId, updateB64: bytesToB64(update) })
  }

  const handleAwarenessChange = (
    changes: { added: number[]; updated: number[]; removed: number[] },
    origin: unknown,
  ) => {
    if (origin === 'remote') return
    for (const c of changes.added) dirtyClients.add(c)
    for (const c of changes.updated) dirtyClients.add(c)
    for (const c of changes.removed) dirtyClients.add(c)
    if (dirtyClients.size === 0) return
    if (awarenessTimer == null) awarenessTimer = setTimeout(flushAwareness, COALESCE_MS)
  }
  awareness.on('update', handleAwarenessChange)

  const subscribe = () => {
    // The server replies with `doc.sync` carrying the encoded state.
    if (ws.isOpen()) {
      ws.send({ type: 'doc.subscribe', documentId })
    }
  }

  const onWsEvent = (e: WsEvent) => {
    if (e.type === 'hello') {
      // Reconnected — re-subscribe FIRST, then push any batch that queued
      // through the outage, and re-broadcast our awareness so peers
      // refresh our cursor on the new socket. Order matters: the gateway
      // silently drops doc.update for connections that haven't (re-)
      // subscribed yet ("必须先订阅"), so flushing before subscribe would
      // lose the batch for good. doc.sync vs. the flushed update may land
      // in either order — both converge (CRDT, idempotent apply).
      subscribe()
      flushUpdate()
      dirtyClients.clear()
      if (awarenessTimer != null) { clearTimeout(awarenessTimer); awarenessTimer = null }
      const clientId = doc.clientID
      const update = encodeAwarenessUpdate(awareness, [clientId])
      ws.send({
        type: 'doc.awareness',
        documentId,
        updateB64: bytesToB64(update),
      })
      return
    }
    if (e.type === 'doc.sync' && e.documentId === documentId) {
      Y.applyUpdate(doc, b64ToBytes(e.stateB64), 'remote')
      resolveSynced()
      return
    }
    if (e.type === 'doc.update' && e.documentId === documentId) {
      Y.applyUpdate(doc, b64ToBytes(e.updateB64), 'remote')
      return
    }
    if (e.type === 'doc.awareness' && e.documentId === documentId) {
      applyAwarenessUpdate(awareness, b64ToBytes(e.updateB64), 'remote')
      return
    }
  }

  const off = ws.on(onWsEvent)
  // Best-effort tail flush when the page goes away (mobile-friendly,
  // bfcache-compatible; anything still unsent is recovered by the
  // retry/hello path if the page lives on).
  const onPageHide = () => {
    flushUpdate()
    flushAwareness()
  }
  window.addEventListener('pagehide', onPageHide)
  // Kick off the connection if it isn't already open, then send subscribe.
  void ws.connect().then(() => subscribe())

  return {
    doc,
    awareness,
    synced,
    destroy() {
      // 尾帧先冲:窗口内攒着的编辑不能随组件卸载丢失。若此刻恰好断线
      // (send 失败),显式弃置 —— 重试定时器若不清会每秒自续、并在日后
      // 全局 ws 重连时把已销毁会话的尾帧迟到送到服务端。
      flushUpdate()
      flushAwareness()
      if (updateRetryTimer != null) { clearTimeout(updateRetryTimer); updateRetryTimer = null }
      pendingUpdate = null
      window.removeEventListener('pagehide', onPageHide)
      off()
      doc.off('update', handleUpdate)
      awareness.off('update', handleAwarenessChange)
      // Clear local awareness so peers see us leave.
      removeAwarenessStates(awareness, [doc.clientID], 'local')
      if (ws.isOpen()) {
        ws.send({ type: 'doc.unsubscribe', documentId })
      }
      doc.destroy()
    },
  }
}

