/**
 * Unit tests for the provider-agnostic cerebellum remote adapter
 * (server/src/cerebellum-adapter.ts). No network access:
 * `chat.completions.create` is stubbed via
 * `__setCerebellumClientOverrideForTesting`, mirroring
 * `server/src/__tests__/novita.test.ts`'s pattern for the Novita adapter.
 * These tests assert the adapter works against an arbitrary provider config
 * (not just Novita) and that, unlike Novita's `novita/`-prefix convention,
 * the model id is sent through unmodified.
 *
 * Run: node --import tsx --test server/src/__tests__/cerebellum-adapter.test.ts
 */

import assert from 'node:assert/strict'
import { test } from 'node:test'
import type OpenAI from 'openai'
import type { ChatCompletionChunk } from 'openai/resources/chat/completions'
import type { ResponseStreamEvent } from 'openai/resources/responses/responses'
import { applyResponseStreamEvent, consumeResponseStream, newResponseStreamState } from '../agents/turn-stream.js'
import {
  __setCerebellumClientOverrideForTesting,
  cerebellumResponsesShim,
} from '../cerebellum-adapter.js'

function fakeClient(create: (...args: unknown[]) => unknown): OpenAI {
  return { chat: { completions: { create } } } as unknown as OpenAI
}

async function* asAsync<T>(items: T[]): AsyncGenerator<T> {
  for (const item of items) yield item
}

test('non-streaming: translates instructions+input to system+user messages, sends the model id unmodified (no novita/-style prefix stripping)', async () => {
  let capturedBody: Record<string, unknown> = {}
  __setCerebellumClientOverrideForTesting(fakeClient(async (args: unknown) => {
    capturedBody = args as Record<string, unknown>
    return {
      id: 'chatcmpl-1',
      choices: [{ message: { role: 'assistant', content: 'yes, actionable', tool_calls: [] } }],
      usage: { prompt_tokens: 7, completion_tokens: 3, total_tokens: 10 },
    }
  }))
  try {
    const r = await cerebellumResponsesShim.create({
      model: 'deepseek-v3.2',
      instructions: 'classify the card',
      input: 'card: fix the bug',
      max_output_tokens: 50,
    } as never) as { output_text: string; usage: unknown }

    assert.equal(r.output_text, 'yes, actionable')
    assert.deepEqual(r.usage, {
      input_tokens: 7,
      input_tokens_details: { cached_tokens: 0 },
      output_tokens: 3,
      output_tokens_details: { reasoning_tokens: 0 },
      total_tokens: 10,
    })
    // No prefix convention for the generic adapter — model passed through as-is.
    assert.equal(capturedBody.model, 'deepseek-v3.2')
    assert.deepEqual(capturedBody.messages, [
      { role: 'system', content: 'classify the card' },
      { role: 'user', content: 'card: fix the bug' },
    ])
    assert.equal(capturedBody.max_tokens, 50)
  } finally {
    __setCerebellumClientOverrideForTesting(null)
  }
})

test('streaming: text-only reply produces created → output_text.delta* → completed, consumable by the real turn-stream reducer', async () => {
  const chunks: ChatCompletionChunk[] = [
    { id: 'c1', created: 0, model: 'x', object: 'chat.completion.chunk', choices: [{ index: 0, delta: { content: 'Ye' }, finish_reason: null }] } as unknown as ChatCompletionChunk,
    { id: 'c1', created: 0, model: 'x', object: 'chat.completion.chunk', choices: [{ index: 0, delta: { content: 's' }, finish_reason: null }] } as unknown as ChatCompletionChunk,
    { id: 'c1', created: 0, model: 'x', object: 'chat.completion.chunk', choices: [{ index: 0, delta: {}, finish_reason: 'stop' }], usage: { prompt_tokens: 2, completion_tokens: 2, total_tokens: 4 } } as unknown as ChatCompletionChunk,
  ]
  __setCerebellumClientOverrideForTesting(fakeClient(async () => asAsync(chunks)))
  try {
    const stream = await cerebellumResponsesShim.create({
      model: 'some-provider/some-model',
      input: 'is this actionable?',
      stream: true,
    } as never) as AsyncIterable<ResponseStreamEvent>

    const state = newResponseStreamState()
    await consumeResponseStream(stream, (ev) => applyResponseStreamEvent(state, ev))

    assert.equal(Array.from(state.responseTextByPart.values()).join(''), 'Yes')
    assert.equal(state.responseStatus, 'completed')
    assert.equal(state.totalTokens, 4)
  } finally {
    __setCerebellumClientOverrideForTesting(null)
  }
})
