#!/usr/bin/env node
/**
 * Integration test runner(#70 TS 退役重塑:MIRROR-only)。
 *
 * 套件不再有 in-process TS app 形态——本 runner 自建 SUT 全套:
 *   1. mock LLM(/v1/responses 'feminine' + /v1/images/generations 1×1 PNG
 *      ——mirror-tail 头像链路的最小形状)
 *   2. yjs-sidecar(doc collab 链路;仍是 TS,apps/yjs-sidecar 保留)
 *   3. Go server(server-go 构建,fake-auth + worker 门 + OAuth 桩 env)
 * 然后以 CUMORA_MIRROR_BASE 指向 Go 服跑全套 node:test。
 *
 * Redis 隔离:本机跑时若 REDIS_URL 指向 localhost 且未带 db 序号,强制
 * 追加 /5——与常驻服务器(默认 db0)分道,防 wake-claim SETNX 被偷
 * (2026-08-28 实测:生产 Go 服与测试服同库会导致 scheduler 用例 0 唤醒)。
 *
 * To run:
 *   1. DEDICATED test DB(套件 TRUNCATE 全表):
 *        createdb cumora_test
 *   2. Redis listening(默认 localhost:6379)
 *   3. export INTEGRATION_DATABASE_URL=postgres://$USER@localhost:5432/cumora_test
 *   4. npm run test:integration
 *
 * INTEGRATION_DATABASE_URL 未设时打一行 skipped 退出 0(CI/pre-commit
 * 无测试库也不挡道)。BOOT-FAILED 时杀尽全部子进程再退出(#68 评审 F17)。
 */
import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { rm } from 'node:fs/promises'
import { readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { basename, dirname, join } from 'node:path'
import 'dotenv/config'

const INTEGRATION_URL = process.env.INTEGRATION_DATABASE_URL
if (!INTEGRATION_URL) {
  console.log('[integration] skipped — set INTEGRATION_DATABASE_URL to enable.')
  console.log('             example: INTEGRATION_DATABASE_URL=postgres://$USER@localhost:5432/cumora_test \\\\')
  console.log('                      npm run test:integration')
  process.exit(0)
}

// Belt-and-braces safety: refuse to run when INTEGRATION_DATABASE_URL
// looks like a production-ish DB name. The suite TRUNCATEs every table,
// so a mis-set var would silently nuke real data.
const SUSPICIOUS = /\b(prod|production|main|live)\b/i
if (SUSPICIOUS.test(INTEGRATION_URL)) {
  console.error(`[integration] refusing to run — INTEGRATION_DATABASE_URL looks production-flavored: ${INTEGRATION_URL}`)
  console.error('              The suite TRUNCATEs every table. Point at a dedicated test DB (e.g. cumora_test).')
  process.exit(2)
}

// Swap DATABASE_URL so the harness env module picks up the test target.
process.env.DATABASE_URL = INTEGRATION_URL

// Never hit live Resend from the suite (same contract as before).
process.env.RESEND_API_KEY = ''
if (!process.env.EMAIL_DOMAIN) process.env.EMAIL_DOMAIN = 'cumora.local'
if (!process.env.EMAIL_INBOUND_HMAC_SECRET) process.env.EMAIL_INBOUND_HMAC_SECRET = 'integration-test-secret'
if (!process.env.CUMORA_SECRETS_KEY) process.env.CUMORA_SECRETS_KEY = 'integration-secrets-key'

// Redis isolation(见文件头)。显式带 db 序号或非本机(CI service redis
// 上没有别的订阅者)的 REDIS_URL 原样放行。
let redisUrl = process.env.REDIS_URL ?? 'redis://127.0.0.1:6379'
if (/\/\/(localhost|127\.0\.0\.1)/.test(redisUrl) && !/\/\d+$/.test(redisUrl)) {
  redisUrl = redisUrl.replace(/\/?$/, '') + '/5'
}
process.env.REDIS_URL = redisUrl
console.log(`[integration] redis: ${redisUrl}`)

const here = dirname(fileURLToPath(import.meta.url))
const repo = join(here, '..')
const GO_DIR = join(repo, 'apps/server-go')

// 端口动态挑选(零占端口假设,可并发跑):唯一固定口是 mirror-oauth
// 测试进程内自起的 18994 桩(测试文件自持)。
async function freePort() {
  const { createServer: cs } = await import('node:http')
  return await new Promise((resolve, reject) => {
    const srv = cs()
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address()
      srv.close(() => resolve(port))
    })
    srv.on('error', reject)
  })
}
const [GO_PORT, SIDECAR_PORT, MOCK_PORT] = await Promise.all([freePort(), freePort(), freePort()])
const GO_BASE = `http://127.0.0.1:${GO_PORT}`

