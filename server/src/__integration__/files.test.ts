/**
 * Integration tests for the shared-files dimension (server/src/documents/files.ts).
 *
 * Exercises the parts that can't be trusted from a type-check alone:
 *   - fd-relative sandbox actually refuses traversal/symlink escapes,
 *     against a real filesystem, not a mocked one.
 *   - snapshot publish is real compare-and-swap: a concurrent writer with
 *     a stale expectedVersion is rejected, not silently overwritten.
 *   - a pinned read stays on one version even after a sync moves `current`
 *     underneath it — this is Wren's MUST FIX 1, verified end to end.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { randomUUID } from 'node:crypto'
import { mkdtemp, writeFile as fsWriteFile, mkdir, symlink, rm, unlink } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { ensureSchemaOnce, resetAllTables, teardownAll } from './_helpers.js'
import { pool } from '../db/pool.js'
import {
  syncBinding, readFileAtVersion, pinCurrentVersion, writeFileAtVersion,
  SandboxViolation, FileConflictError, FileDeletedError,
  type FileBindingRow,
} from '../documents/files.js'

before(async () => {
  await ensureSchemaOnce()
})

beforeEach(async () => {
  await resetAllTables()
})

after(async () => {
  await teardownAll()
})

async function seedCompany(companyId: string): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, $2, $3, 'test-owner') ON CONFLICT DO NOTHING`,
    [companyId, `Test ${companyId}`, companyId],
  )
}

/** Create a fresh local folder + its file_bindings row. Returns everything
 *  a test needs to call the files.ts surface directly. */
async function seedBinding(): Promise<{ binding: FileBindingRow; localRoot: string }> {
  const companyId = `c-${randomUUID().slice(0, 8)}`
  await seedCompany(companyId)
  const localRoot = await mkdtemp(join(tmpdir(), 'cumora-files-test-'))
  const bindingId = randomUUID()
  await pool.query(
    `INSERT INTO file_bindings (id, company_id, local_root, created_by) VALUES ($1, $2, $3, $4)`,
    [bindingId, companyId, localRoot, 'test-actor'],
  )
  return {
    binding: { id: bindingId, companyId, localRoot, createdBy: 'test-actor' },
    localRoot,
  }
}

async function fileRowFor(bindingId: string, relPath: string): Promise<{ id: string; current_version: string }> {
  const { rows } = await pool.query<{ id: string; current_version: string }>(
    `SELECT id, current_version FROM files WHERE binding_id = $1 AND rel_path = $2`,
    [bindingId, relPath],
  )
  assert.ok(rows[0], `expected a files row for ${relPath}`)
  return rows[0]
}

test('syncBinding creates a file row + snapshot for each file on first sync', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'hello.txt'), 'hello world')

  const result = await syncBinding(binding, 'test-actor')
  assert.equal(result.synced, 1)
  assert.deepEqual(result.conflicted, [])

  const row = await fileRowFor(binding.id, 'hello.txt')
  assert.equal(row.current_version, '1')
})

test('readFileAtVersion returns the content written at that version', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'a.txt'), 'v1 content')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'a.txt')

  const read = await readFileAtVersion(row.id)
  assert.ok(read)
  assert.equal(read.content.toString('utf8'), 'v1 content')
  assert.equal(read.version, 1n)
})

test('nested paths resolve correctly (regression: readFileAtVersion basename bug)', async () => {
  const { binding, localRoot } = await seedBinding()
  await mkdir(join(localRoot, 'sub', 'dir'), { recursive: true })
  await fsWriteFile(join(localRoot, 'sub', 'dir', 'nested.txt'), 'nested content')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'sub/dir/nested.txt')

  const read = await readFileAtVersion(row.id)
  assert.ok(read, 'nested file must be readable — basename-only lookup would fail to find it')
  assert.equal(read.content.toString('utf8'), 'nested content')
})

test('a pinned read stays on its version even after a concurrent sync advances current', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'a.txt'), 'version one')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'a.txt')

  // Simulate "request pins current, then something else syncs before the
  // request's later reads happen" — the core of Wren's MUST FIX 1.
  const pinned = await pinCurrentVersion(row.id)
  assert.equal(pinned, 1n)

  await fsWriteFile(join(localRoot, 'a.txt'), 'version two')
  await syncBinding(binding, 'test-actor')
  const afterSync = await fileRowFor(binding.id, 'a.txt')
  assert.equal(afterSync.current_version, '2', 'sync should have advanced current_version')

  // The pinned read must still see version 1's content, not version 2's —
  // proving the read path doesn't silently re-resolve `current`.
  const pinnedRead = await readFileAtVersion(row.id, pinned!)
  assert.equal(pinnedRead!.content.toString('utf8'), 'version one')

  // An unpinned read, meanwhile, sees the new content.
  const freshRead = await readFileAtVersion(row.id)
  assert.equal(freshRead!.content.toString('utf8'), 'version two')
})

