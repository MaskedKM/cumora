/**
 * Contract tests for the BYOA zcode adapter.
 *
 * zcode is not installed in CI. Each test places a fake `zcode-cli` first on
 * PATH (or points CUMORA_ZCODE_BIN at a fake zcode.cjs) and drives the
 * `--json` envelope shapes observed from CLI runtime 0.16.3
 * (docs/byoa-zcode-notes.md).
 */
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { existsSync, readFileSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, test } from 'node:test'
import assert from 'node:assert/strict'
import {
  detectEngines,
  getAdapter,
  parseZcodeEnvelope,
  readZcodeMainModel,
  resolveZcodeLauncher,
  zcodeEngineVersion,
  type EngineHopReport,
} from '../agents/computer/engine.js'

const IS_WIN = process.platform === 'win32'
const tempDirs: string[] = []
const envKeys: string[] = ['CUMORA_ZCODE_BIN', 'CUMORA_ZCODE_ARGS', 'CUMORA_TRIAGE_ARGS', 'CUMORA_TRIAGE_MODEL', 'XDG_CACHE_HOME']
const savedEnv: Record<string, string | undefined> = {}

afterEach(async () => {
  for (const k of envKeys) {
    if (k in savedEnv) {
      // NOTE: assign-undefined stores the STRING "undefined" in Node — a
      // value `extraArgs()` would happily treat as a flag override. Delete
      // instead so "was unset" stays unset.
      if (savedEnv[k] === undefined) delete process.env[k]
      else process.env[k] = savedEnv[k]
    }
    else delete process.env[k]
    delete savedEnv[k]
  }
  await Promise.all(tempDirs.splice(0).map((dir) => rm(dir, { recursive: true, force: true })))
})

function snapshotEnv(): void {
  for (const k of envKeys) if (!(k in savedEnv)) savedEnv[k] = process.env[k]
}

const FAKE_ZCODE = `#!/usr/bin/env node
'use strict'
const fs = require('node:fs')
const argv = process.argv.slice(2)
const record = (o) => { if (process.env.FAKE_ZCODE_LOG) fs.appendFileSync(process.env.FAKE_ZCODE_LOG, JSON.stringify(o) + '\\n') }
const scenario = process.env.FAKE_ZCODE_SCENARIO || 'ok'
const resumeAt = argv.indexOf('--resume')
const prompt = argv[argv.length - 1]
record({ argv, cwd: process.cwd() })
if (argv[0] === 'version') { process.stdout.write('0.16.3\\n'); process.exit(0) }
if (scenario === 'stale' && resumeAt >= 0) {
  process.stderr.write('Error: Session not found: ' + argv[resumeAt + 1] + '\\n')
  process.exit(1)
}
if (scenario === 'unknown-option') {
  process.stderr.write("Unknown option '--mode'. To specify a positional argument starting with a '-', place it at the end of the command after '--'\\n")
  process.exit(1)
}
if (scenario === 'plain-text') {
  process.stdout.write('no envelope, just text\\n')
  process.exit(0)
}
const out = {
  sessionId: resumeAt >= 0 ? argv[resumeAt + 1] : 'sess_zcode_new',
  response: 'echo:' + prompt,
  usage: { inputTokens: 21295, outputTokens: 39, cacheReadTokens: 17600, cacheWriteTokens: 5, reasoningTokens: 7 },
}
process.stdout.write(JSON.stringify(out, null, 2) + '\\n')
`

interface Fixture {
  root: string
  binDir: string
  home: string
  log: string
  env: NodeJS.ProcessEnv
}

/** A fixture with a fake `zcode-cli` on PATH and a logged-in-looking
 *  ~/.zcode/cli/config.json in a fake HOME (for model attribution). */
async function fixture(scenario = 'ok'): Promise<Fixture> {
  snapshotEnv()
  const root = await mkdtemp(join(tmpdir(), 'cumora-engine-zcode-'))
  tempDirs.push(root)
  const binDir = join(root, 'bin')
  const home = join(root, 'home')
  const fakeHome = join(root, 'fakehome')
  await mkdir(binDir)
  await mkdir(home)
  await mkdir(join(fakeHome, '.zcode', 'cli'), { recursive: true })
  const fake = join(binDir, 'zcode-cli')
  await writeFile(fake, FAKE_ZCODE, 'utf8')
  await chmod(fake, 0o755)
  await writeFile(join(fakeHome, '.zcode', 'cli', 'config.json'), JSON.stringify({
    model: { main: 'bigmodel-coding-plan/GLM-5.2' },
    provider: { kimi: { kind: 'anthropic', options: { apiKey: 'k-test-1', baseURL: 'https://api.kimi.com/coding' } } },
  }), 'utf8')
  const log = join(root, 'zcode.log')
  process.env.FAKE_ZCODE_LOG = log
  savedEnv['FAKE_ZCODE_LOG'] = undefined
  envKeys.push('FAKE_ZCODE_LOG')
  return {
    root,
    binDir,
    home,
    log,
    env: { PATH: `${binDir}:${process.env.PATH ?? ''}`, HOME: fakeHome, FAKE_ZCODE_LOG: log, FAKE_ZCODE_SCENARIO: scenario },
  }
}

