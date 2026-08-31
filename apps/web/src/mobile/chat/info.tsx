// 会话详情页(#219 ⑤)—— 从 MobileChat.tsx 原样搬移:
// MobileChatInfo(群/单聊信息页:hero、改名/改 topic、召集/静音、成员
// 列表+搜索、tools/bio、退群)+ Stat(统计小格)。壳经
// `export { MobileChatInfo } from './chat/info'` 再导出,
// 消费方 MobileApp 的 './MobileChat' 导入面不变。
import { useState } from 'react'
import { api } from '@/api/client'
import { Avatar, AvatarStack } from '@/components/Avatar'
import { IBack, ISearch } from '@/components/icons'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useMe } from '@/stores/auth'
import { isMuted, useConversations } from '@/stores/conversations'
import { useParticipants } from '@/stores/participants'
import type { Participant } from '@/types'

export function MobileChatInfo() {
  const t = useT()
  const pushStack = useApp((s) => s.pushMobileStack)
  const setView = useApp((s) => s.setView)
  const convoId = useApp((s) => s.selectedConversationId)
  const c = useConversations((s) => s.list.find((x) => x.id === convoId))
  const byId = useParticipants((s) => s.byId)
  const meId = useMe()
  const openAgentInfo = useApp((s) => s.openAgentInfo)
  const [busy, setBusy] = useState(false)
  // Inline editors for the group's name + topic (groups only — a DM title is
  // derived from the other person and isn't user-editable). Plus a member
  // search filter. All declared before the early returns so the hook order
  // stays stable across renders.
  const [editingTitle, setEditingTitle] = useState(false)
  const [titleDraft, setTitleDraft] = useState('')
  const [editingTopic, setEditingTopic] = useState(false)
  const [topicDraft, setTopicDraft] = useState('')
  const [memberQuery, setMemberQuery] = useState('')
  if (!c) return null

  // For DM / whisper, the page is fundamentally about the OTHER person —
  // show their hero. For a GROUP, there is no single "focus" member, so
  // we render a group-scoped hero (avatar stack + title + counts) and
  // skip the per-agent tools/bio sections. Picking some arbitrary "first
  // agent" and giant-profile-ing them at the top alongside the member
  // list below is what made the group page look like a mash-up.
  const isGroup = c.kind === 'group'
  const focusId = isGroup ? null : (c.members.find((m) => m !== meId) ?? c.members[0])
  const focus = focusId ? byId[focusId] : undefined
  if (!isGroup && !focus) return null

  const muted = isMuted(c)
  const memberPs = c.members.map((m) => byId[m]).filter((p): p is Participant => Boolean(p))
  const agentCount = memberPs.filter((p) => p.kind === 'agent').length
  const humanCount = memberPs.filter((p) => p.kind === 'human').length
  const groupAgents = memberPs.filter((p) => p.kind === 'agent')
  const statusTone = focus?.status ?? 'avail'
  // Localized status pill — same five tones as the desktop ChatPane, just
  // looked up through the i18n layer here so the mobile info card renders
  // the same Chinese/English as everywhere else.
  const statusLabel: Record<string, string> = {
    avail: t('info.statusAvail'),
    working: t('info.statusWorking'),
    thinking: t('info.statusThinking'),
    waiting: t('info.statusWaiting'),
    resting: t('info.statusResting'),
  }

  const startConvene = async () => {
    if (!convoId || busy) return
    setBusy(true)
    try {
      await api.startConvene(convoId, c.title || 'live work session')
      setView('convene')
    } catch (err) { console.warn('start convene failed', err) }
    setBusy(false)
  }

  const onToggleMute = async () => {
    if (!convoId) return
    try {
      await api.setMute(convoId, !muted)
      await useConversations.getState().reload()
    } catch (err) { console.warn('mute toggle failed', err) }
  }

  const onLeave = async () => {
    if (!convoId) return
    if (!confirm(t('mobchat.confirmLeaveBody'))) return
    try {
      await api.leaveConversation(convoId)
      useApp.getState().selectConversation(null)
      await useConversations.getState().reload()
    } catch (err) { console.warn('leave failed', err) }
  }

  // Group rename + topic edit. Optimistic local write, rolled back on a
  // failed network call — mirrors the desktop ChatPane pattern so both
  // surfaces behave identically.
  const startEditTitle = () => { setTitleDraft(c.title); setEditingTitle(true) }
  const saveTitle = async () => {
    const next = titleDraft.trim()
    setEditingTitle(false)
    if (!convoId || !next || next === c.title) return
    const prev = c.title
    useConversations.setState((s) => ({ list: s.list.map((x) => x.id === convoId ? { ...x, title: next } : x) }))
    try { await api.setTitle(convoId, next) }
    catch (err) {
      console.warn('[title] rename failed', err)
      useConversations.setState((s) => ({ list: s.list.map((x) => x.id === convoId ? { ...x, title: prev } : x) }))
    }
  }
  const startEditTopic = () => { setTopicDraft(c.topic ?? ''); setEditingTopic(true) }
  const saveTopic = async () => {
    const next = topicDraft.trim() || null
    setEditingTopic(false)
    if (!convoId) return
    const prev = c.topic ?? null
    if (next === prev) return
    useConversations.setState((s) => ({ list: s.list.map((x) => x.id === convoId ? { ...x, topic: next } : x) }))
    try { await api.setTopic(convoId, next) }
    catch (err) {
      console.warn('[topic] save failed', err)
      useConversations.setState((s) => ({ list: s.list.map((x) => x.id === convoId ? { ...x, topic: prev } : x) }))
    }
  }

  // Member search filter — matches name OR id, case-insensitive (same rule
  // as the desktop add-members picker).
  const mq = memberQuery.trim().toLowerCase()
  const filteredMembers = mq
    ? memberPs.filter((p) => p.name.toLowerCase().includes(mq) || p.id.toLowerCase().includes(mq))
    : memberPs

  return (
    <section className="flex flex-col h-full bg-paper overflow-y-auto">
      <header
        className="border-b border-ink-100 bg-cloud/95 backdrop-blur-md sticky top-0 z-10"
        style={{ paddingTop: 'env(safe-area-inset-top)' }}
      >
        <div className="px-2 py-2.5 flex items-center gap-2">
          <button
            onClick={() => pushStack('chat')}
            className="w-10 h-10 grid place-items-center text-ink-700 active:bg-sky2-50 rounded-full transition"
            aria-label={t('mpinfo.back')}
          >
            <IBack className="w-[22px] h-[22px]" strokeWidth={2} />
          </button>
          <h1 className="font-display font-medium text-[18px] text-ink-900" style={{ letterSpacing: '-0.01em' }}>{t('mobchat.details')}</h1>
        </div>
      </header>

      {isGroup ? (
        <div
          className="text-center pt-7 pb-5 px-5 border-b border-ink-100"
          style={{ background: 'radial-gradient(circle at 50% 0%, var(--sky-100), transparent 70%)' }}
        >
          <div className="mx-auto mb-3 inline-flex justify-center">
            <AvatarStack ps={groupAgents} size={64} max={5} />
          </div>
          {editingTitle ? (
            <input
              autoFocus
              type="text"
              value={titleDraft}
              onChange={(e) => setTitleDraft(e.target.value)}
              onBlur={saveTitle}
              onKeyDown={(e) => {
                if (e.key === 'Enter') { e.preventDefault(); void saveTitle() }
                if (e.key === 'Escape') setEditingTitle(false)
              }}
              maxLength={80}
              className="block w-full text-center bg-transparent outline-none border-b border-skype-deep font-display font-medium text-[26px] tracking-tight text-ink-900 pb-0.5 mb-1"
            />
          ) : (
            <button
              type="button"
              onClick={startEditTitle}
              className="font-display font-medium text-[26px] tracking-tight mb-1 active:text-skype-deep transition border-b border-dashed border-ink-200 leading-tight max-w-full truncate"
              title={t('mobchat.tapToRename')}
            >{c.title}</button>
          )}
          <div className="font-display italic text-[14px] text-ink-500">
            {memberPs.length} {memberPs.length === 1 ? t('mobchat.memberOne') : t('mobchat.memberMany')}
            {agentCount > 0 && ` · ${agentCount} ${agentCount === 1 ? t('mobchat.agentOne') : t('mobchat.agentMany')}`}
          </div>
          {/* Topic — tap to edit; prompt to add when empty. */}
          {editingTopic ? (
            <input
              autoFocus
              type="text"
              value={topicDraft}
              onChange={(e) => setTopicDraft(e.target.value)}
              onBlur={saveTopic}
              onKeyDown={(e) => {
                if (e.key === 'Enter') { e.preventDefault(); void saveTopic() }
                if (e.key === 'Escape') setEditingTopic(false)
              }}
              placeholder={t('mobchat.topicPlaceholder')}
              maxLength={200}
              className="mt-2 block w-full text-center bg-transparent text-[12.5px] text-ink-700 italic font-display placeholder:text-ink-300 outline-none border-b border-sky2-200 focus:border-skype-deep transition pb-0.5"
            />
          ) : c.topic ? (
            <button
              type="button"
              onClick={startEditTopic}
              className="mt-2 block mx-auto max-w-full truncate text-[12.5px] text-ink-500 italic font-display active:text-skype-deep transition"
              title={t('mobchat.tapToEditTopic')}
            >{c.topic}</button>
          ) : (
            <button
              type="button"
              onClick={startEditTopic}
              className="mt-2 inline-block text-[12.5px] text-ink-300 italic font-display active:text-skype-deep transition"
            >{t('mobchat.addTopic')}</button>
          )}
        </div>
      ) : focus ? (
        <div
          className="text-center pt-7 pb-5 px-5 border-b border-ink-100"
          style={{ background: 'radial-gradient(circle at 50% 0%, var(--sky-100), transparent 70%)' }}
        >
          <div className="mx-auto mb-3 inline-block">
            <Avatar p={focus} size={96} ringColor="var(--paper)" />
          </div>
          <h3 className="font-display font-medium text-[26px] tracking-tight mb-1">{focus.name}</h3>
          {focus.role && (
            <div className="font-display italic text-[14px] text-ink-500 mb-3.5">{focus.role}</div>
          )}
          <div className="inline-flex items-center gap-2 py-2 px-4 rounded-full bg-cloud border border-ink-100 text-[13px] text-ink-700 shadow-soft">
            <span className="w-2 h-2 rounded-full animate-pulse-soft" style={{ background: `var(--${statusTone})` }} />
            {statusLabel[statusTone] ?? statusTone}
          </div>
        </div>
      ) : null}

      <div className="grid grid-cols-2 gap-2 px-4 py-4 border-b border-ink-100">
        <button
          onClick={startConvene}
          disabled={busy}
          className="py-3 px-4 rounded-xl text-white font-semibold text-[13px] active:opacity-80 transition disabled:opacity-50"
          style={{ background: 'var(--skype-ink)' }}
        >
          {busy ? t('mobchat.starting') : t('mobchat.convene')}
        </button>
        <button
          onClick={onToggleMute}
          className="py-3 px-4 rounded-xl bg-cloud border border-ink-100 text-ink-700 font-semibold text-[13px] active:bg-sky2-50 transition"
        >
          {muted ? t('mobchat.unmute') : t('mobchat.mute')}
        </button>
      </div>

      {c.kind !== 'direct' && (
        <div className="py-4 px-5 border-b border-ink-100">
          <h4 className="text-[10.5px] font-bold text-ink-300 tracking-wider uppercase mb-3">{t('mobchat.members')}</h4>
          <div className="grid grid-cols-3 gap-2 mb-3">
            <Stat n={String(memberPs.length)} l={t('mobchat.statMembers')} />
            <Stat n={String(agentCount)} l={t('mobchat.statAgents')} />
            <Stat n={String(humanCount)} l={t('mobchat.statHumans')} />
          </div>
          {/* Search — only worth showing once the list is long enough to
              warrant scanning. Filters by name or @id. */}
          {memberPs.length > 5 && (
            <div className="mb-2.5 flex items-center gap-2 px-2.5 py-1.5 rounded-[10px] bg-paper" style={{ border: '1px solid var(--ink-100)' }}>
              <ISearch className="w-3.5 h-3.5 text-ink-300 shrink-0" strokeWidth={2.4} />
              <input
                value={memberQuery}
                onChange={(e) => setMemberQuery(e.target.value)}
                placeholder={t('mobchat.searchMembersPh')}
                className="flex-1 min-w-0 text-[13px] text-ink-700 bg-transparent outline-none placeholder:text-ink-300"
              />
              {memberQuery && (
                <button
                  type="button"
                  onClick={() => setMemberQuery('')}
                  className="w-6 h-6 -mr-1 grid place-items-center text-ink-400 active:text-ink-600 shrink-0"
                  aria-label={t('mobchat.clearSearch')}
                >×</button>
              )}
            </div>
          )}
          <div className="flex flex-col divide-y divide-ink-100 bg-cloud rounded-[12px]" style={{ border: '1px solid var(--ink-100)' }}>
            {filteredMembers.map((p) => {
              const isSelf = p.id === meId
              return (
                <button
                  key={p.id}
                  type="button"
                  disabled={isSelf}
                  onClick={() => openAgentInfo(p.id)}
                  className={cn(
                    'flex items-center gap-3 py-2.5 px-3 text-left transition first:rounded-t-[12px] last:rounded-b-[12px]',
                    !isSelf && 'active:bg-sky2-50',
                  )}
                >
                  <Avatar p={p} size={32} />
                  <div className="flex-1 min-w-0">
                    <div className="text-[13px] font-semibold text-ink-900 truncate">
                      {p.name}{isSelf && <span className="text-ink-300 font-normal">{t('mobchat.youSuffix')}</span>}
                    </div>
                    <div className="text-[11px] text-ink-500 truncate font-display italic">
                      {p.kind === 'agent' ? (p.role ?? t('common.agent')) : t('mobchat.humanTeammate')}
                    </div>
                  </div>
                  <span className="w-1.5 h-1.5 rounded-full shrink-0" style={{ background: `var(--${p.status ?? 'avail'})` }} />
                  {!isSelf && <IBack className="w-4 h-4 text-ink-200 shrink-0 rotate-180" strokeWidth={2} />}
                </button>
              )
            })}
            {filteredMembers.length === 0 && (
              <div className="py-5 text-center text-[12px] text-ink-400 italic font-display">
                {t('mobchat.noMembersMatch', { query: memberQuery })}
              </div>
            )}
          </div>
        </div>
      )}

      {!isGroup && focus && (focus.tools ?? []).length > 0 && (
        <div className="py-4 px-5 border-b border-ink-100">
          <h4 className="text-[10.5px] font-bold text-ink-300 tracking-wider uppercase mb-3">{t('mobchat.tools')}</h4>
          <div className="grid grid-cols-2 gap-2">
            {(focus.tools ?? []).map((t) => (
              <div key={t} className="py-2.5 px-3 bg-cloud border border-ink-100 rounded-[10px] flex items-center gap-2 text-[12.5px] text-ink-700">
                <span className="w-1.5 h-1.5 rounded-full bg-skype" />
                <b className="font-mono font-medium text-[11.5px] truncate">{t}</b>
              </div>
            ))}
          </div>
        </div>
      )}

      {!isGroup && focus?.bio && (
        <div className="py-4 px-5 border-b border-ink-100">
          <h4 className="text-[10.5px] font-bold text-ink-300 tracking-wider uppercase mb-3">{t('mobchat.about')}</h4>
          <div
            className="py-3 px-3.5 rounded-r-lg font-display italic text-[13px] leading-[1.55] text-ink-700"
            style={{
              background: 'linear-gradient(135deg, var(--sky-50), transparent)',
              borderLeft: '2px solid var(--skype)',
            }}
          >
            {focus.bio}
          </div>
        </div>
      )}

      <div className="py-4 px-5">
        <button
          onClick={onLeave}
          className="w-full py-3 px-4 rounded-[12px] text-[13px] font-semibold text-coral-deep transition text-left active:opacity-70"
          style={{ border: '1px solid rgba(255, 122, 107, 0.3)' }}
        >
          {t('mobchat.leaveConversation')}
          <span className="block font-display italic text-[11px] text-ink-500 mt-0.5">{t('mobchat.agentsContinueWithout')}</span>
        </button>
      </div>
    </section>
  )
}

function Stat({ n, l }: { n: string; l: string }) {
  return (
    <div className="py-3 px-2 bg-cloud border border-ink-100 rounded-[10px] text-center">
      <div className="font-display text-[22px] font-medium text-ink-900 leading-none" style={{ letterSpacing: '-0.02em' }}>{n}</div>
      <div className="text-[10px] font-bold text-ink-500 uppercase tracking-wider mt-1">{l}</div>
    </div>
  )
}
