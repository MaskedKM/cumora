/* eslint-env node */
// =============== Shared build/runtime environment constants ===============
// Split out of main.cjs (#219⑥) because every domain module needs them.
// Values are computed once at require time — same moment they were computed
// at main.cjs top-level before the split.
const { app } = require('electron')
const path = require('node:path')

const isDev = !app.isPackaged
const DEV_URL = process.env.ELECTRON_RENDERER_URL || 'http://localhost:5180'

// Resolve once — used for `BrowserWindow.icon` (Win/Linux taskbar) and for
// `app.dock.setIcon` on macOS so the dock / cmd-tab in dev mode show the
// cumora cloud instead of Electron's default.
const ICON_PATH = path.join(app.getAppPath(), 'build', 'icon.png')

module.exports = { isDev, DEV_URL, ICON_PATH }