function readLog(log: string): Array<{ argv: string[]; cwd: string }> {
  try {
    return readFileSync(log, 'utf8').trim().split('\n').filter(Boolean).map((l) => JSON.parse(l))
  } catch {
    return []
  }
}

test('zcode seedHome writes AGENTS.md (always) and common home directories', async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  await adapter.seedHome(f.home, { id: 'ag_test', name: 'Nova', role: 'pilot', systemPrompt: 'Be terse.' })
  const agents = await readFile(join(f.home, 'AGENTS.md'), 'utf8')
  assert.match(agents, /# Nova — pilot/)
  assert.match(agents, /Be terse\./)
  for (const dir of ['memory', 'notes', 'workspace', 'skills']) {
    assert.ok(existsSync(join(f.home, dir)), `missing ${dir}`)
  }
  assert.ok(existsSync(join(f.home, 'memory', 'MEMORY.md')))
})

test('zcode run parses the envelope: text, session id, usage, one hop, config model', { skip: IS_WIN }, async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  const hops: EngineHopReport[] = []
  const ac = new AbortController()
  const r = await adapter.run({
    home: f.home, env: f.env, prompt: 'hello there', signal: ac.signal,
    onLog: () => {}, onHopUsage: (h) => hops.push(h),
  })
  assert.equal(r.exitCode, 0)
  assert.equal(r.error, undefined)
  assert.equal(r.sessionId, 'sess_zcode_new')
  assert.equal(r.model, 'bigmodel-coding-plan/GLM-5.2')
  // inputTokens(21295) includes cacheRead(17600) → non-cached input 3695;
  // output folds reasoning: 39 + 7 = 46.
  assert.deepEqual(r.usage, {
    input_tokens: 3695,
    output_tokens: 46,
    cache_read_input_tokens: 17600,
    cache_creation_input_tokens: 5,
  })
  assert.equal(hops.length, 1)
  assert.equal(hops[0].model, 'bigmodel-coding-plan/GLM-5.2')
  assert.equal(hops[0].hopIndex, 1)
  // argv shape: wake flags from the POC contract
  const calls = readLog(f.log)
  assert.equal(calls.length, 1)
  const argv = calls[0].argv
  for (const expected of ['--cwd', f.home, '--mode', 'yolo', '--no-color', '--json', '-p', 'hello there']) {
    assert.ok(argv.includes(expected), `argv missing ${expected}`)
  }
  assert.equal(calls[0].cwd, f.home)
})

test('zcode run resumes the supplied session id and self-heals a stale one', { skip: IS_WIN }, async () => {
  const f = await fixture('stale')
  const adapter = getAdapter('zcode')
  const ac = new AbortController()
  const lines: string[] = []
  const r = await adapter.run({
    home: f.home, env: f.env, prompt: 'again', signal: ac.signal,
    resumeSessionId: 'sess_gone', onLog: (l) => lines.push(l),
  })
  // First call failed on the stale id, retry dropped --resume and succeeded
  // with a FRESH session id (not the stale one).
  assert.equal(r.exitCode, 0)
  assert.equal(r.sessionId, 'sess_zcode_new')
  assert.ok(lines.some((l) => l.includes('starting a fresh session')))
  const calls = readLog(f.log)
  assert.equal(calls.length, 2)
  assert.ok(calls[0].argv.includes('sess_gone'))
  assert.ok(!calls[1].argv.includes('--resume'))
})

