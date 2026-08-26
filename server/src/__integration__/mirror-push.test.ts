/**
 * 验收镜像 · 推送域(#59):/push/register /push/unregister。
 * 双跑:CUMORA_MIRROR_BASE 指向 Go 候选。发送面(APNs/FCM)凭据缺失
 * 时软关停为 no-op——路由与 upsert 语义为本套件重心;真机投递冒烟在
 * 评审/owner 侧按凭据环境执行。
 */

import assert from 'node:assert/strict'
import { after, beforeEach, test } from 'node:test'
import { pool } from '../db/pool.js'
import {
  ensureSchemaOnce, resetAllTables, seedUserMembership, startMirror,teardownAll, 
} from './_helpers.js'

const USER = 'u-mirror-push'
const OTHER = 'u-mirror-push-2'
const COMPANY = 'c-mirror-push'

await ensureSchemaOnce()
const mirror = startMirror(USER, COMPANY)
const call = mirror.call

beforeEach(async () => {
  await resetAllTables()
  await seedCompanyAndUsers()
})

async function seedCompanyAndUsers(): Promise<void> {
  await pool.query(
    `INSERT INTO companies (id, name, slug, owner_user_id) VALUES ($1, 'Mirror Push Co', $2, $3)`,
    [COMPANY, COMPANY.replace(/[^a-z0-9]/g, '-'), USER],
  )
  await seedUserMembership(USER, COMPANY)
  await seedUserMembership(OTHER, COMPANY)
}

after(async () => { await mirror.close(); await teardownAll() })

test('[mirror] push register validation', async () => {
  assert.equal((await call('/push/register', { method: 'POST', body: JSON.stringify({ platform: 'win32', token: 't' }) })).status, 400)
  const missing = await call('/push/register', { method: 'POST', body: JSON.stringify({ platform: 'ios' }) })
  assert.equal(missing.status, 400)
  assert.equal(missing.json.error, 'token required')
  const tooLong = await call('/push/register', {
    method: 'POST', body: JSON.stringify({ platform: 'ios', token: 'x'.repeat(1025) }),
  })
  assert.equal(tooLong.status, 400)
  assert.equal(tooLong.json.error, 'token too long')
})

test('[mirror] push register upsert: token trim + caps + COALESCE metadata', async () => {
  const r = await call('/push/register', {
    method: 'POST',
    body: JSON.stringify({ platform: 'ios', token: `  tok-1  `, appVersion: '1.2.3', deviceModel: 'iPhone 16' }),
  })
  assert.equal(r.status, 200)
  assert.deepEqual(r.json, { ok: true })
  const row = await pool.query(`SELECT user_id, platform, token, app_version, device_model, disabled_at FROM push_devices`)
  assert.equal(row.rows.length, 1)
  assert.equal(row.rows[0].user_id, USER)
  assert.equal(row.rows[0].token, 'tok-1')
  assert.equal(row.rows[0].app_version, '1.2.3')
  assert.equal(row.rows[0].device_model, 'iPhone 16')
  assert.equal(row.rows[0].disabled_at, null)

  // 再注册:不传 app_version/device_model → COALESCE 保留旧值;token 原值覆盖
  const again = await call('/push/register', {
    method: 'POST', body: JSON.stringify({ platform: 'ios', token: 'tok-1' }),
  })
  assert.equal(again.status, 200)
  const rows2 = await pool.query(`SELECT app_version, device_model FROM push_devices`)
  assert.equal(rows2.rows[0].app_version, '1.2.3')
  assert.equal(rows2.rows[0].device_model, 'iPhone 16')
})

test('[mirror] push register rebinds user and clears disabled', async () => {
  await call('/push/register', { method: 'POST', body: JSON.stringify({ platform: 'android', token: 'tok-a' }) })
  await pool.query(`UPDATE push_devices SET disabled_at = NOW() WHERE token = 'tok-a'`)
  const mirror2 = startMirror(OTHER, COMPANY)
  try {
    const rebind = await mirror2.call('/push/register', {
      method: 'POST', body: JSON.stringify({ platform: 'android', token: 'tok-a' }),
    })
    assert.equal(rebind.status, 200)
    const row = await pool.query(`SELECT user_id, disabled_at FROM push_devices WHERE token = 'tok-a'`)
    assert.equal(row.rows[0].user_id, OTHER)
    assert.equal(row.rows[0].disabled_at, null)
  } finally {
    await mirror2.close()
  }
})

test('[mirror] push unregister: soft-disable scoped by user', async () => {
  assert.equal((await call('/push/unregister', { method: 'POST', body: JSON.stringify({}) })).status, 400)
  await call('/push/register', { method: 'POST', body: JSON.stringify({ platform: 'web', token: 'tok-w' }) })
  // 他人注销不掉我的设备
  const mirror2 = startMirror(OTHER, COMPANY)
  try {
    const foreign = await mirror2.call('/push/unregister', {
      method: 'POST', body: JSON.stringify({ token: 'tok-w' }),
    })
    assert.equal(foreign.status, 200)
    const row = await pool.query(`SELECT disabled_at FROM push_devices WHERE token = 'tok-w'`)
    assert.equal(row.rows[0].disabled_at, null)
  } finally {
    await mirror2.close()
  }
  const mine = await call('/push/unregister', { method: 'POST', body: JSON.stringify({ token: ' tok-w ' }) })
  assert.equal(mine.status, 200)
  const row = await pool.query(`SELECT disabled_at FROM push_devices WHERE token = 'tok-w'`)
  assert.ok(row.rows[0].disabled_at !== null)
})
