/**
 * 验收镜像 · GET /api/og 门面(#122,#117-b):链接预览代理的输入校验与
 * SSRF 拒绝。解析/实体/相对图路径的正命中率在 Go 单测(og_test.go,经
 * httptest)——镜像面无法喂正样本:mock 服必在 localhost,恰是 SSRF
 * 面要拒的(这不是缺口,是安全面的自证)。
 */
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { teardownAll, MIRROR_BASE } from './_helpers.js'

const baseUrl = MIRROR_BASE

before(async () => {
  if (!MIRROR_BASE) throw new Error('CUMORA_MIRROR_BASE not set — run via npm run test:integration')
})

after(async () => {
  await teardownAll()
})

async function getOg(url?: string): Promise<{ status: number; json: any }> {
  const q = url === undefined ? '' : `?url=${encodeURIComponent(url)}`
  const res = await fetch(`${baseUrl}/api/og${q}`, {
    headers: { 'x-test-user': 'u-og-gate' },
  })
  return { status: res.status, json: await res.json().catch(() => null) }
}

test('[mirror-og] missing url → 400', async () => {
  const r = await getOg()
  assert.equal(r.status, 400)
  assert.equal(r.json.error, 'url required')
})

test('[mirror-og] invalid url → 400', async () => {
  assert.equal((await getOg('not a url')).status, 400)
})

test('[mirror-og] non-http scheme → 400', async () => {
  const r = await getOg('ftp://example.com/x')
  assert.equal(r.status, 400)
  assert.equal(r.json.error, 'only http(s) urls are supported')
})

test('[mirror-og] SSRF: localhost / .local / private IP literals → 403', async () => {
  for (const blocked of ['http://localhost:5180/', 'http://foo.localhost/x', 'http://printer.local/', 'http://127.0.0.1:8080/', 'http://10.0.0.5/', 'http://192.168.1.4/admin']) {
    const r = await getOg(blocked)
    assert.equal(r.status, 403, `${blocked} should be blocked`)
    assert.equal(r.json.error, 'blocked private host')
  }
})

test('[mirror-og] unresolvable host → 400 dns lookup failed', async () => {
  const r = await getOg('http://this-domain-really-does-not-exist-xyz.invalid/')
  assert.equal(r.status, 400)
  assert.equal(r.json.error, 'dns lookup failed')
})
