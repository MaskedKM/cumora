/* eslint-env node */
// =============== Shared build/runtime environment constants ===============
// Split out of main.cjs (#219⑥) because every domain module needs them.
// Values are computed once at require time — same moment they were computed
// at main.cjs top-level before the split.
const { app, nativeImage } = require('electron')
const path = require('node:path')

const isDev = !app.isPackaged
const DEV_URL = process.env.ELECTRON_RENDERER_URL || 'http://localhost:5180'

// Resolve once — used for `BrowserWindow.icon` (Win/Linux taskbar) and for
// `app.dock.setIcon` on macOS so the dock / cmd-tab in dev mode show the
// cumora cloud instead of Electron's default.
//
// It must be a decoded NativeImage, NOT the path string: Chromium reads a
// string `icon` with plain file IO, which cannot see inside app.asar — in
// packaged builds that silently yields a window with no icon at all
// (_NET_WM_ICON unset on Linux → dock/taskbar shows a generic placeholder).
// `nativeImage.createFromPath` is asar-aware, so it resolves in dev (repo
// tree) and packaged (asar) layouts alike.
const ICON = nativeImage.createFromPath(path.join(app.getAppPath(), 'build', 'icon.png'))

module.exports = { isDev, DEV_URL, ICON }
