/**
 * Shared files — team-visible, read-write view onto a folder on one
 * member's local disk ("binding").
 *
 * Unlike documents (Yjs CRDTs — every edit is mergeable, nothing ever
 * conflicts), this is a strong-consistency model: every write names the
 * version it was read at, and the write is a no-op if that version has
 * since moved. The caller sees that as a conflict, not a silent merge.
 *
 * The binding's local_root and this server's snapshot store are the same
 * physical disk (this project runs single-host — server and the bound
 * folder share a filesystem), so a "snapshot" is a real, immutable copy
 * of the bound folder's content at one moment, not a network transfer.
 *
 * Publish protocol (why this order, not simpler):
 *   1. Read `local_root` fully into a fresh temp directory. Files unchanged
 *      since the last known-good snapshot are hardlinked, not copied —
 *      cheap, and safe only because snapshot content is immutable (see
 *      `assertImmutableSnapshotTarget`).
 *   2. Hash the temp directory's full tree into one composite digest.
 *   3. `rename()` the temp directory into its final `snapshots/<id>/`
 *      path — one atomic syscall, the only operation that must be
 *      indivisible.
 *   4. CAS-publish: `UPDATE files SET current_version = current_version + 1
 *      WHERE id = $1 AND current_version = $2`. Zero rows updated means a
 *      concurrent writer already advanced the version — this publish is
 *      abandoned, not retried blindly.
 * Hash and rename happen before the version pointer moves, in the same
 * transaction as the CAS update, so a reader can never observe a new
 * version number paired with stale or partial content.
 *
 * Retention: `current` and the version immediately before it (plus any
 * version an in-flight request has pinned — see `pinSnapshot`) are never
 * garbage collected. This is what makes "read stays consistent across a
 * concurrent sync" possible without locking readers.
 */
import { constants as fsConstants, realpathSync } from 'node:fs'
import {
  open as fsOpen, mkdir, rename, rm, readdir, stat, link, readFile, writeFile,
} from 'node:fs/promises'
import { join, resolve, sep } from 'node:path'
import { randomUUID, createHash } from 'node:crypto'
import { pool } from '../db/pool.js'
import type { PoolClient } from 'pg'
import { env } from '../env.js'

/* ============== bind-time allowlist (Wren MUST FIX 1) ============== *
 *
 * A binding's local_root must resolve, via a REAL filesystem realpath (not
 * a string-prefix check on the raw input), inside one of
 * env.FILE_BINDING_ALLOWLIST_ROOTS. Fail closed: an empty allowlist means
 * every bind is rejected, not "fall back to some default root" — a
 * misconfigured deployment should refuse the feature outright, not open a
 * different unintended door.
 *
 * This check runs at bind time AND is re-verified on every sync/write —
 * an allowlist entry (or the binding's local_root itself) could change
 * between when a binding was created and when it's used, and re-checking
 * costs one realpath call, cheap insurance against that drift.
 */

export class BindingNotAllowedError extends Error {
  constructor(localRoot: string) {
    super(`"${localRoot}" is not inside any allowlisted root — binding refused. Configure FILE_BINDING_ALLOWLIST_ROOTS to permit it.`)
    this.name = 'BindingNotAllowedError'
  }
}

/** Resolve `candidateRoot` to its real, symlink-free path and verify it
 *  falls under one of the configured allowlist roots (also realpath'd, so
 *  a symlinked allowlist entry can't be used to widen the effective
 *  boundary). Returns the canonical path to store/use — callers must use
 *  THIS value, not the caller-supplied string, so later checks compare
 *  apples to apples. */
