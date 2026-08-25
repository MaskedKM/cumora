/**
 * CLI → disk integration tests for `cumora bind` / `cumora files ...`.
 *
 * Distinct from files.test.ts, which drives ../documents/files.ts directly
 * (openat/CAS layer). This file drives the same behavior through
 * `runCli(argv)` — real argv parsing, real routing dispatch, real agent
 * identity resolution — because bug #1 in files.test.ts (ELOOP vs ENOTDIR)
 * proved that a correct design + a correct low-level implementation can
 * still have gaps that only surface once every intermediate layer is
 * exercised for real. Nova's acceptance checklist calls this out
 * explicitly: the openat-layer tests don't cover the argv-parsing/routing
 * layer in between.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, writeFile as fsWriteFile, symlink, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pool } from '../db/pool.js'
import { runCli } from '../agents/cli.js'
import { ensureSchemaOnce, resetAllTables, seedCompanyWithAgent, teardownAll } from './_helpers.js'

before(async () => { await ensureSchemaOnce() })
beforeEach(async () => { await resetAllTables() })
after(async () => { await teardownAll() })

test('files ls reports "unbound" rather than crashing when no folder is bound (verification standard #0)', async () => {
  const { agentId } = await seedCompanyWithAgent()
  const res = await runCli(['--as', agentId, 'files', 'ls'])
  assert.equal(res.ok, false)
  assert.match(res.text, /no folder bound/i)
})

test('bind → files sync → files ls → files read → files write round-trips through the CLI', async () => {
  const { agentId } = await seedCompanyWithAgent()
  const localRoot = await mkdtemp(join(tmpdir(), 'cumora-cli-test-'))
  await fsWriteFile(join(localRoot, 'notes.md'), '# hello from disk')

  const bindRes = await runCli(['--as', agentId, 'bind', localRoot])
  assert.equal(bindRes.ok, true)

  const syncRes = await runCli(['--as', agentId, 'files', 'sync'])
  assert.equal(syncRes.ok, true)
  assert.match(syncRes.text, /synced 1/)

  const lsRes = await runCli(['--as', agentId, 'files', 'ls'])
  assert.equal(lsRes.ok, true)
  assert.match(lsRes.text, /notes\.md/)

  const readRes = await runCli(['--as', agentId, 'files', 'read', 'notes.md'])
  assert.equal(readRes.ok, true)
  assert.match(readRes.text, /# hello from disk/)
  const versionMatch = readRes.text.match(/v(\d+)/)
  assert.ok(versionMatch, 'read output should include the version')

  const writeRes = await runCli([
    '--as', agentId, 'files', 'write', 'notes.md', '--version', versionMatch![1], 'updated via cumora files write',
  ])
  assert.equal(writeRes.ok, true, writeRes.text)

  const rereadRes = await runCli(['--as', agentId, 'files', 'read', 'notes.md'])
  assert.match(rereadRes.text, /updated via cumora files write/)

  await rm(localRoot, { recursive: true, force: true })
})

test('files write rejects a stale --version through the full CLI path', async () => {
  const { agentId } = await seedCompanyWithAgent()
  const localRoot = await mkdtemp(join(tmpdir(), 'cumora-cli-test-'))
  await fsWriteFile(join(localRoot, 'a.txt'), 'v1')
  await runCli(['--as', agentId, 'bind', localRoot])
  await runCli(['--as', agentId, 'files', 'sync'])

  // First writer succeeds at version 1.
  const first = await runCli(['--as', agentId, 'files', 'write', 'a.txt', '--version', '1', 'winner'])
  assert.equal(first.ok, true, first.text)

  // Second writer still thinks it's version 1 — must be rejected via the
  // CLI's own error path (HELD — write NOT applied), not silently merged.
  const second = await runCli(['--as', agentId, 'files', 'write', 'a.txt', '--version', '1', 'loser'])
  assert.equal(second.ok, false)
  assert.match(second.text, /HELD/)

  await rm(localRoot, { recursive: true, force: true })
})

test('files read rejects a path that traverses out of the binding via ../, through argv parsing', async () => {
  const { agentId } = await seedCompanyWithAgent()
  const localRoot = await mkdtemp(join(tmpdir(), 'cumora-cli-test-'))
  await runCli(['--as', agentId, 'bind', localRoot])

  // Not tracked (never synced), so the CLI's own "not found" path fires
  // first — this confirms an untracked/malicious path never reaches the
  // sandbox layer with attacker-controlled trust, it's rejected earlier.
  const res = await runCli(['--as', agentId, 'files', 'read', '../../etc/passwd'])
  assert.equal(res.ok, false)

  await rm(localRoot, { recursive: true, force: true })
})

test('D1: a sibling directory whose name is a string-prefix of the binding root is not treated as inside it', async () => {
  // This is the exact class of bug a naive `resolved.startsWith(root)`
  // check misses: "/tmp/cumora-cli-test-ABC" is a string-prefix of
  // "/tmp/cumora-cli-test-ABCD", but is NOT a subdirectory of it — it's a
  // sibling. Reachability of files under the sibling would mean the
  // sandbox has a prefix-confusion hole.
  const { agentId } = await seedCompanyWithAgent()
  const localRoot = await mkdtemp(join(tmpdir(), 'cumora-cli-test-'))
  const siblingRoot = `${localRoot}-evil-sibling`
  await import('node:fs/promises').then((fs) => fs.mkdir(siblingRoot, { recursive: true }))
  await fsWriteFile(join(siblingRoot, 'secret.txt'), 'should not be reachable')

  await runCli(['--as', agentId, 'bind', localRoot])

  // Attempt to reach the sibling by walking "up and back down" through a
  // relative path — the only way an attacker could try to abuse a
  // string-prefix bug via a relative-path API surface (there is no way to
  // literally pass an absolute path to the sibling and have it accepted;
  // the CLI's own "not found" for untracked paths is not the interesting
  // assertion here — what matters is the underlying sandbox never resolves
  // this to a real read of secret.txt).
  const traversal = `../${siblingRoot.split('/').pop()}/secret.txt`
  const res = await runCli(['--as', agentId, 'files', 'read', traversal])
  assert.equal(res.ok, false, 'must not read the sibling directory\'s file')
  assert.doesNotMatch(res.text, /should not be reachable/)

  await rm(localRoot, { recursive: true, force: true })
  await rm(siblingRoot, { recursive: true, force: true })
})

test('files write refuses to write outside the binding via a relative path that escapes it', async () => {
  const { agentId } = await seedCompanyWithAgent()
  const localRoot = await mkdtemp(join(tmpdir(), 'cumora-cli-test-'))
  await fsWriteFile(join(localRoot, 'tracked.txt'), 'v1')
  await runCli(['--as', agentId, 'bind', localRoot])
  await runCli(['--as', agentId, 'files', 'sync'])

  // Not a tracked path, so this exercises the CLI's own not-found gate —
  // paired with files.test.ts's direct writeFileAtVersion sandbox test,
  // this confirms the same class of input is rejected at both layers,
  // not just the one closer to the syscall.
  const res = await runCli(['--as', agentId, 'files', 'write', '../outside.txt', '--version', '0', 'pwned'])
  assert.equal(res.ok, false)

  await rm(localRoot, { recursive: true, force: true })
})

test('after all CLI operations, no new files appear outside the bound local_root', async () => {
  const { agentId } = await seedCompanyWithAgent()
  const localRoot = await mkdtemp(join(tmpdir(), 'cumora-cli-test-'))
  await fsWriteFile(join(localRoot, 'a.txt'), 'v1')
  await runCli(['--as', agentId, 'bind', localRoot])
  await runCli(['--as', agentId, 'files', 'sync'])
  await runCli(['--as', agentId, 'files', 'write', 'a.txt', '--version', '1', 'v2'])
  // Attack attempts — all expected to fail, but run them anyway so this
  // test proves they leave no trace even when rejected.
  await runCli(['--as', agentId, 'files', 'read', '../../etc/passwd'])
  await runCli(['--as', agentId, 'files', 'write', '../escape.txt', '--version', '0', 'pwned'])

  const outsideDir = tmpdir()
  const { readdir } = await import('node:fs/promises')
  const entries = await readdir(outsideDir)
  const suspicious = entries.filter((e) => e === 'escape.txt' || e === 'passwd')
  assert.deepEqual(suspicious, [], 'no attacker-named file should have been created outside the binding')

  await rm(localRoot, { recursive: true, force: true })
})
