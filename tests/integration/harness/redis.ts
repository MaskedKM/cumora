import IORedis from 'ioredis'
import { env } from './env.js'
import type { WsChannels } from '@cumora/contract/ws-events'

// When the agent runtime is in http mode (pod talking to a remote
// server), Redis is never used from this process — every pub/sub flows
// through /runtime/* on the server side. Connect lazily so the bundle
// doesn't spam ECONNREFUSED logs in pods that don't have a Redis
// nearby. In server mode the first publish/subscribe wakes it up
// instantly.
const lazyConnect = process.env.CUMORA_RUNTIME_CLIENT === 'http'

/** Single shared client for normal commands. */
export const redis = new IORedis(env.REDIS_URL, {
  maxRetriesPerRequest: null,
  enableReadyCheck: true,
  lazyConnect,
})

/** Separate connection for blocking SUBSCRIBE — required by the Redis protocol. */
export const sub = new IORedis(env.REDIS_URL, {
  maxRetriesPerRequest: null,
  enableReadyCheck: true,
  lazyConnect,
})

redis.on('error', (e) => console.error('[redis]', e))
sub.on('error', (e) => console.error('[redis sub]', e))

/* === Channel keys === */
export const CH_MESSAGE_NEW = 'cumora:msg.new' satisfies WsChannels['message.new']
export const CH_MESSAGE_DELTA = 'cumora:msg.delta' satisfies WsChannels['message.delta']
export const CH_TYPING = 'cumora:typing' satisfies WsChannels['typing']
export const CH_STATUS = 'cumora:status' satisfies WsChannels['participants.status']
export const CH_REACTIONS = 'cumora:reactions' satisfies WsChannels['message.reactions']
export const CH_POLLS = 'cumora:polls' satisfies WsChannels['poll.updated']
export const CH_GROUP_PULLED = 'cumora:group.pulled' satisfies WsChannels['group.pulled']
export const CH_CONVO_UPDATED = 'cumora:convo.updated' satisfies WsChannels['conversation.updated']
export const CH_CONVENE = 'cumora:convene' satisfies WsChannels['convene']
export const CH_BOARDS = 'cumora:boards' satisfies WsChannels['board.changed']
export const CH_DOCS = 'cumora:docs' satisfies WsChannels['doc.changed']
/* === Collaborative documents (CRDT) ===
 *
 * Yjs binary updates are base64-encoded into the JSON envelope so they
 * fan out through the same Redis bus + WS path as every other event.
 * `originId` is the WS client (or agent) that produced the update so the
 * fan-out can echo-suppress on the sender's own socket. */
export const CH_DOC_UPDATE = 'cumora:doc.update' satisfies WsChannels['doc.update']
export const CH_DOC_AWARENESS = 'cumora:doc.awareness' satisfies WsChannels['doc.awareness']
/** A user / agent was @-mentioned inside a doc. Fanned out via the
 *  generic tenant-scoped WS bridge (NOT the per-doc subscription
 *  bridge) — recipients listen by their participant id, regardless of
 *  whether they currently have the doc open. */
export const CH_DOC_MENTION = 'cumora:doc.mention' satisfies WsChannels['doc.mention']
export const CH_CALENDAR_REMINDER = 'cumora:calendar.reminder' satisfies WsChannels['calendar.reminder']
/** Calendar row CRUD + dispatch-driven status changes. Sent on create /
 *  update / delete / cancel / run-now / dispatcher auto-done so every
 *  client in the company can patch their Calendar view in real time. */
export const CH_CALENDAR_EVENTS = 'cumora:calendar.events' satisfies WsChannels['calendar.changed']

/* === Event types(#221 契约化)===
 *
 * 手写联合退役:事件载荷类型全部来自契约生成物 @cumora/contract/ws-events
 * (packages/contract/ws-events.json → npm run contract:gen 再生)。下方按退
 * 役前的导出名逐一 re-export,测试零改动;通道常量留在本模块(类型包无运
 * 时产物),但用 `satisfies WsChannels[...]` 把值钉死在契约上 —— 契约改通
 * 道名而忘改这里,tsc 立红。多租户注记(原手写版):每个事件携带可选
 * companyId,WS 扇出按接收连接的公司成员资格过滤;无 companyId 的事件被
 * 保守拒绝路由 —— 每个发布点都保持打标。新增事件:改契约一处,三端(前端
 * WsEvent / 本 harness / Go internal/events)类型全部自动跟上。 */

export type {
  MessageNewEvent, MessageDeltaEvent, TypingEvent, StatusEvent, ComputerStatusEvent,
  AvatarEvent, ParticipantAddedEvent, ReactionsEvent, ConversationUpdatedEvent,
  GroupPulledEvent, ConveneEvent, BoardEvent, DocIndexEvent, DocUpdateEvent,
  DocAwarenessEvent, DocMentionEvent, CalendarReminderEvent, CalendarEventChangedEvent,
  PollUpdatedEvent, WsBroadcastEvent,
} from '@cumora/contract/ws-events'

/** 与生成物对齐的发布面联合(harness 只发布 Redis 总线事件)。 */
export type BroadcastEvent = import('@cumora/contract/ws-events').WsBroadcastEvent

export async function publish(channel: string, event: BroadcastEvent): Promise<void> {
  await redis.publish(channel, JSON.stringify(event))
}
