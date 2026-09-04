// GENERATED FILE — DO NOT EDIT.
// 源:packages/contract/ws-events.json · 再生:npm run contract:gen(票 #221)。
// WS 事件契约的 TS 侧生成物:消费方 apps/web client.ts(WsEvent)、
// tests/integration/harness(WsBroadcastEvent)与 apps/yjs-sidecar(doc.* 两事件)。
// 逐事件载荷、通道与作用域语义见契约文件;时间语义坑(消息载荷时间键 at、
// agent ISOms/用户 RFC3339Nano、Go 消息 id 随机串)随 description 流入此处。
import type { components } from './schema'

type Schemas = components['schemas']

/** 一条新消息落库后的广播。message 为消息 wire 全量(shape = OpenAPI Message:at/quoted/email/poll/pollTallies 等),clientId 用于前端乐观气泡与 WS 回显去重(tempId 键配对)。 */
export interface MessageNewEvent {
  type: 'message.new'
  /** 租户标记;Go 发布方为空串时整键省略(对齐 TS `companyId: x ?? undefined`)。 */
  companyId?: string
  conversationId: string
  /** 消息 wire 形状(OpenAPI Message:时间键 at —— agent 路径 ISOms / 用户路径 RFC3339Nano;id 随机串不可排序,排序键 sequence)。 */
  message: Schemas['Message']
} // scope: company,通道 cumora:msg.new

/** 流式增量(代理边打边发,#210 已激活)。daemon 铸流 id(messageId)与终局消息 id 不配对——前端按 (conversationId, authorId) 收口,message.new / done / 陈旧兜底三条退场路径幂等;delta 只上屏不入库。 */
export interface MessageDeltaEvent {
  type: 'message.delta'
  companyId?: string
  conversationId: string
  /** 在途流 id(daemon 铸造);与终局 message.new 的消息 id 不配对——收口按 (conversationId, authorId)。 */
  messageId: string
  authorId: string
  /** 本帧追加到 body 的文本块;客户端按 ACCUMULATE 语义累积。 */
  delta: string
  /** 流内递增序号,起流从 1 分配。 */
  sequence: number
  /** true = 流结束(终局仍以 message.new 为准;done 是 daemon 侧退场兜底)。 */
  done: boolean
} // scope: company,通道 cumora:msg.delta

/** 代理输入态:done=false 开始 / true 停止。 */
export interface TypingEvent {
  type: 'typing'
  /** Go events.Typing(调度器/HTTP 路径)恒带此键(空串也保留);runtime/presence.go 高频路径 companyID 为 nil 时省键——桥对空 companyId 与键缺失同判(丢弃),两形语义等价。 */
  companyId?: string
  conversationId: string
  agentId: string
  done: boolean
} // scope: company,通道 cumora:typing

/** 参与者在场/工作态翻转。statusUpdatedAt 缺席 = 广播方未提供(前端按 undefined 容忍)。 */
export interface StatusEvent {
  type: 'participants.status'
  companyId?: string
  participantId: string
  status: Schemas['Status']
  /** 发布方时间语义:Go 侧 httpx.ISOms。 */
  statusUpdatedAt?: string
} // scope: company,通道 cumora:status

/** 代理头像(重)生成完毕;前端 participants store 原地补丁,不等 60s 刷新滴答。 */
export interface AvatarEvent {
  type: 'participants.avatar'
  companyId?: string
  participantId: string
  avatarUrl: string
} // scope: company,通道 cumora:status

/** 人类接受邀请(或以其他方式新镜像进公司 participants)。participant 为 ApiParticipant 的窄投影:前端 byId 原地 upsert 不再拉取;conversationId 在场时前端顺手补对应会话的成员表。 */
export interface ParticipantAddedEvent {
  type: 'participants.added'
  companyId?: string
  /** 加入恰逢被加进某个会话(通常 #all-hands)时携带;缺席 = 仅入公司。 */
  conversationId?: string
  /** 窄投影:仅渲染成员芯片/花名册所需字段(statusUpdatedAt 可为 null —— DB 行该列 NULL)。刻意不用 OpenAPI Participant 全量:该 wire 事件从不携带 bio/tools/systemPrompt 等列。 */
  participant: { id: string; kind: Schemas['ParticipantKind']; name: string; role: string | null; initial: string; avatarBg: string; avatarUrl: string | null; status: Schemas['Status']; statusUpdatedAt: string | null }
} // scope: company,通道 cumora:status

