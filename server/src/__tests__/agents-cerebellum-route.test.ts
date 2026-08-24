/**
 * Unit tests for the pure cerebellum route decision (ticket #19). Per the
 * spec's testing decision, route+fallback resolution is a small pure
 * function — no DB/network seam needed, just different inputs.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { resolveCerebellumRoute } from '../agents/cerebellum-route.js'

test('CEREBELLUM_ROUTE=remote always resolves remote, regardless of computer state', () => {
  assert.equal(
    resolveCerebellumRoute({ route: 'remote', localEngine: 'claude', computer: null }),
    'remote',
  )
  assert.equal(
    resolveCerebellumRoute({
      route: 'remote',
      localEngine: 'claude',
      computer: { status: 'online', available_engines: ['claude'] },
    }),
    'remote',
  )
})

test('byoa route + no computer (Cloud-managed agent) falls back to remote', () => {
  assert.equal(
    resolveCerebellumRoute({ route: 'byoa', localEngine: 'claude', computer: null }),
    'remote',
  )
})

test('byoa route + online computer advertising the configured engine resolves byoa', () => {
  assert.equal(
    resolveCerebellumRoute({
      route: 'byoa',
      localEngine: 'claude',
      computer: { status: 'online', available_engines: ['claude', 'codex'] },
    }),
    'byoa',
  )
})

test('byoa route + offline computer falls back to remote', () => {
  assert.equal(
    resolveCerebellumRoute({
      route: 'byoa',
      localEngine: 'claude',
      computer: { status: 'offline', available_engines: ['claude'] },
    }),
    'remote',
  )
})

test('byoa route + busy computer falls back to remote (only "online" is eligible)', () => {
  assert.equal(
    resolveCerebellumRoute({
      route: 'byoa',
      localEngine: 'claude',
      computer: { status: 'busy', available_engines: ['claude'] },
    }),
    'remote',
  )
})

test('byoa route + online computer missing the configured engine falls back to remote', () => {
  assert.equal(
    resolveCerebellumRoute({
      route: 'byoa',
      localEngine: 'claude',
      computer: { status: 'online', available_engines: ['codex', 'grok'] },
    }),
    'remote',
  )
})
