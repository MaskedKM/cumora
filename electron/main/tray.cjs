/* eslint-env node */
// =============== System tray ===============
// Split out of main.cjs (#219⑥).
const { app, Tray, Menu, nativeImage } = require('electron')
const path = require('node:path')
const state = require('./state.cjs')
const { createWindow } = require('./window.cjs')

/* ============================ System tray ============================
 * Menu bar item on macOS, system tray on Win/Linux. Same unread-dot
 * indicator the dock uses (we reuse `dockUnreadDotVisible` as the
 * authoritative state). Click toggles the main window; right-click
 * shows a context menu with Open / Hide / Quit. Closing the X on the
 * main window hides instead of quitting (true quit is via tray menu
 * Quit or Cmd-Q / window-all-closed on Win+Linux). */
let trayCleanIcon = null
let trayUnreadIcon = null

/** Hand-rasterise a cloud silhouette to a BGRA pixel buffer and feed
 *  it to `nativeImage.createFromBitmap`. Why the bytes-level approach:
 *  `nativeImage.createFromDataURL` only decodes raster formats Chromium
 *  knows (PNG/JPEG/WebP/GIF) — SVG data URLs silently produce an empty
 *  image, which is why the old SVG-based tray icon never appeared. Raw
 *  bitmap input has none of those decoder constraints.
 *
 *  Shape: three overlapping circles (left/center/right lobes) on top
 *  of a rounded baseline rectangle — the classic cloud-glyph union,
 *  tuned to read like the brand's pudgy cloud at 22pt menu bar size.
 *  All fill is pure black (B=G=R=0); we drive macOS template-image
 *  inversion via `setTemplateImage(true)` after the fact. The unread
 *  variant adds a small filled circle outside the silhouette in the
 *  upper-right — still pure black so the same template-inversion
 *  pipeline keeps working in dark mode. */
const TRAY_ICON_SIZE = 64  // generation size; menu bar downsamples to 22pt automatically

function buildTrayCloudBitmap({ dot = false } = {}) {
  const size = TRAY_ICON_SIZE
  // Design space is 64 units; scale to the chosen pixel size so we can
  // tweak TRAY_ICON_SIZE later (e.g. 128 for sharper retina) without
  // rewriting coordinates.
  const s = size / 64
  const lobes = [
    { cx: 20 * s, cy: 34 * s, r: 10 * s },
    { cx: 32 * s, cy: 22 * s, r: 14 * s },
    { cx: 46 * s, cy: 30 * s, r: 11 * s },
  ]
  const base = { x: 10 * s, y: 32 * s, w: 44 * s, h: 20 * s, r: 10 * s }
  const dotCircle = dot ? { cx: 56 * s, cy: 10 * s, r: 7 * s } : null

  const insideCircle = (px, py, c) => {
    const dx = px - c.cx, dy = py - c.cy
    return dx * dx + dy * dy <= c.r * c.r
  }
  // Rounded-rect inclusion: clamp the test point to the rect's inner
  // axis-aligned bounding box (deflated by corner radius) and check
  // distance from that clamped point — exactly the standard SDF for a
  // rounded box at distance ≤ r.
  const insideRoundedRect = (px, py, b) => {
    const closestX = Math.max(b.x + b.r, Math.min(px, b.x + b.w - b.r))
    const closestY = Math.max(b.y + b.r, Math.min(py, b.y + b.h - b.r))
    const dx = px - closestX, dy = py - closestY
    if (dx * dx + dy * dy <= b.r * b.r) return true
    return px >= b.x && px <= b.x + b.w && py >= b.y && py <= b.y + b.h &&
           ((px >= b.x + b.r && px <= b.x + b.w - b.r) || (py >= b.y + b.r && py <= b.y + b.h - b.r))
  }
  const inside = (px, py) => {
    for (const c of lobes) if (insideCircle(px, py, c)) return true
    if (insideRoundedRect(px, py, base)) return true
    if (dotCircle && insideCircle(px, py, dotCircle)) return true
    return false
  }

  const buf = Buffer.alloc(size * size * 4)
  // 4×4 supersample for cheap anti-aliasing — at 64×64 this is ~16k
  // shape-tests, single-digit ms in V8, runs once at boot and once on
  // first unread. Worth the crispness.
  const SS = 4
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      let hit = 0
      for (let sy = 0; sy < SS; sy++) {
        for (let sx = 0; sx < SS; sx++) {
          const px = x + (sx + 0.5) / SS
          const py = y + (sy + 0.5) / SS
          if (inside(px, py)) hit++
        }
      }
      const alpha = Math.round((hit / (SS * SS)) * 255)
      const idx = (y * size + x) * 4
      buf[idx + 0] = 0      // B
      buf[idx + 1] = 0      // G
      buf[idx + 2] = 0      // R
      buf[idx + 3] = alpha  // A — coverage from the supersample
    }
  }
  return buf
}

