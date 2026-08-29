/**
 * Shown to a brand-new OAuth visitor whose sign-in attempt landed
 * them on the waitlist instead of creating an account. Triggered by
 * the `?waitlist=1&email=...` query (or legacy `#waitlist=1&...`
 * fragment) that handleCallback redirects to. The signal is consumed
 * once and scrubbed so a reload doesn't stick the user on this page
 * forever — they can re-attempt sign-in normally.
 */
import { useState } from 'react'
import { GetDesktopAppLink } from '@/components/GetDesktopAppLink'
import { useT } from '@/lib/i18n'
import './admin.css'

interface CarriedWaitlist { email: string | null }

export function WaitlistConfirmedScreen({ email }: { email: string | null }) {
  const t = useT()
  const [dismissed, setDismissed] = useState(false)
  if (dismissed) {
    // Falling out of the screen reloads → AuthGate decides what to render
    // next based on the (still-empty) auth store.
    location.reload()
    return null
  }
  const displayEmail = email ?? t('waitlist.confirmedEmailFallback')
  // Render the body around the {email} placeholder so we can wrap the
  // rendered address in a styled <span> without resorting to
  // dangerouslySetInnerHTML. Same split() trick as SuspendedScreen.
  const body = t('waitlist.confirmedBody', { email: displayEmail })
  const bodyParts = body.split(displayEmail)
  // Footer also has a {link} placeholder — we pass a NUL marker so the
  // rendered string contains a unique token we can split on, then slot
  // the real <GetDesktopAppLink> in between the two halves.
  const footer = t('waitlist.confirmedFooter', { link: '\u0000' })
  const footerParts = footer.split('\u0000')
  return (
    <div className="cumora-waitlist-screen">
      <div className="cumora-waitlist-card">
        <div className="cumora-waitlist-emoji">⏳</div>
        <div className="cumora-waitlist-title">{t('waitlist.confirmedTitle')}</div>
        <div className="cumora-waitlist-sub" style={{ marginBottom: 18 }}>
          {bodyParts[0]}
          <span className="cumora-waitlist-email">{displayEmail}</span>
          {bodyParts.slice(1).join(displayEmail)}
        </div>
        <div style={{ marginBottom: 24, fontSize: 12.5, color: 'var(--ink-400)', fontStyle: 'italic' }}>
          {footerParts[0]}<GetDesktopAppLink variant="text" />{footerParts.slice(1).join('\u0000')}
        </div>
        <button className="btn-ghost" onClick={() => setDismissed(true)}>
          {t('waitlist.confirmedDone')}
        </button>
      </div>
    </div>
  )
}