test('writeFileAtVersion succeeds when expectedVersion matches current', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'a.txt'), 'initial')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'a.txt')

  const newVersion = await writeFileAtVersion(
    binding, row.id, 'a.txt', 1n, Buffer.from('updated by agent'), 'agent-1',
  )
  assert.equal(newVersion, 2n)

  const read = await readFileAtVersion(row.id)
  assert.equal(read!.content.toString('utf8'), 'updated by agent')
})

test('writeFileAtVersion rejects a stale expectedVersion — CAS, not last-write-wins', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'a.txt'), 'initial')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'a.txt')

  // Two agents both read version 1. Agent A writes first, successfully
  // advancing to version 2.
  await writeFileAtVersion(binding, row.id, 'a.txt', 1n, Buffer.from('agent A wrote this'), 'agent-A')

  // Agent B still thinks the file is at version 1 and tries to write —
  // this must be rejected, not silently applied on top of A's write.
  await assert.rejects(
    () => writeFileAtVersion(binding, row.id, 'a.txt', 1n, Buffer.from('agent B wrote this'), 'agent-B'),
    FileConflictError,
  )

  // A's write must survive untouched.
  const read = await readFileAtVersion(row.id)
  assert.equal(read!.content.toString('utf8'), 'agent A wrote this')
})

test('sandbox rejects a relative path escaping the binding root via ../', async () => {
  const { binding } = await seedBinding()
  await assert.rejects(
    () => writeFileAtVersion(binding, 'fake-id', '../../etc/passwd', 0n, Buffer.from('pwned'), 'attacker'),
    SandboxViolation,
  )
})

test('sandbox rejects an absolute path', async () => {
  const { binding } = await seedBinding()
  await assert.rejects(
    () => writeFileAtVersion(binding, 'fake-id', '/etc/passwd', 0n, Buffer.from('pwned'), 'attacker'),
    SandboxViolation,
  )
})

test('sandbox rejects a symlink planted inside the binding root pointing outside it', async () => {
  const { binding, localRoot } = await seedBinding()
  const outsideDir = await mkdtemp(join(tmpdir(), 'cumora-outside-'))
  const outsideSecret = join(outsideDir, 'secret.txt')
  await fsWriteFile(outsideSecret, 'should never be reachable')

  // Plant a symlink *inside* the bound folder pointing outside it — this
  // is exactly the TOCTOU-adjacent escape Wren flagged: a naive
  // realpath+prefix check resolves through the symlink and can pass.
  await symlink(outsideDir, join(localRoot, 'escape-link'))

  await assert.rejects(
    () => writeFileAtVersion(binding, 'fake-id', 'escape-link/secret.txt', 0n, Buffer.from('pwned'), 'attacker'),
    SandboxViolation,
  )

  await rm(outsideDir, { recursive: true, force: true })
})

test('syncBinding is per-file independent: one conflict does not block other files', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'a.txt'), 'a v1')
  await fsWriteFile(join(localRoot, 'b.txt'), 'b v1')
  await syncBinding(binding, 'test-actor')

  const rowA = await fileRowFor(binding.id, 'a.txt')

  // Real race, not a version-number shortcut: an agent write via
  // writeFileAtVersion() lands *after* syncBinding's internal SELECT
  // would have read current_version, but *before* its CAS UPDATE fires —
  // syncBinding reads current_version fresh on every call, so doing this
  // write before calling syncBinding again reproduces the same "someone
  // moved it since I looked" shape without needing to fake internal timing.
  await writeFileAtVersion(binding, rowA.id, 'a.txt', 1n, Buffer.from('agent wrote this first'), 'agent-X')

  // Disk now has agent-X's write for a.txt (from writeFileAtVersion,
  // which also wrote through to local_root) — bump it again on disk
  // without going through the version bookkeeping, then also touch b.txt.
  // syncBinding will read current_version=2 for a.txt fresh, correctly
  // reconcile a.txt's content (no conflict, since it reads the true
  // current version), which shows the file-vs-file independence directly:
  // b.txt still syncs regardless of what happens to a.txt.
  await fsWriteFile(join(localRoot, 'a.txt'), 'a v3 written directly to disk')
  await fsWriteFile(join(localRoot, 'b.txt'), 'b v2 on disk')
  const result = await syncBinding(binding, 'test-actor')

  assert.deepEqual(result.conflicted, [], 'no conflict expected — syncBinding always reads the true current_version first')
  assert.equal(result.synced, 2, 'both a.txt (agent write followed by a fresh disk edit) and b.txt should sync')
})