export function assertAllowlistedBindingRoot(candidateRoot: string): string {
  if (env.FILE_BINDING_ALLOWLIST_ROOTS.length === 0) {
    throw new BindingNotAllowedError(candidateRoot)
  }
  let canonical: string
  try {
    canonical = realpathSync(candidateRoot)
  } catch {
    // Nonexistent path, permission denied, etc. — fail closed, same as
    // "not in the allowlist". Never treat an unresolvable path as
    // implicitly safe.
    throw new BindingNotAllowedError(candidateRoot)
  }
  const isAllowed = env.FILE_BINDING_ALLOWLIST_ROOTS.some((root) => {
    let canonicalRoot: string
    try {
      canonicalRoot = realpathSync(root)
    } catch {
      return false // a misconfigured allowlist entry never grants access
    }
    // Path-segment prefix, not string prefix — "/x/bound-evil" must not
    // match root "/x/bound" (Nova's D1 finding, applied here too).
    return canonical === canonicalRoot || canonical.startsWith(canonicalRoot + sep)
  })
  if (!isAllowed) throw new BindingNotAllowedError(candidateRoot)
  return canonical
}

/* ============== fd-relative sandbox (Wren MUST FIX 1) ============== *
 *
 * Node has no openat2/RESOLVE_BENEATH binding, so this emulates the same
 * guarantee by walking the path one segment at a time, opening each
 * intermediate directory with O_NOFOLLOW, and refusing to proceed through
 * anything that isn't a plain directory. This closes the TOCTOU window
 * Wren flagged: there is no "resolve the whole string, then act on it"
 * step where a symlink swapped in between the two could redirect the
 * write outside the binding root.
 *
 * Explicitly NOT sufficient and NOT used: string prefix comparison
 * (`resolved.startsWith(root)`) — matches `/bound-evil` against `/bound`,
 * and tells you nothing about symlinks planted *inside* the tree after
 * the check ran.
 */

export class SandboxViolation extends Error {
  constructor(relPath: string, reason: string) {
    super(`sandbox violation for "${relPath}": ${reason}`)
    this.name = 'SandboxViolation'
  }
}

/** Split a caller-supplied relative path into safe segments, rejecting
 *  anything that isn't a plain relative path before it ever touches the
 *  filesystem. Absolute paths and '..' segments are rejected here, not
 *  normalized away — normalizing '..' out would silently change what the
 *  caller asked for. */
function splitRelPath(relPath: string): string[] {
  if (relPath.startsWith('/') || relPath.startsWith('~')) {
    throw new SandboxViolation(relPath, 'absolute paths are not accepted — path must be relative to the binding root')
  }
  const segments = relPath.split(sep === '\\' ? /[/\\]/ : '/').filter((s) => s.length > 0)
  if (segments.length === 0) throw new SandboxViolation(relPath, 'empty path')
  for (const seg of segments) {
    if (seg === '..' || seg === '.') {
      throw new SandboxViolation(relPath, `traversal segment "${seg}" is not allowed`)
    }
  }
  return segments
}

/**
 * Open the final path segment beneath `rootDir`, refusing to follow any
 * symlink at any intermediate step. Returns an open file descriptor for
 * the leaf; the caller is responsible for closing it.
 *
 * `leafFlags` controls how the final segment is opened (e.g. O_RDONLY for
 * a read, O_WRONLY|O_CREAT|O_TRUNC for a write) — intermediate directory
 * segments are always opened O_DIRECTORY|O_NOFOLLOW regardless.
 */
