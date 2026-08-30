import { useEffect, useRef, useState } from 'react'
import { useConversations } from '@/stores/conversations'
import { useParticipants } from '@/stores/participants'
import { api } from '@/api/client'
import { AvatarStack } from '@/components/Avatar'
import { MembersPopover } from '@/components/MembersPopover'
import { cn } from '@/lib/utils'
import { ISearch, IPin, IConvene } from '@/components/icons'
import type { Participant } from '@/types'
import { useT } from '@/lib/i18n'

/** Soft "Coming soon" popover anchored beneath the trigger. Auto-dismisses
 *  after a beat; also closes on outside-click or Escape. The sparkle
 *  drifts gently so the bubble feels alive rather than static. */


/** Soft "Coming soon" popover anchored beneath the trigger. Auto-dismisses
 *  after a beat; also closes on outside-click or Escape. The sparkle
 *  drifts gently so the bubble feels alive rather than static. */
function ComingSoonPop({ onClose }: { onClose: () => void }) {
  const t = useT()
  useEffect(() => {
    // Defer outside-click + key listeners by one tick so the click that
    // opened the bubble doesn't immediately close it again.
    let armed = false
    const arm = setTimeout(() => { armed = true }, 0)
    const auto = setTimeout(onClose, 3200)
    const onDown = () => { if (armed) onClose() }
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('mousedown', onDown)
    window.addEventListener('keydown', onKey)
    return () => {
      clearTimeout(arm)
      clearTimeout(auto)
      window.removeEventListener('mousedown', onDown)
      window.removeEventListener('keydown', onKey)
    }
  }, [onClose])

  return (
    <div
      role="status"
      aria-live="polite"
      className="absolute right-0 top-full mt-2 z-30 animate-rise"
      onMouseDown={(e) => e.stopPropagation()}
    >
      <div
        className="relative bg-cloud border border-ink-100 rounded-[14px] pl-3 pr-3.5 py-2.5 w-[260px]"
        style={{ boxShadow: '0 18px 38px -18px rgba(10, 30, 60, 0.28), 0 2px 8px -2px rgba(10, 30, 60, 0.06)' }}
      >
        {/* caret poking up out of the bubble's top edge */}
        <div
          aria-hidden
          className="absolute -top-[5px] right-6 w-2.5 h-2.5 bg-cloud rotate-45 border-l border-t border-ink-100"
        />
        <div className="flex items-start gap-2.5">
          <span
            aria-hidden
            className="text-[18px] leading-none mt-px"
            style={{ animation: 'cumora-sparkle-drift 2.4s ease-in-out infinite' }}
          >✨</span>
          <div className="min-w-0">
            <div className="text-[12.5px] font-semibold text-ink-900 leading-tight">{t('chat.comingSoon')}</div>
            <div className="text-[11.5px] text-ink-500 font-display italic leading-snug mt-0.5">
              {t('chat.conveneSoon')}
            </div>
          </div>
        </div>
      </div>
      <style>{`
        @keyframes cumora-sparkle-drift {
          0%, 100% { transform: translateY(0) rotate(0deg); opacity: 0.95; }
          50%      { transform: translateY(-2px) rotate(8deg); opacity: 1; }
        }
      `}</style>
    </div>
  )
}


