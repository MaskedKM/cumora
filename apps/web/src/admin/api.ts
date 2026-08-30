// Admin 域线上形状由契约生成(#48);LlmObservability 系为开放对象端点(规范未收
// 具名 schema),暂保留手写,待收紧时并入。
import type { components } from '@cumora/contract'

type Schemas = components['schemas']

export type Tier = Schemas['AdminUser']['tier']
export type CerebellumRoute = Schemas['AdminSettings']['cerebellum_route']
export type AdminUser = Schemas['AdminUser']
export type AdminUserDetail = Schemas['AdminUserDetail']
export type AdminWaitlistEntry = Schemas['AdminWaitlistEntry']
export type AdminSettings = Schemas['AdminSettings']
export type AdminSettingsPatch = Partial<AdminSettings> & { cerebellum_api_key?: string }
export type AdminStats = Schemas['AdminStats']

/** LLM 观测域(开放对象端点,规范未收具名 schema)的枚举镜像 —— 与 server llm-ledger 锁步。 */
export type LlmCallPurpose =
  | 'agent-turn' | 'convene-speech'
  | 'inbox-triage' | 'synthetic-wake-gate' | 'agenda'
  | 'compaction' | 'completion-verify' | 'steer-summary' | 'convene-decision'
  | 'palette' | 'gender' | 'avatar-image' | 'agent-image'
export type LlmCallStatus = 'ok' | 'rate_limited' | 'timeout' | 'failed'
export type LlmCallSource = 'cloud' | 'byoa-claude' | 'byoa-codex' | 'byoa-grok' | 'byoa-cursor' | 'byoa-zcode'

export interface LlmSummary {
  sinceDays: number
  totalCalls: number
  totalCostUsd: number
  totalInputTokens: number
  totalCachedInputTokens: number
  totalOutputTokens: number
  failureRate: number
  rateLimitedCalls: number
  topPurpose: { purpose: LlmCallPurpose; costUsd: number } | null
  activeTenants: number
  /** Overall cache hit rate over the window. Null when there's no input
   *  traffic at all. The single best optimization knob — every cached input
   *  token costs ~10× less. */
  cacheHitRate: number | null
  /** Upper-bound $ savings if EVERY input token were cached at the model's
   *  own cached rate. Aspirational (cold one-shots can't cache). */
  savableUsd: number
}

export interface LlmRollupRow {
  purpose: LlmCallPurpose
  model: string
  source: LlmCallSource
  calls: number
  okCalls: number
  failedCalls: number
  rateLimitedCalls: number
  inputTokens: number
  cachedInputTokens: number
  cacheCreationTokens: number
  outputTokens: number
  reasoningTokens: number
  costUsd: number
  costEstimated: boolean
  /** Upper-bound $ saved if this row's input were 100% cached (at the row
   *  model's own cached rate). 'Money on the table'. */
  savableUsd: number
}

export interface LlmTrendBucket {
  day: string
  purpose: LlmCallPurpose
  costUsd: number
  calls: number
  /** Uncached input tokens for this (day, purpose) bucket. */
  inputTokens: number
  /** Cache-read input tokens for this bucket. */
  cachedInputTokens: number
}

export interface LlmTopAgentRow {
  agentId: string | null
  companyId: string | null
  agentName: string | null
  agentAvatarUrl: string | null
  agentAvatarBg: string | null
  agentInitial: string | null
  companyName: string | null
  costUsd: number
  calls: number
  inputTokens: number
  cachedInputTokens: number
  outputTokens: number
}

export interface LlmTenantRow {
  companyId: string
  name: string | null
  slug: string | null
  costUsd: number
  calls: number
  inputTokens: number
  cachedInputTokens: number
  outputTokens: number
}

export interface LlmDaemonVersionRow {
  daemonVersion: string
  source: LlmCallSource
  calls: number
  costUsd: number
  inputTokens: number
  cachedInputTokens: number
  outputTokens: number
  failureRate: number
  firstSeen: string
  lastSeen: string
}

export interface LlmObservabilityPayload {
  summary: LlmSummary
  rollup: LlmRollupRow[]
  trend: LlmTrendBucket[]
  topAgents: LlmTopAgentRow[]
  tenants: LlmTenantRow[]
  daemonVersions: LlmDaemonVersionRow[]
}

/** One raw call row returned by the drill-down endpoint. Same shape as the
 *  server's LlmCallRow — kept here to avoid a server-only import. */
export interface LlmCallRow {
  id: string
  createdAt: string
  companyId: string | null
  agentId: string | null
  agentName: string | null
  runId: string | null
  conversationId: string | null
  purpose: LlmCallPurpose
  source: LlmCallSource
  model: string
  inputTokens: number
  cachedInputTokens: number
  cacheCreationTokens: number
  outputTokens: number
  reasoningTokens: number
  costUsd: number
  costEstimated: boolean
  measured: boolean
  latencyMs: number | null
  status: LlmCallStatus
  error: string | null
  extras: Record<string, unknown> | null
  daemonVersion: string | null
}

/**
 * Admin API client — talks to /api/admin/*. 共享骨架(#147 ①)在
 * @/api/core:origin 解析/Bearer 注入/401 清 session/错误 detail 解析
 * 与主面 client.ts 合一;本面差异(无 x-company-id/devtools 头、401 恒
 * 清、路径前缀 /api/admin)经 fetchJson opts 表达。
 *
 * Auth + base URL come from the same auth store the main client uses, so
 * a token minted by the regular sign-in flow is what authenticates admin
 * calls.
 */
