import { Router, type Request, type Response, type NextFunction } from 'express'
import { randomUUID } from 'node:crypto'
import { mkdir, readdir, readFile, realpath, stat, writeFile } from 'node:fs/promises'
import { dirname, basename, isAbsolute, join, relative, resolve, sep } from 'node:path'
import type { Pool } from 'pg'
import type { AuthedRequest } from '../auth.js'
import { UPLOAD_DIR } from '../storage.js'

type CompanyContext = { userId: string; companyId: string }
type CompanyRoleContext = CompanyContext & { role: string }

export interface WorkspacesRouterDeps {
  pool: Pool
  requireCompany(req: Request & AuthedRequest): Promise<CompanyContext>
  requireCompanyRole(req: Request & AuthedRequest): Promise<CompanyRoleContext>
}

class WorkspaceError extends Error {
  constructor(public status: number, message: string) { super(message) }
}

/** Cap on file bodies moving through the workspace API in either
 *  direction — a bound folder may contain arbitrarily large files, and
 *  without this a read would buffer one whole in memory. */
const MAX_FILE_BYTES = 2 * 1024 * 1024

type WorkspaceRow = {
  id: string
  company_id: string
  name: string
  folder_path: string
  is_default: boolean
  created_at: Date
}

function text(value: unknown, max = 2_000): string {
  return typeof value === 'string' ? value.trim().slice(0, max) : ''
}

async function isDirectory(p: string): Promise<boolean> {
  try {
    return (await stat(p)).isDirectory()
  } catch {
    return false
  }
}

async function loadWorkspace(pool: Pool, companyId: string, id: string): Promise<WorkspaceRow> {
  const { rows } = await pool.query<WorkspaceRow>(
    `SELECT id, company_id, name, folder_path, is_default, created_at
       FROM workspaces WHERE id = $1 AND company_id = $2`,
    [id, companyId],
  )
  if (!rows[0]) throw new WorkspaceError(404, 'workspace not found')
  return rows[0]
}

function assertInside(root: string, abs: string): void {
  const fromRoot = relative(root, abs)
  if (fromRoot !== '' && (fromRoot === '..' || fromRoot.startsWith(`..${sep}`) || isAbsolute(fromRoot))) {
    throw new WorkspaceError(400, 'path escapes the workspace folder')
  }
}

/** Resolve a caller-supplied path against the workspace folder and refuse
 *  anything that escapes it. Two layers: `resolve()` normalizes `..`
 *  segments (string level), then `realpath()` re-checks the resolved
 *  target so a host-created symlink inside the folder cannot point an
 *  operation outside it (the root itself is realpath-resolved at bind
 *  time). A not-yet-existing target (PUT creating a file) falls back to
 *  realpath-ing its parent directory. Containment, not a sandbox: members
 *  hold full read/write inside the folder; the only boundary defended is
 *  inside vs outside. */
async function resolveInside(root: string, raw: unknown): Promise<{ abs: string; rel: string }> {
  const rel = typeof raw === 'string' ? raw.trim() : ''
  if (rel.includes('\0')) throw new WorkspaceError(400, 'invalid path')
  const abs = resolve(root, rel)
  assertInside(root, abs)
  let real = abs
  try {
    real = await realpath(abs)
  } catch {
    try {
      real = join(await realpath(dirname(abs)), basename(abs))
    } catch {
      real = abs
    }
  }
  assertInside(root, real)
  return { abs: real, rel: relative(root, real) }
}

/** Each team has exactly one default workspace whose member scope is the
 *  entire team — the referent of "everyone reads and writes the same real
 *  files". Lazily provisioned and self-healing on every list call, so
 *  existing companies get one without a migration and new ones without a
 *  creation hook. Unlike operator-bound folders, its folder is
 *  product-managed under UPLOAD_DIR (workspaces/<companyId>). */
async function ensureDefaultWorkspace(pool: Pool, companyId: string): Promise<void> {
  const { rows } = await pool.query(
    `SELECT 1 FROM workspaces WHERE company_id = $1 AND is_default LIMIT 1`,
    [companyId],
  )
  if (rows[0]) return
  const folder = join(UPLOAD_DIR, 'workspaces', companyId)
  await mkdir(folder, { recursive: true })
  const folderReal = await realpath(folder)
  await pool
    .query(
      `INSERT INTO workspaces (id, company_id, name, folder_path, is_default)
       VALUES ($1, $2, $3, $4, TRUE)
       ON CONFLICT DO NOTHING`,
      [`ws-default-${companyId}`, companyId, 'Team files', folderReal],
    )
    .catch(() => {
      // Lost the one-per-company race — the winner's row is correct.
    })
}