export function ChatHeader({
  convoId, onConvene, onToggleSearch, searchOpen,
}: {
  convoId: string
  // Kept wired so the underlying Convene flow can be re-enabled by
  // flipping the button's onClick back; for now the button shows a
  // ComingSoonPop instead.
  onConvene: () => void
  onToggleSearch: () => void
  searchOpen: boolean
}) {
  const t = useT()
  void onConvene
  const c = useConversations((s) => s.list.find((x) => x.id === convoId))
  const byId = useParticipants((s) => s.byId)
  const [editingTopic, setEditingTopic] = useState(false)
  const [topicDraft, setTopicDraft] = useState('')
  const [editingTitle, setEditingTitle] = useState(false)
  const [titleDraft, setTitleDraft] = useState('')
  const [membersAnchor, setMembersAnchor] = useState<DOMRect | null>(null)
  const [showConveneSoon, setShowConveneSoon] = useState(false)
  const memberStackRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    setMembersAnchor(null)
  }, [convoId])

  if (!c) return null

  const memberPs = c.members
    .map((m) => byId[m])
    .filter((p): p is Participant => Boolean(p))
  const agentMembers = memberPs.filter((p) => p.kind === 'agent')
  const agentNames = agentMembers.map((p) => p.name).join(', ')
  const humanCount = memberPs.filter((p) => p.kind === 'human').length

  const onClickStack = () => {
    const trigger = memberStackRef.current
    if (!trigger || memberPs.length === 0) return
    setMembersAnchor((cur) => cur ? null : trigger.getBoundingClientRect())
  }

  // Group rename — only group chats; a DM/whisper title is derived from the
  // other person. Mirrors the topic editor (optimistic update + rollback).
  const canRename = c.kind === 'group'
  const startEditTitle = () => {
    if (!canRename) return
    setTitleDraft(c.title)
    setEditingTitle(true)
  }
  const saveTitle = async () => {
    const next = titleDraft.trim()
    setEditingTitle(false)
    if (!next || next === c.title) return
    const prev = c.title
    useConversations.setState((s) => ({
      list: s.list.map((x) => x.id === c.id ? { ...x, title: next } : x),
    }))
    try { await api.setTitle(c.id, next) }
    catch (err) {
      console.warn('[title] rename failed', err)
      useConversations.setState((s) => ({
        list: s.list.map((x) => x.id === c.id ? { ...x, title: prev } : x),
      }))
    }
  }

  const startEditTopic = () => {
    setTopicDraft(c.topic ?? '')
    setEditingTopic(true)
  }
  const saveTopic = async () => {
    const next = topicDraft.trim() || null
    setEditingTopic(false)
    // Optimistic local update — don't wait for the WS push to round-trip
    // before the chip reflects the new value. (Also defensive against any
    // future WS-filter regression that drops the conversation.updated event.)
    useConversations.setState((s) => ({
      list: s.list.map((x) => x.id === c.id ? { ...x, topic: next } : x),
    }))
    try { await api.setTopic(c.id, next) }
    catch (err) {
      console.warn('[topic] save failed', err)
      // Roll back on failure.
      useConversations.setState((s) => ({
        list: s.list.map((x) => x.id === c.id ? { ...x, topic: c.topic ?? null } : x),
      }))
    }
  }

  return (
    <div className="py-2.5 pl-[22px] pr-6 border-b border-ink-100 flex items-center gap-4">
      <div className="flex-1 min-w-0">
        <h2 className="font-display font-medium text-[19px] tracking-tight leading-[1.35] flex items-center gap-2 min-w-0">
          <span className="text-gold text-[14px] shrink-0">★</span>
          {/* Title truncates at narrow widths instead of wrapping to a
              second line — wrapping interacts badly with the topic input
              right beneath it. min-w-0 on the flex parents is what lets
              the text-overflow: ellipsis kick in. `truncate` sets
              overflow:hidden, so leading-[1.35] (≈25.6px on 19px text)
              is the minimum that preserves descenders on "g" / "p" /
              "y" inside the clipped box. */}
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
              className="flex-1 min-w-0 bg-transparent outline-none border-b border-skype-deep font-display font-medium text-[19px] tracking-tight text-ink-900 pb-0.5"
            />
          ) : (
            <span
              className={cn('truncate', canRename && 'cursor-text hover:text-skype-deep transition')}
              title={canRename ? t('chat.renameHint') : c.title}
              onClick={canRename ? startEditTitle : undefined}
            >{c.title}</span>
          )}
          {c.projectName && (
            <span
              className="ml-1 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider rounded shrink-0"
              style={{
                background: c.projectColor ?? 'var(--cloud)',
                color: c.projectColor ? 'white' : 'var(--ink-700)',
                border: c.projectColor ? 'none' : '1px solid var(--ink-100)',
              }}
              title={t('chat.partOfProject', { name: c.projectName })}
            >{c.projectName}</span>
          )}
        </h2>
        <div className="text-[12px] text-ink-500 flex items-center gap-1.5 min-w-0">
          <span className="truncate">{agentNames || '—'}</span>
          {humanCount > 0 && (
            <>
              <span className="w-1 h-1 rounded-full bg-ink-300 shrink-0" />
              <span className="shrink-0">+ {humanCount === 1 ? t('chat.youInHeader') : t('chat.humansCount', { n: humanCount })}</span>
            </>
          )}
          {!c.topic && !editingTopic && (
            <>
              <span className="w-1 h-1 rounded-full bg-ink-300 shrink-0" />
              <button
                onClick={startEditTopic}
                className="text-ink-300 italic font-display hover:text-skype-deep transition shrink-0"
                title={t('chat.setTopic')}
              >{t('chat.addTopic')}</button>
            </>
          )}
        </div>
        {/* Topic — editable on click. Empty state's "+ topic" affordance lives
            inline in the subtitle above, so this row only renders when
            there's actual content or the input is open. */}
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
            placeholder={t('chat.topicPlaceholder')}
            className="mt-0.5 w-full bg-transparent text-[12px] text-ink-700 italic placeholder:text-ink-300 outline-none border-b border-sky2-200 focus:border-skype-deep transition pb-0.5"
            maxLength={200}
          />
        ) : c.topic ? (
          <button
            onClick={startEditTopic}
            // Italic glyphs lean past their box — without right padding,
            // `truncate`'s overflow:hidden chops the slanted edge of the
            // final character. pr-1 + max-w-full keeps the layout
            // honest while leaving room for the slant.
            className="mt-0.5 text-[12px] text-ink-500 italic hover:text-skype-deep transition truncate text-left max-w-full font-display pr-1 block leading-[1.5]"
            title={t('chat.editTopic')}
          >
            {c.topic}
          </button>
        ) : null}
      </div>
      {/* Avatar stack — hidden once the pane gets cramped (sub-lg). Title
          already lists the names below it, so this is a redundant visual
          worth dropping when space is tight. */}
      <button
        ref={memberStackRef}
        onClick={onClickStack}
        aria-haspopup="dialog"
        aria-expanded={membersAnchor ? true : undefined}
        className={cn(
          'rounded-full transition hover:opacity-80 active:scale-95 shrink-0 hidden lg:block focus-visible:outline focus-visible:outline-2 focus-visible:outline-sky2-300',
          membersAnchor && 'opacity-100',
        )}
        title={t('chat.showMembers')}
      >
        <AvatarStack ps={agentMembers} size={28} max={4} />
      </button>
      {membersAnchor && (
        <MembersPopover
          members={memberPs}
          anchor={membersAnchor}
          triggerRef={memberStackRef}
          onClose={() => setMembersAnchor(null)}
        />
      )}
      {/* Action group — never shrinks (`shrink-0`). At narrow widths Search
          + Pin drop off but Convene stays full-text — it's the primary
          action in this header. */}
      <div className="flex gap-1 text-ink-500 shrink-0">
        <button
          onClick={onToggleSearch}
          title={t('chat.search')}
          aria-label={t('chat.search')}
          className={cn(
            'w-9 h-9 rounded-[9px] hidden md:grid place-items-center transition',
            searchOpen ? 'bg-sky2-100 text-skype-deep' : 'hover:bg-sky2-50 hover:text-skype-deep',
          )}
        >
          <ISearch className="w-[19px] h-[19px]" />
        </button>
        <button
          onClick={async () => {
            // Optimistic flip so the icon updates instantly; reload to sync
            // pinned-order in the sidebar. Mirrors the conversations-pane flow.
            const next = !c.pinned
            useConversations.setState((s) => ({
              list: s.list.map((x) => x.id === c.id ? { ...x, pinned: next } : x),
            }))
            try { await api.togglePin(c.id, next); await useConversations.getState().reload() }
            catch (err) {
              console.warn('[pin] toggle failed', err)
              useConversations.setState((s) => ({
                list: s.list.map((x) => x.id === c.id ? { ...x, pinned: !next } : x),
              }))
            }
          }}
          title={c.pinned ? t('chat.unpinFromTop') : t('chat.pinToTop')}
          aria-label={c.pinned ? t('chat.unpinFromTop') : t('chat.pinToTop')}
          className={cn(
            'w-9 h-9 rounded-[9px] hidden md:grid place-items-center transition',
            c.pinned ? 'bg-gold/15 text-gold-deep' : 'hover:bg-sky2-50 hover:text-skype-deep',
          )}
        >
          <IPin className="w-[19px] h-[19px]" />
        </button>
        <div className="relative">
          <button
            onClick={() => setShowConveneSoon((v) => !v)}
            title={t('chat.convene')}
            className="px-3.5 inline-flex items-center gap-1.5 font-semibold text-[12.5px] rounded-full text-white"
            style={{ height: 36, background: 'var(--skype)', boxShadow: '0 4px 12px -3px rgba(0, 168, 240, 0.5)' }}
          >
            <IConvene className="w-4 h-4" />
            <span>{t('chat.convene')}</span>
          </button>
          {showConveneSoon && (
            <ComingSoonPop onClose={() => setShowConveneSoon(false)} />
          )}
        </div>
      </div>
    </div>
  )
}
