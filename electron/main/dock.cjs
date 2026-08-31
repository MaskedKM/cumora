/* eslint-env node */
// =============== macOS Dock: activation policy + icon badge ===============
// Split out of main.cjs (#219⑥). Owns the activation-policy cache/repair
// machinery (used by notify.cjs / ipc.cjs around panel shows) and the
// dock unread-dot icon painting.
const { app, nativeImage } = require('electron')
const { ICON_PATH } = require('./env.cjs')
const state = require('./state.cjs')
const { setTrayUnreadDot } = require('./tray.cjs')

// Track whether we already pinned the app to `regular`. The previous
// implementation re-applied `setActivationPolicy('regular')` on every
// window show/hide/closed event — 5 staggered times per call — which
// caused AppKit to re-render NSWindow shadows each invocation, producing
// the visible "main window shadow flickers many times on open" bug
// (~15 calls inside the first 3s of boot). We now mark the policy known-
// regular and skip the AppKit call entirely until something explicitly
// invalidates it (i.e. the notification panel is about to show, which is
// the only known trigger for AppKit to silently demote us to accessory).
let activationPolicyKnownRegular = false

function ensureRegularDock() {
  if (process.platform !== 'darwin') return
  if (activationPolicyKnownRegular) return
  try {
    app.setActivationPolicy('regular')
    activationPolicyKnownRegular = true
  } catch { /* swallow */ }
  try {
    if (app.dock) void app.dock.show().catch(() => {})
  } catch { /* swallow */ }
}

/** Mark the cached "we're regular" flag stale ahead of an event we know
 *  can cause AppKit to silently demote us — currently just the
 *  notification panel showing. Forces the next `ensureRegularDock()` /
 *  staggered repair to actually re-apply the activation policy. */
function invalidateActivationPolicyCache() {
  activationPolicyKnownRegular = false
}

function scheduleRegularDockRepair() {
  // Only do work when the cache wasn't already regular at entry — that's
  // the only case where we need to defend against AppKit demoting us
  // asynchronously after this event (notification panel show). When we
  // were already known-regular, this call is a no-op and we skip the
  // staggered timers entirely; that's what fixes the on-open shadow
  // flicker (previously: 5 staggered setActivationPolicy calls per
  // window event, AppKit re-rendered NSWindow shadows each time).
  const needsApply = !activationPolicyKnownRegular
  ensureRegularDock()
  if (!needsApply) return
  // Defense against silent demotion: AppKit may recompute activation
  // policy a few hundred ms after a panel show. Re-invalidate the cache
  // inside each timer so the repair actually re-applies (idempotent at
  // worst, repaired-in-time at best).
  for (const delay of [50, 250, 1000, 3000]) {
    const timer = setTimeout(() => {
      invalidateActivationPolicyCache()
      ensureRegularDock()
    }, delay)
    if (typeof timer.unref === 'function') timer.unref()
  }
}

const DOCK_ICON_SIZE = 1024
const DOCK_UNREAD_DOT = {
  cx: 770,
  cy: 232,
  r: 48,
  stroke: 18,
}

let dockCleanIcon = null
let dockUnreadIcon = null

function getDockCleanIcon() {
  if (!dockCleanIcon) dockCleanIcon = nativeImage.createFromPath(ICON_PATH)
  return dockCleanIcon
}

/** Paint the unread dot straight into an app-icon bitmap, in place.
 *
 *  Same raw-bitmap route as the tray below, for the same reason its comment
 *  gives: `nativeImage.createFromDataURL` only decodes raster formats Chromium
 *  knows (PNG/JPEG/WebP/GIF), so the SVG this used to build produced an EMPTY
 *  image — `isEmpty()` true at 0x0. That tripped the `!img.isEmpty()` guard in
 *  setDockUnreadDot, which skipped `setIcon` entirely, so the dock never showed
 *  any unread indicator while the tray (already on bitmaps) did.
 *
 *  Buffer layout is BGRA with PREMULTIPLIED alpha (Chromium's N32). Blending
 *  `c*a + dst*(1-a)` on all four channels preserves that invariant, since a
 *  premultiplied channel never exceeds its alpha.
 *
 *  Dot geometry stays in the existing 1024-unit design space and is scaled to
 *  whatever the icon's real pixel size is, so DOCK_UNREAD_DOT keeps its
 *  meaning. */
function paintDockUnreadDot(buf, width, height) {
  const s = Math.min(width, height) / DOCK_ICON_SIZE
  const dot = DOCK_UNREAD_DOT
  const cx = dot.cx * s
  const cy = dot.cy * s
  const rIn = dot.r * s
  const rOut = (dot.r + dot.stroke / 2) * s
  const SS = 4  // 4x4 supersample — the same cheap anti-aliasing the tray uses
  const n = SS * SS
  const blend = (i, b, g, r, a) => {
    if (a <= 0) return
    const inv = 1 - a
    buf[i] = Math.round(b * a + buf[i] * inv)
    buf[i + 1] = Math.round(g * a + buf[i + 1] * inv)
    buf[i + 2] = Math.round(r * a + buf[i + 2] * inv)
    buf[i + 3] = Math.round(255 * a + buf[i + 3] * inv)
  }
  const x0 = Math.max(0, Math.floor(cx - rOut - 1))
  const x1 = Math.min(width - 1, Math.ceil(cx + rOut + 1))
  const y0 = Math.max(0, Math.floor(cy - rOut - 1))
  const y1 = Math.min(height - 1, Math.ceil(cy + rOut + 1))
  for (let y = y0; y <= y1; y++) {
    for (let x = x0; x <= x1; x++) {
      let covIn = 0
      let covOut = 0
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          const dx = x + (sx + 0.5) / SS - cx
          const dy = y + (sy + 0.5) / SS - cy
          const d2 = dx * dx + dy * dy
          if (d2 <= rOut * rOut) covOut += 1
          if (d2 <= rIn * rIn) covIn += 1
        }
      }
      if (covOut === 0) continue
      const i = (y * width + x) * 4
      // White halo first, then the red dot on top — same z-order as before.
      blend(i, 255, 255, 255, (covOut / n) * 0.96)
      blend(i, 0x30, 0x3b, 0xff, covIn / n)
    }
  }
}

function getDockUnreadIcon() {
  if (dockUnreadIcon) return dockUnreadIcon
  const base = nativeImage.createFromPath(ICON_PATH)
  const { width, height } = base.getSize()
  // No icon on disk (or an undecodable one) — hand back the empty image and let
  // setDockUnreadDot's isEmpty() guard skip the update, exactly as before.
  if (!width || !height) return base
  const buf = base.toBitmap()
  paintDockUnreadDot(buf, width, height)
  dockUnreadIcon = nativeImage.createFromBitmap(buf, { width, height })
  return dockUnreadIcon
}

function setDockUnreadDot(visible) {
  state.dockUnreadDotVisible = !!visible
  if (process.platform !== 'darwin' || !app.dock) return
  try {
    app.dock.setBadge('')
    const img = state.dockUnreadDotVisible ? getDockUnreadIcon() : getDockCleanIcon()
    if (!img.isEmpty()) app.dock.setIcon(img)
  } catch { /* swallow — Dock is macOS-only and not critical path */ }
  scheduleRegularDockRepair()
  // Mirror to the system tray so menu bar / system tray surfaces stay
  // in sync with the dock. Cheap to call when there's no tray (no-op).
  setTrayUnreadDot(state.dockUnreadDotVisible)
}

module.exports = {
  ensureRegularDock,
  invalidateActivationPolicyCache,
  scheduleRegularDockRepair,
  setDockUnreadDot,
}