async function requireWorkspaceMember(
  deps: WorkspacesRouterDeps,
  req: Request & AuthedRequest,
  workspaceId: string,
): Promise<{ userId: string; companyId: string; workspace: WorkspaceRow }> {
  const { userId, companyId } = await deps.requireCompany(req)
  const workspace = await loadWorkspace(deps.pool, companyId, workspaceId)
  // The default workspace's member scope is the entire team — company
  // membership (just proven by requireCompany) IS workspace membership.
  if (workspace.is_default) return { userId, companyId, workspace }
  // Membership = explicit row ∪ participants of associated targets,
  // computed live so the scope follows participant changes on the targets
  // with no sync hooks. Participant model per kind mirrors
  // implicitMembers() below.
  const { rows } = await deps.pool.query(
    `SELECT 1 FROM workspace_members WHERE workspace_id = $1 AND participant_id = $2
     UNION ALL
     SELECT 1
       FROM workspace_associations a
      WHERE a.workspace_id = $1 AND a.company_id = $3
        AND EXISTS (
          SELECT 1 FROM participants p
           WHERE p.id = $2 AND p.company_id = $3 AND p.departed_at IS NULL)
        AND (
          (a.target_kind = 'project' AND EXISTS (
             SELECT 1 FROM conversations c
              WHERE c.project_id = a.target_id AND c.company_id = $3
                AND c.members @> to_jsonb(ARRAY[$2::text])))
          OR (a.target_kind = 'board_card' AND EXISTS (
             SELECT 1 FROM board_cards bc
              JOIN boards b ON b.id = bc.board_id
              WHERE bc.id = a.target_id AND b.company_id = $3
                AND (bc.assignee_id = $2 OR bc.mentions @> to_jsonb($2::text))))
          OR (a.target_kind = 'document' AND EXISTS (
             SELECT 1 FROM documents d
              WHERE d.id = a.target_id AND d.company_id = $3
                AND (d.created_by = $2 OR d.collaborators @> to_jsonb($2::text))))
        )
      LIMIT 1`,
    [workspace.id, userId, companyId],
  )
  if (!rows[0]) throw new WorkspaceError(403, 'not a member of this workspace')
  return { userId, companyId, workspace }
}

const ASSOCIATION_KINDS = new Set(['project', 'board_card', 'document'])

async function assertTargetExists(
  pool: Pool,
  companyId: string,
  kind: string,
  targetId: string,
): Promise<void> {
  const { rows } = await pool.query(
    kind === 'project'
      ? `SELECT 1 FROM projects WHERE id = $1 AND company_id = $2 LIMIT 1`
      : kind === 'board_card'
        ? `SELECT 1 FROM board_cards bc JOIN boards b ON b.id = bc.board_id
            WHERE bc.id = $1 AND b.company_id = $2 LIMIT 1`
        : `SELECT 1 FROM documents WHERE id = $1 AND company_id = $2 LIMIT 1`,
    [targetId, companyId],
  )
  if (!rows[0]) throw new WorkspaceError(404, `associated ${kind} not found in this company`)
}

/** Distinct participant ids holding implicit membership via associations.
 *  Per-kind participant model (the single source of truth, mirrored in the
 *  requireWorkspaceMember query): project → members of the project's
 *  conversations; board_card → assignee + mentions (creator excluded,
 *  matching agenda-triage semantics); document → creator + collaborators. */
