/**
 * Yjs 协同文档 sidecar 入口(#50 · ADR 0004)。
 *
 * rooms/markdown 自 server/src/documents 原样平移;infra(#142)已内联自持
 * (src/infra/),不再穿透引用 server/src。本进程独占 Y.Doc 房间与持久化,
 * server 的 WS 桥经 127.0.0.1 内表面转发(见 http.ts 的协议注释)。
 * Redis CH_DOC_UPDATE/AWARENESS 仍是跨实例扇出通道——server 侧 relay
 * 订阅同通道把事件推回各 WS 客户端。
 */
import 'dotenv/config'
import { env } from './infra/env.js'
import { pool } from './infra/pool.js'
import { bootDocumentBus, flushAllPending } from './rooms.js'
import { startSidecarHttp } from './http.js'

async function main() {
  if (!env.YJS_SIDECAR_TOKEN) {
    console.error('[yjs-sidecar] YJS_SIDECAR_TOKEN 未配置 —— 拒绝启动(内表面鉴权缺失)')
    process.exit(1)
  }
  // 房间冷加载前先暖一次 pg(失败即退:sidecar 无 pg 不可用)
  await pool.query('SELECT 1')
  bootDocumentBus()
  await startSidecarHttp(env.YJS_SIDECAR_PORT)
  const shutdown = async (sig: string): Promise<void> => {
    console.log(`[yjs-sidecar] ${sig} — flushing pending batches, exiting`)
    // #145:合批窗口内的尾帧必须在关连接池前落库,否则 SIGTERM 丢尾部编辑。
    try { await flushAllPending() } catch { /* ignore */ }
    try { await pool.end() } catch { /* ignore */ }
    process.exit(0)
  }
  process.on('SIGINT', () => void shutdown('SIGINT'))
  process.on('SIGTERM', () => void shutdown('SIGTERM'))
}

main().catch((err) => {
  console.error('[yjs-sidecar] fatal', err)
  process.exit(1)
})
