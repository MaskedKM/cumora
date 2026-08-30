/**
 * 观测双页(admin/ObservabilityPage · 平台级成本账本 / desktop/
 * ObservabilityView · 单租户运行调试器)的共享格式化层(#147 ②)。
 * 两页是"同名不同物":数据形状与面板零交集,能安全共享的只有这层
 * 纯函数 —— $ 的两种语体(紧凑 K/M 档位 vs 精确小数位)、token 档位、
 * 百分比、缓存命中率、相对时间分部。零依赖,可被任何 bundle 安全引入。
 */

/** 紧凑美元:$1.23 / $12.4K / $1.23M。hero 卡读原始九位数是敌意的。 */
export function fmtUsdCompact(n: number, places = 2): string {
  if (!Number.isFinite(n)) return '$0'
  if (Math.abs(n) >= 1_000_000) return `$${(n / 1_000_000).toFixed(2)}M`
  if (Math.abs(n) >= 1_000)     return `$${(n / 1_000).toFixed(1)}K`
  return `$${n.toFixed(places)}`
}

/** 精确美元:按量级 2/4/6 位小数($0.000012 这种 BYOA 长尾也要可读)。 */
export function fmtUsdPrecise(n: number): string {
  const a = Math.abs(n)
  const s = a >= 1 ? n.toFixed(2) : a >= 0.01 ? n.toFixed(4) : n.toFixed(6)
  return `$${s}`
}

/** Token 档位:1.2K / 3.4M / 5.6B。 */
export function fmtTokens(n: number): string {
  if (!Number.isFinite(n)) return '0'
  if (Math.abs(n) >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(2)}B`
  if (Math.abs(n) >= 1_000_000)     return `${(n / 1_000_000).toFixed(2)}M`
  if (Math.abs(n) >= 1_000)         return `${(n / 1_000).toFixed(1)}K`
  return String(n)
}

export function fmtInt(n: number): string {
  return n.toLocaleString('en-US')
}

/** 百分比(入参是 0-1 比率):places 缺省 1 位小数。 */
export function fmtPct(n: number, places = 1): string {
  return `${(n * 100).toFixed(places)}%`
}

/** 缓存命中率:cached / (uncached + cached)。分母为 0 时给 0(渲染层
 * 需要区分"无流量"与"0% 命中"时自行判 null)。此前 admin 页四处内联。 */
export function cacheHitRate(inputTokens: number, cachedInputTokens: number): number {
  const totalIn = inputTokens + cachedInputTokens
  return totalIn > 0 ? cachedInputTokens / totalIn : 0
}

export type RelativeTimeParts =
  | { ms: number; unit: 'sec'; n: number }
  | { ms: number; unit: 'min'; n: number }
  | { ms: number; unit: 'hour'; n: number }
  | { ms: number; unit: 'day'; n: number }

/** 相对时间分部(向下取整):消费方各自映射 i18n 键 —— 两页键族不同
 * (adminobs.timeAgo* vs obs.*Ago),且 desktop 侧有 5s 内 justNow 的
 * 本地阈值,用返回的 ms 自行判断。invalid 输入返回 null(渲染层给 '—')。 */
export function relativeTimeParts(iso: string): RelativeTimeParts | null {
  const ts = new Date(iso).getTime()
  if (!Number.isFinite(ts)) return null
  const ms = Date.now() - ts
  if (ms < 60_000)     return { ms, unit: 'sec',  n: Math.max(0, Math.floor(ms / 1000)) }
  if (ms < 3_600_000)  return { ms, unit: 'min',  n: Math.floor(ms / 60_000) }
  if (ms < 86_400_000) return { ms, unit: 'hour', n: Math.floor(ms / 3_600_000) }
  return { ms, unit: 'day', n: Math.floor(ms / 86_400_000) }
}
