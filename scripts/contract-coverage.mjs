// 契约覆盖率守卫:从 Go 源码提取路由表,与 openapi.yaml 的 paths 对账。
// 任何一端漂移(新路由未进规范 / 规范里的路由已删)即失败。
// #70 TS 退役:提取腿从 TS Express 源换到 apps/server-go 的
// mux.HandleFunc("METHOD /path") 注册(158+ 条,注册即路径,无挂载
// 哨兵面;"/" 根兜底不带方法前缀,天然不入表)。
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'

const GO_ROOT = 'apps/server-go'

function* goFiles(dir) {
  for (const name of readdirSync(dir)) {
    if (name.endsWith('_test.go')) continue
    const p = join(dir, name)
    if (statSync(p).isDirectory()) yield* goFiles(p)
    else if (name.endsWith('.go')) yield p
  }
}

const METHOD_PATH = /Handle(?:Func)?\(\s*"(GET|POST|PUT|PATCH|DELETE) ([^"]+)"/g

// 待实现豁免(#117 missed-routes):规范已登记、TS 已实现、Go 尚未移植
// 的路由。实现一条删一条;表外新增缺路由仍会红。
const PENDING_IMPLEMENTATION = new Set([
  'GET /api/admin/observability/llm',
  'GET /api/admin/observability/llm/calls',
  'GET /api/admin/stats',
  'GET /api/admin/users',
  'GET /api/admin/users/:x',
  'PATCH /api/admin/users/:x',
  'GET /api/admin/waitlist',
  'POST /api/admin/waitlist/:x/approve',
  'POST /api/admin/waitlist/:x/reject',
  'GET /api/agents/:x/autonomy',
  'PUT /api/agents/:x/autonomy',
  'POST /api/auth/apple/native',
  'POST /api/polls',
  'POST /api/polls/:x/vote',
  'POST /api/polls/:x/close',
  'GET /api/shipping/overview',
  'GET /api/shipping/features/:x',
  'POST /api/shipping/features',
  'PATCH /api/shipping/features/:x',
  'POST /api/shipping/features/:x/invariants',
  'PATCH /api/shipping/features/:x/invariants/:x',
  'POST /api/shipping/features/:x/regressions',
  'PATCH /api/shipping/features/:x/regressions/:x',
  'POST /api/shipping/features/:x/releases',
  'POST /api/shipping/features/:x/releases/:x/action',
  'POST /api/shipping/features/:x/transition',
  'POST /api/shipping/features/:x/verifications',
  'PATCH /api/shipping/features/:x/verifications/:x',
  'GET /api/shipping/friction',
  'POST /api/shipping/friction',
  'PATCH /api/shipping/friction/:x',
])
const norm = (s) => s.replace(/\{[^}]+\}/g, ':x').replace(/:[^/]+/g, ':x').replace(/\/+/g, '/').replace(/\/$/, '') || '/'

const src = new Set()
for (const f of goFiles(GO_ROOT)) {
  const text = readFileSync(f, 'utf8')
  for (const m of text.matchAll(METHOD_PATH)) {
    src.add(`${m[1]} ${norm(m[2])}`)
  }
}

const yaml = readFileSync('packages/contract/openapi.yaml', 'utf8')
const spec = new Set()
for (const m of yaml.matchAll(/^ {2}(\/\S+):$/gm)) {
  const p = m[1]
  const block = yaml.slice(yaml.indexOf('  '.concat(p).concat(':')))
  const next = block.slice(1).search(/^ {2}\//m)
  const body = next === -1 ? block : block.slice(0, next + 1)
  for (const mm of body.matchAll(/^ {4}(get|post|put|patch|delete):$/gm)) {
    spec.add(`${mm[1].toUpperCase()} ${norm(p)}`)
  }
}

const missing = [...src].filter((x) => !spec.has(x)).sort()
const extra = [...spec].filter((x) => !src.has(x) && !PENDING_IMPLEMENTATION.has(x)).sort()
const staleExempt = [...PENDING_IMPLEMENTATION].filter((x) => !spec.has(x)).sort()
if (staleExempt.length) {
  console.error('[contract] 豁免表里的路由已不在规范(实现后请删豁免):\n' + staleExempt.map((x) => '  ' + x).join('\n'))
  process.exit(1)
}
if (missing.length || extra.length) {
  if (missing.length) console.error('[contract] 源码有而规范缺:\n' + missing.map((x) => '  ' + x).join('\n'))
  if (extra.length) console.error('[contract] 规范有而源码无:\n' + extra.map((x) => '  ' + x).join('\n'))
  process.exit(1)
}
console.log(`[contract] 路由覆盖 ${src.size}/${src.size} 全对账`)
