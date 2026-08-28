/** yjs-sidecar 自持 pg 池(#142:自 server/src/db/pool.ts 原样内联,
 * 仅改 env 导入路径——连接参数/超时语义逐字保留)。 */
import { Pool } from 'pg'
import { env } from './env.js'

export const pool = new Pool({
  connectionString: env.DATABASE_URL,
  max: 20,
  idleTimeoutMillis: 30_000,
  connectionTimeoutMillis: 5_000,
  // Defense-in-depth against connection-pool exhaustion. A single slow or stuck
  // query must never pin a pool slot indefinitely — that is exactly how one
  // un-indexed hot query (idle.ts' MAX(created_at) seq-scan) held all 20 slots
  // at ~8s each and 503-ed the entire API. 60s is far above any healthy request
  // (sub-second) but reaps genuine runaways; idle-in-transaction reaps leaked
  // transactions holding a slot open doing nothing.
  statement_timeout: 60_000,
  idle_in_transaction_session_timeout: 30_000,
})

pool.on('error', (err) => {
  console.error('[pg] idle client error', err)
})
