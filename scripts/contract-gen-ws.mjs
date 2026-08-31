#!/usr/bin/env node
// contract-gen-ws —— WS 事件契约双端生成(#221)。
//
// 读 packages/contract/ws-events.json(纯 JSON Schema 单文件:$defs 每条目
// 一个事件载荷,x-event 携带事件名/Redis 通道/作用域),产出:
//   TS: packages/contract/src/ws-events.d.ts —— apps/web client.ts 的 WsEvent
//       union、tests/integration harness 与 yjs-sidecar 的发布/消费面;
//   Go: apps/server-go/internal/events/ws.gen.go —— 事件载荷结构体、事件名
//       常量、Redis 通道常量与 CompanyChannels(wsx 聊天桥订阅清单)。
//
// 选型(#221 评审决策):AsyncAPI 工具链(Go 生成器)在离线/弱出口环境不可
// 靠,且事件载荷需要交叉引用既有 OpenAPI 组件(Message/Status/Poll…),
// 故取纯 JSON Schema + 本零依赖生成器 —— JSON.parse 直读、输出确定性
// (幂等)、不引入任何新 npm/go 依赖。接入 npm run contract:gen,CI 漂移
// 守卫(contract:check)再生对账;手写漂移守卫见 scripts/contract-check.sh。
//
// $ref 约定:openapi.yaml#/components/schemas/<Name> → TS 侧映射
// Schemas['<Name>'](schema.d.ts 既生成物);Go 侧默认 map[string]any
// (发布方从 DB 行动态构载荷,深层类型化留给消费端),可用属性的
// x-go-type 覆盖。时间语义等注释随 description 流入两端的生成物。
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const SCHEMA = join(ROOT, 'packages/contract/ws-events.json')
const TS_OUT = join(ROOT, 'packages/contract/src/ws-events.d.ts')
const GO_OUT = join(ROOT, 'apps/server-go/internal/events/ws.gen.go')

const spec = JSON.parse(readFileSync(SCHEMA, 'utf8'))
const defs = spec.$defs
const channels = spec['x-channels']
const SCOPES = new Set(['company', 'doc', 'gateway'])
const TS_SCALARS = { string: 'string', integer: 'number', number: 'number', boolean: 'boolean' }

/* ── 校验:契约自洽(事件名/通道/作用域),坏契约当场红 ── */
const events = []
for (const [name, def] of Object.entries(defs)) {
  const meta = def['x-event']
  if (!meta) throw new Error(`[ws-gen] $defs.${name} 缺 x-event`)
  if (!SCOPES.has(meta.scope)) throw new Error(`[ws-gen] $defs.${name} x-event.scope 非法:${meta.scope}`)
  const typeProp = def.properties?.type
  if (typeProp?.const !== meta.type)
    throw new Error(`[ws-gen] $defs.${name} properties.type.const(${typeProp?.const}) ≠ x-event.type(${meta.type})`)
  if (meta.channel && !channels[meta.channel])
    throw new Error(`[ws-gen] $defs.${name} 引用未声明的通道 ${meta.channel}`)
  if (!meta.required?.includes?.('type')) {
    // required 必含 type(discriminator),顺手补齐而非报错——契约里都写了
    def.required = [...new Set(['type', ...(def.required ?? [])])]
  }
  events.push({ name, def, meta })
}
for (const [ch, meta] of Object.entries(channels)) {
  if (!meta.go || !meta.ts) throw new Error(`[ws-gen] x-channels.${ch} 缺 go/ts 常量名`)
}