test('zcode classify is read-only and reports usage + honest model', { skip: IS_WIN }, async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  const ac = new AbortController()
  const r = await adapter.classify({ cwd: f.home, env: f.env, prompt: 'triage this', signal: ac.signal })
  assert.equal(r.error, undefined)
  assert.equal(r.text, 'echo:triage this')
  assert.equal(r.model, 'bigmodel-coding-plan/GLM-5.2')
  assert.equal(r.usage?.cache_read_input_tokens, 17600)
  const argv = readLog(f.log)[0].argv
  assert.ok(argv.includes('plan'), 'classify must run --mode plan')
  const denyAt = argv.indexOf('--disallowed-tools')
  assert.ok(denyAt >= 0, 'classify must denylist mutating tools')
  const denied = String(argv[denyAt + 1]).split(' ')
  for (const tool of ['Bash', 'Edit', 'Write']) assert.ok(denied.includes(tool))
})

test('zcode run falls back to raw text when stdout is not the envelope', { skip: IS_WIN }, async () => {
  const f = await fixture('plain-text')
  const adapter = getAdapter('zcode')
  const ac = new AbortController()
  const r = await adapter.run({ home: f.home, env: f.env, prompt: 'x', signal: ac.signal, onLog: () => {} })
  assert.equal(r.exitCode, 0)
  assert.ok(!r.sessionId, 'no envelope → no session id')
})

test('zcode CUMORA_ZCODE_ARGS override keeps -p + resume but skips envelope parsing', { skip: IS_WIN }, async () => {
  const f = await fixture('plain-text')
  process.env.CUMORA_ZCODE_ARGS = '--my-flag'
  const adapter = getAdapter('zcode')
  const ac = new AbortController()
  const r = await adapter.run({
    home: f.home, env: { ...f.env, CUMORA_ZCODE_ARGS: '--my-flag' }, prompt: 'x', signal: ac.signal, onLog: () => {},
    resumeSessionId: 'sess_keep',
  })
  assert.equal(r.exitCode, 0)
  const argv = readLog(f.log)[0].argv
  assert.ok(argv.includes('--my-flag'))
  assert.ok(argv.includes('sess_keep'))
  assert.ok(argv.includes('-p'))
})

test('zcode launcher: CUMORA_ZCODE_BIN .cjs rides the daemon node, wrapper runs directly', () => {
  snapshotEnv()
  const empty = { HOME: '/nonexistent-zcode-home', PATH: '' } as NodeJS.ProcessEnv
  const l1 = resolveZcodeLauncher({ ...empty, CUMORA_ZCODE_BIN: '/opt/zcode.cjs' })
  assert.equal(l1?.command, process.execPath)
  assert.deepEqual(l1?.prefix, ['/opt/zcode.cjs'])
  const l2 = resolveZcodeLauncher({ ...empty, CUMORA_ZCODE_BIN: '/usr/local/bin/zcode-wrapper' })
  assert.equal(l2?.command, '/usr/local/bin/zcode-wrapper')
  assert.deepEqual(l2?.prefix, [])
  // A machine with no zcode at all resolves null. HOME points nowhere so the
  // AppImage branch can't hijack on a dev box that HAS ZCode installed.
  assert.equal(resolveZcodeLauncher(empty), null)
})

test('zcode launcher: PATH zcode-cli is found; a bare zcode GUI bin is never used', { skip: IS_WIN }, async () => {
  const f = await fixture()
  const l = resolveZcodeLauncher({ PATH: f.env.PATH, HOME: '/nonexistent-zcode-home' } as NodeJS.ProcessEnv)
  assert.equal(l?.source, 'PATH:zcode-cli')
  assert.equal(l?.prefix.length, 0)
  // A GUI `zcode` binary on PATH must not satisfy the launcher.
  const guiDir = join(f.root, 'gui')
  await mkdir(guiDir)
  await writeFile(join(guiDir, 'zcode'), '#!/bin/sh\nexit 0\n', 'utf8')
  await chmod(join(guiDir, 'zcode'), 0o755)
  assert.equal(resolveZcodeLauncher({ PATH: guiDir, HOME: '/nonexistent-zcode-home' } as NodeJS.ProcessEnv), null)
})

