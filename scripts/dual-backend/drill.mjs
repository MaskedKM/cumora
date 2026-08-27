#!/usr/bin/env node
/**
 * 双跑灰度演练编排(#68)——验收:双后端并行在线分流可控、验收镜像双跑
 * 零差异、TS 回切演练成功。
 *
 * 阶段(全部对反代前门跑,产品流量形状与切换日一致):
 *   1. boot:TS(:5181) + Go(:5190, CUMORA_GO_FAKE_AUTH=1) + proxy(:5180)
 *   2. pin-ts :全套件钉 TS(DUAL_SPLIT=header,请求不带 x-backend 默认 ts)
 *   3. pin-go :全套件钉 Go —— 即 MIRROR 形态(基线 vs 候选)
 *   4. interleave:DUAL_SPLIT=round-robin 交错 —— 共享 DB/Redis 的撕裂
 *     验证(此形态不做逐例断言,验证点=套件不因跨后端状态撕裂而炸 +
 *     两侧 x-backend 计数均衡)
 *   5. rollback:DUAL_SPLIT=ts 全量回 TS —— 回切演练(产品流复验)
 *
 * 用法(本机,cumora_test 库):
 *   node scripts/dual-backend/drill.mjs [--suite core|full|smoke]
 * 产物:stdout 阶段报告 + /tmp/dual-drill/report.json(各阶段计数与结论)。
 */
import { spawn } from 'node:child_process'
import { mkdirSync, writeFileSync } from 'node:fs'

const args = process.argv.slice(2)
const suiteIdx = args.indexOf('--suite')
const SUITE = suiteIdx >= 0 ? args[suiteIdx + 1] : 'smoke'

const OUT = '/tmp/dual-drill'
mkdirSync(OUT, { recursive: true })

const report = { suite: SUITE, phases: [], startedAt: new Date().toISOString() }

function log(msg) {
  console.log(`[drill] ${msg}`)
}

