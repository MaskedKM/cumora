/**
 * 验收镜像 · POST /api/auth/apple/native(#123,#117-c):iOS 原生 SIWA
 * 的服务端全链。JWKS 桩在本进程内起(定端口 18995,与 mirror-oauth 的
 * :18994 同款约定):测试现场生成 RSA 钥匙对、自签 identity_token,
 * Go 服走服务器 env 的 CUMORA_APPLE_JWKS_URL 指向本桩。验签算法的
 * 负样本矩阵(错 iss/aud/exp/签名)在 Go 单测 apple_test.go;镜像面
 * 钉的是 HTTP 语义与 find-or-create 三路 + 错误三态。
 */
import { test, beforeEach, after } from 'node:test'
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { generateKeyPairSync, createSign } from 'node:crypto'
import { pool } from './harness/db/pool.js'
import { ensureSchemaOnce, resetAllTables, teardownAll, MIRROR_BASE } from './_helpers.js'

const STUB_PORT = 18995
const AUD = 'io.cumora.app'
const ISS = 'https://appleid.apple.com'
const KID = 'test-kid-1'

const { publicKey, privateKey } = generateKeyPairSync('rsa', { modulusLength: 2048 })
const jwkPub = publicKey.export({ format: 'jwk' }) as { n: string; e: string }

function b64url(s: string): string {
  return Buffer.from(s, 'utf8').toString('base64url')
}

function mintToken(claims: Record<string, unknown>): string {
  const header = b64url(JSON.stringify({ alg: 'RS256', kid: KID }))
  const payload = b64url(JSON.stringify({
    iss: ISS, aud: AUD, sub: 'sub-apple-1',
    exp: Math.floor(Date.now() / 1000) + 600,
    ...claims,
  }))
  const signer = createSign('RSA-SHA256')
  signer.update(`${header}.${payload}`)
  signer.end()
  return `${header}.${payload}.${signer.sign(privateKey).toString('base64url')}`
}

let stubSrv: ReturnType<typeof createServer> | null = null