async function openBeneath(
  rootDir: string,
  relPath: string,
  leafFlags: number,
  leafMode = 0o644,
): Promise<import('node:fs/promises').FileHandle> {
  const segments = splitRelPath(relPath)
  let dirHandle = await fsOpen(rootDir, fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW)
  try {
    for (let i = 0; i < segments.length - 1; i++) {
      const seg = segments[i]
      let next: import('node:fs/promises').FileHandle
      try {
        // Node's fs/promises has no *at() family (openat/fstatat), so we
        // can't open relative to `dirHandle` directly. We instead re-open
        // by absolute path but with O_NOFOLLOW on every intermediate —
        // if a symlink was swapped in between listing and this open, the
        // O_NOFOLLOW open fails closed rather than following it. The
        // window this doesn't close is a swap of a *non-symlink real
        // directory* at the same path, which O_NOFOLLOW cannot detect —
        // recorded as a residual risk below, not silently assumed away.
        next = await fsOpen(join(rootDir, ...segments.slice(0, i + 1)), fsConstants.O_DIRECTORY | fsConstants.O_NOFOLLOW)
      } catch (e: unknown) {
        const code = (e as NodeJS.ErrnoException)?.code
        // ELOOP: kernel refused to follow a symlink at this segment.
        // ENOTDIR: this segment resolved to a non-directory — on Linux a
        // symlink pointing at a directory, opened with O_DIRECTORY|O_NOFOLLOW,
        // surfaces as ENOTDIR rather than ELOOP (the open never gets far
        // enough to detect the loop; it just sees "not a directory" once
        // O_NOFOLLOW stops it from being resolved into one). Either way the
        // segment isn't a plain directory we're allowed to descend into.
        if (code === 'ELOOP' || code === 'ENOTDIR') {
          throw new SandboxViolation(relPath, `segment "${seg}" is a symlink or not a directory — not allowed`)
        }
        throw e
      }
      await dirHandle.close()
      dirHandle = next
    }
    const leaf = segments[segments.length - 1]
    const leafPath = join(rootDir, ...segments)
    try {
      const handle = await fsOpen(leafPath, leafFlags | fsConstants.O_NOFOLLOW, leafMode)
      return handle
    } catch (e: unknown) {
      const code = (e as NodeJS.ErrnoException)?.code
      if (code === 'ELOOP') throw new SandboxViolation(relPath, `"${leaf}" is a symlink — not allowed`)
      throw e
    }
  } finally {
    await dirHandle.close()
  }
}

/**
 * Residual risk, recorded rather than hidden: Node's fs/promises API has
 * no `openat`/`fstatat`, so each intermediate segment is re-opened by
 * absolute path rather than truly relative to the parent's fd. A directory
 * *replaced* (not symlinked — an actual rename of a same-named real dir)
 * between two segment opens is not caught by O_NOFOLLOW. This is a strictly
 * narrower window than the original TOCTOU (limited to directory-rename
 * races on a path the binding owner controls, not arbitrary symlink
 * planting), and matches what's achievable without a native openat
 * binding. If this residual risk needs closing, the fix is a small native
 * addon wrapping real openat(2) — not attempted here per Wren's "additive,
 * no new deps" constraint discussion.
 */

/* ============== snapshot store ============== */

const SNAPSHOTS_ROOT = resolve(process.cwd(), 'server/file-snapshots')

function snapshotDir(bindingId: string, snapshotId: string): string {
  return join(SNAPSHOTS_ROOT, bindingId, snapshotId)
}

/** Hardlink targets must be immutable, GC-owned snapshot inodes only —
 *  never a path under the binding's live local_root. Linking a live
 *  source file would mean editing "the snapshot" could mutate the
 *  member's real, unversioned file underneath it. */
function assertImmutableSnapshotTarget(path: string): void {
  if (!path.startsWith(SNAPSHOTS_ROOT + sep)) {
    throw new Error(`refusing to hardlink outside the snapshot store: ${path}`)
  }
}

async function walkFiles(dir: string, baseDir = dir): Promise<string[]> {
  const entries = await readdir(dir, { withFileTypes: true })
  const out: string[] = []
  for (const entry of entries) {
    const full = join(dir, entry.name)
    if (entry.isDirectory()) {
      out.push(...await walkFiles(full, baseDir))
    } else if (entry.isFile()) {
      out.push(full.slice(baseDir.length + 1))
    }
    // symlinks inside the bound folder are neither followed nor copied —
    // best-effort skip, not a security boundary (the boundary is the
    // sandbox open above; this is just "don't snapshot garbage").
  }
  return out
}