test('zcode launcher: AppImage is discovered via the desktop file and cached by content key', { skip: IS_WIN }, async () => {
  const f = await fixture()
  // Isolated HOME: no zcode-cli on PATH, a .desktop pointing at a fake AppImage
  // that honors --appimage-extract (and logs each extraction), and an isolated
  // cache root.
  const fakeHome = join(f.root, 'apphome')
  const apps = join(fakeHome, '.local', 'share', 'applications')
  await mkdir(apps, { recursive: true })
  const appDir = join(f.root, 'my apps with spaces')
  await mkdir(appDir)
  const appimage = join(appDir, 'fake.ZCode.AppImage')
  const extractLog = join(f.root, 'extract.log')
  await writeFile(appimage, `#!/bin/sh\nif [ "$1" = "--appimage-extract" ]; then echo x >> ${extractLog}; mkdir -p squashfs-root/resources/glm && echo "ok" > squashfs-root/resources/glm/zcode.cjs; fi\n`, 'utf8')
  await chmod(appimage, 0o755)
  // Quoted Exec path with spaces + a field code — the parse must honor both.
  await writeFile(join(apps, 'zcode.desktop'), `[Desktop Entry]\nExec="${appimage}" %U\n`, 'utf8')
  const cache = join(f.root, 'cache')
  const env = { HOME: fakeHome, XDG_CACHE_HOME: cache, PATH: '/usr/bin:/bin' } as NodeJS.ProcessEnv
  const l = resolveZcodeLauncher(env)
  assert.ok(l, 'launcher should resolve')
  assert.equal(l?.source, `appimage:${appimage}`)
  assert.equal(l?.command, process.execPath)
  assert.ok(l!.prefix[0].startsWith(join(cache, 'cumora', 'zcode')))
  assert.ok(existsSync(l!.prefix[0]), 'extracted zcode.cjs should exist')
  // Cache hit: same content key → same path AND no second extraction.
  const first = l!.prefix[0]
  const again = resolveZcodeLauncher(env)
  assert.equal(again?.prefix[0], first)
  let extractCount = readFileSync(extractLog, 'utf8').trim().split('\n').filter(Boolean).length
  assert.equal(extractCount, 1, `AppImage should be extracted exactly once, got ${extractCount}`)
  // Upgrade simulation: append to the AppImage (size+mtime change) → new
  // content key → the launcher re-extracts instead of serving the stale copy.
  await writeFile(appimage, `#!/bin/sh\nif [ "$1" = "--appimage-extract" ]; then echo x >> ${extractLog}; mkdir -p squashfs-root/resources/glm && echo "ok2" > squashfs-root/resources/glm/zcode.cjs; fi\nsleep 0.01\n`, 'utf8')
  await chmod(appimage, 0o755)
  const upgraded = resolveZcodeLauncher(env)
  assert.notEqual(upgraded?.prefix[0], first, 'upgraded AppImage must map to a new cache entry')
  extractCount = readFileSync(extractLog, 'utf8').trim().split('\n').filter(Boolean).length
  assert.equal(extractCount, 2, `upgrade must re-extract exactly once more, got ${extractCount}`)
})

test('zcode drift diagnosis: an Unknown-option failure carries an actionable hint', { skip: IS_WIN }, async () => {
  const f = await fixture('unknown-option')
  const adapter = getAdapter('zcode')
  const ac = new AbortController()
  const r = await adapter.run({ home: f.home, env: f.env, prompt: 'x', signal: ac.signal, onLog: () => {} })
  assert.equal(r.exitCode, 1)
  assert.match(r.error ?? '', /Unknown option/)
  assert.match(r.error ?? '', /CLI drift/)
  assert.match(r.error ?? '', /CUMORA_ZCODE_BIN/)
})

test('zcode version probe reads the CLI runtime version through the launcher', { skip: IS_WIN }, async () => {
  const f = await fixture()
  assert.equal(zcodeEngineVersion(f.env), '0.16.3')
  assert.equal(zcodeEngineVersion({ HOME: '/nonexistent-zcode-home', PATH: '/usr/bin:/bin' } as NodeJS.ProcessEnv), null)
})

test('zcode detection covers the CUMORA_ZCODE_BIN path', async () => {
  snapshotEnv()
  const f = await fixture()
  process.env.CUMORA_ZCODE_BIN = join(f.binDir, 'zcode-cli')
  const engines = await detectEngines()
  assert.ok(engines.includes('zcode'), `detectEngines should include zcode, got ${engines.join(',')}`)
})

test('zcode envelope parser: object passthrough, non-envelope falls back to text', () => {
  const { envelope, text } = parseZcodeEnvelope('  {"sessionId":"s1","response":"hi","usage":{"inputTokens":5}}  ')
  assert.equal(envelope?.sessionId, 's1')
  assert.equal(envelope?.response, 'hi')
  assert.ok(text.startsWith('{'))
  const raw = parseZcodeEnvelope('just some prose\n')
  assert.equal(raw.envelope, null)
  assert.equal(raw.text, 'just some prose')
  const other = parseZcodeEnvelope('{"unrelated":"json"}')
  assert.equal(other.envelope, null, 'a JSON object without envelope markers is raw text')
})

