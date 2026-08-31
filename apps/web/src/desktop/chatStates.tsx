import { useEffect, useMemo, useRef, useState } from 'react'
import { useConversations } from '@/stores/conversations'
import { useT } from '@/lib/i18n'



export function ThreadLoader() {
  const t = useT()
  return (
    <div
      className="grid place-items-center py-16"
      style={{ animation: 'cumora-empty-in 280ms ease-out both' }}
    >
      <div className="flex flex-col items-center gap-4">
        <div className="relative w-14 h-14 grid place-items-center">
          {/* Ambient halo behind the dots */}
          <span
            className="absolute inset-0 rounded-full"
            style={{
              background: 'radial-gradient(circle, rgba(0, 168, 240, 0.18), transparent 70%)',
              animation: 'cumora-halo 2.4s ease-in-out infinite',
            }}
          />
          <div className="relative flex items-end gap-[5px] h-3">
            {[0, 1, 2].map((i) => (
              <span
                key={i}
                className="w-[7px] h-[7px] rounded-full"
                style={{
                  background: 'var(--skype)',
                  boxShadow: '0 1px 4px rgba(0, 168, 240, 0.45)',
                  animation: 'cumora-pulse-dot 1.2s ease-in-out infinite',
                  animationDelay: `${i * 160}ms`,
                }}
              />
            ))}
          </div>
        </div>
        <div className="font-display italic text-[13px] text-ink-500 tracking-tight">
          {t('chat.gathering')}
        </div>
      </div>
    </div>
  )
}


export function ThreadError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const t = useT()
  const [retrying, setRetrying] = useState(false)
  const handleRetry = async () => {
    if (retrying) return
    setRetrying(true)
    try { await onRetry() } finally { setRetrying(false) }
  }
  return (
    <div
      className="grid place-items-center py-12 px-6"
      style={{ animation: 'cumora-empty-in 280ms ease-out both' }}
    >
      <div
        className="flex flex-col items-center text-center max-w-[340px] gap-3 rounded-2xl px-6 py-6 backdrop-blur-sm"
        style={{
          background: 'linear-gradient(180deg, rgba(255, 255, 255, 0.72), rgba(255, 217, 210, 0.18))',
          border: '1px solid rgba(255, 122, 107, 0.18)',
          boxShadow: '0 12px 32px -16px rgba(200, 78, 63, 0.25)',
        }}
      >
        <div
          className="w-10 h-10 rounded-full grid place-items-center"
          style={{
            background: 'rgba(255, 122, 107, 0.12)',
            color: 'var(--coral-deep)',
          }}
        >
          <svg viewBox="0 0 24 24" fill="none" className="w-[18px] h-[18px]" aria-hidden>
            <path d="M12 8.5v4.5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
            <circle cx="12" cy="16.2" r="1" fill="currentColor" />
            <path
              d="M10.6 3.6a1.6 1.6 0 0 1 2.8 0l8.1 14.4a1.6 1.6 0 0 1-1.4 2.4H3.9a1.6 1.6 0 0 1-1.4-2.4l8.1-14.4Z"
              stroke="currentColor" strokeWidth="1.6" strokeLinejoin="round"
            />
          </svg>
        </div>
        <div className="font-display font-medium text-[15px] tracking-tight text-ink-700">
          {t('chat.loadFailed')}
        </div>
        <div className="text-[12.5px] text-ink-500 leading-relaxed break-words">
          {message}
        </div>
        <button
          onClick={handleRetry}
          disabled={retrying}
          className="mt-1 h-[30px] px-3.5 rounded-full font-semibold text-[12px] text-white inline-flex items-center gap-1.5 transition disabled:cursor-not-allowed"
          style={{
            background: retrying ? 'var(--ink-300)' : 'var(--skype)',
            boxShadow: retrying ? 'none' : '0 4px 12px -3px rgba(0, 168, 240, 0.5)',
          }}
        >
          {retrying ? (
            <>
              <span className="w-3 h-3 rounded-full border-2 border-white/40 border-t-white animate-spin" />
              {t('chat.retrying')}
            </>
          ) : (
            <>
              <svg viewBox="0 0 24 24" fill="none" className="w-3.5 h-3.5" aria-hidden>
                <path
                  d="M4 12a8 8 0 0 1 13.7-5.7L20 8M20 4v4h-4M20 12a8 8 0 0 1-13.7 5.7L4 16M4 20v-4h4"
                  stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round"
                />
              </svg>
              {t('chat.tryAgain')}
            </>
          )}
        </button>
      </div>
    </div>
  )
}


