import { useCallback, useEffect, useMemo, useState } from 'react'
import { isElectron } from '@/lib/runtime'
import type { StackReleaseEntry, StackStatusReport, StackStepResult } from '@/lib/runtime'
import { useMessages } from '@/stores/messages'
import { useT } from '@/lib/i18n'

/**
 * Local Stack console (#286, ADR 0005 §7) — the desktop grows from pure
 * client into "client + console". Everything here drives the bundled
 * cumora-stack binary over IPC (status / releases / restart / rollback);
 * orchestration stays in the CLI so GUI and acceptance paths can't drift.
 *
 * Upgrade = absorb this AppImage's payload + restart, always behind a
 * manual confirm (never silent; downloading a new AppImage touches nothing
 * — autoDownload=false keeps the two steps decoupled).
 */

type OpStep = { status: 'pending' | 'running' | 'ok' | 'fail'; tail?: string }
/** 操作相位:upgrade/rollback = 分段执行中;booting = 等栈回来。 */
type OpPhase = 'upgrade' | 'rollback' | 'booting' | null

function parseJSONStep<T>(res: StackStepResult | null | undefined): T | null {
  if (!res) return null
  try {
    const start = res.stdout.indexOf('{')
    if (start < 0) return null
    return JSON.parse(res.stdout.slice(start)) as T
  } catch {
    return null
  }
}

/** 简版 semver 比较(制品面只有 x.y.z)。 */
function compareVersion(a: string, b: string): number {
  const pa = a.split('.').map((n) => parseInt(n, 10) || 0)
  const pb = b.split('.').map((n) => parseInt(n, 10) || 0)
  for (let i = 0; i < 3; i++) {
    if ((pa[i] ?? 0) !== (pb[i] ?? 0)) return (pa[i] ?? 0) - (pb[i] ?? 0)
  }
  return 0
}