/** BYOA Computer(代理宿主)上线/离线/忙碌;Computers 面板与代理芯片即时反映宿主可用性。 */
export interface ComputerStatusEvent {
  type: 'computers.status'
  companyId?: string
  computerId: string
  status: Schemas['ComputerStatus']
} // scope: company,通道 cumora:status

/** 某条消息的表情反应聚合(全量快照,非增量)。 */
export interface ReactionsEvent {
  type: 'message.reactions'
  companyId?: string
  conversationId: string
  messageId: string
  /** 全量聚合:每 emoji 一条;mine 依接收者视角,users 供悬浮名单。 */
  reactions: Schemas['ReactionEntry'][]
} // scope: company,通道 cumora:reactions

/** 投票状态变更(投票/改票/手动或到期关闭):携带全量反规范化快照,渲染端原地补丁免重拉。 */
export interface PollUpdatedEvent {
  type: 'poll.updated'
  companyId?: string
  conversationId: string
  messageId: string
  /** messages.poll 存储同形;关闭事件带 closedAt。 */
  poll: Schemas['PollPayload']
  /** 逐选项聚合(count + voterIds);voterIds 稳定排序,跨事件 diff 便宜。 */
  tallies: Schemas['PollTally'][]
  /** null = 服务端到期清扫触发。 */
  actorId: string | null
} // scope: company,通道 cumora:polls

/** 会话元数据补丁(主题/标题);客户端外科手术式补丁,不重拉。 */
export interface ConversationUpdatedEvent {
  type: 'conversation.updated'
  companyId?: string
  conversationId: string
  patch: { topic?: string | null; title?: string }
} // scope: company,通道 cumora:convo.updated

/** 代理拉群完成通知;前端刷新会话列表见新群。 */
export interface GroupPulledEvent {
  type: 'group.pulled'
  companyId?: string
  conversationId: string
  pulledById: string
} // scope: company,通道 cumora:group.pulled

/** convene(临场圆桌)会话生命周期:轻载荷,前端收到后重拉全量。 */
export interface ConveneEvent {
  type: 'convene'
  companyId?: string
  sessionId: string
  conversationId: string
  kind: 'started' | 'transcript' | 'ended' | 'tile'
  /** 按 kind 而异的自由载荷(tile 帧 = 布局瓦片等);契约不建模内部结构。 */
  data?: unknown
} // scope: company,通道 cumora:convene

/** 看板变更广播。刻意粗粒度:携带 boardId,前端按需重拉该板;卡级形状回显供乐观渲染免拉取;mentions 回显供通知 toaster 在用户没盯着看板时也能响。 */
export interface BoardEvent {
  type: 'board.changed'
  companyId?: string
  kind: 'board.created' | 'board.updated' | 'board.deleted' | 'column.created' | 'column.updated' | 'column.deleted' | 'card.created' | 'card.updated' | 'card.moved' | 'card.deleted' | 'comment.created' | 'comment.deleted'
  boardId: string
  cardId?: string
  columnId?: string
  commentId?: string
  /** 变更实体里解析出的 @mention 目标 id。 */
  mentions?: string[]
  /** 触发者;前端用于抑制自我通知。 */
  actorId?: string
} // scope: company,通道 cumora:boards

/** 文档索引/元数据变更(内容同步走 doc.* 房间域通道;本事件只让客户端刷新文档列表,代理新文档即刻可见)。 */
export interface DocIndexEvent {
  type: 'doc.changed'
  companyId?: string
  kind: 'document.created' | 'document.updated' | 'document.deleted'
  documentId: string
  actorId?: string
} // scope: company,通道 cumora:docs

