#!/usr/bin/env node
/**
 * 双跑灰度反代(#68)——切换日前的最后防线 harness 的前门。
 *
 * 一个零依赖 node http 反代:前门一个端口,TS(:5181)与 Go(:5190)双
 * 后端并行在线,分流可控:
 *
 *   分流模式(DUAL_SPLIT,默认 round-robin):
 *     - header   :请求头 x-backend: go|ts 钉死后端(测试/灰度探针用)
 *     - round-robin:每请求交替(交错写共享 DB/Redis 的撕裂验证)
 *     - N%(如 30%):按比例随机走 Go,其余走 TS(灰度放量)
 *     - go|ts    :全量单侧(切换日/回切演练)
 *
 *   SSE(/runtime/wake-stream)与 WS(/ws)原样管道转发——分流决策只在
 *   建连时做一次,连接有粘性(同一连接不换后端,符合协议语义)。
 *
 *   响应头回带 x-backend:<go|ts>,验证脚本与差异报告靠它归因。
 *
 * 用法:
 *   DUAL_TS=http://127.0.0.1:5181 DUAL_GO=http://127.0.0.1:5190 \
 *   DUAL_FRONT=:5180 DUAL_SPLIT=round-robin node scripts/dual-backend/proxy.mjs
 */
import http from 'node:http'

const TS = process.env.DUAL_TS ?? 'http://127.0.0.1:5181'
const GO = process.env.DUAL_GO ?? 'http://127.0.0.1:5190'
const FRONT = process.env.DUAL_FRONT ?? '127.0.0.1:5180'
const SPLIT = (process.env.DUAL_SPLIT ?? 'round-robin').toLowerCase()

let rrFlip = false
function pickBackend(req) {
  const pinned = req.headers['x-backend']
  if (SPLIT === 'header') {
    if (pinned === 'go') return 'go'
    if (pinned === 'ts') return 'ts'
    return 'ts'
  }
  if (SPLIT === 'go' || SPLIT === 'ts') return SPLIT
  if (SPLIT.endsWith('%')) {
    const pct = Number.parseFloat(SPLIT)
    return Math.random() * 100 < pct ? 'go' : 'ts'
  }
  if (SPLIT === 'round-robin') {
    rrFlip = !rrFlip
    return rrFlip ? 'go' : 'ts'
  }
  return 'ts'
}

function target(name) {
  return name === 'go' ? GO : TS
}

const server = http.createServer((req, res) => {
  const backend = pickBackend(req)
  const url = new URL(req.url ?? '/', target(backend))
  const upstream = http.request(
    url,
    {
      method: req.method,
      headers: { ...req.headers, host: url.host },
    },
    (ures) => {
      res.writeHead(ures.statusCode ?? 502, {
        ...ures.headers,
        'x-backend': backend,
      })
      ures.pipe(res)
    },
  )
  upstream.on('error', (err) => {
    if (!res.headersSent) {
      res.writeHead(502, { 'content-type': 'application/json', 'x-backend': backend })
    }
    res.end(JSON.stringify({ error: `upstream ${backend} failed`, detail: String(err) }))
  })
  req.pipe(upstream)
})

// 升级(WS)分流:同一连接粘一个后端。
server.on('upgrade', (req, socket, head) => {
  const backend = pickBackend(req)
  const url = new URL(req.url ?? '/', target(backend))
  const upstream = http.request({
    protocol: 'http:',
    hostname: url.hostname,
    port: url.port,
    path: url.pathname + url.search,
    method: 'GET',
    headers: { ...req.headers, host: url.host, connection: 'Upgrade', upgrade: 'websocket' },
  })
  upstream.on('upgrade', (ures, usocket, uhead) => {
    socket.write(
      `HTTP/1.1 101 Switching Protocols\r\n` +
        Object.entries({ ...ures.headers, 'x-backend': backend })
          .map(([k, v]) => `${k}: ${Array.isArray(v) ? v.join(', ') : v}`)
          .join('\r\n') + '\r\n\r\n',
    )
    usocket.pipe(socket).pipe(usocket)
    if (uhead?.length) socket.write(uhead)
  })
  upstream.on('error', () => socket.destroy())
  upstream.end(head)
})

const [host, portStr] = FRONT.includes(':') ? FRONT.split(':') : ['127.0.0.1', FRONT]
server.listen(Number(portStr), host, () => {
  console.log(`[dual-proxy] front=${host}:${portStr} ts=${TS} go=${GO} split=${SPLIT}`)
})
