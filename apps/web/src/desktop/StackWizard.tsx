import { useCallback, useEffect, useRef, useState } from 'react'
import { api, getServerOrigin } from '@/api/client'
import { TitleBar } from '@/desktop/TitleBar'
import { isElectron, type StackStepResult } from '@/lib/runtime'
import { useT } from '@/lib/i18n'

/**
 * First-run Stack wizard (#284, ADR 0005). Appears when the local stack
 * server is unreachable: guides a one-shot import of .env/daemon.env (or
 * fresh GitHub OAuth credentials), then absorbs the AppImage's bundled
 * payload into ~/.local/share/cumora and installs the single systemd
 * user unit. All orchestration lives in the bundled cumora-stack binary
 * (import-env / absorb / install / doctor) — the wizard is pure UI over
 * IPC, so the CLI acceptance path and this GUI can never drift apart.
 *
 * Credential hygiene: values flow one way (into import-env via a 0600
 * staging file in the main process) and never come back — the renderer
 * only ever sees key names and exit codes.
 */

type Step = 'source' | 'install' | 'booting' | 'done'

interface StepState {
  status: 'pending' | 'running' | 'ok' | 'fail'
  tail?: string
}

interface DoctorReport {
  anyFail: boolean
  checks: Array<{ group: string; name: string; status: string; detail?: string }>
}

const WIZARD_SKIP_KEY = 'cumora.wizard.skipForNow'

/** origin 指向非本机 = 用户显式配了远程/LAN 服务器(api/core 的
 *  localStorage 覆盖)—— 这台机器没本地栈是正常态,向导不得拦
 *  (评审 P2;硬拦 = 远程用户每次启动死在全屏向导上)。 */
function originIsRemote(): boolean {
  const origin = getServerOrigin()
  if (!origin) return false
  try {
    const host = new URL(origin).hostname
    return !['127.0.0.1', 'localhost', '[::1]', '::1'].includes(host)
  } catch {
    return false
  }
}

export function StackWizardGate({ children }: { children: React.ReactNode }) {
  const [gate, setGate] = useState<'probing' | 'wizard' | 'pass'>('probing')
  useEffect(() => {
    if (!isElectron || !window.cumora?.stack || originIsRemote() ||
        sessionStorage.getItem(WIZARD_SKIP_KEY) === '1') {
      setGate('pass')
      return
    }
    let alive = true
    window.cumora.stack!.probe().then((p) => {
      if (alive) setGate(p.wizard ? 'wizard' : 'pass')
    }).catch(() => { if (alive) setGate('pass') })
    return () => { alive = false }
  }, [])
  if (gate === 'probing') {
    return <div className="h-screen w-screen" style={{ background: 'var(--paper)' }} />
  }
  if (gate === 'pass') return <>{children}</>
  return <StackWizard />
}