/** Build a new immutable snapshot from `localRoot`, reusing content from
 *  `previousSnapshotDir` via hardlink wherever a file's bytes are known
 *  unchanged (same relative path + same size + same mtime as last
 *  snapshot — a cheap pre-filter; the composite hash is what actually gets
 *  compared for CAS purposes, this is only a copy-cost optimization).
 *
 *  Returns both the snapshot-wide composite hash (proves "these two reads
 *  landed on the same snapshot") AND a per-file hash map (proves "this
 *  file's content is what it should be"). Nova's acceptance run caught
 *  that the composite hash alone can't do both jobs: it's identical for
 *  every file in one snapshot, so it can never detect "snapshot advanced
 *  correctly but this one file's bytes got corrupted along the way" — the
 *  per-file hash is what verification standard #3 actually needs. */
async function buildSnapshot(
  bindingId: string,
  localRoot: string,
  previousSnapshotDir: string | null,
): Promise<{ dir: string; hash: string; files: string[]; fileHashes: Map<string, string> }> {
  const tmpDir = join(SNAPSHOTS_ROOT, bindingId, `tmp-${randomUUID()}`)
  await mkdir(tmpDir, { recursive: true })

  const relFiles = await walkFiles(localRoot)
  const hash = createHash('sha256')
  const sortedFiles = [...relFiles].sort()
  const fileHashes = new Map<string, string>()

  for (const rel of sortedFiles) {
    const srcPath = join(localRoot, rel)
    const dstPath = join(tmpDir, rel)
    await mkdir(join(dstPath, '..'), { recursive: true })

    let linked = false
    if (previousSnapshotDir) {
      const prevPath = join(previousSnapshotDir, rel)
      try {
        const [srcStat, prevStat] = await Promise.all([stat(srcPath), stat(prevPath)])
        if (srcStat.size === prevStat.size && srcStat.mtimeMs === prevStat.mtimeMs) {
          assertImmutableSnapshotTarget(prevPath)
          await link(prevPath, dstPath)
          linked = true
        }
      } catch { /* previous version doesn't have this file — fall through to copy */ }
    }
    if (!linked) {
      const content = await readFile(srcPath)
      await writeFile(dstPath, content)
    }
    // Composite hash is git-tree-style: path + content hash, concatenated
    // in a fixed (sorted) order — not an unordered set of per-file hashes,
    // so two snapshots with the same files in "different order" can never
    // spuriously compare equal or unequal based on enumeration order.
    const fileHash = createHash('sha256').update(await readFile(dstPath)).digest('hex')
    fileHashes.set(rel, fileHash)
    hash.update(rel).update('\0').update(fileHash).update('\n')
  }

  const finalHash = hash.digest('hex')
  return { dir: tmpDir, hash: finalHash, files: sortedFiles, fileHashes }
}

/** Atomically publish a built snapshot: rename the temp dir into its
 *  final path, then CAS-advance the file's version pointer in the same
 *  DB transaction. Returns the new version, or null if a concurrent
 *  writer already published first (caller's snapshot becomes orphaned —
 *  left in place for the retention window, not deleted here).
 *
 *  `externalClient`: when provided, this call joins the CALLER's existing
 *  transaction (BEGIN/COMMIT/ROLLBACK are the caller's responsibility) —
 *  required by writeFileAtVersion so its advisory lock (held on that same
 *  client) actually covers this UPDATE. Without this, a lock taken on one
 *  pool connection would not serialize an UPDATE issued on a different
 *  pooled connection, defeating the lock entirely (Wren's MUST FIX 2
 *  follow-up). When omitted, this function manages its own connection and
 *  transaction — the shape syncBinding uses, which has no concurrent
 *  writer to race against within a single sync pass. */
