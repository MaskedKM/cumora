/**
 * Unit tests for the Cerebellum Route settings accessor (ticket #22).
 *
 * `pool.query` is mocked with a tiny in-memory `app_settings`/`computers`
 * simulator (same style as agent-computer-pairing-token.test.ts) so these
 * tests exercise the real INSERT ... ON CONFLICT semantics the module
 * relies on for idempotent migration, without needing a live Postgres.
 *
 * Run: node --import tsx --test server/src/__tests__/cerebellum-settings.test.ts
 */
import { afterEach, beforeEach, test } from 'node:test'
import assert from 'node:assert/strict'

process.env.CUMORA_SECRETS_KEY = 'unit-test-master-key-do-not-use-in-prod'

const { pool } = await import('../db/pool.js')
const { env } = await import('../env.js')
const settings = await import('../cerebellum-settings.js')

const originalQuery = pool.query.bind(pool)

interface Row { key: string; value: unknown }

/** In-memory stand-in for the `app_settings` table (keyed by `key`) plus a
 *  `computers` list, wired up to the exact query shapes cerebellum-
 *  settings.ts issues. */
let appSettings: Map<string, unknown>
let computers: Array<{ status: string; available_engines: string[]; revoked_at: string | null }>

function installFakePool() {
  ;(pool as unknown as { query: typeof originalQuery }).query = (async (sql: string, params: unknown[] = []) => {
    const q = sql.replace(/\s+/g, ' ').trim()

    if (q.startsWith('SELECT key, value FROM app_settings WHERE key = ANY')) {
      const keys = params[0] as string[]
      const rows: Row[] = keys.filter((k) => appSettings.has(k)).map((k) => ({ key: k, value: appSettings.get(k) }))
      return { rows, rowCount: rows.length }
    }
    if (q.startsWith("SELECT value FROM app_settings WHERE key = 'cerebellum_api_key'")) {
      const has = appSettings.has('cerebellum_api_key')
      return { rows: has ? [{ value: appSettings.get('cerebellum_api_key') }] : [], rowCount: has ? 1 : 0 }
    }
    // NOTE: check the cerebellum_api_key-literal shape BEFORE the generic
    // $1/$2/$3 shape — both start with the same prefix and both contain
    // "ON CONFLICT (key) DO UPDATE", so order matters here.
    if (q.startsWith("INSERT INTO app_settings (key, value, updated_at, updated_by) VALUES ('cerebellum_api_key'") && q.includes('DO UPDATE')) {
      appSettings.set('cerebellum_api_key', JSON.parse(params[0] as string))
      return { rows: [], rowCount: 1 }
    }
    if (q.startsWith('INSERT INTO app_settings') && q.includes('ON CONFLICT (key) DO UPDATE')) {
      const key = params[0] as string
      const value = JSON.parse(params[1] as string)
      appSettings.set(key, value)
      return { rows: [], rowCount: 1 }
    }
    if (q.startsWith('INSERT INTO app_settings') && q.includes('ON CONFLICT (key) DO NOTHING')) {
      // Two shapes: plaintext seed (key, value params) and api-key seed
      // (value only, key literal in the SQL).
      let key: string
      let value: unknown
      if (q.includes("'cerebellum_api_key'")) {
        key = 'cerebellum_api_key'
        value = JSON.parse(params[0] as string)
      } else {
        key = params[0] as string
        value = JSON.parse(params[1] as string)
      }
      if (!appSettings.has(key)) appSettings.set(key, value)
      return { rows: [], rowCount: 1 }
    }
    if (q.startsWith("DELETE FROM app_settings WHERE key = 'cerebellum_api_key'")) {
      appSettings.delete('cerebellum_api_key')
      return { rows: [], rowCount: 1 }
    }
    if (q.startsWith("SELECT available_engines FROM computers WHERE status = 'online'")) {
      const rows = computers.filter((c) => c.status === 'online' && c.revoked_at === null)
        .map((c) => ({ available_engines: c.available_engines }))
      return { rows, rowCount: rows.length }
    }
    throw new Error(`unexpected query: ${q}`)
  }) as typeof originalQuery
}

beforeEach(() => {
  appSettings = new Map()
  computers = []
  installFakePool()
})

afterEach(() => {
  ;(pool as unknown as { query: typeof originalQuery }).query = originalQuery
})

/* ============== encrypt/decrypt round-trip + masked suffix ============== */

test('setCerebellumApiKey encrypts at rest; getCerebellumApiKeyStatus never returns the plaintext', async () => {
  await settings.setCerebellumApiKey('sk-super-secret-12345', 'admin-1')

  const stored = appSettings.get('cerebellum_api_key')
  assert.equal(typeof stored, 'string')
  assert.ok(!(stored as string).includes('sk-super-secret-12345'), 'ciphertext must not contain the plaintext')

  const status = await settings.getCerebellumApiKeyStatus()
  assert.deepEqual(status, { configured: true, suffix: '2345' })
})

test('getCerebellumApiKeyForClient round-trips the exact plaintext (internal-only reader)', async () => {
  await settings.setCerebellumApiKey('sk-round-trip-key', 'admin-1')
  const plaintext = await settings.getCerebellumApiKeyForClient()
  assert.equal(plaintext, 'sk-round-trip-key')
})

test('an unconfigured key reports configured:false with no suffix', async () => {
  const status = await settings.getCerebellumApiKeyStatus()
  assert.deepEqual(status, { configured: false, suffix: null })
})

