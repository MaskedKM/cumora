/* eslint-env node */
// =============== Cumora main process — assembly entry (#219⑥) ===============
// This file used to be a 1,683-line monolith. It is now the boot
// sequence + lifecycle wiring only; the domains live in ./main/:
//
//   main/env.cjs         isDev / DEV_URL / ICON_PATH constants
//   main/state.cjs       cross-domain mutable refs (mainWindow, tray, …)
//   main/appProtocol.cjs `app://` privileged scheme + file resolution
//   main/window.cjs      main BrowserWindow + geometry persistence
//   main/dock.cjs        activation-policy repair + dock unread badge
//   main/tray.cjs        tray icon templates + tray menu
//   main/notify.cjs      notification panel lifecycle + push queue
//   main/auth.cjs        OAuth loopback server + handoff nonce + deep-link parse
//   main/ipc.cjs         every ipcMain.on/handle registration
//   main/stack.cjs       local-stack wizard executor (probe/import/absorb/install)
//   main/dev.cjs         dev-only notification shortcuts
//
// Boot-order-sensitive registrations (privileged scheme, CDP switch,
// single-instance lock, early dock pin, protocol-client registration,
// IPC registration, app event handlers, whenReady) all remain HERE, in
// their exact original order — see the PR equivalence notes. preload.cjs
// is untouched.
const { app, BrowserWindow, nativeImage, nativeTheme, protocol, net, globalShortcut } = require('electron')
const path = require('node:path')
const { pathToFileURL } = require('node:url')
const autoUpdater = require('./autoUpdater.cjs')
const stackWizard = require('./main/stack.cjs')

// Side-effect-free domain modules + the `app://` scheme registration
// (main/appProtocol.cjs calls protocol.registerSchemesAsPrivileged at
// require time — requiring it here keeps it the FIRST side effect of
// the process, exactly where it sat in the old monolith; it must run
// before app ready).
const { isDev, ICON_PATH } = require('./main/env.cjs')
const state = require('./main/state.cjs')
const { appProtocolFile } = require('./main/appProtocol.cjs')
const { createWindow } = require('./main/window.cjs')
const { ensureRegularDock, scheduleRegularDockRepair, setDockUnreadDot } = require('./main/dock.cjs')
const { createTray } = require('./main/tray.cjs')
const { attachDisplayListeners } = require('./main/notify.cjs')
const { DEEP_LINK_SCHEME, parseAuthDeepLink, dispatchAuthToken, startAuthLoopback } = require('./main/auth.cjs')
const { registerDevShortcuts } = require('./main/dev.cjs')

// Expose Chrome DevTools Protocol on a fixed port in dev so external
// debuggers (electron-mcp, chrome://inspect) can attach. MUST be set
// BEFORE app.whenReady().
if (isDev) {
  app.commandLine.appendSwitch('remote-debugging-port', '9222')
}

// Single-instance lock — see main/auth.cjs header for why this guards
// the OAuth loopback port and deep-link routing.
const gotSingleInstance = app.requestSingleInstanceLock()
if (!gotSingleInstance) {
  app.quit()
}

// Pin the activation policy to `regular` at the earliest possible moment
// — BEFORE any BrowserWindow is constructed. On macOS, Electron's default
// is already `regular`, but pinning it explicitly here is the cheapest
// guard against any later code path (notification panel, accessory-style
// auxiliary windows, etc.) that might cause AppKit to recompute and
// demote the app to `accessory`. Calling this before `whenReady()` is
// supported and lands during the NSApp setup, well before any window
// exists to influence the decision.
if (process.platform === 'darwin') {
  ensureRegularDock()
  // Previously: attached show/hide/closed listeners to EVERY window via
  // `browser-window-created`, so the main window's first `show` event
  // alone triggered 5 staggered `setActivationPolicy` calls — visible
  // as a series of shadow flickers. The notification panel is the only
  // window that can actually cause AppKit to demote us, and its own
  // create-path handles repair locally (see createNotificationWindow).
}

// Register the app as the OS handler for cumora:// links. In dev this
// only persists until the dev process ends (Electron writes to LSDb
// each launch); packaged builds get a durable registration via the
// `build.protocols` entry in package.json.
if (process.defaultApp) {
  // `process.defaultApp` is true when Electron is run from CLI (dev).
  // The argv[1] is the script path — register so macOS knows which
  // executable to invoke for cumora:// URLs.
  if (process.argv.length >= 2) {
    app.setAsDefaultProtocolClient(DEEP_LINK_SCHEME, process.execPath, [path.resolve(process.argv[1])])
  }
} else {
  app.setAsDefaultProtocolClient(DEEP_LINK_SCHEME)
}

