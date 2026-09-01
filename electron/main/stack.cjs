/* eslint-env node */
// =============== Local Stack wizard runner (#284, ADR 0005) ===============
// The packaged AppImage carries the whole Stack under resources/bin
// (five binaries + pg/redis payload, #283). This module is the main-side
// executor the first-run wizard drives: probe → import-env → absorb →
// install → doctor. Every step shells out to the bundled cumora-stack
// binary — the wizard owns NO orchestration logic of its own, so the
// CLI acceptance path and the GUI path can never drift apart.
//
// Security posture: execFile (no shell interpolation), env values are
// never logged and never sent back to the renderer except as the key-name
// report import-env itself produces; the credential staging file is 0600
// in the OS temp dir and unlinked right after the import run.
const { ipcMain, app } = require('electron')
const { execFile } = require('node:child_process')
const { mkdtemp, writeFile, rm, access } = require('node:fs/promises')
const { constants } = require('node:fs')
const os = require('node:os')
const path = require('node:path')

/** Resolve the bundled cumora-stack binary.
 *  Packaged: resources/bin/cumora-stack (AppImage payload, #283 PR-C).
 *  Dev: fall back to PATH (build apps/stack first), so the wizard can
 *  be exercised against a dev build without packaging. */
function resolveStackBin() {
  if (app.isPackaged) {
    return path.join(process.resourcesPath, 'bin', 'cumora-stack')
  }
  return 'cumora-stack'
}

/** The AppImage payload dir (wizard's absorb source). Null in dev —
 *  the dev wizard asks the user to point at a payload dir instead. */
function payloadDir() {
  if (app.isPackaged) return path.join(process.resourcesPath, 'bin')
  return null
}

function runStack(args, opts = {}) {
  return new Promise((resolve) => {
    const bin = resolveStackBin()
    execFile(bin, args, { maxBuffer: 16 * 1024 * 1024, timeout: opts.timeout ?? 10 * 60_000 }, (err, stdout, stderr) => {
      // 保留尾部:失败诊断通常在末尾;上限防无界进渲染端内存。
      const MAX = 200_000
      const combined = `${stdout || ''}${stderr || ''}`
      resolve({
        code: err && typeof err.code === 'number' ? err.code : err ? 1 : 0,
        ok: !err,
        output: combined.length > MAX ? `…(前段截断)…\n${combined.slice(-MAX)}` : combined,
        error: err && typeof err.code !== 'number' ? String(err.message || err) : null,
      })
    })
  })
}

/** Expand a leading `~` — the wizard pre-fills ~/… defaults and the Go
 *  side (import-env) takes paths verbatim. */
function expandHome(p) {
  if (typeof p === 'string' && p.startsWith('~/')) {
    return path.join(os.homedir(), p.slice(2))
  }
  return p
}

function registerIpc() {
  // 向导触发判定:栈 server 不可达。5181 通 = 已有部署,向导不出现
  // (这是设计:向导只管"净机/复活",不管运行中栈的任何事)。
  ipcMain.handle('stack:probe', async () => {
    const probe = { serverUp: false, serverErr: '', wizard: true, payloadDir: payloadDir() }
    try {
      const { net } = require('electron')
      const reached = await new Promise((resolve) => {
        const req = net.request('http://127.0.0.1:5181/api/livez')
        req.on('response', () => { req.destroy(); resolve(true) })
        req.on('error', () => resolve(false))
        setTimeout(() => { req.destroy(); resolve(false) }, 1500)
        req.end()
      })
      probe.serverUp = reached
      probe.wizard = !reached
    } catch (e) {
      probe.serverErr = String(e)
      probe.wizard = true
    }
    return probe
  })

  // 一次性导入。creds(净机表单)→ 0600 临时 env 文件;或直接指到既有
  // .env/daemon.env。返回 import-env 的 JSON 报告(键名 only)。
  ipcMain.handle('stack:import', async (_evt, input = {}) => {
    let envFile = input.envFile || ''
    let staging = null
    try {
      if (input.creds && (input.creds.GITHUB_CLIENT_ID || input.creds.GITHUB_CLIENT_SECRET)) {
        const dir = await mkdtemp(path.join(os.tmpdir(), 'cumora-wizard-'))
        staging = path.join(dir, 'creds.env')
        const lines = []
        for (const [k, v] of Object.entries(input.creds)) {
          if (v) lines.push(`${k}=${v}`)
        }
        await writeFile(staging, lines.join('\n') + '\n', { mode: 0o600 })
        envFile = staging
      }
      if (!envFile) return { ok: false, code: 2, error: '需要 envFile 或 creds' }
      const args = ['import-env', '--env-file', expandHome(envFile), '--json']
      if (input.daemonEnvFile) args.push('--daemon-env-file', expandHome(input.daemonEnvFile))
      const res = await runStack(args)
      return { ...res, report: parseReport(res.output) }
    } finally {
      if (staging) await rm(path.dirname(staging), { recursive: true, force: true })
    }
  })

  ipcMain.handle('stack:absorb', async (_evt, input = {}) => {
    const dir = input.payloadDir || payloadDir()
    if (!dir) return { ok: false, code: 2, error: 'dev 构建无内置载荷;请构建 AppImage 或传 payloadDir' }
    try {
      await access(path.join(dir, 'MANIFEST'), constants.R_OK)
    } catch {
      return { ok: false, code: 2, error: `载荷目录缺 MANIFEST: ${dir}` }
    }
    return runStack(['absorb', dir], { timeout: 15 * 60_000 })
  })

  ipcMain.handle('stack:install', () => runStack(['install'], { timeout: 5 * 60_000 }))

  ipcMain.handle('stack:doctor', () => runStack(['doctor', '--json']))
}

function parseReport(output) {
  // import-env --json:报告是 stdout 的最后一个 JSON 对象(前面无杂音,
  // 但稳妥起见从首个 '{' 起解析)。
  try {
    const start = output.indexOf('{')
    if (start < 0) return null
    return JSON.parse(output.slice(start))
  } catch {
    return null
  }
}

module.exports = { registerIpc, resolveStackBin }
