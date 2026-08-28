#!/usr/bin/env node
/**
 * Integration test runner.
 *
 * The integration suite needs a REAL Postgres (we test SKIP LOCKED + jsonb
 * + indexes that pg-mem doesn't reproduce faithfully) and a REAL Redis
 * (persistEmailMessage publishes the wake event; we don't want to monkey-
 * patch that path because it's prod-critical).
 *
 * To run:
 *   1. Create a DEDICATED test DB (the suite TRUNCATEs everything, so
 *      pointing at your dev DB will eat its data):
 *        createdb cumora_test
 *   2. Have Redis listening on REDIS_URL (default localhost:6379).
 *   3. Export INTEGRATION_DATABASE_URL pointing at the test DB:
 *        export INTEGRATION_DATABASE_URL=postgres://$USER@localhost:5432/cumora_test
 *   4. npm run test:integration
 *
 * If INTEGRATION_DATABASE_URL is unset we print a one-line "skipped" and
 * exit 0 — so this script slots into CI / pre-commit hooks without
 * forcing every developer to maintain a test DB.
 */
import { spawn } from 'node:child_process'
import { readdirSync } from 'node:fs'
import { basename, dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
// Load the developer's .env so RESEND_API_KEY / EMAIL_DOMAIN /
// INTEGRATION_DATABASE_URL set there are visible to our gating checks
// before we spawn the test child. The test child also imports
// dotenv/config via env.ts, so this is mostly belt-and-braces — the
// gating checks below need them present in THIS process.
import 'dotenv/config'

const INTEGRATION_URL = process.env.INTEGRATION_DATABASE_URL
if (!INTEGRATION_URL) {
  console.log('[integration] skipped — set INTEGRATION_DATABASE_URL to enable.')
  console.log('             example: INTEGRATION_DATABASE_URL=postgres://$USER@localhost:5432/cumora_test \\\\')
  console.log('                      npm run test:integration')
  process.exit(0)
}

// Belt-and-braces safety: refuse to run when INTEGRATION_DATABASE_URL
// looks like a production-ish DB name. The suite TRUNCATEs every table,
// so a mis-set var would silently nuke real data.
const SUSPICIOUS = /\b(prod|production|main|live)\b/i
if (SUSPICIOUS.test(INTEGRATION_URL)) {
  console.error(`[integration] refusing to run — INTEGRATION_DATABASE_URL looks production-flavored: ${INTEGRATION_URL}`)
  console.error('              The suite TRUNCATEs every table. Point at a dedicated test DB (e.g. cumora_test).')
  process.exit(2)
}

// Swap DATABASE_URL so the env module (server/src/env.ts) picks up the
// test target when it's imported by the test harness.
process.env.DATABASE_URL = INTEGRATION_URL

// By default we force-empty RESEND_API_KEY so the suite never hits live
// Resend — a developer's real key in .env would otherwise reject because
// the test EMAIL_DOMAIN isn't verified. RESEND_LIVE_TEST=1 OPTS IN to
// the live path: keep the real key + real EMAIL_DOMAIN, and the
// resend-live.test.ts spec runs against Resend's magic test addresses
// (delivered@resend.dev / bounced@resend.dev — these consume no quota).
const LIVE_RESEND = process.env.RESEND_LIVE_TEST === '1'
if (!LIVE_RESEND) {
  process.env.RESEND_API_KEY = ''
  if (!process.env.EMAIL_DOMAIN) process.env.EMAIL_DOMAIN = 'cumora.local'
} else {
  // Live mode: refuse to run if the key OR a verified domain isn't set.
  if (!process.env.RESEND_API_KEY) {
    console.error('[integration] RESEND_LIVE_TEST=1 but RESEND_API_KEY is empty.')
    process.exit(2)
  }
  if (!process.env.EMAIL_DOMAIN) {
    console.error('[integration] RESEND_LIVE_TEST=1 but EMAIL_DOMAIN is empty — Resend will reject the From line.')
    process.exit(2)
  }
  console.log(`[integration] LIVE RESEND mode · domain=${process.env.EMAIL_DOMAIN}`)
}

if (!process.env.EMAIL_INBOUND_HMAC_SECRET) process.env.EMAIL_INBOUND_HMAC_SECRET = 'integration-test-secret'

// Forward to node --import tsx --test against the integration suite.
// tsx handles TypeScript; node:test handles the test runner.
const here = dirname(fileURLToPath(import.meta.url))
const dir = join(here, 'src/__integration__')

// Sharding (#115): INTEGRATION_SHARD="N/M" runs only the files whose
// sorted index mod M equals N-1 — a deterministic even split, stable
// across runs. Each shard still serializes its own test FILES
// (--test-concurrency=1: concurrent TRUNCATEs against a shared DB
// deadlock at the catalog-lock level); CI removes the shared-DB
// constraint by giving every shard its own Postgres database AND its
// own Redis instance — pub/sub is instance-wide, so separate Redis DB
// indexes would still cross-talk between shards.
const shardRaw = process.env.INTEGRATION_SHARD ?? '1/1'
const shardMatch = /^([1-9]\d*)\/([1-9]\d*)$/.exec(shardRaw)
if (!shardMatch || Number(shardMatch[1]) > Number(shardMatch[2])) {
  console.error(`[integration] bad INTEGRATION_SHARD: ${shardRaw} (expected "N/M" with 1 ≤ N ≤ M)`)
  process.exit(2)
}
const [shardN, shardM] = [Number(shardMatch[1]), Number(shardMatch[2])]
const files = readdirSync(dir)
  .filter((f) => f.endsWith('.test.ts'))
  .sort()
  .filter((_, i) => i % shardM === shardN - 1)
  .map((f) => join(dir, f))
if (files.length === 0) {
  console.error(`[integration] shard ${shardN}/${shardM} got zero files — refusing to run (miscount?)`)
  process.exit(2)
}
console.log(`[integration] shard ${shardN}/${shardM}: ${files.length} file(s) — ${files.map((f) => basename(f)).join(', ')}`)

const child = spawn(
  'node',
  ['--import', 'tsx', '--test', '--test-concurrency=1', ...files],
  { stdio: 'inherit', env: process.env },
)
child.on('exit', (code) => process.exit(code ?? 1))