test('setCerebellumApiKey("") clears a previously-configured key', async () => {
  await settings.setCerebellumApiKey('sk-to-be-cleared', 'admin-1')
  assert.equal((await settings.getCerebellumApiKeyStatus()).configured, true)
  await settings.setCerebellumApiKey('', 'admin-1')
  assert.deepEqual(await settings.getCerebellumApiKeyStatus(), { configured: false, suffix: null })
})

test('a key encrypted under one CUMORA_SECRETS_KEY reads back as unconfigured under another (no throw)', async () => {
  await settings.setCerebellumApiKey('sk-rotate-me', 'admin-1')
  const original = env.CUMORA_SECRETS_KEY
  env.CUMORA_SECRETS_KEY = 'a-completely-different-master-key'
  try {
    assert.deepEqual(await settings.getCerebellumApiKeyStatus(), { configured: false, suffix: null })
    assert.equal(await settings.getCerebellumApiKeyForClient(), '')
  } finally {
    env.CUMORA_SECRETS_KEY = original
  }
})

/* ============== plaintext fields ============== */

test('getCerebellumSettings fills documented defaults when no rows exist', async () => {
  assert.deepEqual(await settings.getCerebellumSettings(), {
    route: 'remote',
    localEngine: 'claude',
    provider: '',
    baseUrl: '',
    model: '',
  })
})

test('setCerebellumSettings upserts only the provided fields; others keep their stored value', async () => {
  await settings.setCerebellumSettings({ route: 'byoa', provider: 'deepseek' }, 'admin-1')
  const s = await settings.getCerebellumSettings()
  assert.equal(s.route, 'byoa')
  assert.equal(s.provider, 'deepseek')
  assert.equal(s.localEngine, 'claude') // untouched default

  await settings.setCerebellumSettings({ localEngine: 'codex' }, 'admin-1')
  const s2 = await settings.getCerebellumSettings()
  assert.equal(s2.route, 'byoa', 'previously-set fields survive an unrelated update')
  assert.equal(s2.localEngine, 'codex')
})

/* ============== migration idempotency ============== */

test('migration seeds app_settings from env when a key is absent', async () => {
  env.CEREBELLUM_PROVIDER = 'novita'
  env.CEREBELLUM_BASE_URL = 'https://api.novita.ai/openai'
  env.CEREBELLUM_MODEL = 'deepseek-v4'
  try {
    await settings.migrateCerebellumSettingsFromEnv()
    const s = await settings.getCerebellumSettings()
    assert.equal(s.provider, 'novita')
    assert.equal(s.baseUrl, 'https://api.novita.ai/openai')
    assert.equal(s.model, 'deepseek-v4')
    // Always-truthy env defaults (route/localEngine) seed too.
    assert.equal(s.route, env.CEREBELLUM_ROUTE)
    assert.equal(s.localEngine, env.CEREBELLUM_LOCAL_ENGINE)
  } finally {
    env.CEREBELLUM_PROVIDER = ''
    env.CEREBELLUM_BASE_URL = ''
    env.CEREBELLUM_MODEL = ''
  }
})

test('migration never overwrites a value already present in app_settings', async () => {
  await settings.setCerebellumSettings({ provider: 'operator-set-me' }, 'admin-1')
  env.CEREBELLUM_PROVIDER = 'should-not-win'
  try {
    await settings.migrateCerebellumSettingsFromEnv()
    const s = await settings.getCerebellumSettings()
    assert.equal(s.provider, 'operator-set-me')
  } finally {
    env.CEREBELLUM_PROVIDER = ''
  }
})

test('migration seeds the encrypted API key from CEREBELLUM_API_KEY when absent', async () => {
  env.CEREBELLUM_API_KEY = 'sk-from-dotenv'
  try {
    await settings.migrateCerebellumSettingsFromEnv()
    assert.deepEqual(await settings.getCerebellumApiKeyStatus(), { configured: true, suffix: 'tenv' })
    assert.equal(await settings.getCerebellumApiKeyForClient(), 'sk-from-dotenv')
  } finally {
    env.CEREBELLUM_API_KEY = ''
  }
})

test('migration never overwrites an already-configured API key', async () => {
  await settings.setCerebellumApiKey('sk-operator-set', 'admin-1')
  env.CEREBELLUM_API_KEY = 'sk-from-dotenv-should-not-win'
  try {
    await settings.migrateCerebellumSettingsFromEnv()
    assert.equal(await settings.getCerebellumApiKeyForClient(), 'sk-operator-set')
  } finally {
    env.CEREBELLUM_API_KEY = ''
  }
})

/* ============== engine-availability union ============== */

test('onlineComputerAvailableEngines unions distinct engines across multiple online Computers', async () => {
  computers = [
    { status: 'online', available_engines: ['claude', 'codex'], revoked_at: null },
    { status: 'online', available_engines: ['codex', 'grok'], revoked_at: null },
  ]
  assert.deepEqual(await settings.onlineComputerAvailableEngines(), ['claude', 'codex', 'grok'])
})

test('onlineComputerAvailableEngines returns an empty array when no Computer is online', async () => {
  computers = [
    { status: 'offline', available_engines: ['claude'], revoked_at: null },
    { status: 'busy', available_engines: ['codex'], revoked_at: null },
  ]
  assert.deepEqual(await settings.onlineComputerAvailableEngines(), [])
})

test('onlineComputerAvailableEngines excludes an offline Computer from the union', async () => {
  computers = [
    { status: 'online', available_engines: ['claude'], revoked_at: null },
    { status: 'offline', available_engines: ['cursor'], revoked_at: null },
  ]
  assert.deepEqual(await settings.onlineComputerAvailableEngines(), ['claude'])
})
