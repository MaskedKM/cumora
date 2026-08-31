/* eslint-env node */
// =============== OAuth loopback + deep-link auth handoff ===============
// Split out of main.cjs (#219⑥). Owns the loopback HTTP server, the
// single-use handoff nonce, and auth deep-link parsing/dispatch. The
// single-instance lock and `setAsDefaultProtocolClient` boot steps stay
// in main.cjs (timing-sensitive boot order); deep-link routing (open-url
// / second-instance / cold-start argv) also stays in main.cjs and calls
// into parseAuthDeepLink / dispatchAuthToken below.
const http = require('node:http')
const crypto = require('node:crypto')
const state = require('./state.cjs')

// ============== OAuth loopback: http://127.0.0.1:47823/auth/done ==============
// Sign-in opens the system browser; after the provider redirects to
// our prod server's /auth/callback, the server 302s to this tiny
// local-only HTTP server. The /auth/done HTML page renders a polished
// "You're signed in" card with an explicit "Open Cumora" button — the
// button uses the cumora:// deep link to hand the session to the app
// (works whether the app is currently running or not).
//
// Why button + deep link over silent auto-handoff:
//   - User sees a deliberate "open the app" moment instead of an
//     invisible POST that doesn't always bring Cumora to front
//   - Deep link launches the app even if it isn't running yet —
//     fresh-install + first sign-in works without a manual launch
//   - The browser tab stays put on the success page, so if Cumora
//     fails to come forward the user can click again
//
// Tokens ride in the URL fragment (#token=…) so they stay out of
// server access logs and HTTP Referer headers. macOS LaunchServices
// does log the full deep-link URL — same threat surface as any other
// cumora:// link, accepted as the cost of an explicit, user-driven
// handoff. Session TTL stays tight (30d hard / 14d idle).
//
// Single-instance lock keeps a stray second `cumora` from binding the
// same loopback port AND lets deep links routed to a NEW process bounce
// over to the already-running instance via second-instance event below.
const LOOPBACK_PORT = 47823
const DEEP_LINK_SCHEME = 'cumora'

/** HTML served at /auth/done. Self-contained: no external assets, so
 *  it renders identically online/offline. Shows a "You're signed in"
 *  card with an explicit Open Cumora button — clicking it navigates
 *  to `cumora://auth#token=…` so the OS hands the URL to the running
 *  Cumora app (open-url on macOS, second-instance argv on Win/Linux),
 *  launching it if it isn't already running. */