// IPC surface. main/ipc.cjs registers every ipcMain.on/handle at
// require time; requiring it HERE keeps the registrations at the exact
// point in the boot sequence they occupied in the old monolith (after
// the boot one-liners above, before the app event handlers below).
require('./main/ipc.cjs')
// 本地栈向导执行面(#284):stack:probe/import/absorb/install/doctor。
stackWizard.registerIpc()

// ============== Deep-link routing (cumora://auth#token=…) ==============
// Three entry points cover all three OSes:
//   • macOS (running) → `open-url` event
//   • macOS (cold start by Finder/Safari clicking the link) → `open-url` fires
//     after whenReady; we also stash the URL via `process.argv` as belt-and-braces
//   • Windows/Linux (running) → `second-instance` event (because we hold the
//     single-instance lock, a fresh `cumora cumora://…` invocation bounces here)
//   • Windows/Linux (cold start) → URL is in `process.argv` at boot
//
// Each handler funnels into dispatchAuthToken() which IPCs the renderer
// and brings the window to front — the same plumbing the (now-deprecated)
// loopback /auth/token POST used.
let pendingDeepLink = null

function consumeDeepLink(url) {
  const parsed = parseAuthDeepLink(url)
  if (!parsed) return
  // If main window hasn't loaded yet, dispatchAuthToken stashes the
  // payload in pendingAuthToken and the did-finish-load hook flushes
  // it. So we can call it eagerly here regardless of timing.
  dispatchAuthToken(parsed.token, parsed.companyId, parsed.nonce)
}

// macOS — single canonical event for all deep-link arrivals (running OR
// cold start). preventDefault stops Electron's default no-op.
app.on('open-url', (event, url) => {
  event.preventDefault()
  if (!app.isReady()) { pendingDeepLink = url; return }
  consumeDeepLink(url)
})

// Windows / Linux — a second `cumora cumora://auth#…` invocation bounces
// here via the single-instance lock we acquired up top. The URL is the
// last element of argv per Electron convention.
app.on('second-instance', (_event, argv) => {
  const url = argv.find((a) => typeof a === 'string' && a.startsWith(DEEP_LINK_SCHEME + '://'))
  if (url) consumeDeepLink(url)
  // Always surface the existing window — the user just clicked something
  // expecting Cumora to come forward.
  if (state.mainWindow && !state.mainWindow.isDestroyed()) {
    if (state.mainWindow.isMinimized()) state.mainWindow.restore()
    state.mainWindow.show()
    state.mainWindow.focus()
  }
})

// Windows / Linux cold-start path: the URL arrives baked into our own
// argv. macOS doesn't use argv for this — it uses open-url above.
{
  const coldStartUrl = process.argv.find((a) => typeof a === 'string' && a.startsWith(DEEP_LINK_SCHEME + '://'))
  if (coldStartUrl) pendingDeepLink = coldStartUrl
}

app.whenReady().then(() => {
  if (process.platform === 'darwin') {
    nativeTheme.themeSource = 'light'
  }

  // Wire the app:// protocol handler. protocol.handle is the modern
  // API (Electron 25+) — gives us a streaming Response back so the
  // renderer never blocks on whole-bundle buffering. Anything we
  // can't resolve gets a 404. The handler ONLY serves packaged dist
  // content; appProtocolFile() rejects path-traversal attempts.
  protocol.handle('app', async (request) => {
    const file = appProtocolFile(request.url)
    if (!file) return new Response('not found', { status: 404 })
    try {
      return await net.fetch(pathToFileURL(file).toString())
    } catch (e) {
      console.warn('[app://]', request.url, '→', e?.message || e)
      return new Response('not found', { status: 404 })
    }
  })

  createWindow()
  // Notification window is no longer created at boot — it spawns on the
  // first `notification:push` IPC and self-destroys when the last toast
  // dismisses. See `notification:push` / `notification:set-interactive`.
  attachDisplayListeners()
  // System tray (menu bar item on macOS, system tray on Win/Linux).
  // Created after the main window so the `close → hide` interception
  // in createWindow already has its `tray` guard satisfied — no chance
  // of hiding to a non-existent tray.
  createTray()
  // 栈降级常驻感知(#314):主进程 60s 轮询驱动托盘 ⚠,面板外也生效
  // (此前靠 StackTab 挂载上报,面板不开托盘就聋)。
  stackWizard.startDegradedPoller()
  if (process.platform === 'darwin') {
    // Belt-and-suspenders: with `type: 'panel'` on the notification
    // BrowserWindow we should never lose the regular activation policy,
    // but the call is cheap so we keep it as a defensive measure.
    scheduleRegularDockRepair()
    try {
      const img = nativeImage.createFromPath(ICON_PATH)
      if (!img.isEmpty()) app.dock.setIcon(img)
    } catch (_) { /* swallow — dev convenience only */ }
    setDockUnreadDot(state.dockUnreadDotVisible)
  }

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) {
      createWindow()
    } else if (state.mainWindow && !state.mainWindow.isDestroyed()) {
      state.mainWindow.show()
    }
  })

  if (isDev) registerDevShortcuts()
  startAuthLoopback()

  // Cold-start deep link (Windows/Linux, OR a macOS open-url that
  // arrived before whenReady resolved). consumeDeepLink stashes into
  // pendingAuthToken when the renderer isn't ready yet, and the
  // did-finish-load hook in createWindow flushes that.
  if (pendingDeepLink) {
    const url = pendingDeepLink
    pendingDeepLink = null
    consumeDeepLink(url)
  }

  // Wire auto-update. registerIpc() is safe to call even when
  // unsupported (handlers just return the unsupported status).
  // initialize() is a no-op when there's no update channel (e.g. dev
  // without dev-app-update.yml).
  autoUpdater.registerIpc()
  autoUpdater.setBeforeInstallHandler(() => {
    // electron-updater on macOS skips `before-quit` (it goes through
    // Browser::Shutdown), so our close-to-hide handler would otherwise
    // intercept the window close and the app would just hide instead of
    // exiting — Squirrel then waits forever for a quit that never comes
    // and the update never installs. Flip the flag now and tear down the
    // auxiliary windows / servers ourselves.
    state.appIsQuitting = true
    if (state.notificationWindow && !state.notificationWindow.isDestroyed()) {
      try { state.notificationWindow.destroy() } catch { /* swallow */ }
      state.notificationWindow = null
    }
    if (state.authLoopbackServer) {
      try { state.authLoopbackServer.close() } catch { /* swallow */ }
      try { state.authLoopbackServer.closeAllConnections?.() } catch { /* swallow */ }
      state.authLoopbackServer = null
    }
    if (state.tray && !state.tray.isDestroyed?.()) {
      try { state.tray.destroy() } catch { /* swallow */ }
      state.tray = null
    }
  })
  autoUpdater.initialize()
})

