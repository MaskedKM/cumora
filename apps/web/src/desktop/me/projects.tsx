// 我的视图桶(#219 ④)projects 件 —— 从 MeView.tsx 原样搬移:ProjectsTab(项目
// 列表+新建表单+归档/恢复)。
import { useEffect, useState } from 'react'
import { type ApiProject, api } from '@/api/client'
import { useT } from '@/lib/i18n'
import { Section } from './shared'

export function ProjectsTab() {
  const t = useT()
  const [projects, setProjects] = useState<ApiProject[]>([])
  const [showArchived, setShowArchived] = useState(false)
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  const refresh = () => {
    void api.listProjects().then(setProjects).catch(() => { /* ignore */ })
  }
  useEffect(refresh, [])

  const create = async () => {
    const trimmed = name.trim()
    if (!trimmed) return
    setBusy(true); setErr(null)
    try {
      await api.createProject({ name: trimmed, description: description.trim() })
      setName(''); setDescription(''); setCreating(false); refresh()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  const archive = async (id: string, archive: boolean) => {
    try { await api.archiveProject(id, archive); refresh() }
    catch (e) { console.warn('[projects] archive failed', e) }
  }

  const visible = showArchived ? projects : projects.filter((p) => p.status === 'active')
  const archivedCount = projects.filter((p) => p.status === 'archived').length

  return (
    <div className="space-y-6">
      <Section title={t('me.sectionProjects')}>
        <div className="text-[13px] text-ink-500 leading-[1.55] mb-4 max-w-2xl font-display italic">
          {t('me.projectsIntro')}
        </div>

        <div className="space-y-2">
          {visible.length === 0 && !creating && (
            <div className="bg-cloud rounded-[12px] p-6 text-center text-[13px] text-ink-500 italic font-display"
              style={{ border: '1px dashed var(--ink-100)' }}>
              {t('me.noProjects')}
            </div>
          )}
          {visible.map((p) => {
            const count = p.conversationCount
            return (
              <div key={p.id} className="bg-cloud rounded-[12px] p-4 flex items-center gap-4"
                style={{ border: '1px solid var(--ink-100)', opacity: p.status === 'archived' ? 0.55 : 1 }}>
                <div className="w-3 h-10 rounded-full shrink-0" style={{ background: p.color ?? 'var(--ink-200)' }} />
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <div className="font-semibold text-[14px] text-ink-900 truncate">{p.name}</div>
                    {p.status === 'archived' && <span className="text-[10px] text-ink-300 uppercase tracking-wider">{t('me.archived')}</span>}
                  </div>
                  <div className="font-display italic text-[12px] text-ink-500 truncate">
                    {p.description || t('me.noDescription')}  ·  {count === 1 ? t('me.projectConvoCount', { n: count }) : t('me.projectConvoCountPlural', { n: count })}
                  </div>
                </div>
                <button
                  onClick={() => archive(p.id, p.status !== 'archived')}
                  className="px-3 py-1.5 rounded-[8px] text-[11.5px] font-semibold text-ink-700 bg-paper hover:bg-sky2-50 transition"
                  style={{ border: '1px solid var(--ink-100)' }}
                >{p.status === 'archived' ? t('me.restore') : t('me.archive')}</button>
              </div>
            )
          })}
        </div>

        {creating ? (
          <div className="mt-4 bg-cloud rounded-[12px] p-4 space-y-2"
            style={{ border: '1.5px solid var(--sky2-300)' }}>
            <input
              autoFocus
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t('me.projectNamePh')}
              className="w-full px-3 py-2 text-[13px] rounded outline-none"
              style={{ border: '1px solid var(--ink-100)', background: 'var(--paper)' }}
            />
            <input
              type="text"
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder={t('me.projectDescPh')}
              className="w-full px-3 py-2 text-[12.5px] rounded outline-none"
              style={{ border: '1px solid var(--ink-100)', background: 'var(--paper)' }}
            />
            {err && <div className="text-[11.5px] text-coral-deep">{err}</div>}
            <div className="flex gap-2">
              <button
                onClick={create}
                disabled={!name.trim() || busy}
                className="px-4 py-1.5 rounded-[8px] text-[12px] font-semibold text-white disabled:opacity-50"
                style={{ background: 'var(--skype)' }}
              >{busy ? t('me.createProjectBusy') : t('me.createProject')}</button>
              <button
                onClick={() => { setCreating(false); setName(''); setDescription(''); setErr(null) }}
                className="px-3 py-1.5 rounded-[8px] text-[12px] text-ink-500 hover:bg-cloud"
              >{t('common.cancel')}</button>
            </div>
          </div>
        ) : (
          <div className="mt-4 flex items-center gap-3">
            <button
              onClick={() => setCreating(true)}
              className="px-4 py-2 rounded-[10px] text-[12.5px] font-semibold text-skype-deep bg-cloud hover:bg-sky2-50 transition"
              style={{ border: '1px dashed var(--sky2-300)' }}
            >{t('me.newProject')}</button>
            {archivedCount > 0 && (
              <button
                onClick={() => setShowArchived((v) => !v)}
                className="text-[11.5px] text-ink-500 hover:text-skype-deep transition italic font-display"
              >
                {showArchived ? t('me.hideArchived') : t('me.showArchivedCount', { n: archivedCount })}
              </button>
            )}
          </div>
        )}
      </Section>
    </div>
  )
}
