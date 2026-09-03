// SkillsView —— #261 公司 Skills 库(SOP 手册)管理页:一次沉淀、全员
// 复用。daemon 每同步周期把手册物化到各引擎原生 skills 目录,引擎加载
// 器渐进披露直接可用——这个页面只管"手册内容是什么",不碰分发。
import { useCallback, useEffect, useState } from 'react'
import { type ApiCompanySkill, api } from '@/api/client'
import { IPlus, ITrash } from '@/components/icons'
import { useT } from '@/lib/i18n'
import { useAuth } from '@/stores/auth'

export function SkillsView() {
  const t = useT()
  const role = useAuth((s) => s.companies.find((c) => c.id === s.activeCompanyId)?.role)
  const canManage = role === 'owner' || role === 'admin'
  const [skills, setSkills] = useState<ApiCompanySkill[] | null>(null)
  const [editing, setEditing] = useState<ApiCompanySkill | 'new' | null>(null)
  const [error, setError] = useState('')

  const reload = useCallback(async () => {
    try {
      const { skills: list } = await api.listCompanySkills()
      setSkills(list)
      setError('')
    } catch (err) {
      setError(String(err))
    }
  }, [])

  useEffect(() => { void reload() }, [reload])

  const remove = async (id: string, name: string) => {
    if (!window.confirm(t('skills.deleteConfirm', { name }))) return
    try {
      await api.deleteCompanySkill(id)
      await reload()
    } catch (err) {
      setError(String(err))
    }
  }

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-3xl px-8 py-8">
        <div className="mb-1 flex items-center justify-between">
          <h1 className="text-xl font-semibold">{t('skills.title')}</h1>
          {canManage && (
            <button
              type="button"
              onClick={() => setEditing('new')}
              className="flex items-center gap-1.5 rounded-lg bg-ink text-cloud px-3 py-1.5 text-sm font-medium hover:opacity-90"
            >
              <IPlus className="h-4 w-4" /> {t('skills.create')}
            </button>
          )}
        </div>
        <p className="mb-6 text-sm opacity-60">{t('skills.subtitle')}</p>

        {error && <div className="mb-4 rounded-lg bg-red-50 px-4 py-2 text-sm text-red-700">{error}</div>}

        {skills === null ? (
          <div className="py-16 text-center text-sm opacity-50">{t('common.loading')}</div>
        ) : skills.length === 0 ? (
          <div className="rounded-xl border border-dashed px-6 py-12 text-center text-sm opacity-60">
            {t('skills.empty')}
          </div>
        ) : (
          <ul className="space-y-2">
            {skills.map((sk) => (
              <li
                key={sk.id}
                className="group flex items-start justify-between gap-4 rounded-xl border bg-white/60 px-4 py-3"
              >
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="font-mono text-sm font-semibold">{sk.name}</span>
                    <span className="rounded bg-ink/5 px-1.5 py-0.5 text-[11px] opacity-60">
                      {t('skills.fileCount', { n: sk.fileCount })}
                    </span>
                  </div>
                  <p className="mt-0.5 truncate text-sm opacity-70">{sk.description}</p>
                </div>
                {canManage && (
                  <div className="flex shrink-0 items-center gap-1 opacity-0 transition group-hover:opacity-100">
                    <button
                      type="button"
                      onClick={() => setEditing(sk)}
                      className="rounded-lg px-2.5 py-1 text-sm hover:bg-ink/5"
                    >
                      {t('common.edit')}
                    </button>
                    <button
                      type="button"
                      onClick={() => void remove(sk.id, sk.name)}
                      className="rounded-lg p-1.5 text-red-600 hover:bg-red-50"
                      aria-label={t('common.delete')}
                    >
                      <ITrash className="h-4 w-4" />
                    </button>
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      {editing && (
        <SkillEditor
          skill={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); void reload() }}
        />
      )}
    </div>
  )
}

function SkillEditor({ skill, onClose, onSaved }: {
  skill: ApiCompanySkill | null
  onClose: () => void
  onSaved: () => void
}) {
  const t = useT()
  const [name, setName] = useState(skill?.name ?? '')
  const [description, setDescription] = useState(skill?.description ?? '')
  const [body, setBody] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  // 编辑回填:拉全量取 SKILL.md,剥掉 frontmatter(保存时服务端按
  // name/description 重组装,前后台不双写这份头)。
  useEffect(() => {
    if (!skill) return
    let alive = true
    void api.getCompanySkill(skill.id).then((detail) => {
      if (!alive) return
      const md = detail.files.find((f) => f.path === 'SKILL.md')?.body ?? ''
      setBody(stripSkillFrontmatter(md))
    }).catch((err: unknown) => { if (alive) setError(errText(err)) })
    return () => { alive = false }
  }, [skill])

  useEffect(() => {
    document.body.style.overflow = 'hidden'
    return () => { document.body.style.overflow = '' }
  }, [])

  const save = async () => {
    setBusy(true)
    try {
      if (skill) {
        await api.updateCompanySkill(skill.id, { description, body })
      } else {
        await api.createCompanySkill({ name: name.trim(), description: description.trim(), body })
      }
      onSaved()
    } catch (err) {
      setError(errText(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-ink/30 p-6" onClick={onClose}>
      <div
        className="flex max-h-[85vh] w-full max-w-2xl flex-col rounded-2xl bg-cloud p-6 shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 className="mb-4 text-lg font-semibold">
          {skill ? t('skills.editTitle', { name: skill.name }) : t('skills.createTitle')}
        </h2>

        {!skill && (
          <label className="mb-3 block text-sm font-medium">
            {t('skills.name')}
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="deploy-runbook"
              className="mt-1 w-full rounded-lg border px-3 py-2 font-mono text-sm"
            />
          </label>
        )}
        <label className="mb-3 block text-sm font-medium">
          {t('skills.description')}
          <input
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            className="mt-1 w-full rounded-lg border px-3 py-2 text-sm"
          />
        </label>
        <label className="mb-3 block flex-1 text-sm font-medium">
          {t('skills.body')}
          <span className="mb-1 block font-normal opacity-50">{t('skills.bodyHint')}</span>
          <textarea
            value={body}
            onChange={(e) => setBody(e.target.value)}
            rows={12}
            className="h-full min-h-48 w-full rounded-lg border px-3 py-2 font-mono text-xs"
          />
        </label>

        {error && <div className="mb-3 rounded-lg bg-red-50 px-3 py-2 text-sm text-red-700">{error}</div>}

        <div className="flex justify-end gap-2">
          <button type="button" onClick={onClose} className="rounded-lg px-4 py-2 text-sm hover:bg-ink/5">
            {t('common.cancel')}
          </button>
          <button
            type="button"
            disabled={busy || (!skill && (!name.trim() || !description.trim())) || (!body && !skill)}
            onClick={() => void save()}
            className="rounded-lg bg-ink px-4 py-2 text-sm font-medium text-cloud hover:opacity-90 disabled:opacity-40"
          >
            {t('common.save')}
          </button>
        </div>
      </div>
    </div>
  )
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

// stripSkillFrontmatter:剥 SKILL.md 的 --- 头块(无头块原样返回;
// 头后多余空行压成单个换行)。
function stripSkillFrontmatter(md: string): string {
  if (!md.startsWith('---\n')) return md
  const end = md.indexOf('\n---', 4)
  if (end < 0) return md
  return md.slice(end + 4).replace(/^\n+/, '\n').trimStart()
}
