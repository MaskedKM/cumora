// 我的视图桶(#219 ④)profile 件 —— 从 MeView.tsx 原样搬移:ProfileTab(身份卡
// +会话卡)+ CommunitySection(Discord 入口)+ AboutSection(版本/自动更新)。
import { useEffect, useState } from 'react'
import { api, getServerOrigin } from '@/api/client'
import { Avatar } from '@/components/Avatar'
import { useT } from '@/lib/i18n'
import { useAuth } from '@/stores/auth'
import { useParticipants } from '@/stores/participants'
import { Section } from './shared'

export function ProfileTab() {
  // Pull both the auth user (real account: id, email, providers) and the
  // matching participant (for avatar). They're usually the same person but
  // participant rows can lag in the local cache, so we don't gate on it.
  const t = useT()
  const authUser = useAuth((s) => s.user)
  const meParticipant = useParticipants((s) => (authUser ? s.byId[authUser.id] : null))
  const serverOrigin = getServerOrigin() || 'same-origin (Vite proxy)'

  async function signOut() {
    // Server-side: revoke the session row so the token is dead even if it
    // leaks. Best-effort — client clear still happens on network failure.
    try { await api.authLogout() } catch (e) { console.warn('[signout] server call failed', e) }
    useAuth.getState().clear()
    location.reload()
  }

  if (!authUser) return null
  const providers = authUser.providers ?? []
  return (
    <div className="space-y-6">
      <Section title={t('me.sectionIdentity')}>
        <div className="bg-cloud rounded-[14px] p-5 flex items-start gap-5"
          style={{ border: '1px solid var(--ink-100)' }}>
          {meParticipant
            ? <Avatar p={meParticipant} size={88} />
            : <div className="w-[88px] h-[88px] rounded-full bg-ink-100" />}
          <div className="flex-1 min-w-0">
            <h2 className="font-display font-medium text-[26px] tracking-tight truncate" style={{ letterSpacing: '-0.02em' }}>{authUser.name}</h2>
            <div className="font-display italic text-[14px] text-ink-500 truncate">{authUser.email}</div>
            <div className="flex items-center gap-2 mt-3 flex-wrap">
              {providers.map((p) => (
                <span key={p} className="text-[11px] uppercase tracking-wider px-2 py-0.5 rounded-full bg-white text-ink-600" style={{ border: '1px solid var(--ink-100)' }}>
                  {p}
                </span>
              ))}
            </div>
          </div>
        </div>
      </Section>

      <Section title={t('me.sectionSession')}>
        <div className="bg-cloud rounded-[14px] p-5 flex items-center justify-between gap-4"
          style={{ border: '1px solid var(--ink-100)' }}>
          <div className="min-w-0">
            <div className="font-display text-[14px] text-ink-800">{t('me.sessionTitle', { server: serverOrigin })}</div>
            <div className="font-display italic text-[12px] text-ink-400 mt-0.5">
              {t('me.sessionHint')}
            </div>
          </div>
          <button
            type="button"
            onClick={signOut}
            className="shrink-0 h-9 px-4 rounded-[8px] bg-ink-800 hover:bg-ink-900 text-white text-[13px] font-display transition-colors"
          >
            {t('common.signOut')}
          </button>
        </div>
      </Section>

      <CommunitySection />

      <AboutSection />
    </div>
  )
}

/** Discord invite. Renders on every platform (web + desktop) so feedback
 *  has a single, advertised entry point that doesn't go through email. */
