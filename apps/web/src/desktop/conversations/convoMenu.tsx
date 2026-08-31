// 右键菜单装配件(#219 ②)—— 原样搬自壳的 openContextMenu 闭包体
// (仅去一级缩进;闭包捕获改为显式 ctx 透传,setter 名保持不变)。
import type React from 'react'
import type { ContextMenuItem } from '@/components/ContextMenu'
import { isMuted } from '@/stores/conversations'
import type { Conversation, Participant } from '@/types'
import { MUTE_DURATIONS, muteHint, type Translator } from './shared'

/** 菜单装配所需的壳侧依赖。setter 名刻意与壳的 useState 同名,
 *  使下方搬移的菜单体逐字节不变 —— 壳把它真实的 setter 透传进来。 */
export interface ConvoMenuCtx {
  t: Translator
  byId: Record<string, Participant>
  togglePin: (c: Conversation) => Promise<void>
  setMute: (c: Conversation, mute: boolean, until: Date | null) => Promise<void>
  otherMember: (c: Conversation) => string | null
  setAddingMembersTo: (c: Conversation) => void
  setConfirmLeave: (c: Conversation) => void
  setCreatingWithMember: (participantId: string) => void
  setAddingToGroup: (v: { participantId: string; name: string }) => void
  setMenu: (m: { x: number; y: number; items: ContextMenuItem[] } | null) => void
}

export function openConvoContextMenu(c: Conversation, e: React.MouseEvent, ctx: ConvoMenuCtx) {
  const {
    t, byId, togglePin, setMute, otherMember,
    setAddingMembersTo, setConfirmLeave, setCreatingWithMember, setAddingToGroup, setMenu,
  } = ctx
  e.preventDefault()
  e.stopPropagation()
  const items: ContextMenuItem[] = []
  items.push({
    label: c.pinned ? t('convo.unpin') : t('convo.pin'),
    icon: <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M12 17v5"/><path d="M9 10.76V4l-2.5-1.5L5 4l1.5 1.5V10.76A6 6 0 0 0 5 16h14a6 6 0 0 0-1.5-5.24V5.5L19 4l-1.5-1.5L15 4v6.76A6 6 0 0 0 12 11a6 6 0 0 0-3-.24z"/></svg>,
    onSelect: () => togglePin(c),
  })
  // Mute lives right under Pin since both shape "how loud this convo is".
  // When already muted, surface a one-click Unmute at the top of the
  // submenu AND show the active duration as a hint on the parent row.
  const muted = isMuted(c)
  const bellOff = <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M13.73 21a2 2 0 0 1-3.46 0"/><path d="M18.63 13A17.9 17.9 0 0 1 18 8"/><path d="M6.26 6.26A5.86 5.86 0 0 0 6 8c0 7-3 9-3 9h14"/><path d="M18 8a6 6 0 0 0-9.33-5"/><line x1="1" y1="1" x2="23" y2="23"/></svg>
  const muteSubmenu: ContextMenuItem[] = []
  if (muted) {
    muteSubmenu.push({
      label: t('convo.unmute'),
      icon: <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"><path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M13.73 21a2 2 0 0 1-3.46 0"/></svg>,
      onSelect: () => void setMute(c, false, null),
    })
  }
  for (const opt of MUTE_DURATIONS) {
    muteSubmenu.push({
      label: t(opt.label),
      onSelect: () => void setMute(c, true, opt.compute()),
    })
  }
  muteSubmenu.push({
    label: t('convo.muteForever'),
    onSelect: () => void setMute(c, true, null),
  })
  items.push({
    label: muted ? t('convo.muted') : t('convo.mute'),
    icon: bellOff,
    hint: muted ? muteHint(t, c.mutedUntil) : undefined,
    submenu: muteSubmenu,
  })
  if (c.kind === 'group') {
    items.push({
      label: t('convo.addMembers'),
      icon: <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><line x1="19" y1="8" x2="19" y2="14"/><line x1="22" y1="11" x2="16" y2="11"/></svg>,
      onSelect: () => setAddingMembersTo(c),
    })
    items.push({
      label: t('convo.leaveGroup'),
      destructive: true,
      icon: <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" y1="12" x2="9" y2="12"/></svg>,
      onSelect: () => setConfirmLeave(c),
    })
  } else if (c.kind === 'direct') {
    // Direct chats: actions for the *other* participant.
    const otherId = otherMember(c)
    const other = otherId ? byId[otherId] : undefined
    if (other) {
      items.push({
        label: t('convo.createGroupWith', { name: other.name }),
        icon: <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><circle cx="9" cy="7" r="4"/><path d="M3 21v-2a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v2"/><line x1="19" y1="8" x2="19" y2="14"/><line x1="22" y1="11" x2="16" y2="11"/></svg>,
        onSelect: () => setCreatingWithMember(other.id),
      })
      items.push({
        label: t('convo.addToAGroup', { name: other.name }),
        icon: <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><line x1="19" y1="8" x2="19" y2="14"/><line x1="22" y1="11" x2="16" y2="11"/></svg>,
        onSelect: () => setAddingToGroup({ participantId: other.id, name: other.name }),
      })
    }
  }
  if (items.length === 0) return
  setMenu({ x: e.clientX, y: e.clientY, items })
}
