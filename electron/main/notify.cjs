/* eslint-env node */
// =============== Notification window (toast panel) ===============
// Split out of main.cjs (#219⑥). Owns the frameless toast panel's
// lifecycle (create / position / destroy), the pre-ready push queue,
// and the shared pushNotification entry point used by the IPC handler
// (ipc.cjs) and the dev shortcuts (dev.cjs).
const { BrowserWindow, screen } = require('electron')
const path = require('node:path')
const { isDev, DEV_URL } = require('./env.cjs')
const state = require('./state.cjs')
const { scheduleRegularDockRepair } = require('./dock.cjs')

const NOTIF_WIDTH = 360
const NOTIF_HEIGHT_MAX = 480
const NOTIF_MARGIN_RIGHT = 16
const NOTIF_MARGIN_TOP = 24

function notificationUrl() {
  if (isDev) return `${DEV_URL}/#notifications`
  // Same `app://` scheme the main window loads (see the header comment): over
  // `file://` the bundle's absolute `/assets/…` imports resolve outside the asar
  // and 404, so the renderer never boots — meaning `notification:ready` never
  // fires, every push queues in pendingPushes forever, and a packaged build
  // shows no toasts and plays no chime at all.
  return 'app://cumora/index.html#notifications'
}

function positionNotificationWindow() {
  if (!state.notificationWindow) return
  const display = screen.getPrimaryDisplay()
  const { x, y, width } = display.workArea
  state.notificationWindow.setBounds({
    x: x + width - NOTIF_WIDTH - NOTIF_MARGIN_RIGHT,
    y: y + NOTIF_MARGIN_TOP,
    width: NOTIF_WIDTH,
    height: NOTIF_HEIGHT_MAX,
  })
}

/** Queue of pushes that arrived before the notification renderer
 *  signalled `notification:ready` (state.pendingPushes) + the ready
 *  flag (state.notificationReady) live on the shared store: the
 *  `notification:ready` / `notification:set-interactive` IPC handlers
 *  in ipc.cjs reset/flush them too. */

