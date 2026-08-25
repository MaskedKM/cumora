/**
 * Shared files — HTTP surface. Metadata CRUD + read/write content over the
 * binding/files/snapshots model in ../documents/files.ts. Follows the
 * dependency-injected sub-router pattern established by shipping-router.ts
 * (not the "append to the 6000-line router.ts" pattern the older
 * documents endpoints used) — a fresh feature shouldn't inherit that
 * router's size problem.
 *
 * Every write endpoint requires an `If-Match` header carrying the version
 * the client last read (CAS token) — there is deliberately no "just
 * overwrite" endpoint. A mismatch returns 409, matching the design's
 * "reject on conflict, never silently merge" contract for ②-a (internal
 * writers). ②-b (external local edits) is enforced by files.ts itself:
 * best-effort detection, same 409 shape, no special-cased retry.
 */
import { Router, type Request, type Response } from 'express'
import { randomUUID } from 'node:crypto'
import type { Pool } from 'pg'
import type { AuthedRequest } from '../auth.js'
import {
  syncBinding, readFileAtVersion, pinCurrentVersion, writeFileAtVersion,
  SandboxViolation, FileConflictError, FileDeletedError,
  assertAllowlistedBindingRoot, BindingNotAllowedError,
  type FileBindingRow,
} from '../documents/files.js'

type CompanyContext = { userId: string; companyId: string }
type CompanyRoleContext = CompanyContext & { role: string }

export interface FilesRouterDeps {
  pool: Pool
  requireCompany(req: Request & AuthedRequest): Promise<CompanyContext>
  /** Wren's MUST FIX 1: binding a server-readable path is sensitive enough
   *  that it must require an owner/admin role, not any company member —
   *  mirrors how shipping-router.ts gates its admin-only mutations. */
  requireCompanyRole(req: Request & AuthedRequest): Promise<CompanyRoleContext>
}

class FilesHttpError extends Error {
  constructor(public status: number, message: string) { super(message) }
}

function safe(handler: (req: Request, res: Response) => Promise<void>) {
  return async (req: Request, res: Response) => {
    try {
      await handler(req, res)
    } catch (e) {
      if (e instanceof FilesHttpError) { res.status(e.status).json({ error: e.message }); return }
      if (e instanceof SandboxViolation) { res.status(400).json({ error: e.message }); return }
      if (e instanceof FileConflictError) { res.status(409).json({ error: e.message }); return }
      if (e instanceof FileDeletedError) { res.status(410).json({ error: e.message }); return }
      if (e instanceof BindingNotAllowedError) { res.status(403).json({ error: e.message }); return }
      console.error('[files-router] unhandled error', e)
      res.status(500).json({ error: 'internal error' })
    }
  }
}

async function loadBindingForCompany(pool: Pool, companyId: string): Promise<FileBindingRow> {
  const { rows } = await pool.query<{ id: string; local_root: string; created_by: string }>(
    `SELECT id, local_root, created_by FROM file_bindings WHERE company_id = $1`,
    [companyId],
  )
  const row = rows[0]
  if (!row) throw new FilesHttpError(404, 'no folder bound for this workspace — bind one first')
  return { id: row.id, companyId, localRoot: row.local_root, createdBy: row.created_by }
}

async function loadFileRow(pool: Pool, bindingId: string, fileId: string): Promise<{ id: string; relPath: string; currentVersion: bigint }> {
  const { rows } = await pool.query<{ id: string; rel_path: string; current_version: string }>(
    `SELECT id, rel_path, current_version FROM files WHERE id = $1 AND binding_id = $2`,
    [fileId, bindingId],
  )
  const row = rows[0]
  if (!row) throw new FilesHttpError(404, 'not found')
  return { id: row.id, relPath: row.rel_path, currentVersion: BigInt(row.current_version) }
}

/** Parse the CAS token off `If-Match`. Absent or malformed is a 400, not a
 *  silent "write anyway" — there is no unconditional write endpoint. */
function requireIfMatch(req: Request): bigint {
  const header = req.header('if-match')
  if (!header || !/^\d+$/.test(header.trim())) {
    throw new FilesHttpError(400, 'If-Match header (the version you last read) is required for writes')
  }
  return BigInt(header.trim())
}

