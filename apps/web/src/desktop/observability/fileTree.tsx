import type { ApiAgentWorkspaceFile } from '@/api/client'
import { CodeBlock, RichBody } from '@/components/Message'
import { useT } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { bytes } from './shared'

/* ============== Workspace file tree ============== */

interface FileTreeNode {
  name: string
  /** Full path from root, e.g. `memory/preference/mem-abc.md`. */
  path: string
  kind: 'folder' | 'file'
  /** Folder children indexed by name, sorted folders-first then alpha. */
  children?: FileTreeNode[]
  /** File-only: backref to the original API row for metadata + size. */
  file?: ApiAgentWorkspaceFile
}

/** Build a hierarchical tree from a flat list of paths. Folders are
 *  synthesized for every `/`-separated prefix; files sit at the leaves. */
export function buildFileTree(files: ApiAgentWorkspaceFile[]): FileTreeNode[] {
  const root: FileTreeNode = { name: '', path: '', kind: 'folder', children: [] }
  for (const f of files) {
    const segs = f.path.split('/')
    let cursor = root
    for (let i = 0; i < segs.length; i++) {
      const seg = segs[i]
      const isLast = i === segs.length - 1
      const prefix = segs.slice(0, i + 1).join('/')
      let next = cursor.children?.find((c) => c.name === seg && c.kind === (isLast ? 'file' : 'folder'))
      if (!next) {
        next = isLast
          ? { name: seg, path: prefix, kind: 'file', file: f }
          : { name: seg, path: prefix, kind: 'folder', children: [] }
        cursor.children = cursor.children ?? []
        cursor.children.push(next)
      }
      cursor = next
    }
  }
  const sortRec = (n: FileTreeNode) => {
    if (!n.children) return
    n.children.sort((a, b) => {
      if (a.kind !== b.kind) return a.kind === 'folder' ? -1 : 1
      return a.name.localeCompare(b.name)
    })
    for (const c of n.children) sortRec(c)
  }
  sortRec(root)
  return root.children ?? []
}

/** All ancestor folder paths so a freshly-loaded list can start with
 *  every folder expanded (a flat-feeling default for small trees). */
export function allFolderPaths(nodes: FileTreeNode[], out: string[] = []): string[] {
  for (const n of nodes) {
    if (n.kind === 'folder') {
      out.push(n.path)
      if (n.children) allFolderPaths(n.children, out)
    }
  }
  return out
}

function FolderRow({ node, depth, expanded, onToggle }: {
  node: FileTreeNode; depth: number; expanded: boolean; onToggle: () => void
}) {
  return (
    <button
      onClick={onToggle}
      className="flex w-full items-center gap-1.5 rounded-[7px] px-1.5 py-1 text-left transition hover:bg-cloud/80"
      style={{ paddingLeft: 6 + depth * 14 }}
    >
      <svg
        width="10" height="10" viewBox="0 0 10 10"
        className="shrink-0 text-ink-400 transition-transform"
        style={{ transform: expanded ? 'rotate(90deg)' : 'rotate(0deg)' }}
      >
        <path d="M3 1.5L7 5L3 8.5" stroke="currentColor" strokeWidth="1.4" fill="none" strokeLinecap="round" strokeLinejoin="round" />
      </svg>
      <svg width="14" height="14" viewBox="0 0 14 14" className="shrink-0 text-skype-deep">
        <path
          d={expanded
            ? 'M1.5 4h3l1 1h7v6.5a1 1 0 0 1-1 1H1.5z'
            : 'M1.5 3.5h3l1 1H12a1 1 0 0 1 1 1V11a1 1 0 0 1-1 1H2.5a1 1 0 0 1-1-1V3.5z'}
          fill="currentColor" opacity="0.18"
        />
        <path
          d="M1.5 4.5h4l1 1h6.5"
          stroke="currentColor" strokeWidth="1.2" fill="none" strokeLinecap="round" strokeLinejoin="round"
        />
      </svg>
      <span className="truncate font-mono text-[12px] font-semibold text-ink-700">{node.name}</span>
    </button>
  )
}

