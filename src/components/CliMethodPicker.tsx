/**
 * The pairing/run command's launch method: the published npm package
 * (`npx cumora@latest` — the default for normal users) or a local repo
 * build (`node <path>/agent-cli/dist/cli.js` — for running from source or
 * riding unreleased changes, e.g. a fresh BYOA engine before it ships).
 *
 * The choice and the local path persist in localStorage so the onboarding
 * screen and the Me→Computers panel agree across sessions.
 */
import { useEffect, useState } from 'react'
import { useT } from '@/lib/i18n'

export type CliMethod = 'npx' | 'local'

const LS_METHOD = 'cumora.cliMethod'
const LS_PATH = 'cumora.cliLocalPath'

/** Shown (and used verbatim in the command) until the user types a path —
 *  visibly wrong-but-editable beats a silently broken copy. */
export const LOCAL_CLI_PLACEHOLDER = '~/Code/cumora/agent-cli/dist/cli.js'

export function useCliLaunch(): {
  method: CliMethod
  setMethod: (m: CliMethod) => void
  localPath: string
  setLocalPath: (p: string) => void
  /** The command prefix pairing/start commands run with. */
  cli: string
} {
  const [method, setMethod] = useState<CliMethod>(() =>
    (localStorage.getItem(LS_METHOD) as CliMethod | null) === 'local' ? 'local' : 'npx')
  const [localPath, setLocalPath] = useState<string>(() => localStorage.getItem(LS_PATH) ?? '')
  useEffect(() => { localStorage.setItem(LS_METHOD, method) }, [method])
  useEffect(() => { localStorage.setItem(LS_PATH, localPath) }, [localPath])
  const cli = method === 'npx' ? 'npx cumora@latest' : `node ${localPath.trim() || LOCAL_CLI_PLACEHOLDER}`
  return { method, setMethod, localPath, setLocalPath, cli }
}

/** Segmented npx/local picker (+ path input in local mode), visually the
 *  same pattern as the engine pickers in Onboarding/MeView. */
export function CliMethodPicker(props: {
  method: CliMethod
  onMethod: (m: CliMethod) => void
  localPath: string
  onLocalPath: (p: string) => void
}) {
  const t = useT()
  return (
    <div className="flex items-center gap-2.5 mb-2.5 flex-wrap">
      <span className="text-[12px] text-ink-500">{t('cli.method')}</span>
      <div className="inline-flex rounded-[9px] p-0.5" style={{ background: 'var(--ink-100)' }}>
        {([['npx', t('cli.methodNpx')], ['local', t('cli.methodLocal')]] as const).map(([id, label]) => (
          <button key={id} type="button" onClick={() => props.onMethod(id)}
            className="px-3 py-1 rounded-[7px] text-[12px] font-semibold transition-colors duration-150"
            style={props.method === id
              ? { background: 'var(--paper)', color: 'var(--ink-900)', boxShadow: '0 1px 2px rgba(0,0,0,0.08)' }
              : { color: 'var(--ink-500)' }}>
            {label}
          </button>
        ))}
      </div>
      {props.method === 'local' && (
        <input
          type="text"
          value={props.localPath}
          onChange={(e) => props.onLocalPath(e.target.value)}
          placeholder={LOCAL_CLI_PLACEHOLDER}
          spellCheck={false}
          className="flex-1 min-w-[240px] font-mono text-[11.5px] px-2.5 py-1.5 rounded-[8px] text-ink-900"
          style={{ border: '1px solid var(--ink-100)', background: 'var(--paper)' }}
        />
      )}
      <span className="text-[11px] text-ink-400">
        {props.method === 'npx' ? t('cli.methodNpxHint') : t('cli.methodLocalHint')}
      </span>
    </div>
  )
}