/* ───────── children registry(F17:任何失败路径都杀尽再退) ───────── */
const children = []
let exited = false
function killAll() {
  for (const c of children) {
    try { c.kill('SIGTERM') } catch { /* already gone */ }
  }
}
function bail(msg, code = 1) {
  if (exited) return
  exited = true
  console.error(`[integration] ${msg}`)
  killAll()
  mockLLM?.close()
  if (!process.env.INTEGRATION_GO_BIN) rm(GO_BIN, { force: true }).catch(() => {})
  process.exit(code)
}
process.on('SIGINT', () => bail('interrupted', 130))
process.on('SIGTERM', () => bail('terminated', 143))

/* ───────── 1. shared mock LLM ───────── */
const PNG_B64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=='
const mockLLM = createServer((req, res) => {
  const chunks = []
  req.on('data', (c) => chunks.push(c))
  req.on('end', () => {
    if (req.url?.endsWith('/responses')) {
      res.setHeader('content-type', 'application/json')
      res.end(JSON.stringify({
        id: 'resp-mock', object: 'response', status: 'completed',
        output: [{ type: 'message', content: [{ type: 'output_text', text: 'feminine' }] }],
      }))
      return
    }
    if (req.url?.endsWith('/images/generations')) {
      res.setHeader('content-type', 'application/json')
      res.end(JSON.stringify({ data: [{ b64_json: PNG_B64 }] }))
      return
    }
    res.statusCode = 404
    res.end('{}')
  })
})

/* ───────── helpers ───────── */
function spawnChild(name, cmd, args, opts = {}) {
  const c = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'], ...opts })
  children.push(c)
  let tail = ''
  const keep = (d) => { tail = (tail + d.toString()).slice(-4000) }
  c.stdout.on('data', keep)
  c.stderr.on('data', keep)
  c.on('exit', (code) => {
    if (!exited && code !== 0 && code !== null) bail(`${name} exited early (code=${code}):\n${tail}`)
  })
  return c
}

async function waitFor(url, timeoutMs, probe = '/api/livez') {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url + probe, { signal: AbortSignal.timeout(2000) })
      if (res.ok || res.status === 401 || res.status === 404) return true
    } catch { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 500))
  }
  return false
}

const GO_BIN = process.env.INTEGRATION_GO_BIN || join(GO_DIR, '.integration-server-bin')

// Sharding(#115,#70 续):INTEGRATION_SHARD="N/M" 只跑排序后下标 mod M
// == N-1 的文件——确定性均分、跨 run 稳定。每片仍内部串行;CI 给每片
// 独立 Postgres 库 + 独立 Redis 实例(pub/sub 是实例级,db 序号会串)。
// INTEGRATION_GO_BIN:CI 先建一次二进制、三片复用(片内不再竞抢构建)。
const shardRaw = process.env.INTEGRATION_SHARD ?? '1/1'
const shardMatch = /^([1-9]\d*)\/([1-9]\d*)$/.exec(shardRaw)
if (!shardMatch || Number(shardMatch[1]) > Number(shardMatch[2])) {
  console.error(`[integration] bad INTEGRATION_SHARD: ${shardRaw} (expected "N/M" with 1 ≤ N ≤ M)`)
  process.exit(2)
}
const [shardN, shardM] = [Number(shardMatch[1]), Number(shardMatch[2])]
const integrationDir = join(here, 'src/__integration__')
const testFiles = readdirSync(integrationDir)
  .filter((f) => f.endsWith('.test.ts'))
  .sort()
  .filter((_, i) => i % shardM === shardN - 1)
  .map((f) => join(integrationDir, f))
if (testFiles.length === 0) {
  console.error(`[integration] shard ${shardN}/${shardM} got zero files — refusing to run (miscount?)`)
  process.exit(2)
}
console.log(`[integration] shard ${shardN}/${shardM}: ${testFiles.length} file(s) — ${testFiles.map((f) => basename(f)).join(', ')}`)

