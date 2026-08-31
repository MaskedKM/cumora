// 会话列表的三个弹窗(#219 ②)—— 从 ConversationsPane.tsx 原样搬移:
// AddToGroupPicker(把成员加进某群)/ ConfirmLeave(退群确认)/
// AddMembersPicker(往群里加多人)。开关状态仍由壳持有。
import { useState } from 'react'
import { api } from '@/api/client'
import { Avatar } from '@/components/Avatar'
import { ISearch } from '@/components/icons'
import { useT } from '@/lib/i18n'
import { useConversations } from '@/stores/conversations'
import type { Conversation, Participant } from '@/types'

export function AddToGroupPicker({ participantId, participantName, groups, onClose }: {
  participantId: string
  participantName: string
  groups: Conversation[]
  onClose: () => void
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [done, setDone] = useState<{ groupTitle: string } | null>(null)

  const pick = async (g: Conversation) => {
    setBusy(true); setErr(null)
    try {
      await api.addMember(g.id, participantId)
      await useConversations.getState().reload()
      setDone({ groupTitle: g.title })
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 grid place-items-center p-6"
      style={{ background: 'rgba(15, 30, 50, 0.55)', backdropFilter: 'blur(6px)' }}
      onClick={onClose}
    >
      <div
        className="bg-cloud rounded-[16px] shadow-pop max-w-[440px] w-full max-h-[80vh] flex flex-col overflow-hidden"
        style={{ border: '1px solid var(--ink-100)' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 py-5 border-b border-ink-100 shrink-0">
          <h3 className="font-display font-medium text-[18px] tracking-tight">{t('convo.addToTitle', { name: participantName })}</h3>
          <div className="text-[12px] text-ink-500 italic font-display mt-0.5">{t('convo.addToGroupPrompt')}</div>
        </div>
        <div className="flex-1 overflow-y-auto px-3 py-2.5 min-h-0">
          {done ? (
            <div className="py-6 text-center">
              <div className="text-[14px] text-ink-900 font-medium">{t('convo.addedTo')} <em className="not-italic text-skype-deep">"{done.groupTitle}"</em>.</div>
              <div className="text-[12px] text-ink-500 italic font-display mt-1">{t('convo.willSeeMessages', { name: participantName })}</div>
            </div>
          ) : groups.length === 0 ? (
            <div className="py-6 text-center text-[12.5px] text-ink-500 italic font-display">
              {t('convo.noGroupsForAdd', { name: participantName })}
            </div>
          ) : (
            <div className="flex flex-col gap-1.5">
              {groups.map((g) => (
                <button
                  key={g.id}
                  onClick={() => pick(g)}
                  disabled={busy}
                  className="text-left flex items-center gap-3 py-2 px-2.5 rounded-[10px] transition disabled:opacity-50"
                  style={{ background: 'var(--paper)', border: '1.5px solid var(--ink-100)' }}
                >
                  <div className="flex-1 min-w-0">
                    <div className="text-[13px] font-semibold text-ink-900 truncate">{g.title}</div>
                    <div className="text-[11px] text-ink-500 truncate">
                      {t(g.members.length === 1 ? 'convo.member' : 'convo.members', { n: g.members.length })}
                    </div>
                  </div>
                  <span className="text-skype-deep text-[12.5px] font-semibold">+ {t('convo.add')}</span>
                </button>
              ))}
            </div>
          )}
          {err && (
            <div className="text-[12.5px] text-coral-deep bg-coral-soft py-2 px-3 rounded-lg mt-3">{err}</div>
          )}
        </div>
        <div className="px-6 py-4 border-t border-ink-100 flex items-center justify-end shrink-0 bg-paper">
          <button
            onClick={onClose}
            className="px-4 py-2 rounded-[9px] text-[12.5px] font-semibold text-ink-700 bg-cloud hover:bg-sky2-50 transition"
            style={{ border: '1px solid var(--ink-100)' }}
          >{done ? t('common.done') : t('common.cancel')}</button>
        </div>
      </div>
    </div>
  )
}

export function ConfirmLeave({ c, onCancel, onLeft }: {
  c: Conversation
  onCancel: () => void
  onLeft: () => void | Promise<void>
}) {
  const t = useT()
  const [busy, setBusy] = useState(false)
  return (
    <div
      className="fixed inset-0 z-50 grid place-items-center p-6"
      style={{ background: 'rgba(15, 30, 50, 0.55)', backdropFilter: 'blur(6px)' }}
      onClick={onCancel}
    >
      <div
        className="bg-cloud rounded-[16px] shadow-pop max-w-[420px] w-full p-6"
        style={{ border: '1px solid var(--ink-100)' }}
        onClick={(e) => e.stopPropagation()}
      >
        <h3 className="font-display font-medium text-[20px] tracking-tight mb-1.5">{t('convo.leaveTitle', { title: c.title })}</h3>
        <p className="text-[13px] text-ink-700 leading-[1.6] mb-4">
          {t('convo.leaveWarning')}
        </p>
        <div className="flex gap-2">
          <button
            onClick={onCancel}
            disabled={busy}
            className="flex-1 py-2 px-3 rounded-[9px] text-[12.5px] font-semibold text-ink-700 bg-cloud hover:bg-sky2-50 transition"
            style={{ border: '1px solid var(--ink-100)' }}
          >{t('common.cancel')}</button>
          <button
            onClick={async () => { setBusy(true); await onLeft() }}
            disabled={busy}
            className="flex-1 py-2 px-3 rounded-[9px] text-[12.5px] font-semibold text-white transition disabled:opacity-50"
            style={{ background: 'var(--coral-deep)' }}
          >{busy ? t('convo.leaving') : t('convo.leaveGroup')}</button>
        </div>
      </div>
    </div>
  )
}

/** Modal: pick one or more participants to add to an existing group.
 *  Multi-add via repeated clicks — each click fires the API call and
 *  the participant drops off the candidate list. Close when done. */
export function AddMembersPicker({ group, candidates, onClose }: {
  group: Conversation
  candidates: Participant[]
  onClose: () => void
}) {
  const t = useT()
  const [busyId, setBusyId] = useState<string | null>(null)
  const [added, setAdded] = useState<Set<string>>(new Set())
  const [err, setErr] = useState<string | null>(null)
  const [query, setQuery] = useState('')

  const remaining = candidates.filter((p) => !added.has(p.id))
  const filtered = query.trim()
    ? remaining.filter((p) =>
        p.name.toLowerCase().includes(query.toLowerCase()) ||
        p.id.toLowerCase().includes(query.toLowerCase()))
    : remaining

  const pick = async (p: Participant) => {
    setBusyId(p.id); setErr(null)
    try {
      await api.addMember(group.id, p.id)
      setAdded((s) => new Set(s).add(p.id))
      await useConversations.getState().reload()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusyId(null)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 grid place-items-center p-6"
      style={{ background: 'rgba(15, 30, 50, 0.55)', backdropFilter: 'blur(6px)' }}
      onClick={onClose}
    >
      <div
        className="bg-cloud rounded-[16px] shadow-pop max-w-[440px] w-full max-h-[80vh] flex flex-col overflow-hidden"
        style={{ border: '1px solid var(--ink-100)' }}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-6 py-5 border-b border-ink-100 shrink-0">
          <h3 className="font-display font-medium text-[18px] tracking-tight">
            {t('convo.addMembersTo')} <span className="text-skype-deep">{group.title}</span>
          </h3>
          <div className="text-[12px] text-ink-500 italic font-display mt-0.5">
            {remaining.length === 0
              ? t('convo.everyoneAlreadyInGroup')
              : t('convo.clickToAdd', { count: added.size })}
          </div>
        </div>
        {remaining.length > 0 && (
          <div className="px-5 pt-3 shrink-0">
            <div className="flex items-center gap-2 px-2.5 py-1.5 rounded-[10px] bg-paper" style={{ border: '1px solid var(--ink-100)' }}>
              <ISearch className="w-3.5 h-3.5 text-ink-300" strokeWidth={2.4} />
              <input
                autoFocus
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder={t('convo.filterPlaceholder')}
                className="flex-1 text-[13px] text-ink-700 bg-transparent outline-none placeholder:text-ink-300"
              />
            </div>
          </div>
        )}
        <div className="flex-1 overflow-y-auto py-2">
          {filtered.length === 0 && query.trim() && (
            <div className="px-6 py-4 text-[12px] italic text-ink-300 font-display">{t('convo.noMatchQuery', { query })}</div>
          )}
          {filtered.map((p) => {
            const busy = busyId === p.id
            return (
              <button
                key={p.id}
                disabled={busy}
                onClick={() => void pick(p)}
                className="w-full text-left flex items-center gap-3 py-2 px-5 hover:bg-sky2-50 transition disabled:opacity-50"
              >
                <Avatar p={p} size={32} ringColor="var(--cloud)" showStatus={false} />
                <div className="flex-1 min-w-0">
                  <div className="text-[13.5px] font-semibold text-ink-900 truncate">{p.name}</div>
                  <div className="text-[11px] text-ink-500 truncate">
                    {p.role ?? (p.kind === 'human' ? t('convo.roleHuman') : t('convo.roleAgent'))}
                  </div>
                </div>
                {busy
                  ? <span className="text-[11px] text-ink-300">{t('convo.adding')}</span>
                  : <span className="text-[11px] text-skype-deep font-semibold">{t('convo.add')}</span>}
              </button>
            )
          })}
          {added.size > 0 && (
            <div className="px-5 pt-3 pb-1 text-[10px] font-bold text-ink-300 tracking-[0.12em] uppercase">
              {t('convo.justAdded')}
            </div>
          )}
          {added.size > 0 && candidates.filter((p) => added.has(p.id)).map((p) => (
            <div key={`done-${p.id}`} className="flex items-center gap-3 py-2 px-5 opacity-60">
              <Avatar p={p} size={28} ringColor="var(--cloud)" showStatus={false} />
              <div className="flex-1 text-[12.5px] text-ink-700 truncate">{p.name}</div>
              <span className="text-[10px] font-semibold text-avail">{t('convo.addedCheck')}</span>
            </div>
          ))}
        </div>
        {err && (
          <div className="px-5 py-2 text-[12px] text-coral-deep bg-coral-soft border-t border-coral-soft shrink-0">
            {err}
          </div>
        )}
        <div className="px-5 py-3 border-t border-ink-100 flex shrink-0">
          <button
            onClick={onClose}
            className="ml-auto py-2 px-4 rounded-[9px] text-[12.5px] font-semibold text-ink-700 bg-cloud hover:bg-sky2-50 transition"
            style={{ border: '1px solid var(--ink-100)' }}
          >{t('common.done')}</button>
        </div>
      </div>
    </div>
  )
}