function CommunitySection() {
  const t = useT()
  return (
    <Section title={t('me.sectionCommunity')}>
      <div className="bg-cloud rounded-[14px] p-5 flex items-center justify-between gap-4"
        style={{ border: '1px solid var(--ink-100)' }}>
        <div className="min-w-0">
          <div className="font-display text-[14px] text-ink-800">{t('me.communityTitle')}</div>
          <div className="font-display italic text-[12px] text-ink-400 mt-0.5">
            {t('me.communityHint')}
          </div>
        </div>
        <a
          href="https://discord.gg/hzfcsB6vMr"
          target="_blank"
          rel="noopener noreferrer"
          className="shrink-0 h-9 px-4 rounded-[8px] text-[13px] font-display transition-colors text-white inline-flex items-center gap-2"
          style={{ background: '#5865F2' }}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
            <path d="M20.317 4.3698a19.7913 19.7913 0 0 0-4.8851-1.5152.0741.0741 0 0 0-.0785.0371c-.211.3753-.4447.8648-.6083 1.2495-1.8447-.2762-3.68-.2762-5.4868 0-.1636-.3933-.4058-.8742-.6177-1.2495a.077.077 0 0 0-.0785-.037 19.7363 19.7363 0 0 0-4.8852 1.515.0699.0699 0 0 0-.0321.0277C.5334 9.0458-.319 13.5799.0992 18.0578a.0824.0824 0 0 0 .0312.0561c2.0528 1.5076 4.0413 2.4228 5.9929 3.0294a.0777.0777 0 0 0 .0842-.0276c.4616-.6304.8731-1.2952 1.226-1.9942a.076.076 0 0 0-.0416-.1057c-.6528-.2476-1.2743-.5495-1.8722-.8923a.077.077 0 0 1-.0076-.1277c.1258-.0943.2517-.1923.3718-.2914a.0743.0743 0 0 1 .0776-.0105c3.9278 1.7933 8.18 1.7933 12.0614 0a.0739.0739 0 0 1 .0785.0095c.1202.099.246.1981.3728.2924a.077.077 0 0 1-.0066.1276 12.2986 12.2986 0 0 1-1.873.8914.0766.0766 0 0 0-.0407.1067c.3604.698.7719 1.3628 1.225 1.9932a.076.076 0 0 0 .0842.0286c1.961-.6067 3.9495-1.5219 6.0023-3.0294a.077.077 0 0 0 .0313-.0552c.5004-5.177-.8382-9.6739-3.5485-13.6604a.061.061 0 0 0-.0312-.0286zM8.02 15.3312c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9555-2.4189 2.157-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.9555 2.4189-2.1569 2.4189zm7.9748 0c-1.1825 0-2.1569-1.0857-2.1569-2.419 0-1.3332.9554-2.4189 2.1569-2.4189 1.2108 0 2.1757 1.0952 2.1568 2.419 0 1.3332-.946 2.4189-2.1568 2.4189Z" />
          </svg>
          {t('me.joinDiscord')}
        </a>
      </div>
    </Section>
  )
}

/** Cumora version + auto-update entry point. Renders only when the
 *  Electron bridge is available (PWA / web builds have no updater).
 *  Click "Check for updates" → opens the UpdaterDialog mounted at the
 *  AuthedApp level via a custom window event (avoids prop-drilling
 *  through three layers of view components). */
function AboutSection() {
  const t = useT()
  const [version, setVersion] = useState<string | null>(null)
  const [supported, setSupported] = useState<boolean>(false)

  useEffect(() => {
    const bridge = typeof window !== 'undefined' ? window.cumora?.update : undefined
    if (!bridge) return
    void bridge.getAppInfo().then((info) => {
      setVersion(info.version)
      setSupported(info.autoUpdateSupported)
    }).catch(() => { /* swallow — section just hides */ })
  }, [])

  if (!version) return null

  return (
    <Section title={t('me.sectionAbout')}>
      <div className="bg-cloud rounded-[14px] p-5 flex items-center justify-between gap-4"
        style={{ border: '1px solid var(--ink-100)' }}>
        <div className="min-w-0">
          <div className="font-display text-[14px] text-ink-800">{t('me.versionLine', { version })}</div>
          <div className="font-display italic text-[12px] text-ink-400 mt-0.5">
            {supported ? t('me.autoUpdateDaily') : t('me.autoUpdateUnsupported')}
          </div>
        </div>
        <button
          type="button"
          onClick={() => window.dispatchEvent(new CustomEvent('cumora:open-updater'))}
          className="shrink-0 h-9 px-4 rounded-[8px] text-[13px] font-display transition-colors text-white"
          style={{ background: 'var(--skype)' }}
        >
          {t('me.checkUpdates')}
        </button>
      </div>
    </Section>
  )
}
