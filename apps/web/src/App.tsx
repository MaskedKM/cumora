import { lazy, Suspense, useEffect, useRef, useState } from 'react'
import { consumeSuspendedFragment, consumeWaitlistFragment } from '@/admin/fragments'
import { api } from '@/api/client'
import { AuthGate } from '@/components/AuthGate'
import { ErrorBoundary } from '@/components/ErrorBoundary'
import {
  consumeInviteFromUrl, getPendingInvite, clearPendingInvite,
  InviteAcceptScreen,
} from '@/components/InviteAcceptScreen'
import { NotificationToasts } from '@/components/NotificationToasts'
import { UpdateBanner, UpdaterDialog } from '@/components/UpdaterDialog'
import { Onboarding } from '@/desktop/Onboarding'
import { isNotificationWindow, isWebAppHost } from '@/lib/runtime'
import { useIsMobile } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useAuth } from '@/stores/auth'
import { bootComputers, useComputers } from '@/stores/computers'
import { bootConversations, isMuted, useConversations } from '@/stores/conversations'
import { bootMessagesStream, useMessages } from '@/stores/messages'
import { bootParticipants, useParticipants } from '@/stores/participants'
import { usePrefs } from '@/stores/preferences'
import { bootWhispers, useWhispers } from '@/stores/whispers'
import { WebShell } from '@/web/WebShell'

// Route-level code splitting (#144b): the three UI shells (desktop /
// mobile / admin) and the two rare OAuth-gate screens each load as their
// own chunk. Only the shell the entry actually renders is fetched; the
// others ride along as ~0-byte references. Fragment probing stays eager
// via @/admin/fragments, so the waitlist/suspended URL signals are
// consumed without pulling the admin chunk.
const DesktopApp = lazy(() => import('@/desktop/DesktopApp').then((m) => ({ default: m.DesktopApp })))
const MobileApp = lazy(() => import('@/mobile/MobileApp').then((m) => ({ default: m.MobileApp })))
const AdminApp = lazy(() => import('@/admin/AdminApp').then((m) => ({ default: m.AdminApp })))
const WaitlistConfirmedScreen = lazy(() =>
  import('@/admin/WaitlistConfirmedScreen').then((m) => ({ default: m.WaitlistConfirmedScreen })))
const SuspendedScreen = lazy(() =>
  import('@/admin/SuspendedScreen').then((m) => ({ default: m.SuspendedScreen })))
// The Electron notification window is a SEPARATE BrowserWindow that loads
// this bundle with a `#notifications` hash — only that instance ever
// renders NotificationWindow. Keeping the import static forced framer-motion
// into the entry chunk for every main-window user; lazy means the
// animation lib is fetched only by the window that animates (#218).
const NotificationWindow = lazy(() =>
  import('@/components/NotificationWindow').then((m) => ({ default: m.NotificationWindow })))

/** Neutral full-viewport placeholder while a shell chunk loads — keeps
 *  the flash to a blank frame instead of a layout jump. */
function ShellFallback() {
  return <div className="h-screen w-screen" />
}

/** True iff this browser tab is for the admin panel. On prod the
 *  hostname is admin.cumora.ai; in localhost dev the path prefix
 *  `/admin` triggers it. We check both so a dev can hit
 *  http://localhost:5180/admin/ without DNS work. */
function isAdminContext(): boolean {
  if (typeof location === 'undefined') return false
  if (location.hostname.startsWith('admin.')) return true
  if (location.pathname.startsWith('/admin')) return true
  return false
}