// macOS: Dock-icon click (and Cmd-Tab to a hidden app) fires `activate`.
// Without this handler the app sits in the Dock with no way back —
// `window-all-closed` keeps the process alive but never re-shows or
// re-creates the window.
app.on('activate', () => {
  scheduleRegularDockRepair()
  if (!state.mainWindow || state.mainWindow.isDestroyed()) {
    createWindow()
    return
  }
  if (state.mainWindow.isMinimized()) state.mainWindow.restore()
  state.mainWindow.show()
  state.mainWindow.focus()
})

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') app.quit()
})

// Pre-quit cleanup. Without this Cmd-Q on macOS could hang and force
// users to Activity-Monitor-kill the process, because:
//  1. `notificationWindow` is `closable: false`; Electron's quit
//     sequence calls `close()` on every window — which is a no-op for a
//     non-closable window, leaving it alive and blocking `will-quit`.
//     `destroy()` bypasses the flag and the close handlers, so the
//     window goes away regardless.
//  2. The auth loopback HTTP server keeps a listening TCP socket open
//     on 127.0.0.1:47823 indefinitely. The Node event loop won't exit
//     while a server is listening, so even after all windows are gone
//     the process can sit forever.
app.on('before-quit', () => {
  // Flip the close-to-hide guard so the main window's `close` handler
  // lets the close actually happen during real quit (Cmd-Q, tray menu
  // Quit, window-all-closed on Win/Linux). Without this, every quit
  // path would just hide the window forever.
  state.appIsQuitting = true
  stackWizard.stopDegradedPoller()
  if (state.notificationWindow && !state.notificationWindow.isDestroyed()) {
    try { state.notificationWindow.destroy() } catch { /* swallow */ }
    state.notificationWindow = null
  }
  if (state.authLoopbackServer) {
    try { state.authLoopbackServer.close() } catch { /* swallow */ }
    // closeAllConnections is Node 18+ — without it, keep-alive HTTP
    // clients can hold the port past app exit.
    try { state.authLoopbackServer.closeAllConnections?.() } catch { /* swallow */ }
    state.authLoopbackServer = null
  }
  if (state.tray && !state.tray.isDestroyed?.()) {
    try { state.tray.destroy() } catch { /* swallow */ }
    state.tray = null
  }
})

app.on('will-quit', () => {
  // Release the global accelerators so a zombie dev process doesn't
  // keep them claimed and shadow a fresh `pnpm electron:dev` from
  // registering them on next launch.
  globalShortcut.unregisterAll()
})

// Graceful shutdown on signals — primarily for dev where Ctrl-C in the
// terminal that ran `pnpm electron:dev` propagates SIGINT to children.
// Without this Electron ignores the signal and concurrently can't tear
// down vite + server.
for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.on(sig, () => { try { app.quit() } catch { /* swallow */ } })
}
