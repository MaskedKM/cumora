import { expect, test } from 'vitest'
import type { ApiParticipant } from '@/api/client'
import type { Participant } from '@/types'
import { mergeRoster, sameParticipant } from './rosterMerge'

function api(over: Partial<ApiParticipant> & { id: string }): ApiParticipant {
  return {
    kind: 'agent',
    name: `agent-${over.id}`,
    initial: 'A',
    avatarBg: '#123456',
    status: 'avail',
    statusUpdatedAt: '2026-08-29T00:00:00.000Z',
    ...over,
  } as ApiParticipant
}

/** Mirrors the store's fromApi normalization just enough to exercise the
 *  merge: optionals collapse to their stored representation. */
const map = (p: ApiParticipant): Participant => ({
  id: p.id,
  kind: p.kind,
  name: p.name,
  role: p.role ?? undefined,
  initial: p.initial,
  avatarBg: p.avatarBg,
  avatarUrl: p.avatarUrl ?? null,
  status: p.status,
  statusUpdatedAt: p.statusUpdatedAt ?? new Date().toISOString(),
  bio: p.bio ?? undefined,
  tools: p.tools ?? undefined,
  systemPrompt: p.systemPrompt ?? undefined,
  model: p.model ?? null,
  fastModel: p.fastModel ?? null,
  email: p.email ?? null,
  departedAt: p.departedAt ?? null,
  computerId: p.computerId ?? null,
  engine: p.engine ?? null,
})

test('steady-state refresh keeps the whole record reference', () => {
  const current = { a: map(api({ id: 'a' })), b: map(api({ id: 'b' })) }
  const next = mergeRoster(current, [api({ id: 'a' }), api({ id: 'b' })], map)
  expect(next).toBe(current)
})

test('unchanged rows keep their object reference; a changed row swaps', () => {
  const current = { a: map(api({ id: 'a' })), b: map(api({ id: 'b' })) }
  const next = mergeRoster(current, [api({ id: 'a', name: 'renamed' }), api({ id: 'b' })], map)
  expect(next).not.toBe(current)
  expect(next.a).not.toBe(current.a)
  expect(next.a.name).toBe('renamed')
  expect(next.b).toBe(current.b)
})

test('rows absent from the fetch are dropped; new rows are added', () => {
  const current = { a: map(api({ id: 'a' })), gone: map(api({ id: 'gone' })) }
  const next = mergeRoster(current, [api({ id: 'a' }), api({ id: 'new' })], map)
  expect(next.gone).toBeUndefined()
  expect(next.new.id).toBe('new')
  expect(next.a).toBe(current.a)
})

test('tools compares by content, not by array identity', () => {
  const current = { a: map(api({ id: 'a', tools: ['web', 'shell'] })) }
  const sameContent = mergeRoster(current, [api({ id: 'a', tools: ['web', 'shell'] })], map)
  expect(sameContent).toBe(current)

  const changed = mergeRoster(current, [api({ id: 'a', tools: ['web'] })], map)
  expect(changed).not.toBe(current)

  const noTools = map(api({ id: 'a' }))
  expect(sameParticipant(noTools, { ...noTools, tools: undefined })).toBe(true)
  expect(sameParticipant(noTools, { ...noTools, tools: ['web'] })).toBe(false)
})

test('sameParticipant is false on any tracked field drift', () => {
  const base = map(api({ id: 'a' }))
  for (const patch of [
    { name: 'x' },
    { statusUpdatedAt: '2026-08-29T01:00:00.000Z' },
    { avatarUrl: 'https://x/y.png' },
    { engine: 'claude' },
  ] as Array<Partial<ApiParticipant>>) {
    expect(sameParticipant(base, map(api({ id: 'a', ...patch })))).toBe(false)
  }
  expect(sameParticipant(base, map(api({ id: 'a' })))).toBe(true)
})