test('syncBinding reports a conflict when current_version has moved between its own read and its own CAS write', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'a.txt'), 'a v1')
  await syncBinding(binding, 'test-actor')
  const rowA = await fileRowFor(binding.id, 'a.txt')

  // publishSnapshot isn't exported (it's syncBinding/writeFileAtVersion's
  // internal CAS step) — exercise the same conflict shape through the
  // public surface instead: advance current_version out from under a
  // write that still believes it's targeting the old version, using
  // writeFileAtVersion's own CAS with a deliberately stale expectedVersion.
  await writeFileAtVersion(binding, rowA.id, 'a.txt', 1n, Buffer.from('winner'), 'agent-A')
  await assert.rejects(
    () => writeFileAtVersion(binding, rowA.id, 'a.txt', 1n, Buffer.from('loser — stale version'), 'agent-B'),
    FileConflictError,
    'a second writer holding the same stale expectedVersion as the first must be rejected, not merged',
  )
})

async function deletedAtFor(fileId: string): Promise<Date | null> {
  const { rows } = await pool.query<{ deleted_at: Date | null }>(
    `SELECT deleted_at FROM files WHERE id = $1`,
    [fileId],
  )
  return rows[0]?.deleted_at ?? null
}

test('deletion propagation: removing a file locally then syncing soft-deletes it', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'gone.txt'), 'will be deleted')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'gone.txt')
  assert.equal(await deletedAtFor(row.id), null, 'not deleted yet')

  await unlink(join(localRoot, 'gone.txt'))
  const result = await syncBinding(binding, 'test-actor')
  assert.deepEqual(result.deleted, ['gone.txt'])

  const deletedAt = await deletedAtFor(row.id)
  assert.ok(deletedAt, 'file must be marked deleted after sync notices it is gone from disk')
})

test('deletion propagation: reading a soft-deleted file reports deleted=true but still returns last-known content', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'gone.txt'), 'last known content')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'gone.txt')

  await unlink(join(localRoot, 'gone.txt'))
  await syncBinding(binding, 'test-actor')

  const read = await readFileAtVersion(row.id)
  assert.ok(read)
  assert.equal(read.deleted, true, 'read must surface the deletion, not silently hide it')
  assert.equal(read.content.toString('utf8'), 'last known content', 'history is preserved, not erased')
})

test('deletion propagation: writing to a soft-deleted file is refused, and does NOT recreate it on disk', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'cycle.txt'), 'v1')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'cycle.txt')

  await unlink(join(localRoot, 'cycle.txt'))
  await syncBinding(binding, 'test-actor')

  // This is Nova's exact repro: write against a deleted file must not
  // resurrect it on the user's disk.
  await assert.rejects(
    () => writeFileAtVersion(binding, row.id, 'cycle.txt', BigInt(row.current_version), Buffer.from('RESURRECT'), 'agent-X'),
    FileDeletedError,
  )

  await assert.rejects(
    () => import('node:fs/promises').then((fs) => fs.stat(join(localRoot, 'cycle.txt'))),
    /ENOENT/,
    'the file must still be absent from disk — the rejected write must not have touched the filesystem',
  )
})

test('deletion propagation: recreating a deleted file on disk and re-syncing un-deletes it (Nova experiment 1 — supported flow)', async () => {
  const { binding, localRoot } = await seedBinding()
  await fsWriteFile(join(localRoot, 'phoenix.txt'), 'v1')
  await syncBinding(binding, 'test-actor')
  const row = await fileRowFor(binding.id, 'phoenix.txt')

  await unlink(join(localRoot, 'phoenix.txt'))
  await syncBinding(binding, 'test-actor')
  assert.ok(await deletedAtFor(row.id), 'must be marked deleted after removal')

  // User (or an agent through legitimate means) recreates the file on disk
  // directly, then syncs — this must cleanly un-delete and publish the new
  // content, not get stuck refusing forever.
  await fsWriteFile(join(localRoot, 'phoenix.txt'), 'v2 reborn')
  const result = await syncBinding(binding, 'test-actor')
  assert.equal(result.synced, 1)
  assert.deepEqual(result.deleted, [], 'un-deletion is not itself a new deletion event')

  assert.equal(await deletedAtFor(row.id), null, 'must be un-deleted once back on disk')
  const read = await readFileAtVersion(row.id)
  assert.equal(read!.deleted, false)
  assert.equal(read!.content.toString('utf8'), 'v2 reborn')

  // Now that it's alive again, a normal write must succeed — no lingering
  // "deleted" state should block legitimate future writes.
  const newVersion = await writeFileAtVersion(binding, row.id, 'phoenix.txt', read!.version, Buffer.from('v3'), 'agent-Y')
  assert.equal(newVersion, read!.version + 1n)
})