function createNotificationWindow() {
  state.notificationWindow = new BrowserWindow({
    width: NOTIF_WIDTH,
    // Start small — renderer will report exact content height once
    // the toast paints, and we resize then. Without dynamic sizing the
    // vibrant material below would fill a 360×480 rectangle of frosted
    // glass with a tiny toast at the top — ugly.
    height: 120,
    show: false,
    frame: false,
    // alma's recipe: `transparent: true` everywhere + NO vibrancy. The
    // window's empty pixels stay genuinely transparent — the desktop is
    // visible directly underneath, NOT covered by an NSVisualEffectView
    // material rectangle. The toast card brings its own desktop blur via
    // CSS `backdrop-filter` (see NotificationWindow.tsx). Without this,
    // vibrancy 'under-window' paints frosted material across the entire
    // 360×N window bounds, producing the gray slab below the toast.
    transparent: true,
    backgroundColor: '#00000000',
    hasShadow: false,
    // Disable Electron's automatic macOS window-edge rounding. The toast
    // card draws its own border-radius (CSS, 14px); leaving the system
    // rounding on would clip the card's bottom-right corner. wails-gui
    // and alma both omit any window-level rounding for the same reason.
    roundedCorners: false,
    resizable: false,
    movable: false,
    minimizable: false,
    maximizable: false,
    closable: false,
    // IMPORTANT: must be `true`, NOT `false`. `focusable: false` sets the
    // underlying NSWindow's `canBecomeKeyWindow = NO`, and AppKit reacts
    // to a non-key-capable panel becoming visible by recomputing the
    // app's activation policy: if no currently visible window can become
    // key (very common at boot — main window is still `show: false`
    // until `ready-to-show`), the app gets demoted to `accessory` and
    // the Dock icon vanishes. We don't want that.
    //
    // Click-through is enforced by `setIgnoreMouseEvents(true)` below;
    // showInactive() keeps the panel from stealing focus at show time;
    // `type: 'panel'` (NSNonactivatingPanel) ensures even a focused
    // click doesn't activate the whole app. So `focusable: true` is
    // safe — the panel never actually becomes key in practice, AppKit
    // just stops worrying about the app's foreground status.
    //
    // This matches Alma's quickChatWin (also `focusable: true` +
    // `type: 'panel'`), which is why Alma never had to call
    // `setActivationPolicy` to keep its dock icon.
    focusable: true,
    skipTaskbar: true,
    alwaysOnTop: true,
    // macOS NSPanel-style window. Critical because it:
    //  - never activates the app on click (background clicks no longer
    //    pull the main Cumora window forward)
    //  - doesn't appear in Mission Control / window-tab cycles
    //  - combined with `focusable: true` above, doesn't trigger the
    //    accessory-policy demotion that hides the Dock icon
    type: process.platform === 'darwin' ? 'panel' : undefined,
    webPreferences: {
      preload: path.join(__dirname, '..', 'preload.cjs'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      // Panel is shown via `showInactive()` and is click-through by
      // default, so it never receives a real user-gesture activation
      // even though `focusable: true`. Without this, Web Audio's
      // autoplay policy keeps the AudioContext suspended and the chime
      // can never play from this renderer.
      autoplayPolicy: 'no-user-gesture-required',
    },
  })

  // Transparent page surface is established via the constructor
  // option `backgroundColor: '#00000000'` above — that's the supported
  // path in modern Electron. Earlier versions also exposed
  // `webContents.setBackgroundColor(...)` for this, but it was removed
  // and calling it now throws "setBackgroundColor is not a function".
  //
  // `pop-up-menu` rather than `screen-saver` — the screen-saver level
  // (kCGScreenSaverWindowLevel) floats so high it sits over menus; the
  // pop-up-menu level floats well above the main window without that.
  state.notificationWindow.setAlwaysOnTop(true, 'pop-up-menu')
  // THE Dock-icon-vanishing fix. On macOS, `setVisibleOnAllWorkspaces`
  // DEFAULTS to transforming NSApp's process type between
  // `ForegroundApplication` and `UIElementApplication` (the latter is
  // the accessory/agent type — no Dock icon). So the moment the first
  // toast painted and we called this, Cumora flipped to UIElement and
  // the Dock icon disappeared. `skipTransformProcessType: true` tells
  // Electron to set the cross-Space collection behavior WITHOUT touching
  // the process type, so the panel still shows on every Space / over
  // fullscreen apps while the Dock icon stays put. This is the root-cause
  // fix; the `scheduleRegularDockRepair()` machinery below is now just a
  // belt-and-suspenders backstop, not the thing holding the icon on.
  state.notificationWindow.setVisibleOnAllWorkspaces(true, {
    visibleOnFullScreen: true,
    skipTransformProcessType: true,
  })
  // Default to click-through; renderer flips it off while a toast is on
  // screen via `notification:set-interactive`. With `type: 'panel'`
  // alone, clicks on the empty area would still hit-test on this
  // window — even though they no longer activate Cumora, they'd still
  // block clicks reaching whatever's underneath.
  state.notificationWindow.setIgnoreMouseEvents(true, { forward: true })

  positionNotificationWindow()
  void state.notificationWindow.loadURL(notificationUrl())

  state.notificationWindow.webContents.on('did-fail-load', (_e, code, desc, url) => {
    console.error('[NOTIFY:main] notification window load FAILED', { code, desc, url })
  })
  state.notificationWindow.webContents.on('render-process-gone', (_e, details) => {
    console.error('[NOTIFY:main] notification renderer GONE', details)
  })

  // Belt-and-suspenders for the Dock-icon-vanishing bug. The primary
  // fix is `focusable: true` above — that's what stops AppKit from
  // demoting the app when the panel is shown. But the demotion (when
  // it happens at all) is triggered AT SHOW TIME, not at construction,
  // so the right place to re-assert `regular` is the `show` event, not
  // synchronously after `new BrowserWindow`. Also calling `dock.show()`
  // explicitly because some Electron/macOS combinations don't refresh
  // the Dock visibility from `setActivationPolicy` alone.
  if (process.platform === 'darwin') {
    state.notificationWindow.on('show', scheduleRegularDockRepair)
    state.notificationWindow.on('closed', scheduleRegularDockRepair)
  }

  state.notificationWindow.on('closed', () => {
    state.notificationWindow = null
    state.notificationReady = false
    state.pendingPushes = []
  })
}

// Re-position whenever displays change. We attach once at app start
// instead of inside createNotificationWindow so we don't double-attach
// the listener every time the window is recreated.
function attachDisplayListeners() {
  screen.on('display-metrics-changed', positionNotificationWindow)
  screen.on('display-added', positionNotificationWindow)
  screen.on('display-removed', positionNotificationWindow)
}

// MAIN window pushes a toast — forward to the notification window and
// show it (without stealing focus) so the user sees it at screen top-right.
// Shared push routine. Same logic the IPC handler runs — extracted so
// dev shortcuts (and any future programmatic trigger) can fire a
// notification without rebuilding the create-on-demand state machine.
function pushNotification(payload) {
  scheduleRegularDockRepair()
  if (!state.notificationWindow || state.notificationWindow.isDestroyed()) {
    state.pendingPushes.push(payload)
    state.notificationReady = false
    createNotificationWindow()
    return
  }
  if (!state.notificationReady) {
    state.pendingPushes.push(payload)
    return
  }
  state.notificationWindow.webContents.send('notification:push', payload)
  scheduleRegularDockRepair()
  // Don't show here — renderer will signal `painted` after the toast
  // actually renders. Showing here risked flashing an empty 360-wide
  // rectangle before the React tree caught up.
}

module.exports = {
  NOTIF_WIDTH,
  NOTIF_HEIGHT_MAX,
  NOTIF_MARGIN_RIGHT,
  NOTIF_MARGIN_TOP,
  positionNotificationWindow,
  attachDisplayListeners,
  pushNotification,
}
