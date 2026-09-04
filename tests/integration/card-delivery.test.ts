/**
 * Card delivery tracer-bullet tests (#265 — Workspace 战役刀 4):
 * `cumora card start` materializes a git worktree in the card's linked
 * workspace folder (branch cumora/<cardId>, survives task failure by never
 * being deleted) and records the delivery row the moment work begins;
 * `cumora card deliver` records/extends the PR link + review state; the
 * human board view (GET /api/boards/{id}) carries deliveries on every card.
 * Fixture: real tmp folder + real git init; agent acts via /runtime/cli,
 * owner acts via REST (x-test-user), assertions go to disk, git and the DB.
 */
import { test, before, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { mkdtemp, rm, stat, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomUUID } from 'node:crypto'
import { ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, MIRROR_BASE } from './_helpers.js'
import { signAgentToken } from './harness/agents/runtime/jwt.js'
import { pool } from './harness/db/pool.js'

const OWNER = 'dlv-owner'
const COMPANY = 'c-dlv'
const AGENT = 'a-dlv-1'
const OTHER_AGENT = 'a-dlv-2'

let tmpRoot = ''
let repoDir = ''

async function fetchAs(user: string, url: string, init?: RequestInit): Promise<Response> {
  return fetch(url, { ...init, headers: { 'x-test-user': user, ...(init?.headers ?? {}) } })
}

async function cli(token: string, argv: string[]): Promise<{ ok: boolean; text: string; exitCode: number }> {
  const res = await fetch(`${MIRROR_BASE}/runtime/cli`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify({ argv }),
  })
  assert.equal(res.status, 200)
  return (await res.json()) as { ok: boolean; text: string; exitCode: number }
}

function git(cwd: string, ...args: string[]): string {
  return execFileSync('git', ['-C', cwd, ...args], { encoding: 'utf8' })
}

