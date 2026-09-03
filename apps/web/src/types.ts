// 类型分型(ADR 0004 · #48):
//  - wire 形状 = packages/contract 契约生成(单一事实源);
//  - 客户端视图模型(Computer/Conversation/Message 的 store 侧形态)手写,
//    由各 store 的 fromApi() 从 wire 形状映射而来——它们不是 API 类型。
import type { components } from '@cumora/contract'

type Schemas = components['schemas']

// ── wire(生成)──
export type ComputerKind = Schemas['ComputerKind']
export type ComputerStatus = Schemas['ComputerStatus']
export type EngineId = Schemas['EngineId']
export type ParticipantKind = Schemas['ParticipantKind']
export type Status = Schemas['Status']
export type ConversationKind = Schemas['ConversationKind']
export type MessageKind = Schemas['MessageKind']
export type Participant = Schemas['Participant']
export type PollOption = Schemas['PollOption']
export type PollPayload = Schemas['PollPayload']
export type PollTally = Schemas['PollTally']
export type EmailAttachment = Schemas['EmailAttachment']
export type EmailFields = Schemas['EmailFields']
export type ReactionEntry = Schemas['ReactionEntry']
export type QuotedSummary = Schemas['QuotedSummary']
export type MessageTool = Schemas['MessageTool']
export type MessageAttachment = Schemas['MessageAttachment']
export type RecurrenceRule = Schemas['RecurrenceRule']
export type CalendarEventKind = Schemas['CalendarEventKind']
export type CalendarEventStatus = Schemas['CalendarEventStatus']
export type CalendarReminderChannel = Schemas['CalendarReminderChannel']
export type CalendarEvent = Schemas['CalendarEvent']
export type CalendarDispatch = Schemas['CalendarDispatch']
export type BoardSummary = Schemas['BoardSummary']
export type BoardColumn = Schemas['BoardColumn']
export type BoardCard = Schemas['BoardCard']
export type BoardCardComment = Schemas['BoardCardComment']
export type BoardSnapshot = Schemas['BoardSnapshot']
export type BoardCardLookup = Schemas['BoardCardLookup']

// ── 客户端视图模型(store 侧;非 wire)──

/** store 侧 Computer:线上 snake_case 的 camel 化(见 stores/computers.ts fromApi)。 */
export interface Computer {
  id: string
  name: string
  kind: ComputerKind
  status: ComputerStatus
  availableEngines: EngineId[]
  lastSeenAt?: string | null
  pairedAt?: string | null
  /** The cumora daemon version this computer is running (null = unknown). */
  daemonVersion?: string | null
  /** How the daemon runs: true = installed service (launchd/systemd),
   *  false = manually-run foreground command, null = unknown. */
  daemonSupervised?: boolean | null
  /** Newest published daemon version (for the upgrade banner). */
  latestDaemonVersion?: string | null
  /** True when the daemon is behind the latest version → show the upgrade banner. */
  daemonOutdated?: boolean
}

/** store 侧 Conversation:列表 wire 形状之上叠加排序/预览渲染字段。 */
export interface Conversation {
  id: string
  kind: ConversationKind
  title: string
  /** display subtitle - members or whisper pair */
  subtitle?: string
  /** free-form purpose / topic line, editable by any member */
  topic?: string | null
  /** participant ids */
  members: string[]
  /** for whispers: the two agents in private chat */
  whisperPair?: [string, string]
  pinned?: boolean
  /** Per-user mute. When true, the conversation suppresses notifications and is
   *  excluded from the global unread total (but its per-row badge still shows).
   *  Pair with `mutedUntil` to know when the mute auto-expires. */
  muted?: boolean
  /** ISO timestamp when the mute auto-expires; null/undefined = forever. */
  mutedUntil?: string | null
  unread?: number
  /** Latest persisted message id from the conversation list payload. Used to
   *  detect when the sidebar preview has advanced past the open transcript. */
  lastMessageId?: string | null
  lastAt: string
  /** Raw ISO timestamp the row was last touched (last message time, or
   *  the conversation's own updatedAt when there are no messages yet).
   *  Server returns the list in this order; we keep the raw value so any
   *  client-side re-sort uses real time rather than the display label. */
  lastAtIso: string
  preview: string
  /** optional special tag */
  tag?: 'team' | 'whisper' | 'human' | 'fresh-pulled'
  /** if pulled by an agent: the convener id and reason */
  pulledBy?: { agentId: string; at: string; reason: string }
  /** when this conversation belongs to a project, the project's id + name + tint */
  projectId?: string | null
  projectName?: string | null
  projectColor?: string | null
}

/** store 侧 Message:wire 形状 + 乐观渲染标志(仅本地插入的消息携带)。
 *  交叉类型而非 interface extends —— extends 子句不接受索引访问类型(TS2499)。 */
export type Message = Omit<Schemas['Message'], 'sequence' | 'createdAt'> & {
  sequence?: number
  createdAt?: string
  /** 客户端遗留渲染形状(mock/历史数据);服务端现无生产者 —— wire 规范不收录。 */
  whisperLink?: {
    pair: [string, string]
    snippet: string
    count: number
  }
  /** Optimistic-render flags. Only set on locally-inserted messages awaiting
   *  the server round-trip; never returned from the API. */
  pending?: boolean
  failed?: boolean
  /** The request may have committed, but neither HTTP nor WS confirmed it. */
  unconfirmed?: boolean
  /** Live agent stream (#210): synthesized from `message.delta` frames, never
   *  persisted — the final `message.new` from the same author replaces it. */
  streaming?: boolean
}

/** UI-only: starter-agent 模板的角色标签(非线上 wire 枚举) */
export type AgentRole = 'researcher' | 'designer' | 'engineer' | 'pm' | 'brand' | 'ops'

export interface ViewKey {
  view: 'conversations' | 'whispers' | 'convene' | 'agents' | 'boards' | 'calendar' | 'documents' | 'workspaces' | 'skills' | 'shipping' | 'observability' | 'me' | 'library'
}
