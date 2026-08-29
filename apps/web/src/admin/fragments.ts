/**
 * Eager fragment consumers for the two OAuth-gate landing screens
 * (waitlist / suspended). Split out of their screen modules (#144b) so
 * App.tsx can React.lazy the SCREENS while still probing the URL once at
 * startup without pulling the admin chunk — a static import of either
 * screen would defeat the split.
 */

export interface CarriedWaitlist { email: string | null }

export function consumeWaitlistFragment(): CarriedWaitlist | null {
  const search = new URLSearchParams(location.search)
  const hash = new URLSearchParams(location.hash.replace(/^#/, ''))
  // Query string is the new shape (survives cross-origin redirects that
  // drop fragments); hash kept as a fallback for older server builds.
  const fromQuery = search.get('waitlist') === '1'
  const fromHash = hash.get('waitlist') === '1'
  if (!fromQuery && !fromHash) return null
  const email = (fromQuery ? search.get('email') : hash.get('email'))
  // Scrub both the waitlist params and any leftover hash so a refresh
  // lands the user back on the normal sign-in flow.
  if (fromQuery) {
    search.delete('waitlist')
    search.delete('email')
  }
  const q = search.toString()
  history.replaceState(null, '', location.pathname + (q ? `?${q}` : ''))
  return { email }
}

export interface CarriedSuspension { email: string | null; reason: string | null }

export function consumeSuspendedFragment(): CarriedSuspension | null {
  const hash = location.hash.replace(/^#/, '')
  if (!hash) return null
  const params = new URLSearchParams(hash)
  if (params.get('suspended') !== '1') return null
  const email = params.get('email')
  const reason = params.get('reason')
  history.replaceState(null, '', location.pathname + location.search)
  return { email, reason }
}