const isRequired = (def, key) => (def.required ?? []).includes(key)
// description 进 JSDoc 前消毒:字面 星斜杠 序列会提前终结注释块。
const tsDoc = (s) => s.replace(/\*\//g, '*∕')
const openapiRef = (ref) => {
  const m = /^openapi\.yaml#\/components\/schemas\/([A-Za-z0-9_]+)$/.exec(ref)
  if (!m) throw new Error(`[ws-gen] 不支持的 $ref:${ref}(仅 openapi.yaml#/components/schemas/*)`)
  return m[1]
}

/* ── TS 类型(字面量随仓库 TS 风格用单引号)── */
const sq = (v) => `'${String(v).replace(/\\/g, '\\\\').replace(/'/g, "\\'")}'`
function tsType(prop) {
  if (prop['x-ts-type']) return prop['x-ts-type']
  if (prop.$ref) return `Schemas['${openapiRef(prop.$ref)}']`
  const types = Array.isArray(prop.type) ? prop.type : [prop.type ?? 'object']
  const nullable = types.includes('null')
  let base
  if (prop.const !== undefined) base = sq(prop.const)
  else if (prop.enum) base = prop.enum.map((v) => sq(v)).join(' | ')
  else if (prop.items) base = tsType(prop.items).includes(' | ')
    ? `Array<${tsType(prop.items)}>` : `${tsType(prop.items)}[]`
  else if (prop.properties) {
    const fields = Object.entries(prop.properties).map(([k, p]) =>
      `${k}${isRequired(prop, k) ? '' : '?'}: ${tsType(p)}`)
    base = `{ ${fields.join('; ')} }`
  } else base = TS_SCALARS[types.find((t) => t !== 'null')]
  if (!base) throw new Error(`[ws-gen] 无法映射为 TS 类型:${JSON.stringify(prop)}`)
  return nullable ? `${base} | null` : base
}

const tsLines = []
tsLines.push('// GENERATED FILE — DO NOT EDIT.')
tsLines.push('// 源:packages/contract/ws-events.json · 再生:npm run contract:gen(票 #221)。')
tsLines.push('// WS 事件契约的 TS 侧生成物:消费方 apps/web client.ts(WsEvent)、')
tsLines.push('// tests/integration/harness(WsBroadcastEvent)与 apps/yjs-sidecar(doc.* 两事件)。')
tsLines.push('// 逐事件载荷、通道与作用域语义见契约文件;时间语义坑(消息载荷时间键 at、')
tsLines.push('// agent ISOms/用户 RFC3339Nano、Go 消息 id 随机串)随 description 流入此处。')
tsLines.push("import type { components } from './schema'")
tsLines.push('')
tsLines.push('type Schemas = components[\'schemas\']')
tsLines.push('')
for (const { name, def, meta } of events) {
  if (def.description) tsLines.push(`/** ${tsDoc(def.description)} */`)
  tsLines.push(`export interface ${name} {`)
  for (const [key, prop] of Object.entries(def.properties)) {
    if (prop.description) tsLines.push(`  /** ${tsDoc(prop.description)} */`)
    tsLines.push(`  ${key}${isRequired(def, key) ? '' : '?'}: ${tsType(prop)}`)
  }
  const chanNote = meta.channel ? `,通道 ${meta.channel}` : ',gateway 直发(不上 Redis)'
  tsLines.push(`} // scope: ${meta.scope}${chanNote}`)
  tsLines.push('')
}
const union = (list) => list.join(' |\n  ')
tsLines.push('/** 客户端在 /ws 上可能收到的全部事件帧(含 gateway 自产的 hello/doc.sync/doc.error)。 */')
tsLines.push(`export type WsEvent =\n  ${union(events.map((e) => e.name))}`)
tsLines.push('')
tsLines.push('/** Redis 总线上发布的事件(harness publish 面;gateway 自产帧不在其列)。 */')
tsLines.push(`export type WsBroadcastEvent =\n  ${union(events.filter((e) => e.meta.scope !== 'gateway').map((e) => e.name))}`)
tsLines.push('')
tsLines.push('/** 事件 → Redis 通道映射(多事件可共用通道:cumora:status 承载 participants.* 与 computers.*)。')
tsLines.push(' *  harness 的 CH_* 运行时常量以此 `satisfies` 钉值,防手写漂移。 */')
tsLines.push('export interface WsChannels {')
for (const { meta } of events.filter((e) => e.meta.channel)) {
  tsLines.push(`  '${meta.type}': '${meta.channel}'`)
}
tsLines.push('}')
tsLines.push('')

/* ── Go:字段名(camel → Go 导出名,ID/URL/B64 惯用词)与类型 ── */
const goName = (key) => {
  const parts = key.replace(/([a-z0-9])([A-Z])/g, '$1 $2').split(/[ _-]+/)
  const upper = (s) => ({ id: 'ID', ids: 'IDs', url: 'URL', urls: 'URLs', b64: 'B64' })[s.toLowerCase()]
    ?? s[0].toUpperCase() + s.slice(1)
  return parts.map(upper).join('')
}
function goType(prop, key, where) {
  if (prop['x-go-type']) return prop['x-go-type']
  if (prop.$ref) return 'map[string]any' // OpenAPI 组件:Go 发布方动态构载荷,消费端管形状
  const types = Array.isArray(prop.type) ? prop.type : [prop.type ?? 'object']
  const nullable = types.includes('null')
  let base
  if (prop.const !== undefined || prop.enum) base = 'string'
  else if (prop.items) base = `[]${goType(prop.items, key, where)}`
  else if (prop.properties) base = 'map[string]any'
  else if (types[0] === 'string') base = 'string'
  else if (types[0] === 'integer') base = 'int'
  else if (types[0] === 'number') base = 'float64'
  else if (types[0] === 'boolean') base = 'bool'
  else if (prop['x-ts-type'] === 'unknown') base = 'any'
  if (!base) throw new Error(`[ws-gen] 无法映射为 Go 类型:${where}.${key} ${JSON.stringify(prop)}`)
  return nullable ? `*${base}` : base
}

/* ── Go 事件常量/通道/结构体 ──
 * 字节形约束:输出须与 gofmt 逐字节一致(CI gofmt check 与契约漂移守卫双
 * 卡)。gofmt 会按最长名对齐 const 块的 `=`、按 名/类型 最宽对齐结构体字
 * 段列,且正文内的注释行会切分对齐段 —— 因此结构体正文与 const 块正文
 * 零注释(逐字段语义注释进 TS 侧生成物与契约文件),对齐列在此显式计算。 */
const goConstName = (eventDefName) => 'Event' + eventDefName.replace(/Event$/, '')
const go = []
go.push('// Code generated from packages/contract/ws-events.json — DO NOT EDIT.')
go.push('// 再生:npm run contract:gen(票 #221)。事件载荷/通道/作用域语义见契约;')
go.push('// 手写面(internal/events/publish.go、wsx 网关)只组装此处类型,不再内联字面量。')
go.push('package events')
go.push('')
go.push('/* ── 事件名常量(载荷 type 判别值;发布方组帧时填入 Type 字段)── */')
go.push('const (')
{
  const consts = events.map(({ name, meta }) => ({ n: goConstName(name), v: meta.type }))
  const w = Math.max(...consts.map((c) => c.n.length))
  for (const c of consts) go.push(`\t${c.n.padEnd(w)} = ${JSON.stringify(c.v)}`)
}
go.push(')')
go.push('')
go.push('/* ── Redis 通道常量(x-channels;名字与退役前 publish.go 手写版逐一相同,')
go.push(' *  全仓引用方零改动)── */')
go.push('const (')
{
  const consts = Object.entries(channels).map(([ch, meta]) => ({ n: meta.go, v: ch }))
  const w = Math.max(...consts.map((c) => c.n.length))
  for (const c of consts) go.push(`\t${c.n.padEnd(w)} = ${JSON.stringify(c.v)}`)
}
go.push(')')
go.push('')
go.push('// CompanyChannels:公司域事件通道全集 —— wsx 聊天桥订阅清单(#221 起由契约')
go.push('// 推导:scope=company 事件的通道按首现去重;doc.update/doc.awareness 是房间')
go.push('// 域(docrelay 管),不在其列。对齐退役前 publish.go 的手写清单(顺序为推导序)。')
const companyChans = []
for (const { meta } of events.filter((e) => e.meta.scope === 'company')) {
  if (meta.channel && !companyChans.includes(meta.channel)) companyChans.push(meta.channel)
}
go.push('var CompanyChannels = []string{')
go.push(`\t${companyChans.map((c) => channels[c].go).join(', ')},`)
go.push('}')
go.push('')
for (const { name, def, meta } of events) {
  const chanNote = meta.channel ? `通道 ${meta.channel}、` : ''
  go.push(`// ${name} —— ${meta.type}(${chanNote}scope=${meta.scope})。`)
  if (def.description) go.push(`// ${def.description}`)
  go.push(`type ${name} struct {`)
  const fields = Object.entries(def.properties).map(([key, prop]) => {
    const omitempty = !isRequired(def, key) && prop['x-go']?.omitempty !== false
    const tag = `"${key}${omitempty ? ',omitempty' : ''}"`
    return {
      name: key === 'type' ? 'Type' : goName(key),
      type: goType(prop, key, name),
      tag,
    }
  })
  const nameW = Math.max(...fields.map((f) => f.name.length))
  const typeW = Math.max(...fields.map((f) => f.type.length))
  for (const f of fields) {
    go.push(`\t${f.name.padEnd(nameW)} ${f.type.padEnd(typeW)} \`json:${f.tag}\``)
  }
  go.push('}')
  go.push('')
}
while (go.length && go[go.length - 1] === '') go.pop()

mkdirSync(dirname(TS_OUT), { recursive: true })
writeFileSync(TS_OUT, tsLines.join('\n') + '\n')
mkdirSync(dirname(GO_OUT), { recursive: true })
writeFileSync(GO_OUT, go.join('\n') + '\n')
console.log(`[ws-gen] ${events.length} 事件 → ${TS_OUT.replace(ROOT + '/', '')} + ${GO_OUT.replace(ROOT + '/', '')}`)