function StackWizard() {
  const t = useT()
  const [step, setStep] = useState<Step>('source')
  // 导入源:false = 净机新录 GitHub 凭据;true = 指到既有 env 文件。
  const [useExisting, setUseExisting] = useState(true)
  // 既有部署缺省路径:存量布局(向导出现 = 栈没起,但 env 文件可能还在)。
  // 初始值而非 effect 回填 —— 回填会让用户无法清空重输(评审 P3)。
  const [envFile, setEnvFile] = useState('~/Code/cumora/.env')
  const [daemonEnvFile, setDaemonEnvFile] = useState('~/.cumora/daemon.env')
  const [ghId, setGhId] = useState('')
  const [ghSecret, setGhSecret] = useState('')
  const [err, setErr] = useState<string | null>(null)
  const [steps, setSteps] = useState<Record<string, StepState>>({})
  const [doctor, setDoctor] = useState<DoctorReport | null>(null)
  const [providers, setProviders] = useState<{ github: boolean | null }>({ github: null })
  const [elapsed, setElapsed] = useState(0)
  const outputRef = useRef<HTMLPreElement>(null)


  useEffect(() => {
    if (step !== 'booting') return
    const started = Date.now()
    const timer = window.setInterval(() => setElapsed(Math.floor((Date.now() - started) / 1000)), 1000)
    // install(enable --now)后 stackd 链式拉起(首启含 initdb + 迁移,
    // 最坏 ~5 分钟);轮询 livez 直到通。
    const poll = window.setInterval(() => {
      void window.cumora?.stack?.probe().then((p) => {
        if (p.serverUp) {
          window.clearInterval(poll)
          window.clearInterval(timer)
          void finish()
        }
      }).catch(() => { /* IPC 瞬断:下一轮再试 */ })
    }, 3000)
    return () => { window.clearInterval(poll); window.clearInterval(timer) }
  }, [step])

  async function finish() {
    const res = await window.cumora?.stack?.doctor().catch(() => null)
    // 退出码 1(anyFail)时 JSON 仍在 stdout —— 必须照解析,否则红项
    // 被丢弃、完成屏恒显"全绿"(评审 P1 的逻辑倒置)。
    if (res) {
      try {
        const start = res.stdout.indexOf('{')
        if (start >= 0) setDoctor(JSON.parse(res.stdout.slice(start)))
      } catch { /* 报告面解析失败不挡完成 */ }
    }
    // 登录链探活:GitHub provider 配置态(未配 = 显性提示而非裸 503)。
    try {
      const p = await api.authProviders()
      setProviders({ github: p.github })
    } catch { setProviders({ github: null }) }
    setStep('done')
  }

  const runStep = useCallback(async (key: string, fn: () => Promise<StackStepResult>) => {
    setSteps((s) => ({ ...s, [key]: { status: 'running' } }))
    const res = await fn()
    setSteps((s) => ({ ...s, [key]: { status: res.ok ? 'ok' : 'fail', tail: res.output?.split('\n').slice(-8).join('\n') } }))
    return res
  }, [])

  async function startInstall() {
    setErr(null)
    setStep('install')
    const stack = window.cumora?.stack
    if (!stack) return

    const imp = await runStep('import', () => stack.importEnv(
      useExisting ? { envFile, daemonEnvFile } : { creds: { GITHUB_CLIENT_ID: ghId, GITHUB_CLIENT_SECRET: ghSecret } },
    ))
    // spawn 失败(error=ENOENT 等,code 127)= 环境问题,不是红线。
    if (imp.error) {
      setErr(`${imp.error}${imp.stderr ? `\n${imp.stderr}` : ''}`)
      setStep('source')
      return
    }
    // 红线(缺 GitHub OAuth 键)= 回表单补,不是安装失败。
    if (imp.code === 1) {
      setErr(t('wizard.redlineMissing'))
      setStep('source')
      return
    }
    if (!imp.ok) {
      setErr(imp.output || imp.error || 'import-env failed')
      setStep('source')
      return
    }

    const abs = await runStep('absorb', () => stack.absorb({}))
    if (!abs.ok) {
      setErr(abs.output || abs.error || 'absorb failed')
      setStep('source')
      return
    }

    const inst = await runStep('install', () => stack.install())
    if (!inst.ok) {
      setErr(inst.output || inst.error || 'install failed')
      setStep('source')
      return
    }
    setStep('booting')
  }

  useEffect(() => {
    outputRef.current?.scrollTo({ top: outputRef.current.scrollHeight })
  }, [steps])

  const stepRow = (key: string, label: string) => {
    const st = steps[key]?.status ?? 'pending'
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
    <div className="fixed inset-0 grid place-items-center" style={{ background: 'var(--paper)' }}>
      <TitleBar />
      <div className="w-[420px] max-h-[88vh] overflow-y-auto flex flex-col gap-5">
        <div className="text-center">
          <div className="font-display text-[22px] text-ink-900">{t('wizard.title')}</div>
          <div className="font-display italic text-[13px] text-ink-400 mt-1">{t('wizard.desc')}</div>
        </div>

        {step === 'source' && (
          <div className="flex flex-col gap-4">
            <div className="flex gap-2">
              <ChoiceButton active={useExisting} onClick={() => setUseExisting(true)} label={t('wizard.fromExisting')} />
              <ChoiceButton active={!useExisting} onClick={() => setUseExisting(false)} label={t('wizard.freshCreds')} />
            </div>
            {useExisting ? (
              <div className="flex flex-col gap-2">
                <Field label={t('wizard.envPath')} value={envFile} onChange={setEnvFile} />
                <Field label={t('wizard.daemonPath')} value={daemonEnvFile} onChange={setDaemonEnvFile} />
                <div className="text-[11px] text-ink-300">{t('wizard.fromExistingNote')}</div>
              </div>
            ) : (
              <div className="flex flex-col gap-2">
                <Field label="GITHUB_CLIENT_ID" value={ghId} onChange={setGhId} mono />
                <Field label="GITHUB_CLIENT_SECRET" value={ghSecret} onChange={setGhSecret} mono password />
                <div className="text-[11px] text-ink-300">{t('wizard.freshNote')}</div>
              </div>
            )}
            {err && <div className="text-[12px] text-red-600 text-center">{err}</div>}
            <button
              type="button"
              onClick={() => void startInstall()}
              className="h-11 rounded-[10px] bg-[#1f2328] hover:bg-[#2a3037] text-white transition-colors text-[14px]"
            >
              {t('wizard.installButton')}
            </button>
            <button
              type="button"
              onClick={() => { sessionStorage.setItem(WIZARD_SKIP_KEY, '1'); location.reload() }}
              className="text-[11px] text-ink-300 underline mx-auto"
            >
              {t('wizard.skipForNow')}
            </button>
          </div>
        )}

        {(step === 'install' || step === 'booting') && (
          <div className="flex flex-col gap-3">
            {stepRow('import', t('wizard.stepImport'))}
            {stepRow('absorb', t('wizard.stepAbsorb'))}
            {stepRow('install', t('wizard.stepInstall'))}
            {step === 'booting' && (
              <div className="flex flex-col gap-2">
                <div className="text-[13px] text-sky-600 animate-pulse">
                  {t('wizard.booting')}({elapsed}s)
                </div>
                {elapsed > 360 && (
                  <>
                    <div className="text-[12px] text-amber-600 text-center">{t('wizard.bootSlow')}</div>
                    <button
                      type="button"
                      onClick={() => { void finish() }}
                      className="text-[12px] text-ink-500 underline mx-auto"
                    >
                      {t('wizard.runDoctor')}
                    </button>
                  </>
                )}
              </div>
            )}
            {Object.entries(steps).map(([k, st]) => st.tail && (
              <pre key={k} ref={outputRef} className="text-[10px] leading-4 text-ink-400 bg-ink-900/5 rounded-lg p-2 max-h-32 overflow-y-auto whitespace-pre-wrap break-all">{st.tail}</pre>
            ))}
            {err && <div className="text-[12px] text-red-600 text-center">{err}</div>}
            {step === 'install' && err && (
              <button type="button" onClick={() => setStep('source')} className="text-[12px] text-ink-500 underline">{t('wizard.back')}</button>
            )}
          </div>
        )}

        {step === 'done' && (
          <div className="flex flex-col gap-3">
            <div className={`text-[14px] text-center ${doctor?.anyFail ? 'text-red-600' : 'text-emerald-600'}`}>
              {doctor?.anyFail ? t('wizard.doctorRed') : t('wizard.doctorGreen')}
            </div>
            {doctor && (
              <div className="max-h-40 overflow-y-auto flex flex-col gap-0.5">
                {doctor.checks.filter((c) => c.status === 'fail' || c.status === 'warn').slice(0, 12).map((c, i) => (
                  <div key={i} className={`text-[11px] ${c.status === 'fail' ? 'text-red-600' : 'text-amber-600'}`}>
                    [{c.group}] {c.name}{c.detail ? ` — ${c.detail}` : ''}
                  </div>
                ))}
              </div>
            )}
            {providers.github === false && (
              <div className="text-[12px] text-amber-600 text-center">{t('auth.githubNotConfigured')}</div>
            )}
            <button
              type="button"
              onClick={() => location.reload()}
              className="h-11 rounded-[10px] bg-[#1f2328] hover:bg-[#2a3037] text-white transition-colors text-[14px]"
            >
              {t('wizard.openApp')}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

function ChoiceButton({ active, onClick, label }: { active: boolean; onClick: () => void; label: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`flex-1 h-10 rounded-[10px] text-[13px] transition-colors border ${active ? 'border-ink-800 bg-white text-ink-900' : 'border-ink-200 bg-transparent text-ink-400'}`}
    >
      {label}
    </button>
  )
}

function Field({ label, value, onChange, mono, password }: { label: string; value: string; onChange: (v: string) => void; mono?: boolean; password?: boolean }) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-[11px] text-ink-400">{label}</span>
      <input
        type={password ? 'password' : 'text'}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        spellCheck={false}
        autoComplete="off"
        className={`h-9 rounded-[8px] border border-ink-200 bg-white px-2 text-[13px] text-ink-900 outline-none focus:border-ink-400 ${mono ? 'font-mono' : ''}`}
      />
    </label>
  )
}
