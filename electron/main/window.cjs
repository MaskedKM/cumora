/* eslint-env node */
// =============== Main window + geometry persistence ===============
// Split out of main.cjs (#219⑥).
const { app, BrowserWindow, shell, screen } = require('electron')
const path = require('node:path')
const fs = require('node:fs')
const { isDev, DEV_URL, ICON_PATH } = require('./env.cjs')
const state = require('./state.cjs')

/** Persistent main-window geometry. Saved on every resize/move/close and
 *  restored on launch — so users get the size + position they had last,
 *  and first-run gets a sensible non-fullscreen default. */
const WINDOW_STATE_PATH = () => path.join(app.getPath('userData'), 'window-state.json')
const DEFAULT_WINDOW_STATE = { width: 1480, height: 920, fullscreen: false, maximized: false }

function readWindowState() {
  try {
    const raw = fs.readFileSync(WINDOW_STATE_PATH(), 'utf8')
    const parsed = JSON.parse(raw)
    if (typeof parsed !== 'object' || parsed === null) return null
    return parsed
  } catch { return null }
}

function writeWindowState(state) {
  try { fs.writeFileSync(WINDOW_STATE_PATH(), JSON.stringify(state)) }
  catch (e) { console.warn('[window-state] save failed', e?.message || e) }
}

/** Returns `{ x, y, width, height }` if the saved rect intersects a
 *  currently-connected display by at least ~80px on each axis, or null
 *  if the saved position is offscreen (monitor disconnected, etc.). */
function visibleRect(saved) {
  if (!saved || typeof saved.x !== 'number' || typeof saved.y !== 'number'
      || typeof saved.width !== 'number' || typeof saved.height !== 'number') return null
  const r = { x: saved.x, y: saved.y, width: saved.width, height: saved.height }
  const displays = screen.getAllDisplays()
  for (const d of displays) {
    const wa = d.workArea
    const interW = Math.min(r.x + r.width, wa.x + wa.width) - Math.max(r.x, wa.x)
    const interH = Math.min(r.y + r.height, wa.y + wa.height) - Math.max(r.y, wa.y)
    if (interW >= 80 && interH >= 80) return r
  }
  return null
}