async function publishSnapshot(
  bindingId: string,
  fileId: string,
  expectedVersion: bigint,
  builtDir: string,
  snapshotHash: string,
  fileContentHash: string,
  authorId: string,
  externalClient?: PoolClient,
): Promise<bigint | null> {
  const snapshotId = randomUUID()
  const finalDir = snapshotDir(bindingId, snapshotId)
  await mkdir(join(finalDir, '..'), { recursive: true })
  // The single indivisible step: directory-level rename, not per-file
  // overwrite. A crash before this line leaves only an orphaned tmp dir
  // (harmless, swept later); a crash after leaves a complete, correctly
  // named snapshot with no DB row yet pointing at it (harmless — GC'd on
  // the same "unreferenced" basis as an abandoned publish race).
  await rename(builtDir, finalDir)

  const ownsConnection = !externalClient
  const client = externalClient ?? await pool.connect()
  try {
    if (ownsConnection) await client.query('BEGIN')
    const newVersion = expectedVersion + 1n
    const updateResult = await client.query(
      `UPDATE files SET current_version = $1, updated_at = NOW()
        WHERE id = $2 AND current_version = $3`,
      [newVersion.toString(), fileId, expectedVersion.toString()],
    )
    if (updateResult.rowCount === 0) {
      // Lost the race — another writer already advanced current_version.
      // Roll back; leave `finalDir` on disk for the retention window
      // rather than deleting it here (a concurrent reader may have
      // already resolved a pin against this exact directory in the tiny
      // gap between CAS failing and this function returning).
      if (ownsConnection) await client.query('ROLLBACK')
      return null
    }
    await client.query(
      `INSERT INTO file_snapshots (file_id, version, storage_key, snapshot_hash, content_hash, author_id)
       VALUES ($1, $2, $3, $4, $5, $6)`,
      [fileId, newVersion.toString(), finalDir, snapshotHash, fileContentHash, authorId],
    )
    if (ownsConnection) await client.query('COMMIT')
    return newVersion
  } catch (e) {
    if (ownsConnection) await client.query('ROLLBACK')
    throw e
  } finally {
    if (ownsConnection) client.release()
  }
}

/* ============== public surface ============== */

export interface FileBindingRow {
  id: string
  companyId: string
  localRoot: string
  createdBy: string
}

export interface FileRow {
  id: string
  bindingId: string
  companyId: string
  relPath: string
  currentVersion: bigint
}

/** Sync one binding: build a fresh snapshot from its local_root and
 *  publish it for every known file whose content actually changed
 *  (content_hash differs from its current snapshot's), each under its
 *  own per-file CAS — so a conflicting concurrent write to file A never
 *  blocks the sync of unrelated file B. */
