// 消息流件(#219 ⑤)—— 从 MobileChat.tsx 原样搬移:
// StreamCtx(Virtuoso context 动态旗标)+ StreamHeader/StreamFooter
// (顶/底节点,模块级稳定身份,防流式 tick 重挂载抖动)+ STREAM_COMPONENTS
// + MessageRowMobileShell(行长压检测壳,per-row hook 槽位)。
import { MessageRow } from '@/components/Message'
import { useT } from '@/lib/i18n'
import type { Message, Participant } from '@/types'
import { useLongPress } from '../useLongPress'

/** Dynamic flags the virtualized stream's Header needs, threaded through
 *  Virtuoso's `context` prop instead of through a fresh `components` object
 *  every render. */
export type StreamCtx = { hasMoreOlder: boolean; loadingOlder: boolean }

/** Top-of-list node: either the older-history loading affordance or the
 *  "Beginning" divider once the first page is reached. Defined at MODULE
 *  scope — never recreated per render — which is the whole point: an inline
 *  `components={{ Header: () => … }}` literal makes a brand-new component
 *  *type* on every render, and React unmounts+remounts a node whose type
 *  identity changed. Cumora streams agent messages many times per second, so
 *  that inline Header was being torn down and rebuilt on every streaming tick,
 *  re-measuring the very top of the list and nudging every row below it — a
 *  primary driver of the scroll-up jitter. The flags now arrive via `context`,
 *  which RE-RENDERS these stable components without remounting them.
 *
 *  Height stays constant across the loading↔idle toggle: while more history
 *  exists we always render the same `py-1 px-2.5` pill (text swaps between
 *  "Loading earlier…" and a blank space), so paging never changes the
 *  header's height and never re-anchors the scroll. */
function StreamHeader({ context }: { context?: StreamCtx }) {
  const t = useT()
  const hasMoreOlder = context?.hasMoreOlder ?? false
  const loadingOlder = context?.loadingOlder ?? false
  return (
    <div className="px-3 pt-4 flex flex-col gap-3">
      {hasMoreOlder ? (
        <div className="self-center py-1 px-2.5 rounded-full text-[10.5px] font-medium text-ink-400">
          {loadingOlder ? t('chat.loadingEarlier') : ' '}
        </div>
      ) : (
        <div className="flex items-center gap-3 text-ink-300 text-[10.5px] font-bold tracking-[0.08em] uppercase">
          <span className="flex-1 h-px bg-gradient-to-r from-transparent via-ink-100 to-transparent" />
          {t('chat.beginning')}
          <span className="flex-1 h-px bg-gradient-to-r from-transparent via-ink-100 to-transparent" />
        </div>
      )}
    </div>
  )
}

function StreamFooter() {
  return <div className="h-3" />
}

/** Stable identity — passed to Virtuoso once so it never sees a changed
 *  `components` reference (see StreamHeader for why that matters). */
export const STREAM_COMPONENTS = { Header: StreamHeader, Footer: StreamFooter }

/** Per-message wrapper that attaches the long-press detector and
 *  preserves the same padding the bare MessageRow used to ship inside
 *  Virtuoso's itemContent. The wrapper exists as its own component
 *  so the long-press hook gets its own state slot — re-using one
 *  detector across all visible items would cross-fire.
 *
 *  `userSelect: none` + `WebkitTouchCallout: none` are critical:
 *  without them, iOS WKWebView's long-press triggers system text
 *  selection AT THE SAME TIME as our Tapback menu, with the system
 *  occasionally extending the selection across the whole screen
 *  before our menu paints on top — "全屏的文字都被选中了". iOS
 *  Messages itself doesn't allow per-bubble text selection for
 *  exactly this reason; Copy comes through the Tapback menu (our
 *  "Copy text" row), which puts the body on the clipboard
 *  programmatically. */
export function MessageRowMobileShell({
  msg, author, animate, onLongPress,
}: {
  msg: Message
  author?: Participant
  animate: boolean
  onLongPress: (coords: { x: number; y: number }) => void
}) {
  const press = useLongPress(onLongPress)
  return (
    <div
      className="px-3 py-2"
      style={{
        userSelect: 'none',
        WebkitUserSelect: 'none',
        WebkitTouchCallout: 'none',
      }}
      {...press}
    >
      <MessageRow msg={msg} author={author} delay={0} animate={animate} />
    </div>
  )
}
