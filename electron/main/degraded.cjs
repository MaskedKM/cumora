/* eslint-env node */
// =============== Stack degraded verdict (#314, ADR 0005 阶段 3 余量) ===============
// Pure computation: `cumora-stack status --json` 报告 → 托盘降级判定。
// 主进程常驻轮询(stack.cjs)与渲染端 StackTab 面板红条同源同一套规则:
//
//   stackd.children 非空 → 状态文件新鲜(<30s;stackd 活着每 2s 落盘)时
//   任一子进程 !running || circuitOpen = 降级;陈旧 = stackd 已停/被杀
//   后的冻结快照,不可判定(主动全停不永久挂 ⚠;SIGKILL 残留的
//   running=true 也不再装活)。
//   否则旧三 unit 形态 → 有 unit 实际 active 才看 livez,非 ok = 降级
//   (8-31 事故形态:三件套停摆仅 sidecar 存活 → livez 死 → ⚠)
//
// 返回 null = 无法判定(报告缺字段/栈未装/unit 全停/快照陈旧):调用方
// 保持上一次判定 —— 未知 ≠ 降级,与 StackTab「轮询出错保留最后已知值」
// 同一取舍。抽成无依赖纯函数正是为了这份规则能有真单测(wiring 型
// 功能必须配断言)。
function computeDegradedFromStatus(rep) {
  if (!rep || typeof rep !== 'object') return null
  const children = rep.stackd?.children
  if (Array.isArray(children) && children.length > 0) {
    const updated = Date.parse(rep.stackd.updatedAt || '')
    if (!Number.isFinite(updated) || Date.now() - updated >= 30_000) return null
    return children.some((c) => c && (!c.running || c.circuitOpen))
  }
  const units = Array.isArray(rep.units) ? rep.units : []
  const anyActive = units.some((u) => u && u.active === 'active')
  if (!anyActive) return null
  return rep.livez?.status !== 'ok'
}

module.exports = { computeDegradedFromStatus }
