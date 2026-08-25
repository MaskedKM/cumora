/**
 * Workspace domain core — the single source of truth for workspace
 * membership, containment and file operations. Used by BOTH the human
 * HTTP surface (api/workspaces-router.ts, caller = a user id, which for
 * humans is also their participant id) and the agent CLI (agents/cli.ts,
 * caller = the agent's participant id), so agents and humans get
 * identical access semantics and identical rejection messages.
 */
import { mkdir, readdir, readFile, realpath, stat, writeFile } from 'node:fs/promises'
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path'
import type { Pool } from 'pg'
import { UPLOAD_DIR } from '../storage.js'

export class WorkspaceError extends Error {
  constructor(public status: number, message: string) { super(message) }
}

/** Cap on file bodies moving through workspace surfaces in either
 *  direction — a bound folder may contain arbitrarily large files, and
 *  without this a read would buffer one whole in memory. */
export const MAX_FILE_BYTES = 2 * 1024 * 1024

export type WorkspaceRow = {
  id: string
  company_id: string
  name: string
  folder_path: string
  is_default: boolean
  created_at: Date
  unbound_at: Date | null
  unbound_by: string | null
}

export async function loadWorkspace(pool: Pool, companyId: string, id: string): Promise<WorkspaceRow> {
  const { rows } = await pool.query<WorkspaceRow>(
    `SELECT id, company_id, name, folder_path, is_default, created_at, unbound_at, unbound_by
       FROM workspaces WHERE id = $1 AND company_id = $2`,
    [id, companyId],
  )
  if (!rows[0]) throw new WorkspaceError(404, 'workspace not found')
  return rows[0]
}

/** Each team has exactly one default workspace whose member scope is the
 *  entire team — the referent of "everyone reads and writes the same real
 *  files". Lazily provisioned and self-healing on every list call, so
 *  existing companies get one without a migration and new ones without a
 *  creation hook. Unlike operator-bound folders, its folder is
 *  product-managed under UPLOAD_DIR (workspaces/<companyId>). Provisioning
 *  fails loud: if the folder can't be created, listing 500s rather than
 *  silently omitting the team's core artifact. */
export async function ensureDefaultWorkspace(pool: Pool, companyId: string): Promise<void> {
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
    .catch((e: unknown) => {
      if ((e as { code?: string }).code !== '23505') throw e
      // Lost the one-per-company race — the winner's row is correct.
    })
}

/** Resolve workspace access for a participant (human user id or agent
 *  participant id — for humans they are the same value). The default
 *  workspace's scope is the entire team; otherwise membership = explicit
 *  row ∪ live participants of associated targets, so the scope follows
 *  participant changes on the targets with no sync hooks. Participant
 *  model per kind mirrors implicitMembers() below. */
export async function resolveWorkspaceAccess(
  pool: Pool,
  args: { companyId: string; participantId: string; workspaceId: string },
): Promise<WorkspaceRow> {
  const workspace = await loadWorkspace(pool, args.companyId, args.workspaceId)
  if (workspace.unbound_at) throw new WorkspaceError(410, 'workspace is unbound')
  if (workspace.is_default) return workspace
  const { rows } = await pool.query(
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
    [workspace.id, args.participantId, args.companyId],
  )
  if (!rows[0]) throw new WorkspaceError(403, 'not a member of this workspace')
  return workspace
}

/** Distinct participant ids holding implicit membership via associations.
 *  Per-kind participant model (the single source of truth, mirrored in the
 *  resolveWorkspaceAccess query): project → members of the project's
 *  conversations; board_card → assignee + mentions (creator excluded,
 *  matching agenda-triage semantics); document → creator + collaborators. */
export async function implicitMembers(
  pool: Pool,
  workspaceId: string,
  companyId: string,
): Promise<Set<string>> {
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
 *  time). A not-yet-existing target (creating a file) falls back to
 *  realpath-ing its parent directory. Containment, not a sandbox: members
 *  hold full read/write inside the folder; the only boundary defended is
 *  inside vs outside. */
export async function resolveInside(root: string, raw: unknown): Promise<{ abs: string; rel: string }> {
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

async function isDirectory(p: string): Promise<boolean> {
  try {
    return (await stat(p)).isDirectory()
  } catch {
    return false
  }
}

export async function listWorkspaceFiles(
  root: string,
  rawPath: unknown,
): Promise<{ path: string; entries: Array<{ name: string; dir: boolean; size: number | null; modifiedAt: string | null }> }> {
  const dir = await resolveInside(root, rawPath)
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
  return { path: dir.rel, entries: out }
}

export async function readWorkspaceFile(
  root: string,
  rawPath: unknown,
): Promise<{ path: string; body: string; size: number; modifiedAt: string }> {
  const target = await resolveInside(root, rawPath)
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
  return { path: target.rel, body, size: s.size, modifiedAt: s.mtime.toISOString() }
}

export async function writeWorkspaceFile(
  root: string,
  rawPath: unknown,
  content: string,
): Promise<{ path: string }> {
  const target = await resolveInside(root, rawPath)
  if (target.rel === '') throw new WorkspaceError(400, 'path required')
  if (typeof content !== 'string') throw new WorkspaceError(400, 'body required (string)')
  if (Buffer.byteLength(content, 'utf8') > MAX_FILE_BYTES) throw new WorkspaceError(413, 'file too large')
  if (await isDirectory(target.abs)) throw new WorkspaceError(400, 'path is a directory')
  await mkdir(dirname(target.abs), { recursive: true })
  await writeFile(target.abs, content, 'utf8')
  return { path: target.rel }
}
