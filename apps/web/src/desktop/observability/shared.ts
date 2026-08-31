/** ObservabilityView(#219 ①)共享词表 —— 面板键 / 状态样式 / 时间与字节
 *  格式化。壳与 ./observability/ 下各件共同消费;分部相对时间在
 *  @/lib/format(#147 ②),relative() 只保留本页 i18n 键族映射。 */
import type { ApiAgentRunStatus } from '@/api/client'
import { relativeTimeParts } from '@/lib/format'
import type { MessageKey, useT } from '@/lib/i18n'

export type DevPanel = 'traces' | 'workspace' | 'triage'
export const DEV_PANELS: DevPanel[] = ['traces', 'workspace', 'triage']
export const PANEL_LABEL_KEY: Record<DevPanel, MessageKey> = { traces: 'obs.panelTraces', workspace: 'obs.panelWorkspace', triage: 'obs.panelTriage' }

export const STATUS_STYLE: Record<ApiAgentRunStatus, { key: MessageKey; cls: string; dot: string }> = {
  running: {
    key: 'obs.statusRunning',
    cls: 'bg-sky2-50 text-skype-deep border-sky2-100',
    dot: 'var(--skype)',
  },
  stalled: {
    key: 'obs.statusStalled',
    cls: 'bg-coral-soft text-coral-deep border-coral-soft',
    dot: 'var(--coral-deep)',
  },
  failed: {
    key: 'obs.statusFailed',
    cls: 'bg-coral-soft text-coral-deep border-coral-soft',
    dot: 'var(--coral-deep)',
  },
  completed: {
    key: 'obs.statusCompleted',
    cls: 'bg-cloud text-ink-700 border-ink-100',
    dot: 'var(--avail)',
  },
  skipped: {
    key: 'obs.statusSkipped',
    cls: 'bg-ink-100 text-ink-600 border-ink-100',
    dot: 'var(--ink-300)',
  },
}

export function clock(ts: string): string {
  return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

export function elapsed(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  const s = Math.round(ms / 100) / 10
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  const rest = Math.round(s % 60)
  return `${m}m ${rest}s`
}

export function relative(ts: string, t: ReturnType<typeof useT>): string {
  // 分部计算共享 @/lib/format(#147 ②);5s 内 justNow 的阈值与 obs.*
  // 键族是本页局部约定。sec/min 段用分部的 floor(与原 Math.round 有
  // 字面差);hours 段保留 round —— 见 switch 内注释。
  const parts = relativeTimeParts(ts)
  if (!parts) return '—'
  if (parts.unit === 'sec' && parts.ms < 5000) return t('obs.justNow')
  switch (parts.unit) {
    case 'sec':  return t('obs.secondsAgo', { n: parts.n })
    case 'min':  return t('obs.minutesAgo', { n: parts.n })
    // hours 必须用 ms 重算并保留 Math.round,不能用 parts.n(它是 floor):
    // ≥1h 段与原实现逐字等价全靠这行 round(评审 34 万采样点穷举验证)。
    // "对齐"成 parts.n 会把 90min 显示成 1h(原 2h)。
    default:     return t('obs.hoursAgo', { n: Math.round(parts.ms / 3_600_000) })
  }
}

export function bytes(size: number): string {
  if (size < 1024) return `${size} B`
  if (size < 1024 * 1024) return `${Math.round(size / 1024)} KB`
  return `${Math.round(size / 1024 / 102.4) / 10} MB`
}
