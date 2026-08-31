/* eslint-env node */
// =============== IPC surface (ipcMain registrations) ===============
// Split out of main.cjs (#219⑥). Every ipcMain.on/handle registration
// from the old monolith lives here, in the exact original order; main.cjs
// requires this module at the point in the boot sequence where the
// registrations used to sit, so registration order is unchanged. Channel
// names and handler bodies are byte-identical — preload.cjs is untouched.
const { ipcMain, shell, screen } = require('electron')
const state = require('./state.cjs')
const {
  NOTIF_WIDTH,
  NOTIF_HEIGHT_MAX,
  NOTIF_MARGIN_RIGHT,
  NOTIF_MARGIN_TOP,
  positionNotificationWindow,
  pushNotification,
} = require('./notify.cjs')
const {
  setDockUnreadDot,
  invalidateActivationPolicyCache,
  scheduleRegularDockRepair,
} = require('./dock.cjs')
const { armAuthHandoff } = require('./auth.cjs')

ipcMain.on('notification:push', (_event, payload) => {
  pushNotification(payload)
})

ipcMain.on('dock:set-unread-dot', (_event, visible) => {
  setDockUnreadDot(!!visible)
})

// Renderer mounted + subscribed to onPush. Flush queued pushes, but
// LEAVE THE WINDOW HIDDEN — it'll be shown on the `painted` signal
// after the first toast actually renders.
ipcMain.on('notification:ready', () => {
  if (!state.notificationWindow || state.notificationWindow.isDestroyed()) return
  state.notificationReady = true
  for (const p of state.pendingPushes) {
    state.notificationWindow.webContents.send('notification:push', p)
  }
  state.pendingPushes = []
})

// Renderer has painted its first toast. Safe to show the window now.
// Position lazily here so any display change between create and paint
// (multi-monitor unplug) lands in the right spot.
ipcMain.on('notification:painted', () => {
  if (!state.notificationWindow || state.notificationWindow.isDestroyed()) return
  if (!state.notificationWindow.isVisible()) {
    positionNotificationWindow()
    // Showing the panel is the one event we know can demote us to
    // accessory — invalidate the cached policy state so the repair
    // actually re-applies, instead of no-opping.
    invalidateActivationPolicyCache()
    state.notificationWindow.showInactive()
    scheduleRegularDockRepair()
  }
  // Tell the MAIN window that a fresh toast is on screen. The main
  // window owns the chime — it's always loaded, so playing it from
  // there has zero boot delay. The small `setTimeout` ensures the
  // panel has committed to the screen BEFORE the audio starts; firing
  // synchronously made the bell beat the visible toast by ~30-50ms.
  if (state.mainWindow && !state.mainWindow.isDestroyed()) {
    setTimeout(() => {
      if (state.mainWindow && !state.mainWindow.isDestroyed()) {
        state.mainWindow.webContents.send('notification:visible')
      }
    }, 120)
  }
})

// Renderer reports the exact content height. Resize the panel so the
// vibrant material (real macOS Gaussian blur) only fills the area
// occupied by the toast(s), not a full 360×480 rectangle.
ipcMain.on('notification:set-height', (_event, h) => {
  if (!state.notificationWindow || state.notificationWindow.isDestroyed()) return
  const height = Math.max(60, Math.min(NOTIF_HEIGHT_MAX, Math.ceil(Number(h) || 0)))
  const display = screen.getPrimaryDisplay()
  const wa = display.workArea
  state.notificationWindow.setBounds({
    x: wa.x + wa.width - NOTIF_WIDTH - NOTIF_MARGIN_RIGHT,
    y: wa.y + NOTIF_MARGIN_TOP,
    width: NOTIF_WIDTH,
    height,
  })
})

// Renderer toggles whether the notification window accepts clicks.
//  - `true`  → a toast is on screen; user must be able to click it.
//  - `false` → no toasts; destroy the window entirely so nothing sits
//              invisibly on top of the user's desktop. Cleaner than
//              hiding (the window literally doesn't exist) and matches
//              what Alma/wails-gui do with their NSPanel toasts.
ipcMain.on('notification:set-interactive', (_event, interactive) => {
  if (!state.notificationWindow || state.notificationWindow.isDestroyed()) return
  if (interactive) {
    state.notificationWindow.setIgnoreMouseEvents(false)
    scheduleRegularDockRepair()
  } else {
    // Destroy() bypasses `closable: false` and any close-prevent
    // handlers. `closed` resets pendingPushes + notificationReady.
    state.notificationWindow.destroy()
    scheduleRegularDockRepair()
  }
})

// MAIN window asks to clear a specific toast (e.g. after auto-dismiss).
ipcMain.on('notification:dismiss', (_event, id) => {
  if (!state.notificationWindow || state.notificationWindow.isDestroyed()) return
  state.notificationWindow.webContents.send('notification:dismiss', id)
})

// Renderer asks for the main window's current focus state synchronously
// (e.g. at boot, before the first focus/blur event has fired).
ipcMain.handle('app:is-focused', () => {
  return !!(state.mainWindow && !state.mainWindow.isDestroyed() && state.mainWindow.isFocused())
})

// Renderer asks main to open a URL in the user's default browser
// (used for OAuth — embedded webviews are banned by Google and the
// experience is better in a familiar browser anyway). Restricted to
// http/https so a compromised renderer can't shell-out arbitrary URLs.
ipcMain.handle('auth:open-external', (_event, url) => {
  if (typeof url !== 'string') return false
  if (!url.startsWith('http://') && !url.startsWith('https://')) return false
  void shell.openExternal(url)
  return true
})

// Renderer arms a sign-in and gets a single-use nonce to thread through the
// OAuth return URL; only an inbound token carrying this nonce is accepted
// (see dispatchAuthToken / consumeAuthNonce — anti session-fixation).
ipcMain.handle('auth:arm', () => armAuthHandoff())

// NOTIFICATION window asks main to focus a conversation when user clicks
// a toast. Bring the main window forward + forward the id over to its
// renderer so the React app selects the conversation.
ipcMain.on('notification:focus-convo', (_event, conversationId) => {
  if (state.mainWindow && !state.mainWindow.isDestroyed()) {
    if (state.mainWindow.isMinimized()) state.mainWindow.restore()
    state.mainWindow.show()
    state.mainWindow.focus()
    state.mainWindow.webContents.send('notification:focus-convo', conversationId)
  }
})