async function postNative(body: unknown): Promise<{ status: number; json: any }> {
  const res = await fetch(`${MIRROR_BASE}/api/auth/apple/native`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

async function findIdentityRow(providerId: string): Promise<any> {
  const r = await pool.query(
    `SELECT * FROM user_identities WHERE provider = 'apple' AND provider_id = $1`,
    [providerId],
  )
  return r.rows[0] ?? null
}

await ensureSchemaOnce()

// JWKS 桩:整个文件一个(Go 服侧 24h 缓存,桩常驻才对得上)。
await new Promise<void>((resolve, reject) => {
  stubSrv = createServer((req, res) => {
    if (req.url?.includes('/keys')) {
      res.setHeader('content-type', 'application/json')
      res.end(JSON.stringify({ keys: [{ kty: 'RSA', kid: KID, use: 'sig', alg: 'RS256', n: jwkPub.n, e: jwkPub.e }] }))
      return
    }
    res.statusCode = 404
    res.end()
  })
  stubSrv.once('error', reject)
  stubSrv.listen(STUB_PORT, '127.0.0.1', () => resolve())
})

beforeEach(async () => {
  await resetAllTables()
})

after(async () => {
  await new Promise<void>((resolve) => stubSrv?.close(() => resolve()))
  await teardownAll()
})

test('[mirror-apple] 新用户全链:验签 → 建号建区 → 会话 token 可用', async () => {
  const r = await postNative({
    identityToken: mintToken({ email: 'Apple.New@Example.com', email_verified: 'true' }),
    name: 'Apple 新客',
  })
  assert.equal(r.status, 200)
  assert.ok(typeof r.json.token === 'string' && r.json.token.length > 0)
  assert.equal(r.json.user.email, 'apple.new@example.com')
  assert.equal(r.json.user.displayName, 'Apple 新客')
  assert.ok(r.json.companyId, '新用户自动建个人区')

  // DB 侧:users + apple 身份行 + owner 成员
  const ident = await findIdentityRow('sub-apple-1')
  assert.ok(ident, 'user_identities apple 行落库')
  assert.equal(ident.email_lower, 'apple.new@example.com')
  const member = await pool.query(
    `SELECT role FROM company_members WHERE user_id = $1 AND company_id = $2`,
    [ident.user_id, r.json.companyId],
  )
  assert.equal(member.rows[0].role, 'owner')

  // 会话 token 真可用(非 fake-auth 头,纯 Bearer)
  const me = await fetch(`${MIRROR_BASE}/api/auth/me`, {
    headers: { authorization: `Bearer ${r.json.token}` },
  })
  assert.equal(me.status, 200)
  assert.equal((await me.json() as any).user.id, ident.user_id)
})

test('[mirror-apple] 回头客:token 不带 email,靠已链 sub 解析', async () => {
  const first = await postNative({
    identityToken: mintToken({ email: 'returning@example.com', email_verified: 'true' }),
  })
  assert.equal(first.status, 200)
  const second = await postNative({
    identityToken: mintToken({ email: undefined, email_verified: undefined, sub: 'sub-apple-1' }),
  })
  assert.equal(second.status, 200)
  assert.equal(second.json.user.id, first.json.user.id)
  assert.equal(second.json.user.email, 'returning@example.com')
})

test('[mirror-apple] 未链 sub + 未验证 email → 400', async () => {
  const r = await postNative({
    identityToken: mintToken({ sub: 'sub-unlinked-1', email: 'x@example.com', email_verified: 'false' }),
  })
  assert.equal(r.status, 400)
  assert.match(r.json.error, /verified email not available/)
})

test('[mirror-apple] 跨链:同 email 已有账号 → 绑 apple 身份不建新号', async () => {
  await pool.query(
    `INSERT INTO users (id, email, display_name, email_verified_at, tier)
     VALUES ('u-existing-1', 'existing@example.com', 'Existing', NOW(), 'free')`,
  )
  const r = await postNative({
    identityToken: mintToken({ sub: 'sub-crosslink-1', email: 'Existing@example.com', email_verified: 'true' }),
  })
  assert.equal(r.status, 200)
  assert.equal(r.json.user.id, 'u-existing-1')
  assert.ok(await findIdentityRow('sub-crosslink-1'))
})

test('[mirror-apple] 等待名单开门 → 403 waitlisted', async () => {
  await pool.query(
    `INSERT INTO app_settings (key, value) VALUES ('waitlist_enabled', 'true')`,
  )
  const r = await postNative({
    identityToken: mintToken({ sub: 'sub-wl-1', email: 'waitlisted@example.com', email_verified: 'true' }),
  })
  assert.equal(r.status, 403)
  assert.equal(r.json.error, 'waitlisted')
  assert.equal(r.json.email, 'waitlisted@example.com')
  const wl = await pool.query(`SELECT * FROM waitlist WHERE email = $1`, ['waitlisted@example.com'])
  assert.ok(wl.rows[0], '入列落库')
})

test('[mirror-apple] 停用账号 → 403 suspended', async () => {
  const first = await postNative({
    identityToken: mintToken({ sub: 'sub-susp-1', email: 'suspended@example.com', email_verified: 'true' }),
  })
  assert.equal(first.status, 200)
  await pool.query(
    `UPDATE users SET suspended_at = NOW(), suspension_reason = 'test ban' WHERE id = $1`,
    [first.json.user.id],
  )
  const r = await postNative({ identityToken: mintToken({ sub: 'sub-susp-1' }) })
  assert.equal(r.status, 403)
  assert.equal(r.json.error, 'suspended')
  assert.equal(r.json.reason, 'test ban')
})

test('[mirror-apple] 坏 token / 缺 identityToken → 400', async () => {
  assert.equal((await postNative({ identityToken: 'garbage.not-a-jwt' })).status, 400)
  const missing = await postNative({ name: 'No Token' })
  assert.equal(missing.status, 400)
  assert.equal(missing.json.error, 'identityToken required')
})

test('[mirror-apple] 签名不匹配(桩只公布正钥)→ 400 bad signature', async () => {
  const { privateKey: otherPriv } = generateKeyPairSync('rsa', { modulusLength: 2048 })
  const header = b64url(JSON.stringify({ alg: 'RS256', kid: KID }))
  const payload = b64url(JSON.stringify({ iss: ISS, aud: AUD, sub: 'sub-evil', exp: Math.floor(Date.now() / 1000) + 600 }))
  const signer = createSign('RSA-SHA256')
  signer.update(`${header}.${payload}`)
  signer.end()
  const forged = `${header}.${payload}.${signer.sign(otherPriv).toString('base64url')}`
  const r = await postNative({ identityToken: forged })
  assert.equal(r.status, 400)
  assert.equal(r.json.error, 'apple identity_token bad signature')
})
