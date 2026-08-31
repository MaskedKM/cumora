/** #220 ② — WS 绑定样板收敛。八个 store 此前各自重复同一形态:
 *  module 级 `wsBound` 布尔 + `ws.connect()` + `ws.on((e) => { if 链早退 })`
 *  (+ 部分 store 的 `hello` 重同步)。本帮手把「绑定一次 → 连接 → 单
 *  listener 查表分发」集中到一处,分发语义与原先的 if 链早退逐字等价:
 *
 *  - 每个 store 传入自己的 token(`{ bound }`),首次调用执行绑定并把
 *    token 置位,此后调用直接跳过 —— 与原 `if (wsBound) return` 同一
 *    守护;`ws.on` 的注册次数因此与收敛前完全一致:每 store 恰好一次。
 *  - 绑定当次仍调用 `ws.connect()`(顺序保持 connect → on)。WsClient
 *    对并发 / 已连接调用幂等(`connecting` memo + readyState 早退),
 *    所以 App.tsx 同 tick 的五个 boot 依旧合流到一条连接。
 *  - 未登记的事件类型查表落空后静默返回,等价原先 if 链走到底;需要
 *    「其余事件统一转交」形态的 store(messages 的 applyEvent 转发)
 *    用 `fallback` 表达,同样是原先语义的直译。
 *  - `hello` 重同步不抽 —— 它是每个 store 各自的数据语义,仍以普通
 *    handler 留在各 store 的表里。
 *
 *  每帧扇出路径不变慢:WsClient.on 仍是每 store 一个 listener 进 Set,
 *  帧到达时每个 listener 只做一次对象属性查找(不劣于原先 1~4 次
 *  字符串全等比较的 if 链),命中即调用,函数体原样搬入。 */
import { ws, type WsEvent } from '@/api/client'

/** 按 `WsEvent['type']` 键控的 handler 表;handler 收到的是对应类型
 *  收窄后的事件(Extract),未列出的类型静默跳过。 */
export type WsEventHandlers = Partial<{
  [K in WsEvent['type']]: (e: Extract<WsEvent, { type: K }>) => void
}>

export interface WsBindOptions {
  /** 默认 true:绑定的同时调用 `ws.connect()`。module 顶层注册、原本
   *  就不发起连接的 store(documents / calendar / boards —— 连接由
   *  boot 系列 store 负责)传 false,保持「import 不触发连接」的现状。 */
  connect?: boolean
  /** 表未命中时的兜底 —— 对应原先 if 链尾部的「其余事件统一转交」
   *  (messages store 的 `applyEvent(e)`)。 */
  fallback?: (e: WsEvent) => void
}

/** 把一个 store 的 WS handler 表接入全局 WsClient。
 *
 *  返回本次调用是否执行了绑定(token 首次置位)。需要挂一次性伴生
 *  注册的调用方(participants 的两个后台刷新 interval)以返回值做
 *  门闸,等价旧代码里 `if (wsBound) return` 块只跑一次的语义。 */
export function bindWsEvents(
  token: { bound: boolean },
  rawHandlers: WsEventHandlers,
  opts?: WsBindOptions,
): boolean {
  if (token.bound) return false
  token.bound = true
  // Null 原型包装:查表只可能命中显式登记的键(评审 P3)。裸对象上
  // `handlers['__proto__']` 这类畸形 type 会取到 Object.prototype 并在
  // 调用时抛错,炸断 WsClient.onmessage 的 listener 循环 —— 同帧后续
  // store 全部失联回归原 if 链没有的问题。
  const handlers = Object.assign(Object.create(null), rawHandlers) as WsEventHandlers
  if (opts?.connect !== false) ws.connect()
  ws.on((e) => {
    // 查表分发:key 与 e.type 同源,运行时命中的 handler 形参类型
    // 必然覆盖当前事件,cast 只是绕过映射类型按联合键索引的静态限制。
    const handler = handlers[e.type] as ((e: WsEvent) => void) | undefined
    if (handler) handler(e)
    else opts?.fallback?.(e)
  })
  return true
}