async function buildGoServer() {
  if (process.env.INTEGRATION_GO_BIN) {
    console.log(`[integration] reusing prebuilt SUT: ${GO_BIN}`)
    return
  }
  // Prefer a system Go(CI 的 golang 容器);本机无 Go 时借 docker
  // golang:1.24(与 CI 同镜像,godocker.sh 语义)。
  const hasGo = await new Promise((resolve) => {
    const p = spawn('go', ['version'], { stdio: 'ignore' })
    p.on('error', () => resolve(false))
    p.on('exit', (c) => resolve(c === 0))
  })
  if (hasGo) {
    const build = spawn('go', ['build', '-o', GO_BIN, './cmd/server'], {
      cwd: GO_DIR,
      env: { ...process.env, CGO_ENABLED: '0', GOFLAGS: '-buildvcs=false' },
      stdio: 'inherit',
    })
    const code = await new Promise((resolve) => build.on('exit', resolve))
    if (code !== 0) bail('go build failed')
  } else {
    const build = spawn('./godocker.sh', ['build', '-o', './.integration-server-bin', './cmd/server'], {
      cwd: GO_DIR, stdio: 'inherit',
    })
    const code = await new Promise((resolve) => build.on('exit', resolve))
    if (code !== 0) bail('godocker build failed')
  }
}

/* ───────── main ───────── */
const SHARED_ENV = {
  ...process.env,
  DATABASE_URL: INTEGRATION_URL,
  REDIS_URL: redisUrl,
}

await new Promise((resolve, reject) => {
  mockLLM.once('error', reject)
  mockLLM.listen(MOCK_PORT, '127.0.0.1', resolve)
})
console.log(`[integration] mock LLM :${MOCK_PORT}`)

await buildGoServer()

spawnChild('sidecar', 'node', ['--import', 'tsx', 'apps/yjs-sidecar/src/main.ts'], {
  cwd: repo,
  env: {
    ...SHARED_ENV,
    OPENAI_API_KEY: 'test-key',
    OPENAI_BASE_URL: `http://127.0.0.1:${MOCK_PORT}/v1`,
    RESEND_API_KEY: '',
    YJS_SIDECAR_TOKEN: 't',
    YJS_SIDECAR_PORT: String(SIDECAR_PORT),
  },
})
if (!(await waitFor(`http://127.0.0.1:${SIDECAR_PORT}`, 20_000, '/'))) {
  bail('BOOT-FAILED: sidecar never came up')
}

spawnChild('go-server', GO_BIN, [], {
  cwd: repo,
  env: {
    ...SHARED_ENV,
    CUMORA_GO_LISTEN: `127.0.0.1:${GO_PORT}`,
    // 迁移目录按 CWD 相对解析——cwd 在仓库根,故给绝对路径(生产单元同款)。
    CUMORA_GO_MIGRATIONS: join(GO_DIR, 'migrations'),
    CUMORA_GO_FAKE_AUTH: '1',
    ENABLE_SCANNER: 'false',
    ENABLE_IDLE: 'false',
    LLM_ROLLUP_INTERVAL_MS: '0',
    OPENAI_API_KEY: 'test-key',
    OPENAI_BASE_URL: `http://127.0.0.1:${MOCK_PORT}/v1`,
    SKILLHUB_URL: `http://127.0.0.1:${MOCK_PORT}`,
    // mirror-oauth 的桩约定:测试文件在自身进程内起 :18994 桩,Go 服
    // 的 provider 配置必须指向它(见 mirror-oauth.test.ts 头注)。
    GITHUB_CLIENT_ID: 'stub-id',
    GITHUB_CLIENT_SECRET: 'stub-secret',
    CUMORA_OAUTH_GITHUB_BASE: 'http://127.0.0.1:18994',
    CUMORA_AUTH_RETURN_ALLOWLIST: 'http://localhost:5180/',
    YJS_SIDECAR_URL: `http://127.0.0.1:${SIDECAR_PORT}`,
    YJS_SIDECAR_TOKEN: 't',
  },
})
if (!(await waitFor(GO_BASE, 30_000))) {
  bail('BOOT-FAILED: Go server never came up')
}
console.log(`[integration] SUT up: ${GO_BASE} (sidecar :${SIDECAR_PORT}, mock :${MOCK_PORT})`)

// Forward to node --import tsx --test against the MIRROR-only suite.
// --test-concurrency=1 serializes test FILES: every file's beforeEach
// TRUNCATEs the same tables on the shared test DB; concurrent TRUNCATE
// CASCADEs deadlock at the catalog-lock level.
const child = spawn(
  'node',
  ['--import', 'tsx', '--test', '--test-concurrency=1', ...testFiles],
  {
    stdio: 'inherit',
    env: { ...process.env, CUMORA_MIRROR_BASE: GO_BASE },
  },
)
const builtHere = !process.env.INTEGRATION_GO_BIN
child.on('exit', (code) => {
  killAll()
  mockLLM.close()
  if (builtHere) rm(GO_BIN, { force: true }).catch(() => { /* best effort */ })
  exited = true
  process.exit(code ?? 1)
})
