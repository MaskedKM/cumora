// 我的视图桶(#219 ④)computers 件 —— 从 MeView.tsx 原样搬移:ComputersTab(设备
// 列表+配对命令装配+重连展开)+ DaemonUpgradeBanner(守护进程落后横幅,壳借以
// 跳 computers 页)+ 引擎/机型/状态词表(ENGINE_LABEL/KIND_ICON/STATUS_COLOR)。
import { useEffect, useState } from 'react'
import { api, getPairingServerOrigin } from '@/api/client'
import { CliMethodPicker, useCliLaunch } from '@/components/CliMethodPicker'
import { useT } from '@/lib/i18n'
import { isWindows } from '@/lib/runtime'
import { useComputers } from '@/stores/computers'
import { useParticipants } from '@/stores/participants'
import { Section } from './shared'

// Brand and engine names stay in English in every locale. The values
// below are display labels surfaced to users when we list computers —
// products keep their own casing.
const ENGINE_LABEL: Record<string, string> = { claude: 'Claude Code', codex: 'Codex', grok: 'Grok Build', cursor: 'Cursor', zcode: 'ZCode' }
const KIND_ICON: Record<string, string> = { local: '💻', vps: '🖥' }
const STATUS_COLOR: Record<string, string> = { online: '#3BB273', busy: '#E6A23C', offline: 'var(--ink-300)' }

