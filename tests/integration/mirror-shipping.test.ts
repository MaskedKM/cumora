/**
 * 验收镜像 · shipping 全子面(#125,#117-f)。移植自 TS 退役前的
 * shipping.test.ts(三用例语义不变,切换 MIRROR 形态打 Go 服):
 * 1) 契约机全生命周期 —— 独立证据、staged production、readback;
 * 2) 建设者不得自证;
 * 3) 生产读回失败 → 退回 building + critical 摩擦。
 */
import { test, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { pool } from './harness/db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, teardownAll, startMirror,
} from './_helpers.js'

const USER_ID = 'u-ship-verifier'
const COMPANY_ID = 'c-shipping-loop'
const BUILDER_ID = 'a-builder'

await ensureSchemaOnce()
const mirror = startMirror(USER_ID, COMPANY_ID)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await pool.query(
    `INSERT INTO companies (id,name,slug,owner_user_id) VALUES ($1,'Shipping Co','shipping-co',$2)`,
    [COMPANY_ID, USER_ID],
  )
  await seedUserMembership(USER_ID, COMPANY_ID, { displayName: 'Independent Verifier' })
  await pool.query(
    `INSERT INTO participants (id,company_id,kind,name,role,initial,avatar_bg,status)
     VALUES ($1,$2,'agent','Builder','engineer','B','#123456','avail')`,
    [BUILDER_ID, COMPANY_ID],
  )
})

after(async () => { await mirror.close(); await teardownAll() })