export async function syncBinding(binding: FileBindingRow, actorId: string): Promise<{ synced: number; conflicted: string[]; deleted: string[] }> {
  // Re-verify on every sync, not just at bind time — the allowlist config
  // or the binding's local_root could have drifted since binding creation.
  assertAllowlistedBindingRoot(binding.localRoot)

  const { rows: fileRows } = await pool.query<{ id: string; rel_path: string; current_version: string; deleted_at: Date | null }>(
    `SELECT id, rel_path, current_version, deleted_at FROM files WHERE binding_id = $1`,
    [binding.id],
  )
  const byPath = new Map(fileRows.map((r) => [r.rel_path, r]))

  const relFiles = await walkFiles(binding.localRoot)
  const relFileSet = new Set(relFiles)
  let synced = 0
  const conflicted: string[] = []
  const deleted: string[] = []

  // Deletion propagation: a file tracked in the DB but no longer present
  // on disk is soft-deleted, not left dangling forever. Historical
  // snapshots are kept (the "intentional retention" reading stays valid —
  // read-only inspection of old versions is unaffected), but writes must
  // refuse once marked, so an agent can't silently recreate a file the
  // user deliberately deleted on their own disk.
  for (const fileRow of fileRows) {
    if (!relFileSet.has(fileRow.rel_path) && !fileRow.deleted_at) {
      await pool.query(`UPDATE files SET deleted_at = NOW() WHERE id = $1`, [fileRow.id])
      deleted.push(fileRow.rel_path)
    }
  }

  for (const rel of relFiles) {
    let fileRow = byPath.get(rel)
    if (!fileRow) {
      const insertResult = await pool.query<{ id: string; current_version: string; deleted_at: Date | null }>(
        `INSERT INTO files (id, binding_id, company_id, rel_path, created_by)
         VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT (binding_id, rel_path) DO UPDATE SET rel_path = EXCLUDED.rel_path
         RETURNING id, current_version, deleted_at`,
        [randomUUID(), binding.id, binding.companyId, rel, actorId],
      )
      fileRow = {
        id: insertResult.rows[0].id, rel_path: rel,
        current_version: insertResult.rows[0].current_version, deleted_at: insertResult.rows[0].deleted_at,
      }
    } else if (fileRow.deleted_at) {
      // Reappeared on disk after being soft-deleted — Nova's experiment 1
      // (delete → recreate → sync) is the normal, supported flow. Un-delete
      // and let the rest of this loop publish its (new) content as usual.
      await pool.query(`UPDATE files SET deleted_at = NULL WHERE id = $1`, [fileRow.id])
    }

    const { rows: latestSnapRows } = await pool.query<{ storage_key: string; snapshot_hash: string }>(
      `SELECT storage_key, snapshot_hash FROM file_snapshots WHERE file_id = $1 ORDER BY version DESC LIMIT 1`,
      [fileRow.id],
    )
    const previousDir = latestSnapRows[0]?.storage_key ?? null

    // Compare against the snapshot-wide composite hash (built.hash) to
    // decide whether ANYTHING changed under this binding — cheap, and
    // correct for "skip if unchanged" purposes (see buildSnapshot's docstring
    // for why this differs from the per-file content_hash column).
    const built = await buildSnapshot(binding.id, binding.localRoot, previousDir)
    if (latestSnapRows[0]?.snapshot_hash === built.hash) {
      await rm(built.dir, { recursive: true, force: true })
      continue // unchanged, nothing to publish
    }

    const expectedVersion = BigInt(fileRow.current_version)
    const fileContentHash = built.fileHashes.get(rel) ?? built.hash
    const published = await publishSnapshot(
      binding.id, fileRow.id, expectedVersion, built.dir, built.hash, fileContentHash, actorId,
    )
    if (published === null) {
      conflicted.push(rel)
    } else {
      synced++
    }
  }

  return { synced, conflicted, deleted }
}

/** Read a file at its current (or explicitly pinned) version. All reads
 *  within one logical request must call this with the SAME `pinnedVersion`
 *  (obtained from the first call in the request) — never re-resolve
 *  `current` mid-request. This is Wren's MUST FIX 1: write-side atomicity
 *  doesn't imply read-side consistency unless the read path itself commits
 *  to one snapshot for its whole lifetime. */
export async function readFileAtVersion(
  fileId: string,
  pinnedVersion?: bigint,
): Promise<{ content: Buffer; version: bigint; contentHash: string; snapshotHash: string; deleted: boolean } | null> {
  const { rows } = await pool.query<{ current_version: string; rel_path: string; binding_id: string; deleted_at: Date | null }>(
    `SELECT current_version, rel_path, binding_id, deleted_at FROM files WHERE id = $1`,
    [fileId],
  )
  const fileRow = rows[0]
  if (!fileRow) return null
  const version = pinnedVersion ?? BigInt(fileRow.current_version)

  const { rows: snapRows } = await pool.query<{ storage_key: string; content_hash: string; snapshot_hash: string }>(
    `SELECT storage_key, content_hash, snapshot_hash FROM file_snapshots WHERE file_id = $1 AND version = $2`,
    [fileId, version.toString()],
  )
  const snap = snapRows[0]
  if (!snap) return null

  // storage_key is the snapshot's own root directory (see snapshotDir) —
  // the file lives at its full rel_path underneath it, not just its
  // basename, so nested paths (e.g. "foo/bar.txt") resolve correctly.
  const handle = await openBeneath(snap.storage_key, fileRow.rel_path, fsConstants.O_RDONLY)
  try {
    const content = await handle.readFile()
    // contentHash: this file's own bytes (verification standard #3 —
    // detects a single corrupted file within an otherwise-correct sync).
    // snapshotHash: the composite for the whole snapshot this version
    // belongs to (proves "two reads landed on the same snapshot" — the
    // id-equality half of standard #3). Callers needing both compare
    // separately; this function returns each without conflating them.
    // deleted: true means this path was removed from local_root and a
    // subsequent sync soft-deleted it. The content returned here is still
    // the last known-good snapshot (read-only inspection of history stays
    // valid), but callers surfacing this to a human/agent must say so —
    // silently returning old content as if nothing happened is what let
    // the deletion-propagation gap go unnoticed.
    return {
      content, version, contentHash: snap.content_hash, snapshotHash: snap.snapshot_hash,
      deleted: Boolean(fileRow.deleted_at),
    }
  } finally {
    await handle.close()
  }
}