function makeTrayImageFromBitmap(buffer) {
  const img = nativeImage.createFromBitmap(buffer, { width: TRAY_ICON_SIZE, height: TRAY_ICON_SIZE })
  // Mark as template ONLY on macOS — AppKit then handles all theming
  // (light/dark menu bar, highlight on click) automatically. On
  // Win/Linux template-ness is a no-op; the black silhouette renders
  // as-is, which is the standard look on those platforms too.
  if (process.platform === 'darwin') {
    try { img.setTemplateImage(true) } catch { /* swallow */ }
  }
  return img
}

/** Load a pre-rendered tray template PNG. The cloud silhouette + the
 *  unread-badge variant are baked from `public/logo.png` (the real
 *  brand cloud — alpha-transparent background, cloud body as the
 *  largest connected component) by `scripts-gen-tray-icons.py` and
 *  committed under `build/`:
 *
 *    build/tray-template.png         (22×22 — 1×)
 *    build/tray-template@2x.png      (44×44 — Retina)
 *    build/tray-template@3x.png      (66×66 — XDR)
 *    build/tray-template-unread.png  (+ @2x / @3x variants)
 *
 *  Re-run that script after touching `public/logo.png` and the menu
 *  bar icon tracks. Why baked-on-disk rather than runtime composition:
 *    • `nativeImage.createFromDataURL('data:image/svg+xml,…')` returns
 *      an empty image — Electron decodes only PNG/JPEG/WebP/GIF from
 *      data URLs, so SVG silently fails (which is why the tray icon
 *      disappeared in earlier attempts).
 *    • Largest-connected-component extraction is O(width·height) —
 *      fine once at build time, wasteful on every Electron boot.
 *    • Electron auto-picks the right `@2x`/`@3x` sibling for the
 *      screen's scale factor when you call createFromPath on the 1×
 *      base, so we get pixel-perfect rendering on every display with
 *      zero runtime cost. */
function loadTrayTemplate(suffix) {
  const p = path.join(app.getAppPath(), 'build', `tray-template${suffix}.png`)
  const img = nativeImage.createFromPath(p)
  if (img.isEmpty()) return null
  if (process.platform === 'darwin') {
    // AppKit handles light/dark menu-bar inversion + click-highlight
    // tinting automatically once we mark this as a template. Off-darwin
    // it's a no-op; the black silhouette renders as-is, which is the
    // standard look on Win/Linux system trays too.
    try { img.setTemplateImage(true) } catch { /* swallow */ }
  }
  return img
}

function getTrayCleanIcon() {
  if (trayCleanIcon) return trayCleanIcon
  trayCleanIcon = loadTrayTemplate('') ?? makeTrayImageFromBitmap(buildTrayCloudBitmap({ dot: false }))
  return trayCleanIcon
}

function getTrayUnreadIcon() {
  if (trayUnreadIcon) return trayUnreadIcon
  trayUnreadIcon = loadTrayTemplate('-unread') ?? makeTrayImageFromBitmap(buildTrayCloudBitmap({ dot: true }))
  return trayUnreadIcon
}

