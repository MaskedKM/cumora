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
 *
 * Config source (ticket #22): `CEREBELLUM_BASE_URL`/`CEREBELLUM_API_KEY` are
 * read through `cerebellum-settings.ts` (DB-backed, admin-editable) instead
 * of `env.*` — a fresh client is built per call from whatever's currently
 * stored, so an admin edit takes effect on the next cerebellum call, no
 * restart. This is the one caller allowed to see the decrypted API key.
 */
import OpenAI from 'openai'
import { createOpenAIResponsesAdapter } from './novita.js'
import { getCerebellumSettings, getCerebellumApiKeyForClient } from './cerebellum-settings.js'

/** Test-only override — mirrors `__setNovitaClientOverrideForTesting` in
 *  novita.ts. Lets unit tests exercise the translation logic against a fake
 *  `chat.completions.create` without real Cerebellum settings or network
 *  access. Production code never sets this. */
let testCerebellumClientOverride: OpenAI | null = null
export function __setCerebellumClientOverrideForTesting(client: OpenAI | null): void {
  testCerebellumClientOverride = client
}
async function cerebellumClient(): Promise<OpenAI> {
  if (testCerebellumClientOverride) return testCerebellumClientOverride
  const [{ baseUrl }, apiKey] = await Promise.all([getCerebellumSettings(), getCerebellumApiKeyForClient()])
  return new OpenAI({ apiKey, baseURL: baseUrl })
}

/** True once a base URL and API key are both configured — the minimum
 *  needed to actually reach a remote provider through this adapter. */
export async function cerebellumRemoteConfigured(): Promise<boolean> {
  const [{ baseUrl }, apiKey] = await Promise.all([getCerebellumSettings(), getCerebellumApiKeyForClient()])
  return Boolean(baseUrl && apiKey)
}

/** The Responses-API-shaped surface — same contract as `novitaResponsesShim`
 *  — routed at `CEREBELLUM_BASE_URL` with `CEREBELLUM_API_KEY`. */
export const cerebellumResponsesShim = createOpenAIResponsesAdapter(cerebellumClient)