let token = ''
let otherToken = ''

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
  await ensureSchemaOnce()
  tmpRoot = await mkdtemp(join(tmpdir(), 'cumora-dlv-'))
  token = signAgentToken({ agentId: AGENT, companyId: COMPANY })
  otherToken = signAgentToken({ agentId: OTHER_AGENT, companyId: COMPANY })
})

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Deliv Co', 'dlv', $2)`,
    [COMPANY, OWNER],
  )
  await seedUserMembership(OWNER, COMPANY)
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', 'Agent Dlv', 'tester', 'A', '#abcdef', 'avail'),
            ($3, $2, 'agent', 'Agent Two', 'tester', 'B', '#fedcba', 'avail')`,
    [AGENT, COMPANY, OTHER_AGENT],
  )
  // Fresh git repo fixture per test (real repo on real disk — the whole point).
  repoDir = await mkdtemp(join(tmpRoot, 'repo-'))
  git(repoDir, 'init')
  git(repoDir, 'config', 'user.email', 't@cumora.local')
  git(repoDir, 'config', 'user.name', 't')
  await writeFile(join(repoDir, 'README.md'), '# repo\n')
  git(repoDir, 'add', '.')
  git(repoDir, 'commit', '-m', 'init')
})

after(async () => {
  await rm(tmpRoot, { recursive: true, force: true }).catch(() => {})
  await teardownAll()
})

/** board + column + card via SQL fixture; workspace via REST (realpath
 * resolution), association via SQL (its REST gate is #338's business). */
async function seedBoardCardWithWorkspace(): Promise<{ cardId: string; wsId: string; boardId: string }> {
  const boardId = `bd-${randomUUID().slice(0, 8)}`
  const cardId = `card-${randomUUID().slice(0, 12)}`
  await pool.query(
    `INSERT INTO boards (id, company_id, title, created_by) VALUES ($1, $2, 'Delivery board', $3)`,
    [boardId, COMPANY, AGENT],
  )
  await pool.query(
    `INSERT INTO board_columns (id, board_id, title, position) VALUES ('col-todo', $1, 'Todo', 1000)`,
    [boardId],
  )
  await pool.query(
    `INSERT INTO board_cards (id, board_id, column_id, title, created_by) VALUES ($1, $2, 'col-todo', 'Fix login flow', $3)`,
    [cardId, boardId, AGENT],
  )
  const wsRes = await fetchAs(OWNER, `${MIRROR_BASE}/api/workspaces`, {
    method: 'POST',
    headers: { 'x-company-id': COMPANY, 'content-type': 'application/json' },
    body: JSON.stringify({ name: 'Repo', folderPath: repoDir }),
  })
  assert.equal(wsRes.status, 201)
  const wsId = ((await wsRes.json()) as { id: string }).id
  await pool.query(
    `INSERT INTO workspace_associations (id, workspace_id, company_id, target_kind, target_id, created_by)
     VALUES ($1, $2, $3, 'board_card', $4, $5)`,
    [`wa-${randomUUID().slice(0, 12)}`, wsId, COMPANY, cardId, OWNER],
  )
  return { cardId, wsId, boardId }
}

async function boardCards(boardId: string): Promise<any[]> {
  const res = await fetchAs(OWNER, `${MIRROR_BASE}/api/boards/${boardId}`, {
    headers: { 'x-company-id': COMPANY },
  })
  assert.equal(res.status, 200)
  return ((await res.json()) as { cards: any[] }).cards
}

test('claim → start: worktree materialized on disk, delivery row visible immediately (failure-safe)', async () => {
  const { cardId, wsId, boardId } = await seedBoardCardWithWorkspace()

  // Not claimed yet → the gate holds.
  const gated = await cli(token, ['card', 'start', cardId])
  assert.equal(gated.ok, false)
  assert.match(gated.text, /claim the card first/)

  await cli(token, ['card', 'claim', cardId])
  const started = await cli(token, ['card', 'start', cardId])
  assert.equal(started.ok, true)
  assert.match(started.text, new RegExp(`cumora/${cardId}`))
  assert.match(started.text, new RegExp(`team/${wsId}/\\.cumora/worktrees/${cardId}`))

  // Real worktree on disk with a pointer .git; branch exists in the repo.
  const wtDir = join(repoDir, '.cumora', 'worktrees', cardId)
  const st = await stat(join(wtDir, '.git'))
  assert.equal(st.isFile(), true, 'worktree .git is a pointer file')
  assert.match(git(repoDir, 'branch', '--list'), new RegExp(`cumora/${cardId}`))

  // Delivery row is already on the card — a failing task's progress stays
  // findable (branch recorded before any PR exists).
  const cards = await boardCards(boardId)
  assert.equal(cards.length, 1)
  assert.equal(cards[0].deliveries.length, 1)
  assert.equal(cards[0].deliveries[0].branch, `cumora/${cardId}`)
  assert.equal(cards[0].deliveries[0].prUrl, null)

  // Idempotent: second start reuses, does not duplicate the row.
  const again = await cli(token, ['card', 'start', cardId])
  assert.equal(again.ok, true)
  assert.match(again.text, /already exists/)
  const rows = await pool.query(`SELECT * FROM card_deliveries WHERE card_id = $1`, [cardId])
  assert.equal(rows.rowCount, 1)
})

test('deliver: PR link + state recorded, incremental update keeps old fields', async () => {
  const { cardId, boardId } = await seedBoardCardWithWorkspace()
  await cli(token, ['card', 'claim', cardId])
  await cli(token, ['card', 'start', cardId])

  const badPr = await cli(token, ['card', 'deliver', cardId, '--branch', `cumora/${cardId}`, '--pr', 'ftp://nope'])
  assert.equal(badPr.ok, false)
  assert.match(badPr.text, /--pr must be an http\(s\) URL/)

  const badBranch = await cli(token, ['card', 'deliver', cardId, '--branch', 'bad branch..x'])
  assert.equal(badBranch.ok, false)
  assert.match(badBranch.text, /usage: card deliver/)

  const d1 = await cli(token, ['card', 'deliver', cardId, '--branch', `cumora/${cardId}`, '--pr', 'https://github.com/x/y/pull/1'])
  assert.equal(d1.ok, true)
  let cards = await boardCards(boardId)
  assert.equal(cards[0].deliveries[0].prUrl, 'https://github.com/x/y/pull/1')
  assert.equal(cards[0].deliveries[0].prState, 'open')

  // State-only update (--state merged, no --pr) keeps the recorded URL.
  const d2 = await cli(token, ['card', 'deliver', cardId, '--branch', `cumora/${cardId}`, '--state', 'merged'])
  assert.equal(d2.ok, true)
  cards = await boardCards(boardId)
  assert.equal(cards[0].deliveries[0].prUrl, 'https://github.com/x/y/pull/1')
  assert.equal(cards[0].deliveries[0].prState, 'merged')

  // card show surfaces the delivery to the next agent looking at the card.
  const show = await cli(token, ['card', 'show', cardId])
  assert.equal(show.ok, true)
  assert.match(show.text, /--- delivery ---/)
  assert.match(show.text, new RegExp(`cumora/${cardId}`))
})

test('start guards: unlinked card, non-git folder, non-assignee agent', async () => {
  // Unlinked card → operator guidance, nothing materialized.
  const boardId = `bd-${randomUUID().slice(0, 8)}`
  const cardId = `card-${randomUUID().slice(0, 12)}`
  await pool.query(`INSERT INTO boards (id, company_id, title, created_by) VALUES ($1, $2, 'B', $3)`, [boardId, COMPANY, AGENT])
  await pool.query(`INSERT INTO board_columns (id, board_id, title, position) VALUES ('col-t', $1, 'Todo', 1000)`, [boardId])
  await pool.query(`INSERT INTO board_cards (id, board_id, column_id, title, created_by) VALUES ($1, $2, 'col-t', 'No link', $3)`, [cardId, boardId, AGENT])
  await cli(token, ['card', 'claim', cardId])
  const unlinked = await cli(token, ['card', 'start', cardId])
  assert.equal(unlinked.ok, false)
  assert.match(unlinked.text, /not linked to a team workspace/)

  // Linked but folder is not a git repo → honest refusal.
  const plainDir = await mkdtemp(join(tmpRoot, 'plain-'))
  const wsRes = await fetchAs(OWNER, `${MIRROR_BASE}/api/workspaces`, {
    method: 'POST',
    headers: { 'x-company-id': COMPANY, 'content-type': 'application/json' },
    body: JSON.stringify({ name: 'Plain', folderPath: plainDir }),
  })
  const plainWs = ((await wsRes.json()) as { id: string }).id
  await pool.query(
    `INSERT INTO workspace_associations (id, workspace_id, company_id, target_kind, target_id, created_by)
     VALUES ($1, $2, $3, 'board_card', $4, $5)`,
    [`wa-${randomUUID().slice(0, 12)}`, plainWs, COMPANY, cardId, OWNER],
  )
  const notRepo = await cli(token, ['card', 'start', cardId])
  assert.equal(notRepo.ok, false)
  assert.match(notRepo.text, /not a git repository/)

  // A different agent than the assignee cannot start/deliver.
  const { cardId: claimed, boardId: b2 } = await seedBoardCardWithWorkspace()
  await cli(token, ['card', 'claim', claimed])
  const notMine = await cli(otherToken, ['card', 'start', claimed])
  assert.equal(notMine.ok, false)
  assert.match(notMine.text, /only the assignee/)
  const notMine2 = await cli(otherToken, ['card', 'deliver', claimed, '--branch', 'cumora/x'])
  assert.equal(notMine2.ok, false)
  assert.match(notMine2.text, /only the assignee/)
  // Sanity: the rightful assignee still can.
  const mine = await cli(token, ['card', 'start', claimed])
  assert.equal(mine.ok, true)
  assert.equal(b2.length > 0, true)
})

test('worktree survives: branch keeps agent progress after the task "fails"', async () => {
  const { cardId, boardId } = await seedBoardCardWithWorkspace()
  await cli(token, ['card', 'claim', cardId])
  await cli(token, ['card', 'start', cardId])

  // Agent did some work on the worktree branch, then the task failed —
  // no PR, no deliver. The branch and the recorded delivery stay.
  const wtDir = join(repoDir, '.cumora', 'worktrees', cardId)
  await writeFile(join(wtDir, 'wip.txt'), 'half-finished fix\n')
  git(wtDir, 'add', '.')
  git(wtDir, 'commit', '-m', 'wip')

  const cards = await boardCards(boardId)
  assert.equal(cards[0].deliveries[0].branch, `cumora/${cardId}`)
  assert.match(git(repoDir, 'branch', '--list'), new RegExp(`cumora/${cardId}`))
  const log = git(repoDir, 'log', `cumora/${cardId}`, '--oneline')
  assert.match(log, /wip/)
})

test('deliver without start: fresh INSERT on a self-built branch, --ws routing, single-card lookup', async () => {
  const { cardId, boardId } = await seedBoardCardWithWorkspace()
  await cli(token, ['card', 'claim', cardId])

  // Self-built branch (no `card start`): deliver takes the fresh-INSERT
  // path and records the workspace of the card's association.
  const d = await cli(token, ['card', 'deliver', cardId, '--branch', 'feature/self-1', '--pr', 'https://github.com/x/y/pull/9'])
  assert.equal(d.ok, true)
  const wsRows = await pool.query<{ workspace_id: string }>(
    `SELECT workspace_id FROM card_deliveries WHERE card_id = $1 AND branch = 'feature/self-1'`,
    [cardId],
  )
  assert.equal(wsRows.rowCount, 1)
  const wsId = wsRows.rows[0].workspace_id
  const cards = await boardCards(boardId)
  assert.equal(cards[0].deliveries[0].branch, 'feature/self-1')
  assert.equal(cards[0].deliveries[0].prState, 'open')

  // --ws pointing at a workspace NOT linked to the card is refused.
  const badWs = await cli(token, ['card', 'deliver', cardId, '--branch', 'feature/self-2', '--ws', 'ws-nope'])
  assert.equal(badWs.ok, false)
  assert.match(badWs.text, /not linked to card/)

  // Single-card lookup (GET /api/cards/{id}) carries the delivery too.
  const res = await fetchAs(OWNER, `${MIRROR_BASE}/api/cards/${cardId}`, {
    headers: { 'x-company-id': COMPANY },
  })
  assert.equal(res.status, 200)
  const lookup = (await res.json()) as { card: { deliveries: { branch: string; prUrl: string | null }[] } }
  assert.equal(lookup.card.deliveries.length, 1)
  assert.equal(lookup.card.deliveries[0].branch, 'feature/self-1')
  assert.equal(lookup.card.deliveries[0].prUrl, 'https://github.com/x/y/pull/9')
  assert.equal(wsId.length > 0, true)
})