function AuthedApp() {
  const isMobile = useIsMobile()
  const convoId = useApp((s) => s.selectedConversationId)
  const view = useApp((s) => s.view)
  // Free tier is BYOA-only: gate the app behind pairing a computer until one
  // exists. The gate clears automatically when a non-cloud computer comes
  // online (WS computers.status → store → re-render). Wait for the computers
  // list to load before deciding, to avoid a flash of onboarding.
  const activeTier = useAuth((s) => s.companies.find((c) => c.id === s.activeCompanyId)?.tier)
  const computersLoaded = useComputers((s) => s.loaded)
  const hasOwnComputer = useComputers((s) => Object.values(s.byId).length > 0)
  const ownComputerCount = useComputers((s) => Object.values(s.byId).length)
  // Free tier is BYOA-only: gate the app into onboarding whenever a free
  // workspace has no paired (non-cloud) computer — pairing a machine is how a
  // free user activates their agents. We previously ALSO required
  // agentCount === 0, to avoid disrupting "grandfathered" free users who ran on
  // Cumora Cloud with no paired machine. That state has been eliminated (free
  // agents no longer run on managed cloud), so the guard is removed: any unpaired
  // free workspace — even one with existing (now-dormant) agents — is sent to
  // pair. `hasOwnComputer` checks existence not online status, so a
  // paired-but-asleep machine never re-triggers onboarding.
  const needsOnboarding = activeTier === 'free'
    && computersLoaded && !hasOwnComputer
  const hasDockUnread = useConversations((s) =>
    s.list.some((c) => !isMuted(c) && (c.unread ?? 0) > 0),
  )
  const selectedConvoExists = useConversations((s) =>
    convoId ? s.list.some((c) => c.id === convoId) : false,
  )
  const [updaterOpen, setUpdaterOpen] = useState(false)
  // Dev shortcut to preview the onboarding screen on demand (⌘/Ctrl+Shift+O),
  // regardless of tier/computer state. Toggle again to exit. The preview also
  // auto-clears once a NEW computer pairs while you're previewing, so you can
  // test the real "pair → advance into the app" behavior. baselineRef captures
  // the own-computer count at the moment you toggle the preview on.
  const [forceOnboarding, setForceOnboarding] = useState(false)
  const baselineRef = useRef(0)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.shiftKey && e.key.toLowerCase() === 'o') {
        e.preventDefault()
        setForceOnboarding((v) => {
          if (!v) baselineRef.current = Object.values(useComputers.getState().byId).length
          return !v
        })
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])
  useEffect(() => {
    if (forceOnboarding && ownComputerCount > baselineRef.current) {
      setForceOnboarding(false)
      useApp.getState().setView('agents')
    }
  }, [forceOnboarding, ownComputerCount])
  // When real onboarding completes (a computer connected → the gate flips
  // off), land the user on the Agents view to meet / add to their team.
  const wasOnboarding = useRef(needsOnboarding)
  useEffect(() => {
    if (wasOnboarding.current && !needsOnboarding) {
      // Onboarding just completed: the pair flow seeded the starter team +
      // "Everyone" group server-side (and only broadcasts the computer online
      // once that's done). Pull both lists in now so the Conversations/Agents
      // views are populated immediately, instead of waiting for WS events to
      // trickle in and leaving the user staring at an empty list.
      void useParticipants.getState().load()
      void useConversations.getState().reload()
      useApp.getState().setView('agents')
    }
    wasOnboarding.current = needsOnboarding
  }, [needsOnboarding])

  useEffect(() => {
    bootMessagesStream()
    bootParticipants()
    bootConversations()
    bootWhispers()
    bootComputers()
    void usePrefs.getState().load()
  }, [])

  useEffect(() => {
    window.cumora?.dock?.setUnreadDot(hasDockUnread)
  }, [hasDockUnread])

  useEffect(() => {
    return () => window.cumora?.dock?.setUnreadDot(false)
  }, [])

  // Lazy-load messages + mark conversation as read when selected
  useEffect(() => {
    if (!convoId || !selectedConvoExists) return
    void useMessages.getState().loadConversation(convoId)
    void api.markRead(convoId).then(() => {
      // refresh list so the badge clears
      void useConversations.getState().reload()
    }).catch(() => { /* swallow */ })
  }, [convoId, selectedConvoExists])

  // Lazy-refresh whisper list when entering whispers view
  useEffect(() => {
    if (view === 'whispers') useWhispers.getState().loadList()
  }, [view])

  // Cross-component "open updater" channel — MeView's "Check for
  // updates" button posts this event to ask AuthedApp to open the
  // dialog without prop-drilling.
  useEffect(() => {
    const onOpen = () => setUpdaterOpen(true)
    window.addEventListener('cumora:open-updater', onOpen)
    return () => window.removeEventListener('cumora:open-updater', onOpen)
  }, [])

  return (
    <>
      <Suspense fallback={<ShellFallback />}>
        {(needsOnboarding || forceOnboarding) ? <Onboarding /> : (isMobile ? <MobileApp /> : <DesktopApp />)}
      </Suspense>
      {/* In-app message toasts (window-blur / different-convo only) —
          rendered at the AuthedApp level so they share auth context and
          unmount cleanly on sign-out. */}
      <NotificationToasts />
      <UpdateBanner onOpen={() => setUpdaterOpen(true)} />
      <UpdaterDialog open={updaterOpen} onClose={() => setUpdaterOpen(false)} />
    </>
  )
}