test('zcode readZcodeMainModel: absent/malformed config resolves null', async () => {
  // HOME must be pinned to a nonexistent dir: the fallback is the REAL
  // homedir, and a dev box with a logged-in zcode (~/.zcode/cli/config.json)
  // would otherwise flip this assertion — the test must not depend on
  // machine state.
  assert.equal(readZcodeMainModel({ HOME: '/nonexistent-zcode-home' } as NodeJS.ProcessEnv), null)
  // Malformed configs (unparseable JSON, or model.main not a string) must
  // fall through the same catch, not throw.
  const root = await mkdtemp(join(tmpdir(), 'cumora-engine-zcode-bad-'))
  tempDirs.push(root)
  await mkdir(join(root, '.zcode', 'cli'), { recursive: true })
  await writeFile(join(root, '.zcode', 'cli', 'config.json'), '{not json', 'utf8')
  assert.equal(readZcodeMainModel({ HOME: root } as NodeJS.ProcessEnv), null)
  await writeFile(join(root, '.zcode', 'cli', 'config.json'), JSON.stringify({ model: { main: 42 } }), 'utf8')
  assert.equal(readZcodeMainModel({ HOME: root } as NodeJS.ProcessEnv), null)
})

test('zcode seedHome pins the UI model via a project config with the provider copied in', async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  const proj = join(f.home, '.zcode', 'config.json')
  await adapter.seedHome(f.home, { id: 'a1', name: 'Nova', role: null, systemPrompt: null, model: 'kimi/k3', fastModel: 'kimi/kimi-for-coding' }, f.env)
  const written = JSON.parse(readFileSync(proj, 'utf8')) as { model?: { main?: string; lite?: string }; provider?: Record<string, { options?: { apiKey?: string } }> }
  assert.equal(written.model?.main, 'kimi/k3')
  assert.equal(written.model?.lite, 'kimi/kimi-for-coding')
  // provider tables do not merge across config layers — the entry (with its
  // apiKey) must be copied verbatim from the operator's user config.
  assert.equal(written.provider?.kimi?.options?.apiKey, 'k-test-1')
  // Attribution follows the project layer, not the user pin.
  assert.equal(readZcodeMainModel(f.env, f.home), 'kimi/k3')
  assert.equal(readZcodeMainModel(f.env), 'bigmodel-coding-plan/GLM-5.2')
})

test('zcode seedHome: unknown provider or malformed ref leaves the machine pin (and clears a stale override)', async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  const proj = join(f.home, '.zcode', 'config.json')
  await mkdir(join(f.home, '.zcode'), { recursive: true })
  await writeFile(proj, '{"model":{"main":"stale/old"}}', 'utf8')
  // no slash → malformed ref
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null, model: 'kimi-k3' }, f.env)
  assert.ok(!existsSync(proj), 'malformed ref must clear the stale project override')
  // provider absent from the operator's config
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null, model: 'nope/k3' }, f.env)
  assert.ok(!existsSync(proj), 'unknown provider must clear the stale project override')
  assert.equal(readZcodeMainModel(f.env, f.home), 'bigmodel-coding-plan/GLM-5.2')
  // model cleared in the UI → project override removed, machine pin back
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null, model: 'kimi/k3' }, f.env)
  assert.ok(existsSync(proj))
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null }, f.env)
  assert.ok(!existsSync(proj), 'clearing the model must remove the override')
})

test('zcode seedHome: cross-provider fast model copies both providers; missing fast provider drops lite only', async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  // main on kimi, fast on a second provider present in the user config
  await (async () => {
    const { writeFile } = await import('node:fs/promises')
    await writeFile(join(f.root, 'fakehome', '.zcode', 'cli', 'config.json'), JSON.stringify({
      model: { main: 'bigmodel-coding-plan/GLM-5.2' },
      provider: {
        kimi: { kind: 'anthropic', options: { apiKey: 'k-test-1', baseURL: 'https://api.kimi.com/coding' } },
        glm: { kind: 'anthropic', options: { apiKey: 'g-test-1', baseURL: 'https://open.bigmodel.cn/api/anthropic' } },
      },
    }), 'utf8')
  })()
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null, model: 'kimi/k3', fastModel: 'glm/glm-4-flash' }, f.env)
  let written = JSON.parse(readFileSync(join(f.home, '.zcode', 'config.json'), 'utf8')) as { model?: { main?: string; lite?: string }; provider?: Record<string, unknown> }
  assert.equal(written.model?.lite, 'glm/glm-4-flash')
  assert.ok(written.provider?.kimi && written.provider?.glm, 'both providers must be copied for a cross-provider lite pin')
  // fast provider absent from the operator's config → lite dropped, main pin intact
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null, model: 'kimi/k3', fastModel: 'nope/mini' }, f.env)
  written = JSON.parse(readFileSync(join(f.home, '.zcode', 'config.json'), 'utf8'))
  assert.equal(written.model?.lite, undefined, 'unresolvable fast provider must drop lite')
  assert.equal(written.model?.main, 'kimi/k3')
  assert.ok(written.provider?.kimi && !written.provider?.nope)
  // no fastModel at all → no lite key
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null, model: 'kimi/k3' }, f.env)
  written = JSON.parse(readFileSync(join(f.home, '.zcode', 'config.json'), 'utf8'))
  assert.equal(written.model?.lite, undefined)
})