import { fetchJson, SERVER_ORIGIN } from '@/api/core'

async function http<T>(path: string, init?: RequestInit): Promise<T> {
  return fetchJson<T>(path, init, { base: `${SERVER_ORIGIN}/api/admin` })
}

export const adminApi = {
  me: () => http<{ userId: string; isAdmin: true }>('/me'),
  stats: () => http<AdminStats>('/stats'),

  observabilityLlm: (params: { sinceDays?: number; model?: string; companyId?: string; fresh?: boolean } = {}) => {
    const qs = new URLSearchParams()
    if (params.sinceDays) qs.set('sinceDays', String(params.sinceDays))
    if (params.model)     qs.set('model', params.model)
    if (params.companyId) qs.set('companyId', params.companyId)
    // `fresh` bypasses the server's short response cache — used by the manual
    // refresh button and auto-refresh so they actually re-read current data.
    if (params.fresh)     qs.set('fresh', '1')
    const s = qs.toString()
    return http<LlmObservabilityPayload>(s ? `/observability/llm?${s}` : '/observability/llm')
  },
  /** Drill-down: raw call rows for a (purpose × model × source) bucket OR
   *  for a specific run / agent. Used by the panel that opens when the
   *  operator clicks a rollup row on the Observability page. */
  observabilityLlmCalls: (params: {
    sinceDays?: number
    purpose?: LlmCallPurpose
    model?: string
    source?: LlmCallSource
    runId?: string
    agentId?: string
    companyId?: string
    limit?: number
    sortBy?: 'cost' | 'latency' | 'hop' | 'created'
  } = {}) => {
    const qs = new URLSearchParams()
    if (params.sinceDays) qs.set('sinceDays', String(params.sinceDays))
    if (params.purpose)   qs.set('purpose', params.purpose)
    if (params.model)     qs.set('model', params.model)
    if (params.source)    qs.set('source', params.source)
    if (params.runId)     qs.set('runId', params.runId)
    if (params.agentId)   qs.set('agentId', params.agentId)
    if (params.companyId) qs.set('companyId', params.companyId)
    if (params.limit)     qs.set('limit', String(params.limit))
    if (params.sortBy)    qs.set('sortBy', params.sortBy)
    const s = qs.toString()
    return http<LlmCallRow[]>(s ? `/observability/llm/calls?${s}` : '/observability/llm/calls')
  },

  settings: () => http<AdminSettings>('/settings'),
  setSettings: (patch: AdminSettingsPatch) =>
    http<AdminSettings>('/settings', { method: 'PUT', body: JSON.stringify(patch) }),
  /** Union of `available_engines` across every currently-online Computer —
   *  feeds the byoa local-engine dropdown. Empty when none are online. */
  availableEngines: () => http<{ engines: string[] }>('/computers/available-engines'),

  listUsers: (params: { q?: string; tier?: Tier | ''; limit?: number; offset?: number } = {}) => {
    const qs = new URLSearchParams()
    if (params.q)      qs.set('q', params.q)
    if (params.tier)   qs.set('tier', params.tier)
    if (params.limit)  qs.set('limit', String(params.limit))
    if (params.offset) qs.set('offset', String(params.offset))
    const s = qs.toString()
    return http<{ items: AdminUser[]; total: number; limit: number; offset: number }>(
      s ? `/users?${s}` : '/users',
    )
  },
  getUser: (id: string) => http<AdminUserDetail>(`/users/${id}`),
  patchUser: (
    id: string,
    patch: { tier?: Tier; isAdmin?: boolean; suspended?: boolean; suspensionReason?: string | null },
  ) =>
    http<AdminUser>(`/users/${id}`, { method: 'PATCH', body: JSON.stringify(patch) }),

  /** Convenience wrappers around patchUser — the panel uses these for the
   *  two suspend/unsuspend buttons. Server-side they hit the same PATCH
   *  endpoint; keeping the call sites readable. */
  suspendUser: (id: string, reason: string | null) =>
    http<AdminUser>(`/users/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ suspended: true, suspensionReason: reason }),
    }),
  unsuspendUser: (id: string) =>
    http<AdminUser>(`/users/${id}`, {
      method: 'PATCH',
      body: JSON.stringify({ suspended: false }),
    }),

  listWaitlist: (params: {
    status?: 'pending' | 'approved' | 'rejected'
    q?: string
    limit?: number
    offset?: number
  } = {}) => {
    const qs = new URLSearchParams()
    if (params.status) qs.set('status', params.status)
    if (params.q) qs.set('q', params.q)
    if (params.limit)  qs.set('limit', String(params.limit))
    if (params.offset) qs.set('offset', String(params.offset))
    const s = qs.toString()
    return http<{ items: AdminWaitlistEntry[]; total: number; limit: number; offset: number }>(
      s ? `/waitlist?${s}` : '/waitlist',
    )
  },
  approveWaitlist: (id: string) =>
    http<{ userId: string; companyId: string | null }>(`/waitlist/${id}/approve`, { method: 'POST' }),
  rejectWaitlist: (id: string, note?: string) =>
    http<{ ok: true }>(`/waitlist/${id}/reject`, {
      method: 'POST',
      body: JSON.stringify(note ? { note } : {}),
    }),
}
