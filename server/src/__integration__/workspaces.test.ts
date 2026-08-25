/**
 * Workspace tracer-bullet tests (#27): create a workspace bound to a real
 * folder, manage explicit members, and gate file list/read/write on
 * workspace membership. Everything runs through the HTTP API against
 * temporary directories on the real filesystem — the single test seam for
 * this feature. The full api router is mounted (fake auth stamps the
 * acting userId), with three app instances so owner / plain member /
 * another-company outsider can act in the same test.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import { mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { buildApiTestApp, ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll } from './_helpers.js'
import { pool } from '../db/pool.js'

const OWNER = 'ws-owner' // COMPANY owner (privileged)
const MEMBER = 'ws-member' // COMPANY plain member — no workspace until added
const AGENT = 'ws-agent' // COMPANY agent participant
const OUTSIDER = 'ws-outsider' // member of COMPANY_B only
const COMPANY = 'c-ws-a'
const COMPANY_B = 'c-ws-b'

let ownerServer: Server
let ownerBase: string
let memberServer: Server
let memberBase: string
let outsiderServer: Server
let outsiderBase: string
let tmpRoot: string
let boundDir: string

const jsonHeaders = (company: string) => ({ 'x-company-id': company, 'content-type': 'application/json' })

async function startApp(userId: string): Promise<[Server, string]> {
  const app = await buildApiTestApp(userId)
  const server = createServer(app)
  await new Promise<void>((resolve) => server.listen(0, () => resolve()))
  const { port } = server.address() as { port: number }
  return [server, `http://127.0.0.1:${port}`]
}

async function createWorkspace(opts?: {
  base?: string
  company?: string
  folderPath?: string
  name?: string
}): Promise<Response> {
  const base = opts?.base ?? ownerBase
  return fetch(`${base}/api/workspaces`, {
    method: 'POST',
    headers: jsonHeaders(opts?.company ?? COMPANY),
    body: JSON.stringify({ name: opts?.name ?? 'Team files', folderPath: opts?.folderPath ?? boundDir }),
  })
}

async function createWorkspaceJson(opts?: {
  base?: string
  company?: string
  folderPath?: string
  name?: string
}): Promise<{ id: string }> {
  const res = await createWorkspace(opts)
  assert.equal(res.status, 201)
  return (await res.json()) as { id: string }
}

async function addMember(workspaceId: string, participantId: string): Promise<Response> {
  return fetch(`${ownerBase}/api/workspaces/${workspaceId}/members`, {
    method: 'POST',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ participantId }),
  })
}

const q = (p: string) => `?path=${encodeURIComponent(p)}`

before(async () => {
  await ensureSchemaOnce()
  ;[ownerServer, ownerBase] = await startApp(OWNER)
  ;[memberServer, memberBase] = await startApp(MEMBER)
  ;[outsiderServer, outsiderBase] = await startApp(OUTSIDER)
  tmpRoot = await mkdtemp(join(tmpdir(), 'cumora-ws-'))
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id)
     VALUES ($1,'Ws Co A','ws-a',$2), ($3,'Ws Co B','ws-b',$4)
     ON CONFLICT DO NOTHING`,
    [COMPANY, OWNER, COMPANY_B, OUTSIDER],
  )
  await seedUserMembership(OWNER, COMPANY)
  await seedUserMembership(MEMBER, COMPANY)
  await pool.query(`UPDATE company_members SET role = 'member' WHERE company_id = $1 AND user_id = $2`, [
    COMPANY,
    MEMBER,
  ])
  await seedUserMembership(OUTSIDER, COMPANY_B)
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Agent Ws', 'tester', 'A', '#abcdef', 'avail')
     ON CONFLICT DO NOTHING`,
    [AGENT, COMPANY],
  )
  boundDir = await mkdtemp(join(tmpRoot, 'bound-'))
})

after(async () => {
  await rm(tmpRoot, { recursive: true, force: true }).catch(() => {})
  await Promise.all(
    [memberServer, outsiderServer].map((s) => new Promise<void>((resolve) => s.close(() => resolve()))),
  )
  await teardownAll(ownerServer)
})

test('owner creates a workspace bound to a real folder and becomes its first explicit member', async () => {
  const res = await createWorkspace()
  assert.equal(res.status, 201)
  const ws = (await res.json()) as { id: string; name: string; isDefault: boolean }
  assert.match(ws.id, /^ws-/)
  assert.equal(ws.name, 'Team files')
  assert.equal(ws.isDefault, false)

  const { rows } = await pool.query<{ company_id: string; folder_path: string }>(
    `SELECT company_id, folder_path FROM workspaces WHERE id = $1`,
    [ws.id],
  )
  assert.equal(rows[0].company_id, COMPANY)
  assert.equal(rows[0].folder_path, boundDir) // stored realpath-resolved

  const members = await pool.query<{ source: string }>(
    `SELECT source FROM workspace_members WHERE workspace_id = $1 AND participant_id = $2`,
    [ws.id, OWNER],
  )
  assert.equal(members.rows[0].source, 'explicit')
})

test('folderPath must be an absolute path to an existing directory', async () => {
  const relative = await createWorkspace({ folderPath: 'relative/dir' })
  assert.equal(relative.status, 400)

  const missing = await createWorkspace({ folderPath: '/definitely/not/here' })
  assert.equal(missing.status, 404)

  const filePath = join(boundDir, 'not-a-dir.txt')
  await writeFile(filePath, 'x', 'utf8')
  const notDir = await createWorkspace({ folderPath: filePath })
  assert.equal(notDir.status, 400)
})

test('a folder can be bound by at most one workspace — same company, alternate spelling, cross-company', async () => {
  const first = await createWorkspace()
  assert.equal(first.status, 201)

  const again = await createWorkspace({ name: 'Second try' })
  assert.equal(again.status, 409)

  const altSpelling = await createWorkspace({ folderPath: join(boundDir, '.') })
  assert.equal(altSpelling.status, 409)

  const otherCompany = await createWorkspace({ base: outsiderBase, company: COMPANY_B, name: 'B wants it too' })
  assert.equal(otherCompany.status, 409)
})

test('only owner/admin can create workspaces and manage members', async () => {
  const memberCreate = await createWorkspace({ base: memberBase })
  assert.equal(memberCreate.status, 403)

  const { id } = await createWorkspaceJson()
  const memberAdd = await fetch(`${memberBase}/api/workspaces/${id}/members`, {
    method: 'POST',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ participantId: MEMBER }),
  })
  assert.equal(memberAdd.status, 403)
})

test('explicit members can be humans or agents and are listed with their source', async () => {
  const { id } = await createWorkspaceJson()

  assert.equal((await addMember(id, MEMBER)).status, 201)
  assert.equal((await addMember(id, AGENT)).status, 201)

  const detailRes = await fetch(`${ownerBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  assert.equal(detailRes.status, 200)
  const detail = (await detailRes.json()) as {
    members: Array<{ participantId: string; source: string; kind: string }>
  }
  const byId = new Map(detail.members.map((m) => [m.participantId, m]))
  const humanRow = byId.get(MEMBER)
  const agentRow = byId.get(AGENT)
  assert.ok(humanRow && agentRow)
  assert.equal(humanRow.source, 'explicit')
  assert.equal(humanRow.kind, 'human')
  assert.equal(agentRow.source, 'explicit')
  assert.equal(agentRow.kind, 'agent')

  assert.equal((await addMember(id, 'no-such-participant')).status, 404)
  assert.equal((await addMember(id, MEMBER)).status, 409)
})

test('in-scope member lists, reads and writes files — content verified on disk', async () => {
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)

  const put = await fetch(`${memberBase}/api/workspaces/${id}/file${q('notes/hello.txt')}`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ body: 'hi workspace' }),
  })
  assert.equal(put.status, 200)

  const onDisk = await readFile(join(boundDir, 'notes', 'hello.txt'), 'utf8')
  assert.equal(onDisk, 'hi workspace')

  const read = await fetch(`${memberBase}/api/workspaces/${id}/file${q('notes/hello.txt')}`, {
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(read.status, 200)
  const file = (await read.json()) as { body: string; path: string }
  assert.equal(file.body, 'hi workspace')
  assert.equal(file.path, 'notes/hello.txt')

  const listNested = await fetch(`${memberBase}/api/workspaces/${id}/files${q('notes')}`, {
    headers: jsonHeaders(COMPANY),
  })
  const nested = (await listNested.json()) as { entries: Array<{ name: string }> }
  assert.ok(nested.entries.some((e: { name: string }) => e.name === 'hello.txt'))

  const listRoot = await fetch(`${memberBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY) })
  const root = (await listRoot.json()) as { entries: Array<{ name: string }> }
  assert.ok(root.entries.some((e: { name: string }) => e.name === 'notes'))

  assert.equal(
    (await fetch(`${memberBase}/api/workspaces/${id}/file${q('missing.txt')}`, { headers: jsonHeaders(COMPANY) }))
      .status,
    404,
  )
})

test('company members outside the scope are denied file operations; other companies see nothing', async () => {
  const { id } = await createWorkspaceJson()

  const list = await fetch(`${memberBase}/api/workspaces`, { headers: jsonHeaders(COMPANY) })
  assert.equal(list.status, 200)
  assert.equal(((await list.json()) as unknown[]).length, 1) // visible, just not accessible

  const detailRes = await fetch(`${memberBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  assert.equal(detailRes.status, 200)
  const detailJson = (await detailRes.json()) as Record<string, unknown>
  assert.equal('folderPath' in detailJson, false)

  assert.equal(
    (await fetch(`${memberBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY) })).status,
    403,
  )
  assert.equal(
    (
      await fetch(`${memberBase}/api/workspaces/${id}/file${q('x.txt')}`, {
        method: 'PUT',
        headers: jsonHeaders(COMPANY),
        body: JSON.stringify({ body: 'nope' }),
      })
    ).status,
    403,
  )

  assert.equal((await fetch(`${outsiderBase}/api/workspaces`, { headers: jsonHeaders(COMPANY_B) })).status, 200)
  assert.equal((await fetch(`${outsiderBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY_B) })).status, 404)
  assert.equal(
    (await fetch(`${outsiderBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY_B) })).status,
    404,
  )
})

test('removing an explicit member revokes access; removing twice 404s', async () => {
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)

  const put = await fetch(`${memberBase}/api/workspaces/${id}/file${q('a.txt')}`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ body: 'still member' }),
  })
  assert.equal(put.status, 200)

  const remove = await fetch(`${ownerBase}/api/workspaces/${id}/members/${MEMBER}`, {
    method: 'DELETE',
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(remove.status, 200)

  assert.equal(
    (await fetch(`${memberBase}/api/workspaces/${id}/file${q('a.txt')}`, { headers: jsonHeaders(COMPANY) })).status,
    403,
  )

  const removeAgain = await fetch(`${ownerBase}/api/workspaces/${id}/members/${MEMBER}`, {
    method: 'DELETE',
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(removeAgain.status, 404)
})

test('paths that escape the workspace folder are rejected', async () => {
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)

  assert.equal(
    (await fetch(`${memberBase}/api/workspaces/${id}/file${q('../../etc/passwd')}`, {
      headers: jsonHeaders(COMPANY),
    })).status,
    400,
  )
  assert.equal(
    (
      await fetch(`${memberBase}/api/workspaces/${id}/file${q('../evil.txt')}`, {
        method: 'PUT',
        headers: jsonHeaders(COMPANY),
        body: JSON.stringify({ body: 'escape' }),
      })
    ).status,
    400,
  )
  assert.equal(
    (await fetch(`${memberBase}/api/workspaces/${id}/files${q('..')}`, { headers: jsonHeaders(COMPANY) })).status,
    400,
  )
})

test('folderPath is exposed only to owner/admin in the workspace detail', async () => {
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)

  const ownerRes = await fetch(`${ownerBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  const ownerJson = (await ownerRes.json()) as { folderPath?: string }
  assert.equal(ownerJson.folderPath, boundDir)

  const memberRes = await fetch(`${memberBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  const memberJson = (await memberRes.json()) as Record<string, unknown>
  assert.equal('folderPath' in memberJson, false)
})

test('reads beyond the 2 MB cap are refused with 413', async () => {
  const { id } = await createWorkspaceJson()
  await writeFile(join(boundDir, 'big.txt'), 'a'.repeat(3 * 1024 * 1024), 'utf8')
  const res = await fetch(`${ownerBase}/api/workspaces/${id}/file${q('big.txt')}`, { headers: jsonHeaders(COMPANY) })
  assert.equal(res.status, 413)
})

test('a symlink inside the folder pointing outside is refused', async () => {
  const { id } = await createWorkspaceJson()
  const secret = join(tmpRoot, 'secret-outside.txt')
  await writeFile(secret, 'server secret', 'utf8')
  await symlink(secret, join(boundDir, 'leak.txt'))
  assert.equal(
    (await fetch(`${ownerBase}/api/workspaces/${id}/file${q('leak.txt')}`, { headers: jsonHeaders(COMPANY) })).status,
    400,
  )
})