test('[mirror-shipping] 契约机:独立证据 + staged production + readback', async () => {
  const created = await call('/shipping/features', {
    method: 'POST',
    body: JSON.stringify({
      title: 'Reliable export',
      problem: 'Exports can finish without a durable artifact.',
      desiredOutcome: 'Every completed export is downloadable and traceable.',
      contractSummary: 'One export path; no format redesign.',
      builderIds: [BUILDER_ID],
      riskLevel: 'high',
    }),
  })
  assert.equal(created.status, 201)
  const featureId = created.json.id as string
  assert.equal(created.json.verifications.length, 3, '三张默认必答格')

  const invariant = await call(`/shipping/features/${featureId}/invariants`, {
    method: 'POST',
    body: JSON.stringify({ title: 'Completion always references an existing artifact', kind: 'data', required: true }),
  })
  assert.equal(invariant.status, 201)
  const invariantId = invariant.json.invariants[0].id as string
  const square = await call(`/shipping/features/${featureId}/verifications`, {
    method: 'POST',
    body: JSON.stringify({
      title: 'Reconcile completed exports to storage', method: 'data_reconciliation',
      invariantId, ownerId: USER_ID, builderIds: [BUILDER_ID], required: true,
    }),
  })
  assert.equal(square.status, 201)

  for (const verification of square.json.verifications as Array<{ id: string; ownerId: string | null }>) {
    if (verification.ownerId) continue
    const assigned = await call(`/shipping/features/${featureId}/verifications/${verification.id}`, {
      method: 'PATCH', body: JSON.stringify({ ownerId: USER_ID }),
    })
    assert.equal(assigned.status, 200)
  }

  assert.equal((await call(`/shipping/features/${featureId}/transition`, { method: 'POST', body: '{"status":"contract"}' })).status, 200)
  assert.equal((await call(`/shipping/features/${featureId}/transition`, { method: 'POST', body: '{"status":"building"}' })).status, 200)
  assert.equal((await call(`/shipping/features/${featureId}/transition`, { method: 'POST', body: '{"status":"verifying"}' })).status, 200)

  const detail = (await call(`/shipping/features/${featureId}`)).json
  for (const verification of detail.verifications as Array<{ id: string }>) {
    const passed = await call(`/shipping/features/${featureId}/verifications/${verification.id}`, {
      method: 'PATCH',
      body: JSON.stringify({ status: 'passed', evidence: [{ note: `independent proof for ${verification.id}` }] }),
    })
    assert.equal(passed.status, 200)
  }
  assert.equal((await call(`/shipping/features/${featureId}/transition`, { method: 'POST', body: '{"status":"ready"}' })).status, 200)

  const prematureProduction = await call(`/shipping/features/${featureId}/releases`, {
    method: 'POST',
    body: JSON.stringify({
      environment: 'production', version: 'v1', releaseNotes: 'Reliable exports',
      rollbackPlan: 'Roll deployment back to prior digest', baseline: [{ metric: 'export_success_rate=99%' }],
    }),
  })
  assert.equal(prematureProduction.status, 201)
  const productionId = prematureProduction.json.releases[0].id as string
  const blockedApproval = await call(`/shipping/features/${featureId}/releases/${productionId}/action`, {
    method: 'POST', body: '{"action":"approve"}',
  })
  assert.equal(blockedApproval.status, 409)
  assert.match(blockedApproval.json.error, /staging|canary/i)

  const staging = await call(`/shipping/features/${featureId}/releases`, {
    method: 'POST', body: JSON.stringify({ environment: 'staging', version: 'v1-rc1' }),
  })
  const stagingId = (staging.json.releases as Array<{ id: string; environment: string }>).find((x) => x.environment === 'staging')?.id
  assert.ok(stagingId)
  assert.equal((await call(`/shipping/features/${featureId}/releases/${stagingId}/action`, { method: 'POST', body: '{"action":"start"}' })).status, 200)
  const undocumentedFailure = await call(`/shipping/features/${featureId}/releases/${stagingId}/action`, {
    method: 'POST', body: '{"action":"fail"}',
  })
  assert.equal(undocumentedFailure.status, 409)
  assert.match(undocumentedFailure.json.error, /diagnostic evidence/i)
  assert.equal((await call(`/shipping/features/${featureId}/releases/${stagingId}/action`, {
    method: 'POST', body: JSON.stringify({ action: 'succeed', evidence: [{ note: 'staging user path passed' }] }),
  })).status, 200)

  assert.equal((await call(`/shipping/features/${featureId}/releases/${productionId}/action`, { method: 'POST', body: '{"action":"approve"}' })).status, 200)
  assert.equal((await call(`/shipping/features/${featureId}/releases/${productionId}/action`, { method: 'POST', body: '{"action":"start"}' })).status, 200)
  const shipped = await call(`/shipping/features/${featureId}/releases/${productionId}/action`, {
    method: 'POST', body: JSON.stringify({ action: 'succeed', evidence: [{ note: 'authenticated production smoke passed' }] }),
  })
  assert.equal(shipped.status, 200)
  assert.equal(shipped.json.status, 'watching')
  assert.ok(shipped.json.releases.find((release: any) => release.id === productionId).readbackDueAt)

  assert.equal((await call(`/shipping/features/${featureId}/releases/${productionId}/action`, {
    method: 'POST', body: JSON.stringify({ action: 'readback_pass', evidence: [{ note: '24h export success rate remained above baseline' }] }),
  })).status, 200)
  const learned = await call(`/shipping/features/${featureId}/transition`, { method: 'POST', body: '{"status":"learned"}' })
  assert.equal(learned.status, 200)
  assert.equal(learned.json.status, 'learned')

  // overview:三块齐全,事件流贯穿
  const overview = await call('/shipping/overview')
  assert.equal(overview.status, 200)
  assert.ok(Array.isArray(overview.json.features))
  assert.ok(Array.isArray(overview.json.friction))
  assert.ok(Array.isArray(overview.json.dueReadbacks))
  const events = learned.json.events as Array<{ kind: string }>
  assert.ok(events.some((e) => e.kind === 'feature.created'))
  assert.ok(events.some((e) => e.kind === 'release.succeed'))
})

test('[mirror-shipping] 建设者不得自证', async () => {
  const created = await call('/shipping/features', {
    method: 'POST', body: JSON.stringify({ title: 'Self check', builderIds: [USER_ID] }),
  })
  const featureId = created.json.id as string
  const squareId = created.json.verifications[0].id as string
  const result = await call(`/shipping/features/${featureId}/verifications/${squareId}`, {
    method: 'PATCH',
    body: JSON.stringify({ status: 'passed', evidence: [{ note: 'I checked my own work' }] }),
  })
  assert.equal(result.status, 409)
  assert.match(result.json.error, /cannot verify their own work/i)
})

