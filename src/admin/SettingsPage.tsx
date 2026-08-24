/**
 * Global toggles + Cerebellum Route config. Each control is a single call
 * to /settings. Renders pessimistically — disable the row while the
 * request flies so a fast double-click doesn't race the server.
 */
import { useEffect, useState, type ReactNode } from 'react'
import { useT } from '@/lib/i18n'
import { adminApi, type AdminSettings, type CerebellumRoute } from './api'

type BusyKey =
  | keyof AdminSettings
  | 'cerebellum_provider_group'
  | 'cerebellum_api_key'

export function SettingsPage() {
  const t = useT()
  const [s, setS] = useState<AdminSettings | null>(null)
  const [busyKey, setBusyKey] = useState<BusyKey | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [engines, setEngines] = useState<string[]>([])

  // Draft state for the fields that shouldn't submit on every keystroke
  // (unlike the toggles/selects below, which save immediately on change).
  const [providerDraftLoaded, setProviderDraftLoaded] = useState(false)
  const [draftProvider, setDraftProvider] = useState('')
  const [draftBaseUrl, setDraftBaseUrl] = useState('')
  const [draftModel, setDraftModel] = useState('')
  const [draftApiKey, setDraftApiKey] = useState('')

  useEffect(() => {
    adminApi.settings()
      .then(setS)
      .catch((e) => setErr(e instanceof Error ? e.message : String(e)))
    adminApi.availableEngines()
      .then((r) => setEngines(r.engines))
      .catch(() => { /* dropdown just shows the "no engines" warning */ })
  }, [])

  // First load only — later refreshes of `s` (from an unrelated toggle
  // save) shouldn't clobber text the operator is mid-typing.
  useEffect(() => {
    if (s && !providerDraftLoaded) {
      setDraftProvider(s.cerebellum_provider)
      setDraftBaseUrl(s.cerebellum_base_url)
      setDraftModel(s.cerebellum_model)
      setProviderDraftLoaded(true)
    }
  }, [s, providerDraftLoaded])

  const flip = async (key: 'waitlist_enabled' | 'signups_paused') => {
    if (!s || busyKey) return
    setBusyKey(key); setErr(null)
    try {
      const next = await adminApi.setSettings({ [key]: !s[key] })
      setS(next)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusyKey(null) }
  }

  const saveRoute = async (route: CerebellumRoute) => {
    if (!s || busyKey) return
    setBusyKey('cerebellum_route'); setErr(null)
    try {
      setS(await adminApi.setSettings({ cerebellum_route: route }))
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusyKey(null) }
  }

  const saveLocalEngine = async (engine: string) => {
    if (!s || busyKey) return
    setBusyKey('cerebellum_local_engine'); setErr(null)
    try {
      setS(await adminApi.setSettings({ cerebellum_local_engine: engine }))
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusyKey(null) }
  }

  const saveProviderGroup = async () => {
    if (!s || busyKey) return
    setBusyKey('cerebellum_provider_group'); setErr(null)
    try {
      const next = await adminApi.setSettings({
        cerebellum_provider: draftProvider,
        cerebellum_base_url: draftBaseUrl,
        cerebellum_model: draftModel,
      })
      setS(next)
      setDraftProvider(next.cerebellum_provider)
      setDraftBaseUrl(next.cerebellum_base_url)
      setDraftModel(next.cerebellum_model)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusyKey(null) }
  }

  const saveApiKey = async () => {
    if (!s || busyKey || !draftApiKey) return
    setBusyKey('cerebellum_api_key'); setErr(null)
    try {
      setS(await adminApi.setSettings({ cerebellum_api_key: draftApiKey }))
      setDraftApiKey('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusyKey(null) }
  }

  return (
    <div className="admin-page">
      <header className="admin-page-head">
        <div>
          <h1 className="admin-h1">{t('adminSettings.title')}</h1>
          <div className="admin-sub">{t('adminSettings.sub')}</div>
        </div>
      </header>

      {err && <div className="admin-banner-err">{err}</div>}

      <div className="admin-settings">
        <SettingRow
          title={t('adminSettings.waitlistTitle')}
          desc={t('adminSettings.waitlistDesc')}
          on={!!s?.waitlist_enabled}
          busy={busyKey === 'waitlist_enabled'}
          disabled={!s}
          onToggle={() => void flip('waitlist_enabled')}
        />
        <SettingRow
          title={t('adminSettings.signupsPausedTitle')}
          desc={t('adminSettings.signupsPausedDesc')}
          on={!!s?.signups_paused}
          busy={busyKey === 'signups_paused'}
          disabled={!s}
          onToggle={() => void flip('signups_paused')}
        />
      </div>

      <h1 className="admin-h1" style={{ fontSize: 18, marginTop: 32 }}>{t('adminSettings.cerebellumTitle')}</h1>
      <div className="admin-sub">{t('adminSettings.cerebellumDesc')}</div>

      <div className="admin-settings">
        <FieldRow
          title={t('adminSettings.cerebellumRouteTitle')}
          desc={t('adminSettings.cerebellumRouteDesc')}
        >
          <select
            className="admin-select"
            value={s?.cerebellum_route ?? 'remote'}
            disabled={!s || busyKey === 'cerebellum_route'}
            onChange={(e) => void saveRoute(e.target.value as CerebellumRoute)}
          >
            <option value="remote">{t('adminSettings.cerebellumRouteRemote')}</option>
            <option value="byoa">{t('adminSettings.cerebellumRouteByoa')}</option>
          </select>
        </FieldRow>

        <FieldRow
          title={t('adminSettings.cerebellumLocalEngineTitle')}
          desc={
            engines.length === 0
              ? t('adminSettings.cerebellumLocalEngineNoEngines')
              : t('adminSettings.cerebellumLocalEngineDesc')
          }
        >
          <select
            className="admin-select"
            value={s?.cerebellum_local_engine ?? ''}
            disabled={!s || busyKey === 'cerebellum_local_engine'}
            onChange={(e) => void saveLocalEngine(e.target.value)}
          >
            {s?.cerebellum_local_engine && !engines.includes(s.cerebellum_local_engine) && (
              <option value={s.cerebellum_local_engine}>{s.cerebellum_local_engine}</option>
            )}
            {engines.map((engine) => (
              <option key={engine} value={engine}>{engine}</option>
            ))}
          </select>
        </FieldRow>

        <FieldRow
          title={t('adminSettings.cerebellumProviderTitle')}
          desc={t('adminSettings.cerebellumProviderDesc')}
        >
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <input
              className="admin-input"
              placeholder={t('adminSettings.cerebellumProviderPlaceholder')}
              value={draftProvider}
              disabled={!s || busyKey === 'cerebellum_provider_group'}
              onChange={(e) => setDraftProvider(e.target.value)}
            />
            <input
              className="admin-input"
              placeholder={t('adminSettings.cerebellumBaseUrlPlaceholder')}
              value={draftBaseUrl}
              disabled={!s || busyKey === 'cerebellum_provider_group'}
              onChange={(e) => setDraftBaseUrl(e.target.value)}
            />
            <input
              className="admin-input"
              placeholder={t('adminSettings.cerebellumModelPlaceholder')}
              value={draftModel}
              disabled={!s || busyKey === 'cerebellum_provider_group'}
              onChange={(e) => setDraftModel(e.target.value)}
            />
            <button
              className="btn-primary"
              disabled={!s || busyKey === 'cerebellum_provider_group'}
              onClick={() => void saveProviderGroup()}
            >
              {t('adminSettings.cerebellumSave')}
            </button>
          </div>
        </FieldRow>

        <FieldRow
          title={t('adminSettings.cerebellumApiKeyTitle')}
          desc={
            s?.cerebellum_api_key_configured
              ? t('adminSettings.cerebellumApiKeyConfigured', { suffix: s.cerebellum_api_key_suffix ?? '' })
              : t('adminSettings.cerebellumApiKeyDesc')
          }
        >
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
            <input
              type="password"
              className="admin-input"
              placeholder={t('adminSettings.cerebellumApiKeyPlaceholder')}
              value={draftApiKey}
              disabled={!s || busyKey === 'cerebellum_api_key'}
              onChange={(e) => setDraftApiKey(e.target.value)}
            />
            <button
              className="btn-primary"
              disabled={!s || !draftApiKey || busyKey === 'cerebellum_api_key'}
              onClick={() => void saveApiKey()}
            >
              {t('adminSettings.cerebellumSave')}
            </button>
          </div>
        </FieldRow>
      </div>
    </div>
  )
}

function SettingRow({ title, desc, on, busy, disabled, onToggle }: {
  title: string; desc: string; on: boolean; busy: boolean; disabled: boolean; onToggle: () => void
}) {
  return (
    <div className="admin-setting">
      <div>
        <div className="admin-setting-title">{title}</div>
        <div className="admin-setting-desc">{desc}</div>
      </div>
      <button
        className={`admin-switch${on ? ' is-on' : ''}`}
        onClick={onToggle}
        disabled={disabled || busy}
        aria-pressed={on}
      >
        <span className="admin-switch-thumb" />
      </button>
    </div>
  )
}

/** Same row shell as `SettingRow`, but the right-hand control is arbitrary
 *  (select / text inputs / password field) instead of a fixed switch. */
function FieldRow({ title, desc, children }: {
  title: string; desc: string; children: ReactNode
}) {
  return (
    <div className="admin-setting">
      <div>
        <div className="admin-setting-title">{title}</div>
        <div className="admin-setting-desc">{desc}</div>
      </div>
      {children}
    </div>
  )
}