/** Resolve `current` once and return a pin token the caller threads
 *  through every subsequent read in the same logical request (see
 *  `readFileAtVersion`'s `pinnedVersion` param). */
export async function pinCurrentVersion(fileId: string): Promise<bigint | null> {
  const { rows } = await pool.query<{ current_version: string }>(
    `SELECT current_version FROM files WHERE id = $1`,
    [fileId],
  )
  return rows[0] ? BigInt(rows[0].current_version) : null
}

/** Write new content to a file inside its binding's sandbox, subject to
 *  CAS against `expectedVersion`. Returns the new version, or throws
 *  FileConflictError if `expectedVersion` no longer matches — the caller
 *  (②-a: internal/agent writers) must treat this as "reread and retry",
 *  never "overwrite anyway". */
export class FileConflictError extends Error {
  constructor(relPath: string, expectedVersion: bigint) {
    super(`conflict writing "${relPath}": expected version ${expectedVersion} is no longer current`)
    this.name = 'FileConflictError'
  }
}

/** Thrown by writeFileAtVersion when the target was soft-deleted by a
 *  prior syncBinding (the user removed it from local_root themselves).
 *  Writing anyway would recreate a file the user deliberately deleted, on
 *  their own disk, with no warning — the deletion-propagation gap Nova's
 *  acceptance run found. The caller must re-sync (which will un-delete it
 *  if it's back on disk) or pick a different path, not force the write. */
export class FileDeletedError extends Error {
  constructor(relPath: string) {
    super(`"${relPath}" was deleted locally and is no longer tracked — writing would recreate it on the user's disk without their consent. Re-sync if it's back on disk, or write a different path.`)
    this.name = 'FileDeletedError'
  }
}

