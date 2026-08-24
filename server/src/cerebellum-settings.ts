/**
 * Cerebellum Route settings accessor (spec #21, ticket #22).
 *
 * The sole reader/writer of the six `app_settings` keys backing Cerebellum
 * Route: `cerebellum_route`, `cerebellum_local_engine`, `cerebellum_provider`,
 * `cerebellum_base_url`, `cerebellum_api_key`, `cerebellum_model`. Mirrors
 * `admin.ts`'s existing `waitlist_enabled`/`signups_paused` get/set pattern —
 * same `app_settings` table, same upsert shape — plus one new concern: the
 * API key is a credential, not a toggle, so it's encrypted at rest
 * (AES-256-GCM, see docs/adr/0001-encrypt-cerebellum-secrets.md) and never
 * read back in plaintext by any caller outside this module.
 *
 * Consumers (`cerebellum-adapter.ts`, `agents/cerebellum-route.ts`,
 * `api/admin-router.ts`) query this module at call time instead of reading
 * `env.CEREBELLUM_*` constants, so an admin edit takes effect on the next
 * call — no restart.
 */
import { createCipheriv, createDecipheriv, createHash, randomBytes } from 'node:crypto'
import { pool } from './db/pool.js'
import { env } from './env.js'

/* ============== Encryption (cerebellum_api_key only) ============== */

// Deliberately NOT importing admin.ts's HttpError here — this module is a
// low-level accessor consumed by cerebellum-adapter.ts/cerebellum-route.ts
// (hot call paths) as well as the admin router, and admin.ts pulls in the
// oauth/email/sub2api import graph. Plain Error keeps this module's own
// dependency footprint small; admin-router.ts's generic 500 fallback is a
// fine outcome for "server is missing a required env var" anyway.
class CerebellumConfigError extends Error {}

// ponytail: passphrase-of-any-length -> fixed 32-byte AES-256 key via a plain
// hash, not a KDF (scrypt/argon2) — CUMORA_SECRETS_KEY is a high-entropy
// deploy-time secret (e.g. `openssl rand -hex 32`), not a human password, so
// there's no brute-force-slowing need. Upgrade to scrypt if that assumption
// ever changes.
function secretsKey(): Buffer {
  if (!env.CUMORA_SECRETS_KEY) {
    throw new CerebellumConfigError('CUMORA_SECRETS_KEY is not configured on the server')
  }
  return createHash('sha256').update(env.CUMORA_SECRETS_KEY).digest()
}

/** `iv.authTag.ciphertext`, each base64 — self-contained so decryption needs
 *  nothing but the master key and this one stored string. */
function encryptApiKey(plaintext: string): string {
  const key = secretsKey()
  const iv = randomBytes(12)
  const cipher = createCipheriv('aes-256-gcm', key, iv)
  const ciphertext = Buffer.concat([cipher.update(plaintext, 'utf8'), cipher.final()])
  const tag = cipher.getAuthTag()
  return `${iv.toString('base64')}.${tag.toString('base64')}.${ciphertext.toString('base64')}`
}

/** Returns `null` on any failure (missing/rotated `CUMORA_SECRETS_KEY`,
 *  corrupt data) rather than throwing — per ADR 0001's documented
 *  consequence, a lost master key just makes the stored value look
 *  unconfigured; the operator re-enters it. */
function decryptApiKey(stored: string): string | null {
  try {
    const [ivB64, tagB64, ctB64] = stored.split('.')
    if (!ivB64 || !tagB64 || !ctB64) return null
    const decipher = createDecipheriv('aes-256-gcm', secretsKey(), Buffer.from(ivB64, 'base64'))
    decipher.setAuthTag(Buffer.from(tagB64, 'base64'))
    const plaintext = Buffer.concat([decipher.update(Buffer.from(ctB64, 'base64')), decipher.final()])
    return plaintext.toString('utf8')
  } catch {
    return null
  }
}

/* ============== Plaintext fields (route/localEngine/provider/baseUrl/model) ============== */

