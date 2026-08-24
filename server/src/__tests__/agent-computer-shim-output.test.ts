/**
 * The BYOA `cumora` shim must hand the engine the WHOLE CLI result.
 *
 * stdout on a pipe is asynchronous in Node, so `process.exit()` on the line
 * after `process.stdout.write()` discards whatever is still buffered. The engine
 * always runs the shim with stdout piped, so this silently truncated every large
 * result at the pipe buffer — with exit code 0 and empty stderr, which is why
 * nothing ever surfaced it.
 *
 * These run the REAL shim text against a REAL pipe, since that is the only place
 * the bug exists.
 *
 * Run: node --import tsx --test server/src/__tests__/agent-computer-shim-output.test.ts
 */
import { execFile, spawn } from 'node:child_process'
import { createServer, type Server } from 'node:http'
import { mkdtemp, writeFile, chmod, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { delimiter, join } from 'node:path'
import { afterEach, test } from 'node:test'
import { promisify } from 'node:util'
import assert from 'node:assert/strict'
import {
  CUMORA_SHIM,
  CUMORA_WINDOWS_SHIM,
  prependAgentBinToPath,
  writeShim,
} from '../agents/computer/daemon.js'

const execFileP = promisify(execFile)

const cleanup: Array<() => Promise<void> | void> = []

afterEach(async () => {
  for (const fn of cleanup.splice(0)) await fn()
})

/** A stand-in for /runtime/cli that returns a payload of the requested size. */
async function serveCli(payload: string, exitCode = 0): Promise<string> {
  const server: Server = createServer((_req, res) => {
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({ text: payload, exitCode, ok: exitCode === 0, sideEffects: [] }))
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  cleanup.push(() => new Promise<void>((resolve) => { server.close(() => resolve()) }))
  const addr = server.address()
  if (!addr || typeof addr === 'string') throw new Error('no port')
  return `http://127.0.0.1:${addr.port}`
}

/** Write the real shim to disk and run it with stdout on a PIPE — the way the
 *  engine's bash tool always invokes it. */
async function runShim(url: string): Promise<{ stdoutBytes: number; exitCode: number; stderr: string }> {
  const dir = await mkdtemp(join(tmpdir(), 'cumora-shim-'))
  cleanup.push(() => rm(dir, { recursive: true, force: true }))
  const shim = join(dir, 'cumora.js')
  await writeFile(shim, CUMORA_SHIM, 'utf8')
  await chmod(shim, 0o755)

  return new Promise((resolve) => {
    const child = spawn(process.execPath, [shim, 'inbox'], {
      stdio: ['ignore', 'pipe', 'pipe'],
      env: {
        ...process.env,
        CUMORA_AGENT_RUNTIME_URL: url,
        CUMORA_AGENT_RUNTIME_TOKEN: 'test-token',
        CUMORA_AGENT_RUNTIME_TOKEN_FILE: '',
      },
    })
    let stdoutBytes = 0
    let stderr = ''
    child.stdout.on('data', (b: Buffer) => { stdoutBytes += b.length })
    child.stderr.on('data', (b: Buffer) => { stderr += b.toString('utf8') })
    child.on('close', (code) => resolve({ stdoutBytes, exitCode: code ?? -1, stderr }))
  })
}

test('the shim writes the whole CLI payload before exiting', async () => {
  // 1MB cannot fit any pipe buffer, so this is deterministic — there is no size
  // at which the old write-then-exit accidentally passed.
  const payload = 'x'.repeat(1_000_000)
  const url = await serveCli(payload)
  const r = await runShim(url)
  assert.equal(
    r.stdoutBytes, payload.length + 1,
    'stdout must carry the full text plus its trailing newline, not just the pipe buffer',
  )
  assert.equal(r.exitCode, 0)
  assert.equal(r.stderr, '')
})

test('a small payload still round-trips unchanged', async () => {
  const payload = 'no unread messages'
  const url = await serveCli(payload)
  const r = await runShim(url)
  assert.equal(r.stdoutBytes, payload.length + 1)
  assert.equal(r.exitCode, 0)
})

test('the CLI exit code still propagates, with a large payload', async () => {
  // The exit code must survive being moved into the write callback.
  const payload = 'y'.repeat(300_000)
  const url = await serveCli(payload, 2)
  const r = await runShim(url)
  assert.equal(r.stdoutBytes, payload.length + 1)
  assert.equal(r.exitCode, 2, 'a non-zero CLI exit code must still reach the engine')
})

test('an empty payload exits without writing', async () => {
  const url = await serveCli('', 1)
  const r = await runShim(url)
  assert.equal(r.stdoutBytes, 0)
  assert.equal(r.exitCode, 1)
})

test('agent bin is prepended with the platform PATH delimiter', () => {
  const inherited = ['first', 'second'].join(delimiter)
  assert.equal(
    prependAgentBinToPath('agent-bin', inherited),
    ['agent-bin', 'first', 'second'].join(delimiter),
  )
  assert.equal(prependAgentBinToPath('agent-bin', ''), 'agent-bin')
})

test('writeShim emits a Windows command launcher beside the shared Node shim', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'cumora-windows-shim-'))
  cleanup.push(() => rm(dir, { recursive: true, force: true }))

  await writeShim(dir, 'win32')

  assert.equal(await readFile(join(dir, 'cumora'), 'utf8'), CUMORA_SHIM)
  assert.equal(await readFile(join(dir, 'cumora.cmd'), 'utf8'), CUMORA_WINDOWS_SHIM)
})

test('PowerShell resolves cumora from the injected PATH and forwards arguments', {
  skip: process.platform !== 'win32',
}, async () => {
  let receivedAuthorization = ''
  let receivedArgv: string[] | undefined
  const server: Server = createServer(async (req, res) => {
    receivedAuthorization = req.headers.authorization ?? ''
    let body = ''
    for await (const chunk of req) body += chunk.toString()
    receivedArgv = (JSON.parse(body) as { argv: string[] }).argv
    res.writeHead(200, { 'content-type': 'application/json' })
    res.end(JSON.stringify({ text: 'sent from test', exitCode: 0, ok: true, sideEffects: [] }))
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  cleanup.push(() => new Promise<void>((resolve) => { server.close(() => resolve()) }))
  const addr = server.address()
  if (!addr || typeof addr === 'string') throw new Error('no port')

  const dir = await mkdtemp(join(tmpdir(), 'cumora-powershell-shim-'))
  cleanup.push(() => rm(dir, { recursive: true, force: true }))
  await writeShim(dir)

  const { stdout, stderr } = await execFileP(
    'powershell.exe',
    ['-NoProfile', '-NonInteractive', '-Command', "cumora reply direct-test 'hello from windows'"],
    {
      env: {
        ...process.env,
        PATH: prependAgentBinToPath(dir),
        CUMORA_AGENT_RUNTIME_URL: `http://127.0.0.1:${addr.port}`,
        CUMORA_AGENT_RUNTIME_TOKEN: 'test-token',
        CUMORA_AGENT_RUNTIME_TOKEN_FILE: '',
      },
    },
  )

  assert.equal(stdout.trim(), 'sent from test')
  assert.equal(stderr, '')
  assert.equal(receivedAuthorization, 'Bearer test-token')
  assert.deepEqual(receivedArgv, ['reply', 'direct-test', 'hello from windows'])
})