export function ComputersTab() {
  const t = useT()
  const byId = useComputers((s) => s.byId)
  const loaded = useComputers((s) => s.loaded)
  const participants = useParticipants((s) => s.byId)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const [code, setCode] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  // Engine for a NEWLY added computer's starter/assigned agents. Claude is the
  // default (no flag → daemon auto-detects); every other pick is named
  // explicitly, or the daemon auto-detects and engines[0] silently wins.
  const [engine, setEngine] = useState<'claude' | 'codex' | 'grok' | 'cursor' | 'zcode'>('claude')
  // Default on: install the always-on service (auto-start/restart/update).
  // --install-service is macOS/Linux only (daemon throws on Windows) → off + hidden there.
  const [asService, setAsService] = useState(!isWindows)
  // Per-computer re-pair (reconnect) command, keyed by computer id.
  const [repairFor, setRepairFor] = useState<string | null>(null)
  const [repairCode, setRepairCode] = useState<string | null>(null)
  const [repairCopied, setRepairCopied] = useState(false)

  useEffect(() => { void useComputers.getState().refresh() }, [])
  useEffect(() => { if (!repairCopied) return; const id = window.setTimeout(() => setRepairCopied(false), 1600); return () => window.clearTimeout(id) }, [repairCopied])

  async function toggleRepair(id: string) {
    if (repairFor === id) { setRepairFor(null); setRepairCode(null); return }
    setRepairFor(id); setRepairCode(null)
    try { setRepairCode((await api.repairComputer(id)).code) }
    catch (e) { alert(e instanceof Error ? e.message : String(e)); setRepairFor(null) }
  }
  useEffect(() => { if (!copied) return; const id = window.setTimeout(() => setCopied(false), 1600); return () => window.clearTimeout(id) }, [copied])

  function copyCommand() {
    void navigator.clipboard?.writeText(pairCommand)
    setCopied(true)
  }

  const origin = getPairingServerOrigin()
  const serverFlag = origin ? ` --server ${origin}` : ''
  // Launch method for the pairing + repair commands: published npm package vs
  // a local repo build. Persisted — see CliMethodPicker.
  const { method, setMethod, localPath, setLocalPath, cli } = useCliLaunch()
  const engineFlag = engine === 'claude' ? '' : ` --engine ${engine}`
  const pairCommand = code ? `${cli} agent computer --pair ${code}${serverFlag}${engineFlag}${asService ? ' --install-service' : ''}` : ''
  const list = Object.values(byId).sort((a, b) => a.name.localeCompare(b.name))

  function agentCount(computerId: string): number {
    return Object.values(participants).filter((p) =>
      p.kind === 'agent' && !p.departedAt && p.computerId === computerId).length
  }

  // Clicking "Add a computer" just mints a pairing token and shows the command.
  // The computer itself is created server-side only when the daemon pairs and
  // reports the machine's real hostname — so no placeholder row, and it shows
  // up here (named after the machine) once paired, via the WS status event.
  async function addComputer() {
    setErr(null); setBusy(true)
    try {
      const res = await api.requestPairingCode()
      setCode(res.code)
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally { setBusy(false) }
  }

  async function remove(id: string, label: string) {
    if (!confirm(t('me.removeConfirm', { label }))) return
    try {
      await api.deleteComputer(id)
      await useComputers.getState().refresh()
    } catch (e) { alert(e instanceof Error ? e.message : String(e)) }
  }

  return (
    <div className="space-y-6">
      <Section title={t('me.sectionComputers')}>
        {/* biome-ignore lint/security/noDangerouslySetInnerHtml: static copy from the locale bundle, not user input */}
        <p className="text-[13px] text-ink-500 mb-4 max-w-[640px]" dangerouslySetInnerHTML={{ __html: t('me.computersIntro') }} />

        {!loaded && <div className="text-[13px] text-ink-400">{t('common.loading')}</div>}

        <div className="grid gap-3">
          {list.map((c) => {
            const n = agentCount(c.id)
            const expanded = repairFor === c.id
            const repairCmd = repairCode ? `${cli} agent computer --pair ${repairCode}${serverFlag}` : ''
            return (
              <div key={c.id} className="bg-cloud rounded-[14px]" style={{ border: '1px solid var(--ink-100)' }}>
                <div
                  className={'p-4 flex items-center gap-4 rounded-[14px] cursor-pointer hover:bg-sky-50/50'}
                  onClick={() => void toggleRepair(c.id)}>
                  <div className="text-[22px] w-8 text-center">{KIND_ICON[c.kind] ?? '🖥'}</div>
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-display font-medium text-[16px] text-ink-900">{c.name}</span>
                      <span className="inline-flex items-center gap-1.5 text-[11px] text-ink-500">
                        <span className="w-2 h-2 rounded-full" style={{ background: STATUS_COLOR[c.status] ?? 'var(--ink-300)' }} />
                        {c.status}
                      </span>
                      {c.daemonOutdated && (
                        <span className="inline-flex items-center gap-1 text-[10.5px] font-semibold px-2 py-0.5 rounded-full"
                          style={{ background: 'rgba(244,183,64,0.18)', color: 'var(--gold-deep)' }}
                          title={c.latestDaemonVersion ? t('me.updateToVersion', { version: c.latestDaemonVersion }) : t('me.updateAvailableTitle')}>
                          ↑ {t('me.updateAvailableShort')}{c.daemonVersion ? ` · ${t('me.daemonVersion', { version: c.daemonVersion })}` : ''}
                        </span>
                      )}
                    </div>
                    <div className="text-[12px] text-ink-500 mt-0.5">
                      {c.availableEngines.map((e) => ENGINE_LABEL[e] ?? e).join(', ') || '—'}
                      {' · '}{n === 1 ? t('me.agentsCountOne', { n }) : t('me.agentsCountOther', { n })}
                      {c.daemonVersion && (
                        <>{' · '}<span className="font-mono text-[11px] text-ink-400">{t('me.daemonVersion', { version: c.daemonVersion })}</span></>
                      )}
                    </div>
                  </div>
                  <span className="text-[12px] font-semibold text-skype-deep">{expanded ? t('me.hideAction') : t('me.reconnect')}</span>
                  <button onClick={(e) => { e.stopPropagation(); void remove(c.id, c.name) }}
                    className="text-[12px] font-semibold text-coral-deep hover:underline px-2 py-1">
                    {t('me.remove')}
                  </button>
                </div>
                {expanded && (
                  <div className="px-4 pb-4 pt-3 border-t border-ink-100">
                    <div className="text-[12px] text-ink-500 mb-2 italic font-display">
                      {t('me.repairHint', { name: c.name })}
                    </div>
                    {!repairCode ? (
                      <div className="text-[12px] text-ink-400">{t('me.computersGenerating')}</div>
                    ) : (
                      <>
                        <pre className="bg-ink-900 text-cloud rounded-[10px] p-3 text-[12px] overflow-x-auto whitespace-pre-wrap break-all font-mono select-all">{repairCmd}</pre>
                        <button onClick={(e) => { e.stopPropagation(); void navigator.clipboard?.writeText(repairCmd); setRepairCopied(true) }}
                          className="mt-2 inline-flex items-center justify-center min-w-[120px] text-[12px] font-semibold px-3 py-1.5 rounded-[9px] text-white transition-colors duration-200"
                          style={{ background: repairCopied ? '#3BB273' : 'var(--skype)' }}>
                          {repairCopied ? t('me.copied') : t('me.copyCommand')}
                        </button>
                      </>
                    )}
                  </div>
                )}
              </div>
            )
          })}
        </div>

        {code ? (
          <div className="mt-4 bg-sky-50 rounded-[14px] p-4" style={{ border: '1px solid var(--sky-100)' }}>
            <div className="text-[13px] font-semibold text-ink-900 mb-1">
              {t('me.runOnHost')}
            </div>
            {/* biome-ignore lint/security/noDangerouslySetInnerHtml: static copy from the locale bundle, not user input */}
            <div className="text-[11.5px] text-ink-500 mb-2.5 italic font-display" dangerouslySetInnerHTML={{ __html: t('me.engineRequired') }} />
            <CliMethodPicker method={method} onMethod={setMethod} localPath={localPath} onLocalPath={setLocalPath} />
            <div className="flex items-center gap-2.5 mb-2.5">
              <span className="text-[12px] text-ink-500">{t('me.engineLabel')}</span>
              <div className="inline-flex rounded-[9px] p-0.5" style={{ background: 'var(--ink-100)' }}>
                {([['claude', 'Claude Code'], ['codex', 'Codex'], ['grok', 'Grok Build'], ['cursor', 'Cursor'], ['zcode', 'ZCode']] as const).map(([id, label]) => (
                  <button key={id} type="button" onClick={() => setEngine(id)}
                    className="px-3 py-1 rounded-[7px] text-[12px] font-semibold transition-colors duration-150"
                    style={engine === id
                      ? { background: 'var(--paper)', color: 'var(--ink-900)', boxShadow: '0 1px 2px rgba(0,0,0,0.08)' }
                      : { color: 'var(--ink-500)' }}>
                    {label}
                  </button>
                ))}
              </div>
              <span className="text-[11px] text-ink-400">{t('me.engineDefaultHint')}</span>
            </div>
            {isWindows ? (
              <div className="mb-2.5 text-[12px] text-ink-600">
                {t('me.keepTerminalOpen')}
                <span className="text-ink-400"> — {t('me.bgServiceUnsupported')}</span>
              </div>
            ) : (
              <label className="flex items-start gap-2 mb-2.5 cursor-pointer select-none">
                <input type="checkbox" checked={asService} onChange={(e) => setAsService(e.target.checked)} className="mt-[3px]" />
                <span className="text-[12px] text-ink-600">
                  {t('me.keepInBackground')} <span className="text-ink-400">— {t('me.keepInBackgroundDetail')}</span>
                </span>
              </label>
            )}
            <pre className="bg-ink-900 text-cloud rounded-[10px] p-3 text-[12px] overflow-x-auto whitespace-pre-wrap break-all font-mono select-all">{pairCommand}</pre>
            <div className="flex gap-2 mt-3">
              <button onClick={copyCommand} aria-live="polite"
                className="inline-flex items-center justify-center gap-1.5 min-w-[128px] text-[12px] font-semibold px-3 py-1.5 rounded-[9px] text-white transition-colors duration-200"
                style={{ background: copied ? '#3BB273' : 'var(--skype)' }}>
                {copied ? (
                  <>
                    <svg key="ck" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor"
                      strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"
                      style={{ animation: 'cp-pop 0.32s cubic-bezier(.36,1.6,.4,1)' }}>
                      <polyline points="20 6 9 17 4 12" />
                    </svg>
                    <span style={{ animation: 'cp-fade 0.2s ease' }}>{t('me.copiedShort')}</span>
                  </>
                ) : t('me.copyCommand')}
              </button>
              <button onClick={() => setCode(null)}
                className="text-[12px] font-semibold px-3 py-1.5 rounded-[9px] border border-ink-100 text-ink-600">{t('me.done')}</button>
            </div>
            <style>{`
              @keyframes cp-pop { 0% { transform: scale(0.2); opacity: 0 } 55% { transform: scale(1.3) } 100% { transform: scale(1); opacity: 1 } }
              @keyframes cp-fade { from { opacity: 0; transform: translateX(-2px) } to { opacity: 1; transform: none } }
            `}</style>
          </div>
        ) : (
          <>
            {err && <div className="mt-4 text-[12px] text-coral-deep bg-coral-soft rounded-[8px] p-2">{err}</div>}
            <button onClick={addComputer} disabled={busy}
              className="mt-4 px-4 py-2 rounded-[10px] bg-skype text-white text-[13px] font-semibold disabled:opacity-50">
              {busy ? t('me.computersGenerating') : t('me.addComputer')}
            </button>
          </>
        )}
      </Section>
    </div>
  )
}

/** Elegant, self-clearing "your daemon is behind" banner. Shows the moment any
 *  paired computer reports an outdated `cumora` daemon, and disappears on its own
 *  once the daemon restarts onto the latest (the next heartbeat clears the flag).
 *  Warm gold (an invitation to update), not alarm red. One-click copy. */
export function DaemonUpgradeBanner({ onJump }: { onJump: () => void }) {
  const t = useT()
  const byId = useComputers((s) => s.byId)
  const [copied, setCopied] = useState(false)
  const outdated = Object.values(byId).filter((c) => c.daemonOutdated)
  if (outdated.length === 0) return null

  const latest = outdated.map((c) => c.latestDaemonVersion).find(Boolean) ?? null
  const one = outdated.length === 1 ? outdated[0] : null
  // Run-mode-aware instructions. A supervised daemon (--install-service) is
  // restarted through its launchd/systemd wrapper; a manually-run foreground
  // daemon has no wrapper — the user must Ctrl-C it and re-run the command.
  // Unknown (old daemon not reporting the mode yet) gets the service command,
  // which itself prints install-service guidance when no service exists.
  const manual = outdated.filter((c) => c.daemonSupervised === false)
  const allManual = manual.length === outdated.length
  const cmd = allManual ? 'npx cumora@latest agent computer' : 'npx cumora@latest agent computer --restart'
  const copy = () => { void navigator.clipboard?.writeText(cmd); setCopied(true); window.setTimeout(() => setCopied(false), 1600) }

  return (
    <div className="relative overflow-hidden rounded-[16px] mb-6"
      style={{ border: '1px solid rgba(244,183,64,0.45)', background: 'linear-gradient(135deg, rgba(244,183,64,0.15), rgba(244,183,64,0.035) 70%)' }}>
      <div className="p-4 flex items-start gap-4">
        <div className="shrink-0 mt-0.5 w-9 h-9 rounded-full flex items-center justify-center text-[17px] font-bold"
          style={{ background: 'rgba(244,183,64,0.22)', color: 'var(--gold-deep)' }}>↑</div>
        <div className="flex-1 min-w-0">
          <div className="flex items-center gap-2.5 flex-wrap">
            <span className="font-display font-semibold text-[15px] text-ink-900">{t('me.updateAvailableTitle')}</span>
            {latest && (
              <span className="inline-flex items-center gap-1.5 text-[11px] font-mono">
                {one?.daemonVersion && (
                  <span className="px-1.5 py-0.5 rounded-full" style={{ background: 'var(--ink-100)', color: 'var(--ink-500)' }}>{t('me.daemonVersion', { version: one.daemonVersion })}</span>
                )}
                <span className="text-ink-300">→</span>
                <span className="px-1.5 py-0.5 rounded-full font-semibold" style={{ background: 'rgba(244,183,64,0.30)', color: 'var(--gold-deep)' }}>{t('me.daemonVersion', { version: latest })}</span>
              </span>
            )}
          </div>
          <div className="text-[12.5px] text-ink-600 mt-1 leading-relaxed">
            {one ? (
              <><strong className="text-ink-900 font-medium">{one.name}</strong>{' '}{t('me.daemonOutdatedOne')}</>
            ) : (
              <><strong className="text-ink-900 font-medium">{outdated.length}</strong> {t('me.daemonOutdatedMany', { n: outdated.length, names: outdated.map((c) => c.name).join(', ') })}</>
            )}
            {allManual ? (
              <>{' '}{one ? t('me.daemonManualHelp') : t('me.daemonManualHelpPlural')}</>
            ) : (
              <>
                {' '}{one ? t('me.daemonAutoHelp') : t('me.daemonAutoHelpPlural')}
                {manual.length > 0 && (
                  <>{' '}({manual.map((c) => c.name).join(', ')} {manual.length === 1 ? t('me.daemonManualInfix') : t('me.daemonManualInfixPlural')} {t('me.daemonManualRun')} — Ctrl-C and re-run there instead.)</>
                )}
              </>
            )}
          </div>
          <div className="mt-2.5 flex items-stretch gap-2 max-w-[580px]">
            <code className="flex-1 bg-ink-900 text-cloud rounded-[10px] px-3 py-2 text-[12px] font-mono overflow-x-auto whitespace-nowrap select-all flex items-center">{cmd}</code>
            <button onClick={copy}
              className="shrink-0 inline-flex items-center justify-center min-w-[82px] text-[12px] font-semibold px-3 rounded-[10px] text-white transition-colors duration-200"
              style={{ background: copied ? 'var(--avail)' : 'var(--gold-deep)' }}>
              {copied ? t('me.copiedShort') : t('me.copy')}
            </button>
          </div>
          <button onClick={onJump} className="mt-2 text-[11.5px] font-semibold text-gold-deep hover:underline" style={{ color: 'var(--gold-deep)' }}>
            {t('me.manageComputers')}
          </button>
        </div>
      </div>
    </div>
  )
}