/** Yjs 增量已应用到文档房间。base64 承载(WS 路径 JSON-only);originId 供发送方自己的连接回声抑制。Redis 总线上的载荷(sidecar → docrelay)多带 companyId/authorId;wsx 网关下发帧不透出这两键 —— 契约按并集建模(均可选)。 */
export interface DocUpdateEvent {
  type: 'doc.update'
  /** 仅 Redis 总线载荷携带;网关下发帧无此键。 */
  companyId?: string
  documentId: string
  /** Base64 的 Yjs 增量字节(非全量 state)。 */
  updateB64: string
  /** 产生此更新的 WS 客户端/代理稳定 id。 */
  originId: string
  /** 仅 Redis 总线载荷携带;活动通知用(通常为 user/agent id)。 */
  authorId?: string
} // scope: doc,通道 cumora:doc.update

/** awareness(光标/选区/在场)—— 临时态不落库,与 doc.update 同扇出路径。 */
export interface DocAwarenessEvent {
  type: 'doc.awareness'
  /** 仅 Redis 总线载荷携带;网关下发帧无此键。 */
  companyId?: string
  documentId: string
  updateB64: string
  originId: string
} // scope: doc,通道 cumora:doc.awareness

/** 文档订阅握手回执:订阅登记成功即下发全量 state(客户端以收到此帧 = 订阅建立)。 */
export interface DocSyncEvent {
  type: 'doc.sync'
  documentId: string
  /** Base64 的 Yjs 全量 state(区别于 updateB64 增量)。 */
  stateB64: string
  /** 网关为该连接分配的 origin id。 */
  originId: string
} // scope: gateway,gateway 直发(不上 Redis)

/** doc.* 帧服务端错误统一回执(订阅失败/未找到等);documentId 可能缺席(畸形帧无有效 id)。 */
export interface DocErrorEvent {
  type: 'doc.error'
  /** 恒携带(空串 = 原帧无有效 id),对齐网关现状。 */
  documentId?: string
  error: string
} // scope: gateway,gateway 直发(不上 Redis)

/** 文档内 @mention:载荷足以让渲染端零往返弹 toast(标题/提及者显示名/目标 id 列表),走公司域通用桥 —— 收件人不必开着该文档。 */
export interface DocMentionEvent {
  type: 'doc.mention'
  companyId?: string
  documentId: string
  documentTitle: string
  mentionerId: string
  mentionerName: string
  /** 被提及的参与者 id;客户端按 includes(meId) 过滤。 */
  mentionedIds: string[]
} // scope: company,通道 cumora:doc.mention

/** 「距此日历事件触发还有 N 分钟」提醒。桥扇出到全公司,渲染端按 recipientUserIds.includes(meId) 过滤;接收者可为空(仅代理的事件 —— 桥照发,无渲染端匹配)。 */
export interface CalendarReminderEvent {
  type: 'calendar.reminder'
  companyId?: string
  eventId: string
  title: string
  /** 本次触发对应的出现时刻(重复事件逐次计算)。 */
  occurrenceAt: string
  /** 触发时刻距 occurrenceAt 的分钟数,toast 文案用。 */
  leadMinutes: number
  /** 非空时仅这些人类用户弹 toast;代理刻意排除(已走派发路径唤醒)。 */
  recipientUserIds: string[]
  kind: Schemas['CalendarEventKind']
  /** toast 副标题展示用;null = 无指派。 */
  assigneeId: string | null
} // scope: company,通道 cumora:calendar.reminder

/** 日历行 CRUD / 派发器推进 last_fired_at。载荷刻意薄:客户端重拉受影响行(删除则丢弃),不做行内 diff —— 与 doc.changed 同思路,避免 router 与 CLI 两套编码器锁步。 */
export interface CalendarEventChangedEvent {
  type: 'calendar.changed'
  companyId?: string
  kind: 'event.created' | 'event.updated' | 'event.deleted' | 'event.dispatched'
  eventId: string
  /** null = 服务端自身(到期清扫);非空时渲染端避免把 actor 自己的乐观写回显给自己。 */
  actorId: string | null
} // scope: company,通道 cumora:calendar.events

/** 握手帧:握手完成即发(重连同发)。客户端以收到 hello = 连接完成,据此重放 doc.subscribe、冲刷断线攒批、重引数据;它是每条连接的第一帧。 */
export interface HelloEvent {
  type: 'hello'
  /** 服务端实例 id。 */
  instanceId: string
  /** epoch 毫秒(UnixMilli)。 */
  ts: number
} // scope: gateway,gateway 直发(不上 Redis)