export function EmptyConversationState() {
  const t = useT()
  // Live counts pulled straight from the store so the empty stage carries
  // one tiny piece of "alive" data at the bottom — matches the inline
  // italic counter pattern in WhispersView's sidebar header.
  const list = useConversations((s) => s.list)
  const total = list.length
  const unread = useMemo(
    () => list.reduce((n, c) => n + (c.muted ? 0 : (c.unread ?? 0)), 0),
    [list],
  )

  // Click "pop" reaction: cloud bounces + tilts when you tap it. The
  // animation runs once via a CSS keyframe; we toggle the boolean back
  // off after the keyframe duration so the next click can re-trigger.
  // Ignoring re-clicks while popping is intentional — a rapid click
  // mid-animation looks like jitter, not playfulness.
  const [popping, setPopping] = useState(false)
  const onCloudPoke = () => {
    if (popping) return
    setPopping(true)
    window.setTimeout(() => setPopping(false), 760)
    // Treat a poke as a little surprise — blink once shortly after.
    triggerBlink()
  }

  // Periodic blink: every 4-7 s the cloud closes its eyes briefly.
  // The closed-eye frame is /cloud-blink.png — same cloud, generated by
  // gpt-image-2's /images/edits with a mask covering only the eye
  // regions. Animation is driven by the `cumora-eye-blink` keyframe
  // (asymmetric close-fast / open-slow timing) — see globals.css. The
  // hold duration here must match the keyframe duration so the React
  // state clears at the same moment the animation ends.
  const BLINK_MS = 220
  const [blinking, setBlinking] = useState(false)
  const blinkResetRef = useRef<number | null>(null)
  const triggerBlink = () => {
    setBlinking(true)
    if (blinkResetRef.current !== null) window.clearTimeout(blinkResetRef.current)
    blinkResetRef.current = window.setTimeout(() => {
      setBlinking(false)
      blinkResetRef.current = null
    }, BLINK_MS)
  }
  useEffect(() => {
    // Schedule the next blink at a randomized 4-7 s interval. After it
    // fires, the timeout reschedules itself recursively, which lets us
    // re-randomize on each tick instead of running on a fixed cadence
    // (fixed cadence looks robotic — real eye blinks are irregular).
    let cancelled = false
    let timer: number | null = null
    const schedule = () => {
      const delay = 4000 + Math.random() * 3000
      timer = window.setTimeout(() => {
        if (cancelled) return
        triggerBlink()
        // Occasionally do a double-blink for extra personality. Gap is
        // tuned to start the second blink JUST after the first one ends
        // (220ms keyframe + ~70ms beat) so it reads as "blink-blink"
        // rather than two unrelated blinks.
        if (Math.random() < 0.18) {
          window.setTimeout(() => { if (!cancelled) triggerBlink() }, 290)
        }
        schedule()
      }, delay)
    }
    schedule()
    return () => {
      cancelled = true
      if (timer !== null) window.clearTimeout(timer)
      if (blinkResetRef.current !== null) window.clearTimeout(blinkResetRef.current)
    }
  }, [])


  return (
    <main
      className="relative overflow-hidden"
      style={{
        // Sky pocket centered on the cloud — the previous linear
        // top→bottom gradient put pale sky-blue along the WHOLE top
        // edge, which collided visually with the white conversations
        // sidebar to its left and read as a hard seam. A radial
        // gradient concentrated where the cloud actually sits keeps
        // the sky atmosphere around the mascot but fades to paper at
        // every pane edge, so the boundary with the sidebar is paper
        // meeting white instead of sky-blue meeting white.
        background:
          'radial-gradient(ellipse 60% 55% at 50% 40%,' +
          ' var(--sky-100) 0%,' +
          ' var(--sky-50) 45%,' +
          ' var(--paper) 100%)',
      }}
    >
      {/* Scroll container — the previous version centered with `grid
          place-items-center` directly on <main>, which clipped the title
          on shorter windows because excess content overflowed equally
          top + bottom. This pattern (`min-h-full` + `place-items: center`
          inside an overflow-y-auto wrapper) keeps content centered when
          it fits and falls back to scroll-from-top when it doesn't. */}
      <div className="absolute inset-0 overflow-y-auto">
        <div className="relative min-h-full grid place-items-center px-6 py-12">
          <div
            className="flex flex-col items-center text-center max-w-md"
            style={{ animation: 'cumora-empty-in 480ms cubic-bezier(0.2, 0.8, 0.2, 1) both' }}
          >
            {/* Hero — Cumora's mascot cloud. The PNG was rendered offline
                with gpt-image-2 against a magenta backdrop and chroma-
                keyed to a transparent silhouette (see
                /tmp/gen-cumora-cloud.py for the prompt + extraction
                pipeline). Against the now-flat cloud-white background
                the cloud lives unadorned — no halo, no aura, no tinted
                wash. The kawaii face + plush shading is enough on its
                own; anything extra reads as fussy decoration. */}
            <div className="relative mb-9" style={{ width: 300, height: 220 }} aria-hidden>
              {/* The cloud — a slow ambient bob restores the light,
                  breezy mascot feel without turning the empty screen
                  into a loader. Sized to the PNG's ~1.36:1 aspect ratio
                  (the fluffier cloud is taller than the original
                  wide-cumulus take).
                  Two-layer transform: the OUTER div carries the ambient
                  placement; the INNER button carries hover-scale and
                  click-pop. Splitting the concerns keeps interaction
                  transforms independent from layout. */}
              <div
                className="absolute cumora-cloud-float"
                style={{
                  left: 30, top: 16,
                  width: 240, height: 176,
                }}
              >
                <button
                  type="button"
                  onClick={onCloudPoke}
                  aria-label={t('chat.helloCloud')}
                  className="cumora-cloud-poke group block w-full h-full cursor-pointer p-0 border-0 bg-transparent focus:outline-none"
                  style={{
                    // Silky-spring transition. The hover-state CSS rule
                    // sets target scale(1.03); this transition tweens
                    // BOTH directions (mouseenter AND mouseleave) with
                    // the same easing — important so the return feels
                    // as elegant as the entry. cubic-bezier (0.34, 1.5,
                    // 0.4, 1) is a soft spring: 0.5× overshoot ramp,
                    // settling cleanly. Long 540ms so the gentle
                    // overshoot is felt as Q-bounce rather than tween.
                    transition: popping
                      ? undefined
                      : 'transform 540ms cubic-bezier(0.34, 1.5, 0.4, 1)',
                    transformOrigin: 'center 65%',
                    willChange: popping ? 'transform' : undefined,
                    // Click pop — same gentler curve as hover-return so
                    // the whole motion vocabulary feels consistent.
                    animation: popping
                      ? 'cumora-cloud-pop 760ms cubic-bezier(0.34, 1.5, 0.4, 1) both'
                      : undefined,
                  }}
                >
                  {/* Single-base + eye-only overlay. Previously we
                      crossfaded between two FULL PNG frames; even
                      though they look identical outside the eyes,
                      gpt-image-2 re-encodes the whole image during a
                      masked edit and produces sub-pixel shifts across
                      the body, so the crossfade made the whole cloud
                      "shimmer" on every blink. Fix: cloud.png is the
                      permanent base, always at opacity 1. cloud-blink
                      .png sits on top but its CSS mask reveals ONLY
                      a small ellipse over the eye band, so the body
                      pixels are guaranteed to come from a single
                      source even during a blink. */}
                  <div className="relative w-full h-full">
                    <img
                      src="/cloud.png"
                      alt=""
                      width={240}
                      height={176}
                      draggable={false}
                      style={{
                        position: 'absolute',
                        inset: 0,
                        width: '100%',
                        height: '100%',
                        objectFit: 'contain',
                        filter:
                          'drop-shadow(0 18px 28px rgba(94, 168, 215, 0.18))' +
                          'drop-shadow(0 6px 12px rgba(94, 168, 215, 0.10))',
                      }}
                    />
                    <img
                      src="/cloud-blink.png"
                      alt=""
                      width={240}
                      height={176}
                      draggable={false}
                      style={{
                        position: 'absolute',
                        inset: 0,
                        width: '100%',
                        height: '100%',
                        objectFit: 'contain',
                        // Driven by a keyframe (cumora-eye-blink) rather
                        // than a symmetric opacity transition. The
                        // keyframe enforces asymmetric close-fast /
                        // open-slow timing — see globals.css. When the
                        // animation prop becomes `undefined` between
                        // blinks the element snaps back to opacity 0
                        // (its rest style), which is exactly what we
                        // want for the next trigger to start clean.
                        opacity: 0,
                        animation: blinking
                          ? 'cumora-eye-blink 220ms ease-in-out both'
                          : undefined,
                        willChange: blinking ? 'opacity' : undefined,
                        // Mask reveals only the eye band — radial
                        // ellipse centered on the eye row. Position +
                        // size are tuned to the deployed cloud.png:
                        // run scipy connected-components on it (see
                        // gen-cumora-blink.py for the detection
                        // recipe) to find eye centroids, then express
                        // (cx, cy, half-width, half-height) as % of
                        // the image dimensions.
                        //   batch1+v14 eyes (current): eyes at cx 50%,
                        //   cy 52%, ~48×46 px each in 512×339 → mask
                        //   45%×20% at (50%, 52%). Previously was
                        //   40%×16% at (50%, 67%) for the v10-family
                        //   clouds whose faces sat lower in frame.
                        WebkitMaskImage:
                          'radial-gradient(ellipse 45% 20% at 50% 52%, #000 0%, #000 35%, transparent 92%)',
                        maskImage:
                          'radial-gradient(ellipse 45% 20% at 50% 52%, #000 0%, #000 35%, transparent 92%)',
                        // No drop-shadow on this layer — the base
                        // cloud already casts the shadow; doubling it
                        // would show during a blink as a dark rim.
                      }}
                    />
                  </div>
                </button>
              </div>

              {/* Gold ★ — perched on the cloud's upper-right shoulder
                  like a tiny ornament. Asymmetric placement reads CUTE
                  rather than POSED. */}
              <span
                className="absolute font-display select-none leading-none"
                style={{
                  top: 6, right: 42,
                  fontSize: 18,
                  color: 'var(--gold)',
                  textShadow:
                    '0 0 14px rgba(244, 183, 64, 0.72),' +
                    '0 2px 4px rgba(186, 132, 24, 0.38)',
                }}
              >★</span>

              {/* One barely-there ✦ drifting on the opposite side, to
                  balance the star without crowding the scene. */}
              <span
                className="absolute font-display select-none leading-none"
                style={{
                  bottom: 18, left: 38,
                  fontSize: 11,
                  color: 'var(--gold)',
                  opacity: 0.55,
                  textShadow: '0 0 6px rgba(244, 183, 64, 0.50)',
                }}
              >✦</span>
            </div>

            <h2
              className="font-display font-medium text-[28px] text-ink-900 leading-[1.12]"
              style={{ letterSpacing: '-0.025em' }}
            >
              {t('chat.emptyTitle')}
            </h2>
            <p className="mt-2.5 font-display italic text-[14px] text-ink-500 leading-relaxed max-w-[360px]">
              {t('chat.emptySub')}
            </p>

            {total > 0 && (
              <div className="mt-6 text-[12px] text-ink-400 font-display italic flex items-center gap-1.5">
                <span className="text-gold leading-none not-italic" style={{ fontSize: 10 }}>★</span>
                <b className="not-italic text-ink-700 font-semibold tabular-nums">{total}</b>
                <span>{t(total === 1 ? 'chat.threadWaiting' : 'chat.threadsWaiting')}</span>
                {unread > 0 && (
                  <>
                    <span className="text-ink-200" aria-hidden>·</span>
                    <b className="not-italic text-coral-deep font-semibold tabular-nums">{unread}</b>
                    <span>{t('chat.unread')}</span>
                  </>
                )}
              </div>
            )}
          </div>
        </div>
      </div>
    </main>
  )
}