export async function writeFileAtVersion(
  binding: FileBindingRow,
  fileId: string,
  relPath: string,
  expectedVersion: bigint,
  content: Buffer,
  authorId: string,
  /** Test-only hook, fired immediately before the advisory-lock query is
   *  issued (i.e. this call is genuinely about to contend for the lock,
   *  not merely "the promise exists"). Production callers never pass
   *  this — it exists so a concurrency test can synchronize on the exact
   *  instant a second writer starts waiting, instead of inferring it from
   *  timing, which Wren's review correctly flagged as non-deterministic. */
  onBeforeLock?: () => void,
  /** Test-only hook, awaited immediately AFTER the advisory lock is
   *  granted and before any disk/DB work in the critical section runs.
   *  Lets a concurrency test hold a winning writer inside the critical
   *  section while a second writer reaches (and is provably blocked at)
   *  the same lock, before releasing the first. Production callers never
   *  pass this. */
  onAfterLock?: () => Promise<void>,
): Promise<bigint> {
  // Re-verify the allowlist on every write, not just at bind time — see
  // syncBinding's identical check for why this can't be a bind-time-only
  // gate.
  assertAllowlistedBindingRoot(binding.localRoot)

  // Wren's MUST FIX 2: the previous version checked the CAS token, then
  // wrote to disk, then separately re-checked CAS when publishing — but
  // between the first check and the disk write, nothing stopped a second
  // concurrent writer from ALSO passing its own first check (current_version
  // hadn't moved yet) and racing to overwrite the same file on disk. The
  // loser's content could land on disk even though its call correctly
  // reported a conflict. A Postgres session-scoped advisory transaction
  // lock closes this: only one writer at a time can hold the lock for this
  // fileId, and precheck -> disk write -> snapshot publish all run on the
  // SAME client inside the SAME transaction, so nothing else can interleave.
  // (Locking on `hashtext(fileId)` — a UUID string, not a bigint — Postgres
  // advisory locks take a bigint key; hashtext gives a stable one without
  // a separate lock-id column.)
  const client = await pool.connect()
  try {
    await client.query('BEGIN')
    onBeforeLock?.()
    await client.query('SELECT pg_advisory_xact_lock(hashtext($1))', [fileId])
    if (onAfterLock) await onAfterLock()

    // Deletion check MUST happen before any disk write below — writing
    // first and checking after would recreate the file on disk before the
    // rejection even fires, which is exactly the bug being fixed here.
    const { rows: fileRows } = await client.query<{ deleted_at: Date | null; current_version: string }>(
      `SELECT deleted_at, current_version FROM files WHERE id = $1`,
      [fileId],
    )
    if (fileRows[0]?.deleted_at) throw new FileDeletedError(relPath)
    if (!fileRows[0] || BigInt(fileRows[0].current_version) !== expectedVersion) {
      throw new FileConflictError(relPath, expectedVersion)
    }

    // ① sandbox: write goes through the local_root via the fd-relative
    // walk, never a bare join+write — this is the write path Wren's MUST
    // FIX 1 was about, applied to writes not just reads. This fs call and
    // everything below it run while still holding the advisory lock, so no
    // other writer for this fileId can be mid-flight concurrently.
    const handle = await openBeneath(
      binding.localRoot,
      relPath,
      fsConstants.O_WRONLY | fsConstants.O_CREAT | fsConstants.O_TRUNC,
    )
    try {
      await handle.writeFile(content)
    } finally {
      await handle.close()
    }

    const { rows: latestSnapRows } = await client.query<{ storage_key: string }>(
      `SELECT storage_key FROM file_snapshots WHERE file_id = $1 ORDER BY version DESC LIMIT 1`,
      [fileId],
    )
    const built = await buildSnapshot(binding.id, binding.localRoot, latestSnapRows[0]?.storage_key ?? null)
    const fileContentHash = built.fileHashes.get(relPath) ?? built.hash
    // externalClient=client: publishSnapshot's UPDATE runs on this same
    // client/transaction, inside the advisory lock — not a second pooled
    // connection racing against it. Its CAS still can't fail here (we
    // already verified current_version === expectedVersion above, under
    // the lock, and nothing else can move it while we hold the lock) —
    // kept for defense in depth, not because it's expected to fire.
    const published = await publishSnapshot(
      binding.id, fileId, expectedVersion, built.dir, built.hash, fileContentHash, authorId, client,
    )
    if (published === null) {
      await rm(built.dir, { recursive: true, force: true })
      throw new FileConflictError(relPath, expectedVersion)
    }
    await client.query('COMMIT')
    return published
  } catch (e) {
    await client.query('ROLLBACK')
    throw e
  } finally {
    client.release()
  }
}

/** Retention sweep: delete snapshot directories for versions that are
 *  neither `current`, the version immediately before `current`, nor
 *  referenced by any row this sweep doesn't know about pinning them
 *  (pin tracking itself is a follow-up — until it lands, this sweep only
 *  removes versions at least two behind current, which trivially
 *  satisfies "current + one before" without needing a live pin registry). */
export async function sweepOldSnapshots(fileId: string): Promise<number> {
  const { rows } = await pool.query<{ version: string; storage_key: string }>(
    `SELECT version, storage_key FROM file_snapshots WHERE file_id = $1 ORDER BY version DESC`,
    [fileId],
  )
  if (rows.length <= 2) return 0
  const toRemove = rows.slice(2) // keep current + immediately-previous
  for (const row of toRemove) {
    await rm(row.storage_key, { recursive: true, force: true })
  }
  await pool.query(
    `DELETE FROM file_snapshots WHERE file_id = $1 AND version = ANY($2::bigint[])`,
    [fileId, toRemove.map((r) => r.version)],
  )
  return toRemove.length
}
