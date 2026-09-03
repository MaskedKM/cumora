/**
 * 验收镜像 · 公司 Skills 库(#261)—— CRUD 面(/api/skills)+ daemon
 * 分发面(/api/computers/me/skills*)。daemon 物化半边(引擎目录落盘)
 * 在 byoa-daemon 的 skills_sync_internal_test.go;此处钉住"服务器给出
 * 的清单与整包和物化端约定一致":bundle_hash 内容寻址(跨公司同内容同
 * 键)、清单带 companyId 分组键、设备令牌门。
 */
import assert from 'node:assert/strict'
import { createHash, randomUUID } from 'node:crypto'
import { after, beforeEach, test } from 'node:test'
import { pool } from './harness/db/pool.js'
import { signAgentToken } from './harness/agents/runtime/jwt.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror, teardownAll,
} from './_helpers.js'

const USER = 'u-mirror-skills'
const COMPANY = 'c-mirror-skills'

async function seedCompanyAndUser(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Skills Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
}

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUser()
})

after(async () => { await mirror.close(); await teardownAll() })

/** 服务端 composeSkillMd 的 TS 镜像(body 便捷位组装形)。 */
function composeSkillMd(name: string, description: string, body: string): string {
  return `---\nname: ${name}\ndescription: ${description}\n---\n\n${body}`
}

/** 服务端 bundleHash 的 TS 镜像(长度前缀拼接,文件按 path 排序)。 */
function bundleHash(files: Array<{ path: string; body: string }>): string {
  const h = createHash('sha256')
  for (const f of [...files].sort((a, b) => (a.path < b.path ? -1 : 1))) {
    h.update(`${f.path.length}:${f.path}${f.body.length}:${f.body}`)
  }
  return h.digest('hex')
}

test('[mirror-skills] list starts empty (skills array, never null)', async () => {
  const r = await call('/skills')
  assert.equal(r.status, 200)
  assert.deepEqual(r.json.skills, [])
})

test('[mirror-skills] create via body convenience: frontmatter composed, hash content-addressed', async () => {
  const body = 'Step 1. Deploy.\nStep 2. Verify.'
  const r = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'deploy-runbook', description: 'How we ship', body }),
  })
  assert.equal(r.status, 201)
  const skillMd = composeSkillMd('deploy-runbook', 'How we ship', body)
  assert.equal(r.json.bundleHash, bundleHash([{ path: 'SKILL.md', body: skillMd }]))

  const got = await call(`/skills/${r.json.id}`)
  assert.equal(got.status, 200)
  assert.equal(got.json.name, 'deploy-runbook')
  assert.equal(got.json.files.length, 1)
  assert.equal(got.json.files[0].path, 'SKILL.md')
  assert.equal(got.json.files[0].body, skillMd)

  const list = await call('/skills')
  assert.equal(list.json.skills.length, 1)
  assert.equal(list.json.skills[0].fileCount, 1)
  assert.equal(list.json.skills[0].createdBy, USER)
})

test('[mirror-skills] create with explicit multi-file bundle', async () => {
  const files = [
    { path: 'SKILL.md', body: 'Use the rollback reference.' },
    { path: 'references/rollback.md', body: '1. Restore snapshot' },
  ]
  const r = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'multi', description: 'multi-file', files }),
  })
  assert.equal(r.status, 201)
  assert.equal(r.json.bundleHash, bundleHash(files))
  const got = await call(`/skills/${r.json.id}`)
  assert.equal(got.json.files.length, 2)
})

test('[mirror-skills] duplicate name → 409; unknown id → 404', async () => {
  const a = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'dup', description: 'first', body: 'x' }),
  })
  assert.equal(a.status, 201)
  const b = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'dup', description: 'second', body: 'y' }),
  })
  assert.equal(b.status, 409)
  assert.match(String(b.json.error), /already exists/)
  assert.equal((await call('/skills/none')).status, 404)
})