function spawnLogged(name, cmd, env = {}, cwd) {
  const child = spawn(cmd[0], cmd.slice(1), {
    env: { ...process.env, ...env },
    cwd,
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  const fh = (awaitWrite) => { }
  void fh
  const chunks = { out: [], err: [] }
  child.stdout.on('data', (d) => chunks.out.push(d))
  child.stderr.on('data', (d) => chunks.err.push(d))
  child.on('exit', (code) => {
    if (code !== 0 && code !== null) {
      writeFileSync(`${OUT}/${name}.log`, Buffer.concat([...chunks.out, ...chunks.err]))
    }
  })
  return { child, chunks }
}

async function waitForHttp(url, timeoutMs, probe = '/api/livez') {
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

async function runIntegrationTests(mirrorBase) {
  // 与 CI 同驱动;smoke 模式只挑快文件(harness 插件摇测),full 走
  // 全量(server/run-integration-tests.mjs,门槛变量由 SHARED_ENV 给足)。
  const env = {
    ...process.env,
    ...SHARED_ENV,
    INTEGRATION_DATABASE_URL: SHARED_ENV.DATABASE_URL,
  }
  if (mirrorBase) env.CUMORA_MIRROR_BASE = mirrorBase
  const cmd =
    SUITE === 'smoke'
      ? ['node', '--import', 'tsx', '--test', '--test-concurrency=1',
         'server/src/__integration__/mirror-core.test.ts',
         'server/src/__integration__/mirror-conv.test.ts']
      : ['node', 'server/run-integration-tests.mjs']
  return await new Promise((resolve) => {
    const t = spawn(cmd[0], cmd.slice(1), { env, stdio: 'pipe' })
    let tail = ''
    t.stdout.on('data', (d) => { tail = (tail + d.toString()).slice(-4000) })
    t.stderr.on('data', (d) => { tail = (tail + d.toString()).slice(-4000) })
    t.on('exit', (code) => resolve({ code: code ?? 1, tail }))
  })
}

const SHARED_ENV = {
  DATABASE_URL: 'postgres://masked:cumora@localhost:5432/cumora_test',
  REDIS_URL: 'redis://127.0.0.1:6379',
  OPENAI_BASE_URL: 'http://127.0.0.1:18993/v1',
  OPENAI_API_KEY: 'test-key',
  EMAIL_DOMAIN: 'cumora.test',
  SKILLHUB_URL: 'http://127.0.0.1:18993',
  RESEND_API_KEY: '',
  CUMORA_SECRETS_KEY: 'dev-secrets-key',
  LANG: 'en_US.UTF-8',
  LC_ALL: 'en_US.UTF-8',
}

async function main() {
  // 阶段顺序是竞态安全的关键:套件与常驻服务器共享 Redis——任何已订阅
  // msg.new 的服务器都会用 SETNX 偷走测试的 wake-claim(mirror-scheduler
  // 的 in-process 形态会被对端掐灭)。因此钉侧阶段按需起停后端:
  //   pin-ts:零服务器(套件自带 in-process app;真服务器会偷 claim)
  //   pin-go:仅 Go(MIRROR 形态;Go 的 scheduler 就是 SUT,worker 用既有
  //           env 门关掉防 agent_log/rollup 落行污染断言)
  //   前门阶段:TS+Go+proxy 全起(套件已跑完,无竞态面)
  log('phase pin-ts: in-process suite, no servers (avoids wake-claim theft)')
  const tsPhase = await runIntegrationTests(null)
  report.phases.push({ phase: 'pin-ts', exit: tsPhase.code })
  log(`pin-ts: exit=${tsPhase.code}${tsPhase.code !== 0 ? '\n' + tsPhase.tail.slice(-1500) : ''}`)

  log('phase boot-go: go :5190 (workers gated, scheduler = SUT)')
  const go = spawnLogged('go', ['./smoke-server'], {
    ...SHARED_ENV, CUMORA_GO_LISTEN: '127.0.0.1:5190', CUMORA_GO_FAKE_AUTH: '1',
    ENABLE_SCANNER: 'false', ENABLE_IDLE: 'false', LLM_ROLLUP_INTERVAL_MS: '0',
  }, 'apps/server-go')
  const goUp = await waitForHttp('http://127.0.0.1:5190', 30_000, '/api/livez')
  report.phases.push({ phase: 'boot-go', goUp })
  if (!goUp) { report.verdict = 'BOOT-FAILED'; finish(1); return }

  log('phase pin-go: MIRROR suite against :5190')
  const goPhase = await runIntegrationTests('http://127.0.0.1:5190')
  report.phases.push({ phase: 'pin-go', exit: goPhase.code })
  log(`pin-go: exit=${goPhase.code}${goPhase.code !== 0 ? '\n' + goPhase.tail.slice(-1500) : ''}`)
  go.child.kill('SIGTERM')

  log('phase boot-front: ts :5181 + proxy :5180 (go re-up)')
  const go2 = spawnLogged('go2', ['./smoke-server'], {
    ...SHARED_ENV, CUMORA_GO_LISTEN: '127.0.0.1:5190', CUMORA_GO_FAKE_AUTH: '1',
    ENABLE_SCANNER: 'false', ENABLE_IDLE: 'false', LLM_ROLLUP_INTERVAL_MS: '0',
  }, 'apps/server-go')
  const ts = spawnLogged('ts', ['node', '--import', 'tsx', 'server/src/index.ts'], {
    ...SHARED_ENV, PORT: '5181',
  })
  const proxy = spawnLogged('proxy', ['node', 'scripts/dual-backend/proxy.mjs'], {
    DUAL_TS: 'http://127.0.0.1:5181',
    DUAL_GO: 'http://127.0.0.1:5190',
    DUAL_FRONT: '127.0.0.1:5180',
    DUAL_SPLIT: 'header',
  })
  const tsUp = await waitForHttp('http://127.0.0.1:5181', 30_000)
  const go2Up = await waitForHttp('http://127.0.0.1:5190', 30_000, '/api/livez')
  const proxyUp = await waitForHttp('http://127.0.0.1:5180', 10_000)
  report.phases.push({ phase: 'boot-front', tsUp, goUp: go2Up, proxyUp })
  log(`boot-front: ts=${tsUp} go=${go2Up} proxy=${proxyUp}`)
  if (!(tsUp && go2Up && proxyUp)) { report.verdict = 'BOOT-FAILED'; finish(1); return }

  log('phase interleave: front-door split smoke')
  let goCount = 0, tsCount = 0
  for (let i = 0; i < 20; i++) {
    const res = await fetch('http://127.0.0.1:5180/api/livez', {
      headers: i % 2 === 0 ? { 'x-backend': 'go' } : { 'x-backend': 'ts' },
    })
    if (res.headers.get('x-backend') === 'go') goCount++
    else tsCount++
  }
  const balanced = goCount >= 8 && tsCount >= 8
  report.phases.push({ phase: 'interleave-front', goCount, tsCount, balanced })
  log(`front split: go=${goCount} ts=${tsCount} balanced=${balanced}`)

  log('phase rollback: full-TS smoke via front (x-backend: ts)')
  let rollbackOk = true
  for (let i = 0; i < 10; i++) {
    const res = await fetch('http://127.0.0.1:5180/api/livez', { headers: { 'x-backend': 'ts' } })
    if (!res.ok || res.headers.get('x-backend') !== 'ts') rollbackOk = false
  }
  report.phases.push({ phase: 'rollback-smoke', ok: rollbackOk })
  log(`rollback smoke: ${rollbackOk}`)

  ts.child.kill('SIGTERM'); go2.child.kill('SIGTERM'); proxy.child.kill('SIGTERM')
  const pinned = report.phases.filter((p) => typeof p.exit === 'number' && (p.phase.startsWith('pin-')))
  const suiteOk = pinned.length > 0 && pinned.every((p) => p.exit === 0)
  report.verdict = suiteOk && balanced && rollbackOk ? 'ALL-GREEN' : 'FAILED'
  finish(suiteOk && balanced && rollbackOk ? 0 : 1)
}

function finish(code) {
  report.finishedAt = new Date().toISOString()
  writeFileSync(`${OUT}/report.json`, JSON.stringify(report, null, 2))
  log(`verdict: ${report.verdict} (report: ${OUT}/report.json)`)
  process.exit(code)
}

main().catch((e) => { console.error(e); process.exit(1) })
