// NavMenu —— #368 刀1:左侧全高滑出菜单(rail 退役后的二级导航层,ADR 0009)。
// 垂直层级:头像行(我)→ 工作面组 → 公司组(弱化)→ 退出钉底。权限闸随项:
// 人事 = owner/admin(服务端同闸),观测 = devtools;私聊不在此列 —— 它已并入
// 会话列表的「Agent 对话」分区(WhispersView 退役)。
import { useEffect, useState } from 'react'
import { api } from '@/api/client'
import { Avatar } from '@/components/Avatar'
import { IAgent, IAgents, ICalendar, IDoc, IExit, IFile, IFolder, IObserve, IShip } from '@/components/icons'
import { type MessageKey, useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useAuth, useMe } from '@/stores/auth'
import { useComputers } from '@/stores/computers'
import { useDevtools } from '@/stores/devtools'
import { useParticipants } from '@/stores/participants'
import type { Participant, ViewKey } from '@/types'

type Item = { key: ViewKey['view']; Icon: typeof ICalendar; label: MessageKey }

// 工作面 = 内容产物面;公司 = 配置/观察面(渲染层弱化,拉开层级)。
const WORK_ITEMS: Item[] = [
  { key: 'calendar', Icon: ICalendar, label: 'nav.calendar' },
  { key: 'documents', Icon: IDoc, label: 'nav.docs' },
  { key: 'projects', Icon: IFolder, label: 'nav.projects' },
  { key: 'shipping', Icon: IShip, label: 'nav.ship' },
]
const COMPANY_ITEMS: Item[] = [
  { key: 'agents', Icon: IAgent, label: 'nav.agents' },
  { key: 'skills', Icon: IFile, label: 'nav.skills' },
  { key: 'hr', Icon: IAgents, label: 'nav.hr' },
  { key: 'observability', Icon: IObserve, label: 'nav.observe' },
]

function MenuButton({ item, dim, badge }: { item: Item; dim?: boolean; badge?: number }) {
  const t = useT()
  const view = useApp((s) => s.view)
  const setView = useApp((s) => s.setView)
  const close = useApp((s) => s.closeNavMenu)
  const active = view === item.key
  return (
    <button
      type="button"
      onClick={() => { setView(item.key); close() }}
      className={cn(
        'flex w-full items-center gap-2.5 rounded-[10px] px-2.5 py-2 text-left transition-colors',
        dim ? 'text-[12.5px] text-ink-500 py-1.5' : 'text-[13.5px] text-ink-900',
        active ? 'bg-sky2-50 text-skype-deep' : 'hover:bg-sky2-50 hover:text-skype-deep',
      )}
    >
      <span
        className={cn(
          'grid shrink-0 place-items-center rounded-[9px]',
          dim ? 'w-[26px] h-[26px] bg-paper border border-ink-100' : 'w-8 h-8 bg-sky2-100',
        )}
      >
        <item.Icon className={dim ? 'w-[13px] h-[13px]' : 'w-4 h-4'} strokeWidth={2} />
      </span>
      <span className="truncate">{t(item.label)}</span>
      {badge !== undefined && badge > 0 && (
        <span
          className="ml-auto grid h-[18px] min-w-[18px] place-items-center rounded-full px-1 text-[10.5px] font-bold"
          style={{ background: 'var(--coral)', color: 'white' }}
        >{badge}</span>
      )}
    </button>
  )
}

