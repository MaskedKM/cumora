import { expect, test } from 'vitest'
import { StreamingDeltaBatch, applyBufferedDeltas, type StreamingBody } from './streamingBatch'

test('pushes for one message merge in arrival order, sequence keeps the latest', () => {
  const b = new StreamingDeltaBatch()
  b.push('m1', 'c1', 'a1', 3, 'Hel')
  b.push('m1', 'c1', 'a1', 4, 'lo w')
  b.push('m1', 'c1', 'a1', 5, 'orld')
  const drained = b.drain()
  expect(drained).toHaveLength(1)
  expect(drained[0]).toMatchObject({ messageId: 'm1', text: 'Hello world', sequence: 5 })
})

test('drain returns distinct messages in insertion order and resets the batch', () => {
  const b = new StreamingDeltaBatch()
  b.push('m1', 'c1', 'a1', 1, 'aa')
  b.push('m2', 'c2', 'a2', 1, 'bb')
  b.push('m1', 'c1', 'a1', 2, 'cc')
  const drained = b.drain()
  expect(drained.map((d) => d.messageId)).toEqual(['m1', 'm2'])
  expect(drained[0].text).toBe('aacc')
  expect(b.isEmpty).toBe(true)
  expect(b.bufferedChars).toBe(0)
  expect(b.drain()).toEqual([])
})

test('bufferedChars tracks total text length', () => {
  const b = new StreamingDeltaBatch()
  b.push('m1', 'c1', 'a1', 1, '12345')
  b.push('m2', 'c1', 'a1', 1, '678')
  expect(b.bufferedChars).toBe(8)
})

test('drop removes a message tail and gives the chars back', () => {
  const b = new StreamingDeltaBatch()
  b.push('m1', 'c1', 'a1', 1, 'keep-me')
  b.push('m2', 'c1', 'a1', 1, 'gone')
  b.drop('m2')
  expect(b.bufferedChars).toBe(7)
  const drained = b.drain()
  expect(drained.map((d) => d.messageId)).toEqual(['m1'])
  b.drop('never-pushed')  // no-op, no char drift
  expect(b.bufferedChars).toBe(0)
})

test('applyBufferedDeltas creates a missing entry (first flush = birth)', () => {
  const out = applyBufferedDeltas<Record<string, StreamingBody>>({}, [
    { messageId: 'm1', conversationId: 'c1', authorId: 'a1', sequence: 2, text: 'Hi' },
  ])
  expect(out.m1).toEqual({ body: 'Hi', conversationId: 'c1', authorId: 'a1', sequence: 2 })
})

test('applyBufferedDeltas appends to live entries and keeps the last sequence', () => {
  const live = {
    m1: { body: 'Hel', conversationId: 'c1', authorId: 'a1', sequence: 1 },
    untouched: { body: 'x', conversationId: 'c9', authorId: 'a9', sequence: 9 },
  }
  const out = applyBufferedDeltas(live, [
    { messageId: 'm1', conversationId: 'c1', authorId: 'a1', sequence: 7, text: 'lo' },
  ])
  expect(out.m1).toEqual({ body: 'Hello', conversationId: 'c1', authorId: 'a1', sequence: 7 })
  expect(out.untouched).toBe(live.untouched)
})

test('applyBufferedDeltas returns the same reference for an empty drain', () => {
  const live = { m1: { body: 'x', conversationId: 'c1', authorId: 'a1', sequence: 1 } }
  expect(applyBufferedDeltas(live, [])).toBe(live)
})