async function implicitMembers(pool: Pool, workspaceId: string, companyId: string): Promise<Set<string>> {
  const [projects, cards, docs] = await Promise.all([
    pool.query<{ pid: string }>(
      `SELECT DISTINCT x.pid
         FROM workspace_associations a,
         LATERAL (SELECT jsonb_array_elements_text(c.members) AS pid
                    FROM conversations c
                   WHERE c.project_id = a.target_id AND c.company_id = $2) x
        WHERE a.workspace_id = $1 AND a.company_id = $2 AND a.target_kind = 'project'`,
      [workspaceId, companyId],
    ),
    pool.query<{ pid: string }>(
      `SELECT DISTINCT x.pid
         FROM workspace_associations a,
         LATERAL (SELECT bc.assignee_id AS pid
                    FROM board_cards bc JOIN boards b ON b.id = bc.board_id
                   WHERE bc.id = a.target_id AND b.company_id = $2
                  UNION ALL
                  SELECT jsonb_array_elements_text(bc.mentions) AS pid
                    FROM board_cards bc JOIN boards b ON b.id = bc.board_id
                   WHERE bc.id = a.target_id AND b.company_id = $2) x
        WHERE a.workspace_id = $1 AND a.company_id = $2 AND a.target_kind = 'board_card'`,
      [workspaceId, companyId],
    ),
    pool.query<{ pid: string }>(
      `SELECT DISTINCT x.pid
         FROM workspace_associations a,
         LATERAL (SELECT d.created_by AS pid
                    FROM documents d
                   WHERE d.id = a.target_id AND d.company_id = $2
                  UNION ALL
                  SELECT jsonb_array_elements_text(d.collaborators) AS pid
                    FROM documents d
                   WHERE d.id = a.target_id AND d.company_id = $2) x
        WHERE a.workspace_id = $1 AND a.company_id = $2 AND a.target_kind = 'document'`,
      [workspaceId, companyId],
    ),
  ])
  const out = new Set<string>()
  for (const q of [projects, cards, docs]) {
    for (const row of q.rows) {
      if (row.pid) out.add(row.pid)
    }
  }
  return out
}