export function NavMenu() {
  const t = useT()
  const open = useApp((s) => s.navMenuOpen)
  const close = useApp((s) => s.closeNavMenu)
  const setView = useApp((s) => s.setView)
  const devtoolsEnabled = useDevtools((s) => s.enabled)
  const isOwner = useAuth((s) => s.companies.find((c) => c.id === s.activeCompanyId)?.role === 'owner')
  const canManage = useAuth((s) => {
    const r = s.companies.find((c) => c.id === s.activeCompanyId)?.role
    return r === 'owner' || r === 'admin'
  })
  // Any paired computer running an outdated daemon → gold dot on the avatar
  // (migrated from Rail): the upgrade nudge stays visible app-wide.
  const daemonOutdated = useComputers((s) => Object.values(s.byId).some((c) => c.daemonOutdated))

  // 待批提案数:每次开菜单现拉一次(owner/admin 且菜单开着才请求)——不轮询,
  // 打开即最新,与 HrView 的轮询面互不干扰。
  const [hrOpen, setHrOpen] = useState<number | undefined>(undefined)
  useEffect(() => {
    if (!open || !canManage) { setHrOpen(undefined); return }
    let alive = true
    api.listHrProposals('open')
      .then(({ rows }) => { if (alive) setHrOpen(rows.length) })
      .catch(() => { if (alive) setHrOpen(undefined) })
    return () => { alive = false }
  }, [open, canManage])

  // Esc closes — the menu is the topmost layer it can dismiss (peek panes /
  // threads handle their own Esc).
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') close() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, close])

  // The profile row's avatar is the SIGNED-IN user (same resolution chain the
  // old Rail top avatar used) — Gravatar shows once participants load, with a
  // minimal ad-hoc fallback to avoid a placeholder flash.
  const meId = useMe()
  const authUser = useAuth((s) => s.user)
  const byId = useParticipants((s) => s.byId)
  const meParticipant: Participant | null = (meId && byId[meId]) ? byId[meId] : null
  const fallback: Participant = {
    id: authUser?.id ?? 'me',
    kind: 'human',
    name: authUser?.name ?? t('common.you'),
    initial: (authUser?.name ?? 'Y').charAt(0).toUpperCase(),
    avatarBg: 'linear-gradient(135deg, #FF7A6B, #F4B740)',
    status: 'avail',
  } as Participant
  const meAvatar = meParticipant ?? fallback

  const companyItems = COMPANY_ITEMS.filter((i) => {
    if (i.key === 'hr') return canManage
    if (i.key === 'observability') return devtoolsEnabled
    return true
  })

  return (
    <>
      {/* Scrim — click anywhere outside to dismiss. Opacity transition keeps
          the panel's slide from feeling disconnected from the dim behind it. */}
      <div
        onClick={close}
        aria-hidden="true"
        className={cn(
          'absolute inset-0 z-40 transition-opacity duration-200',
          open ? 'opacity-100' : 'pointer-events-none opacity-0',
        )}
        style={{ background: 'rgba(10, 27, 46, 0.30)' }}
      />
      <nav
        className={cn(
          'absolute inset-y-0 left-0 z-50 flex w-[300px] flex-col bg-cloud transition-transform duration-200 ease-out',
          open ? 'translate-x-0' : '-translate-x-full pointer-events-none',
        )}
        style={{ boxShadow: '24px 0 60px -18px rgba(10, 30, 60, 0.38), 1px 0 0 var(--ink-100)' }}
        aria-label={t('nav.menu')}
      >
        {/* Profile row — the single entry to the Me view (rail had two). */}
        <button
          type="button"
          onClick={() => { setView('me'); close() }}
          className="flex items-center gap-3 border-b border-ink-100 px-4 pb-[15px] pt-[18px] text-left transition-colors hover:bg-sky2-50"
          title={daemonOutdated ? t('common.daemonOutdatedTip') : (authUser?.name ?? t('common.you'))}
        >
          <span className="relative">
            <Avatar p={meAvatar} size={44} ringColor="var(--cloud)" />
            {daemonOutdated && (
              <span
                className="absolute -top-0.5 -right-0.5 w-3 h-3 rounded-full"
                style={{ background: 'var(--gold-deep)', border: '2px solid var(--cloud)' }}
              />
            )}
          </span>
          <span className="min-w-0 flex-1">
            <b className="block truncate text-[15px] font-semibold text-ink-900">{authUser?.name ?? t('common.you')}</b>
            <span className="block truncate text-[11.5px] text-ink-500">
              {isOwner ? t('menu.roleOwner') : t('menu.roleMember')}
            </span>
          </span>
          <span className="text-[20px] leading-none text-ink-300">›</span>
        </button>

        <div className="flex-1 overflow-y-auto px-2 py-2">
          <div className="px-2.5 pb-1">
            <span className="text-[10.5px] font-bold tracking-[0.05em] text-ink-300">{t('menu.work')}</span>
          </div>
          {WORK_ITEMS.map((item) => <MenuButton key={item.key} item={item} />)}
          <div className="mx-2.5 my-2.5 h-px bg-ink-100" />
          <div className="px-2.5 pb-1">
            <span className="text-[10.5px] font-bold tracking-[0.05em] text-ink-300">{t('menu.company')}</span>
          </div>
          {companyItems.map((item) => (
            <MenuButton key={item.key} item={item} dim badge={item.key === 'hr' ? hrOpen : undefined} />
          ))}
        </div>

        <button
          type="button"
          onClick={async () => {
            // Revoke session server-side before clearing local state, so a
            // leaked token isn't valid anywhere after sign out. Best-effort —
            // we still clear locally if the network is down.
            try { await api.authLogout() } catch (e) { console.warn('[signout] server call failed', e) }
            useAuth.getState().clear()
            location.reload()
          }}
          className="flex items-center gap-2.5 border-t border-ink-100 px-4 py-3 text-[13px] text-ink-500 transition-colors hover:bg-[#FFF6F5] hover:text-coral-deep"
        >
          <IExit className="w-[15px] h-[15px]" strokeWidth={2} />
          {t('common.signOut')}
        </button>
      </nav>
    </>
  )
}
