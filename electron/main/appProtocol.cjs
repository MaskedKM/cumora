/* eslint-env node */
// =============== `app://` scheme — packaged renderer is served from here ==
// Split out of main.cjs (#219⑥), unchanged. The top-level
// registerSchemesAsPrivileged call below MUST run before app ready — main.cjs
// requires this module first, so the registration still happens at the same
// earliest moment it did before the split.
const { protocol } = require('electron')
const path = require('node:path')

// Production packaging would otherwise load index.html over `file://`,
// where ANY absolute path (`/logo.png`, `/assets/index-XYZ.js`, the
// favicons in <link>, …) resolves to `file:///foo` — outside the
// asar — and 404s. Vite's `base: './'` trick only fixes the bundled
// imports; hard-coded JSX `src="/logo.png"` stays absolute.
//
// Registering `app://cumora/` as a privileged scheme + serving every
// request out of the packaged `dist/` directory normalises both classes
// of paths: absolute `/foo` and relative `./foo` both end up as
// `app://cumora/<path>` and resolve to `<resources>/apps/web/dist/<path>`. The
// renderer otherwise can't tell it isn't running over plain http(s).
//
// Why `cumora` as the hostname (not `localhost`): the renderer's Origin
// header gets stamped from this URL on any cross-origin fetch, and the
// API server's CUMORA_CORS_ORIGINS gates the response on that origin.
// Picking a project-specific host keeps the allowlist tight (no
// arbitrary local app gets a CORS pass) and reads more semantically.
//
// `standard: true` makes the scheme behave like http(s) for URL parsing,
// CORS, history.pushState, etc. `secure: true` qualifies it as a secure
// context so the WebCrypto subtle API + service workers (if we ever want
// one) work. `supportFetchAPI: true` enables fetch() against same-scheme
// URLs — the renderer's own JS doesn't need it currently, but leaving it
// on costs nothing and avoids a future foot-gun.
protocol.registerSchemesAsPrivileged([
  { scheme: 'app', privileges: { standard: true, secure: true, supportFetchAPI: true } },
])

/** Resolve the on-disk path for an `app://<host>/<request-path>` URL.
 *  Strips leading slashes from the URL path and joins under the packaged
 *  dist directory. Rejects anything that resolves OUTSIDE dist (path
 *  traversal guard). Host is ignored — we accept any so legacy
 *  `app://localhost` URLs that might be sitting in localStorage redirect
 *  history still resolve. */
function appProtocolFile(reqUrl) {
  const u = new URL(reqUrl)
  const distRoot = path.join(__dirname, '..', '..', 'apps', 'web', 'dist')
  let rel = decodeURIComponent(u.pathname).replace(/^\/+/, '')
  if (!rel || rel === 'index.html') rel = 'index.html'
  const resolved = path.normalize(path.join(distRoot, rel))
  if (!resolved.startsWith(distRoot)) return null
  return resolved
}

module.exports = { appProtocolFile }