test('[mirror-skills] validation: name/description/files rules', async () => {
  const bad = async (payload: unknown) => call('/skills', {
    method: 'POST', body: JSON.stringify(payload),
  })
  assert.equal((await bad({ name: 'Bad_Name', description: 'd', body: 'x' })).status, 400)
  assert.equal((await bad({ name: 'ok', description: '', body: 'x' })).status, 400)
  assert.equal((await bad({ name: 'ok', description: 'd', body: 'x', files: [] })).status, 400)
  // files 给了但没有根 SKILL.md
  assert.equal((await bad({
    name: 'ok', description: 'd',
    files: [{ path: 'other.md', body: 'x' }],
  })).status, 400)
  // 路径穿越
  assert.equal((await bad({
    name: 'ok', description: 'd',
    files: [{ path: '../evil.md', body: 'x' }, { path: 'SKILL.md', body: 'x' }],
  })).status, 400)
})

test('[mirror-skills] update: description-only keeps bundle; body change re-keys', async () => {
  const created = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'runbook', description: 'v1', body: 'original' }),
  })
  const id = created.json.id

  const descOnly = await call(`/skills/${id}`, {
    method: 'PUT', body: JSON.stringify({ description: 'v1 (edited)' }),
  })
  assert.equal(descOnly.status, 200)
  assert.equal(descOnly.json.bundleHash, created.json.bundleHash)
  const got = await call(`/skills/${id}`)
  assert.equal(got.json.description, 'v1 (edited)')
  assert.equal(got.json.files[0].body, composeSkillMd('runbook', 'v1', 'original'))

  const reBody = await call(`/skills/${id}`, {
    method: 'PUT', body: JSON.stringify({ body: 'rewritten' }),
  })
  assert.equal(reBody.status, 200)
  assert.notEqual(reBody.json.bundleHash, created.json.bundleHash)
  const got2 = await call(`/skills/${id}`)
  assert.equal(got2.json.files[0].body, composeSkillMd('runbook', 'v1 (edited)', 'rewritten'))

  assert.equal((await call(`/skills/${id}`, { method: 'PUT', body: '{}' })).status, 400)
})

test('[mirror-skills] delete removes the row', async () => {
  const created = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'gone', description: 'd', body: 'x' }),
  })
  assert.equal((await call(`/skills/${created.json.id}`, { method: 'DELETE' })).status, 200)
  assert.equal((await call(`/skills/${created.json.id}`, { method: 'DELETE' })).status, 404)
  assert.deepEqual((await call('/skills')).json.skills, [])
})

test('[mirror-skills] write face is privileged (member → 403, read stays 200)', async () => {
  await pool.query(`UPDATE company_members SET role = 'member' WHERE company_id = $1`, [COMPANY])
  const r = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'nope', description: 'd', body: 'x' }),
  })
  assert.equal(r.status, 403)
  assert.equal((await call('/skills')).status, 200)
})