test('zcode project config is written 0600 (it carries the provider apiKey)', async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  await adapter.seedHome(f.home, { id: 'a1', name: 'N', role: null, systemPrompt: null, model: 'kimi/k3' }, f.env)
  const st = statSync(join(f.home, '.zcode', 'config.json'))
  assert.equal(st.mode & 0o077, 0, `project config must not be group/world readable, got ${(st.mode & 0o777).toString(8)}`)
})

test('zcode run attributes the turn to the project-pinned model', { skip: IS_WIN }, async () => {
  const f = await fixture()
  const adapter = getAdapter('zcode')
  await adapter.seedHome(f.home, { id: 'a1', name: 'Nova', role: null, systemPrompt: null, model: 'kimi/k3' }, f.env)
  const ac = new AbortController()
  const hops: EngineHopReport[] = []
  const r = await adapter.run({
    home: f.home, env: f.env, prompt: 'hi', signal: ac.signal,
    onLog: () => {}, onHopUsage: (h) => hops.push(h),
  })
  assert.equal(r.exitCode, 0)
  assert.equal(r.model, 'kimi/k3')
  assert.equal(hops[0]?.model, 'kimi/k3')
})

/**
 * REAL-engine smoke (opt-in — it spends the operator's tokens). Skipped
 * unless BOTH CUMORA_ZCODE_SMOKE=1 and CUMORA_ZCODE_BIN are set:
 *
 *   CUMORA_ZCODE_SMOKE=1 CUMORA_ZCODE_BIN=<path to zcode.cjs> \
 *     node --import tsx --test server/src/__tests__/agents-computer-engine-zcode.test.ts
 *
 * Requires a logged-in CLI (~/.zcode/cli/config.json with a model.main —
 * see docs/byoa-zcode-notes.md for the bootstrap). Pins the cross-process
 * contract the fakes can only imitate: a real envelope, a stable session id
 * across --resume, and real usage numbers.
 */
test('zcode REAL smoke: envelope, resume continuity, usage', { skip: !(process.env.CUMORA_ZCODE_SMOKE === '1' && process.env.CUMORA_ZCODE_BIN) }, async () => {
  snapshotEnv()
  const root = await mkdtemp(join(tmpdir(), 'cumora-engine-zcode-real-'))
  tempDirs.push(root)
  const adapter = getAdapter('zcode')
  const ac = new AbortController()
  const env: NodeJS.ProcessEnv = { ...process.env }
  const codeword = `SMOKE-${Date.now().toString(36)}`
  const r1 = await adapter.run({ home: root, env, prompt: `Remember this codeword: ${codeword}. Reply with exactly: noted`, signal: ac.signal, onLog: () => {} })
  assert.equal(r1.exitCode, 0, r1.error)
  assert.ok(r1.sessionId?.startsWith('sess_'), `sessionId should look like a session id, got ${r1.sessionId}`)
  assert.ok(r1.usage && (r1.usage.input_tokens! > 0 || r1.usage.output_tokens! > 0), 'real usage expected')
  assert.ok(r1.model, 'model attribution from config expected')
  assert.notEqual(r1.model, 'zcode-unknown-model', 'config must be readable — run the CLI login bootstrap first')
  const r2 = await adapter.run({ home: root, env, prompt: 'Reply with only the codeword I told you.', signal: ac.signal, resumeSessionId: r1.sessionId, onLog: () => {} })
  assert.equal(r2.exitCode, 0, r2.error)
  assert.equal(r2.sessionId, r1.sessionId, 'resumed turn must keep the session id')
})
