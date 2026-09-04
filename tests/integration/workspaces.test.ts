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
import { mkdtemp, readFile, readdir, rm, symlink, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, MIRROR_BASE } from './_helpers.js'
import { pool } from './harness/db/pool.js'

const OWNER = 'ws-owner' // COMPANY owner (privileged)
const MEMBER = 'ws-member' // COMPANY plain member — no workspace until added
const AGENT = 'ws-agent' // COMPANY agent participant
const OUTSIDER = 'ws-outsider' // member of COMPANY_B only
const COMPANY = 'c-ws-a'
const COMPANY_B = 'c-ws-b'

let tmpRoot: string
let boundDir: string

/* #70 MIRROR-only:三身份原是三个盖章 app;现统一为 fetchAs(user, …)
 * 的 x-test-user 头选择(三个 *_base 常量保留仅作历史标记,值同)。 */
const ownerBase = MIRROR_BASE
const memberBase = MIRROR_BASE
const outsiderBase = MIRROR_BASE

const jsonHeaders = (company: string) => ({ 'x-company-id': company, 'content-type': 'application/json' })

async function fetchAs(user: string, url: string, init?: RequestInit): Promise<Response> {
  return fetch(url, { ...init, headers: { 'x-test-user': user, ...(init?.headers ?? {}) } })
}

async function createWorkspace(opts?: {
  user?: string
  company?: string
  folderPath?: string
  name?: string
}): Promise<Response> {
  const user = opts?.user ?? OWNER
  return fetchAs(user, `${MIRROR_BASE}/api/workspaces`, {
    method: 'POST',
    headers: jsonHeaders(opts?.company ?? COMPANY),
    body: JSON.stringify({ name: opts?.name ?? 'Team files', folderPath: opts?.folderPath ?? boundDir }),
  })
}

async function createWorkspaceJson(opts?: {
  user?: string
  company?: string
  folderPath?: string
  name?: string
}): Promise<{ id: string }> {
  const res = await createWorkspace(opts)
  assert.equal(res.status, 201)
  return (await res.json()) as { id: string }
}

async function addMember(workspaceId: string, participantId: string): Promise<Response> {
  return fetchAs(OWNER, `${ownerBase}/api/workspaces/${workspaceId}/members`, {
    method: 'POST',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ participantId }),
  })
}

const q = (p: string) => `?path=${encodeURIComponent(p)}`

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
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
  await teardownAll()
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

  const otherCompany = await createWorkspace({ user: OUTSIDER, company: COMPANY_B, name: 'B wants it too' })
  assert.equal(otherCompany.status, 409)
})

test('only owner/admin can create workspaces and manage members', async () => {
  const memberCreate = await createWorkspace({ user: MEMBER })
  assert.equal(memberCreate.status, 403)

  const { id } = await createWorkspaceJson()
  const memberAdd = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/members`, {
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

  const detailRes = await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
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

  const put = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('notes/hello.txt')}`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ body: 'hi workspace' }),
  })
  assert.equal(put.status, 200)

  const onDisk = await readFile(join(boundDir, 'notes', 'hello.txt'), 'utf8')
  assert.equal(onDisk, 'hi workspace')

  const read = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('notes/hello.txt')}`, {
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(read.status, 200)
  const file = (await read.json()) as { body: string; path: string }
  assert.equal(file.body, 'hi workspace')
  assert.equal(file.path, 'notes/hello.txt')

  const listNested = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/files${q('notes')}`, {
    headers: jsonHeaders(COMPANY),
  })
  const nested = (await listNested.json()) as { entries: Array<{ name: string }> }
  assert.ok(nested.entries.some((e: { name: string }) => e.name === 'hello.txt'))

  const listRoot = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY) })
  const root = (await listRoot.json()) as { entries: Array<{ name: string }> }
  assert.ok(root.entries.some((e: { name: string }) => e.name === 'notes'))

  assert.equal(
    (await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('missing.txt')}`, { headers: jsonHeaders(COMPANY) }))
      .status,
    404,
  )
})

test('company members outside the scope are denied file operations; other companies see nothing', async () => {
  const { id } = await createWorkspaceJson()

  const list = await fetchAs(MEMBER, `${memberBase}/api/workspaces`, { headers: jsonHeaders(COMPANY) })
  assert.equal(list.status, 200)
  assert.ok(((await list.json()) as Array<{ id: string }>).some((r) => r.id === id)) // visible, just not accessible

  const detailRes = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  assert.equal(detailRes.status, 200)
  const detailJson = (await detailRes.json()) as Record<string, unknown>
  assert.equal('folderPath' in detailJson, false)

  assert.equal(
    (await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY) })).status,
    403,
  )
  assert.equal(
    (
      await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('x.txt')}`, {
        method: 'PUT',
        headers: jsonHeaders(COMPANY),
        body: JSON.stringify({ body: 'nope' }),
      })
    ).status,
    403,
  )

  assert.equal((await fetchAs(OUTSIDER, `${outsiderBase}/api/workspaces`, { headers: jsonHeaders(COMPANY_B) })).status, 200)
  assert.equal((await fetchAs(OUTSIDER, `${outsiderBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY_B) })).status, 404)
  assert.equal(
    (await fetchAs(OUTSIDER, `${outsiderBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY_B) })).status,
    404,
  )
})

