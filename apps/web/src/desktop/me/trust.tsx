// 我的视图桶(#219 ④)trust 件 —— 从 MeView.tsx 原样搬移:TrustTab(逐 agent
// 自治阈值滑杆+战绩三格)+ Stat(战绩数字原子)。
import { Avatar } from '@/components/Avatar'
import { type MessageKey, useT } from '@/lib/i18n'
import { useParticipants } from '@/stores/participants'
import { usePrefs } from '@/stores/preferences'
import { Section } from './shared'

export function TrustTab() {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const autonomy = usePrefs((s) => s.autonomy)
  const setAutonomy = usePrefs((s) => s.setAutonomy)
  const agents = Object.values(byId).filter((p) => p.kind === 'agent')

  return (
    <div className="space-y-6">
      <Section title={t('me.sectionPerAgent')}>
        <div className="text-[13px] text-ink-500 leading-[1.55] mb-4 max-w-2xl font-display italic">
          {t('me.perAgentIntro')}
        </div>
        <div className="space-y-2">
          {agents.map((a) => {
            const trust = autonomy[a.id]?.threshold ?? 0.6
            return (
              <div key={a.id} className="bg-cloud rounded-[12px] p-4 grid grid-cols-[184px_minmax(0,1fr)] items-center gap-5"
                style={{ border: '1px solid var(--ink-100)' }}>
                <div className="flex items-center gap-4 min-w-0">
                  <Avatar p={a} size={36} showStatus={false} />
                  <div className="min-w-0">
                    <div className="font-bold text-[13.5px] text-ink-900 truncate">{a.name}</div>
                    <div className="font-display italic text-[11.5px] text-ink-500 truncate">{a.role}</div>
                  </div>
                </div>
                <div className="min-w-0">
                  <div className="text-[11px] text-ink-500 mb-1.5 flex justify-between">
                    <span>{t('me.autonomyThreshold')}</span>
                    <span className="font-mono text-[11px] font-semibold text-ink-700">{trust.toFixed(2)}</span>
                  </div>
                  <input type="range" min={0} max={1} step={0.01} value={trust}
                    onChange={(e) => setAutonomy(a.id, parseFloat(e.target.value))}
                    className="w-full accent-whisper" />
                </div>
              </div>
            )
          })}
        </div>
      </Section>

      <Section title={t('me.sectionTrackRecords')}>
        <div className="grid grid-cols-3 gap-3">
          {agents.slice(0, 3).map((a) => {
            const ar = autonomy[a.id]
            return (
              <div key={a.id} className="bg-cloud rounded-[12px] p-4"
                style={{ border: '1px solid var(--ink-100)' }}>
                <div className="flex items-center gap-2.5 mb-3">
                  <Avatar p={a} size={28} showStatus={false} />
                  <div className="font-bold text-[13px] text-ink-900">{a.name}</div>
                </div>
                <div className="grid grid-cols-3 gap-1.5 text-center">
                  <Stat n={ar?.pulled ?? 0} l="me.statPulled" tone="good" />
                  <Stat n={ar?.led ?? 0} l="me.statLed" tone="good" />
                  <Stat n={ar?.dissolved ?? 0} l="me.statNoise" tone="warn" />
                </div>
              </div>
            )
          })}
        </div>
      </Section>
    </div>
  )
}

function Stat({ n, l, tone }: { n: number; l: MessageKey; tone: 'good' | 'warn' }) {
  const t = useT()
  return (
    <div>
      <div className="font-display text-[20px] font-medium" style={{ color: tone === 'good' ? 'var(--avail)' : 'var(--coral-deep)' }}>{n}</div>
      <div className="text-[9px] font-bold text-ink-300 uppercase tracking-wider">{t(l)}</div>
    </div>
  )
}