function createWindow() {
  const saved = readWindowState() ?? DEFAULT_WINDOW_STATE
  const rect = visibleRect(saved)
  // Cap initial size to fit comfortably inside the primary display's
  // work area — 90% of work area, with the configured default as the
  // upper ceiling. Without this, the 1480×920 default would exceed
  // smaller laptop displays and macOS would clamp on launch, making
  // every first-run feel "fullscreen".
  const wa = screen.getPrimaryDisplay().workArea
  const initW = Math.min(saved.width ?? DEFAULT_WINDOW_STATE.width, Math.round(wa.width * 0.9))
  const initH = Math.min(saved.height ?? DEFAULT_WINDOW_STATE.height, Math.round(wa.height * 0.9))
  state.mainWindow = new BrowserWindow({
    width: initW,
    height: initH,
    // Only restore position if the rect is still inside a connected
    // display. Otherwise let the OS center the window — avoids opening
    // off-screen when the user disconnects a monitor.
    ...(rect ? { x: rect.x, y: rect.y } : {}),
    // Constructor flag — overrides macOS NSWindow state restoration's
    // "open fullscreen because it was fullscreen last quit" behaviour.
    // We apply our own fullscreen/maximize state right after `show`.
    fullscreen: false,
    // In dev, relax the floor so the window can shrink past the 768px
    // mobile breakpoint (defined in src/lib/utils.ts) — lets us debug the
    // mobile layout without spinning up a separate device emulator.
    // Production keeps the comfortable desktop floor.
    minWidth: isDev ? 320 : 900,
    minHeight: isDev ? 480 : 600,
    show: false,
    backgroundColor: '#E6F3FB',
    icon: ICON_PATH,
    titleBarStyle: process.platform === 'darwin' ? 'hidden' : 'default',
    trafficLightPosition: { x: 16, y: 15 },
    webPreferences: {
      preload: path.join(__dirname, '..', 'preload.cjs'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      // Chromium throttles timers + some idle work when the window is
      // hidden. We rely on the WS subscriber firing promptly to surface
      // background notifications, so opt out.
      backgroundThrottling: false,
    },
  })

  // Forward native OS-level focus state to the renderer. We can't rely on
  // `document.hasFocus()` from inside the renderer — in Electron on macOS
  // it returns stale `true` values when the window is unfocused, which
  // suppresses the screen-corner notification.
  //
  // We also can't rely on `BrowserWindow.on('blur'/'focus')` alone — on
  // some macOS versions with `titleBarStyle: 'hidden'` those events skip
  // transitions when focus passes through the menu bar or Mission
  // Control. Use both: events fire fast on direct focus changes, and a
  // 500ms heartbeat catches everything the events miss.
  let lastEmittedFocus = null
  const sendFocusState = (focused, _reason) => {
    if (!state.mainWindow || state.mainWindow.isDestroyed()) return
    if (focused === lastEmittedFocus) return
    lastEmittedFocus = focused
    state.mainWindow.webContents.send('app:focus-state', focused)
  }
  const pollFocus = () => {
    if (!state.mainWindow || state.mainWindow.isDestroyed()) return
    sendFocusState(state.mainWindow.isFocused(), 'poll')
  }
  state.mainWindow.on('focus', () => sendFocusState(true, 'focus'))
  state.mainWindow.on('blur', () => sendFocusState(false, 'blur'))
  state.mainWindow.on('show', () => sendFocusState(state.mainWindow.isFocused(), 'show'))
  state.mainWindow.on('hide', () => sendFocusState(false, 'hide'))
  const focusPollTimer = setInterval(pollFocus, 500)

  // Debounced state persistence. We don't write on every pixel of a
  // drag/resize — wait 300ms of quiet then snapshot getNormalBounds (the
  // pre-maximize/fullscreen size, so we restore the user's actual chosen
  // size rather than the maximized rect).
  let saveTimer = null
  const persistState = () => {
    if (!state.mainWindow || state.mainWindow.isDestroyed()) return
    const bounds = state.mainWindow.getNormalBounds()
    writeWindowState({
      x: bounds.x, y: bounds.y, width: bounds.width, height: bounds.height,
      fullscreen: state.mainWindow.isFullScreen(),
      maximized: state.mainWindow.isMaximized(),
    })
  }
  const queueSave = () => {
    if (saveTimer) clearTimeout(saveTimer)
    saveTimer = setTimeout(persistState, 300)
  }
  state.mainWindow.on('resize', queueSave)
  state.mainWindow.on('move', queueSave)
  state.mainWindow.on('maximize', queueSave)
  state.mainWindow.on('unmaximize', queueSave)
  state.mainWindow.on('enter-full-screen', queueSave)
  state.mainWindow.on('leave-full-screen', queueSave)
  state.mainWindow.on('close', (event) => {
    // Close-to-hide: clicking the window's X (or otherwise asking it to
    // close) just hides it to the tray, keeping the WS connection alive
    // so notifications keep arriving. True quit only happens when
    // `appIsQuitting` is set — which the tray menu's Quit, Cmd-Q, and
    // window-all-closed (Win/Linux) all flow through via `before-quit`.
    // Persist state first either way so the geometry is fresh on next
    // open / launch.
    if (saveTimer) { clearTimeout(saveTimer); saveTimer = null }
    persistState()
    // In dev we DON'T close-to-hide: the existing `closed` handler runs
    // `app.quit()` so `concurrently -k` propagates SIGTERM to vite + the
    // API server. Intercepting here would strand those children and the
    // next launch hits "port 5180 already in use".
    if (!isDev && !state.appIsQuitting && state.tray && !state.tray.isDestroyed?.()) {
      event.preventDefault()
      try { state.mainWindow.hide() } catch { /* swallow */ }
    }
  })

  state.mainWindow.once('ready-to-show', () => {
    state.mainWindow.show()
    // Apply saved fullscreen / maximize state AFTER show so the OS
    // accepts the transition. Doing it via constructor options is
    // unreliable on macOS state-restoration paths.
    if (saved.fullscreen) state.mainWindow.setFullScreen(true)
    else if (saved.maximized) state.mainWindow.maximize()
  })

  // If a token arrived on the loopback server BEFORE the window finished
  // loading (the user was that fast), flush it now.
  state.mainWindow.webContents.once('did-finish-load', () => {
    if (state.pendingAuthToken) {
      const t = state.pendingAuthToken
      state.pendingAuthToken = null
      state.mainWindow.webContents.send('auth:token', t)
    }
  })

  state.mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    if (url.startsWith('http://') || url.startsWith('https://')) {
      shell.openExternal(url)
    }
    return { action: 'deny' }
  })

  if (isDev) {
    void state.mainWindow.loadURL(DEV_URL)
    if (process.env.OPEN_DEVTOOLS === '1') {
      state.mainWindow.webContents.openDevTools({ mode: 'detach' })
    }
  } else {
    // Serve over the app:// scheme registered above. `/` paths
    // (whether emitted by Vite or hard-coded in JSX) all flow through
    // the protocol handler in app.whenReady() and resolve from
    // <resources>/apps/web/dist. No more file:// 404s.
    //
    // Host name matters: it becomes the renderer's `Origin` header on
    // any cross-origin fetch. The cumora API server's
    // CUMORA_CORS_ORIGINS allows `app://cumora` — keep that in sync, or
    // /auth/me + every other /api call gets CORS-blocked and the user
    // sits forever on the sign-in screen with a valid token in store.
    void state.mainWindow.loadURL('app://cumora/index.html')
  }

  state.mainWindow.on('closed', () => {
    clearInterval(focusPollTimer)
    state.mainWindow = null
    // In dev, closing the main window should also tear down the
    // notification window + the whole app so `concurrently -k` propagates
    // the kill to vite (port 5180) + the API server (port 5181).
    //
    // Without this:
    //  - macOS convention keeps the Electron process alive after window
    //    close (mac apps stay in the dock with no windows).
    //  - The notification window is `closable: false`, so `app.quit()`
    //    alone won't end the process — we destroy() it ourselves first.
    //    destroy() bypasses the closable flag and any close handlers.
    // Both together mean `pnpm electron:dev`'s child processes leak and
    // the next launch hits "port 5180 already in use".
    if (isDev) {
      if (state.notificationWindow && !state.notificationWindow.isDestroyed()) {
        state.notificationWindow.destroy()
      }
      app.quit()
    }
  })
}

module.exports = { createWindow }
