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
 *   1. 什么都不用配(本机有 docker 时):未设 INTEGRATION_DATABASE_URL
 *      则自动起一次性测试栈(pgvector + redis 各一容器,随机端口,退出
 *      即拆)——cumora-pg/cumora-redis 生产实例不再被测试打搅(#146)。
 *      镜像与 CI/scratch 同款(pgvector/pgvector:pg16 + redis:7)。
 *   2. 显式自带库:export INTEGRATION_DATABASE_URL=postgres://…/cumora_test
 *      (CI 即此形态,auto 路径不触发);REDIS_URL 同理,本机 localhost
 *      形态会被强制加 /5 db 序号与常驻服务分道。
 *   3. INTEGRATION_FILES="mirror-scheduler" 可按文件名子串过滤(逗号分隔
 *      多个子串,叠在 shard 之后)——#119 单文件复跑复现器用。
 *
 * 两者皆无时(无 env 且 docker 不可用)打一行 skipped 退出 0(pre-commit
 * 不挡道)。BOOT-FAILED 时杀尽全部子进程、拆掉 auto 栈再退出(#68 F17)。
 */
import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { rm } from 'node:fs/promises'
import { readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { basename, dirname, join } from 'node:path'
import 'dotenv/config'

const here = dirname(fileURLToPath(import.meta.url))
const repo = join(here, '..')
const GO_DIR = join(repo, 'apps/server-go')

/* ───────── shard / 文件选择(纯同步校验,必须先于一切起栈副作用) ─────────
 * #199 评审 P1:exit(2) 类校验若落在 auto 栈起好之后,打错一个
 * INTEGRATION_FILES 子串就会确定性留下两个孤儿容器;提前也让 typo
 * 秒报,不用等起栈。 */
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

// #119 复现器入口:INTEGRATION_FILES 按文件名子串过滤(逗号分隔多个
// 子串任一命中),叠在 shard 之后 —— 单文件循环复跑不再需要动 shard。
// 全空白值视为未设(no-op),不算零命中。
if (process.env.INTEGRATION_FILES) {
  const subs = process.env.INTEGRATION_FILES.split(',').map((s) => s.trim()).filter(Boolean)
  if (subs.length > 0) {
    const filtered = testFiles.filter((f) => subs.some((s) => basename(f).includes(s)))
    if (filtered.length === 0) {
      console.error(`[integration] INTEGRATION_FILES="${process.env.INTEGRATION_FILES}" matched zero files (after shard ${shardN}/${shardM})`)
      process.exit(2)
    }
    testFiles.length = 0
    testFiles.push(...filtered)
    console.log(`[integration] INTEGRATION_FILES filter: ${testFiles.length} file(s) — ${testFiles.map((f) => basename(f)).join(', ')}`)
  }
}

/* ───────── #146 一次性测试栈(仅本机、仅未显式给 env 时) ─────────
 * 随机端口起 pgvector + redis 各一容器,套件退出(含 bail/信号)即拆。
 * 此前本机测试直连 cumora-pg 里的 cumora_test:TRUNCATE churn 与生产共
 * 缓冲池/CPU/IO,是 #119 时序 flake 的环境根源(2026-08-28 实测
 * bgwriter ≈516GB/13.5h、checkpoint 每 ~93s)。 */
let autoStack = null // { pg, redis, dbUrl, redisUrl }

function runCmd(cmd, args, timeoutMs = 30_000) {
  return new Promise((resolve) => {
    const p = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'pipe'] })
    let out = '', err = ''
    const timer = setTimeout(() => { try { p.kill('SIGKILL') } catch { /* gone */ } }, timeoutMs)
    p.stdout.on('data', (d) => { out += d })
    p.stderr.on('data', (d) => { err += d })
    p.on('error', () => { clearTimeout(timer); resolve({ code: 1, out, err }) })
    p.on('exit', (c) => { clearTimeout(timer); resolve({ code: c ?? 1, out, err }) })
  })
}

async function waitContainerReady(name, checkArgs, deadlineMs) {
  const deadline = Date.now() + deadlineMs
  while (Date.now() < deadline) {
    if ((await runCmd('docker', ['exec', name, ...checkArgs], 10_000)).code === 0) return true
    await new Promise((r) => setTimeout(r, 500))
  }
  return false
}

/** 起栈结果:{ ok:false, reason } 或 { ok:true, stack }。reason 区分
 *  'no-docker'(daemon 不在 → 维持旧 skipped 语义退 0)与
 *  'provision-failed'(run/就绪/端口失败 → 调用方按真失败退 1,
 *  不再误报"无 docker")。 */