const FILE_TYPE_TINT: Record<string, { bg: string; fg: string }> = {
  md:   { bg: 'var(--sky2-50)',    fg: 'var(--skype-deep)' },
  json: { bg: 'var(--gold-soft)',  fg: 'var(--gold-deep)' },
  yaml: { bg: 'var(--gold-soft)',  fg: 'var(--gold-deep)' },
  yml:  { bg: 'var(--gold-soft)',  fg: 'var(--gold-deep)' },
  ts:   { bg: 'var(--whisper-50)', fg: 'var(--whisper-deep)' },
  tsx:  { bg: 'var(--whisper-50)', fg: 'var(--whisper-deep)' },
  js:   { bg: 'var(--whisper-50)', fg: 'var(--whisper-deep)' },
  py:   { bg: 'var(--coral-soft)', fg: 'var(--coral-deep)' },
  sh:   { bg: 'var(--ink-100)',    fg: 'var(--ink-700)' },
  txt:  { bg: 'var(--ink-100)',    fg: 'var(--ink-500)' },
}

function FileRow({ node, depth, active, onClick }: {
  node: FileTreeNode; depth: number; active: boolean; onClick: () => void
}) {
  if (!node.file) return null
  const ext = node.name.includes('.') ? node.name.split('.').pop()!.toLowerCase() : ''
  const tint = FILE_TYPE_TINT[ext] ?? { bg: 'var(--ink-100)', fg: 'var(--ink-500)' }
  return (
    <button
      onClick={onClick}
      className={cn(
        'flex w-full items-center gap-2 rounded-[7px] py-1 pr-1.5 text-left transition',
        active ? 'bg-sky2-50 ring-1 ring-sky2-200' : 'hover:bg-cloud/80',
      )}
      style={{ paddingLeft: 6 + depth * 14 }}
    >
      <span className="ml-[10px] grid h-[18px] w-[22px] shrink-0 place-items-center rounded-[4px] font-mono text-[9px] font-bold uppercase tracking-tight"
        style={{ background: tint.bg, color: tint.fg }}>
        {ext || 'f'}
      </span>
      <span className={cn('truncate font-mono text-[12px]', active ? 'font-semibold text-skype-deep' : 'text-ink-700')}>{node.name}</span>
      <span className="ml-auto shrink-0 font-mono text-[10px] text-ink-300">{bytes(node.file.size)}</span>
    </button>
  )
}

export function FileTree({ nodes, depth, expanded, onToggle, selectedPath, onSelect }: {
  nodes: FileTreeNode[]
  depth: number
  expanded: Set<string>
  onToggle: (path: string) => void
  selectedPath: string | null
  onSelect: (path: string) => void
}) {
  return (
    <div className="space-y-px">
      {nodes.map((n) => {
        if (n.kind === 'folder') {
          const isOpen = expanded.has(n.path)
          return (
            <div key={n.path}>
              <FolderRow node={n} depth={depth} expanded={isOpen} onToggle={() => onToggle(n.path)} />
              {isOpen && n.children && (
                <FileTree
                  nodes={n.children}
                  depth={depth + 1}
                  expanded={expanded}
                  onToggle={onToggle}
                  selectedPath={selectedPath}
                  onSelect={onSelect}
                />
              )}
            </div>
          )
        }
        return (
          <FileRow
            key={n.path}
            node={n}
            depth={depth}
            active={n.path === selectedPath}
            onClick={() => onSelect(n.path)}
          />
        )
      })}
    </div>
  )
}

/** File-content renderer that picks the right view based on extension.
 *   - `.md` → RichBody (cumora's own markdown-ish renderer)
 *   - `.json` / `.ts` / `.py` / etc → CodeBlock (syntax-highlit)
 *   - anything else → wrapped plain text in paper theme */
export function FileViewer({ path, body }: { path: string; body: string }) {
  const t = useT()
  const ext = path.includes('.') ? path.split('.').pop()!.toLowerCase() : ''
  if (!body) return <div className="text-[13px] italic text-ink-400">{t('obs.emptyFile')}</div>
  if (ext === 'md' || ext === 'markdown') {
    return (
      <div className="cumora-prose font-display text-[14px] leading-[1.7] text-ink-900">
        <RichBody body={body} />
      </div>
    )
  }
  const codeLangs: Record<string, string> = {
    json: 'json', yaml: 'yaml', yml: 'yaml', ts: 'typescript', tsx: 'tsx',
    js: 'javascript', jsx: 'jsx', py: 'python', sh: 'bash', go: 'go',
    rs: 'rust', sql: 'sql', css: 'css', html: 'html',
  }
  if (codeLangs[ext]) {
    return <CodeBlock lang={codeLangs[ext]} code={body} />
  }
  return (
    <pre className="whitespace-pre-wrap break-words font-mono text-[12.5px] leading-[1.6] text-ink-900">
      {body}
    </pre>
  )
}
