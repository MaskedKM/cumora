import { Router, type Request, type Response, type NextFunction } from 'express'
import { randomUUID } from 'node:crypto'
import { realpath, stat } from 'node:fs/promises'
import { isAbsolute } from 'node:path'
import type { Pool } from 'pg'
import type { AuthedRequest } from '../auth.js'
import {
  WorkspaceError,
  ensureDefaultWorkspace,
  implicitMembers,
  listWorkspaceFiles,
  loadWorkspace,
  readWorkspaceFile,
  resolveWorkspaceAccess,
  writeWorkspaceFile,
} from '../workspaces/core.js'

type CompanyContext = { userId: string; companyId: string }
type CompanyRoleContext = CompanyContext & { role: string }

export interface WorkspacesRouterDeps {
  pool: Pool
  requireCompany(req: Request & AuthedRequest): Promise<CompanyContext>
  requireCompanyRole(req: Request & AuthedRequest): Promise<CompanyRoleContext>
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

async function requireWorkspaceMember(
  deps: WorkspacesRouterDeps,
  req: Request & AuthedRequest,
  workspaceId: string,
): Promise<{ userId: string; companyId: string; workspace: Awaited<ReturnType<typeof loadWorkspace>> }> {
  const { userId, companyId } = await deps.requireCompany(req)
  // For humans the user id IS the participant id, so the shared core gives
  // agents and humans identical membership semantics and messages.
  const workspace = await resolveWorkspaceAccess(deps.pool, {
    companyId,
    participantId: userId,
    workspaceId,
  })
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
    res.json(await listWorkspaceFiles(workspace.folder_path, req.query.path))
  })

  router.get('/:id/file', async (req, res) => {
    const { workspace } = await requireWorkspaceMember(deps, req, req.params.id)
    res.json(await readWorkspaceFile(workspace.folder_path, req.query.path))
  })

  router.put('/:id/file', async (req, res) => {
    const { workspace } = await requireWorkspaceMember(deps, req, req.params.id)
    const content = (req.body ?? {}).body
    if (typeof content !== 'string') throw new WorkspaceError(400, 'body required (string)')
    const result = await writeWorkspaceFile(workspace.folder_path, req.query.path, content)
    res.json({ ok: true, path: result.path })
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
