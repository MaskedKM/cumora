/**
 * Unit tests for the low-priority wake budget — the post-incident
 * (FUSE-cap, 2026-05-20) backpressure that caps idle/scanner-driven
 * agent spawns at 20/min per cumora-server process.
 *
 * Run: node --import tsx --test server/src/__tests__/scheduler-low-pri-budget.test.ts
 */
import { test, after, beforeEach } from 'node:test'
import assert from 'node:assert/strict'
import { pool } from '../db/pool.js'
import {
  _consumeLowPriorityWakeBudget,
  _resetLowPriorityWakeBudgetForTests,
  shouldDeliverToMutedAgent,
} from '../agents/scheduler.js'

after(async () => {
  // scheduler.ts transitively imports redis.ts which opens connections.
  try { await pool.end() } catch { /* ignore */ }
  try {
    const { redis, sub } = await import('../redis.js')
    redis.disconnect()
    sub.disconnect()
  } catch { /* ignore */ }
})

beforeEach(() => { _resetLowPriorityWakeBudgetForTests() })

test('first 20 calls allowed within a 60s window', () => {
  const t = 1_000_000
  for (let i = 0; i < 20; i++) {
    assert.equal(_consumeLowPriorityWakeBudget(t), true, `call ${i + 1} should be allowed`)
  }
})

test('call 21 within the same window is rejected', () => {
  const t = 1_000_000
  for (let i = 0; i < 20; i++) _consumeLowPriorityWakeBudget(t)
  assert.equal(_consumeLowPriorityWakeBudget(t), false)
})

test('budget resets after 60s', () => {
  const t0 = 1_000_000
  for (let i = 0; i < 20; i++) _consumeLowPriorityWakeBudget(t0)
  assert.equal(_consumeLowPriorityWakeBudget(t0), false, 'still rejected before window rolls')
  // 60s later, window rolls
  const t1 = t0 + 60_000
  assert.equal(_consumeLowPriorityWakeBudget(t1), true, 'allowed after window roll')
})

test('budget does NOT reset before 60s', () => {
  const t0 = 1_000_000
  for (let i = 0; i < 20; i++) _consumeLowPriorityWakeBudget(t0)
  assert.equal(_consumeLowPriorityWakeBudget(t0 + 59_999), false)
})

test('many rejections in a window are absorbed, then the next window is fresh', () => {
  const t0 = 2_000_000
  for (let i = 0; i < 20; i++) _consumeLowPriorityWakeBudget(t0)
  // 500 rejections — what a crashloop-recovery burst would look like
  for (let i = 0; i < 500; i++) {
    assert.equal(_consumeLowPriorityWakeBudget(t0), false)
  }
  // After the window
  const t1 = t0 + 60_000
  assert.equal(_consumeLowPriorityWakeBudget(t1), true)
})

test('muted agent delivery only allows direct, exact mention, or quote reply', () => {
  const base = { agentId: 'nova-12', conversationKind: 'group', body: 'ordinary room chatter', quotedAuthorId: null }
  assert.equal(shouldDeliverToMutedAgent(base), false)
  assert.equal(shouldDeliverToMutedAgent({ ...base, conversationKind: 'direct' }), true)
  assert.equal(shouldDeliverToMutedAgent({ ...base, body: 'please check this @nova-12' }), true)
  assert.equal(shouldDeliverToMutedAgent({ ...base, body: 'ping @NOVA-12, please' }), true)
  assert.equal(shouldDeliverToMutedAgent({ ...base, body: 'this is for @nova-123' }), false, 'prefix mentions must not leak through')
  assert.equal(shouldDeliverToMutedAgent({ ...base, body: 'email@nova-12 is not a mention' }), false)
  assert.equal(shouldDeliverToMutedAgent({ ...base, quotedAuthorId: 'nova-12' }), true)
})