export interface CerebellumRouteSettings {
  route: 'remote' | 'byoa'
  localEngine: string
  provider: string
  baseUrl: string
  model: string
}

const DEFAULTS: CerebellumRouteSettings = {
  route: 'remote',
  localEngine: 'claude',
  provider: '',
  baseUrl: '',
  model: '',
}

const PLAINTEXT_KEYS = [
  'cerebellum_route',
  'cerebellum_local_engine',
  'cerebellum_provider',
  'cerebellum_base_url',
  'cerebellum_model',
] as const

const FIELD_TO_KEY: Record<keyof CerebellumRouteSettings, (typeof PLAINTEXT_KEYS)[number]> = {
  route: 'cerebellum_route',
  localEngine: 'cerebellum_local_engine',
  provider: 'cerebellum_provider',
  baseUrl: 'cerebellum_base_url',
  model: 'cerebellum_model',
}

/** Read the five plaintext fields in one round-trip, filling defaults for
 *  missing rows (mirrors `admin.ts`'s `getSettings`). */
export async function getCerebellumSettings(): Promise<CerebellumRouteSettings> {
  const { rows } = await pool.query<{ key: string; value: unknown }>(
    `SELECT key, value FROM app_settings WHERE key = ANY($1::text[])`,
    [PLAINTEXT_KEYS],
  )
  const map = new Map(rows.map((r) => [r.key, r.value]))
  const route = map.get('cerebellum_route')
  const localEngine = map.get('cerebellum_local_engine')
  const provider = map.get('cerebellum_provider')
  const baseUrl = map.get('cerebellum_base_url')
  const model = map.get('cerebellum_model')
  return {
    route: route === 'byoa' || route === 'remote' ? route : DEFAULTS.route,
    localEngine: typeof localEngine === 'string' && localEngine ? localEngine : DEFAULTS.localEngine,
    provider: typeof provider === 'string' ? provider : DEFAULTS.provider,
    baseUrl: typeof baseUrl === 'string' ? baseUrl.replace(/\/+$/, '') : DEFAULTS.baseUrl,
    model: typeof model === 'string' ? model : DEFAULTS.model,
  }
}

/** Upsert any subset of the five plaintext fields (mirrors `setSetting`). */
export async function setCerebellumSettings(
  updates: Partial<CerebellumRouteSettings>,
  updatedBy: string,
): Promise<void> {
  for (const field of Object.keys(updates) as Array<keyof CerebellumRouteSettings>) {
    const value = updates[field]
    if (value === undefined) continue
    await pool.query(
      `INSERT INTO app_settings (key, value, updated_at, updated_by)
         VALUES ($1, $2::jsonb, NOW(), $3)
       ON CONFLICT (key) DO UPDATE
         SET value = EXCLUDED.value,
             updated_at = NOW(),
             updated_by = EXCLUDED.updated_by`,
      [FIELD_TO_KEY[field], JSON.stringify(value), updatedBy],
    )
  }
}

/* ============== cerebellum_api_key (encrypted) ============== */

export interface CerebellumApiKeyStatus {
  configured: boolean
  suffix: string | null
}

async function readApiKeyPlaintext(): Promise<string | null> {
  const { rows } = await pool.query<{ value: unknown }>(
    `SELECT value FROM app_settings WHERE key = 'cerebellum_api_key' LIMIT 1`,
  )
  const stored = rows[0]?.value
  if (typeof stored !== 'string' || !stored) return null
  return decryptApiKey(stored)
}

/** The only API-key read shape any admin-facing caller may use — never the
 *  decrypted value (ADR 0001 / issue #22 acceptance criteria). */
export async function getCerebellumApiKeyStatus(): Promise<CerebellumApiKeyStatus> {
  const plaintext = await readApiKeyPlaintext()
  if (!plaintext) return { configured: false, suffix: null }
  return { configured: true, suffix: plaintext.slice(-4) }
}

/** Internal-only decrypted read for building the outbound HTTP client in
 *  `cerebellum-adapter.ts`. Never route this through the admin API. */
