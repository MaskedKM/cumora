// 聊天顶栏件(#219 ⑤)—— 从 MobileChat.tsx 原样搬移:
// ChatHeader(返回/标题 AvatarStack 入口/召集/更多菜单:详情·静音·退群)。
// JSX 体仅去一级缩进,状态与动作仍由壳持有、经 props 透传。
import type { Dispatch, SetStateAction } from 'react'
import { AvatarStack } from '@/components/Avatar'
import { IBack, IConvene, IMore } from '@/components/icons'
import { useT } from '@/lib/i18n'
import type { Conversation, Participant } from '@/types'
import { Pressable } from '../Pressable'

/** 顶栏装配所需的壳侧依赖。形参名刻意与壳的变量同名,使下方搬移的
 *  JSX 体逐字节不变 —— 壳把它真实的值与 setter 透传进来。 */
interface ChatHeaderProps {
  c: Conversation
  agents: Participant[]
  select: (id: string | null) => void
  pushStack: (s: 'list' | 'chat' | 'info') => void
  onConvene: () => void
  conveneStarting: boolean
  menuOpen: boolean
  setMenuOpen: Dispatch<SetStateAction<boolean>>
  toggleMute: () => void
  muted: boolean
  leaveConvo: () => void
}

export function ChatHeader({
  c, agents, select, pushStack, onConvene, conveneStarting, menuOpen, setMenuOpen, toggleMute, muted, leaveConvo,
}: ChatHeaderProps) {
  const t = useT()
  return (
    <header
      className="bg-cloud/95 backdrop-blur-md sticky top-0 z-10 border-b border-ink-100"
      style={{ paddingTop: 'env(safe-area-inset-top)' }}
    >
      <div className="px-2 py-2.5 flex items-center gap-2">
        <Pressable
          onClick={() => select(null)}
          className="w-10 h-10 grid place-items-center text-ink-700 active:bg-sky2-50 rounded-full"
        >
          <IBack className="w-[22px] h-[22px]" strokeWidth={2} />
        </Pressable>
        <Pressable
          onClick={() => pushStack('info')}
          scale={0.985}
          className="flex-1 flex items-center gap-2.5 py-1 active:opacity-70"
        >
          <AvatarStack ps={agents} size={26} max={3} />
          <div className="text-left flex-1 min-w-0">
            <div className="font-display font-medium text-[16px] text-ink-900 leading-tight truncate" style={{ letterSpacing: '-0.01em' }}>
              {c.title}
            </div>
            <div className="text-[11px] font-semibold flex items-center gap-1 leading-none mt-0.5 text-working">
              <span className="w-1.5 h-1.5 rounded-full animate-pulse-soft bg-working" />
              {t('mobchat.agentsHeader', { n: agents.length })}
            </div>
          </div>
        </Pressable>
        <Pressable
          onClick={onConvene}
          disabled={conveneStarting}
          className="w-10 h-10 grid place-items-center text-ink-700 active:bg-sky2-50 rounded-full disabled:opacity-50"
          aria-label={t('mobchat.startConvene')}
        >
          <IConvene className="w-[20px] h-[20px]" />
        </Pressable>
        <div className="relative">
          <Pressable
            onClick={() => setMenuOpen((v) => !v)}
            haptic="medium"
            className="w-10 h-10 grid place-items-center text-ink-700 active:bg-sky2-50 rounded-full"
            aria-label={t('mobchat.more')}
          >
            <IMore className="w-[20px] h-[20px]" />
          </Pressable>
          {menuOpen && (
            <>
              <div
                className="fixed inset-0 z-20"
                onClick={() => setMenuOpen(false)}
              />
              <div
                className="absolute right-2 top-11 z-30 min-w-[180px] py-1 rounded-[12px] bg-paper animate-rise"
                style={{
                  border: '1px solid var(--ink-100)',
                  boxShadow: '0 12px 28px -8px rgba(10, 30, 60, 0.20), 0 4px 10px -4px rgba(10, 30, 60, 0.12)',
                }}
              >
                <button
                  onClick={() => { pushStack('info'); setMenuOpen(false) }}
                  className="w-full text-left py-2.5 px-3.5 text-[13px] text-ink-700 active:bg-sky2-50"
                >
                  {t('mobchat.viewDetails')}
                </button>
                <button
                  onClick={toggleMute}
                  className="w-full text-left py-2.5 px-3.5 text-[13px] text-ink-700 active:bg-sky2-50"
                >
                  {muted ? t('mobchat.unmute') : t('mobchat.muteNotifications')}
                </button>
                <div className="h-px bg-ink-100 mx-1.5 my-1" />
                <button
                  onClick={leaveConvo}
                  className="w-full text-left py-2.5 px-3.5 text-[13px] text-coral-deep active:bg-coral-soft/60"
                >
                  {t('mobchat.leaveConversation')}
                </button>
              </div>
            </>
          )}
        </div>
      </div>
    </header>
  )
}
