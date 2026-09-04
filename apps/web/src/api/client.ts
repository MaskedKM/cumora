import type { components } from '@cumora/contract'
import type { WsEvent as WsServerEvent } from '@cumora/contract/ws-events'

type Schemas = components['schemas']

export type ApiMessage = Schemas['Message']
export type ApiConversation = Schemas['Conversation']
export type ApiQuotaWindow = Schemas['QuotaWindow']
export type ApiQuotaSnapshot = Schemas['QuotaSnapshot']
export type ApiQuotaResponse = Schemas['QuotaResponse']
export type ApiProject = Schemas['Project']
export type ApiParticipant = Schemas['Participant']
export type ApiComputer = Schemas['Computer']
export type ApiSearchResults = Schemas['SearchResults']
export type AgentInput = Schemas['AgentInput']
/** 上传结果:MessageAttachment + 服务端必回的 url(发送消息的入参允许 mock 无 url)。 */
export type ApiAttachment = Schemas['MessageAttachment'] & { url: string }
export type UploadCapabilities = Schemas['UploadCapabilities']
export type ApiWhisper = Schemas['Whisper']
export type ApiWhisperMessage = Schemas['WhisperMessage']
export type ApiAutonomy = Schemas['Autonomy']
export type ApiAgentRun = Schemas['AgentRun']
export type ApiTriageAgentRow = Schemas['TriageAgentRow']
export type ApiTriageLedgerRow = Schemas['TriageLedgerRow']
export type ApiTriageUnitPrice = Schemas['TriageUnitPrice']
export type ApiTriagePriceRow = Schemas['TriagePriceRow']
export type ApiTriageEconomics = Schemas['TriageEconomics']
export type ApiAgentEvent = Schemas['AgentEvent']
export type ApiTranscriptEntry = { seq: number; type: string; tool?: string | null; content?: string | null; input?: unknown; createdAt?: string }
// #261 公司 Skills 库(SOP 手册)行形状(/api/skills 列表)。
export type ApiCompanySkill = {
  id: string
  name: string
  description: string
  bundleHash: string
  fileCount: number
  createdBy?: string | null
  createdAt: string
  updatedAt: string
}
// #264 人侧 Inbox 条目/响应形状(/api/inbox;store 消费)。
export type ApiInboxItem = {
  id: string
  severity: 'action_required' | 'attention' | 'info'
  type: string
  title: string
  body?: string | null
  linkKind?: string | null
  linkId?: string | null
  read: boolean
  createdAt: string
}
export type ApiInboxResponse = {
  items: ApiInboxItem[]
  counts: { actionRequired: number; attention: number; info: number }
  mutedTypes: string[]
}

export type ApiCompanySkillDetail = {
  id: string
  name: string
  description: string
  bundleHash: string
  files: Array<{ path: string; body: string }>
}
export type ApiConveneSession = Schemas['ConveneSession']
export type ApiConveneTranscript = Schemas['ConveneTranscript']
export type ApiDevtoolsCapabilities = Schemas['DevtoolsCapabilities']
export type ApiAgentWorkspaceFile = Schemas['AgentWorkspaceFile']
export type ApiAgentWorkspaceFileContent = Schemas['AgentWorkspaceFileContent']
export type ServerCapabilities = Schemas['ServerCapabilities']
export type ApiInvitation = Schemas['Invitation']
export type ApiInvitationWithToken = Schemas['InvitationWithToken']
export type ApiInvitationEmailDelivery = Schemas['InvitationEmailDelivery']
export type ApiInvitationPreview = Schemas['InvitationPreview']
export type ApiInvitationAccept = Schemas['InvitationAccept']
export type ShippingFeatureSummary = Schemas['ShippingFeatureSummary']
export type ShippingInvariant = Schemas['ShippingInvariant']
export type ShippingVerification = Schemas['ShippingVerification']
export type ShippingRelease = Schemas['ShippingRelease']
export type ShippingFriction = Schemas['ShippingFriction']
export type ShippingRegression = Schemas['ShippingRegression']
export type ShippingFeatureDetail = Schemas['ShippingFeatureDetail']
export type ShippingOverview = Schemas['ShippingOverview']
export type ApiWorkspaceSummary = Schemas['WorkspaceSummary']
export type ApiWorkspaceMember = Schemas['WorkspaceMember']
export type ApiWorkspaceAssociation = Schemas['WorkspaceAssociation']
export type ApiWorkspaceDetail = Schemas['WorkspaceDetail']
export type ApiWorkspaceFileEntry = Schemas['WorkspaceFileEntry']
export type ApiDocument = Schemas['Document']
export type CalendarEventInput = Schemas['CalendarEventInput']
export type PresignResponse = Schemas['PresignResponse']
export type MeResponse = Schemas['MeResponse']

import { getActiveCompanyId, getAuthToken } from '@/stores/auth'
/* #221:Status/CalendarEventKind/ComputerStatus 原只被手写 WsEvent union 引用,
 * 契约化后随之退役(事件载荷里的枚举由生成物直接引用契约 schema)。 */
import type {BoardCardComment, BoardCardLookup,BoardSnapshot, 
  BoardSummary, CalendarDispatch, 
  CalendarEvent, CalendarEventStatus, ComputerKind, EngineId,
} from '@/types'
import { ApiError, fetchJson, getDevModeEnabled, SERVER_ORIGIN } from './core'

// 共享骨架(#147 ①)挪到 ./core —— origin 三层解析/Bearer/401 清 session/
// 错误 detail 解析与 admin 面合一;此处 re-export 维持既有导入方不变。
export {
  ApiError,getDevModeEnabled, 
  getServerOrigin, setDevModeEnabled,setServerOrigin, 
} from './core'

const DEV_API_TARGET = import.meta.env.DEV
  ? (import.meta.env.VITE_CUMORA_DEV_API_TARGET as string | undefined)?.replace(/\/+$/, '')
  : undefined

const API = `${SERVER_ORIGIN}/api`

/** Origin to embed in a local computer pairing command.
 * In Vite dev the browser uses a relative proxy, so SERVER_ORIGIN is empty;
 * the daemon still needs the API target rather than the renderer origin. */
export function getPairingServerOrigin(): string {
  return SERVER_ORIGIN || DEV_API_TARGET || ''
}

/** WS origin (ws:// or wss://) derived from the resolved server origin.
 *  When SERVER_ORIGIN is empty (Vite dev, same-origin static deploy),
 *  fall back to the page's location so the existing relative path
 *  (`/ws`) still works through the proxy. */
function wsOrigin(): string {
  if (SERVER_ORIGIN) return SERVER_ORIGIN.replace(/^http/, 'ws')
  return `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}`
}