export function App() {
  // The Electron notification BrowserWindow loads this same React bundle
  // with a `#notifications` hash. Bypass everything else (auth, stores,
  // routing) and just render the toast stack — it receives payloads over
  // IPC from the main window. Lazy chunk (see declaration site): the
  // ShellFallback keeps the window blank-but-sized for the one fetch.
  if (isNotificationWindow) {
    return (
      <Suspense fallback={<ShellFallback />}>
        <NotificationWindow />
      </Suspense>
    )
  }

  // Waitlist landing — handleCallback redirects here with `#waitlist=1`
  // when a brand-new OAuth visitor hit the gate. Consume the fragment
  // once so refresh doesn't pin them on this screen forever. State is
  // read-only — the screen's dismiss button calls location.reload() to
  // fall back into the normal flow.
  const [waitlist] = useState<{ email: string | null } | null>(() => consumeWaitlistFragment())
  // Suspension landing — handleCallback redirects here with
  // `#suspended=1&email=...&reason=...` when a returning user whose
  // account is currently suspended finishes OAuth. Same consume-once
  // semantics as the waitlist screen.
  const [suspended] = useState<{ email: string | null; reason: string | null } | null>(
    () => consumeSuspendedFragment(),
  )

  // Force AuthedApp to remount when the user logs in/out OR switches between
  // companies — every store keys off the active tenant, so a clean remount is
  // the simplest way to reload all data without stale rows leaking across.
  const userId = useAuth((s) => s.user?.id ?? null)
  const companyId = useAuth((s) => s.activeCompanyId)

  // Invite-link handling — runs BEFORE the rest of the shell so the
  // accept page is the user's first impression when they open a link. The
  // token can ride in via URL (fresh click) or localStorage (resumed
  // after an OAuth round-trip). The state is unset on done so the user
  // lands in the freshly-joined workspace. We do NOT support cumora://
  // deep links: invite URLs are always https://<web>/invite/<token>, so
  // the OS hands them to the default browser — Electron picks up the new
  // workspace on its next /auth/me refresh.
  const [inviteToken, setInviteToken] = useState<string | null>(() => {
    const fromUrl = consumeInviteFromUrl()
    if (fromUrl) {
      // Scrub the URL immediately so a refresh doesn't re-fire the same
      // flow; the token stays in component state.
      fromUrl.clear()
      return fromUrl.token
    }
    return getPendingInvite()
  })

  if (waitlist) {
    return (
      <Suspense fallback={<ShellFallback />}>
        <WaitlistConfirmedScreen email={waitlist.email} />
      </Suspense>
    )
  }

  if (suspended) {
    return (
      <Suspense fallback={<ShellFallback />}>
        <SuspendedScreen email={suspended.email} reason={suspended.reason} />
      </Suspense>
    )
  }

  // Admin panel runs in its own shell — different sidebar, different
  // routes, no chat stores. It still goes through AuthGate so the user
  // signs in via the same OAuth flow before reaching it.
  if (isAdminContext()) {
    return (
      <AuthGate>
        <ErrorBoundary>
          <Suspense fallback={<ShellFallback />}>
            <AdminApp />
          </Suspense>
        </ErrorBoundary>
      </AuthGate>
    )
  }

  if (inviteToken) {
    const screen = (
      <InviteAcceptScreen
        token={inviteToken}
        onDone={() => { clearPendingInvite(); setInviteToken(null) }}
      />
    )
    return (
      <AuthGate unauthFallback={screen}>
        {screen}
      </AuthGate>
    )
  }

  // Public web client (app.cumora.ai) — Cumora ships only as a desktop
  // app; the web surface is intentionally limited to OAuth sign-in, the
  // waitlist verdict (handled above), and a cumora:// hand-off. The full
  // chat shell below is reserved for Electron.
  if (isWebAppHost) {
    return (
      <ErrorBoundary>
        <WebShell />
      </ErrorBoundary>
    )
  }

  return (
    <AuthGate>
      <ErrorBoundary>
        <AuthedApp key={`${userId ?? 'anon'}::${companyId ?? 'none'}`} />
      </ErrorBoundary>
    </AuthGate>
  )
}
