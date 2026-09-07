// HrView —— #345/#346 HR Agent 配置面 + 评估面:编外隐形人事代理的状态卡、
// owner/admin 专属配置(prompt / Computer+Engine 执行指派)、手动评估触发
// (单个/全员)与报告列表。它不在花名册、对其他 agent 不可见不可召唤
// (ADR 0007)。数据走页内局部 state(SkillsView 范式)——单页数据,不入
// 共享 store。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { type ApiHrAgent, type ApiHrChange, type ApiHrEvaluation, type ApiHrRating, api, type HrAgentConfigInput } from '@/api/client'
import { Select } from '@/components/Select'
import { type MessageKey, useT } from '@/lib/i18n'
import { useAuth } from '@/stores/auth'
import { useComputers } from '@/stores/computers'
import { useParticipants } from '@/stores/participants'
import type { EngineId } from '@/types'

function errText(err: unknown): string {
  if (err instanceof Error && err.message) return err.message
  return String(err)
}

// 评估轮状态 → i18n 键(模板串会丢 MessageKey 字面量类型,故显式映射)。
const EVAL_STATUS_KEYS: Record<string, MessageKey> = {
  pending: 'hr.st.pending',
  running: 'hr.st.running',
  done: 'hr.st.done',
  failed: 'hr.st.failed',
}

// 变更字段 → i18n 键(同上,显式映射)。
const CHANGE_FIELD_KEYS: Record<string, MessageKey> = {
  systemPrompt: 'hr.field.systemPrompt',
  bio: 'hr.field.bio',
  role: 'hr.field.role',
}

function engineLabel(t: ReturnType<typeof useT>, en: string): string {
  return en === 'claude' ? t('agent.engineClaude')
    : en === 'codex' ? t('agent.engineCodex')
    : en === 'grok' ? t('agent.engineGrok')
    : en === 'cursor' ? t('agent.engineCursor')
    : en === 'zcode' ? t('agent.engineZcode')
    : en
}