const AUTH_DONE_HTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Cumora — Signed in</title>
<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; }
  html, body { margin: 0; padding: 0; height: 100%; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, 'Inter', system-ui, sans-serif;
    background: linear-gradient(180deg, #F8FBFD 0%, #EDF4F9 100%);
    display: grid; place-items: center; color: #1A2733;
  }
  .card {
    width: min(420px, calc(100vw - 32px));
    background: #fff;
    border-radius: 18px;
    padding: 40px 32px 32px;
    box-shadow: 0 30px 60px -30px rgba(10, 30, 60, 0.20), 0 0 0 1px rgba(0, 80, 140, 0.06);
    text-align: center;
  }
  .cloud { font-size: 56px; line-height: 1; margin-bottom: 24px; }
  h1 { font-size: 22px; font-weight: 500; margin: 0 0 6px; letter-spacing: -0.01em; }
  .sub { font-style: italic; font-size: 13.5px; color: #65778A; margin: 0 0 28px; }
  /* Same shape + color story as the website's Download CTA: flat sky-
     blue rectangle with generous rounding, white label, no gradients,
     soft tinted shadow. Hover lifts a touch + darkens the surface a
     hair. */
  .btn {
    appearance: none;
    display: inline-flex;
    align-items: center;
    gap: 10px;
    border: 0;
    border-radius: 14px;
    padding: 14px 28px;
    font-size: 15px;
    font-weight: 600;
    font-family: inherit;
    letter-spacing: 0.005em;
    color: #fff;
    background: #0BB3F0;
    cursor: pointer;
    transition: transform 140ms ease, background 160ms ease, box-shadow 200ms ease;
    box-shadow:
      0 8px 18px -6px rgba(11, 179, 240, 0.55),
      0 2px 4px rgba(11, 179, 240, 0.20);
  }
  .btn:hover {
    background: #0AA5E0;
    transform: translateY(-1px);
    box-shadow:
      0 12px 24px -6px rgba(11, 179, 240, 0.6),
      0 3px 6px rgba(11, 179, 240, 0.22);
  }
  .btn:active { transform: translateY(0); background: #0997D0; }
  .btn:disabled {
    background: #B7D7E6;
    cursor: default;
    box-shadow: none;
    transform: none;
  }
  .btn-mark { width: 14px; height: 14px; opacity: 0.9; }
  .hint { font-size: 11.5px; color: #8B9AAC; margin-top: 20px; font-style: italic; }
  .ok { color: #34A853; font-weight: 600; }
  .err { color: #D03A3A; font-size: 13px; margin-top: 16px; }
</style>
</head>
<body>
  <div class="card">
    <div class="cloud">☁️</div>
    <h1 id="h1">You're signed in</h1>
    <p class="sub" id="sub">Ready when you are.</p>
    <button id="open" class="btn">
      <span id="btn-label">Open Cumora</span>
      <svg class="btn-mark" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M9 6l6 6-6 6"/>
      </svg>
    </button>
    <p class="hint" id="hint">You can close this tab after Cumora opens.</p>
    <p class="err" id="err" style="display:none"></p>
  </div>
<script>
(() => {
  // Token must stay in the fragment (out of access logs). Waitlist/error
  // signals are non-secret and the server prefers query string for them so
  // they survive cross-origin redirects that drop fragments. Read both so
  // either shape works.
  const hashParams = new URLSearchParams(location.hash.replace(/^#/, ''));
  const queryParams = new URLSearchParams(location.search);
  const params = { get: (k) => hashParams.get(k) ?? queryParams.get(k) };
  const token = params.get('token');
  const companyId = params.get('companyId');
  // Single-use nonce the app armed with (round-tripped via the return URL's
  // query). Threaded back into the deep link so main can verify this handoff
  // was app-initiated (anti session-fixation).
  const nonce = params.get('n');
  const error = params.get('error');
  const h1 = document.getElementById('h1');
  const sub = document.getElementById('sub');
  const btn = document.getElementById('open');
  const label = document.getElementById('btn-label');
  const hint = document.getElementById('hint');
  const err = document.getElementById('err');

  if (error) {
    h1.textContent = 'Sign-in failed';
    sub.textContent = '';
    btn.style.display = 'none';
    hint.style.display = 'none';
    err.style.display = 'block';
    err.textContent = decodeURIComponent(error);
    return;
  }
  // Waitlist gate is on and this email isn't an admin: server enqueued
  // them instead of minting a session. No token to deep-link, so we
  // surface the confirmation here in the browser tab the user is already
  // looking at — same shape as the renderer's WaitlistConfirmedScreen.
  if (params.get('waitlist') === '1') {
    const email = params.get('email');
    h1.textContent = "You're on the waitlist";
    sub.textContent = email
      ? 'We saved ' + email + ' and will let you know the moment your account is ready.'
      : 'We saved your email and will let you know the moment your account is ready.';
    btn.style.display = 'none';
    hint.style.display = 'none';
    return;
  }
  if (!token) {
    h1.textContent = 'No session token';
    sub.textContent = 'Try signing in again from Cumora.';
    btn.style.display = 'none';
    hint.style.display = 'none';
    return;
  }

  // Build the deep link. Token + companyId ride in the URL fragment
  // so they're not URL-encoded as a query string (cleaner) and don't
  // appear in any web-server access logs along the path (we never
  // navigate to a remote URL here — the OS routes the cumora:// scheme
  // directly to the local app).
  const frag = new URLSearchParams({ token });
  if (companyId) frag.set('companyId', companyId);
  if (nonce) frag.set('n', nonce);
  const deepLink = 'cumora://auth#' + frag.toString();

  // PRIMARY handoff: POST straight back to the loopback server that served
  // this page. Same origin, so no CORS and no preflight, and the token goes
  // to THE process that armed the nonce — the one waiting for it.
  //
  // The deep link below cannot do that. The cumora:// scheme is resolved by
  // the OS against whatever it has registered for it, which on a dev
  // machine is regularly the WRONG binary: an unpackaged "electron ." run
  // registers its Electron.app bundle, so a stray "npx electron" (or an old
  // release/ build, or a mounted DMG) can win the scheme and swallow every
  // sign-in — the token opens a stranger's window and the app you are
  // actually running never sees it. It stays as the fallback for the case
  // this POST can't cover: the app quit between opening the browser and
  // finishing, so nothing is listening here anymore.
  fetch('/auth/token', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ token, companyId, nonce }),
  }).then((r) => {
    if (!r.ok) throw new Error('handoff rejected: ' + r.status);
    h1.textContent = 'Signed in';
    sub.textContent = 'Cumora has your session.';
    label.innerHTML = '<span class="ok">✓</span> Signed in';
    btn.disabled = true;
    hint.textContent = 'You can close this tab.';
  }).catch(() => {
    // Loopback gone (app quit) or handoff refused — offer the OS route.
    sub.textContent = 'Ready when you are.';
    hint.textContent = 'You can close this tab after Cumora opens.';
  });

  let opened = false;
  btn.addEventListener('click', () => {
    if (opened) return;
    opened = true;
    location.href = deepLink;
    // Optimistic confirmation — the OS handler is fire-and-forget. If
    // the app fails to open (e.g. uninstalled), the user can still
    // click again; we re-enable after a short delay.
    label.innerHTML = '<span class="ok">✓</span> Opening Cumora…';
    btn.disabled = true;
    setTimeout(() => {
      opened = false;
      btn.disabled = false;
      label.textContent = 'Open Cumora again';
    }, 2500);
  });
})();
</script>
</body>
</html>`

// ============== Auth-handoff nonce (anti session-fixation) ==============
// An inbound `cumora://auth#token=…` deep link is otherwise unauthenticated:
// any web page the user visits can navigate to that scheme and silently log
// the app into the ATTACKER's account (session fixation). We defend with a
// single-use nonce that only the app can originate: the renderer asks main to
// ARM before opening the browser, threads the nonce through the OAuth return
// URL (server round-trips it back onto the /auth/done page), and every inbound
// token must carry the matching armed nonce or it's dropped. A drive-by deep
// link has no valid nonce because the app never armed for it.
//
// The armed state is in-memory: the normal desktop flow keeps the app open
// across the browser OAuth detour, so it's armed when the token returns. If
// the user QUITS the app mid-sign-in, a deep link that cold-starts a fresh
// process finds no armed nonce and is (correctly) dropped — they just sign in
// again from the now-running app. That rare edge is the accepted cost of not
// persisting a bearer-handoff credential to disk.
let armedAuthNonce = null
let armedAuthExpiry = 0
const AUTH_NONCE_TTL_MS = 10 * 60 * 1000

/** Arm for one sign-in and return a fresh nonce the renderer appends to the
 *  OAuth return URL. Supersedes any previous unused nonce. */
function armAuthHandoff() {
  armedAuthNonce = crypto.randomBytes(16).toString('hex')
  armedAuthExpiry = Date.now() + AUTH_NONCE_TTL_MS
  return armedAuthNonce
}

/** Validate + single-use-consume an inbound nonce. Constant-time compare so a
 *  mismatch leaks nothing; clears the armed nonce on any check so a token can
 *  be accepted at most once. */
function consumeAuthNonce(nonce) {
  const armed = armedAuthNonce
  const expiry = armedAuthExpiry
  armedAuthNonce = null
  armedAuthExpiry = 0
  if (!armed || Date.now() > expiry) return false
  if (typeof nonce !== 'string' || nonce.length !== armed.length) return false
  try {
    return crypto.timingSafeEqual(Buffer.from(nonce), Buffer.from(armed))
  } catch {
    return false
  }
}

/** Pull token + companyId + nonce out of a `cumora://auth#token=…` URL. The OS
 *  hands us the full URL on open-url / second-instance; we only care
 *  about the fragment. Returns null if the URL isn't auth-shaped. */
function parseAuthDeepLink(rawUrl) {
  try {
    const u = new URL(rawUrl)
    if (u.protocol !== DEEP_LINK_SCHEME + ':') return null
    if (u.hostname !== 'auth' && u.pathname !== '//auth' && u.pathname !== '/auth') return null
    const hash = (u.hash || '').replace(/^#/, '')
    const params = new URLSearchParams(hash)
    const token = params.get('token')
    if (!token || token.length < 8) return null
    return { token, companyId: params.get('companyId'), nonce: params.get('n') }
  } catch {
    return null
  }
}

/** Push token+companyId into the renderer — but ONLY for a handoff this app
 *  itself initiated (matching armed nonce). Bringing the main window forward
 *  is the polish touch — the user came from the browser, they want Cumora on
 *  top. An inbound token with a missing/stale/wrong nonce is dropped. */
function dispatchAuthToken(token, companyId, nonce) {
  if (!consumeAuthNonce(nonce)) {
    console.warn('[auth] dropped inbound token: no matching armed nonce (possible drive-by deep link)')
    return
  }
  if (state.mainWindow && !state.mainWindow.isDestroyed()) {
    if (state.mainWindow.isMinimized()) state.mainWindow.restore()
    state.mainWindow.show()
    state.mainWindow.focus()
    state.mainWindow.webContents.send('auth:token', { token, companyId })
  } else {
    state.pendingAuthToken = { token, companyId }
  }
}

function startAuthLoopback() {
  const server = http.createServer((req, res) => {
    // Cross-origin protection: only accept requests whose Origin (when
    // present) is empty (top-level navigation) or self. Belt-and-suspenders
    // against a malicious page on another origin POSTing tokens at us.
    const origin = req.headers.origin
    const selfOrigin = `http://127.0.0.1:${LOOPBACK_PORT}`
    if (origin && origin !== selfOrigin) {
      res.statusCode = 403; res.end('forbidden'); return
    }
    if (req.method === 'GET' && (req.url === '/auth/done' || req.url.startsWith('/auth/done?'))) {
      res.setHeader('content-type', 'text/html; charset=utf-8')
      res.end(AUTH_DONE_HTML)
      return
    }
    if (req.method === 'POST' && req.url === '/auth/token') {
      let body = ''
      req.on('data', (chunk) => {
        body += chunk
        // 4kb cap — token is base64url 32 bytes, companyId ~14 chars.
        // Anything over this is malicious or buggy.
        if (body.length > 4096) { req.destroy(); }
      })
      req.on('end', () => {
        try {
          const parsed = JSON.parse(body)
          if (typeof parsed?.token !== 'string' || parsed.token.length < 8) {
            res.statusCode = 400; res.end('bad token'); return
          }
          const companyId = typeof parsed.companyId === 'string' ? parsed.companyId : null
          const nonce = typeof parsed.nonce === 'string' ? parsed.nonce : null
          dispatchAuthToken(parsed.token, companyId, nonce)
          res.statusCode = 204; res.end()
        } catch {
          res.statusCode = 400; res.end('bad json')
        }
      })
      return
    }
    res.statusCode = 404; res.end('not found')
  })
  server.on('error', (e) => {
    console.warn('[auth-loopback] server error', e.message || e)
  })
  server.listen(LOOPBACK_PORT, '127.0.0.1', () => {
    console.log(`[auth-loopback] listening on http://127.0.0.1:${LOOPBACK_PORT}`)
  })
  state.authLoopbackServer = server
}

module.exports = {
  DEEP_LINK_SCHEME,
  armAuthHandoff,
  parseAuthDeepLink,
  dispatchAuthToken,
  startAuthLoopback,
}