export async function http<T>(path: string, init?: RequestInit): Promise<T> {
  return fetchJson<T>(path, init, {
    base: API,
    companyHeader: true,
    devModeHeader: true,
    // 匿名 auth 端点(/auth/*)的 401 是"凭证错误"而非"会话过期"——
    // 不能清掉半登录态,交给调用方渲染表单错误。
    clear401: (p) => !p.startsWith('/auth/'),
  })
}


/** A Computer (agent host) as returned by GET /api/computers. */

/** Universal-search response. The backend ranks results inside each bucket;
 *  the frontend renders them in this declared order (participants → rooms →
 *  groups → messages), matching the product priority. */


/** A peek-view entry — either a 1-on-1 direct chat or a multi-agent
 *  group, where every member is an agent. "Whisper" is the frontend tab
 *  name; on the server these are just regular conversations the user
 *  isn't a member of (so they don't show up in /conversations, but the
 *  peek tab lets the user eavesdrop). */


export type ApiAgentRunStatus = Schemas['AgentRunStatus']
export type ApiAgentEventLevel = NonNullable<Schemas['AgentEvent']['level']>[number] | never


// ── Triage cost-effectiveness ledger ──
export type ApiTriageSource = Schemas['TriageSource']


export type ApiInvitationStatus = Schemas['Invitation']['status']


/** Returned exactly ONCE from the create endpoint. Embeds the freshly-minted
 *  raw token + the public accept URL — the server keeps only the hash, so
 *  the UI must surface this immediately for the user to copy / send. */


export type ApiInvitationPreviewStatus = Schemas['InvitationPreview']['status']


export type ShippingFeatureStatus = Schemas['ShippingFeatureStatus']
export type ShippingVerificationStatus = Schemas['ShippingVerification']['status']


