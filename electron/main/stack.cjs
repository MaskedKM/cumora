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
const tray = require('./tray.cjs')
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
      // stdout 与 stderr 分离保留(评审 P1):--json 报告解析必须只吃
      // stdout,stderr 噪音拼在后面会让 JSON.parse 碎掉。output 仍是
      // 合并面(尾部截断,上限防无界进渲染端)。
      const MAX = 200_000
      const clamp = (x) => (x.length > MAX ? `…(前段截断)…\n${x.slice(-MAX)}` : x)
      resolve({
        // execFile 的 err.code:非零退出 = number;spawn 失败(ENOENT)=
        // string —— 后者归一为 127(评审 P2:不得与红线退出码 1 混淆)。
        code: err ? (typeof err.code === 'number' ? err.code : 127) : 0,
        ok: !err,
        stdout: clamp(String(stdout || '')),
        stderr: clamp(String(stderr || '')),
        output: clamp(`${stdout || ''}${stderr || ''}`),
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
        // 200|503 才算 cumora 栈在(503=Redis 红的诚实活信号);其他
        // 状态码 = 别的进程占了 5181,不当栈在。
        req.on('response', (res) => { const up = res.statusCode === 200 || res.statusCode === 503; req.destroy(); resolve(up) })
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
        // 键白名单 + 值拒换行/等号(评审 P3:粘贴值里的 \n 可向
        // staging env 注入额外键行)。坏值按不存在处理并点名。
        const allowed = new Set(['GITHUB_CLIENT_ID', 'GITHUB_CLIENT_SECRET'])
        const lines = []
        for (const [k, v] of Object.entries(input.creds)) {
          if (!allowed.has(k)) return { ok: false, code: 2, error: `credential key not allowed: ${k}` }
          if (v === undefined || v === null || v === '') continue
          if (/[\n=]/.test(String(v))) return { ok: false, code: 2, error: `credential value rejected (newline/=): ${k}` }
          lines.push(`${k}=${v}`)
        }
        if (lines.length === 0) return { ok: false, code: 2, stdout: '', stderr: '', output: '', error: 'envFile or creds required' }
        const dir = await mkdtemp(path.join(os.tmpdir(), 'cumora-wizard-'))
        staging = path.join(dir, 'creds.env')
        await writeFile(staging, lines.join('\n') + '\n', { mode: 0o600 })
        envFile = staging
      }
      if (!envFile) return { ok: false, code: 2, stdout: '', stderr: '', output: '', error: 'envFile or creds required' }
      const args = ['import-env', '--env-file', expandHome(envFile), '--json']
      if (input.daemonEnvFile) args.push('--daemon-env-file', expandHome(input.daemonEnvFile))
      const res = await runStack(args)
      return { ...res, report: parseReport(res.stdout) }
    } finally {
      if (staging) await rm(path.dirname(staging), { recursive: true, force: true })
    }
  })

  ipcMain.handle('stack:absorb', async (_evt, input = {}) => {
    const dir = input.payloadDir || payloadDir()
    if (!dir) return { ok: false, code: 2, stdout: '', stderr: '', output: '', error: 'dev build has no bundled payload; build the AppImage or pass payloadDir' }
    try {
      await access(path.join(dir, 'MANIFEST'), constants.R_OK)
    } catch {
      return { ok: false, code: 2, stdout: '', stderr: '', output: '', error: `payload dir has no MANIFEST: ${dir}` }
    }
    return runStack(['absorb', dir], { timeout: 15 * 60_000 })
  })

  ipcMain.handle('stack:install', () => runStack(['install'], { timeout: 5 * 60_000 }))

  ipcMain.handle('stack:doctor', () => runStack(['doctor', '--json']))

  // ==== 管理面(#286):status/releases/restart/rollback ====
  // 升级 = absorb(制品内载荷)+ restart,两步由渲染端驱动展示分段
  // 进度(与向导同形态)——不设聚合 upgrade 命令,语义留 CLI 小而正交。
  ipcMain.handle('stack:status', () => runStack(['status', '--json']))
  ipcMain.handle('stack:releases', () => runStack(['releases', '--json']))
  ipcMain.handle('stack:restart', () => runStack(['restart'], { timeout: 5 * 60_000 }))
  ipcMain.handle('stack:rollback', (_evt, input = {}) => {
    // 正向白名单:版本 token 只许字母数字与 . _ -(路径形态全拒)。
    if (!input.version || !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(input.version)) {
      return { ok: false, code: 2, stdout: '', stderr: '', output: '', error: 'invalid version' }
    }
    return runStack(['rollback', input.version], { timeout: 5 * 60_000 })
  })

  // degraded 提醒(渲染端状态轮询发现熔断/子进程死 → 托盘警示)。
  ipcMain.on('stack:degraded', (_evt, degraded) => {
    try { tray.setStackDegraded(!!degraded) } catch { /* tray 非关键路径 */ }
  })
}

function parseReport(stdout) {
  // import-env --json:报告打在 stdout(评审 P1:stderr 一概不进解析面,
  // 防拼接噪音击碎 JSON.parse)。从首个 '{' 起解析以容忍前导杂音。
  try {
    const start = stdout.indexOf('{')
    if (start < 0) return null
    return JSON.parse(stdout.slice(start))
  } catch {
    return null
  }
}

module.exports = { registerIpc, resolveStackBin }
