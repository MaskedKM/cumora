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

  const members = await pool.query(
    `SELECT 1 FROM workspace_members WHERE workspace_id = $1 AND participant_id = $2`,
    [ws.id, OWNER],
  )
  assert.equal(members.rowCount, 1)
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
  assert.ok(((await list.json()) as Array<{ id: string }>).some((r) => r.id === id)) // visible, just not accessible

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

// ---------- Association fixtures (#29) ----------

async function seedProjectWithConversation(memberIds: string[]): Promise<void> {
  await pool.query(
    `INSERT INTO projects (id, company_id, name) VALUES ('p-ws', $1, 'WS Project') ON CONFLICT DO NOTHING`,
    [COMPANY],
  )
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members, project_id)
     VALUES ('cv-ws', $1, 'group', 'WS Conv', $2::jsonb, 'p-ws')
     ON CONFLICT DO NOTHING`,
    [COMPANY, JSON.stringify(memberIds)],
  )
}

async function seedBoardCard(opts: { assignee: string | null; mentions: string[]; creator: string }): Promise<void> {
  await pool.query(
    `INSERT INTO boards (id, company_id, title, created_by) VALUES ('b-ws', $1, 'WS Board', $2) ON CONFLICT DO NOTHING`,
    [COMPANY, OWNER],
  )
  await pool.query(
    `INSERT INTO board_columns (id, board_id, title, position) VALUES ('col-ws', 'b-ws', 'To Do', 0) ON CONFLICT DO NOTHING`,
  )
  await pool.query(
    `INSERT INTO board_cards (id, board_id, column_id, title, position, assignee_id, mentions, created_by)
     VALUES ('card-ws', 'b-ws', 'col-ws', 'Deliver', 0, $1, $2::jsonb, $3)
     ON CONFLICT DO NOTHING`,
    [opts.assignee, JSON.stringify(opts.mentions), opts.creator],
  )
}

async function seedDocument(id: string, creator: string, collaborators: string[]): Promise<void> {
  await pool.query(
    `INSERT INTO documents (id, company_id, title, created_by, collaborators)
     VALUES ($1, $2, 'WS Req Doc', $3, $4::jsonb)
     ON CONFLICT DO NOTHING`,
    [id, COMPANY, creator, JSON.stringify(collaborators)],
  )
}

async function associate(
  workspaceId: string,
  kind: string,
  targetId: string,
  base: string = ownerBase,
  company: string = COMPANY,
): Promise<Response> {
  return fetch(`${base}/api/workspaces/${workspaceId}/associations`, {
    method: 'POST',
    headers: jsonHeaders(company),
    body: JSON.stringify({ kind, targetId }),
  })
}

async function writeAs(workspaceId: string, path: string, base: string): Promise<number> {
  const res = await fetch(`${base}/api/workspaces/${workspaceId}/file${q(path)}`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ body: 'x' }),
  })
  return res.status
}

async function detailJson(workspaceId: string, base: string = ownerBase): Promise<{
  members: Array<{ participantId: string; source: string }>
  associations: Array<{ kind: string; targetId: string }>
}> {
  const res = await fetch(`${base}/api/workspaces/${workspaceId}`, { headers: jsonHeaders(COMPANY) })
  return (await res.json()) as {
    members: Array<{ participantId: string; source: string }>
    associations: Array<{ kind: string; targetId: string }>
  }
}

test('project association: conversation members become implicit members and the scope follows membership', async () => {
  await seedProjectWithConversation([MEMBER, AGENT])
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'project', 'p-ws')).status, 201)

  assert.equal(await writeAs(id, 'a.txt', memberBase), 200) // implicit via the project's conversation

  const detail = await detailJson(id)
  const sources = new Map(detail.members.map((m) => [m.participantId, m.source]))
  assert.equal(sources.get(OWNER), 'explicit')
  assert.equal(sources.get(MEMBER), 'implicit')
  assert.equal(sources.get(AGENT), 'implicit')
  assert.deepEqual(detail.associations.map((a) => `${a.kind}:${a.targetId}`), ['project:p-ws'])

  await pool.query(`UPDATE conversations SET members = '["AGENT"]'::jsonb WHERE id = 'cv-ws'`)
  assert.equal(await writeAs(id, 'b.txt', memberBase), 403) // left the conversation → out of scope
})

test('board card association: assignee + mentions are implicit, the creator is not, and reassignment follows', async () => {
  await seedBoardCard({ assignee: AGENT, mentions: [], creator: MEMBER })
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'board_card', 'card-ws')).status, 201)

  assert.equal(await writeAs(id, 'a.txt', memberBase), 403) // creator deliberately not a participant

  await pool.query(`UPDATE board_cards SET assignee_id = $1 WHERE id = 'card-ws'`, [MEMBER])
  assert.equal(await writeAs(id, 'a.txt', memberBase), 200) // assignee path

  await pool.query(`UPDATE board_cards SET assignee_id = NULL, mentions = $1::jsonb WHERE id = 'card-ws'`, [
    JSON.stringify([MEMBER]),
  ])
  assert.equal(await writeAs(id, 'a.txt', memberBase), 200) // mentions path

  await pool.query(`UPDATE board_cards SET mentions = '[]'::jsonb WHERE id = 'card-ws'`)
  assert.equal(await writeAs(id, 'a.txt', memberBase), 403) // no longer assignee or mentioned
})

test('document association: creator + collaborators implicit; collaborator edits via API and access follows', async () => {
  await seedDocument('doc-ws', OWNER, [AGENT])
  await seedDocument('doc-ws2', MEMBER, [])
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'document', 'doc-ws', memberBase)).status, 201) // any member may associate docs
  assert.equal((await associate(id, 'document', 'doc-ws2', ownerBase)).status, 201)

  assert.equal(await writeAs(id, 'a.txt', memberBase), 200) // creator of doc-ws2

  const before = await detailJson(id)
  assert.equal(new Map(before.members.map((m) => [m.participantId, m.source])).get(AGENT), 'implicit')

  // collaborator editing: non-creator member refused, creator may edit
  const forbidden = await fetch(`${memberBase}/api/documents/doc-ws/collaborators`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ participantIds: [MEMBER] }),
  })
  assert.equal(forbidden.status, 403)
  assert.equal(
    (
      await fetch(`${ownerBase}/api/documents/doc-ws/collaborators`, {
        method: 'PUT',
        headers: jsonHeaders(COMPANY),
        body: JSON.stringify({ participantIds: ['nope'] }),
      })
    ).status,
    400,
  )
  const edit = await fetch(`${ownerBase}/api/documents/doc-ws/collaborators`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ participantIds: [] }),
  })
  assert.equal(edit.status, 200)

  const after = await detailJson(id)
  assert.equal(new Map(after.members.map((m) => [m.participantId, m.source])).get(AGENT), undefined)
})

test('association lifecycle: kind whitelist, unknown target, duplicate, delete revokes implicit access', async () => {
  await seedProjectWithConversation([MEMBER])
  const { id } = await createWorkspaceJson()

  assert.equal((await associate(id, 'conversation', 'cv-ws')).status, 400)
  assert.equal((await associate(id, 'project', 'nope')).status, 404)
  assert.equal((await associate(id, 'project', 'p-ws')).status, 201)
  assert.equal((await associate(id, 'project', 'p-ws')).status, 409)
  assert.equal(await writeAs(id, 'a.txt', memberBase), 200)

  const del = await fetch(`${ownerBase}/api/workspaces/${id}/associations/project/p-ws`, {
    method: 'DELETE',
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(del.status, 200)
  assert.equal(await writeAs(id, 'b.txt', memberBase), 403)
  assert.equal(await writeAs(id, 'c.txt', ownerBase), 200) // explicit members unaffected by association churn
  assert.deepEqual((await detailJson(id)).associations, [])
  assert.equal(
    (
      await fetch(`${ownerBase}/api/workspaces/${id}/associations/project/p-ws`, {
        method: 'DELETE',
        headers: jsonHeaders(COMPANY),
      })
    ).status,
    404,
  )
})

test('association rights: project and board_card need owner/admin, document does not', async () => {
  await seedProjectWithConversation([AGENT])
  await seedBoardCard({ assignee: null, mentions: [], creator: OWNER })
  await seedDocument('doc-ws', OWNER, [])
  const { id } = await createWorkspaceJson()

  assert.equal((await associate(id, 'project', 'p-ws', memberBase)).status, 403)
  assert.equal((await associate(id, 'board_card', 'card-ws', memberBase)).status, 403)
  assert.equal((await associate(id, 'document', 'doc-ws', memberBase)).status, 201)
})

test('cross-company isolation: associations only see same-company targets', async () => {
  await seedProjectWithConversation([MEMBER])
  const dirB = await mkdtemp(join(tmpRoot, 'bound-b-'))
  const bCreate = await createWorkspace({ base: outsiderBase, company: COMPANY_B, folderPath: dirB })
  assert.equal(bCreate.status, 201)
  const bId = ((await bCreate.json()) as { id: string }).id
  await pool.query(
    `INSERT INTO projects (id, company_id, name) VALUES ('p-b', $1, 'B Project') ON CONFLICT DO NOTHING`,
    [COMPANY_B],
  )

  assert.equal((await associate(bId, 'project', 'p-ws', outsiderBase, COMPANY_B)).status, 404) // A's project invisible to B
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'project', 'p-b')).status, 404) // B's project invisible to A
  assert.equal((await associate(id, 'project', 'p-ws')).status, 201) // same-company works
})

// ---------- Default workspace (#30) ----------

async function defaultWorkspaceId(base: string = ownerBase): Promise<string> {
  const res = await fetch(`${base}/api/workspaces`, { headers: jsonHeaders(COMPANY) })
  const rows = (await res.json()) as Array<{ id: string; isDefault: boolean }>
  return (rows.find((r) => r.isDefault) as { id: string }).id
}

test('every team gets exactly one default workspace, self-healing on repeat listing', async () => {
  const firstId = await defaultWorkspaceId()
  assert.match(firstId, /^ws-default-/)
  const again = await fetch(`${ownerBase}/api/workspaces`, { headers: jsonHeaders(COMPANY) })
  const rows = (await again.json()) as Array<{ id: string; isDefault: boolean }>
  const defaults = rows.filter((r) => r.isDefault)
  assert.equal(defaults.length, 1)
  assert.equal(defaults[0].id, firstId)
})

test('default workspace: whole team reads and writes without being added; scope follows company membership', async () => {
  const defId = await defaultWorkspaceId()
  assert.equal(await writeAs(defId, 'a.txt', memberBase), 200) // never explicitly added

  const read = await fetch(`${memberBase}/api/workspaces/${defId}/file${q('a.txt')}`, {
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(read.status, 200)
  assert.equal(((await read.json()) as { body: string }).body, 'x')

  const detail = (await (
    await fetch(`${ownerBase}/api/workspaces/${defId}`, { headers: jsonHeaders(COMPANY) })
  ).json()) as { folderPath: string; members: Array<{ participantId: string; source: string }> }
  assert.ok(detail.folderPath.includes('workspaces')) // product-managed folder
  const sources = new Map(detail.members.map((m) => [m.participantId, m.source]))
  assert.equal(sources.get(MEMBER), 'implicit')
  assert.equal(sources.get(AGENT), 'implicit') // humans and agents alike
  assert.equal(sources.get(OWNER), 'implicit') // the whole team, not an explicit list

  await pool.query(`DELETE FROM company_members WHERE company_id = $1 AND user_id = $2`, [COMPANY, MEMBER])
  assert.equal(await writeAs(defId, 'b.txt', memberBase), 403) // out of the team
  await pool.query(
    `INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`,
    [COMPANY, MEMBER],
  )
  assert.equal(await writeAs(defId, 'b.txt', memberBase), 200) // back in
})

test('cross-company: another company cannot reach a team default workspace by its deterministic id', async () => {
  const defId = await defaultWorkspaceId()
  assert.equal(
    (await fetch(`${outsiderBase}/api/workspaces/${defId}`, { headers: jsonHeaders(COMPANY_B) })).status,
    404,
  )
  assert.equal(
    (await fetch(`${outsiderBase}/api/workspaces/${defId}/files`, { headers: jsonHeaders(COMPANY_B) })).status,
    404,
  )
})

// ---------- Safe unbind (#34) ----------

async function unbind(workspaceId: string, base: string = ownerBase): Promise<Response> {
  return fetch(`${base}/api/workspaces/${workspaceId}/unbind`, { method: 'POST', headers: jsonHeaders(COMPANY) })
}

test('safe unbind: files untouched, all access refused, associations visible as inert history', async () => {
  await writeFile(join(boundDir, 'keep.txt'), 'precious', 'utf8')
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)
  assert.equal(await writeAs(id, 'a.txt', memberBase), 200)

  assert.equal((await unbind(id)).status, 200)

  // Not a single file touched
  assert.equal(await readFile(join(boundDir, 'keep.txt'), 'utf8'), 'precious')
  assert.equal(await readFile(join(boundDir, 'a.txt'), 'utf8'), 'x')

  // All access refused (410) for every file surface
  assert.equal(await writeAs(id, 'b.txt', memberBase), 410)
  assert.equal(
    (await fetch(`${memberBase}/api/workspaces/${id}/file${q('keep.txt')}`, { headers: jsonHeaders(COMPANY) })).status,
    410,
  )
  assert.equal((await fetch(`${memberBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY) })).status, 410)

  // Hidden from the list; the detail shows the unbound state
  const list = (await (await fetch(`${ownerBase}/api/workspaces`, { headers: jsonHeaders(COMPANY) })).json()) as Array<{
    id: string
  }>
  assert.ok(!list.some((r) => r.id === id))
  const detail = (await (
    await fetch(`${ownerBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  ).json()) as { unboundAt: string }
  assert.ok(detail.unboundAt)

  // Mutations refused; idempotency explicit
  assert.equal((await unbind(id)).status, 409)
  assert.equal((await addMember(id, AGENT)).status, 410)
})

test('implicit access via associations ends at unbind; association create is 410; default never unbinds', async () => {
  await seedProjectWithConversation([MEMBER])
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'project', 'p-ws')).status, 201)
  assert.equal(await writeAs(id, 'a.txt', memberBase), 200)

  assert.equal((await unbind(id)).status, 200)
  assert.equal(await writeAs(id, 'b.txt', memberBase), 410) // implicit membership inert
  assert.equal((await detailJson(id)).associations.length, 1) // still visible as history
  assert.equal((await associate(id, 'board_card', 'nope')).status, 410) // guard precedes target lookup

  const defId = await defaultWorkspaceId()
  assert.equal((await unbind(defId)).status, 403)
})

test('only owner/admin can unbind', async () => {
  const { id } = await createWorkspaceJson()
  assert.equal((await unbind(id, memberBase)).status, 403)
})
