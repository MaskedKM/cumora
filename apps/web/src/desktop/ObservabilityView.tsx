// 子组件分桶(#219 ①):本文件保留 ObservabilityView 壳(面板状态/取数/
// 双栏布局),20+ 个局部子组件按职责分居 ./observability/:
//   shared(面板键/状态样式/时间与字节格式化)· RefreshButton · runs
//   (StatusPill/AgentDot/RunRow/EventRow)· eventDetails(事件详情族)
//   · fileTree(workspace 文件树+查看器)· triagePanel(triage 经济账)。
// format 层与轮询数据层仍由 #147 ② 的 @/lib/format 与 usePollingRefresh 承担。
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  type ApiAgentEvent,
  ApiTranscriptEntry,
  type ApiAgentRun,
  type ApiAgentRunStatus,
  type ApiAgentWorkspaceFile,
  type ApiAgentWorkspaceFileContent,
  type ApiTriageEconomics,
  api,
} from '@/api/client'
import { Checkbox } from '@/components/Checkbox'
import { ResizeHandle } from '@/components/ResizeHandle'
import { Select } from '@/components/Select'
import { useT } from '@/lib/i18n'
import { usePollingRefresh } from '@/lib/usePollingRefresh'
import { useResizableWidth } from '@/lib/useResizableWidth'
import { cn } from '@/lib/utils'
import { useParticipants } from '@/stores/participants'
import { allFolderPaths, buildFileTree, FileTree, FileViewer } from './observability/fileTree'
import { RefreshButton } from './observability/RefreshButton'
import { AgentDot, EventRow, RunRow, StatusPill } from './observability/runs'
import {
  bytes,
  clock,
  DEV_PANELS,
  type DevPanel,
  elapsed,
  PANEL_LABEL_KEY,
  relative,
  STATUS_STYLE,
} from './observability/shared'
import { TriageEconomicsPanel } from './observability/triagePanel'

type StatusFilter = ApiAgentRunStatus | 'all'

const STATUS_OPTIONS: StatusFilter[] = ['all', 'running', 'stalled', 'failed', 'completed', 'skipped']

