// 子组件分桶(#219 ④):本文件保留 MeView 壳(标签身份词表 tabs/Tab、活动页
// 状态、守护进程横幅编排、六页条件装配),原局部子组件按职责分居 ./me/:
//   profile(身份/会话/社区/关于)· usage(配额三卡族)· trust(自治阈值+战绩)
//   · projects(项目 CRUD)· preferences(开关组/语言/提示音/开发者)
//   · computers(设备配对+DaemonUpgradeBanner)· shared(Section 节壳)。
// store 消费面未动(zustand v5 无新对象 selector,无需 useShallow)。
import { useEffect, useState } from 'react'
import { type MessageKey, useT } from '@/lib/i18n'
import { isElectron } from '@/lib/runtime'
import { cn } from '@/lib/utils'
import { useComputers } from '@/stores/computers'
import { ComputersTab, DaemonUpgradeBanner } from './me/computers'
import { PreferencesTab } from './me/preferences'
import { ProfileTab } from './me/profile'
import { ProjectsTab } from './me/projects'
import { TrustTab } from './me/trust'
import { UsageTab } from './me/usage'
import { StackTab } from './me/StackTab'

// The tab's identity is its `key`; the label is a message key resolved at
// render. Before this they were the same string, which would have made
// the active tab depend on the UI language.
const baseTabs = [
  { key: 'profile', label: 'me.tab.profile' },
  { key: 'usage', label: 'me.tab.usage' },
  { key: 'computers', label: 'me.tab.computers' },
  { key: 'projects', label: 'me.tab.projects' },
  { key: 'trust', label: 'me.tab.trust' },
  { key: 'preferences', label: 'me.tab.preferences' },
] as const satisfies ReadonlyArray<{ key: string; label: MessageKey }>
// 本地栈控制台(#286):Electron 专属 —— 面板驱动的是本机 cumora-stack
// 二进制,浏览器形态没有这个面。
const tabs = isElectron
  ? [...baseTabs, { key: 'stack', label: 'me.tab.stack' } as { key: string; label: MessageKey }]
  : baseTabs
type Tab = (typeof tabs)[number]['key']

export function MeView() {
  const t = useT()
  const [tab, setTab] = useState<Tab>('profile')
  const hasOutdated = useComputers((s) => Object.values(s.byId).some((c) => c.daemonOutdated))
  useEffect(() => { void useComputers.getState().refresh() }, [])

  return (
    <main className="overflow-y-auto p-8 pt-6"
      style={{ background: 'linear-gradient(180deg, transparent, var(--paper))' }}>
      <div className="max-w-[1100px] mx-auto">
        <div className="mb-6">
          <h1 className="font-display font-medium text-[36px] tracking-tight text-ink-900 mb-1" style={{ letterSpacing: '-0.025em' }}>
            {t('me.headline')} <em className="italic text-coral-deep" style={{ fontStyle: 'italic', fontWeight: 400 }}>{t('me.headlineEm')}</em>
          </h1>
          <div className="font-display italic font-normal text-[15px] text-ink-500">
            {t('me.subtitle')}
          </div>
        </div>

        <DaemonUpgradeBanner onJump={() => setTab('computers')} />

        <div className="flex gap-1 mb-7 border-b border-ink-100">
          {tabs.map((tabDef, i) => (
            <button
              key={tabDef.key}
              onClick={() => setTab(tabDef.key)}
              className={cn(
                'py-2.5 text-[13px] font-semibold border-b-2 transition -mb-px inline-flex items-center gap-1.5',
                i === 0 ? 'pl-0 pr-5' : 'px-5',
                tab === tabDef.key ? 'border-skype text-skype-deep' : 'border-transparent text-ink-500 hover:text-ink-700',
              )}>
              {t(tabDef.label)}
              {tabDef.key === 'computers' && hasOutdated && (
                <span className="w-1.5 h-1.5 rounded-full" style={{ background: 'var(--gold-deep)' }} title={t('me.daemonNeedsUpdate')} />
              )}
            </button>
          ))}
        </div>

        {tab === 'profile' && <ProfileTab />}
        {tab === 'usage' && <UsageTab />}
        {tab === 'computers' && <ComputersTab />}
        {tab === 'projects' && <ProjectsTab />}
        {tab === 'trust' && <TrustTab />}
        {tab === 'preferences' && <PreferencesTab />}
        {tab === 'stack' && <StackTab />}
      </div>
    </main>
  )
}
