/**
 * Provider-agnostic remote adapter for the "cerebellum" tier (lightweight
 * classifiers — agenda triage, palette, gender inference, ... — as opposed
 * to the "brain" tier's main reasoning turns).
 *
 * Reuses `server/src/novita.ts`'s Responses-API ⇄ Chat-Completions
 * translation (`createOpenAIResponsesAdapter`) — the same gap-bridging logic
 * that lets Novita's Chat-Completions-only surface serve Responses-shaped
 * call sites — but parameterized by `CEREBELLUM_BASE_URL`/`CEREBELLUM_API_KEY`
 * instead of `NOVITA_*`, so it works against any OpenAI-Chat-Completions-
 * compatible endpoint (DeepSeek, a self-hosted proxy, etc.), not just Novita.
 *
 * No model-prefix convention here (unlike `novita/<model>`): cerebellum
 * routing is a separate, explicit config surface — see server/src/env.ts —
 * so the model id passed to `.create()` is sent to the provider as-is.
 *
 * This module only builds the adapter. Deciding *when* to use it (vs. plain
 * OpenAI, vs. a local BYOA engine) and wiring it into a call site is out of
 * scope here — left to the callers that consume `cerebellumResponsesShim`.
 */
import OpenAI from 'openai'
import { createOpenAIResponsesAdapter } from './novita.js'
import { env } from './env.js'

let _cerebellumClient: OpenAI | null = null
/** Test-only override — mirrors `__setNovitaClientOverrideForTesting` in
 *  novita.ts. Lets unit tests exercise the translation logic against a fake
 *  `chat.completions.create` without real `CEREBELLUM_*` credentials or
 *  network access. Production code never sets this. */
let testCerebellumClientOverride: OpenAI | null = null
export function __setCerebellumClientOverrideForTesting(client: OpenAI | null): void {
  testCerebellumClientOverride = client
}
function cerebellumClient(): OpenAI {
  if (testCerebellumClientOverride) return testCerebellumClientOverride
  if (!_cerebellumClient) {
    _cerebellumClient = new OpenAI({
      apiKey: env.CEREBELLUM_API_KEY,
      baseURL: env.CEREBELLUM_BASE_URL,
    })
  }
  return _cerebellumClient
}

/** True once `CEREBELLUM_BASE_URL`/`CEREBELLUM_API_KEY` are both set — the
 *  minimum needed to actually reach a remote provider through this adapter. */
export function cerebellumRemoteConfigured(): boolean {
  return Boolean(env.CEREBELLUM_BASE_URL && env.CEREBELLUM_API_KEY)
}

/** The Responses-API-shaped surface — same contract as `novitaResponsesShim`
 *  — routed at `CEREBELLUM_BASE_URL` with `CEREBELLUM_API_KEY`. */
export const cerebellumResponsesShim = createOpenAIResponsesAdapter(cerebellumClient)