async function provisionOneOffStack() {
  const dockerOk = await runCmd('docker', ['info', '--format', '{{.ServerVersion}}'], 10_000)
  if (dockerOk.code !== 0) return { ok: false, reason: 'no-docker' }
  const id = Math.random().toString(36).slice(2, 8)
  const pg = `cumora-it-pg-${id}`
  const rd = `cumora-it-redis-${id}`
  // 与 CI/scratch 同款镜像(pgvector 为迁移所需;本机两者皆有缓存,
  // 拉取仅首次)。POSTGRES_* 与 CI services 块同参。run 宽超时:首次
  // 拉取数百 MB,30s 必掐死(#199 评审 P2)。
  const pgRun = await runCmd('docker', [
    'run', '-d', '--name', pg, '-p', '127.0.0.1::5432',
    '-e', 'POSTGRES_USER=cumora', '-e', 'POSTGRES_PASSWORD=cumora', '-e', 'POSTGRES_DB=cumora_test',
    'pgvector/pgvector:pg16',
  ], 300_000)
  if (pgRun.code !== 0) {
    console.error(`[integration] docker run pg failed: ${(pgRun.err.trim() || 'timeout/no-output').slice(0, 500)}`)
    return { ok: false, reason: 'provision-failed' }
  }
  const rdRun = await runCmd('docker', [
    'run', '-d', '--name', rd, '-p', '127.0.0.1::6379', 'redis:7',
  ], 300_000)
  if (rdRun.code !== 0) {
    console.error(`[integration] docker run redis failed: ${(rdRun.err.trim() || 'timeout/no-output').slice(0, 500)}`)
    await runCmd('docker', ['rm', '-f', pg])
    return { ok: false, reason: 'provision-failed' }
  }
  const ready = (await waitContainerReady(pg, ['pg_isready', '-U', 'cumora', '-d', 'cumora_test'], 45_000))
    && (await waitContainerReady(rd, ['redis-cli', 'ping'], 20_000))
  if (!ready) {
    console.error('[integration] one-off stack never became ready — tearing down')
    await runCmd('docker', ['rm', '-f', pg, rd])
    return { ok: false, reason: 'provision-failed' }
  }
  const pgPort = (await runCmd('docker', ['port', pg, '5432'])).out.trim().split('\n')[0]?.split(':').pop()
  const rdPort = (await runCmd('docker', ['port', rd, '6379'])).out.trim().split('\n')[0]?.split(':').pop()
  if (!pgPort || !rdPort) {
    console.error('[integration] could not discover one-off stack ports')
    await runCmd('docker', ['rm', '-f', pg, rd])
    return { ok: false, reason: 'provision-failed' }
  }
  return {
    ok: true,
    stack: {
      pg, redis: rd,
      dbUrl: `postgres://cumora:cumora@127.0.0.1:${pgPort}/cumora_test`,
      redisUrl: `redis://127.0.0.1:${rdPort}`,
    },
  }
}

/** 拆 auto 栈。10s 硬上限兜底:docker CLI 若陷在挂死的 daemon socket 上
 *  不回 'exit' 事件,promise 永悬会把退出流程一起挂死(#199 评审 P3)。 */
async function teardownAutoStack() {
  if (!autoStack) return
  const { pg, redis } = autoStack
  autoStack = null
  await Promise.race([
    Promise.allSettled([runCmd('docker', ['rm', '-f', pg]), runCmd('docker', ['rm', '-f', redis])]),
    new Promise((r) => setTimeout(r, 10_000)),
  ])
}

