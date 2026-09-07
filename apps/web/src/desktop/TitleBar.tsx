import type React from 'react'
import { CloudLogo } from '@/components/Avatar'
import { CompanySwitcher } from '@/components/CompanySwitcher'
import { IBoard } from '@/components/icons'
import { isElectron, trafficLightInset } from '@/lib/runtime'
import { useT } from '@/lib/i18n'
import { useApp } from '@/stores/app'

export function TitleBar() {
  const t = useT()
  const view = useApp((s) => s.view)
  const setView = useApp((s) => s.setView)
  // 评审 P3-4(#372):scrim 只盖内容行,标题栏暴露在外 —— 这里换场必须
  // 顺手关菜单,与 B 键路径(closeNavMenu 先行)保持一致。
  const closeNavMenu = useApp((s) => s.closeNavMenu)
  // In Electron with hidden titleBarStyle on mac, native traffic lights land in this strip.
  // Reserve space on the left for them, and make the bar a draggable region.
  const dragStyle = isElectron
    ? { WebkitAppRegion: 'drag' as const, userSelect: 'none' as const }
    : {}

  // Three equal-flex columns so the middle cell (and therefore the title)
  // is anchored to the WINDOW's horizontal center regardless of how wide
  // the left (traffic lights) or right (workspace switcher) cells happen to
  // be. The auto middle column shrinks to the title's intrinsic width,
  // so the 1fr cells on either side balance perfectly.
  const reservedLeft = Math.max(84, trafficLightInset)
  return (
    <header
      className="grid items-center px-4 border-b border-ink-100"
      style={{
        height: 44,
        background: 'linear-gradient(180deg, #FBFDFF 0%, #F1F7FB 100%)',
        gridTemplateColumns: `1fr auto 1fr`,
        ...dragStyle,
      }}
    >
      {!isElectron ? (
        <div className="flex gap-2" style={{ paddingLeft: 0 }}>
          <span className="w-3 h-3 rounded-full" style={{ background: '#FF6058', boxShadow: 'inset 0 -1px 0 rgba(0,0,0,0.1)' }} />
          <span className="w-3 h-3 rounded-full" style={{ background: '#FFBD2E', boxShadow: 'inset 0 -1px 0 rgba(0,0,0,0.1)' }} />
          <span className="w-3 h-3 rounded-full" style={{ background: '#28C940', boxShadow: 'inset 0 -1px 0 rgba(0,0,0,0.1)' }} />
        </div>
      ) : (
        // Empty cell — native traffic lights paint over this region on mac.
        // We still need at least `reservedLeft` of width so the title's 1fr
        // start can't push back to 0 (which would let the title slide under
        // the traffic lights).
        <div style={{ minWidth: reservedLeft }} />
      )}
      <div className="flex items-center justify-center gap-2.5 font-display font-medium text-[14px] text-ink-700 tracking-wide whitespace-nowrap">
        <CloudLogo />
        <span>Cumora</span>
        <em className="font-normal text-ink-500" style={{ fontStyle: 'italic' }}>{t('common.titlebarTagline')}</em>
      </div>
      <div className="flex items-center justify-end gap-2 pr-2">
        {/* #368 刀1:非对话视图的临时返回出口 —— ☰ 触发钮在会话列表头,进了
            二级视图后列表不在场,没有它就是死胡同。刀2 的全屏视图壳
            (‹ 返回对话)落地后此钮退役。 */}
        {view !== 'conversations' && (
          <button
            type="button"
            onClick={() => { closeNavMenu(); setView('conversations') }}
            className="inline-flex h-7 items-center gap-1 rounded-lg border border-ink-200 bg-cloud px-2.5 text-[12.5px] text-ink-700 transition-colors hover:border-skype hover:bg-sky2-50 hover:text-skype-deep"
            style={isElectron ? { WebkitAppRegion: 'no-drag' } as React.CSSProperties : undefined}
            aria-label={t('nav.conversations')}
          >
            ‹ <span className="hidden sm:inline">{t('nav.conversations')}</span>
          </button>
        )}
        {/* #368 刀1:看板是唯一保留一键可达的非聊天面(ADR 0009)——标题栏
            常驻 + B 快捷键(DesktopApp 注册)。no-drag 让按钮在 Electron
            拖拽区里仍可点击。 */}
          <button
            type="button"
            onClick={() => { closeNavMenu(); setView(view === 'boards' ? 'conversations' : 'boards') }}
          className="inline-flex h-7 items-center gap-1.5 rounded-lg border border-ink-200 bg-cloud px-2.5 text-[12.5px] text-ink-700 transition-colors hover:border-skype hover:bg-sky2-50 hover:text-skype-deep"
          style={isElectron ? { WebkitAppRegion: 'no-drag' } as React.CSSProperties : undefined}
          title={`${t('nav.boards')} (B)`}
          aria-label={t('nav.boards')}
        >
          <IBoard className="w-3.5 h-3.5" strokeWidth={2} />
          <span className="hidden sm:inline">{t('nav.boards')}</span>
          <kbd className="rounded border border-ink-200 px-1 font-sans text-[10px] leading-4 text-ink-300">B</kbd>
        </button>
        <CompanySwitcher />
      </div>
    </header>
  )
}
