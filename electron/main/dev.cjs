/* eslint-env node */
// =============== Dev-only notification shortcuts ===============
// Split out of main.cjs (#219⑥).
const { globalShortcut } = require('electron')
const { pushNotification } = require('./notify.cjs')

// Inspired by Alma's debug menu. Press once to test the full pipeline
// (panel create → vibrancy → entry spring → chime → exit fade) without
// needing a real agent message round-trip.
//
//  - Cmd/Ctrl+Shift+N → single canned toast, cycles through samples
//  - Cmd/Ctrl+Shift+M → burst of 3 toasts from different convos (test
//                       the stack + per-toast chime)
const DEV_SAMPLES = [
  { authorId: 'iris-a0ab', authorName: 'Iris', conversationTitle: '设计评审', body: 'Mock-up #3 已经发出去了，回头看一眼。' },
  { authorId: 'atlas-dd9c', authorName: 'Atlas', conversationTitle: 'Strategy', body: 'Got the metrics breakdown ready. Tl;dr: Q3 looks fine, Q4 is the question.' },
  { authorId: 'bram-0154', authorName: 'Bram', conversationTitle: 'Engineering', body: '把那个 race condition 修了，已经合到 main。' },
  { authorId: 'nova-6596', authorName: 'Nova', conversationTitle: 'PM check-in', body: 'Quick ping — can you look at the v2 spec when you have a sec?' },
  { authorId: 'saga', authorName: 'Saga', conversationTitle: 'Direct', body: 'I have a draft of the storyboard. Want to see it now or after standup?' },
]
let devSampleIdx = 0
function makeDevPayload(sample) {
  const at = Date.now()
  return {
    id: `dev-${at}-${Math.random().toString(36).slice(2, 8)}`,
    conversationId: `dev-${sample.authorId}`,
    authorId: sample.authorId,
    authorName: sample.authorName,
    authorAvatarUrl: null,
    conversationTitle: sample.conversationTitle,
    body: sample.body,
    at,
  }
}
function registerDevShortcuts() {
  const single = 'CommandOrControl+Shift+N'
  const burst = 'CommandOrControl+Shift+M'
  const okSingle = globalShortcut.register(single, () => {
    const s = DEV_SAMPLES[devSampleIdx % DEV_SAMPLES.length]
    devSampleIdx += 1
    pushNotification(makeDevPayload(s))
  })
  const okBurst = globalShortcut.register(burst, () => {
    // Three different conversations so the renderer treats them as
    // separate toasts (rather than coalescing). Slight stagger so the
    // spring entrance reads as a cascade, not a snap.
    for (let i = 0; i < 3; i++) {
      const s = DEV_SAMPLES[(devSampleIdx + i) % DEV_SAMPLES.length]
      setTimeout(() => pushNotification(makeDevPayload(s)), i * 160)
    }
    devSampleIdx += 3
  })
  console.log(`[dev-shortcuts] ${single}=${okSingle ? 'ok' : 'failed'}, ${burst}=${okBurst ? 'ok' : 'failed'}`)
}

module.exports = { registerDevShortcuts }
