import { useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { cn } from '@/lib/utils'
import { useMe } from '@/stores/auth'
import { toggleReaction } from '@/stores/messages'
import { useParticipants } from '@/stores/participants'
import { TwEmoji } from '../TwEmoji'




export const QUICK_REACTIONS = ['👍', '❤️', '👀', '🌤️', '🔥', '👏', '✅', '🎯', '📌']

export function ReactionPill({ msgId, r }: { msgId: string; r: import('@/types').ReactionEntry }) {
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  const [burst, setBurst] = useState(0)
  const userIds = r.users ?? []
  const orderedNames = (() => {
    const me: string[] = []
    const others: string[] = []
    for (const uid of userIds) {
      if (uid === meId) { me.push('You'); continue }
      const name = byId[uid]?.name
      if (!name) continue  // participant not loaded → omit, never leak raw id
      others.push(name)
    }
    others.sort((a, b) => a.localeCompare(b))
    return [...me, ...others]
  })()

  // Portal-rendered tooltip with fixed coords so it escapes any ancestor's
  // overflow / clip / transform / stacking context. Coords are computed from
  // the button's bounding rect on hover, then clamped to the viewport.
  const btnRef = useRef<HTMLButtonElement>(null)
  const [coord, setCoord] = useState<{ x: number; y: number } | null>(null)

  const onEnter = () => {
    const el = btnRef.current
    if (!el || orderedNames.length === 0) return
    const r = el.getBoundingClientRect()
    setCoord({ x: r.left + r.width / 2, y: r.top })
  }
  const onLeave = () => setCoord(null)
  const onClick = () => {
    if (!r.mine) setBurst((n) => n + 1)
    void toggleReaction(msgId, r.emoji)
  }

  return (
    <>
      <button
        ref={btnRef}
        onMouseEnter={onEnter}
        onMouseLeave={onLeave}
        onFocus={onEnter}
        onBlur={onLeave}
        onClick={onClick}
        data-mine={r.mine ? 'true' : 'false'}
        className={cn(
          'reaction-control reaction-pill rounded-full text-[11px] py-0.5 px-2 inline-flex items-center gap-1 border transition',
          r.mine
            ? 'bg-sky2-100 border-sky2-200 text-skype-deep font-semibold'
            : 'bg-cloud border-ink-100 text-ink-500 hover:border-sky2-200',
        )}
      >
        <span className="reaction-emoji inline-flex"><TwEmoji emoji={r.emoji} size={14} /></span>
        {/* No key={r.count} — that used to force unmount + replay the
            entrance animation on every count change, which under rapid
            clicks left visible stutter (multiple count spans coexisting
            for a frame). Update the text in place; the pill itself
            already does the per-click feedback via ReactionBurst. */}
        <span className="reaction-count">{r.count}</span>
        {burst > 0 && <ReactionBurst key={burst} />}
      </button>
      {coord && orderedNames.length > 0 && createPortal(
        <ReactionTooltip emoji={r.emoji} names={orderedNames} anchorX={coord.x} anchorY={coord.y} />,
        document.body,
      )}
    </>
  )
}

function ReactionBurst() {
  return (
    <span className="reaction-burst" aria-hidden="true">
      <span />
      <span />
      <span />
      <span />
    </span>
  )
}

export function QuickReactionButton({ msgId, emoji }: { msgId: string; emoji: string }) {
  const [burst, setBurst] = useState(0)
  return (
    <button
      onClick={() => {
        setBurst((n) => n + 1)
        void toggleReaction(msgId, emoji)
      }}
      className="reaction-control reaction-quick-button w-6 h-6 rounded-full hover:bg-sky2-50 grid place-items-center"
      title={`React ${emoji}`}
      aria-label={`React ${emoji}`}
    >
      <TwEmoji emoji={emoji} size={16} />
      {burst > 0 && <ReactionBurst key={burst} />}
    </button>
  )
}


function ReactionTooltip({ emoji, names, anchorX, anchorY }: {
  emoji: string
  names: string[]
  /** anchor center-x in viewport coords */
  anchorX: number
  /** anchor top-y in viewport coords (we render above) */
  anchorY: number
}) {
  const ref = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState<{ left: number; top: number; arrowX: number } | null>(null)

  // Measure the tooltip after it's mounted, then reposition it relative to
  // the anchor and clamp inside the viewport. useLayoutEffect runs before the
  // browser paints, so the user never sees the initial offscreen state.
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const r = el.getBoundingClientRect()
    const margin = 8
    const vw = window.innerWidth
    let left = anchorX - r.width / 2
    if (left < margin) left = margin
    if (left + r.width > vw - margin) left = vw - r.width - margin
    const top = anchorY - r.height - 8        // 8px gap above the pill
    const arrowX = anchorX - left              // arrow stays under the pill center
    setPos({ left, top, arrowX })
  }, [anchorX, anchorY, names.join(',')])

  return (
    <div
      ref={ref}
      role="tooltip"
      className="pointer-events-none fixed z-[70]"
      style={{
        left: pos?.left ?? -9999,
        top: pos?.top ?? -9999,
        opacity: pos ? 1 : 0,
        transition: 'opacity 120ms ease-out',
        maxWidth: 320,
      }}
    >
      <div
        className="text-[11.5px] py-1.5 px-2.5 rounded-lg shadow-lg text-white inline-flex items-center"
        style={{ background: 'rgba(15, 30, 50, 0.92)', backdropFilter: 'blur(6px)' }}
      >
        <TwEmoji emoji={emoji} size={14} className="mr-1.5" />
        <span className="font-medium whitespace-nowrap">{names.join(', ')}</span>
      </div>
      <div
        className="w-2 h-2 rotate-45 -mt-1 absolute"
        style={{
          left: (pos?.arrowX ?? 0) - 4,
          background: 'rgba(15, 30, 50, 0.92)',
        }}
      />
    </div>
  )
}