export interface InboxNewEvent {
  type: 'inbox.new'
  companyId?: string
  /** 收件人(users.id;human participant 同 id)。客户端按它过滤自己的条目。 */
  recipientUserId: string
  itemId: string
  severity: 'action_required' | 'attention' | 'info'
  /** 生成源类型:run.failed / card.needs-human / card.assigned / dispatch.failed / dispatch.done */
  itemType: string
  title: string
  body?: string
  /** conversation / board / calendar / observability */
  linkKind?: string
  linkId?: string
  at: string
} // scope: company,通道 cumora:inbox

/** 团队工作区文件变更广播(#337,区级粒度,带文件清单不含内容)。发布方:server(挂载 watcher 上报处理 / 兜底扫描);帧路径对同租户成员可见,内容不随帧下发。 */
export interface WorkspaceFilesChangedEvent {
  type: 'workspace.files_changed'
  companyId: string
  /** 变更所属工作区 id */
  workspaceId: string
  changes: { path: string; mtimeNanos?: string; size?: number; removed: boolean }[]
  /** ISO 毫秒(httpx.ISOms 同形) */
  at?: string
} // scope: company,通道 cumora:workspace

/** 客户端在 /ws 上可能收到的全部事件帧(含 gateway 自产的 hello/doc.sync/doc.error)。 */
export type WsEvent =
  MessageNewEvent |
  MessageDeltaEvent |
  TypingEvent |
  StatusEvent |
  AvatarEvent |
  ParticipantAddedEvent |
  ComputerStatusEvent |
  ReactionsEvent |
  PollUpdatedEvent |
  ConversationUpdatedEvent |
  GroupPulledEvent |
  ConveneEvent |
  BoardEvent |
  DocIndexEvent |
  DocUpdateEvent |
  DocAwarenessEvent |
  DocSyncEvent |
  DocErrorEvent |
  DocMentionEvent |
  CalendarReminderEvent |
  CalendarEventChangedEvent |
  HelloEvent |
  InboxNewEvent |
  WorkspaceFilesChangedEvent

/** Redis 总线上发布的事件(harness publish 面;gateway 自产帧不在其列)。 */
export type WsBroadcastEvent =
  MessageNewEvent |
  MessageDeltaEvent |
  TypingEvent |
  StatusEvent |
  AvatarEvent |
  ParticipantAddedEvent |
  ComputerStatusEvent |
  ReactionsEvent |
  PollUpdatedEvent |
  ConversationUpdatedEvent |
  GroupPulledEvent |
  ConveneEvent |
  BoardEvent |
  DocIndexEvent |
  DocUpdateEvent |
  DocAwarenessEvent |
  DocMentionEvent |
  CalendarReminderEvent |
  CalendarEventChangedEvent |
  InboxNewEvent |
  WorkspaceFilesChangedEvent

/** 事件 → Redis 通道映射(多事件可共用通道:cumora:status 承载 participants.* 与 computers.*)。
 *  harness 的 CH_* 运行时常量以此 `satisfies` 钉值,防手写漂移。 */
export interface WsChannels {
  'message.new': 'cumora:msg.new'
  'message.delta': 'cumora:msg.delta'
  'typing': 'cumora:typing'
  'participants.status': 'cumora:status'
  'participants.avatar': 'cumora:status'
  'participants.added': 'cumora:status'
  'computers.status': 'cumora:status'
  'message.reactions': 'cumora:reactions'
  'poll.updated': 'cumora:polls'
  'conversation.updated': 'cumora:convo.updated'
  'group.pulled': 'cumora:group.pulled'
  'convene': 'cumora:convene'
  'board.changed': 'cumora:boards'
  'doc.changed': 'cumora:docs'
  'doc.update': 'cumora:doc.update'
  'doc.awareness': 'cumora:doc.awareness'
  'doc.mention': 'cumora:doc.mention'
  'calendar.reminder': 'cumora:calendar.reminder'
  'calendar.changed': 'cumora:calendar.events'
  'inbox.new': 'cumora:inbox'
  'workspace.files_changed': 'cumora:workspace'
}

