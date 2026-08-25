/**
 * Integration test: `cumora workspace …` — the TEAM workspace surface for
 * agents (#31). Exercises the agent CLI against a real bound folder, using
 * the same shared membership core as the human HTTP API (covered by
 * workspaces.test.ts). Also pins the ADR 0002 vocabulary split: `workspace`
 * = team surface, `ws` = the agent's Private Area.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { ensureSchemaOnce, resetAllTables, seedCompanyWithAgent, teardownAll } from './_helpers.js'
import { pool } from '../db/pool.js'

const NOVA = 'nova-ws'
const BRAM = 'bram-ws'

let tmpRoot: string
let boundDir: string
let companyId: string

before(async () => {
  await ensureSchemaOnce()
  tmpRoot = await mkdtemp(join(tmpdir(), 'cumora-wscli-'))
})

beforeEach(async () => {
  await resetAllTables()
  const seeded = await seedCompanyWithAgent({ agentId: NOVA })
  companyId = seeded.companyId
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Bram', 'engineer', 'B', '#abcdef', 'avail')
     ON CONFLICT DO NOTHING`,
    [BRAM, companyId],
  )
  boundDir = await mkdtemp(join(tmpRoot, 'bound-'))
  await pool.query(
    `INSERT INTO workspaces (id, company_id, name, folder_path) VALUES ('ws-t1', $1, 'Repo files', $2)`,
    [companyId, boundDir],
  )
  await pool.query(
    `INSERT INTO workspace_members (workspace_id, participant_id, added_by) VALUES ('ws-t1', $1, $1)`,
    [NOVA],
  )
})

after(async () => {
  await rm(tmpRoot, { recursive: true, force: true }).catch(() => {})
  await teardownAll()
})

async function runCliRaw(argv: string[]) {
  const { runCli } = await import('../agents/cli.js')
  return runCli(argv)
}

test('workspace ls lists team workspaces incl. the auto-provisioned default', async () => {
  const r = await runCliRaw(['workspace', 'ls', '--as', NOVA])
  assert.ok(r.ok, r.text)
  assert.match(r.text, /ws-t1/)
  assert.match(r.text, /ws-default-/)
  assert.match(r.text, /\[default\]/)
})

test('member agent reads and writes team workspace files — verified on disk', async () => {
  const w = await runCliRaw(['workspace', 'write', 'ws-t1', 'notes/a.txt', 'hello team', '--as', NOVA])
  assert.ok(w.ok, w.text)
  assert.match(w.text, /wrote notes\/a\.txt/)

  const onDisk = await readFile(join(boundDir, 'notes', 'a.txt'), 'utf8')
  assert.equal(onDisk, 'hello team')

  const r = await runCliRaw(['workspace', 'read', 'ws-t1', 'notes/a.txt', '--as', NOVA])
  assert.ok(r.ok, r.text)
  assert.equal(r.text, 'hello team')
})

test('non-member agent is rejected with the same message a human gets', async () => {
  const r = await runCliRaw(['workspace', 'read', 'ws-t1', 'a.txt', '--as', BRAM])
  assert.ok(!r.ok)
  assert.equal(r.text, 'not a member of this workspace')
  const w = await runCliRaw(['workspace', 'write', 'ws-t1', 'a.txt', 'nope', '--as', BRAM])
  assert.ok(!w.ok)
  assert.equal(w.text, 'not a member of this workspace')
})

test('default workspace: every agent of the team reads and writes without membership', async () => {
  const r = await runCliRaw(['workspace', 'ls', '--as', BRAM])
  assert.ok(r.ok, r.text)
  const defId = `ws-default-${companyId}`
  assert.match(r.text, new RegExp(defId))

  const w = await runCliRaw(['workspace', 'write', defId, 'bram.txt', 'from bram', '--as', BRAM])
  assert.ok(w.ok, w.text)
  const back = await runCliRaw(['workspace', 'read', defId, 'bram.txt', '--as', BRAM])
  assert.ok(back.ok, back.text)
  assert.equal(back.text, 'from bram')
})

test('ws still addresses the Private Area — vocabulary split (ADR 0002)', async () => {
  const priv = await runCliRaw(['ws', 'write', 'scratch.md', 'private note', '--as', NOVA])
  assert.ok(priv.ok, priv.text)

  const privList = await runCliRaw(['ws', 'ls', '--as', NOVA])
  assert.ok(privList.ok, privList.text)
  assert.match(privList.text, /Private Area/)
  assert.match(privList.text, /scratch\.md/)

  // The private file must NOT be readable through the team surface.
  const teamRead = await runCliRaw(['workspace', 'read', 'ws-t1', 'scratch.md', '--as', NOVA])
  assert.ok(!teamRead.ok)
  assert.match(teamRead.text, /file not found/)
})