function setTrayUnreadDot(visible) {
  if (!state.tray || state.tray.isDestroyed?.()) return
  try {
    const img = visible ? getTrayUnreadIcon() : getTrayCleanIcon()
    if (!img.isEmpty()) state.tray.setImage(img)
  } catch { /* swallow — tray isn't critical path */ }
}

function toggleMainWindowVisibility() {
  if (!state.mainWindow || state.mainWindow.isDestroyed()) {
    createWindow()
    return
  }
  if (state.mainWindow.isVisible() && state.mainWindow.isFocused()) {
    state.mainWindow.hide()
    return
  }
  if (state.mainWindow.isMinimized()) state.mainWindow.restore()
  state.mainWindow.show()
  state.mainWindow.focus()
}

let stackDegraded = false

/** 栈降级警示(#286):tooltip 打标 + 菜单首行红字项(点击 = 开窗,
 * 渲染端的 Me>Stack 面板在窗里)。非关键路径,任何 tray 缺席都吞掉。 */
function setStackDegraded(visible) {
  stackDegraded = !!visible
  if (!state.tray || state.tray.isDestroyed?.()) return
  try {
    state.tray.setToolTip(stackDegraded ? 'Cumora — ⚠ 本地栈已降级' : 'Cumora')
    if (process.platform !== 'darwin') state.tray.setContextMenu(buildTrayMenu())
  } catch { /* swallow */ }
}

function buildTrayMenu() {
  const items = []
  if (stackDegraded) {
    items.push({ label: '⚠ 本地栈已降级 — 打开管理面', click: () => {
      if (!state.mainWindow || state.mainWindow.isDestroyed()) { createWindow(); return }
      if (state.mainWindow.isMinimized()) state.mainWindow.restore()
      state.mainWindow.show()
      state.mainWindow.focus()
    } })
    items.push({ type: 'separator' })
  }
  return Menu.buildFromTemplate([
    ...items,
    { label: `Cumora v${app.getVersion()}`, enabled: false },
    { type: 'separator' },
    { label: 'Open Cumora', click: () => {
      if (!state.mainWindow || state.mainWindow.isDestroyed()) { createWindow(); return }
      if (state.mainWindow.isMinimized()) state.mainWindow.restore()
      state.mainWindow.show()
      state.mainWindow.focus()
    } },
    { label: 'Hide Window', click: () => {
      if (state.mainWindow && !state.mainWindow.isDestroyed() && state.mainWindow.isVisible()) state.mainWindow.hide()
    } },
    { type: 'separator' },
    { label: 'Quit Cumora', click: () => {
      // before-quit will set appIsQuitting=true and tear down the
      // notification window + auth loopback server; this is the only
      // legitimate exit path that bypasses the close-to-hide trap.
      app.quit()
    } },
  ])
}

function createTray() {
  if (state.tray) return
  const img = getTrayCleanIcon()
  if (img.isEmpty()) {
    console.warn('[tray] no icon — skipping tray creation')
    return
  }
  state.tray = new Tray(img)
  state.tray.setToolTip('Cumora')
  // macOS: don't `setContextMenu` — Electron's docs are explicit that
  // doing so swallows the `click` event entirely, making left-click
  // toggle-window impossible. Instead handle `click` ourselves and
  // pop the menu manually on `right-click`.
  state.tray.on('click', toggleMainWindowVisibility)
  state.tray.on('right-click', () => {
    try { state.tray.popUpContextMenu(buildTrayMenu()) } catch { /* swallow */ }
  })
  if (process.platform !== 'darwin') {
    // Win/Linux: setContextMenu drives the right-click menu reliably
    // across DEs; left-click still hits our `click` handler above.
    state.tray.setContextMenu(buildTrayMenu())
  }
  // Restore unread state in case set-unread-dot was called before tray
  // existed (dock was, but tray wasn't — keep the two in lockstep).
  setTrayUnreadDot(state.dockUnreadDotVisible)
}

module.exports = { setTrayUnreadDot, setStackDegraded, createTray }