test('[mirror-skills] computer distribution: list + bundle by hash + tenant gate', async () => {
  // 配对会种 4 个 starter agent 到该机——分发清单的公司集合来自它们。
  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const paired = await call('/computers/pair', {
    method: 'POST',
    body: JSON.stringify({ code, hostName: 'skills-host', engines: ['claude'], version: '9.9.9' }),
  })
  assert.equal(paired.status, 200)
  const device = paired.json.deviceToken
  const baseUrl = mirror.baseUrl()

  const body = 'Playbook body.'
  const created = await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'team-playbook', description: 'shared', body }),
  })
  assert.equal(created.status, 201)
  const hash = created.json.bundleHash

  // agents 名册行带 companyId(#261 分组键)
  const agents = await fetch(`${baseUrl}/api/computers/me/agents`, {
    headers: { authorization: `Bearer ${device}` },
  })
  const roster = (await agents.json()) as any[]
  assert.ok(roster.length >= 1)
  for (const a of roster) assert.equal(a.companyId, COMPANY)

  // 清单:companyId + name + hash
  const listRes = await fetch(`${baseUrl}/api/computers/me/skills`, {
    headers: { authorization: `Bearer ${device}` },
  })
  assert.equal(listRes.status, 200)
  const list = (await listRes.json()) as any[]
  assert.equal(list.length, 1)
  assert.equal(list[0].companyId, COMPANY)
  assert.equal(list[0].name, 'team-playbook')
  assert.equal(list[0].bundleHash, hash)

  // 整包:内容与哈希对得上(SKILL.md 组装形)
  const bundleRes = await fetch(`${baseUrl}/api/computers/me/skills/${hash}`, {
    headers: { authorization: `Bearer ${device}` },
  })
  assert.equal(bundleRes.status, 200)
  const bundle = (await bundleRes.json()) as any
  assert.equal(bundle.name, 'team-playbook')
  assert.equal(bundle.files[0].path, 'SKILL.md')
  assert.equal(bundle.files[0].body, composeSkillMd('team-playbook', 'shared', body))

  // 未知哈希 404;坏设备令牌 401
  assert.equal((await fetch(`${baseUrl}/api/computers/me/skills/${'0'.repeat(64)}`, {
    headers: { authorization: `Bearer ${device}` },
  })).status, 404)
  assert.equal((await fetch(`${baseUrl}/api/computers/me/skills`, {
    headers: { authorization: 'Bearer nope' },
  })).status, 401)
})

test('[mirror-skills] distribution is scoped to companies with hosted agents', async () => {
  // 别的公司有技能但没有 agent 在本机 → 不出现在清单
  const otherCo = 'c-skills-other'
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Other Co', $2, 'x-owner')`,
    [otherCo, otherCo],
  )
  const files = [{ path: 'SKILL.md', body: 'secret playbook' }]
  await pool.query(
    `INSERT INTO company_skills (id, company_id, name, description, files, bundle_hash)
     VALUES ('sk-other', $1, 'other-playbook', 'other', $2::jsonb, $3)`,
    [otherCo, JSON.stringify(files), bundleHash(files)],
  )
  const code = (await call('/computers', { method: 'POST', body: '{}' })).json.code
  const paired = await call('/computers/pair', {
    method: 'POST',
    body: JSON.stringify({ code, hostName: 'scoped-host', engines: ['claude'], version: '9.9.9' }),
  })
  const listRes = await fetch(`${mirror.baseUrl()}/api/computers/me/skills`, {
    headers: { authorization: `Bearer ${paired.json.deviceToken}` },
  })
  const list = (await listRes.json()) as any[]
  assert.equal(list.length, 0)
  // 且其哈希不可拉
  assert.equal((await fetch(`${mirror.baseUrl()}/api/computers/me/skills/${bundleHash(files)}`, {
    headers: { authorization: `Bearer ${paired.json.deviceToken}` },
  })).status, 404)
})

test('[mirror-skills] agent CLI: skills company lists the shared library', async () => {
  const agentId = `a-skills-${randomUUID().slice(0, 6)}`
  await pool.query(
    `INSERT INTO participants (id, company_id, kind, name, role, initial, avatar_bg, status)
     VALUES ($1, $2, 'agent', $1, 'tester', 'A', '#abcdef', 'avail')`,
    [agentId, COMPANY],
  )
  const token = signAgentToken({ agentId, companyId: COMPANY })
  await call('/skills', {
    method: 'POST',
    body: JSON.stringify({ name: 'team-playbook', description: 'shared ops', body: 'x' }),
  })
  const res = await fetch(`${mirror.baseUrl()}/runtime/cli`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
    body: JSON.stringify({ argv: ['skills', 'company'] }),
  })
  assert.equal(res.status, 200)
  const body = (await res.json()) as any
  assert.equal(body.ok, true)
  assert.match(body.text, /team-playbook/)
  assert.match(body.text, /shared ops/)
})