export function HrView() {
  const t = useT()
  const role = useAuth((s) => s.companies.find((c) => c.id === s.activeCompanyId)?.role)
  const canManage = role === 'owner' || role === 'admin'
  const [hr, setHr] = useState<ApiHrAgent | null>(null)
  const [promptDraft, setPromptDraft] = useState('')
  const [computerId, setComputerId] = useState('')
  const [engine, setEngine] = useState<EngineId | ''>('')
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const [savedFlash, setSavedFlash] = useState(false)

  // 评估面(#346):目标选择 + 轮次列表 + 展开详情(payload/输入快照)。
  // v5 selector 纪律:选稳定引用(byId),派生走 useMemo —— selector 里
  // Object.values().filter() 每次产新数组,破快照稳定性即无限重渲。
  const participantsById = useParticipants((s) => s.byId)
  const agents = useMemo(
    () => Object.values(participantsById).filter((p) => p.kind === 'agent'),
    [participantsById],
  )
  const [evalTarget, setEvalTarget] = useState('')
  const [evaluating, setEvaluating] = useState(false)
  const [evalError, setEvalError] = useState('')
  const [evals, setEvals] = useState<ApiHrEvaluation[] | null>(null)
  const [expanded, setExpanded] = useState<Record<string, ApiHrEvaluation>>({})

  const computersById = useComputers((s) => s.byId)
  const computers = Object.values(computersById).sort((a, b) => a.name.localeCompare(b.name))
  const selectedComputer = computerId ? computersById[computerId] : undefined

  useEffect(() => { void useComputers.getState().refresh() }, [])

  const reloadEvals = useCallback(async () => {
    try {
      const { rows } = await api.listHrEvaluations()
      setEvals(rows)
      // 缓存详情只在状态未变时保留;轮次状态翻页(pending→done)即失效,
      // 展开重新拉(评审 P2:expanded 陈旧)。
      setExpanded((prev) => {
        const next: Record<string, ApiHrEvaluation> = {}
        for (const row of rows) {
          const cached = prev[row.id]
          if (cached && cached.status === row.status) next[row.id] = cached
        }
        return next
      })
      setEvalError('')
    } catch (err) {
      setEvalError(errText(err))
    }
  }, [])

  const runEvaluation = async () => {
    if (evaluating) return
    setEvaluating(true)
    try {
      await api.createHrEvaluation(evalTarget ? { targetAgentId: evalTarget } : {})
      await reloadEvals()
      setEvalError('')
    } catch (err) {
      setEvalError(errText(err))
    } finally {
      setEvaluating(false)
    }
  }

  const toggleDetail = async (id: string) => {
    if (expanded[id]) {
      const next = { ...expanded }
      delete next[id]
      setExpanded(next)
      return
    }
    try {
      const detail = await api.getHrEvaluation(id)
      setExpanded((prev) => ({ ...prev, [id]: detail }))
    } catch (err) {
      setEvalError(errText(err))
    }
  }

  // 评分面(#347):owner 主观打分/评语,进入下一轮评估输入
  const [ratingsBy, setRatingsBy] = useState<Record<string, ApiHrRating>>({})
  const [ratingDraft, setRatingDraft] = useState<Record<string, { score: string; comment: string }>>({})
  const [savingRatings, setSavingRatings] = useState<Set<string>>(new Set())

  // 变更历史(#348):岗位层修改台账 + 一键回滚
  const [changes, setChanges] = useState<ApiHrChange[] | null>(null)
  const [reverting, setReverting] = useState<Set<string>>(new Set())
  const reloadChanges = useCallback(async () => {
    try {
      const { rows } = await api.listHrChanges()
      setChanges(rows)
    } catch (err) {
      setEvalError(errText(err))
    }
  }, [])
  useEffect(() => { void reloadChanges() }, [reloadChanges])

  const revertChange = async (id: string) => {
    if (reverting.has(id)) return
    setReverting((prev) => new Set(prev).add(id))
    try {
      await api.revertHrChange(id)
      await reloadChanges()
      setEvalError('')
    } catch (err) {
      setEvalError(errText(err))
    } finally {
      setReverting((prev) => {
        const next = new Set(prev)
        next.delete(id)
        return next
      })
    }
  }

  const reloadRatings = useCallback(async () => {
    try {
      const { rows } = await api.listHrRatings()
      setRatingsBy(Object.fromEntries(rows.map((r) => [r.agentId, r])))
    } catch { /* 评分区静默降级:评估区已有错误面 */ }
  }, [])
  useEffect(() => { void reloadRatings() }, [reloadRatings])

  // 有未终态轮时低频轮询(报告落库后列表自己翻页;无在飞轮不空转)。
  // 变更历史一并轮询 —— 报告可带 jobEdits,在飞轮收口时变更区要跟着落账。
  const hasOpenRound = !!evals?.some((e) => e.status === 'pending' || e.status === 'running')
  useEffect(() => {
    if (!hasOpenRound) return
    const timer = window.setInterval(() => { void reloadEvals(); void reloadChanges() }, 8000)
    return () => window.clearInterval(timer)
  }, [hasOpenRound, reloadEvals, reloadChanges])

  const ratingValue = (agentId: string): { score: string; comment: string } =>
    ratingDraft[agentId] ?? { score: ratingsBy[agentId] ? String(ratingsBy[agentId].score) : '', comment: ratingsBy[agentId]?.comment ?? '' }

  const saveRating = async (agentId: string) => {
    const v = ratingValue(agentId)
    const score = Number(v.score)
    if (!Number.isInteger(score) || score < 1 || score > 5) return
    setSavingRatings((prev) => new Set(prev).add(agentId))
    try {
      const saved = await api.putHrRating(agentId, { score, comment: v.comment })
      setRatingsBy((prev) => ({ ...prev, [agentId]: saved }))
      // 保存期间的继续编辑不吞(评审 P1):仅当 draft 仍等于发出快照才清
      setRatingDraft((prev) => {
        const cur = prev[agentId]
        if (!cur || (cur.score === v.score && cur.comment === v.comment)) {
          const next = { ...prev }
          delete next[agentId]
          return next
        }
        return prev
      })
    } catch (err) {
      setEvalError(errText(err))
    } finally {
      setSavingRatings((prev) => {
        const next = new Set(prev)
        next.delete(agentId)
        return next
      })
    }
  }

  const reload = useCallback(async () => {
    try {
      const row = await api.getHrAgent()
      setHr(row)
      setPromptDraft(row.systemPrompt)
      setComputerId(row.computerId ?? '')
      setEngine((row.engine as EngineId | null) ?? '')
      setError('')
    } catch (err) {
      setError(errText(err))
    }
  }, [])

  useEffect(() => { void reload() }, [reload])
  useEffect(() => { void reloadEvals() }, [reloadEvals])
  useEffect(() => {
    if (!savedFlash) return
    const timer = window.setTimeout(() => setSavedFlash(false), 1600)
    return () => window.clearTimeout(timer)
  }, [savedFlash])

  // 换机时若旧引擎不在新机 advertised 里,回退首项(AgentEditor 同款联动)
  const changeComputer = (id: string): void => {
    setComputerId(id)
    const c = id ? computersById[id] : undefined
    if (!c) { setEngine(''); return }
    setEngine((cur) => c.availableEngines.includes(cur as EngineId)
      ? cur
      : ((c.availableEngines[0] as EngineId) ?? ''))
  }

  const dirty = !!hr && (
    promptDraft.trim() !== hr.systemPrompt.trim()
    || computerId !== (hr.computerId ?? '')
    || engine !== ((hr.engine as EngineId | null) ?? '')
  )

  const save = async () => {
    if (!hr || !dirty || saving) return
    setSaving(true)
    try {
      const input: HrAgentConfigInput = {}
      if (promptDraft.trim() !== hr.systemPrompt.trim()) input.systemPrompt = promptDraft
      if (computerId !== (hr.computerId ?? '')) {
        // 空串 = 清空指派(computer+engine 一并清);指派则带上解析引擎
        input.computerId = computerId
        if (computerId) input.engine = engine || undefined
      } else if (engine !== ((hr.engine as EngineId | null) ?? '')) {
        input.engine = engine
      }
      const row = await api.putHrAgentConfig(input)
      setHr(row)
      setPromptDraft(row.systemPrompt)
      setComputerId(row.computerId ?? '')
      setEngine((row.engine as EngineId | null) ?? '')
      setError('')
      setSavedFlash(true)
    } catch (err) {
      setError(errText(err))
    } finally {
      setSaving(false)
    }
  }

  if (!canManage) {
    return (
      <div className="h-full overflow-y-auto">
        <div className="mx-auto max-w-3xl px-8 py-16 text-center text-sm opacity-60">{t('hr.denied')}</div>
      </div>
    )
  }

  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto max-w-3xl px-8 py-8">
        <h1 className="text-xl font-semibold">{t('hr.title')}</h1>
        <p className="mb-6 mt-1 text-sm opacity-60">{t('hr.subtitle')}</p>

        {error && <div className="mb-4 rounded-lg bg-red-50 px-4 py-2 text-sm text-red-700">{error}</div>}

        {hr === null ? (
          <div className="py-16 text-center text-sm opacity-50">{t('common.loading')}</div>
        ) : (
          <>
            {/* 状态卡:在位 + 执行指派 + 观测归因键 */}
            <div
              className="mb-6 rounded-[14px] p-4"
              style={{ background: 'var(--sky-50)', border: '1px solid var(--sky-100)' }}
            >
              <div className="mb-2 flex items-center gap-2 text-[13px] font-semibold">
                <span className="inline-block h-2 w-2 rounded-full" style={{ background: '#3BB273' }} />
                {t('hr.present')}
              </div>
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-[12.5px]">
                <dt className="opacity-55">{t('hr.attribution')}</dt>
                <dd className="font-mono break-all">{hr.agentId}</dd>
                <dt className="opacity-55">{t('hr.computerLabel')}</dt>
                <dd>{hr.computerId
                  ? (computersById[hr.computerId]
                      ? `${computersById[hr.computerId].name} · ${engineLabel(t, hr.engine ?? '')}`
                      : t('hr.computerRemoved'))
                  : t('hr.unassigned')}</dd>
                <dt className="opacity-55">{t('hr.updatedAt')}</dt>
                <dd>{new Date(hr.updatedAt).toLocaleString()}</dd>
              </dl>
              <p className="mt-2 text-[11.5px] opacity-50">{t('hr.attributionHint')}</p>
            </div>

            {/* 评估面(#346):手动触发(单个/全员)+ 轮次列表 */}
            <div className="mb-6">
              <label className="mb-1.5 block text-[13px] font-semibold">{t('hr.evalLabel')}</label>
              <p className="mb-2 text-[12px] opacity-55">{t('hr.evalHint')}</p>
              <div className="mb-3 flex max-w-md items-center gap-2">
                <div className="flex-1">
                  <Select
                    ariaLabel={t('hr.evalLabel')}
                    value={evalTarget}
                    onValueChange={setEvalTarget}
                    options={[
                      { value: '', label: t('hr.evalAllTargets') },
                      ...agents.map((a) => ({ value: a.id, label: a.name })),
                    ]}
                  />
                </div>
                <button
                  type="button"
                  disabled={evaluating}
                  onClick={() => { void runEvaluation() }}
                  className="rounded-lg bg-ink px-3.5 py-2 text-sm font-medium text-cloud transition hover:opacity-90 disabled:opacity-40"
                >
                  {evaluating ? t('common.loading') : t('hr.evalRun')}
                </button>
              </div>
              {evalError && <div className="mb-3 rounded-lg bg-red-50 px-3 py-2 text-[12.5px] text-red-700">{evalError}</div>}

              <h2 className="mb-2 text-[13px] font-semibold">{t('hr.evalsTitle')}</h2>
              {evals === null ? (
                <div className="py-6 text-center text-[12.5px] opacity-50">{t('common.loading')}</div>
              ) : evals.length === 0 ? (
                <p className="text-[12.5px] opacity-50">{t('hr.evalsEmpty')}</p>
              ) : (
                <ul className="space-y-1.5">
                  {evals.map((ev) => (
                    <li key={ev.id} className="rounded-[10px] border border-ink-100 bg-white">
                      <button
                        type="button"
                        onClick={() => { void toggleDetail(ev.id) }}
                        className="flex w-full items-center gap-3 px-3 py-2 text-left text-[12.5px]"
                      >
                        <span
                          className="inline-block rounded-full px-2 py-0.5 text-[10.5px] font-bold uppercase"
                          style={{
                            background: ev.status === 'done' ? '#E6F6EE'
                              : ev.status === 'failed' ? '#FDECEC'
                              : 'var(--sky-50)',
                            color: ev.status === 'done' ? '#2E7D5B'
                              : ev.status === 'failed' ? '#C0392B'
                              : 'var(--skype-deep)',
                          }}
                        >{t(EVAL_STATUS_KEYS[ev.status] ?? 'hr.st.pending')}</span>
                        <span className="opacity-70">
                          {ev.targetAgentId
                            ? `${t('hr.targetLabel')}: ${participantsById[ev.targetAgentId]?.name ?? ev.targetAgentId}`
                            : t('hr.evalAllTargets')}
                        </span>
                        <span className="ml-auto opacity-45">{new Date(ev.createdAt).toLocaleString()}</span>
                      </button>
                      {expanded[ev.id] && (
                        <div className="border-t border-ink-100 px-3 py-2">
                          {ev.error && <div className="mb-2 text-[12px] text-red-700">{ev.error}</div>}
                          <details className="mb-1.5">
                            <summary className="cursor-pointer text-[12px] font-semibold opacity-70">{t('hr.detailPayload')}</summary>
                            <pre className="mt-1 max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-[8px] bg-ink-50 p-2 font-mono text-[11px]">
                              {JSON.stringify(expanded[ev.id].payload, null, 2)}
                            </pre>
                          </details>
                          <details>
                            <summary className="cursor-pointer text-[12px] font-semibold opacity-70">{t('hr.detailInputs')}</summary>
                            <pre className="mt-1 max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-[8px] bg-ink-50 p-2 font-mono text-[11px]">
                              {JSON.stringify(expanded[ev.id].inputSnapshot, null, 2)}
                            </pre>
                          </details>
                        </div>
                      )}
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {/* 评分面(#347):四路输入的主观校准信号,进入下一轮评估装配 */}
            <div className="mb-6">
              <label className="mb-1.5 block text-[13px] font-semibold">{t('hr.ratingsTitle')}</label>
              <p className="mb-2 text-[12px] opacity-55">{t('hr.ratingsHint')}</p>
              <ul className="space-y-1.5">
                {agents.map((a) => {
                  const v = ratingValue(a.id)
                  const saved = ratingsBy[a.id]
                  const dirty = (v.score || '') !== (saved ? String(saved.score) : '') || v.comment !== (saved?.comment ?? '')
                  return (
                    <li key={a.id} className="flex items-center gap-2 rounded-[10px] border border-ink-100 bg-white px-3 py-1.5 text-[12.5px]">
                      <span className="min-w-24 truncate">{a.name}</span>
                      <div className="w-20">
                        <Select
                          ariaLabel={`${t('hr.ratingsTitle')} ${a.name}`}
                          value={v.score}
                          onValueChange={(score) => setRatingDraft((prev) => ({ ...prev, [a.id]: { ...ratingValue(a.id), score } }))}
                          options={[
                            // 已有存档时不给"未评"项(无删除操作,选了会悬死在
                            // 不可保存态——评审 P2)
                            ...(saved ? [] : [{ value: '', label: t('hr.ratingUnrated') }]),
                            ...[1, 2, 3, 4, 5].map((n) => ({ value: String(n), label: String(n) })),
                          ]}
                        />
                      </div>
                      <input
                        type="text"
                        value={v.comment}
                        placeholder={t('hr.ratingCommentPlaceholder')}
                        onChange={(e) => setRatingDraft((prev) => ({ ...prev, [a.id]: { ...ratingValue(a.id), comment: e.target.value } }))}
                        className="min-w-0 flex-1 rounded-[8px] border border-ink-100 px-2 py-1 text-[12px] outline-none focus:border-skype"
                      />
                      <button
                        type="button"
                        disabled={!dirty || !v.score || savingRatings.has(a.id)}
                        onClick={() => { void saveRating(a.id) }}
                        className="rounded-lg bg-ink px-3 py-1 text-[11.5px] font-medium text-cloud transition hover:opacity-90 disabled:opacity-30"
                      >
                        {savingRatings.has(a.id) ? '…' : t('hr.ratingSave')}
                      </button>
                    </li>
                  )
                })}
                {agents.length === 0 && <li className="text-[12.5px] opacity-50">{t('hr.ratingsEmpty')}</li>}
              </ul>
            </div>

            {/* 变更历史(#348):岗位层修改台账 + 一键回滚 */}
            <div className="mb-6">
              <label className="mb-1.5 block text-[13px] font-semibold">{t('hr.changesTitle')}</label>
              <p className="mb-2 text-[12px] opacity-55">{t('hr.changesHint')}</p>
              {changes === null ? (
                <div className="py-4 text-center text-[12.5px] opacity-50">{t('common.loading')}</div>
              ) : changes.length === 0 ? (
                <p className="text-[12.5px] opacity-50">{t('hr.changesEmpty')}</p>
              ) : (
                <ul className="space-y-1.5">
                  {changes.map((ch) => (
                    <li key={ch.id} className="flex items-center gap-2 rounded-[10px] border border-ink-100 bg-white px-3 py-1.5 text-[12.5px]">
                      <span className="min-w-20 truncate">{participantsById[ch.agentId]?.name ?? ch.agentId}</span>
                      <span className="rounded-full bg-sky-50 px-2 py-0.5 text-[10.5px] font-semibold" style={{ color: 'var(--skype-deep)' }}>
                        {t(CHANGE_FIELD_KEYS[ch.field] ?? 'hr.field.systemPrompt')}
                      </span>
                      <span className="min-w-0 flex-1 truncate opacity-75" title={`${ch.oldValue} → ${ch.newValue}`}>
                        {(ch.oldValue || '∅').slice(0, 40)} → {(ch.newValue || '∅').slice(0, 60)}
                      </span>
                      <span className="shrink-0 opacity-45">{new Date(ch.createdAt).toLocaleDateString()}</span>
                      <button
                        type="button"
                        disabled={reverting.has(ch.id)}
                        onClick={() => { void revertChange(ch.id) }}
                        className="shrink-0 rounded-lg border border-ink-200 px-2.5 py-1 text-[11px] font-medium transition hover:bg-cloud disabled:opacity-30"
                      >
                        {reverting.has(ch.id) ? '…' : t('hr.changesRevert')}
                      </button>
                    </li>
                  ))}
                </ul>
              )}
            </div>

            {/* prompt:HR 的评判标准,仅 owner/admin 可改 */}
            <label className="mb-1.5 block text-[13px] font-semibold" htmlFor="hr-prompt">
              {t('hr.promptLabel')}
            </label>
            <p className="mb-2 text-[12px] opacity-55">{t('hr.promptHint')}</p>
            <textarea
              id="hr-prompt"
              value={promptDraft}
              onChange={(e) => setPromptDraft(e.target.value)}
              spellCheck={false}
              className="mb-5 h-64 w-full resize-none rounded-[10px] border border-ink-100 bg-white p-3 font-mono text-[12px] leading-relaxed outline-none focus:border-skype"
            />

            {/* 执行指派:与 AgentEditor 同款 Computer/Engine 联动,外加"未指派"项 */}
            <label className="mb-1.5 block text-[13px] font-semibold">{t('agent.runsOnLabel')}</label>
            <p className="mb-2 text-[12px] opacity-55">{t('hr.runsOnHint')}</p>
            <div className="mb-5 max-w-sm">
              <Select
                ariaLabel={t('agent.runsOnLabel')}
                value={computerId}
                onValueChange={changeComputer}
                options={[
                  { value: '', label: t('hr.unassigned') },
                  // 陈旧指派(机器已吊销/消失)保持选中值可见,不静默漂到"未指派"
                  ...(computerId && !computersById[computerId]
                    ? [{ value: computerId, label: `${computerId} ${t('hr.computerRemoved')}` }]
                    : []),
                  ...computers.map((c) => ({
                    value: c.id,
                    label: `${c.kind === 'vps' ? '🖥' : '💻'} ${c.name}`
                      + (c.status !== 'online' ? ` ${t('agent.offlineSuffix')}` : ''),
                  })),
                ]}
              />
              {selectedComputer && (
                <div className="mt-2">
                  <Select
                    ariaLabel={t('agent.engineLabel')}
                    value={engine as string}
                    onValueChange={(v) => setEngine(v as EngineId)}
                    options={(selectedComputer.availableEngines.length
                      ? selectedComputer.availableEngines
                      : (['claude'] as EngineId[])
                    ).map((en) => ({ value: en, label: engineLabel(t, en) }))}
                  />
                </div>
              )}
            </div>

            <div className="flex items-center gap-3">
              <button
                type="button"
                disabled={!dirty || saving}
                onClick={() => { void save() }}
                className="rounded-lg bg-ink px-4 py-2 text-sm font-medium text-cloud transition hover:opacity-90 disabled:opacity-40"
              >
                {saving ? t('common.loading') : t('hr.save')}
              </button>
              {savedFlash && !dirty && <span className="text-[12.5px] text-[#3BB273]">{t('hr.saved')}</span>}
            </div>
          </>
        )}
      </div>
    </div>
  )
}