test('removing an explicit member revokes access; removing twice 404s', async () => {
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)

  const put = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('a.txt')}`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ body: 'still member' }),
  })
  assert.equal(put.status, 200)

  const remove = await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}/members/${MEMBER}`, {
    method: 'DELETE',
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(remove.status, 200)

  assert.equal(
    (await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('a.txt')}`, { headers: jsonHeaders(COMPANY) })).status,
    403,
  )

  const removeAgain = await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}/members/${MEMBER}`, {
    method: 'DELETE',
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(removeAgain.status, 404)
})

test('paths that escape the workspace folder are rejected', async () => {
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)

  assert.equal(
    (await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('../../etc/passwd')}`, {
      headers: jsonHeaders(COMPANY),
    })).status,
    400,
  )
  assert.equal(
    (
      await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('../evil.txt')}`, {
        method: 'PUT',
        headers: jsonHeaders(COMPANY),
        body: JSON.stringify({ body: 'escape' }),
      })
    ).status,
    400,
  )
  assert.equal(
    (await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/files${q('..')}`, { headers: jsonHeaders(COMPANY) })).status,
    400,
  )
})

test('folderPath is exposed only to owner/admin in the workspace detail', async () => {
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)

  const ownerRes = await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  const ownerJson = (await ownerRes.json()) as { folderPath?: string }
  assert.equal(ownerJson.folderPath, boundDir)

  const memberRes = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  const memberJson = (await memberRes.json()) as Record<string, unknown>
  assert.equal('folderPath' in memberJson, false)
})

test('reads beyond the 2 MB cap are refused with 413', async () => {
  const { id } = await createWorkspaceJson()
  await writeFile(join(boundDir, 'big.txt'), 'a'.repeat(3 * 1024 * 1024), 'utf8')
  const res = await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}/file${q('big.txt')}`, { headers: jsonHeaders(COMPANY) })
  assert.equal(res.status, 413)
})

