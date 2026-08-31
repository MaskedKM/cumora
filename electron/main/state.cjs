/* eslint-env node */
// =============== Cross-domain mutable refs (shared store) ===============
// Split out of main.cjs (#219⑥). These top-level `let` bindings used to
// live in the single main.cjs scope and were read/written across domain
// sections (window / tray / notify / auth / lifecycle). Now that the
// domains are separate cjs modules, the mutable bindings live on this
// shared object so every module observes the same current value. Domain
// code references them as `state.<name>` — the only textual change to the
// moved lines (see PR equivalence notes). Domain-private bindings (focus
// poll timers, icon caches, armed auth nonce, pending pushes, …) stayed
// private inside their domain module.
//
// `pendingDeepLink` deliberately does NOT live here: it is only touched by
// the deep-link routing section, which remains in main.cjs.
const state = {
  /** True once an actual quit is in flight (Cmd-Q, tray menu Quit,
   *  window-all-closed on Win/Linux — anything that flows through
   *  `before-quit`). The main window's `close` handler checks this flag
   *  and falls through to a real close; otherwise it hides to tray. */
  appIsQuitting: false,

  /** The "real" app window. */
  mainWindow: null,

  /** Frameless transparent always-on-top window pinned to the top-right
   *  of the primary display. Renders the same React bundle with the
   *  `#notifications` hash so it shows only the toast stack. Hidden when
   *  there are no live toasts, shown (without stealing focus) on push. */
  notificationWindow: null,

  /** Set by auth.cjs when a loopback/deep-link token arrives before the
   *  main window finished loading; flushed by createWindow's
   *  `did-finish-load` hook. */
  pendingAuthToken: null,

  /** Reference to the auth loopback HTTP server. Held so `before-quit`
   *  can `.close()` it — otherwise the listening socket keeps Node's event
   *  loop alive after all windows close and the user is forced to
   *  Activity-Monitor-kill the process. */
  authLoopbackServer: null,

  /** Authoritative unread state shared by dock badge + tray mirror
   *  (dock.cjs writes it; tray.cjs and main.cjs read it). */
  dockUnreadDotVisible: false,

  /** System tray handle (menu bar item on macOS, tray on Win/Linux).
   *  Written by createTray; torn down by before-quit / auto-update
   *  before-install; read by the main window's close-to-hide guard. */
  tray: null,

  /** Queue of pushes that arrived before the notification renderer
   *  signalled `notification:ready`. Flushed once the renderer mounts.
   *  (notify.cjs produces; the `notification:ready` IPC handler in
   *  ipc.cjs consumes.) */
  pendingPushes: [],

  /** True once the notification renderer signalled `notification:ready`. */
  notificationReady: false,
}

module.exports = state
