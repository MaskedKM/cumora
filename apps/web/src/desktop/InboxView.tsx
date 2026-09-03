// InboxView —— #264 人侧 Inbox:分级条目列表 + 静音偏好。分级纪律:
// action_required 才有推送与响铃弹条(生成面 + NotificationToasts);这里
// 是全部条目的账本视图,点击条目跳转源面并标已读。
import { useEffect, useState } from 'react'
import { useApp } from '@/stores/app'
import { useInbox } from '@/stores/inbox'
import { useT } from '@/lib/i18n'
import type { ApiInboxItem } from '@/api/client'

const SEV_STYLE: Record<string, string> = {
  action_required: 'bg-coral-soft text-coral-deep',
  attention: 'bg-sky2-50 text-skype-deep',
  info: 'bg-ink/5 text-ink/60',
}

// 动态 key 收敛成映射(t() 的 key 需静态可查)。
const SEV_LABEL: Record<string, 'inbox.sevAction' | 'inbox.sevAttention' | 'inbox.sevInfo'> = {
  action_required: 'inbox.sevAction',
  attention: 'inbox.sevAttention',
  info: 'inbox.sevInfo',
}

export function InboxView() {
  const t = useT()
  const { loaded, items, counts, mutedTypes, load, markRead, markAllRead, setMutes } = useInbox()
  const setView = useApp((s) => s.setView)
  const selectConversation = useApp((s) => s.selectConversation)
  const [mutesOpen, setMutesOpen] = useState(false)

  useEffect(() => { void load() }, [load])

  const typeUniverse = Array.from(new Set(items.map((it) => it.type))).sort()

  const openItem = (it: ApiInboxItem) => {
    if (!it.read) void markRead(it.id)
    if (it.linkKind === 'conversation' && it.linkId) {
      setView('conversations')
      selectConversation(it.linkId)
    } else if (it.linkKind === 'board') {
      setView('boards')
    } else if (it.linkKind === 'calendar') {
      setView('calendar')
    } else if (it.linkKind === 'observability') {
      setView('observability')
    }
  }

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-3xl px-8 py-8">
        <div className="mb-1 flex items-center justify-between">
          <h1 className="text-xl font-semibold">{t('inbox.title')}</h1>
          <div className="flex gap-2">
            <button
              type="button"
              onClick={() => setMutesOpen((v) => !v)}
              className="rounded-lg px-3 py-1.5 text-sm hover:bg-ink/5"
            >
              {t('inbox.mutes')}
            </button>
            <button
              type="button"
              onClick={() => void markAllRead()}
              className="rounded-lg bg-ink px-3 py-1.5 text-sm font-medium text-cloud hover:opacity-90"
            >
              {t('inbox.readAll')}
            </button>
          </div>
        </div>
        <p className="mb-6 text-sm opacity-60">{t('inbox.subtitle')}</p>

        <div className="mb-4 flex gap-3 text-xs">
          <span className={`rounded-full px-2.5 py-1 font-semibold ${SEV_STYLE.action_required}`}>
            {t('inbox.sevAction')} · {counts.actionRequired}
          </span>
          <span className={`rounded-full px-2.5 py-1 font-semibold ${SEV_STYLE.attention}`}>
            {t('inbox.sevAttention')} · {counts.attention}
          </span>
          <span className={`rounded-full px-2.5 py-1 font-semibold ${SEV_STYLE.info}`}>
            {t('inbox.sevInfo')} · {counts.info}
          </span>
        </div>

        {mutesOpen && (
          <div className="mb-4 rounded-xl border p-4">
            <p className="mb-2 text-sm font-medium">{t('inbox.mutesHint')}</p>
            {typeUniverse.length === 0 ? (
              <p className="text-sm opacity-50">{t('inbox.mutesEmpty')}</p>
            ) : (
              <div className="flex flex-wrap gap-2">
                {typeUniverse.map((type) => {
                  const muted = mutedTypes.includes(type)
                  return (
                    <button
                      key={type}
                      type="button"
                      onClick={() => {
                        const next = muted ? mutedTypes.filter((x) => x !== type) : [...mutedTypes, type]
                        void setMutes(next)
                      }}
                      className={`rounded-full px-2.5 py-1 font-mono text-xs ${
                        muted ? 'bg-ink text-cloud' : 'bg-ink/5 text-ink/70'
                      }`}
                    >
                      {type}{muted ? ' 🔇' : ''}
                    </button>
                  )
                })}
              </div>
            )}
          </div>
        )}

        {!loaded ? (
          <div className="py-16 text-center text-sm opacity-50">{t('common.loading')}</div>
        ) : items.length === 0 ? (
          <div className="rounded-xl border border-dashed px-6 py-12 text-center text-sm opacity-60">
            {t('inbox.empty')}
          </div>
        ) : (
          <ul className="space-y-2">
            {items.map((it) => (
              <li key={it.id}>
                <button
                  type="button"
                  onClick={() => openItem(it)}
                  className={`flex w-full items-start justify-between gap-4 rounded-xl border px-4 py-3 text-left hover:bg-ink/[0.03] ${
                    it.read ? 'bg-white/40 opacity-60' : 'bg-white/70'
                  }`}
                >
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className={`rounded px-1.5 py-0.5 text-[11px] font-semibold uppercase tracking-wide ${SEV_STYLE[it.severity] ?? SEV_STYLE.info}`}>
                        {t(SEV_LABEL[it.severity] ?? 'inbox.sevInfo')}
                      </span>
                      <span className="rounded bg-ink/5 px-1.5 py-0.5 font-mono text-[11px] opacity-60">{it.type}</span>
                      {!it.read && <span className="h-2 w-2 rounded-full bg-coral" aria-label="unread" />}
                    </div>
                    <p className="mt-1 truncate text-sm font-medium">{it.title}</p>
                    {it.body && <p className="mt-0.5 line-clamp-2 text-xs opacity-70">{it.body}</p>}
                  </div>
                  <span className="shrink-0 text-[11px] opacity-50">
                    {new Date(it.createdAt).toLocaleString()}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}
