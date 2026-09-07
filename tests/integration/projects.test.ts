/**
 * Projects tracer-bullet tests (#354 — 概念整合刀 1,ADR 0008):
 * 建项目强制盘(默认自动建/自填校验/冲突 409)、列表扩列与默认置顶、
 * 并表后 workspaces 族与 projects 族返回同一集合、删除端点(唯一生命
 * 周期出口)的级联语义:对话 SET NULL、交付台账随卡片存活(引用置
 * NULL)、成员/关联清理、盘文件原地保留、is_default 拒删、归档 410。
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, readdir, readFile, rm, stat, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, MIRROR_BASE } from './_helpers.js'
import { pool } from './harness/db/pool.js'

const OWNER = 'pj-owner'
const MEMBER = 'pj-member'
const AGENT = 'pj-agent'
const COMPANY = 'c-pj'

let tmpRoot = ''

const jsonHeaders = (company: string) => ({ 'x-company-id': company, 'content-type': 'application/json' })

async function fetchAs(user: string, url: string, init?: RequestInit): Promise<Response> {
  return fetch(url, { ...init, headers: { 'x-test-user': user, ...(init?.headers ?? {}) } })
}

async function createProject(body: Record<string, unknown>, user = OWNER): Promise<Response> {
  return fetchAs(user, `${MIRROR_BASE}/api/projects`, {
    method: 'POST',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify(body),
  })
}

async function listProjects(user = OWNER): Promise<Array<Record<string, any>>> {
  const res = await fetchAs(user, `${MIRROR_BASE}/api/projects`, { headers: jsonHeaders(COMPANY) })
  assert.equal(res.status, 200)
  return (await res.json()) as Array<Record<string, any>>
}

async function deleteProject(id: string, user = OWNER): Promise<Response> {
  return fetchAs(user, `${MIRROR_BASE}/api/projects/${id}`, { method: 'DELETE', headers: jsonHeaders(COMPANY) })
}

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
  tmpRoot = await mkdtemp(join(tmpdir(), 'cumora-pj-'))
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Pj Co', 'pj', $2)`,
    [COMPANY, OWNER],
  )
  await seedUserMembership(OWNER, COMPANY)
  await seedUserMembership(MEMBER, COMPANY)
  await pool.query(`UPDATE company_members SET role = 'member' WHERE company_id = $1 AND user_id = $2`, [
    COMPANY,
    MEMBER,
  ])
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Agent Pj', 'tester', 'A', '#abcdef', 'avail') ON CONFLICT DO NOTHING`,
    [AGENT, COMPANY],
  )
})

after(async () => {
  await rm(tmpRoot, { recursive: true, force: true }).catch(() => {})
  await teardownAll()
})

test('create: mandatory folder — auto-created under the managed root; creator becomes explicit member', async () => {
  const res = await createProject({ name: 'Auto', description: 'd' })
  assert.equal(res.status, 201)
  const p = (await res.json()) as { id: string; folderPath: string; isDefault: boolean }
  assert.match(p.id, /^p-/)
  assert.ok(p.folderPath.length > 0, 'folder auto-created')
  assert.equal(p.isDefault, false)
  const st = await stat(p.folderPath)
  assert.equal(st.isDirectory(), true)

  // creator is an explicit member: file access works without any conversation
  const w = await fetchAs(OWNER, `${MIRROR_BASE}/api/workspaces/${p.id}/file?path=hi.txt`, {
    method: 'PUT',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ body: 'x' }),
  })
  assert.equal(w.status, 200)
})

test('create: custom folderPath — realpath required, at most one project per folder (409)', async () => {
  const dir = await mkdtemp(join(tmpRoot, 'custom-'))
  const ok = await createProject({ name: 'Custom', folderPath: dir })
  assert.equal(ok.status, 201)
  const p1 = (await ok.json()) as { id: string; folderPath: string }
  assert.equal(p1.folderPath, dir)

  const again = await createProject({ name: 'Dup', folderPath: dir })
  assert.equal(again.status, 409)

  const missing = await createProject({ name: 'Nope', folderPath: '/tmp/definitely-not-here-354' })
  assert.equal(missing.status, 404)
})

test('list: merged collection — workspaces-family rows appear among projects; default pinned first', async () => {
  const wdir = await mkdtemp(join(tmpRoot, 'wsl-'))
  const wsRes = await fetchAs(OWNER, `${MIRROR_BASE}/api/workspaces`, {
    method: 'POST',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ name: 'FromWs', folderPath: wdir }),
  })
  assert.equal(wsRes.status, 201)
  const ws = (await wsRes.json()) as { id: string }

  await createProject({ name: 'P1' })
  const rows = await listProjects()
  const ids = rows.map((r) => r.id)
  assert.ok(ids.includes(ws.id), 'workspace-family row present in projects list (merged entity)')
  assert.equal(rows[0].isDefault, true, 'default project pinned first')
  const defaults = rows.filter((r) => r.isDefault)
  assert.equal(defaults.length, 1)
  for (const r of rows) {
    assert.ok('folderPath' in r && 'isDefault' in r, 'new columns present')
  }
})

test('delete: cascades — conversations SET NULL, delivery survives with NULL ref, members/associations cleared, folder kept', async () => {
  const dir = await mkdtemp(join(tmpRoot, 'del-'))
  await writeFile(join(dir, 'keep.txt'), 'precious', 'utf8')
  const res = await createProject({ name: 'Doomed', folderPath: dir })
  const p = (await res.json()) as { id: string }

  // a conversation attached to the project survives detached
  await pool.query(
    `INSERT INTO conversations (id, company_id, kind, title, members, project_id)
     VALUES ('cv-pj', $1, 'group', 'Pj Conv', '[]'::jsonb, $2) ON CONFLICT DO NOTHING`,
    [COMPANY, p.id],
  )
  // a delivery row referencing the project (start-style record) on a real card
  await pool.query(
    `INSERT INTO boards (id, company_id, title, created_by) VALUES ('b-pj', $1, 'Pj Board', $2) ON CONFLICT DO NOTHING`,
    [COMPANY, OWNER],
  )
  await pool.query(
    `INSERT INTO board_columns (id, board_id, title, position) VALUES ('col-pj', 'b-pj', 'T', 0) ON CONFLICT DO NOTHING`,
  )
  await pool.query(
    `INSERT INTO board_cards (id, board_id, column_id, title, created_by) VALUES ('card-pj', 'b-pj', 'col-pj', 'D', $1) ON CONFLICT DO NOTHING`,
    [OWNER],
  )
  await pool.query(
    `INSERT INTO card_deliveries (id, card_id, workspace_id, branch, created_by)
     VALUES ('dlv-pj', 'card-pj', $1, 'cumora/card-pj', $2)`,
    [p.id, AGENT],
  )

  const del = await deleteProject(p.id)
  assert.equal(del.status, 200)
  const body = (await del.json()) as { ok: boolean; folderKept: string }
  assert.equal(body.ok, true)
  assert.equal(body.folderKept, dir)

  // conversation survived, detached
  const cv = await pool.query(`SELECT project_id FROM conversations WHERE id = 'cv-pj'`)
  assert.equal(cv.rowCount, 1)
  assert.equal(cv.rows[0].project_id, null)
  // delivery row survived with a NULL reference (traceability kept)
  const dlv = await pool.query(`SELECT workspace_id, branch FROM card_deliveries WHERE id = 'dlv-pj'`)
  assert.equal(dlv.rowCount, 1)
  assert.equal(dlv.rows[0].workspace_id, null)
  // members/associations cleared; row gone; folder files untouched
  const members = await pool.query(`SELECT 1 FROM workspace_members WHERE workspace_id = $1`, [p.id])
  assert.equal(members.rowCount, 0)
  const gone = await deleteProject(p.id)
  assert.equal(gone.status, 404)
  assert.equal(await readFile(join(dir, 'keep.txt'), 'utf8'), 'precious')
  assert.ok((await readdir(dir)).includes('keep.txt'))
})

test('delete: default project refused; member role refused; archived endpoint retired (410)', async () => {
  const rows = await listProjects()
  const def = rows.find((r) => r.isDefault) as { id: string }
  assert.equal((await deleteProject(def.id)).status, 403)

  const mine = await createProject({ name: 'X' })
  const p = (await mine.json()) as { id: string }
  assert.equal((await deleteProject(p.id, MEMBER)).status, 403)

  const arch = await fetchAs(OWNER, `${MIRROR_BASE}/api/projects/${p.id}/archive`, {
    method: 'POST',
    headers: jsonHeaders(COMPANY),
    body: JSON.stringify({ archive: true }),
  })
  assert.equal(arch.status, 410)
  assert.match(String(((await arch.json()) as { error?: string }).error), /retired/)
})
