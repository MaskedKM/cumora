// 我的视图桶(#219 ④)preferences 件 —— 从 MeView.tsx 原样搬移:PreferencesTab
// (PREF_GROUPS 开关组+语言+提示音+开发者模式)+ LanguageSection + SkypeSoundSection。
import { useEffect } from 'react'
import { Checkbox } from '@/components/Checkbox'
import { LanguagePicker } from '@/components/LanguagePicker'
import { type MessageKey, useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useDevtools } from '@/stores/devtools'
import { usePrefs } from '@/stores/preferences'
import { useSoundStore } from '@/stores/sound'
import { Section } from './shared'

// `lbl` / `sub` are message keys — the preference `key` is the server-side
// identifier and never changes with the language.
const PREF_GROUPS: Array<{ title: MessageKey; items: Array<{ key: string; lbl: MessageKey; sub: MessageKey; default: boolean }> }> = [
  {
    title: 'me.prefs.notifications',
    items: [
      { key: 'notify.group_pulled', lbl: 'me.prefs.groupPulled', sub: 'me.prefs.groupPulledSub', default: true },
      { key: 'notify.whisper_mention', lbl: 'me.prefs.whisperMention', sub: 'me.prefs.whisperMentionSub', default: true },
      { key: 'notify.convene_called', lbl: 'me.prefs.conveneCalled', sub: 'me.prefs.conveneCalledSub', default: true },
      { key: 'notify.daily_summary', lbl: 'me.prefs.dailySummary', sub: 'me.prefs.dailySummarySub', default: false },
    ],
  },
  {
    title: 'me.prefs.lookFeel',
    items: [
      { key: 'ui.reduce_motion', lbl: 'me.prefs.reduceMotion', sub: 'me.prefs.reduceMotionSub', default: false },
      { key: 'ui.typing_indicators', lbl: 'me.prefs.typingIndicators', sub: 'me.prefs.typingIndicatorsSub', default: true },
      { key: 'ui.thoughts_in_main', lbl: 'me.prefs.thoughtsInMain', sub: 'me.prefs.thoughtsInMainSub', default: false },
    ],
  },
  {
    title: 'me.prefs.privacy',
    items: [
      { key: 'priv.allow_silent_whispers', lbl: 'me.prefs.silentWhispers', sub: 'me.prefs.silentWhispersSub', default: true },
      { key: 'priv.allow_new_tools', lbl: 'me.prefs.newTools', sub: 'me.prefs.newToolsSub', default: true },
      { key: 'priv.allow_human_invites', lbl: 'me.prefs.humanInvites', sub: 'me.prefs.humanInvitesSub', default: false },
    ],
  },
]

export function PreferencesTab() {
  const t = useT()
  const prefs = usePrefs((s) => s.prefs)
  const setPref = usePrefs((s) => s.setPref)
  const devtoolsEnabled = useDevtools((s) => s.enabled)
  const devtoolsCanEnable = useDevtools((s) => s.canEnable)
  const devtoolsLocal = useDevtools((s) => s.localDev)
  const setDevMode = useDevtools((s) => s.setDevMode)
  const loadDevtools = useDevtools((s) => s.load)
  const get = (k: string, fallback: boolean) => (prefs[k] === undefined ? fallback : Boolean(prefs[k]))

  useEffect(() => {
    void loadDevtools()
  }, [loadDevtools])

  return (
    <div className="space-y-6">
      {PREF_GROUPS.map((g) => (
        <Section key={g.title} title={`↳ ${t(g.title)}`}>
          <div className="bg-cloud rounded-[14px] divide-y divide-ink-100"
            style={{ border: '1px solid var(--ink-100)' }}>
            {g.items.map((it, i) => {
              const on = get(it.key, it.default)
              return (
                <div key={i} className="flex items-center gap-4 p-4 cursor-pointer" onClick={() => setPref(it.key, !on)}>
                  <div className="flex-1 min-w-0">
                    <div className="font-semibold text-[13px] text-ink-900">{t(it.lbl)}</div>
                    <div className="font-display italic font-normal text-[11.5px] text-ink-500 mt-0.5">{t(it.sub)}</div>
                  </div>
                  <span className={cn('w-9 h-5 rounded-full relative shrink-0 transition-colors', on ? 'bg-skype' : 'bg-ink-200')}>
                    <span className={cn('absolute w-4 h-4 bg-white rounded-full top-0.5 transition-all', on ? 'left-[18px]' : 'left-0.5')}
                      style={{ boxShadow: '0 1px 3px rgba(0,0,0,0.2)' }} />
                  </span>
                </div>
              )
            })}
          </div>
        </Section>
      ))}
      <LanguageSection />
      <SkypeSoundSection />
      {devtoolsCanEnable && (
        <Section title={`↳ ${t('me.prefs.developer')}`}>
          <Checkbox
            checked={devtoolsEnabled}
            disabled={devtoolsLocal}
            onCheckedChange={(next) => { void setDevMode(next) }}
            label={t('me.prefs.developerMode')}
            description={devtoolsLocal
              ? t('me.prefs.developerModeLocal')
              : t('me.prefs.developerModeSub')}
          />
        </Section>
      )}
    </div>
  )
}

function LanguageSection() {
  const t = useT()
  return (
    <Section title={`↳ ${t('me.prefs.languageSection')}`}>
      <div className="bg-cloud rounded-[14px] p-4 flex items-center gap-4"
        style={{ border: '1px solid var(--ink-100)' }}>
        <div className="flex-1 min-w-0">
          <div className="font-semibold text-[13px] text-ink-900">{t('common.language')}</div>
          <div className="font-display italic font-normal text-[11.5px] text-ink-500 mt-0.5">
            {t('common.languageSub')}
          </div>
        </div>
        <LanguagePicker className="w-[180px] shrink-0" />
      </div>
    </Section>
  )
}

function SkypeSoundSection() {
  // Local-only toggle — see stores/sound.ts for why this isn't synced
  // through the server preferences store. Default is muted; users opt
  // in if they want the classic (clap) / (drum) chimes.
  const t = useT()
  const muted = useSoundStore((s) => s.muted)
  const setMuted = useSoundStore((s) => s.setMuted)
  const on = !muted
  return (
    <Section title={`↳ ${t('me.prefs.skypeSection')}`}>
      <div className="bg-cloud rounded-[14px]"
        style={{ border: '1px solid var(--ink-100)' }}>
        <div className="flex items-center gap-4 p-4 cursor-pointer" onClick={() => setMuted(on)}>
          <div className="flex-1 min-w-0">
            <div className="font-semibold text-[13px] text-ink-900">{t('me.prefs.skypeSounds')}</div>
            <div className="font-display italic font-normal text-[11.5px] text-ink-500 mt-0.5">
              {t('me.prefs.skypeSoundsSub')}
            </div>
          </div>
          <span className={cn('w-9 h-5 rounded-full relative shrink-0 transition-colors', on ? 'bg-skype' : 'bg-ink-200')}>
            <span className={cn('absolute w-4 h-4 bg-white rounded-full top-0.5 transition-all', on ? 'left-[18px]' : 'left-0.5')}
              style={{ boxShadow: '0 1px 3px rgba(0,0,0,0.2)' }} />
          </span>
        </div>
      </div>
    </Section>
  )
}