export function createWorkspacesRouter(deps: WorkspacesRouterDeps): Router {
  const { pool } = deps
  const router = Router()

  // Create a workspace bound to a real folder. Privileged-only: binding
  // exposes a host path to the product, so it stays an owner/admin action.
  router.post('/', async (req, res) => {
    const { userId, companyId } = await deps.requireCompanyRole(req)
    const body = req.body ?? {}
    const name = text(body.name, 80)
    if (!name) throw new WorkspaceError(400, 'name required')
    const rawPath = text(body.folderPath, 4_096)
    if (!rawPath || !isAbsolute(rawPath)) throw new WorkspaceError(400, 'folderPath must be an absolute path')
    let folder: string
    try {
      folder = await realpath(rawPath)
    } catch {
      throw new WorkspaceError(404, 'folder not found')
    }
    if (!(await isDirectory(folder))) throw new WorkspaceError(400, 'folderPath must be a directory')

    const { rows: bound } = await pool.query<{ id: string }>(
      `SELECT id FROM workspaces WHERE folder_path = $1 LIMIT 1`,
      [folder],
    )
    if (bound[0]) throw new WorkspaceError(409, 'folder already bound to a workspace')

    const id = `ws-${randomUUID().slice(0, 10)}`
    const client = await pool.connect()
    try {
      await client.query('BEGIN')
      const inserted = await client.query<{ created_at: Date }>(
        `INSERT INTO workspaces (id, company_id, name, folder_path)
         VALUES ($1, $2, $3, $4) RETURNING created_at`,
        [id, companyId, name, folder],
      )
      // The creator is the first explicit member — a freshly bound
      // workspace with an empty member scope would be unusable (and
      // unmanageable for anyone but other admins).
      await client.query(
        `INSERT INTO workspace_members (workspace_id, participant_id, added_by)
         VALUES ($1, $2, $2)`,
        [id, userId],
      )
      await client.query('COMMIT')
      res.status(201).json({
        id,
        name,
        folderPath: folder,
        isDefault: false,
        createdAt: inserted.rows[0].created_at,
      })
    } catch (e) {
      await client.query('ROLLBACK').catch(() => {})
      if ((e as { code?: string }).code === '23505') {
        throw new WorkspaceError(409, 'folder already bound to a workspace')
      }
      throw e
    } finally {
      client.release()
    }
  })

  router.get('/', async (req, res) => {
    const { companyId } = await deps.requireCompany(req)
    await ensureDefaultWorkspace(pool, companyId)
    const { rows } = await pool.query(
      `SELECT w.id, w.name, w.is_default AS "isDefault", w.created_at AS "createdAt",
              count(m.participant_id)::int AS "explicitMemberCount"
         FROM workspaces w
         LEFT JOIN workspace_members m ON m.workspace_id = w.id
        WHERE w.company_id = $1
        GROUP BY w.id
        ORDER BY w.created_at ASC`,
      [companyId],
    )
    res.json(rows)
  })

  // Visible to every company member (member scope is public inside the
  // team); the bound folder path is privileged-only.
  router.get('/:id', async (req, res) => {
    const { userId, companyId } = await deps.requireCompany(req)
    const workspace = await loadWorkspace(pool, companyId, req.params.id)

    const explicit = await pool.query<{
      participantId: string
      name: string
      kind: string
      addedAt: Date
    }>(
      `SELECT m.participant_id AS "participantId", p.name, p.kind,
              m.created_at AS "addedAt"
         FROM workspace_members m
         JOIN participants p ON p.id = m.participant_id AND p.company_id = $2
        WHERE m.workspace_id = $1
        ORDER BY m.created_at ASC`,
      [workspace.id, companyId],
    )

    const implicitIds = await implicitMembers(pool, workspace.id, companyId)
    if (workspace.is_default) {
      const all = await pool.query<{ id: string }>(
        `SELECT id FROM participants WHERE company_id = $1 AND departed_at IS NULL`,
        [companyId],
      )
      for (const row of all.rows) implicitIds.add(row.id)
    }
    const explicitIds = new Set(explicit.rows.map((r) => r.participantId))
    const derivedOnly = [...implicitIds].filter((pid) => !explicitIds.has(pid))
    let implicitRows: Array<{ participantId: string; name: string; kind: string; source: string; addedAt: null }> = []
    if (derivedOnly.length > 0) {
      const found = await pool.query<{ participantId: string; name: string; kind: string }>(
        `SELECT p.id AS "participantId", p.name, p.kind
           FROM participants p
          WHERE p.company_id = $1 AND p.id = ANY($2::text[]) AND p.departed_at IS NULL`,
        [companyId, derivedOnly],
      )
      implicitRows = found.rows.map((r) => ({ ...r, source: 'implicit', addedAt: null }))
    }

    const associations = await pool.query(
      `SELECT target_kind AS kind, target_id AS "targetId", created_at AS "createdAt"
         FROM workspace_associations
        WHERE workspace_id = $1
        ORDER BY created_at ASC`,
      [workspace.id],
    )

    const role = await pool.query<{ role: string }>(
      `SELECT role FROM company_members WHERE company_id = $1 AND user_id = $2 LIMIT 1`,
      [companyId, userId],
    )
    const privileged = role.rows[0]?.role === 'owner' || role.rows[0]?.role === 'admin'
    res.json({
      id: workspace.id,
      name: workspace.name,
      isDefault: workspace.is_default,
      createdAt: workspace.created_at,
      ...(privileged ? { folderPath: workspace.folder_path } : {}),
      members: [
        ...explicit.rows.map((r) => ({ ...r, source: 'explicit' })),
        ...implicitRows,
      ],
      associations: associations.rows,
    })
  })

  router.post('/:id/members', async (req, res) => {
    const { userId, companyId } = await deps.requireCompanyRole(req)
    const workspace = await loadWorkspace(pool, companyId, req.params.id)
    const participantId = text((req.body ?? {}).participantId, 100)
    if (!participantId) throw new WorkspaceError(400, 'participantId required')
    const { rows: participant } = await pool.query(
      `SELECT id FROM participants WHERE id = $1 AND company_id = $2 AND departed_at IS NULL LIMIT 1`,
      [participantId, companyId],
    )
    if (!participant[0]) throw new WorkspaceError(404, 'participant not found in this company')
    const { rowCount } = await pool.query(
      `INSERT INTO workspace_members (workspace_id, participant_id, added_by)
       VALUES ($1, $2, $3)
       ON CONFLICT DO NOTHING`,
      [workspace.id, participantId, userId],
    )
    if (rowCount === 0) throw new WorkspaceError(409, 'already a member of this workspace')
    res.status(201).json({ ok: true })
  })

  // Only explicit rows are deletable here; implicit membership ends by
  // ending the association or leaving the associated item.
  router.delete('/:id/members/:participantId', async (req, res) => {
    const { companyId } = await deps.requireCompanyRole(req)
    const workspace = await loadWorkspace(pool, companyId, req.params.id)
    const { rowCount } = await pool.query(
      `DELETE FROM workspace_members
        WHERE workspace_id = $1 AND participant_id = $2`,
      [workspace.id, req.params.participantId],
    )
    if (!rowCount) throw new WorkspaceError(404, 'not an explicit member of this workspace')
    res.json({ ok: true })
  })

  // Association rights: projects and board cards gate on owner/admin
  // (associating grants folder access to every conversation member /
  // assignee); a document is associable by any company member.
  router.post('/:id/associations', async (req, res) => {
    const body = req.body ?? {}
    const kind = text(body.kind, 20)
    const targetId = text(body.targetId, 100)
    if (!ASSOCIATION_KINDS.has(kind)) {
      throw new WorkspaceError(400, 'kind must be one of project, board_card, document')
    }
    if (!targetId) throw new WorkspaceError(400, 'targetId required')
    const { userId, companyId } =
      kind === 'document' ? await deps.requireCompany(req) : await deps.requireCompanyRole(req)
    const workspace = await loadWorkspace(pool, companyId, req.params.id)
    await assertTargetExists(pool, companyId, kind, targetId)
    const id = `wa-${randomUUID().slice(0, 10)}`
    const { rowCount } = await pool.query(
      `INSERT INTO workspace_associations (id, workspace_id, company_id, target_kind, target_id, created_by)
       VALUES ($1, $2, $3, $4, $5, $6)
       ON CONFLICT (workspace_id, target_kind, target_id) DO NOTHING`,
      [id, workspace.id, companyId, kind, targetId, userId],
    )
    if (rowCount === 0) throw new WorkspaceError(409, 'already associated with this workspace')
    res.status(201).json({ ok: true, kind, targetId })
  })

  router.delete('/:id/associations/:kind/:targetId', async (req, res) => {
    const kind = String(req.params.kind)
    if (!ASSOCIATION_KINDS.has(kind)) {
      throw new WorkspaceError(400, 'kind must be one of project, board_card, document')
    }
    const authed = kind === 'document' ? await deps.requireCompany(req) : await deps.requireCompanyRole(req)
    const workspace = await loadWorkspace(pool, authed.companyId, req.params.id)
    const { rowCount } = await pool.query(
      `DELETE FROM workspace_associations
        WHERE workspace_id = $1 AND company_id = $2 AND target_kind = $3 AND target_id = $4`,
      [workspace.id, authed.companyId, kind, req.params.targetId],
    )
    if (!rowCount) throw new WorkspaceError(404, 'no such association on this workspace')
    res.json({ ok: true })
  })

  router.get('/:id/files', async (req, res) => {
    const { workspace } = await requireWorkspaceMember(deps, req, req.params.id)
    const dir = await resolveInside(workspace.folder_path, req.query.path)
    if (!(await isDirectory(dir.abs))) {
      throw new WorkspaceError(400, 'path is not a directory inside the workspace folder')
    }
    const entries = await readdir(dir.abs, { withFileTypes: true })
    const out = []
    for (const entry of entries.slice(0, 500)) {
      const s = await stat(join(dir.abs, entry.name)).catch(() => null)
      out.push({
        name: entry.name,
        dir: entry.isDirectory(),
        size: s?.size ?? null,
        modifiedAt: s?.mtime.toISOString() ?? null,
      })
    }
    res.json({ path: dir.rel, entries: out })
  })

  router.get('/:id/file', async (req, res) => {
    const { workspace } = await requireWorkspaceMember(deps, req, req.params.id)
    const target = await resolveInside(workspace.folder_path, req.query.path)
    if (target.rel === '') throw new WorkspaceError(400, 'path required')
    let s: Awaited<ReturnType<typeof stat>>
    try {
      s = await stat(target.abs)
    } catch {
      throw new WorkspaceError(404, 'file not found')
    }
    if (s.isDirectory()) throw new WorkspaceError(400, 'path is a directory')
    if (s.size > MAX_FILE_BYTES) throw new WorkspaceError(413, 'file too large')
    const body = await readFile(target.abs, 'utf8')
    res.json({ path: target.rel, body, size: s.size, modifiedAt: s.mtime.toISOString() })
  })

  router.put('/:id/file', async (req, res) => {
    const { workspace } = await requireWorkspaceMember(deps, req, req.params.id)
    const target = await resolveInside(workspace.folder_path, req.query.path)
    if (target.rel === '') throw new WorkspaceError(400, 'path required')
    const content = (req.body ?? {}).body
    if (typeof content !== 'string') throw new WorkspaceError(400, 'body required (string)')
    if (Buffer.byteLength(content, 'utf8') > MAX_FILE_BYTES) throw new WorkspaceError(413, 'file too large')
    if (await isDirectory(target.abs)) throw new WorkspaceError(400, 'path is a directory')
    await mkdir(dirname(target.abs), { recursive: true })
    await writeFile(target.abs, content, 'utf8')
    res.json({ ok: true, path: target.rel })
  })

  // Mirror shipping-router: local error middleware maps WorkspaceError to
  // its status; anything else bubbles to the parent router's handler.
  router.use((err: unknown, _req: Request, res: Response, next: NextFunction) => {
    if (err instanceof WorkspaceError) {
      res.status(err.status).json({ error: err.message })
      return
    }
    next(err)
  })

  return router
}
