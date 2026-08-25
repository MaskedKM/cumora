// 契约覆盖率守卫:从 Express 源码提取路由表,与 openapi.yaml 的 paths 对账。
// 任何一端漂移(新路由未进规范 / 规范里的路由已删)即失败。
import { readFileSync } from 'node:fs'

const ROUTERS = [
  ['/api', 'server/src/api/router.ts', /\bapi\./],
  ['/api/admin', 'server/src/api/admin-router.ts', /\badminRouter\./],
  ['/api/shipping', 'server/src/api/shipping-router.ts', /\brouter\./],
  ['/api/workspaces', 'server/src/api/workspaces-router.ts', /\brouter\./],
  ['/runtime', 'server/src/agents/runtime/server.ts', /\bruntimeRouter\./],
]
const METHOD = /\.(get|post|put|patch|delete)\(\s*'([^']+)'/
const norm = (s) => s.replace(/\{[^}]+\}/g, ':x').replace(/:[^/]+/g, ':x').replace(/\/+/g, '/').replace(/\/$/, '') || '/'

const src = new Set()
for (const [prefix, file, owner] of ROUTERS) {
  const lines = readFileSync(file, 'utf8').split('\n')
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]
    if (!owner.test(line)) continue
    const m = line.match(METHOD)
    if (!m) continue
    // route registration lines are short; header checks / comments don't carry (method)('path')
    const path = m[2] === '/' ? '' : m[2]
    src.add(`${m[1].toUpperCase()} ${norm(prefix + path)}`)
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
const extra = [...spec].filter((x) => !src.has(x)).sort()
if (missing.length || extra.length) {
  if (missing.length) console.error('[contract] 源码有而规范缺:\n' + missing.map((x) => '  ' + x).join('\n'))
  if (extra.length) console.error('[contract] 规范有而源码无:\n' + extra.map((x) => '  ' + x).join('\n'))
  process.exit(1)
}
console.log(`[contract] 路由覆盖 ${src.size}/${src.size} 全对账`)