let INTEGRATION_URL = process.env.INTEGRATION_DATABASE_URL
let exited = false // bail / child-exit / 早期信号共享,防双拆双退
if (!INTEGRATION_URL) {
  console.log('[integration] INTEGRATION_DATABASE_URL 未设 —— 尝试 docker 一次性测试栈(#146)…')
  // 起栈窗口(拉镜像+就绪门控,最坏 ~2min)的信号兜底:此刻主 bail 的
  // 依赖(children/mockLLM/GO_BIN)尚未初始化,单独兜拆栈再退;主信号
  // 处理器注册后再摘掉(#199 评审 P2)。
  const earlySignal = (label, code) => () => {
    if (exited) return
    exited = true
    console.error(`[integration] ${label} during provisioning`)
    void teardownAutoStack().finally(() => process.exit(code))
  }
  const onEarlyInt = earlySignal('interrupted', 130)
  const onEarlyTerm = earlySignal('terminated', 143)
  process.on('SIGINT', onEarlyInt)
  process.on('SIGTERM', onEarlyTerm)
  const prov = await provisionOneOffStack()
  process.off('SIGINT', onEarlyInt)
  process.off('SIGTERM', onEarlyTerm)
  if (!prov.ok && prov.reason === 'no-docker') {
    console.log('[integration] skipped — 无 docker 且未设 INTEGRATION_DATABASE_URL。')
    console.log('             起本地 docker 后重跑即自动起一次性测试栈;或显式指定:')
    console.log('             INTEGRATION_DATABASE_URL=postgres://$USER@localhost:5432/cumora_test npm run test:integration')
    process.exit(0)
  }
  if (!prov.ok) {
    console.error('[integration] 一次性测试栈起失败(见上)。修好 docker 后重跑,或显式给 INTEGRATION_DATABASE_URL。')
    process.exit(1)
  }
  autoStack = prov.stack
  INTEGRATION_URL = autoStack.dbUrl
  console.log(`[integration] one-off stack: ${autoStack.pg} + ${autoStack.redis}(退出即拆)`)
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

// Redis isolation(见文件头)。auto 栈是一次性实例(pub/sub 实例级隔离,
// 无需 db 序号);显式给的 REDIS_URL 若指向本机且未带 db 序号,强制追加
// /5——与常驻服务器(默认 db0)分道,防 wake-claim SETNX 被偷
// (2026-08-28 实测:生产 Go 服与测试服同库会导致 scheduler 用例 0 唤醒)。
let redisUrl
if (autoStack) {
  redisUrl = autoStack.redisUrl
} else {
  redisUrl = process.env.REDIS_URL ?? 'redis://127.0.0.1:6379'
  if (/\/\/(localhost|127\.0\.0\.1)/.test(redisUrl) && !/\/\d+$/.test(redisUrl)) {
    redisUrl = redisUrl.replace(/\/?$/, '') + '/5'
  }
}
process.env.REDIS_URL = redisUrl
console.log(`[integration] redis: ${redisUrl}`)

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
function killAll() {
  for (const c of children) {
    if (!c || c.killed) continue
    try {
      if (process.platform !== 'win32' && typeof c.pid === 'number') {
        // 进程组:detach 子进程后向整组发信号,逮住孙进程。
        process.kill(-c.pid, 'SIGTERM')
      } else {
        c.kill('SIGTERM')
      }
    } catch {
      // 组信号失手(如组已被回收)时退回单杀,绝不静默漏杀。
      try { c.kill('SIGTERM') } catch { /* already gone */ }
    }
  }
}
function bail(msg, code = 1) {
  if (exited) return
  exited = true
  console.error(`[integration] ${msg}`)
  killAll()
  mockLLM?.close()
  if (!process.env.INTEGRATION_GO_BIN) rm(GO_BIN, { force: true }).catch(() => {})
  // auto 栈是本 runner 起的,任何失败路径都得拆干净再退(#146)。
  if (autoStack) void teardownAutoStack().finally(() => process.exit(code))
  else process.exit(code)
}
process.on('SIGINT', () => bail('interrupted', 130))
process.on('SIGTERM', () => bail('terminated', 143))
// 顶层未捕获异常同样要走拆栈再退(#199 评审 P3):ESM 顶层 throw 的默认
// 退出不经过任何清理,auto 栈会漏。
process.on('uncaughtException', (e) => bail(`uncaught: ${e?.stack ?? e}`, 1))
process.on('unhandledRejection', (e) => bail(`unhandled rejection: ${e}`, 1))

/* ───────── 1. shared mock LLM ───────── */
const PNG_B64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=='
let mockLLM = createServer((req, res) => {
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
  // 非 win32 detach 自成进程组组长 —— killAll 的 kill(-pid) 整组信号
  // 才能命中(否则 -pid 非法 PGID → ESRCH 被吞,子进程全部漏杀)。
  const c = spawn(cmd, args, {
    stdio: ['ignore', 'pipe', 'pipe'],
    detached: process.platform !== 'win32',
    ...opts,
  })
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
    // mirror-apple 的桩约定(同 :18994 款):JWKS 桩由测试文件自身起,
    // Go 服的 apple 验签指到它。
    CUMORA_APPLE_JWKS_URL: 'http://127.0.0.1:18995/keys',
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
  // bail(SIGINT/BOOT-FAILED)已接管退出时不再抢:exit 码归 bail(130/143
  // 而非 child 的 1),拆栈也由 bail 的 teardown 统一做(#199 评审 P3)。
  if (exited) return
  exited = true
  killAll()
  mockLLM.close()
  if (builtHere) rm(GO_BIN, { force: true }).catch(() => { /* best effort */ })
  if (autoStack) void teardownAutoStack().finally(() => process.exit(code ?? 1))
  else process.exit(code ?? 1)
})