export const api = {
  health: () => http<{ ok: boolean; ts: number }>('/health'),
  me: () => http<{ id: string; name: string; kind: string }>('/me'),
  /** Full-page redirect into the provider's consent screen. Use
   *  `window.location.assign(api.authStartUrl('google'))` rather than
   *  fetch — the browser needs to do the actual navigation so the
   *  callback can land back on AUTH_DONE_URL with the session token. */
  authStartUrl: (provider: 'google' | 'github', opts?: { inviteToken?: string | null; returnUrl?: string | null }) => {
    const params = new URLSearchParams()
    if (opts?.returnUrl) params.set('return', opts.returnUrl)
    if (opts?.inviteToken) params.set('invite', opts.inviteToken)
    const qs = params.toString()
    return `${API}/auth/start/${provider}${qs ? `?${qs}` : ''}`
  },
  /** OAuth provider 探活(#284):未配 provider 在按钮层显性化 —— 点进
   *  /auth/start/<p> 吃裸 503 的老坑。栈未起时 fetch 直接网络错,调用方
   *  按"不可知"处理,不误报未配置。 */
  authProviders: () =>
    http<{ github: boolean; google: boolean }>('/auth/providers'),
  authLogout: () =>
    http<{ ok: boolean }>('/auth/logout', { method: 'POST' }),
  /** Permanently delete the signed-in user's account. Soft-deletes
   *  the user row + clears PII + invalidates every session + drops
   *  OAuth linkages. After this call returns 200, the local Bearer
   *  token is invalid — caller should `useAuth.clear()` immediately. */
  deleteAccount: () =>
    http<{ ok: boolean }>('/me/account', { method: 'DELETE' }),
  /** Native Sign in with Apple — POST the identity_token JWT obtained
   *  from the iOS-native ASAuthorization flow. Server verifies the JWT
   *  against Apple's JWKS, find-or-creates the user, and returns a
   *  fresh session token. */
  authAppleNative: (input: { identityToken: string; name?: string | null; inviteToken?: string | null }) =>
    http<{ token: string; user: { id: string; email: string; displayName: string }; companyId: string | null }>('/auth/apple/native', {
      method: 'POST',
      body: JSON.stringify({
        identityToken: input.identityToken,
        name: input.name ?? null,
        inviteToken: input.inviteToken ?? null,
      }),
    }),
  authMe: () =>
    http<MeResponse>('/auth/me'),
  /** sub2api-backed quota snapshot for the signed-in user. `configured`
   *  is false on deployments that don't run a sub2api gateway; `snapshot`
   *  is null when the user has no active subscription (e.g. provisioning
   *  hasn't completed yet). Both states render as "unavailable" in the
   *  Usage tab rather than as errors. */
  getQuota: () =>
    http<ApiQuotaResponse>('/me/quota'),
  listCompanies: () =>
    http<Array<{ id: string; name: string; slug: string; createdAt: string; role: string }>>('/companies'),
  listProjects: () => http<ApiProject[]>('/projects'),
  getShippingOverview: () => http<ShippingOverview>('/shipping/overview'),
  getShippingFeature: (id: string) => http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(id)}`),
  createShippingFeature: (input: {
    title: string; problem?: string; desiredOutcome?: string; contractSummary?: string;
    priority?: string; riskLevel?: string; releaseTarget?: string | null; builderIds?: string[];
    projectId?: string | null; conversationId?: string | null; documentId?: string | null; boardCardId?: string | null;
  }) => http<ShippingFeatureDetail>('/shipping/features', { method: 'POST', body: JSON.stringify(input) }),
  updateShippingFeature: (id: string, input: Partial<{
    title: string; problem: string; desiredOutcome: string; contractSummary: string;
    priority: string; riskLevel: string; releaseTarget: string | null; builderIds: string[];
    projectId: string | null; conversationId: string | null; documentId: string | null; boardCardId: string | null;
  }>) => http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(input) }),
  transitionShippingFeature: (id: string, status: ShippingFeatureStatus) =>
    http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(id)}/transition`, { method: 'POST', body: JSON.stringify({ status }) }),
  createShippingInvariant: (featureId: string, input: { title: string; description?: string; kind?: string; required?: boolean }) =>
    http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(featureId)}/invariants`, { method: 'POST', body: JSON.stringify(input) }),
  createShippingVerification: (featureId: string, input: {
    title: string; description?: string; method?: string; required?: boolean; invariantId?: string | null;
    ownerId?: string | null; builderIds?: string[]; dueAt?: string | null;
  }) => http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(featureId)}/verifications`, { method: 'POST', body: JSON.stringify(input) }),
  updateShippingVerification: (featureId: string, verificationId: string, input: Record<string, unknown>) =>
    http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(featureId)}/verifications/${encodeURIComponent(verificationId)}`, { method: 'PATCH', body: JSON.stringify(input) }),
  createShippingRelease: (featureId: string, input: {
    environment: string; version?: string; commitSha?: string; releaseNotes?: string; rollbackPlan?: string;
    knownGaps?: Array<Record<string, unknown>>; baseline?: Array<Record<string, unknown>>; readbackDueAt?: string | null;
  }) => http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(featureId)}/releases`, { method: 'POST', body: JSON.stringify(input) }),
  shippingReleaseAction: (featureId: string, releaseId: string, input: { action: string; evidence?: Array<Record<string, unknown>>; reason?: string }) =>
    http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(featureId)}/releases/${encodeURIComponent(releaseId)}/action`, { method: 'POST', body: JSON.stringify(input) }),
  createShippingFriction: (input: Record<string, unknown>) =>
    http<{ id: string }>('/shipping/friction', { method: 'POST', body: JSON.stringify(input) }),
  updateShippingFriction: (id: string, input: Record<string, unknown>) =>
    http<{ ok: boolean }>(`/shipping/friction/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(input) }),
  createShippingRegression: (featureId: string, input: Record<string, unknown>) =>
    http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(featureId)}/regressions`, { method: 'POST', body: JSON.stringify(input) }),
  updateShippingRegression: (featureId: string, regressionId: string, input: Record<string, unknown>) =>
    http<ShippingFeatureDetail>(`/shipping/features/${encodeURIComponent(featureId)}/regressions/${encodeURIComponent(regressionId)}`, { method: 'PATCH', body: JSON.stringify(input) }),
  createProject: (input: { name: string; description?: string; color?: string }) =>
    http<{ id: string; name: string; description: string; color: string | null; status: string }>('/projects', {
      method: 'POST', body: JSON.stringify(input),
    }),
  updateProject: (id: string, input: { name?: string; description?: string; color?: string | null }) =>
    http<{ ok: boolean }>(`/projects/${encodeURIComponent(id)}`, {
      method: 'PUT', body: JSON.stringify(input),
    }),
  archiveProject: (id: string, archive = true) =>
    http<{ ok: boolean; status: string }>(`/projects/${encodeURIComponent(id)}/archive`, {
      method: 'POST', body: JSON.stringify({ archive }),
    }),
  attachProject: (conversationId: string, projectId: string | null) =>
    http<{ ok: boolean; projectId: string | null }>(`/conversations/${encodeURIComponent(conversationId)}/project`, {
      method: 'POST', body: JSON.stringify({ projectId }),
    }),
  createCompany: (name: string) =>
    http<{ id: string; name: string; slug: string; role: string }>('/companies', {
      method: 'POST', body: JSON.stringify({ name }),
    }),
  /** Owner/admin-only: list every invitation (active + historical) for a
   *  company so the management UI can show recent activity. */
  listInvitations: (companyId: string) =>
    http<ApiInvitation[]>(`/companies/${encodeURIComponent(companyId)}/invitations`),
  /** Owner/admin-only: mint a fresh invite. Pass `email` for a single-use
   *  email-locked invite, omit (or set `multiUse: true`) for a shareable
   *  link. The returned `url` + `token` are the ONLY copy — the server
   *  stores just the hash. */
  createInvitation: (companyId: string, input: {
    email?: string | null
    role?: 'member' | 'admin'
    note?: string | null
    multiUse?: boolean
    maxUses?: number
    /** Ask the server to send the invitation email on the inviter's
     *  behalf. Ignored unless `email` is also set. Result reported back
     *  via `emailDelivery` on the response. */
    sendEmail?: boolean
  }) =>
    http<ApiInvitationWithToken>(`/companies/${encodeURIComponent(companyId)}/invitations`, {
      method: 'POST', body: JSON.stringify(input),
    }),
  /** Owner/admin-only: revoke an invitation by its id (= token hash). */
  revokeInvitation: (companyId: string, inviteId: string) =>
    http<{ ok: boolean; revoked: boolean }>(
      `/companies/${encodeURIComponent(companyId)}/invitations/${encodeURIComponent(inviteId)}`,
      { method: 'DELETE' },
    ),
  /** Public: preview an invitation by its raw token. Returns the company
   *  + inviter so the accept screen can show "<X> invited you to <Y>"
   *  before the visitor signs in. When the caller IS signed in, the status
   *  also reflects `already_member` / `wrong_email`. */
  previewInvitation: (token: string) =>
    http<ApiInvitationPreview>(`/invitations/${encodeURIComponent(token)}`),
  /** Auth required: redeem an invitation. Joins the caller to the target
   *  company + adds them as a participant + posts "X joined" to #all-hands.
   *  Idempotent — calling twice with the same valid token returns
   *  `alreadyMember: true` on the second hit instead of incrementing
   *  use_count. */
  acceptInvitation: (token: string) =>
    http<ApiInvitationAccept>(`/invitations/${encodeURIComponent(token)}/accept`, {
      method: 'POST', body: JSON.stringify({}),
    }),
  getParticipants: () => http<ApiParticipant[]>('/participants'),

  // ─── Computers (agent hosts: Cumora Cloud + BYOA) ───
  getComputers: () => http<ApiComputer[]>('/computers'),
  /** Start pairing a BYOA computer: returns a persistent token for the daemon.
   *  No computer is created until the daemon pairs and reports the machine's
   *  real hostname, so the UI just shows the command. */
  requestPairingCode: () =>
    http<{ code: string; expiresInSeconds: number | null }>(
      '/computers', { method: 'POST', body: '{}' }),
  /** Revoke a paired computer (its device token + agent JWTs stop working). */
  deleteComputer: (id: string) =>
    http<{ ok: boolean }>(`/computers/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  /** Get a re-pair code to reconnect an existing computer (keeps its agents). */
  repairComputer: (id: string) =>
    http<{ code: string; expiresInSeconds: number | null }>(
      `/computers/${encodeURIComponent(id)}/repair`, { method: 'POST', body: '{}' }),
  /** Move an agent to a computer, choosing its engine. */
  assignAgentComputer: (agentId: string, computerId: string, engine?: EngineId) =>
    http<{ ok: boolean; kind: ComputerKind; engine: EngineId }>(
      `/agents/${encodeURIComponent(agentId)}/computer`,
      { method: 'POST', body: JSON.stringify({ computerId, engine }) }),
  createAgent: (input: AgentInput) =>
    http<{ id: string }>('/agents', { method: 'POST', body: JSON.stringify(input) }),
  updateAgent: (id: string, input: AgentInput) =>
    http<{ ok: boolean }>(`/agents/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(input) }),
  /** Soft-delete: marks the agent as off-boarded. Memory + log preserved. */
  offboardAgent: (id: string) =>
    http<{ ok: boolean; departedAt: string }>(`/agents/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  rehireAgent: (id: string) =>
    http<{ ok: boolean }>(`/agents/${encodeURIComponent(id)}/rehire`, { method: 'POST' }),
  generateAgentAvatar: (id: string) =>
    http<{ url: string }>(`/agents/${encodeURIComponent(id)}/avatar/generate`, { method: 'POST' }),
  getConversations: () => http<ApiConversation[]>('/conversations'),
  createGroup: (input: { title: string; members: string[]; subtitle?: string; projectId?: string | null }) =>
    http<{ id: string; members: string[]; projectId: string | null }>('/conversations', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  leaveConversation: (conversationId: string) =>
    http<{ ok: boolean; members: string[] }>(`/conversations/${encodeURIComponent(conversationId)}/leave`, {
      method: 'POST',
    }),
  openDirect: (otherId: string) =>
    http<{ id: string; created: boolean }>('/conversations/direct', {
      method: 'POST',
      body: JSON.stringify({ otherId }),
    }),
  setTopic: (conversationId: string, topic: string | null) =>
    http<{ ok: boolean; topic: string | null }>(`/conversations/${encodeURIComponent(conversationId)}/topic`, {
      method: 'POST',
      body: JSON.stringify({ topic }),
    }),
  setTitle: (conversationId: string, title: string) =>
    http<{ ok: boolean; title: string }>(`/conversations/${encodeURIComponent(conversationId)}/title`, {
      method: 'POST',
      body: JSON.stringify({ title }),
    }),
  togglePin: (conversationId: string, pinned?: boolean) =>
    http<{ ok: boolean; pinned: boolean }>(`/conversations/${encodeURIComponent(conversationId)}/pin`, {
      method: 'POST',
      body: JSON.stringify(pinned === undefined ? {} : { pinned }),
    }),
  /**
   * Mute/unmute. Pass `mute: false` to unmute; `mute: true` with an optional
   * `until` ISO string for a finite mute window (omit for "forever"). The
   * server validates `until` and rejects past timestamps.
   */
  /**
   * Register a push-notification device token. Called from src/lib/push.ts
   * after the user grants permission and Capacitor's `registration` event
   * fires with the APNs/FCM token. Idempotent server-side (upsert on
   * platform+token).
   */
  registerPushDevice: (input: {
    platform: 'ios' | 'android' | 'web'
    token: string
    appVersion?: string
    deviceModel?: string
  }) =>
    http<{ ok: boolean }>(`/push/register`, {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  /** Best-effort sign-out housekeeping. Soft-disables the token row so
   *  the server stops sending APNs but keeps the audit trail. */
  unregisterPushDevice: (input: { token: string }) =>
    http<{ ok: boolean }>(`/push/unregister`, {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  setMute: (conversationId: string, mute: boolean, until?: string | null) =>
    http<{ ok: boolean; muted: boolean; mutedUntil: string | null }>(
      `/conversations/${encodeURIComponent(conversationId)}/mute`,
      {
        method: 'POST',
        body: JSON.stringify({ mute, until: until ?? null }),
      },
    ),
  addMember: (conversationId: string, participantId: string) =>
    http<{ ok: boolean; members: string[]; alreadyIn?: boolean }>(`/conversations/${encodeURIComponent(conversationId)}/members`, {
      method: 'POST',
      body: JSON.stringify({ id: participantId }),
    }),
  getMessages: (
    conversationId: string,
    opts?: { before?: number; limit?: number },
  ) => {
    const qs = new URLSearchParams()
    if (opts?.before !== undefined) qs.set('before', String(opts.before))
    if (opts?.limit !== undefined) qs.set('limit', String(opts.limit))
    const q = qs.toString()
    return http<ApiMessage[]>(
      `/conversations/${encodeURIComponent(conversationId)}/messages${q ? `?${q}` : ''}`,
    )
  },
  /** All direct replies to a root message (i.e. messages whose quoted_message_id
   *  equals rootId). Used by the thread drawer. */
  getReplies: (conversationId: string, rootId: string) =>
    http<ApiMessage[]>(
      `/conversations/${encodeURIComponent(conversationId)}/messages/${encodeURIComponent(rootId)}/replies`,
    ),
  sendMessage: (
    conversationId: string,
    body: string,
    attachment?: ApiAttachment | null,
    quotedMessageId?: string | null,
    /** Optional client-supplied idempotency key (the optimistic bubble's
     *  tempId). The server persists it and returns the original message when
     *  the same send is retried. */
    clientId?: string | null,
  ) =>
    http<{ id: string; sequence: number }>(`/conversations/${encodeURIComponent(conversationId)}/messages`, {
      method: 'POST',
      body: JSON.stringify({
        body,
        attachment: attachment ?? undefined,
        quotedMessageId: quotedMessageId ?? undefined,
        clientId: clientId ?? undefined,
      }),
    }),
  /* ============== Polls ====================================================
   * createPoll → POST /api/polls (server creates a kind='poll' message + broadcasts).
   * castPollVote → POST /api/polls/:id/vote (replaces caller's existing picks).
   * closePoll  → POST /api/polls/:id/close (only the original author). */
  createPoll: (args: {
    conversationId: string
    question: string
    mode: 'single' | 'multi'
    options: string[]
    /** Minutes until the poll auto-closes. null / undefined ⇒ no expiration. */
    expiresInMinutes?: number | null
  }) =>
    http<{ messageId: string; sequence: number; poll: import('../types.js').PollPayload }>(
      '/polls',
      { method: 'POST', body: JSON.stringify(args) },
    ),
  castPollVote: (messageId: string, optionIds: string[]) =>
    http<{ tallies: import('../types.js').PollTally[]; poll: import('../types.js').PollPayload }>(
      `/polls/${encodeURIComponent(messageId)}/vote`,
      { method: 'POST', body: JSON.stringify({ optionIds }) },
    ),
  closePoll: (messageId: string) =>
    http<{ closed: boolean; poll: import('../types.js').PollPayload | null }>(
      `/polls/${encodeURIComponent(messageId)}/close`,
      { method: 'POST' },
    ),
  /** Send a brand-new email thread. Recipients are addresses or
   *  in-tenant participant ids; the server resolves either. `attachments`
   *  references files already uploaded via `api.uploadFile` (the same
   *  upload path chat attachments use) — we hand over the storage key so
   *  Resend can fetch the URL server-side. Returns the resulting
   *  messages.id + the thread's conversations.id so the caller can
   *  navigate to it on success. */
  sendEmail: (args: {
    to: string[]
    cc?: string[]
    subject: string
    body: string
    attachments?: Array<{ key: string; filename: string; mimeType: string; sizeBytes: number }>
  }) =>
    http<{ messageId: string; conversationId: string; transportStatus: string; mock?: boolean; error?: string | null }>(
      '/email/send',
      { method: 'POST', body: JSON.stringify(args) },
    ),
  /** Reply to an existing email message. Headers (subject Re:, In-Reply-To,
   *  References, recipients) are derived server-side from the original. */
  replyEmail: (messageId: string, args: {
    body: string
    cc?: string[]
    attachments?: Array<{ key: string; filename: string; mimeType: string; sizeBytes: number }>
  }) =>
    http<{ messageId: string; conversationId: string; transportStatus: string; mock?: boolean; error?: string | null }>(
      `/email/reply/${encodeURIComponent(messageId)}`,
      { method: 'POST', body: JSON.stringify(args) },
    ),
  /** Fetch the server-sanitized HTML body for an email message. Returns
   *  null when the row has no HTML part (text-only mail) — the renderer
   *  treats that as "nothing to show" rather than an error. */
  fetchEmailHtml: async (messageId: string): Promise<string | null> => {
    const headers: Record<string, string> = {}
    const token = getAuthToken()
    if (token) headers.authorization = `Bearer ${token}`
    const company = getActiveCompanyId()
    if (company) headers['x-company-id'] = company
    if (getDevModeEnabled()) headers['x-cumora-dev-mode'] = '1'
    const res = await fetch(`${API}/email/${encodeURIComponent(messageId)}/html`, { headers })
    if (res.status === 204) return null
    if (!res.ok) {
      const text = await res.text().catch(() => '')
      throw new Error(text || `${res.status} ${res.statusText}`)
    }
    return res.text()
  },
  /** Fetch upload-system capabilities. Cached on the client so repeat
   *  uploads in the same session don't re-probe the server. */
  uploadCapabilities: (() => {
    let cache: Promise<UploadCapabilities> | null = null
    return (): Promise<UploadCapabilities> => {
      // Cache the SUCCESSFUL probe only. If the request rejects (network
      // blip at boot, a transient 502, offline-then-online), drop the
      // cached promise so the next upload re-probes — otherwise a single
      // early failure poisons the cache and every subsequent image select
      // instantly throws "Failed to fetch" without ever hitting the wire,
      // until a full page reload.
      if (!cache) {
        cache = http<UploadCapabilities>('/uploads/capabilities').catch((err) => {
          cache = null
          throw err
        })
      }
      return cache
    }
  })(),
  /**
   * Upload a file using the best available path:
   *   - R2 mode → presign + direct browser PUT to R2 (no base64 round-trip,
   *               no server CPU spent decoding, big files work fine)
   *   - local   → POST /uploads with base64 body (only practical option
   *               when there's no presigned-PUT endpoint)
   *
   * Returns an ApiAttachment ready to drop into `sendMessage`.
   */
  uploadFile: async (file: File): Promise<ApiAttachment> => {
    const caps = await api.uploadCapabilities()
    if (caps.maxBytes && file.size > caps.maxBytes) {
      throw new Error(`file too large: ${Math.round(file.size / 1024 / 1024)}MB (max ${Math.round(caps.maxBytes / 1024 / 1024)}MB)`)
    }
    const mime = file.type || 'application/octet-stream'
    if (caps.allowedMimes.length && !caps.allowedMimes.includes(mime)) {
      throw new Error(`file type not allowed: ${mime}`)
    }

    if (caps.presignSupported) {
      // Step 1 — ask the server for a presigned PUT URL.
      const signed = await http<PresignResponse>('/uploads/presign', {
        method: 'POST',
        body: JSON.stringify({ name: file.name, mime, size: file.size }),
      })
      // Step 2 — PUT the raw bytes directly to R2. No auth header; the
      // presigned URL carries everything the bucket needs.
      const r = await fetch(signed.uploadUrl, {
        method: 'PUT',
        headers: { 'Content-Type': mime },
        body: file,
      })
      if (!r.ok) {
        const text = await r.text().catch(() => '')
        throw new Error(`R2 PUT failed: ${r.status} ${text.slice(0, 200)}`)
      }
      return {
        url: signed.publicUrl,
        key: signed.key,
        name: signed.name,
        mime: signed.mime,
        size: signed.size,
        kind: signed.kind === 'img' ? 'img' : 'file',
      }
    }

    // Local-storage fallback — base64 through the server.
    const buf = await file.arrayBuffer()
    const bytes = new Uint8Array(buf)
    let binary = ''
    const chunk = 0x8000
    for (let i = 0; i < bytes.length; i += chunk) {
      binary += String.fromCharCode(...bytes.subarray(i, i + chunk))
    }
    const dataBase64 = btoa(binary)
    return http<ApiAttachment>('/uploads', {
      method: 'POST',
      body: JSON.stringify({ name: file.name, mime, dataBase64 }),
    })
  },
  refreshUploadUrl: (input: string | { url?: string; key?: string }) =>
    http<{ key: string; url: string }>('/uploads/refresh-url', {
      method: 'POST',
      body: JSON.stringify(typeof input === 'string' ? { url: input } : input),
    }),
  markRead: (conversationId: string) =>
    http<{ ok: boolean }>(`/conversations/${encodeURIComponent(conversationId)}/read`, {
      method: 'POST',
      body: JSON.stringify({}),
    }),
  /** Broadcast a typing indicator into a conversation. Callers should
   *  throttle to roughly one POST every few seconds while typing
   *  continues, then send a final `done:true` when the composer goes
   *  idle, blurs, or sends. */
  emitTyping: (conversationId: string, done: boolean) =>
    http<{ ok: boolean }>(`/conversations/${encodeURIComponent(conversationId)}/typing`, {
      method: 'POST',
      body: JSON.stringify({ done }),
    }),
  toggleReaction: (messageId: string, emoji: string) =>
    http<{ reactions: Array<{ emoji: string; count: number; mine?: boolean; users?: string[] }> }>(
      `/messages/${encodeURIComponent(messageId)}/reactions`,
      { method: 'POST', body: JSON.stringify({ emoji }) },
    ),
  /** "Whispers" is a frontend tab name — on the server these are just
   *  agent-to-agent direct conversations the user can peek at. */
  getWhispers: () => http<ApiWhisper[]>('/peek/agent-chats'),
  getWhisperMessages: (id: string) =>
    http<ApiWhisperMessage[]>(`/peek/agent-chats/${encodeURIComponent(id)}/messages`),
  startConvene: (conversationId: string, topic: string) =>
    http<ApiConveneSession>(`/conversations/${encodeURIComponent(conversationId)}/convene`, {
      method: 'POST',
      body: JSON.stringify({ topic }),
    }),
  getActiveConvene: (conversationId: string) =>
    http<ApiConveneSession | null>(`/conversations/${encodeURIComponent(conversationId)}/convene`),
  getConveneTranscript: (sessionId: string) =>
    http<ApiConveneTranscript[]>(`/convene/${encodeURIComponent(sessionId)}/transcript`),
  getPreferences: () => http<Record<string, unknown>>('/me/preferences'),
  putPreferences: (prefs: Record<string, unknown>) =>
    http<{ ok: boolean }>('/me/preferences', { method: 'PUT', body: JSON.stringify(prefs) }),
  getAllAutonomy: () => http<ApiAutonomy[]>('/agents/autonomy'),
  putAutonomy: (agentId: string, threshold: number) =>
    http<{ ok: boolean; threshold: number }>(`/agents/${encodeURIComponent(agentId)}/autonomy`, {
      method: 'PUT',
      body: JSON.stringify({ threshold }),
    }),
  getAgentRuns: (filters?: { agentId?: string | null; status?: ApiAgentRunStatus | 'all'; limit?: number }) => {
    const q = new URLSearchParams()
    if (filters?.agentId) q.set('agentId', filters.agentId)
    if (filters?.status && filters.status !== 'all') q.set('status', filters.status)
    if (filters?.limit) q.set('limit', String(filters.limit))
    const suffix = q.toString() ? `?${q.toString()}` : ''
    return http<ApiAgentRun[]>(`/agents/observability/runs${suffix}`)
  },
  getAgentRunEvents: (runId: string) =>
    http<ApiAgentEvent[]>(`/agents/observability/runs/${encodeURIComponent(runId)}/events`),
  // #260 工具级转录回放(sinceSeq 分页;UI 一次拉全量即停)。
  getAgentRunTranscript: (runId: string, sinceSeq = 0) =>
    http<ApiTranscriptEntry[]>(`/agents/observability/runs/${encodeURIComponent(runId)}/transcript?sinceSeq=${sinceSeq}&limit=1000`),

  // #264 人侧 Inbox 分级:列表/已读/静音。
  getInbox: () => http<ApiInboxResponse>('/inbox'),
  markInboxItemRead: (id: string) =>
    http<{ ok: boolean }>(`/inbox/${encodeURIComponent(id)}/read`, { method: 'POST' }),
  markAllInboxRead: () =>
    http<{ ok: boolean }>('/inbox/read-all', { method: 'POST' }),
  getInboxMutes: () => http<{ types: string[] }>('/inbox/mutes'),
  setInboxMutes: (types: string[]) =>
    http<{ ok: boolean }>('/inbox/mutes', { method: 'PUT', body: JSON.stringify({ types }) }),

  // #261 公司 Skills 库(SOP 手册):管理页 CRUD。写面 owner/admin
  // (服务端 privileged 门);body 便捷位 = 服务端组装单文件 SKILL.md。
  listCompanySkills: () => http<{ skills: ApiCompanySkill[] }>('/skills'),
  getCompanySkill: (id: string) =>
    http<ApiCompanySkillDetail>(`/skills/${encodeURIComponent(id)}`),
  createCompanySkill: (input: { name: string; description: string; body: string }) =>
    http<{ id: string; bundleHash: string }>('/skills', { method: 'POST', body: JSON.stringify(input) }),
  updateCompanySkill: (id: string, input: { description?: string; body?: string }) =>
    http<{ id: string; bundleHash: string }>(`/skills/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(input) }),
  deleteCompanySkill: (id: string) =>
    http<{ ok: boolean }>(`/skills/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  getTriageEconomics: (filters?: { agentId?: string | null; sinceHours?: number }) => {
    const q = new URLSearchParams()
    if (filters?.agentId) q.set('agentId', filters.agentId)
    if (filters?.sinceHours) q.set('sinceHours', String(filters.sinceHours))
    const suffix = q.toString() ? `?${q.toString()}` : ''
    return http<ApiTriageEconomics>(`/agents/observability/triage${suffix}`)
  },
  getDevtoolsCapabilities: () => http<ApiDevtoolsCapabilities>('/devtools/capabilities'),
  listAgentWorkspace: (agentId: string) =>
    http<ApiAgentWorkspaceFile[]>(`/devtools/agent-workspace?agentId=${encodeURIComponent(agentId)}`),
  readAgentWorkspaceFile: (agentId: string, path: string) =>
    http<ApiAgentWorkspaceFileContent>(
      `/devtools/agent-workspace/file?agentId=${encodeURIComponent(agentId)}&path=${encodeURIComponent(path)}`,
    ),
  search: (q: string, signal?: AbortSignal) =>
    http<ApiSearchResults>(`/search?q=${encodeURIComponent(q)}`, { signal }),

  /* ============== Kanban boards ============== */
  listBoards: () => http<BoardSummary[]>('/boards'),
  getBoard: (id: string) => http<BoardSnapshot>(`/boards/${encodeURIComponent(id)}`),
  getBoardCard: (id: string) => http<BoardCardLookup>(`/cards/${encodeURIComponent(id)}`),
  createBoard: (input: { title: string; description?: string }) =>
    http<{ id: string }>('/boards', { method: 'POST', body: JSON.stringify(input) }),
  updateBoard: (id: string, input: { title?: string; description?: string }) =>
    http<{ ok: boolean }>(`/boards/${encodeURIComponent(id)}`, {
      method: 'PATCH', body: JSON.stringify(input),
    }),
  deleteBoard: (id: string) =>
    http<{ ok: boolean }>(`/boards/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  addBoardColumn: (boardId: string, title: string) =>
    http<{ id: string; position: number }>(
      `/boards/${encodeURIComponent(boardId)}/columns`,
      { method: 'POST', body: JSON.stringify({ title }) },
    ),
  updateBoardColumn: (boardId: string, columnId: string, input: { title?: string; position?: number }) =>
    http<{ ok: boolean }>(
      `/boards/${encodeURIComponent(boardId)}/columns/${encodeURIComponent(columnId)}`,
      { method: 'PATCH', body: JSON.stringify(input) },
    ),
  deleteBoardColumn: (boardId: string, columnId: string) =>
    http<{ ok: boolean }>(
      `/boards/${encodeURIComponent(boardId)}/columns/${encodeURIComponent(columnId)}`,
      { method: 'DELETE' },
    ),
  createCard: (boardId: string, input: {
    columnId: string; title: string; description?: string; assigneeId?: string | null
  }) =>
    http<{ id: string; position: number; mentions: string[] }>(
      `/boards/${encodeURIComponent(boardId)}/cards`,
      { method: 'POST', body: JSON.stringify(input) },
    ),
  updateCard: (boardId: string, cardId: string, input: {
    title?: string; description?: string; position?: number
    columnId?: string; assigneeId?: string | null
  }) =>
    http<{ ok: boolean; mentions?: string[] }>(
      `/boards/${encodeURIComponent(boardId)}/cards/${encodeURIComponent(cardId)}`,
      { method: 'PATCH', body: JSON.stringify(input) },
    ),
  deleteCard: (boardId: string, cardId: string) =>
    http<{ ok: boolean }>(
      `/boards/${encodeURIComponent(boardId)}/cards/${encodeURIComponent(cardId)}`,
      { method: 'DELETE' },
    ),
  listCardComments: (boardId: string, cardId: string) =>
    http<BoardCardComment[]>(
      `/boards/${encodeURIComponent(boardId)}/cards/${encodeURIComponent(cardId)}/comments`,
    ),
  addCardComment: (boardId: string, cardId: string, body: string) =>
    http<{ id: string; mentions: string[] }>(
      `/boards/${encodeURIComponent(boardId)}/cards/${encodeURIComponent(cardId)}/comments`,
      { method: 'POST', body: JSON.stringify({ body }) },
    ),
  deleteCardComment: (boardId: string, cardId: string, commentId: string) =>
    http<{ ok: boolean }>(
      `/boards/${encodeURIComponent(boardId)}/cards/${encodeURIComponent(cardId)}/comments/${encodeURIComponent(commentId)}`,
      { method: 'DELETE' },
    ),

  /* ============== Calendar ==============
   * Shared schedule used by both humans and agents. Events are scoped to the
   * active company; the server-side scheduler fires agent_task events at
   * their start time (+ recurrence) by posting a typed Calendar system dispatch
   * into the target conversation. */
  listCalendarEvents: (range?: { from?: string; to?: string }) => {
    const params = new URLSearchParams()
    if (range?.from) params.set('from', range.from)
    if (range?.to) params.set('to', range.to)
    const qs = params.toString()
    return http<{ events: CalendarEvent[] }>(`/calendar/events${qs ? `?${qs}` : ''}`)
  },
  getCalendarEvent: (id: string) =>
    http<{ event: CalendarEvent }>(`/calendar/events/${encodeURIComponent(id)}`),
  createCalendarEvent: (input: CalendarEventInput) =>
    http<{ event: CalendarEvent }>('/calendar/events', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  updateCalendarEvent: (id: string, patch: Partial<CalendarEventInput> & { status?: CalendarEventStatus }) =>
    http<{ event: CalendarEvent }>(`/calendar/events/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      body: JSON.stringify(patch),
    }),
  deleteCalendarEvent: (id: string) =>
    http<{ ok: boolean }>(`/calendar/events/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  runCalendarEventNow: (id: string) =>
    http<{ status: string; messageId?: string; conversationId?: string; error?: string }>(
      `/calendar/events/${encodeURIComponent(id)}/run-now`,
      { method: 'POST' },
    ),
  listCalendarDispatches: (id: string) =>
    http<{ dispatches: CalendarDispatch[] }>(
      `/calendar/events/${encodeURIComponent(id)}/dispatches`,
    ),
  /* ============== Collaborative documents (CRDT) ============== */
  listDocuments: () =>
    http<{ documents: ApiDocument[] }>('/documents'),
  createDocument: (input: { title?: string; conversationId?: string | null } = {}) =>
    http<ApiDocument>('/documents', { method: 'POST', body: JSON.stringify(input) }),
  getDocument: (id: string) =>
    http<ApiDocument>(`/documents/${encodeURIComponent(id)}`),
  renameDocument: (id: string, title: string) =>
    http<{ ok: boolean; title: string }>(`/documents/${encodeURIComponent(id)}`, {
      method: 'PUT', body: JSON.stringify({ title }),
    }),
  deleteDocument: (id: string) =>
    http<{ ok: boolean }>(`/documents/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  /* ============== Team workspaces (shared real folders) ============== */
  listWorkspaces: () =>
    http<ApiWorkspaceSummary[]>('/workspaces'),
  getWorkspace: (id: string) =>
    http<ApiWorkspaceDetail>(`/workspaces/${encodeURIComponent(id)}`),
  listWorkspaceFiles: (id: string, path: string) =>
    http<{ path: string; entries: ApiWorkspaceFileEntry[] }>(
      `/workspaces/${encodeURIComponent(id)}/files?path=${encodeURIComponent(path)}`,
    ),
  readWorkspaceFile: (id: string, path: string) =>
    http<{ path: string; body: string; size: number; modifiedAt: string }>(
      `/workspaces/${encodeURIComponent(id)}/file?path=${encodeURIComponent(path)}`,
    ),
  writeWorkspaceFile: (id: string, path: string, body: string) =>
    http<{ ok: boolean; path: string }>(
      `/workspaces/${encodeURIComponent(id)}/file?path=${encodeURIComponent(path)}`,
      { method: 'PUT', body: JSON.stringify({ body }) },
    ),

  /* #338 管理面 mutation(服务端/契约已就绪,补齐前端封装) */
  createWorkspace: (name: string, folderPath: string) =>
    http<ApiWorkspaceSummary>('/workspaces', { method: 'POST', body: JSON.stringify({ name, folderPath }) }),
  addWorkspaceMember: (id: string, participantId: string) =>
    http<{ ok: boolean }>(`/workspaces/${encodeURIComponent(id)}/members`, {
      method: 'POST', body: JSON.stringify({ participantId }),
    }),
  removeWorkspaceMember: (id: string, participantId: string) =>
    http<{ ok: boolean }>(`/workspaces/${encodeURIComponent(id)}/members/${encodeURIComponent(participantId)}`, {
      method: 'DELETE',
    }),
  addWorkspaceAssociation: (id: string, kind: 'project' | 'board_card' | 'document', targetId: string) =>
    http<{ ok: boolean; kind: string; targetId: string }>(`/workspaces/${encodeURIComponent(id)}/associations`, {
      method: 'POST', body: JSON.stringify({ kind, targetId }),
    }),
  removeWorkspaceAssociation: (id: string, kind: 'project' | 'board_card' | 'document', targetId: string) =>
    http<{ ok: boolean }>(`/workspaces/${encodeURIComponent(id)}/associations/${kind}/${encodeURIComponent(targetId)}`, {
      method: 'DELETE',
    }),
  unbindWorkspace: (id: string) =>
    http<{ ok: boolean; unboundAt: string }>(`/workspaces/${encodeURIComponent(id)}/unbind`, { method: 'POST' }),

  /* #338 multipart 上传 / 原始字节读:fetchJson 恒 JSON(core.ts),此二
  面必须裸 fetch(带 Bearer + x-company-id;FormData 不设 content-type,
  让浏览器填 multipart boundary)。错误形状对齐 ApiError。 */
  uploadWorkspaceFile: async (id: string, path: string, file: File) => {
    const form = new FormData()
    form.append('path', path)
    form.append('file', file)
    const headers: Record<string, string> = {}
    const token = getAuthToken()
    if (token) headers.authorization = `Bearer ${token}`
    const company = getActiveCompanyId()
    if (company) headers['x-company-id'] = company
    const res = await fetch(`${SERVER_ORIGIN}/api/workspaces/${encodeURIComponent(id)}/upload`, {
      method: 'POST', headers, body: form,
    })
    if (!res.ok) {
      let detail = `${res.status} ${res.statusText}`
      try {
        const j = (await res.json()) as { error?: string }
        if (j.error) detail = `${j.error} (${res.status})`
      } catch { /* keep status text */ }
      throw new ApiError(detail, res.status)
    }
    return (await res.json()) as { ok: boolean; path: string; size: number; mtimeNanos: string }
  },
  fetchWorkspaceRaw: async (id: string, path: string): Promise<Blob> => {
    const headers: Record<string, string> = {}
    const token = getAuthToken()
    if (token) headers.authorization = `Bearer ${token}`
    const company = getActiveCompanyId()
    if (company) headers['x-company-id'] = company
    const res = await fetch(`${SERVER_ORIGIN}/api/workspaces/${encodeURIComponent(id)}/raw?path=${encodeURIComponent(path)}`, { headers })
    if (!res.ok) throw new ApiError(`${res.status} ${res.statusText}`, res.status)
    return await res.blob()
  },
}


/** Body shape for create/update calendar event. The server validates each
 *  field independently so partial updates work. */

/* ============== WebSocket bridge ============== */

/* #221:WS 事件联合类型退役手写 —— 契约生成(packages/contract/ws-events.json
 * → npm run contract:gen 出 @cumora/contract/ws-events)。新增/修改事件改契
 * 约一处,本文件的 WsEvent 随再生自动跟上;CI 手写漂移守卫(contract-check.sh)
 * 拦回到此处内联事件形状的写法。逐事件载荷语义(时间键 at 而非 createdAt、
 * agent ISOms / 用户 RFC3339Nano 双格式、Go 消息 id 随机串)见契约文件。 */
export type WsEvent = WsServerEvent

type Listener = (e: WsEvent) => void

class WsClient {
  private ws: WebSocket | null = null
  private listeners = new Set<Listener>()
  private reconnectDelay = 500
  private intentionalClose = false
  /** In-flight `connect()` de-dupe. The `this.ws` guard below cannot catch a
   *  second caller: `this.ws` is not assigned until AFTER the ticket fetch
   *  awaits, and boot fires five connects in the same tick
   *  (bootMessagesStream / bootParticipants / bootConversations /
   *  bootWhispers / bootComputers, each behind its own module-local `wsBound`
   *  flag, so none of them suppresses another). Every socket they opened fanned
   *  into this same `listeners` set, and since `message.delta` is applied by
   *  ACCUMULATING onto the body, the streaming bubble rendered each chunk five
   *  times while an agent typed. Concurrent callers ride the first attempt; the
   *  memo clears once it settles, so a later connect still gets a fresh socket. */
  private connecting: Promise<void> | null = null

  connect(): Promise<void> {
    const existing = this.connecting
    if (existing) return existing
    const p = this.connectImpl().finally(() => { this.connecting = null })
    this.connecting = p
    return p
  }

  private async connectImpl() {
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return
    const token = getAuthToken()
    if (!token) return  // not signed in → don't even try
    // Fetch a SHORT-LIVED one-shot ticket so we never put the actual
    // session token on the WS URL (which would land in proxy access logs
    // / referrer headers). The ticket is consumed atomically server-side.
    let ticket: string
    try {
      const r = await fetch(`${API}/auth/ws-ticket`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: `Bearer ${token}` },
      })
      if (!r.ok) {
        // Schedule a retry — auth might be in flight, server bouncing, etc.
        this.scheduleReconnect()
        return
      }
      const j = await r.json() as { ticket: string }
      ticket = j.ticket
    } catch {
      this.scheduleReconnect()
      return
    }
    const url = `${wsOrigin()}/ws?t=${encodeURIComponent(ticket)}`
    const sock = new WebSocket(url)
    this.ws = sock
    sock.onopen = () => { this.reconnectDelay = 500 }
    sock.onmessage = (ev) => {
      try {
        const data = JSON.parse(ev.data) as WsEvent
        this.listeners.forEach((l) => { l(data) })
      } catch { /* ignore */ }
    }
    sock.onclose = () => {
      // Only the socket that is still CURRENT may clear the field. `reconnect()`
      // closes the old socket and opens its replacement immediately, but the
      // close event lands a tick later — nulling `this.ws` then would orphan a
      // LIVE socket (`isOpen()`/`send()` start reporting closed while typing
      // frames are silently dropped) and schedule a second one on top of it,
      // putting us back to two sockets sharing one listener set.
      if (this.ws !== sock) return
      this.ws = null
      if (!this.intentionalClose) this.scheduleReconnect()
    }
    sock.onerror = () => { /* onclose follows */ }
  }

  private scheduleReconnect() {
    const d = this.reconnectDelay
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, 8000)
    setTimeout(() => { void this.connect() }, d)
  }

  on(l: Listener): () => void {
    this.listeners.add(l)
    return () => this.listeners.delete(l)
  }

  /** Send a JSON frame upstream. Drops silently if the socket isn't open —
   *  callers should re-emit on `hello` reconnect rather than queuing. */
  send(payload: unknown): boolean {
    const sock = this.ws
    if (!sock || sock.readyState !== WebSocket.OPEN) return false
    try { sock.send(JSON.stringify(payload)); return true } catch { return false }
  }

  isOpen(): boolean {
    return !!this.ws && this.ws.readyState === WebSocket.OPEN
  }

  close() {
    this.intentionalClose = true
    this.ws?.close()
  }

  /** Force a fresh ticket fetch + reconnect. Used when the auth context
   *  changes (login, logout, company switch) so the socket re-handshakes
   *  with the new identity instead of staying on the old session. */
  reconnect() {
    this.intentionalClose = true
    this.ws?.close()
    this.ws = null
    this.intentionalClose = false
    this.reconnectDelay = 500
    void this.connect()
  }
}

export const ws = new WsClient()