export async function getCerebellumApiKeyForClient(): Promise<string> {
  return (await readApiKeyPlaintext()) ?? ''
}

/** Encrypt-and-overwrite. An empty string explicitly clears the stored key
 *  (the PUT /settings handler only calls this when the field was present in
 *  the request body — omitting the field entirely leaves the key untouched,
 *  per the API-key write contract in issue #22). */
export async function setCerebellumApiKey(plaintext: string, updatedBy: string): Promise<void> {
  if (!plaintext) {
    await pool.query(`DELETE FROM app_settings WHERE key = 'cerebellum_api_key'`)
    return
  }
  const encrypted = encryptApiKey(plaintext)
  await pool.query(
    `INSERT INTO app_settings (key, value, updated_at, updated_by)
       VALUES ('cerebellum_api_key', $1::jsonb, NOW(), $2)
     ON CONFLICT (key) DO UPDATE
       SET value = EXCLUDED.value,
           updated_at = NOW(),
           updated_by = EXCLUDED.updated_by`,
    [JSON.stringify(encrypted), updatedBy],
  )
}

/* ============== Startup migration: .env -> app_settings ============== */

/** One-time carry-over for deployments upgrading from `.env`-only Cerebellum
 *  Route config. For each of the six keys: if `app_settings` has no row yet
 *  and the corresponding env var is set, seed it. `ON CONFLICT DO NOTHING`
 *  makes this naturally idempotent — a value already set (via the UI or a
 *  prior boot's migration) is never overwritten, no pre-read needed. */
export async function migrateCerebellumSettingsFromEnv(): Promise<void> {
  const plaintextSeeds: Array<[(typeof PLAINTEXT_KEYS)[number], string]> = [
    ['cerebellum_route', env.CEREBELLUM_ROUTE],
    ['cerebellum_local_engine', env.CEREBELLUM_LOCAL_ENGINE],
    ['cerebellum_provider', env.CEREBELLUM_PROVIDER],
    ['cerebellum_base_url', env.CEREBELLUM_BASE_URL],
    ['cerebellum_model', env.CEREBELLUM_MODEL],
  ].filter(([, value]) => Boolean(value)) as Array<[(typeof PLAINTEXT_KEYS)[number], string]>

  for (const [key, value] of plaintextSeeds) {
    await pool.query(
      `INSERT INTO app_settings (key, value, updated_at, updated_by)
         VALUES ($1, $2::jsonb, NOW(), 'env-migration')
       ON CONFLICT (key) DO NOTHING`,
      [key, JSON.stringify(value)],
    )
  }

  if (env.CEREBELLUM_API_KEY) {
    if (!env.CUMORA_SECRETS_KEY) {
      console.warn(
        '[cerebellum-settings] CEREBELLUM_API_KEY is set but CUMORA_SECRETS_KEY is missing; ' +
        'skipping the one-time migration of the API key into app_settings',
      )
    } else {
      await pool.query(
        `INSERT INTO app_settings (key, value, updated_at, updated_by)
           VALUES ('cerebellum_api_key', $1::jsonb, NOW(), 'env-migration')
         ON CONFLICT (key) DO NOTHING`,
        [JSON.stringify(encryptApiKey(env.CEREBELLUM_API_KEY))],
      )
    }
  }
}

/* ============== Engine availability (future UI dropdown) ============== */

/** Union of `available_engines` across every currently-online, non-revoked
 *  Computer. Empty array when none are online. Feeds the admin UI's
 *  local-engine dropdown (#23) — not consumed here. */
export async function onlineComputerAvailableEngines(): Promise<string[]> {
  const { rows } = await pool.query<{ available_engines: string[] }>(
    `SELECT available_engines FROM computers WHERE status = 'online' AND revoked_at IS NULL`,
  )
  const union = new Set<string>()
  for (const row of rows) {
    for (const engine of row.available_engines ?? []) union.add(engine)
  }
  return Array.from(union).sort()
}