test('a symlink inside the folder pointing outside is refused', async () => {
  const { id } = await createWorkspaceJson()
  const secret = join(tmpRoot, 'secret-outside.txt')
  await writeFile(secret, 'server secret', 'utf8')
  await symlink(secret, join(boundDir, 'leak.txt'))
  assert.equal(
    (await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}/file${q('leak.txt')}`, { headers: jsonHeaders(COMPANY) })).status,
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
  user: string = OWNER,
  company: string = COMPANY,
): Promise<Response> {
  return fetchAs(user, `${MIRROR_BASE}/api/workspaces/${workspaceId}/associations`, {
    method: 'POST',
    headers: jsonHeaders(company),
    body: JSON.stringify({ kind, targetId }),
  })
}

async function writeAs(workspaceId: string, path: string, user: string): Promise<number> {
  const res = await fetchAs(user, `${MIRROR_BASE}/api/workspaces/${workspaceId}/file${q(path)}`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ body: 'x' }),
  })
  return res.status
}

async function detailJson(workspaceId: string, user: string = OWNER): Promise<{
  members: Array<{ participantId: string; source: string }>
  associations: Array<{ kind: string; targetId: string }>
}> {
  const res = await fetchAs(user, `${MIRROR_BASE}/api/workspaces/${workspaceId}`, { headers: jsonHeaders(COMPANY) })
  return (await res.json()) as {
    members: Array<{ participantId: string; source: string }>
    associations: Array<{ kind: string; targetId: string }>
  }
}

test('project association: conversation members become implicit members and the scope follows membership', async () => {
  await seedProjectWithConversation([MEMBER, AGENT])
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'project', 'p-ws')).status, 201)

  assert.equal(await writeAs(id, 'a.txt', MEMBER), 200) // implicit via the project's conversation

  const detail = await detailJson(id)
  const sources = new Map(detail.members.map((m) => [m.participantId, m.source]))
  assert.equal(sources.get(OWNER), 'explicit')
  assert.equal(sources.get(MEMBER), 'implicit')
  assert.equal(sources.get(AGENT), 'implicit')
  assert.deepEqual(detail.associations.map((a) => `${a.kind}:${a.targetId}`), ['project:p-ws'])

  await pool.query(`UPDATE conversations SET members = '["AGENT"]'::jsonb WHERE id = 'cv-ws'`)
  assert.equal(await writeAs(id, 'b.txt', MEMBER), 403) // left the conversation → out of scope
})

test('board card association: assignee + mentions are implicit, the creator is not, and reassignment follows', async () => {
  await seedBoardCard({ assignee: AGENT, mentions: [], creator: MEMBER })
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'board_card', 'card-ws')).status, 201)

  assert.equal(await writeAs(id, 'a.txt', MEMBER), 403) // creator deliberately not a participant

  await pool.query(`UPDATE board_cards SET assignee_id = $1 WHERE id = 'card-ws'`, [MEMBER])
  assert.equal(await writeAs(id, 'a.txt', MEMBER), 200) // assignee path

  await pool.query(`UPDATE board_cards SET assignee_id = NULL, mentions = $1::jsonb WHERE id = 'card-ws'`, [
    JSON.stringify([MEMBER]),
  ])
  assert.equal(await writeAs(id, 'a.txt', MEMBER), 200) // mentions path

  await pool.query(`UPDATE board_cards SET mentions = '[]'::jsonb WHERE id = 'card-ws'`)
  assert.equal(await writeAs(id, 'a.txt', MEMBER), 403) // no longer assignee or mentioned
})

test('document association: creator + collaborators implicit; collaborator edits via API and access follows', async () => {
  await seedDocument('doc-ws', OWNER, [AGENT])
  await seedDocument('doc-ws2', MEMBER, [])
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'document', 'doc-ws', MEMBER)).status, 201) // any member may associate docs
  assert.equal((await associate(id, 'document', 'doc-ws2', OWNER)).status, 201)

  assert.equal(await writeAs(id, 'a.txt', MEMBER), 200) // creator of doc-ws2

  const before = await detailJson(id)
  assert.equal(new Map(before.members.map((m) => [m.participantId, m.source])).get(AGENT), 'implicit')

  // collaborator editing: non-creator member refused, creator may edit
  const forbidden = await fetchAs(MEMBER, `${memberBase}/api/documents/doc-ws/collaborators`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ participantIds: [MEMBER] }),
  })
  assert.equal(forbidden.status, 403)
  assert.equal(
    (
      await fetchAs(OWNER, `${ownerBase}/api/documents/doc-ws/collaborators`, {
        method: 'PUT',
        headers: jsonHeaders(COMPANY),
        body: JSON.stringify({ participantIds: ['nope'] }),
      })
    ).status,
    400,
  )
  const edit = await fetchAs(OWNER, `${ownerBase}/api/documents/doc-ws/collaborators`, {
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
  assert.equal(await writeAs(id, 'a.txt', MEMBER), 200)

  const del = await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}/associations/project/p-ws`, {
    method: 'DELETE',
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(del.status, 200)
  assert.equal(await writeAs(id, 'b.txt', MEMBER), 403)
  assert.equal(await writeAs(id, 'c.txt', OWNER), 200) // explicit members unaffected by association churn
  assert.deepEqual((await detailJson(id)).associations, [])
  assert.equal(
    (
      await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}/associations/project/p-ws`, {
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

  assert.equal((await associate(id, 'project', 'p-ws', MEMBER)).status, 403)
  assert.equal((await associate(id, 'board_card', 'card-ws', MEMBER)).status, 403)
  assert.equal((await associate(id, 'document', 'doc-ws', MEMBER)).status, 201)
})

test('cross-company isolation: associations only see same-company targets', async () => {
  await seedProjectWithConversation([MEMBER])
  const dirB = await mkdtemp(join(tmpRoot, 'bound-b-'))
  const bCreate = await createWorkspace({ user: OUTSIDER, company: COMPANY_B, folderPath: dirB })
  assert.equal(bCreate.status, 201)
  const bId = ((await bCreate.json()) as { id: string }).id
  await pool.query(
    `INSERT INTO projects (id, company_id, name) VALUES ('p-b', $1, 'B Project') ON CONFLICT DO NOTHING`,
    [COMPANY_B],
  )

  assert.equal((await associate(bId, 'project', 'p-ws', OUTSIDER, COMPANY_B)).status, 404) // A's project invisible to B
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'project', 'p-b')).status, 404) // B's project invisible to A
  assert.equal((await associate(id, 'project', 'p-ws')).status, 201) // same-company works
})

// ---------- Default workspace (#30) ----------

async function defaultWorkspaceId(user: string = OWNER): Promise<string> {
  const res = await fetchAs(user, `${MIRROR_BASE}/api/workspaces`, { headers: jsonHeaders(COMPANY) })
  const rows = (await res.json()) as Array<{ id: string; isDefault: boolean }>
  return (rows.find((r) => r.isDefault) as { id: string }).id
}

test('every team gets exactly one default workspace, self-healing on repeat listing', async () => {
  const firstId = await defaultWorkspaceId()
  assert.match(firstId, /^ws-default-/)
  const again = await fetchAs(OWNER, `${ownerBase}/api/workspaces`, { headers: jsonHeaders(COMPANY) })
  const rows = (await again.json()) as Array<{ id: string; isDefault: boolean }>
  const defaults = rows.filter((r) => r.isDefault)
  assert.equal(defaults.length, 1)
  assert.equal(defaults[0].id, firstId)
})

test('default workspace: whole team reads and writes without being added; scope follows company membership', async () => {
  const defId = await defaultWorkspaceId()
  assert.equal(await writeAs(defId, 'a.txt', MEMBER), 200) // never explicitly added

  const read = await fetchAs(MEMBER, `${memberBase}/api/workspaces/${defId}/file${q('a.txt')}`, {
    headers: jsonHeaders(COMPANY),
  })
  assert.equal(read.status, 200)
  assert.equal(((await read.json()) as { body: string }).body, 'x')

  const detail = (await (
    await fetchAs(OWNER, `${ownerBase}/api/workspaces/${defId}`, { headers: jsonHeaders(COMPANY) })
  ).json()) as { folderPath: string; members: Array<{ participantId: string; source: string }> }
  assert.ok(detail.folderPath.includes('workspaces')) // product-managed folder
  const sources = new Map(detail.members.map((m) => [m.participantId, m.source]))
  assert.equal(sources.get(MEMBER), 'implicit')
  assert.equal(sources.get(AGENT), 'implicit') // humans and agents alike
  assert.equal(sources.get(OWNER), 'implicit') // the whole team, not an explicit list

  await pool.query(`DELETE FROM company_members WHERE company_id = $1 AND user_id = $2`, [COMPANY, MEMBER])
  assert.equal(await writeAs(defId, 'b.txt', MEMBER), 403) // out of the team
  await pool.query(
    `INSERT INTO company_members (company_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`,
    [COMPANY, MEMBER],
  )
  assert.equal(await writeAs(defId, 'b.txt', MEMBER), 200) // back in
})

test('cross-company: another company cannot reach a team default workspace by its deterministic id', async () => {
  const defId = await defaultWorkspaceId()
  assert.equal(
    (await fetchAs(OUTSIDER, `${outsiderBase}/api/workspaces/${defId}`, { headers: jsonHeaders(COMPANY_B) })).status,
    404,
  )
  assert.equal(
    (await fetchAs(OUTSIDER, `${outsiderBase}/api/workspaces/${defId}/files`, { headers: jsonHeaders(COMPANY_B) })).status,
    404,
  )
})

// ---------- Safe unbind (#34) ----------

async function unbind(workspaceId: string, user: string = OWNER): Promise<Response> {
  return fetchAs(user, `${MIRROR_BASE}/api/workspaces/${workspaceId}/unbind`, { method: 'POST', headers: jsonHeaders(COMPANY) })
}

test('safe unbind: files untouched, all access refused, associations visible as inert history', async () => {
  await writeFile(join(boundDir, 'keep.txt'), 'precious', 'utf8')
  const { id } = await createWorkspaceJson()
  await addMember(id, MEMBER)
  assert.equal(await writeAs(id, 'a.txt', MEMBER), 200)

  assert.equal((await unbind(id)).status, 200)

  // Not a single file touched
  assert.equal(await readFile(join(boundDir, 'keep.txt'), 'utf8'), 'precious')
  assert.equal(await readFile(join(boundDir, 'a.txt'), 'utf8'), 'x')

  // All access refused (410) for every file surface
  assert.equal(await writeAs(id, 'b.txt', MEMBER), 410)
  assert.equal(
    (await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/file${q('keep.txt')}`, { headers: jsonHeaders(COMPANY) })).status,
    410,
  )
  assert.equal((await fetchAs(MEMBER, `${memberBase}/api/workspaces/${id}/files`, { headers: jsonHeaders(COMPANY) })).status, 410)

  // Hidden from the list; the detail shows the unbound state
  const list = (await (await fetchAs(OWNER, `${ownerBase}/api/workspaces`, { headers: jsonHeaders(COMPANY) })).json()) as Array<{
    id: string
  }>
  assert.ok(!list.some((r) => r.id === id))
  const detail = (await (
    await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}`, { headers: jsonHeaders(COMPANY) })
  ).json()) as { unboundAt: string; unboundBy: string }
  assert.ok(detail.unboundAt)
  assert.equal(detail.unboundBy, OWNER)

  // Mutations refused; idempotency explicit
  assert.equal((await unbind(id)).status, 409)
  assert.equal((await addMember(id, AGENT)).status, 410)
})

test('implicit access via associations ends at unbind; association create is 410; default never unbinds', async () => {
  await seedProjectWithConversation([MEMBER])
  const { id } = await createWorkspaceJson()
  assert.equal((await associate(id, 'project', 'p-ws')).status, 201)
  assert.equal(await writeAs(id, 'a.txt', MEMBER), 200)

  assert.equal((await unbind(id)).status, 200)
  assert.equal(await writeAs(id, 'b.txt', MEMBER), 410) // implicit membership inert
  assert.equal((await detailJson(id)).associations.length, 1) // still visible as history
  assert.equal((await associate(id, 'board_card', 'nope')).status, 410) // guard precedes target lookup
  // the audit record cannot be silently rewritten post-unbind
  assert.equal(
    (
      await fetchAs(OWNER, `${ownerBase}/api/workspaces/${id}/associations/project/p-ws`, {
        method: 'DELETE',
        headers: jsonHeaders(COMPANY),
      })
    ).status,
    410,
  )

  const defId = await defaultWorkspaceId()
  assert.equal((await unbind(defId)).status, 403)
})

test('only owner/admin can unbind', async () => {
  const { id } = await createWorkspaceJson()
  assert.equal((await unbind(id, MEMBER)).status, 403)
})

// #338 multipart 上传 + 原始字节读:round-trip 字节一致 / 25MB 帽 / 防
// 逃逸与保留路径 / 图片 Content-Type(mutation 管理面 API 由上方 28 个
// 既有用例覆盖,此处不重复)。
test('[mirror-workspaces] upload → raw round-trip + guards', async () => {
  const folder = await mkdtemp(join(tmpdir(), 'ws-up-'))
  const wsRes = await createWorkspace({ name: 'UploadRT', folderPath: folder })
  const ws = (await wsRes.json()) as { id: string }
  const post = (body: FormData) =>
    fetchAs(OWNER, `${MIRROR_BASE}/api/workspaces/${ws.id}/upload`, { method: 'POST', body })

  // 小 PNG 头(字节级 round-trip 的最小非文本样本)。
  const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 250, 255, 0, 128])
  const form = new FormData()
  form.append('path', 'pics/logo.png')
  form.append('file', new Blob([png]), 'logo.png')
  let res = await post(form)
  assert.equal(res.status, 200)
  const up = (await res.json()) as { ok: boolean; path: string; size: number; mtimeNanos: string }
  assert.ok(up.ok)
  assert.equal(up.size, png.byteLength)
  assert.ok(/^\d+$/.test(up.mtimeNanos), 'mtimeNanos is a decimal string (JS-safe)')

  // raw 读:字节一致 + 图片 Content-Type。
  const raw = await fetchAs(OWNER, `${MIRROR_BASE}/api/workspaces/${ws.id}/raw?path=${encodeURIComponent('pics/logo.png')}`)
  assert.equal(raw.status, 200)
  assert.equal(raw.headers.get('content-type'), 'image/png')
  const back = new Uint8Array(await raw.arrayBuffer())
  assert.deepEqual(Array.from(back), Array.from(png), 'binary round-trip must be byte-exact')

  // 非图片 → octet-stream。
  await writeFile(join(folder, 'data.bin'), Buffer.from([0, 255, 1, 254]))
  const bin = await fetchAs(OWNER, `${MIRROR_BASE}/api/workspaces/${ws.id}/raw?path=data.bin`)
  assert.equal(bin.headers.get('content-type'), 'application/octet-stream')

  // 保留路径 / 逃逸路径拒。
  const bad = new FormData()
  bad.append('path', '.cumora/evil')
  bad.append('file', new Blob(['x']), 'evil')
  res = await post(bad)
  assert.equal(res.status, 400)
  const esc = new FormData()
  esc.append('path', '../escape')
  esc.append('file', new Blob(['x']), 'esc')
  res = await post(esc)
  assert.equal(res.status, 400)

  // 25MB 帽:25MB+1 → 413(灌一个超限 buffer,只填首尾)。
  const huge = new Uint8Array(25 * 1024 * 1024 + 1)
  huge[0] = 1; huge[huge.length - 1] = 2
  const over = new FormData()
  over.append('path', 'big.bin')
  over.append('file', new Blob([huge]), 'big.bin')
  res = await post(over)
  assert.equal(res.status, 413)

  // 写前快照:再传同 path → 旧版留档 .cumora/versions/。
  const again = new FormData()
  again.append('path', 'pics/logo.png')
  again.append('file', new Blob([new Uint8Array([9, 9, 9])]), 'logo.png')
  res = await post(again)
  assert.equal(res.status, 200)
  const versions = await readdir(join(folder, '.cumora', 'versions', 'pics', 'logo.png'))
  assert.ok(versions.length >= 1, 'overwrite upload leaves a version snapshot')
})