test('[mirror-shipping] 生产读回失败 → 退回 building + critical 摩擦', async () => {
  const created = await call('/shipping/features', {
    method: 'POST', body: JSON.stringify({ title: 'Production contract' }),
  })
  const featureId = created.json.id as string
  const releaseId = 'sr-readback-failure'
  await pool.query(`UPDATE shipping_features SET status='watching' WHERE id=$1`, [featureId])
  await pool.query(
    `INSERT INTO shipping_releases
      (id,feature_id,environment,status,readback_status,completed_at,readback_due_at)
     VALUES ($1,$2,'production','succeeded','pending',NOW(),NOW()+INTERVAL '24 hours')`,
    [releaseId, featureId],
  )

  const failed = await call(`/shipping/features/${featureId}/releases/${releaseId}/action`, {
    method: 'POST',
    body: JSON.stringify({ action: 'readback_fail', evidence: [{ note: 'Error rate exceeded the release baseline' }] }),
  })
  assert.equal(failed.status, 200)
  assert.equal(failed.json.status, 'building')
  assert.equal(failed.json.releases.find((release: any) => release.id === releaseId).readbackStatus, 'failed')
  const friction = failed.json.frictions.find((item: any) => item.source === 'production-readback')
  assert.ok(friction)
  assert.equal(friction.severity, 'critical')

  // friction 全量列表同可见
  const list = await call('/shipping/friction')
  assert.equal(list.status, 200)
  assert.ok(list.json.some((item: any) => item.id === friction.id))

  // 失格验证自动升摩擦 + 回归资产(verification.failed 双写)
  const verifyFail = await call(`/shipping/features/${featureId}/verifications`, {
    method: 'POST',
    body: JSON.stringify({ title: 'Extra square', ownerId: USER_ID, builderIds: [BUILDER_ID] }),
  })
  const extraId = verifyFail.json.verifications.find((v: any) => v.title === 'Extra square').id
  const failSquare = await call(`/shipping/features/${featureId}/verifications/${extraId}`, {
    method: 'PATCH',
    body: JSON.stringify({ status: 'failed', evidence: [{ note: 'diverged' }] }),
  })
  assert.equal(failSquare.status, 200)
  assert.ok(failSquare.json.regressions.some((rg: any) => rg.kind === 'manual_replay' && rg.status === 'failing'))
  assert.ok(failSquare.json.frictions.some((fr: any) => fr.source === 'verification' && fr.severity === 'high'))
})

test('[mirror-shipping] friction 增量计数 + regression patch', async () => {
  const created = await call('/shipping/features', {
    method: 'POST', body: JSON.stringify({ title: 'Friction host' }),
  })
  const featureId = created.json.id as string
  const fr = await call('/shipping/friction', {
    method: 'POST',
    body: JSON.stringify({ title: 'Slow click', severity: 'low', frequency: 'frequent' }),
  })
  assert.equal(fr.status, 201)
  const frId = fr.json.id
  const bumped = await call(`/shipping/friction/${frId}`, {
    method: 'PATCH',
    body: JSON.stringify({ incrementOccurrence: true, status: 'triaged', featureId }),
  })
  assert.equal(bumped.status, 200)
  assert.equal(bumped.json.ok, true)
  const row = await pool.query(`SELECT occurrence_count, status, feature_id FROM shipping_friction_reports WHERE id=$1`, [frId])
  assert.equal(row.rows[0].occurrence_count, 2) // 建=1 + 增=1
  assert.equal(row.rows[0].status, 'triaged')
  assert.equal(row.rows[0].feature_id, featureId)

  const rg = await call(`/shipping/features/${featureId}/regressions`, {
    method: 'POST',
    body: JSON.stringify({ title: 'Export replay', command: 'cumora export --replay' }),
  })
  assert.equal(rg.status, 201)
  const rgId = rg.json.regressions.find((x: any) => x.title === 'Export replay').id
  const rgp = await call(`/shipping/features/${featureId}/regressions/${rgId}`, {
    method: 'PATCH',
    body: JSON.stringify({ status: 'passing', lastResult: 'ok', lastEvidence: [{ run: 1 }] }),
  })
  assert.equal(rgp.status, 200)
  const patched = rgp.json.regressions.find((x: any) => x.id === rgId)
  assert.equal(patched.status, 'passing')
  assert.equal(patched.lastResult, 'ok')
  assert.ok(patched.lastRunAt)
})