export function ObservabilityView() {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const loaded = useParticipants((s) => s.loaded)
  const { width: sidebarWidth, onResizeStart } = useResizableWidth('sidebar:observability', 380, { min: 280, max: 600 })
  const [panel, setPanel] = useState<DevPanel>('traces')
  const [runs, setRuns] = useState<ApiAgentRun[]>([])
  const [events, setEvents] = useState<ApiAgentEvent[]>([])
  const [transcript, setTranscript] = useState<ApiTranscriptEntry[]>([])
  const [detailTab, setDetailTab] = useState<'events' | 'transcript'>('events')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [agentId, setAgentId] = useState<string>('all')
  const [status, setStatus] = useState<StatusFilter>('all')
  const [autoRefresh, setAutoRefresh] = useState(true)
  const [loading, setLoading] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [workspaceAgentId, setWorkspaceAgentId] = useState<string>('')
  const [workspaceFiles, setWorkspaceFiles] = useState<ApiAgentWorkspaceFile[]>([])
  const [selectedPath, setSelectedPath] = useState<string | null>(null)
  const [workspaceFile, setWorkspaceFile] = useState<ApiAgentWorkspaceFileContent | null>(null)
  const [workspaceLoading, setWorkspaceLoading] = useState(false)
  const [workspaceErr, setWorkspaceErr] = useState<string | null>(null)
  const [triage, setTriage] = useState<ApiTriageEconomics | null>(null)
  const [triageHours, setTriageHours] = useState(24)
  const [triageLoading, setTriageLoading] = useState(false)
  const [triageErr, setTriageErr] = useState<string | null>(null)
  // Tree state: which folder paths are expanded. New file lists default to
  // every folder open (most agents have <20 files, flat-feeling is right).
  const [expandedFolders, setExpandedFolders] = useState<Set<string>>(new Set())
  const fileTree = useMemo(() => buildFileTree(workspaceFiles), [workspaceFiles])
  useEffect(() => {
    setExpandedFolders(new Set(allFolderPaths(fileTree)))
  }, [fileTree])
  const toggleFolder = useCallback((p: string) => {
    setExpandedFolders((prev) => {
      const next = new Set(prev)
      if (next.has(p)) next.delete(p); else next.add(p)
      return next
    })
  }, [])

  const agents = useMemo(
    () => Object.values(byId).filter((p) => p.kind === 'agent' && !p.departedAt).sort((a, b) => a.name.localeCompare(b.name)),
    [byId],
  )

  useEffect(() => {
    if (!loaded) void useParticipants.getState().load()
  }, [loaded])

  useEffect(() => {
    if (workspaceAgentId || agents.length === 0) return
    setWorkspaceAgentId(agents[0].id)
  }, [agents, workspaceAgentId])

  const loadRuns = useCallback(async () => {
    setLoading(true)
    setErr(null)
    try {
      const data = await api.getAgentRuns({
        agentId: agentId === 'all' ? null : agentId,
        status,
        limit: 80,
      })
      setRuns(data)
      setSelectedId((cur) => cur && data.some((run) => run.id === cur) ? cur : data[0]?.id ?? null)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [agentId, status])

  const loadEvents = useCallback(async (runId: string | null) => {
    if (!runId) {
      setEvents([])
      return
    }
    try {
      setEvents(await api.getAgentRunEvents(runId))
      // 转录是终态档案(不像 events 随轮刷新)——拉一次全量,失败不阻塞主面。
      api.getAgentRunTranscript(runId).then(setTranscript).catch(() => setTranscript([]))
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    }
  }, [])

  const loadWorkspaceFiles = useCallback(async () => {
    if (!workspaceAgentId) return
    setWorkspaceLoading(true)
    setWorkspaceErr(null)
    try {
      const files = await api.listAgentWorkspace(workspaceAgentId)
      setWorkspaceFiles(files)
      setSelectedPath((cur) => cur && files.some((file) => file.path === cur) ? cur : files[0]?.path ?? null)
    } catch (e) {
      setWorkspaceErr(e instanceof Error ? e.message : String(e))
    } finally {
      setWorkspaceLoading(false)
    }
  }, [workspaceAgentId])

  useEffect(() => {
    void loadRuns()
  }, [loadRuns])

  useEffect(() => {
    void loadEvents(selectedId)
  }, [loadEvents, selectedId])

  // 15s(#144a): each tick re-fetches runs (up to 80) + the selected
  // run's events — 3s was ~20 req/min against the server for a panel
  // that rarely changes that fast. Manual refresh stays instant.
  // 共享 hook(#147 ②):后台 tab 暂停语义原样保留。
  usePollingRefresh(
    useCallback(() => {
      void loadRuns()
      void loadEvents(selectedId)
    }, [loadEvents, loadRuns, selectedId]),
    autoRefresh ? 15_000 : 0,
  )

  useEffect(() => {
    if (panel === 'workspace') void loadWorkspaceFiles()
  }, [loadWorkspaceFiles, panel])

  const loadTriage = useCallback(async () => {
    setTriageLoading(true)
    setTriageErr(null)
    try {
      setTriage(await api.getTriageEconomics({ agentId: agentId === 'all' ? null : agentId, sinceHours: triageHours }))
    } catch (e) {
      setTriageErr(e instanceof Error ? e.message : String(e))
    } finally {
      setTriageLoading(false)
    }
  }, [agentId, triageHours])

  useEffect(() => {
    if (panel === 'triage') void loadTriage()
  }, [panel, loadTriage])

  useEffect(() => {
    if (panel !== 'workspace' || !workspaceAgentId || !selectedPath) {
      setWorkspaceFile(null)
      return
    }
    let cancelled = false
    setWorkspaceErr(null)
    void api.readAgentWorkspaceFile(workspaceAgentId, selectedPath)
      .then((file) => {
        if (!cancelled) setWorkspaceFile(file)
      })
      .catch((e) => {
        if (!cancelled) setWorkspaceErr(e instanceof Error ? e.message : String(e))
      })
    return () => { cancelled = true }
  }, [panel, selectedPath, workspaceAgentId])

  const selected = useMemo(
    () => runs.find((run) => run.id === selectedId) ?? null,
    [runs, selectedId],
  )

  if (panel === 'triage') {
    return (
      <TriageEconomicsPanel
        panel={panel}
        setPanel={setPanel}
        agents={agents}
        agentId={agentId}
        setAgentId={setAgentId}
        hours={triageHours}
        setHours={setTriageHours}
        data={triage}
        loading={triageLoading}
        err={triageErr}
        onRefresh={() => void loadTriage()}
      />
    )
  }

  return (
    <main
      className="grid h-full min-h-0 overflow-hidden bg-paper"
      style={{ gridTemplateColumns: `${sidebarWidth}px minmax(0, 1fr)` }}
    >
      <section className="relative flex min-h-0 min-w-0 flex-col border-r border-ink-100">
        <div className="border-b border-ink-100 px-5 py-4">
          <div className="flex items-start justify-between gap-3">
            <div>
              <h1 className="font-display text-[25px] font-medium tracking-tight text-ink-900">{t('obs.title')}</h1>
              <div className="mt-1 text-[12px] text-ink-500">{t('obs.subtitle')}</div>
            </div>
            <RefreshButton
              loading={panel === 'traces' ? loading : workspaceLoading}
              onClick={() => panel === 'traces' ? void loadRuns() : void loadWorkspaceFiles()}
            />
          </div>

          <div className="mt-4 grid grid-cols-3 gap-1 rounded-[11px] border border-ink-100 bg-cloud p-1">
            {DEV_PANELS.map((item) => (
              <button
                key={item}
                onClick={() => setPanel(item)}
                className={cn(
                  'rounded-[8px] px-3 py-2 text-[12px] font-semibold transition',
                  panel === item ? 'bg-sky2-50 text-skype-deep shadow-soft' : 'text-ink-500 hover:text-ink-700',
                )}
              >
                {t(PANEL_LABEL_KEY[item])}
              </button>
            ))}
          </div>

          {panel === 'traces' ? (
            <>
              <div className="mt-4 grid grid-cols-2 gap-2">
                <label className="text-[10.5px] font-bold uppercase tracking-[0.12em] text-ink-400">
                  {t('obs.agent')}
                  <Select
                    value={agentId}
                    onValueChange={setAgentId}
                    options={[
                      { value: 'all', label: t('obs.allAgents') },
                      ...agents.map((agent) => ({ value: agent.id, label: agent.name })),
                    ]}
                    className="mt-1 normal-case tracking-normal"
                  />
                </label>
                <label className="text-[10.5px] font-bold uppercase tracking-[0.12em] text-ink-400">
                  {t('obs.status')}
                  <Select<StatusFilter>
                    value={status}
                    onValueChange={setStatus}
                    options={STATUS_OPTIONS.map((s) => ({
                      value: s,
                      label: s === 'all' ? t('obs.allStatuses') : t(STATUS_STYLE[s].key),
                    }))}
                    className="mt-1 normal-case tracking-normal"
                  />
                </label>
              </div>

              <Checkbox
                checked={autoRefresh}
                onCheckedChange={setAutoRefresh}
                label={t('obs.autoRefresh')}
                description={t('obs.autoRefreshSub')}
                className="mt-3"
              />
            </>
          ) : (
            <label className="mt-4 block text-[10.5px] font-bold uppercase tracking-[0.12em] text-ink-400">
              {t('obs.agentWorkspace')}
              <Select
                value={workspaceAgentId}
                onValueChange={(next) => {
                  setWorkspaceAgentId(next)
                  setSelectedPath(null)
                  setWorkspaceFile(null)
                }}
                options={agents.length > 0
                  ? agents.map((agent) => ({ value: agent.id, label: agent.name }))
                  : [{ value: '', label: t('obs.noAgents'), disabled: true }]}
                disabled={agents.length === 0}
                className="mt-1 normal-case tracking-normal"
              />
            </label>
          )}
        </div>

        {panel === 'traces' && err && (
          <div className="mx-5 mt-4 rounded-[10px] bg-coral-soft px-3 py-2 text-[12px] text-coral-deep">{err}</div>
        )}
        {panel === 'workspace' && workspaceErr && (
          <div className="mx-5 mt-4 rounded-[10px] bg-coral-soft px-3 py-2 text-[12px] text-coral-deep">{workspaceErr}</div>
        )}

        <div className="min-h-0 flex-1 overflow-auto p-3">
          {panel === 'workspace' ? (
            workspaceFiles.length === 0 ? (
              <div className="grid h-full place-items-center px-8 text-center text-[13px] leading-[1.6] text-ink-400">
                {t('obs.workspaceEmpty')}
              </div>
            ) : (
              <FileTree
                nodes={fileTree}
                depth={0}
                expanded={expandedFolders}
                onToggle={toggleFolder}
                selectedPath={selectedPath}
                onSelect={setSelectedPath}
              />
            )
          ) : (
            runs.length === 0 ? (
              <div className="grid h-full place-items-center px-8 text-center text-[13px] leading-[1.6] text-ink-400">
                {t('obs.noTraces')}
              </div>
            ) : (
              <div className="space-y-2">
                {runs.map((run) => (
                  <RunRow key={run.id} run={run} active={run.id === selectedId} onClick={() => setSelectedId(run.id)} />
                ))}
              </div>
            )
          )}
        </div>
        <ResizeHandle onMouseDown={onResizeStart} />
      </section>

      <section className="min-h-0 min-w-0 overflow-hidden">
        {panel === 'workspace' ? (
          <div className="grid h-full min-h-0 grid-rows-[auto_1fr] overflow-hidden">
            <header className="border-b border-ink-100 bg-cloud/80 px-6 py-4 backdrop-blur">
              <div className="flex flex-wrap items-start justify-between gap-4">
                <div className="min-w-0">
                  <h2 className="truncate font-display text-[24px] font-medium text-ink-900">
                    {workspaceFile?.path ?? t('obs.workspaceHeaderFallback')}
                  </h2>
                  <div className="mt-1 flex flex-wrap items-center gap-2 font-mono text-[11px] text-ink-400">
                    <span>{workspaceAgentId || t('obs.noAgentId')}</span>
                    {workspaceFile && (
                      <>
                        <span>/</span>
                        <span>{bytes(workspaceFile.size)}</span>
                        <span>/</span>
                        <span>{workspaceFile.lineCount} {t('obs.lines')}</span>
                        <span>/</span>
                        <span>{t('obs.metaUpdatedAt', { time: relative(workspaceFile.updatedAt, t) })}</span>
                      </>
                    )}
                  </div>
                </div>
                <div className="rounded-[10px] border border-ink-100 bg-paper px-3 py-2">
                  <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300">{t('obs.workspaceFiles')}</div>
                  <div className="mt-1 font-mono text-[13px] text-ink-900">{workspaceFiles.length}</div>
                </div>
              </div>
            </header>
            <div className="min-h-0 overflow-auto px-6 py-5">
              {workspaceFile ? (
                <article
                  className="min-h-full rounded-[14px] border border-ink-100 bg-paper p-6 shadow-soft"
                  style={{ background: 'var(--paper)' }}
                >
                  <FileViewer path={workspaceFile.path} body={workspaceFile.body || ''} />
                </article>
              ) : (
                <div className="grid h-full place-items-center text-[13px] text-ink-400">
                  {t('obs.pickFile')}
                </div>
              )}
            </div>
          </div>
        ) : selected ? (
          <div className="grid h-full min-h-0 grid-rows-[auto_1fr] overflow-hidden">
            <header className="border-b border-ink-100 bg-cloud/80 px-6 py-4 backdrop-blur">
              <div className="flex flex-wrap items-start justify-between gap-4">
                <div className="min-w-0">
                  <div className="flex items-center gap-3">
                    <AgentDot run={selected} />
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-center gap-2">
                        <h2 className="truncate font-display text-[24px] font-medium text-ink-900">{selected.agentName}</h2>
                        <StatusPill status={selected.status} />
                      </div>
                      <div className="mt-1 flex flex-wrap items-center gap-2 font-mono text-[11px] text-ink-400">
                        <span>{selected.id}</span>
                        <span>/</span>
                        <span>{clock(selected.startedAt)}</span>
                        <span>/</span>
                        <span>{selected.stage ?? t('common.idle')}</span>
                      </div>
                    </div>
                  </div>
                  {(selected.error || selected.summary) && (
                    <div className={cn('mt-3 max-w-[900px] rounded-[10px] px-3 py-2 text-[12.5px] leading-[1.55]', selected.error ? 'bg-coral-soft text-coral-deep' : 'bg-sky2-50 text-ink-700')}>
                      {selected.error || selected.summary}
                    </div>
                  )}
                </div>
                <div className="grid min-w-[360px] grid-cols-4 gap-2">
                  <div className="rounded-[10px] border border-ink-100 bg-paper px-3 py-2">
                    <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300">{t('obs.metaDuration')}</div>
                    <div className="mt-1 font-mono text-[13px] text-ink-900">{elapsed(selected.durationMs)}</div>
                  </div>
                  <div className="rounded-[10px] border border-ink-100 bg-paper px-3 py-2">
                    <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300">{t('obs.metaInbox')}</div>
                    <div className="mt-1 font-mono text-[13px] text-ink-900">{selected.inboxCount}</div>
                  </div>
                  <div className="rounded-[10px] border border-ink-100 bg-paper px-3 py-2">
                    <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300">{t('obs.metaTools')}</div>
                    <div className="mt-1 font-mono text-[13px] text-ink-900">{selected.toolCallCount}</div>
                  </div>
                  <div className="rounded-[10px] border border-ink-100 bg-paper px-3 py-2">
                    <div className="text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300">{t('obs.metaTokens')}</div>
                    <div className="mt-1 font-mono text-[13px] text-ink-900">{selected.tokenCount}</div>
                  </div>
                </div>
              </div>
            </header>

            <div className="flex items-center gap-2 px-6 pt-4 text-[12px]">
              {(['events', 'transcript'] as const).map((tab) => (
                <button
                  key={tab}
                  onClick={() => setDetailTab(tab)}
                  className={detailTab === tab ? 'rounded-lg bg-ink-900 px-3 py-1 text-paper' : 'rounded-lg bg-ink-100 px-3 py-1 text-ink-600'}
                >
                  {t(tab === 'events' ? 'obs.tabEvents' : 'obs.tabTranscript')}
                  {tab === 'transcript' ? ` (${transcript.length})` : ''}
                </button>
              ))}
            </div>
            <div className="min-h-0 overflow-auto px-6 py-5">
              {detailTab === 'transcript' ? (
                transcript.length === 0 ? (
                  <div className="grid h-full place-items-center text-[13px] text-ink-400">{t('obs.noTranscript')}</div>
                ) : (
                  <div className='max-w-[1040px] font-mono text-[12px] leading-relaxed'>
                    {transcript.map((e) => (
                      <div key={e.seq} className='mb-1.5 flex gap-2'>
                        <span className='w-10 shrink-0 text-right text-ink-300'>{e.seq}</span>
                        <span className={
                          e.type === 'tool_use' ? 'w-24 shrink-0 text-sky-600' :
                          e.type === 'tool_result' ? 'w-24 shrink-0 text-emerald-600' :
                          e.type === 'thinking' ? 'w-24 shrink-0 text-ink-400' : 'w-24 shrink-0 text-ink-600'
                        }>{e.type}</span>
                        <div className='min-w-0 flex-1 whitespace-pre-wrap break-words text-ink-700'>
                          {e.tool ? <span className='text-ink-500'>[{e.tool}] </span> : null}
                          {(e.content ?? '').slice(0, 2000)}
                          {e.input != null ? (
                            <details className='mt-0.5'>
                              <summary className='cursor-pointer text-ink-400'>input</summary>
                              <pre className='whitespace-pre-wrap break-all text-ink-500'>{JSON.stringify(e.input, null, 2).slice(0, 4000)}</pre>
                            </details>
                          ) : null}
                        </div>
                      </div>
                    ))}
                  </div>
                )
              ) : events.length === 0 ? (
                <div className="grid h-full place-items-center text-[13px] text-ink-400">{t('obs.noEvents')}</div>
              ) : (
                <div className="max-w-[1040px]">
                  {events.map((event) => (
                    <EventRow key={event.id} event={event} />
                  ))}
                </div>
              )}
            </div>
          </div>
        ) : (
          <div className="grid h-full place-items-center text-[13px] text-ink-400">{t('obs.pickRun')}</div>
        )}
      </section>
    </main>
  )
}