export function createFilesRouter(deps: FilesRouterDeps): Router {
  const { pool, requireCompany, requireCompanyRole } = deps
  const router = Router()

  // GET /files — list every known file under this workspace's binding.
  // Returns 404 shaped as an empty-ish response only via loadBindingForCompany's
  // explicit "no folder bound" error — this is verification standard #0
  // (isolation baseline): unbound reads report "unbound", not a crash.
  // Deliberately does NOT return binding.localRoot (Wren's MUST FIX 1,
  // item 3) — the absolute server-side path is not something every
  // company member needs to see just to list tracked files.
  router.get('/', safe(async (req, res) => {
    const { companyId } = await requireCompany(req as Request & AuthedRequest)
    const binding = await loadBindingForCompany(pool, companyId)
    const { rows } = await pool.query<{ id: string; rel_path: string; current_version: string }>(
      `SELECT id, rel_path, current_version FROM files WHERE binding_id = $1 ORDER BY rel_path`,
      [binding.id],
    )
    res.json({
      binding: { id: binding.id },
      files: rows.map((r) => ({ id: r.id, path: r.rel_path, version: r.current_version })),
    })
  }))

  // POST /files/bind — create this workspace's one binding. One binding
  // per company today (see file_bindings' unique index on company_id) —
  // matches the requirement's "single, not multi-folder" scope.
  //
  // Wren's MUST FIX 1: this is the sensitive entry point. Gated to
  // owner/admin (requireCompanyRole, not requireCompany — matches
  // shipping-router.ts's admin-only mutations), and the path must resolve
  // inside FILE_BINDING_ALLOWLIST_ROOTS. The stored local_root is the
  // REALPATH'd canonical form assertAllowlistedBindingRoot returns, not
  // the caller-supplied string — every later check compares against this
  // canonical value, not a possibly-symlinked/relative original.
  router.post('/bind', safe(async (req, res) => {
    const { userId, companyId } = await requireCompanyRole(req as Request & AuthedRequest)
    const requestedRoot = typeof req.body?.localRoot === 'string' ? req.body.localRoot.trim() : ''
    if (!requestedRoot) throw new FilesHttpError(400, 'localRoot is required')
    const canonicalRoot = assertAllowlistedBindingRoot(requestedRoot)
    const bindingId = randomUUID()
    const { rows } = await pool.query<{ id: string }>(
      `INSERT INTO file_bindings (id, company_id, local_root, created_by)
       VALUES ($1, $2, $3, $4)
       ON CONFLICT (company_id) DO UPDATE SET local_root = EXCLUDED.local_root, updated_at = NOW()
       RETURNING id`,
      [bindingId, companyId, canonicalRoot, userId],
    )
    // Response omits localRoot too (MUST FIX 1 item 3) — the caller
    // already knows what they asked to bind; echoing the canonical
    // server path back doesn't serve a UI need that couldn't be met by
    // just confirming success.
    res.status(201).json({ bindingId: rows[0].id })
  }))

  // POST /files/sync — rebuild snapshots for every changed file. Each
  // file's publish is independently CAS'd (see syncBinding), so one
  // conflicting file never blocks the rest.
  router.post('/sync', safe(async (req, res) => {
    const { userId, companyId } = await requireCompany(req as Request & AuthedRequest)
    const binding = await loadBindingForCompany(pool, companyId)
    const result = await syncBinding(binding, userId)
    res.json(result)
  }))

  // GET /files/:id — read content at current version, or a pinned one via
  // ?version=. The pin is resolved ONCE here and threaded through — a
  // single request never re-resolves `current` mid-flight (Wren MUST FIX 1).
  router.get('/:id', safe(async (req, res) => {
    const { companyId } = await requireCompany(req as Request & AuthedRequest)
    const binding = await loadBindingForCompany(pool, companyId)
    const fileRow = await loadFileRow(pool, binding.id, String(req.params.id))
    const requestedVersion = typeof req.query.version === 'string' && /^\d+$/.test(req.query.version)
      ? BigInt(req.query.version)
      : await pinCurrentVersion(fileRow.id)
    if (requestedVersion === null) throw new FilesHttpError(404, 'not found')

    const result = await readFileAtVersion(fileRow.id, requestedVersion)
    if (!result) throw new FilesHttpError(404, 'not found at that version')
    res.set('ETag', result.version.toString())
    res.json({
      path: fileRow.relPath,
      version: result.version.toString(),
      // contentHash: this file's own hash. snapshotHash: shared by every
      // file in the same snapshot, proves "same snapshot", not "correct
      // content" — see files.ts buildSnapshot's docstring.
      contentHash: result.contentHash,
      snapshotHash: result.snapshotHash,
      deleted: result.deleted,
      content: result.content.toString('utf8'),
    })
  }))

  // PUT /files/:id — write new content, CAS'd against If-Match. 409 on a
  // stale version (②-a: strong CAS for in-protocol writers). Sandbox
  // violations surface as 400 via the `safe()` wrapper's SandboxViolation case.
  router.put('/:id', safe(async (req, res) => {
    const { userId, companyId } = await requireCompany(req as Request & AuthedRequest)
    const binding = await loadBindingForCompany(pool, companyId)
    const fileRow = await loadFileRow(pool, binding.id, String(req.params.id))
    const expectedVersion = requireIfMatch(req)
    const content = typeof req.body?.content === 'string' ? req.body.content : ''

    const newVersion = await writeFileAtVersion(
      binding, fileRow.id, fileRow.relPath, expectedVersion, Buffer.from(content, 'utf8'), userId,
    )
    res.set('ETag', newVersion.toString())
    res.json({ path: fileRow.relPath, version: newVersion.toString() })
  }))

  return router
}