export function StackTab() {
  const t = useT()
  const [status, setStatus] = useState<StackStatusReport | null>(null)
  const [releases, setReleases] = useState<StackReleaseEntry[]>([])
  const [appVersion, setAppVersion] = useState('')
  const [statusErr, setStatusErr] = useState<string | null>(null)
  const [op, setOp] = useState<OpPhase>(null)
  const [opSteps, setOpSteps] = useState<Record<string, OpStep>>({})
  const [confirming, setConfirming] = useState<'upgrade' | 'rollback' | null>(null)
  const [pendingRollback, setPendingRollback] = useState<string | null>(null)
  const [bootingSeconds, setBootingSeconds] = useState(0)

  const streamingCount = useMessages((s) => Object.keys(s.streaming).length)

  const refresh = useCallback(async () => {
    if (!isElectron || !window.cumora?.stack) return
    try {
      const res = await window.cumora.stack.status()
      const rep = parseJSONStep<StackStatusReport>(res)
      if (rep) { setStatus(rep); setStatusErr(null) } else { setStatusErr(res?.output || 'status failed') }
      const rel = await window.cumora.stack.releases()
      const list = parseJSONStep<StackReleaseEntry[]>(rel)
      if (Array.isArray(list)) setReleases(list)
    } catch (e) {
      setStatusErr(String(e))
    }
  }, [])

  useEffect(() => {
    if (!isElectron || !window.cumora?.stack) return
    void refresh()
    const timer = window.setInterval(() => { void refresh() }, 30_000)
    return () => window.clearInterval(timer)
  }, [refresh])

  useEffect(() => {
    if (!isElectron) return
    void window.cumora?.update?.getAppInfo?.().then((info) => {
      if (info?.version) setAppVersion(info.version)
    }).catch(() => { /* 版本缺省 = 不给升级按钮,无害 */ })
  }, [])

  // degraded → 托盘警示(状态轮询驱动;面板红与托盘警示同源)。
  const degraded = useMemo(() => {
    if (!status?.stackd?.children) return false
    return status.stackd.children.some((c) => !c.running || c.circuitOpen)
  }, [status])
  useEffect(() => {
    window.cumora?.stack?.reportDegraded?.(degraded)
  }, [degraded])

  // 操作进行中:轮询等 livez 回来(与向导 booting 同形态)。
  useEffect(() => {
    if (op !== 'booting') return
    const started = Date.now()
    const tick = window.setInterval(() => setBootingSeconds(Math.floor((Date.now() - started) / 1000)), 1000)
    const poll = window.setInterval(() => {
      void window.cumora?.stack?.probe().then((p) => {
        if (p.serverUp) {
          window.clearInterval(poll); window.clearInterval(tick)
          void finishOp()
        }
      }).catch(() => { /* 下一轮 */ })
    }, 3000)
    return () => { window.clearInterval(poll); window.clearInterval(tick) }
  }, [op])

  async function finishOp() {
    await refresh()
    setOp(null)
    setOpSteps({})
  }

  const runStep = useCallback(async (key: string, fn: () => Promise<StackStepResult>) => {
    setOpSteps((s) => ({ ...s, [key]: { status: 'running' } }))
    const res = await fn()
    setOpSteps((s) => ({ ...s, [key]: { status: res.ok ? 'ok' : 'fail', tail: res.output?.split('\n').slice(-6).join('\n') } }))
    return res
  }, [])

  async function startUpgrade() {
    setConfirming(null)
    setOp('upgrade')
    const stack = window.cumora?.stack
    if (!stack) return
    const absorb = await runStep('absorb', () => stack.absorb({}))
    if (!absorb.ok) { setOp(null); return }
    const restart = await runStep('restart', () => stack.restart())
    if (!restart.ok) { setOp(null); return }
    setBootingSeconds(0)
    setOp('booting')
  }

  async function startRollback(version: string) {
    setConfirming(null)
    setPendingRollback(null)
    setOp('rollback')
    const stack = window.cumora?.stack
    if (!stack) return
    const rb = await runStep('rollback', () => stack.rollback(version))
    if (!rb.ok) { setOp(null); return }
    setBootingSeconds(0)
    setOp('booting')
  }

  if (!isElectron || !window.cumora?.stack) {
    return <div className="text-[13px] text-ink-400">{t('stack.consoleOnly')}</div>
  }

  const stackVersion = status?.version || ''
  const upgradeAvailable = !!appVersion && !!stackVersion && compareVersion(appVersion, stackVersion) > 0
  const children = status?.stackd?.children ?? []
  const rollable = releases.filter((r) => !r.current)

  const stepRow = (key: string, label: string) => {
    const st = opSteps[key]?.status ?? 'pending'
    return (
      <div className="flex items-center gap-2 text-[13px]">
        <span className={st === 'ok' ? 'text-emerald-600' : st === 'fail' ? 'text-red-600' : st === 'running' ? 'text-sky-600 animate-pulse' : 'text-ink-300'}>
          {st === 'ok' ? '✓' : st === 'fail' ? '✗' : st === 'running' ? '⟳' : '·'}
        </span>
        <span className={st === 'pending' ? 'text-ink-400' : 'text-ink-800'}>{label}</span>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-6 pb-10">
      {degraded && (
        <div className="rounded-[10px] border border-red-300 bg-red-50 text-red-700 text-[13px] px-3 py-2">
          {t('stack.degraded')}
        </div>
      )}

      {/* 状态区:五服务 + 探针 + 版本 */}
      <section className="flex flex-col gap-2">
        <h3 className="font-display text-[15px] text-ink-900">{t('stack.statusTitle')}</h3>
        {statusErr && <div className="text-[12px] text-red-600">{statusErr}</div>}
        <div className="flex flex-wrap gap-2">
          <Chip ok={status?.livez?.status === 'ok'} label={`livez ${status?.livez?.status || '?'}`} detail={status?.livez?.detail} />
          <Chip ok={status?.healthz?.status === 'ok'} label={`healthz ${status?.healthz?.status || '?'}`} detail={status?.healthz?.detail} />
        </div>
        {children.length > 0 ? (
          <div className="flex flex-col gap-1">
            {children.map((c) => (
              <div key={c.name} className="flex items-center gap-2 text-[13px]">
                <span className={c.running && !c.circuitOpen ? 'text-emerald-600' : 'text-red-600'}>
                  {c.running && !c.circuitOpen ? '✓' : '✗'}
                </span>
                <span className="w-24 text-ink-800">{c.name}</span>
                <span className="text-ink-400 text-[11px]">
                  restarts={c.restarts ?? 0}{c.circuitOpen ? ' · circuit-open' : ''}{c.lastErr ? ` · ${c.lastErr}` : ''}
                </span>
              </div>
            ))}
          </div>
        ) : (
          <div className="text-[12px] text-ink-400">{t('stack.noStackdSection')}</div>
        )}
        <div className="text-[12px] text-ink-500">
          {t('stack.versionLine')}: <span className="font-mono">{stackVersion || '-'}</span>
          {status?.manifest?.deps && (
            <span className="text-ink-400">
              {' '}({Object.entries(status.manifest.deps).map(([k, v]) => `${k}@${v}`).join(' · ')})</span>
          )}
        </div>
      </section>

      {/* 升级区:制品版本 > 栈版本才出现;手动确认 + 分段进度 */}
      <section className="flex flex-col gap-2">
        <h3 className="font-display text-[15px] text-ink-900">{t('stack.upgradeTitle')}</h3>
        {(op === 'upgrade' || op === 'booting') ? (
          <div className="flex flex-col gap-1">
            {stepRow('absorb', t('stack.stepAbsorb'))}
            {stepRow('restart', t('stack.stepRestart'))}
            {op === 'booting' && (
              <div className="text-[13px] text-sky-600 animate-pulse">{t('stack.booting')}({bootingSeconds}s)</div>
            )}
            {Object.entries(opSteps).map(([k, st]) => st.tail && (
              <pre key={k} className="text-[10px] leading-4 text-ink-400 bg-ink-900/5 rounded-lg p-2 max-h-28 overflow-y-auto whitespace-pre-wrap break-all">{st.tail}</pre>
            ))}
          </div>
        ) : upgradeAvailable ? (
          <>
            <div className="text-[13px] text-ink-600">
              {t('stack.upgradeAvailable')}: <span className="font-mono">{stackVersion}</span> → <span className="font-mono">{appVersion}</span>
            </div>
            <button
              type="button"
              onClick={() => setConfirming('upgrade')}
              className="h-10 w-fit px-4 rounded-[10px] bg-[#1f2328] hover:bg-[#2a3037] text-white text-[13px] transition-colors"
            >
              {t('stack.upgradeButton')}
            </button>
          </>
        ) : (
          <div className="text-[12px] text-ink-400">
            {t('stack.upgradeToDate')}
            {appVersion ? ` (制品 ${appVersion})` : ''}
          </div>
        )}
      </section>

      {/* 回滚区:最近 releases;安全门禁用的条目带原因 */}
      <section className="flex flex-col gap-2">
        <h3 className="font-display text-[15px] text-ink-900">{t('stack.rollbackTitle')}</h3>
        {rollable.length === 0 && <div className="text-[12px] text-ink-400">{t('stack.noRollback')}</div>}
        {rollable.map((r) => (
          <div key={r.version} className="flex items-center gap-3">
            <span className="font-mono text-[13px] text-ink-800 w-28">{r.version}</span>
            {r.rolloutBlocked ? (
              <span className="text-[11px] text-amber-600" title={r.rolloutBlocked}>{t('stack.rollbackBlocked')}</span>
            ) : (
              <button
                type="button"
                disabled={op !== null}
                onClick={() => { setPendingRollback(r.version); setConfirming('rollback') }}
                className="h-8 px-3 rounded-[8px] border border-ink-200 hover:bg-cloud text-[12px] text-ink-700 disabled:opacity-50"
              >
                {t('stack.rollbackButton')}
              </button>
            )}
          </div>
        ))}
        {(op === 'rollback' || op === 'booting') && opSteps.rollback && (
          <div className="flex flex-col gap-1">
            {stepRow('rollback', t('stack.stepRollback'))}
            {op === 'booting' && (
              <div className="text-[13px] text-sky-600 animate-pulse">{t('stack.booting')}({bootingSeconds}s)</div>
            )}
          </div>
        )}
      </section>

      {/* 确认弹层:升级/回滚共形(影响面明示) */}
      {confirming && (
        <div className="fixed inset-0 z-50 grid place-items-center bg-black/30" onClick={() => setConfirming(null)}>
          <div className="w-[420px] rounded-[14px] bg-white p-5 flex flex-col gap-3 shadow-xl" onClick={(e) => e.stopPropagation()}>
            <div className="font-display text-[16px] text-ink-900">
              {confirming === 'upgrade'
                ? t('stack.confirmUpgradeTitle')
                : t('stack.confirmRollbackTitle', { version: pendingRollback ?? '' })}
            </div>
            <div className="text-[13px] text-ink-600 flex flex-col gap-1">
              <div>{confirming === 'upgrade' ? t('stack.confirmUpgradeBody') : t('stack.confirmRollbackBody')}</div>
              <div className={streamingCount > 0 ? 'text-amber-600' : 'text-ink-400'}>
                {streamingCount > 0
                  ? t('stack.agentsActiveWarning', { count: streamingCount })
                  : t('stack.agentsIdleNote')}
              </div>
            </div>
            <div className="flex gap-2 justify-end">
              <button type="button" onClick={() => setConfirming(null)} className="h-9 px-4 rounded-[8px] border border-ink-200 text-[13px] text-ink-600">
                {t('stack.confirmCancel')}
              </button>
              <button
                type="button"
                onClick={() => {
                  if (confirming === 'upgrade') void startUpgrade()
                  else if (pendingRollback) void startRollback(pendingRollback)
                }}
                className="h-9 px-4 rounded-[8px] bg-[#1f2328] hover:bg-[#2a3037] text-white text-[13px]"
              >
                {t('stack.confirmGo')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function Chip({ ok, label, detail }: { ok: boolean; label: string; detail?: string }) {
  return (
    <span
      title={detail}
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[11px] font-mono border ${ok ? 'border-emerald-200 bg-emerald-50 text-emerald-700' : 'border-red-200 bg-red-50 text-red-700'}`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${ok ? 'bg-emerald-500' : 'bg-red-500'}`} />
      {label}
    </span>
  )
}
