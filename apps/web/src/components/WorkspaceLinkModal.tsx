import { useEffect, useState } from 'react'
import { ApiError, api } from '../api/client'
import { useT } from '../lib/i18n'

export type WorkspaceLinkKind = 'project' | 'board_card' | 'document'

/**
 * #338 双向入口的被关联物一侧:把一张卡片 / 一个项目 / 一篇文档挂进
 * 某个团队工作区(服务端语义:关联目标的参与者随之进入该区的 member
 * scope)。权限门在调用方 —— project/board_card 关联服务端要求
 * owner/admin,document 任意成员可建(与 AddWorkspaceAssociation 一致)。
 */
export function WorkspaceLinkModal({ kind, targetId, onClose }: {
  kind: WorkspaceLinkKind
  targetId: string
  onClose: () => void
}) {
  const t = useT()
  const [list, setList] = useState<{ id: string; name: string; isDefault: boolean }[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    api.listWorkspaces()
      .then((rows) => { if (!cancelled) setList(rows) })
      .catch((e) => { if (!cancelled) setError(e instanceof Error ? e.message : String(e)) })
    return () => { cancelled = true }
  }, [])

  const link = async (wsId: string, wsName: string) => {
    setBusy(true)
    setError(null)
    try {
      await api.addWorkspaceAssociation(wsId, kind, targetId)
      setDone(wsName)
    } catch (e) {
      setError(e instanceof ApiError && e.status === 409 ? t('wsLink.already') : e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 grid place-items-center bg-ink-900/30 p-4" onClick={onClose}>
      <div
        className="w-full max-w-sm rounded-xl border border-ink-100 bg-paper p-4 shadow-lg"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="text-[14px] font-semibold text-stone-900">{t('wsLink.title')}</div>
        <div className="mt-1 font-mono text-[11px] text-ink-400" title={targetId}>{targetId}</div>
        {error && <div className="mt-2 rounded-md bg-coral-soft px-2 py-1 text-[12px] text-coral-deep">{error}</div>}
        {done ? (
          <div className="mt-3 text-[12.5px] text-skype-deep">{t('wsLink.done', { name: done })}</div>
        ) : list === null ? (
          <div className="mt-3 text-[12.5px] text-ink-400">{t('common.loading')}</div>
        ) : list.length === 0 ? (
          <div className="mt-3 text-[12.5px] text-ink-400">{t('wsLink.noWorkspaces')}</div>
        ) : (
          <div className="mt-3 flex max-h-56 flex-col gap-1 overflow-y-auto">
            {list.map((w) => (
              <button
                key={w.id}
                type="button"
                disabled={busy}
                onClick={() => void link(w.id, w.name)}
                className="flex items-center gap-2 rounded-lg px-2.5 py-1.5 text-left text-[13px] text-stone-700 hover:bg-stone-50 disabled:opacity-40"
              >
                <span className="truncate">{w.name}</span>
                {w.isDefault && (
                  <span className="ml-auto shrink-0 rounded-full bg-skype/15 px-1.5 py-0.5 text-[10px] font-medium text-skype-deep">
                    {t('ws.default')}
                  </span>
                )}
              </button>
            ))}
          </div>
        )}
        <button
          type="button"
          onClick={onClose}
          className="mt-3 w-full rounded-lg border border-ink-100 px-2 py-1.5 text-[12.5px] text-ink-500 hover:bg-cloud"
        >
          {t('ws.close')}
        </button>
      </div>
    </div>
  )
}
