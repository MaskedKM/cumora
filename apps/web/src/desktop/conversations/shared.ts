// 会话列表桶(#219 ②)共享词表 —— 从 ConversationsPane.tsx 原样搬移:
// Translator(响应式翻译器类型)+ 静音词表(MUTE_DURATIONS/muteHint/muteTooltip),
// 供 ConvoRow(tooltip)与 convoMenu(菜单时长/提示)两件共用,免桶间互倚。
import type { MessageKey, useT } from '@/lib/i18n'

/** The reactive translator, threaded into the helpers below so they stay
 *  pure and their callers' components re-render on a locale switch. */
export type Translator = ReturnType<typeof useT>

/** Mute duration menu — Slack-shaped defaults plus relative anchors
 *  ("until tomorrow morning", "until next Monday morning"). `compute` runs
 *  at click time, not at module load, so "tomorrow" always resolves
 *  against the user's current wall-clock — not whatever it was when the
 *  app first booted. */
export const MUTE_DURATIONS: Array<{ label: MessageKey; compute: () => Date }> = [
  { label: 'convo.mute15m', compute: () => new Date(Date.now() + 15 * 60_000) },
  { label: 'convo.mute1h',  compute: () => new Date(Date.now() + 60 * 60_000) },
  { label: 'convo.mute8h',  compute: () => new Date(Date.now() + 8 * 60 * 60_000) },
  { label: 'convo.mute24h', compute: () => new Date(Date.now() + 24 * 60 * 60_000) },
  {
    label: 'convo.muteTomorrow',
    // 9 AM the next calendar day, local time. If the user clicks this at
    // 02:00 on Tuesday they get silence until Wednesday 09:00 — matches
    // how a human reads "until tomorrow".
    compute: () => {
      const d = new Date()
      d.setDate(d.getDate() + 1)
      d.setHours(9, 0, 0, 0)
      return d
    },
  },
  {
    label: 'convo.muteMonday',
    // Next Monday 9 AM local. If today IS Monday we skip to the FOLLOWING
    // Monday — "next" reads as "a whole week of quiet", not "a few hours".
    compute: () => {
      const d = new Date()
      const day = d.getDay() // 0=Sun, 1=Mon ...
      const daysUntil = ((1 - day + 7) % 7) || 7
      d.setDate(d.getDate() + daysUntil)
      d.setHours(9, 0, 0, 0)
      return d
    },
  },
]

/** Short label shown next to the "Muted" menu row so users know how long
 *  they've still got left without diving into the submenu. */
export function muteHint(t: Translator, mutedUntil: string | null | undefined): string {
  if (!mutedUntil) return t('convo.muteHintForever')
  const until = new Date(mutedUntil)
  if (Number.isNaN(until.getTime())) return ''
  const now = new Date()
  const sameDay = until.toDateString() === now.toDateString()
  return t('convo.muteHintUntil', {
    when: sameDay
      ? until.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
      : until.toLocaleDateString([], { month: 'short', day: 'numeric' }),
  })
}

/** Tooltip for the bell-off glyph — surfaces the auto-unmute time so the
 *  user remembers when notifications come back. */
export function muteTooltip(t: Translator, mutedUntil: string | null | undefined): string {
  if (!mutedUntil) return t('convo.muted')
  const until = new Date(mutedUntil)
  if (Number.isNaN(until.getTime())) return t('convo.muted')
  const now = new Date()
  const sameDay = until.toDateString() === now.toDateString()
  const fmt = sameDay
    ? until.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : until.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  return t('convo.mutedUntil', { when: fmt })
}
