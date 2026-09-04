import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ApiError,
  type ApiWorkspaceDetail,
  type ApiWorkspaceFileEntry,
  type ApiWorkspaceSummary,
  api,ws
} from '../api/client'
import { Input } from '../components/Input'
import { CodeBlock, RichBody } from '../components/Message'
import { ResizeHandle } from '../components/ResizeHandle'
import { TextArea } from '../components/TextArea'
import { useT } from '../lib/i18n'
import { useResizableWidth } from '../lib/useResizableWidth'
import { useAuth } from '../stores/auth'
import { useDocuments } from '../stores/documents'

const CODE_LANGS: Record<string, string> = {
  ts: 'typescript', tsx: 'typescript', js: 'javascript', jsx: 'javascript', mjs: 'javascript',
  py: 'python', go: 'go', rs: 'rust', java: 'java', sh: 'bash', bash: 'bash', zsh: 'bash',
  json: 'json', yaml: 'yaml', yml: 'yaml', toml: 'ini', ini: 'ini', css: 'css', sql: 'sql',
  html: 'xml', xml: 'xml', md: 'markdown',
}

function extOf(path: string): string {
  const dot = path.lastIndexOf('.')
  return dot === -1 ? '' : path.slice(dot + 1).toLowerCase()
}

const IMG_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg'])

function isImagePath(path: string): boolean {
  return IMG_EXTS.has(extOf(path))
}

function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name
}

function parentDir(path: string): string {
  const slash = path.lastIndexOf('/')
  return slash === -1 ? '' : path.slice(0, slash)
}

function bytes(n: number | null): string {
  if (n === null) return ''
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / (1024 * 1024)).toFixed(1)} MB`
}

function FilePreview({ path, body }: { path: string; body: string }) {
  const ext = extOf(path)
  if (ext === 'md' || ext === 'markdown') return <RichBody body={body} />
  const lang = CODE_LANGS[ext]
  if (lang) return <CodeBlock lang={lang} code={body} />
  return <pre className="whitespace-pre-wrap break-words font-mono text-[12.5px] leading-[1.6] text-stone-700">{body}</pre>
}

/**
 * Team workspaces — the human surface for the shared-real-folder concept
 * (CONTEXT.md: Workspace). List → detail with the member scope (explicit /
 * implicit sources), associations, and a folder browser whose read/write
 * goes through the same API the agents use, so the human is exactly as
 * privileged as their membership. All state is component-local: the app
 * remounts on company switch, so there is no cross-team leak surface.
 */
export function WorkspacesView() {
  const t = useT()
  const { width, onResizeStart } = useResizableWidth('sidebar:workspaces', 280, { min: 220, max: 480 })
  const docList = useDocuments((s) => s.list)
  const docLoad = useDocuments((s) => s.load)

  const [list, setList] = useState<ApiWorkspaceSummary[]>([])
  const [listError, setListError] = useState<string | null>(null)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [detail, setDetail] = useState<ApiWorkspaceDetail | null>(null)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [dirPath, setDirPath] = useState('')
  const [entries, setEntries] = useState<ApiWorkspaceFileEntry[] | null>(null)
  const [filesError, setFilesError] = useState<string | null>(null)
  const [openFile, setOpenFile] = useState<{ wsId: string; path: string; body: string } | null>(null)
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [fileError, setFileError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [savedTick, setSavedTick] = useState(0)
  const [creating, setCreating] = useState(false)
  const [newPath, setNewPath] = useState('')
  // #338 管理面与传输
  const role = useAuth((st) => st.companies.find((c) => c.id === st.activeCompanyId)?.role)
  const canManage = role === 'owner' || role === 'admin'
  const [nameFilter, setNameFilter] = useState('')
  const [openImage, setOpenImage] = useState<{ wsId: string; path: string; url: string; size: number } | null>(null)
  const [uploading, setUploading] = useState(false)
  const uploadRef = useRef<HTMLInputElement | null>(null)
  const [creatingWs, setCreatingWs] = useState(false)
  const [newWsName, setNewWsName] = useState('')
  const [newWsFolder, setNewWsFolder] = useState('')
  const [addingMember, setAddingMember] = useState(false)
  const [memberId, setMemberId] = useState('')
  const [addingLink, setAddingLink] = useState(false)
  const [linkKind, setLinkKind] = useState<'project' | 'board_card' | 'document'>('project')
  const [linkTarget, setLinkTarget] = useState('')
  const [manageError, setManageError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    setListError(null)
    api.listWorkspaces()
      .then((rows) => {
        if (cancelled) return
        setList(rows)
        setSelectedId((cur) => cur ?? rows[0]?.id ?? null)
      })
      .catch((e) => { if (!cancelled) setListError(e instanceof Error ? e.message : String(e)) })
    return () => { cancelled = true }
  }, [])

  // Document titles for the associations rail (best effort — projects and
  // board cards have no frontend store, those stay as ids).
  useEffect(() => { void docLoad() }, [docLoad])

  useEffect(() => {
    if (!selectedId) { setDetail(null); setDetailError(null); return }
    let cancelled = false
    setDetail(null)
    setDetailError(null)
    setOpenFile(null)
    setEditing(false)
    setDirPath('')
    setEntries(null)
    setFilesError(null)
    setFileError(null)
    setCreating(false)
    setNewPath('')
    setSavedTick(0)
    setManageError(null)
    setAddingMember(false)
    setAddingLink(false)
    setMemberId('')
    setLinkTarget('')
    if (openImage) { URL.revokeObjectURL(openImage.url); setOpenImage(null) }
    api.getWorkspace(selectedId)
      .then((d) => { if (!cancelled) setDetail(d) })
      .catch((e) => { if (!cancelled) setDetailError(e instanceof Error ? e.message : String(e)) })
    return () => { cancelled = true }
  }, [selectedId])

  const reloadDir = useCallback(() => {
    setFilesError(null)
    if (!selectedId || detail?.unboundAt) return
    let cancelled = false
    api.listWorkspaceFiles(selectedId, dirPath)
      .then((r) => { if (!cancelled) setEntries(r.entries) })
      .catch((e) => {
        if (cancelled) return
        setEntries(null)
        setFilesError(
          e instanceof ApiError && e.status === 403 ? t('ws.notMember')
            : e instanceof ApiError && e.status === 410 ? t('ws.unbound')
              : e instanceof Error ? e.message : String(e),
        )
      })
    return () => { cancelled = true }
  }, [selectedId, dirPath, detail?.unboundAt, t])

  // #337 实时刷新:订阅 workspace.files_changed(区级),当前打开的区
  // 变了就重拉目录;正在编辑时不覆盖草稿(保存后 reloadDir 自会追平)。
  // ws.on 幂等订阅,reloadDir deps 变化重挂无妨。
  useEffect(() => {
    const off = ws.on((e) => {
      if (e.type !== 'workspace.files_changed' || e.workspaceId !== selectedId) return
      if (editing) return
      reloadDir()
    })
    return off
  }, [selectedId, editing, reloadDir])

  useEffect(() => { reloadDir() }, [reloadDir])

  // blob URL 生命周期:组件卸载即撤销(openImage 切换处已即时撤销)。
  useEffect(() => () => { if (openImage) URL.revokeObjectURL(openImage.url) }, [openImage])

  // 迟到响应守卫(#342 评审 P2):mutation 后立即切区时,旧区的 detail/
  // 图片响应晚到不得回写 —— selectedIdRef 比对后再 set。
  const selectedIdRef = useRef<string | null>(null)
  useEffect(() => { selectedIdRef.current = selectedId }, [selectedId])

  const reloadDetail = useCallback(() => {
    if (!selectedId) return
    const wsAtStart = selectedId
    api.getWorkspace(selectedId)
      .then((d) => { if (selectedIdRef.current === wsAtStart) setDetail(d) })
      .catch(() => { /* 详情拉取失败保留旧态,下次切换重试 */ })
  }, [selectedId])

  const filteredEntries = useMemo(() => {
    if (!entries) return null
    const q = nameFilter.trim().toLowerCase()
    if (!q) return entries
    return entries.filter((e) => e.name.toLowerCase().includes(q))
  }, [entries, nameFilter])

  const dirty = editing && openFile !== null && openFile.wsId === selectedId && draft !== openFile.body
  const guardDirty = () => !dirty || confirm(t('ws.dirtyConfirm'))
  // Switching views via the Rail can't be intercepted from here — unsaved
  // edits are lost on view switch (same trade-off as the documents editor).
  const closeFile = () => {
    setOpenFile(null)
    setEditing(false)
    setSavedTick(0)
    if (openImage) { URL.revokeObjectURL(openImage.url); setOpenImage(null) }
  }

  const openEntry = async (entry: ApiWorkspaceFileEntry) => {
    if (!selectedId) return
    const path = joinPath(dirPath, entry.name)
    if (!guardDirty()) return
    setFileError(null)
    if (entry.dir) {
      closeFile()
      setDirPath(path)
      return
    }
    // #338 图片(不限 2MB 文本帽):原始字节读 → blob 预览。
    if (isImagePath(path)) {
      try {
        const blob = await api.fetchWorkspaceRaw(selectedId, path)
        if (selectedIdRef.current !== selectedId) { return } // 切区后迟到,弃
        if (openImage) URL.revokeObjectURL(openImage.url)
        setOpenFile(null)
        setEditing(false)
        setOpenImage({ wsId: selectedId, path, url: URL.createObjectURL(blob), size: blob.size })
      } catch (e) {
        setFileError(e instanceof Error ? e.message : String(e))
      }
      return
    }
    try {
      const f = await api.readWorkspaceFile(selectedId, path)
      if (openFile !== null || editing) {
        // a previous file was open — reset the saved flash for the new one
        setSavedTick(0)
      }
      if (openImage) URL.revokeObjectURL(openImage.url)
      setOpenImage(null)
      setOpenFile({ wsId: selectedId, path, body: f.body })
      setEditing(false)
    } catch (e) {
      if (e instanceof ApiError && e.status === 413) setFileError(t('ws.tooLarge'))
      else setFileError(e instanceof Error ? e.message : String(e))
    }
  }

  const save = async () => {
    if (!selectedId || !openFile || !editing || openFile.wsId !== selectedId) return
    setSaving(true)
    try {
      await api.writeWorkspaceFile(selectedId, openFile.path, draft)
      setOpenFile({ wsId: selectedId, path: openFile.path, body: draft })
      setEditing(false)
      setSavedTick((n) => n + 1)
      void reloadDir()
    } catch (e) {
      setFileError(e instanceof Error ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  const createFile = async () => {
    if (!selectedId || !newPath.trim()) return
    if (!guardDirty()) return
    setFileError(null)
    const path = newPath.trim().replace(/^\/+/, '')
    try {
      await api.writeWorkspaceFile(selectedId, path, '')
      setCreating(false)
      setNewPath('')
      setOpenFile({ wsId: selectedId, path, body: '' })
      setDraft('')
      setEditing(true)
      setSavedTick(0)
      void reloadDir()
    } catch (e) {
      setFileError(e instanceof Error ? e.message : String(e))
    }
  }

  const resetCreateWs = () => { setCreatingWs(false); setNewWsName(''); setNewWsFolder('') }

  const addMember = async () => {
    if (!selectedId || !memberId.trim()) return
    setManageError(null)
    try {
      await api.addWorkspaceMember(selectedId, memberId.trim())
      setAddingMember(false); setMemberId('')
      reloadDetail()
    } catch (e) { setManageError(e instanceof Error ? e.message : String(e)) }
  }

  const removeMember = async (pid: string) => {
    if (!selectedId) return
    setManageError(null)
    try {
      await api.removeWorkspaceMember(selectedId, pid)
      reloadDetail()
    } catch (e) { setManageError(e instanceof Error ? e.message : String(e)) }
  }

  const addLink = async () => {
    if (!selectedId || !linkTarget.trim()) return
    setManageError(null)
    try {
      await api.addWorkspaceAssociation(selectedId, linkKind, linkTarget.trim())
      setAddingLink(false); setLinkTarget('')
      reloadDetail()
    } catch (e) { setManageError(e instanceof Error ? e.message : String(e)) }
  }

  const removeLink = async (kind: 'project' | 'board_card' | 'document', targetId: string) => {
    if (!selectedId) return
    setManageError(null)
    try {
      await api.removeWorkspaceAssociation(selectedId, kind, targetId)
      reloadDetail()
    } catch (e) { setManageError(e instanceof Error ? e.message : String(e)) }
  }

  const unbind = async () => {
    if (!selectedId) return
    if (!confirm(t('ws.unbindConfirm'))) return
    setManageError(null)
    try {
      await api.unbindWorkspace(selectedId)
      reloadDetail()
    } catch (e) { setManageError(e instanceof Error ? e.message : String(e)) }
  }

  const upload = async (file: File) => {
    if (!selectedId || detail?.unboundAt) return
    setUploading(true)
    setFileError(null)
    try {
      const target = joinPath(dirPath, file.name)
      await api.uploadWorkspaceFile(selectedId, target, file)
      void reloadDir()
    } catch (e) {
      setFileError(e instanceof Error ? e.message : String(e))
    } finally {
      setUploading(false)
      if (uploadRef.current) uploadRef.current.value = ''
    }
  }

  const createWs = async () => {
    if (!newWsName.trim() || !newWsFolder.trim()) return
    setManageError(null)
    try {
      const ws = await api.createWorkspace(newWsName.trim(), newWsFolder.trim())
      resetCreateWs()
      // POST 201 响应不含 explicitMemberCount(契约内联对象),追加行补 0。
      setList((rows) => [...rows, { ...ws, explicitMemberCount: 0 }])
      setSelectedId(ws.id)
    } catch (e) {
      setManageError(e instanceof Error ? e.message : String(e))
    }
  }

  const breadcrumb = dirPath ? dirPath.split('/') : []
  const docTitles = new Map(docList.map((d) => [d.id, d.title]))
  const activeFile = openFile && openFile.wsId === selectedId ? openFile : null

  return (
    <div className="h-full grid" style={{ gridTemplateColumns: `${width}px 1fr` }}>
      <aside className="relative min-w-0 h-full flex flex-col border-r border-ink-100 bg-paper">
        <div className="px-4 pt-4 pb-2 text-[13px] font-semibold text-stone-800">{t('ws.title')}</div>
        <div className="flex-1 min-h-0 overflow-y-auto px-2 pb-3">
          {listError && <div className="px-2 py-1 text-[12.5px] text-coral-deep">{listError}</div>}
          {list.map((w) => (
            <button
              key={w.id}
              type="button"
              onClick={() => { if (guardDirty()) setSelectedId(w.id) }}
              className={cnRow(selectedId === w.id)}
              title={w.name}
            >
              <span className="truncate">{w.name}</span>
              {w.isDefault && (
                <span className="ml-1.5 shrink-0 rounded-full bg-skype/15 px-1.5 py-0.5 text-[10px] font-medium text-skype-deep">
                  {t('ws.default')}
                </span>
              )}
            </button>
          ))}
        </div>
        {canManage && (creatingWs ? (
          <div className="flex flex-col gap-1.5 border-t border-ink-100 px-3 py-2.5">
            <Input
              autoFocus
              value={newWsName}
              onChange={(e) => setNewWsName(e.target.value)}
              placeholder={t('ws.newWsNamePh')}
              className="text-[12.5px]"
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.nativeEvent.isComposing) void createWs()
                if (e.key === 'Escape') resetCreateWs()
              } }
            />
            <Input
              value={newWsFolder}
              onChange={(e) => setNewWsFolder(e.target.value)}
              placeholder={t('ws.newWsFolderPh')}
              className="font-mono text-[12px]"
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.nativeEvent.isComposing) void createWs()
                if (e.key === 'Escape') resetCreateWs()
              } }
            />
            <div className="flex items-center gap-2">
              <button type="button" onClick={() => void createWs()} className="rounded-lg bg-skype px-2.5 py-1 text-[12px] font-medium text-white">
                {t('ws.create')}
              </button>
              <button type="button" onClick={resetCreateWs} className="rounded-lg px-2 py-1 text-[12px] text-ink-500 hover:bg-cloud">
                {t('ws.cancel')}
              </button>
            </div>
          </div>
        ) : (
          <div className="border-t border-ink-100 px-3 py-2">
            <button
              type="button"
              onClick={() => setCreatingWs(true)}
              className="w-full rounded-lg px-2 py-1 text-left text-[12px] text-skype-deep hover:bg-cloud"
            >
              + {t('ws.newWs')}
            </button>
          </div>
        ))}
        <ResizeHandle onMouseDown={onResizeStart} />
      </aside>

      <main className="min-w-0 h-full flex flex-col bg-cloud">
        {!selectedId || (detailError && !detail) ? (
          <div className="h-full grid place-items-center text-sm text-ink-400">
            {detailError ?? t('ws.title')}
          </div>
        ) : !detail ? (
          <div className="h-full grid place-items-center text-sm text-ink-400">{t('common.loading')}</div>
        ) : (
          <>
            <header className="flex items-center gap-2 border-b border-ink-100 px-5 py-3">
              <span className="text-[15px] font-semibold text-stone-900">{detail.name}</span>
              {detail.isDefault && (
                <span className="rounded-full bg-skype/15 px-2 py-0.5 text-[10.5px] font-medium text-skype-deep">
                  {t('ws.default')}
                </span>
              )}
              {detail.unboundAt && (
                <span className="rounded-full bg-coral-soft px-2 py-0.5 text-[10.5px] font-medium text-coral-deep">
                  {t('ws.unbound')}
                </span>
              )}
              {detail.folderPath && (
                <span className="ml-auto max-w-[45%] truncate font-mono text-[11.5px] text-ink-400" title={detail.folderPath}>
                  {detail.folderPath}
                </span>
              )}
            </header>

            <div className="flex-1 min-h-0 grid" style={{ gridTemplateColumns: 'minmax(0, 1fr) 264px' }}>
              {/* Files */}
              <section className="min-w-0 h-full flex flex-col border-r border-ink-100">
                {detail.unboundAt ? (
                  <div className="flex-1 grid place-items-center px-6 text-center text-[13px] text-ink-400">
                    {t('ws.unbound')}
                  </div>
                ) : (
                  <>
                    <div className="flex items-center gap-1.5 border-b border-ink-100 px-4 py-2 text-[12px] text-ink-500">
                      <button
                        type="button"
                        disabled={!dirPath}
                        onClick={() => { if (guardDirty()) { closeFile(); setDirPath(parentDir(dirPath)) } }}
                        className="rounded-lg px-1.5 py-0.5 hover:bg-cloud disabled:opacity-40"
                      >
                        {t('ws.up')}
                      </button>
                      <span className="truncate font-mono text-ink-400">/{breadcrumb.join('/')}</span>
                      <input
                        value={nameFilter}
                        onChange={(e) => setNameFilter(e.target.value)}
                        placeholder={t('ws.filterPh')}
                        className="ml-auto w-28 rounded-md border border-ink-100 px-1.5 py-0.5 text-[11.5px] text-stone-700 outline-none focus:border-skype/50"
                      />
                      <input
                        ref={uploadRef}
                        type="file"
                        className="hidden"
                        onChange={(e) => { const f = e.target.files?.[0]; if (f) void upload(f) }}
                      />
                      <button
                        type="button"
                        disabled={uploading}
                        onClick={() => uploadRef.current?.click()}
                        className="rounded-lg px-2 py-0.5 text-skype-deep hover:bg-cloud disabled:opacity-40"
                        title={t('ws.upload')}
                      >
                        {uploading ? t('common.loading') : '↑'}
                      </button>
                      <button
                        type="button"
                        onClick={() => { if (guardDirty()) { closeFile(); setCreating(true) } }}
                        className="rounded-lg px-2 py-0.5 text-skype-deep hover:bg-cloud"
                      >
                        {t('ws.newFile')}
                      </button>
                    </div>

                    {fileError && <div className="border-b border-ink-100 px-4 py-1.5 text-[12px] text-coral-deep">{fileError}</div>}

                    {filesError ? (
                      <div className="px-4 py-3 text-[12.5px] text-coral-deep">{filesError}</div>
                    ) : creating ? (
                      <div className="flex items-center gap-2 px-4 py-2">
                        <Input
                          autoFocus
                          value={newPath}
                          onChange={(e) => setNewPath(e.target.value)}
                          placeholder={t('ws.newFilePh')}
                          className="flex-1"
                          onKeyDown={(e) => {
                            if (e.key === 'Enter' && !e.nativeEvent.isComposing) void createFile()
                            if (e.key === 'Escape') { setCreating(false); setNewPath('') }
                          } }
                        />
                        <button type="button" onClick={() => void createFile()} className="rounded-lg bg-skype px-2.5 py-1 text-[12px] font-medium text-white">
                          {t('ws.create')}
                        </button>
                        <button type="button" onClick={() => { setCreating(false); setNewPath('') }} className="rounded-lg px-2 py-1 text-[12px] text-ink-500 hover:bg-cloud">
                          {t('ws.cancel')}
                        </button>
                      </div>
                    ) : openImage && openImage.wsId === selectedId ? (
                      <div className="flex-1 min-h-0 flex flex-col">
                        <div className="flex items-center gap-2 border-b border-ink-100 px-4 py-1.5">
                          <span className="truncate font-mono text-[11.5px] text-ink-500" title={openImage.path}>{openImage.path}</span>
                          <span className="text-[11px] text-ink-400">{bytes(openImage.size)}</span>
                          <button
                            type="button"
                            onClick={closeFile}
                            className="ml-auto rounded-lg px-2 py-1 text-[12px] text-ink-500 hover:bg-cloud"
                          >
                            {t('ws.close')}
                          </button>
                        </div>
                        <div className="flex-1 min-h-0 overflow-auto grid place-items-center p-4">
                          <img src={openImage.url} alt={openImage.path} className="max-h-full max-w-full object-contain" />
                        </div>
                      </div>
                    ) : activeFile ? (
                      <div className="flex-1 min-h-0 flex flex-col">
                        <div className="flex items-center gap-2 border-b border-ink-100 px-4 py-1.5">
                          <span className="truncate font-mono text-[11.5px] text-ink-500" title={activeFile.path}>{activeFile.path}</span>
                          {savedTick > 0 && !editing && !dirty && (
                            <span className="text-[11px] text-skype-deep">{t('ws.saved')}</span>
                          )}
                          {editing ? (
                            <>
                              <button
                                type="button"
                                disabled={saving}
                                onClick={() => void save()}
                                className="ml-auto rounded-lg bg-skype px-2.5 py-1 text-[12px] font-medium text-white disabled:opacity-50"
                              >
                                {t('ws.save')}
                              </button>
                              <button
                                type="button"
                                onClick={() => { setEditing(false); setDraft(activeFile.body) }}
                                className="rounded-lg px-2 py-1 text-[12px] text-ink-500 hover:bg-cloud"
                              >
                                {t('ws.cancel')}
                              </button>
                            </>
                          ) : (
                            <button
                              type="button"
                              onClick={() => { setDraft(activeFile.body); setEditing(true); setSavedTick(0) }}
                              className="ml-auto rounded-lg px-2 py-1 text-[12px] text-skype-deep hover:bg-cloud"
                            >
                              {t('ws.edit')}
                            </button>
                          )}
                        </div>
                        <div className="flex-1 min-h-0 overflow-y-auto px-4 py-3">
                          {editing ? (
                            <TextArea
                              value={draft}
                              onChange={(e) => setDraft(e.target.value)}
                              className="h-full font-mono text-[12.5px] leading-[1.6]"
                              spellCheck={false}
                            />
                          ) : (
                            <FilePreview path={activeFile.path} body={activeFile.body} />
                          )}
                        </div>
                      </div>
                    ) : (
                      <div className="flex-1 min-h-0 overflow-y-auto px-2 py-2">
                        {filteredEntries === null ? null : filteredEntries.length === 0 ? (
                          <div className="px-2 py-2 text-[12.5px] text-ink-400">{t('ws.emptyDir')}</div>
                        ) : (
                          filteredEntries.map((e) => (
                            <button
                              key={e.name}
                              type="button"
                              onClick={() => void openEntry(e)}
                              title={e.name}
                              className="flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-[12.5px] text-stone-700 hover:bg-stone-50"
                            >
                              <span className={e.dir ? 'font-medium' : ''}>{e.dir ? '📁' : '📄'}</span>
                              <span className="truncate">{e.name}</span>
                              <span className="ml-auto shrink-0 text-[11px] text-ink-400">{e.dir ? '' : bytes(e.size)}</span>
                            </button>
                          ))
                        )}
                      </div>
                    )}
                  </>
                )}
              </section>

              {/* Member scope + associations */}
              <aside className="min-w-0 h-full overflow-y-auto px-4 py-3">
                {manageError && <div className="mb-2 rounded-md bg-coral-soft px-2 py-1 text-[11.5px] text-coral-deep">{manageError}</div>}
                <div className="text-[12px] font-semibold text-stone-800">{t('ws.members')}</div>
                <div className="mt-2 flex flex-col gap-1.5">
                  {detail.members.map((m) => (
                    <div key={m.participantId} className="flex items-center gap-2 text-[12.5px] text-stone-700" title={m.name}>
                      <span className="truncate">{m.name}</span>
                      {m.kind === 'agent' && <span className="shrink-0 text-[10.5px] text-ink-400">{t('common.agent')}</span>}
                      <span className={cnPill(m.source)} title={m.source === 'explicit' ? t('ws.explicit') : t('ws.implicit')}>
                        {m.source === 'explicit' ? t('ws.explicit') : t('ws.implicit')}
                      </span>
                      {canManage && m.source === 'explicit' && (
                        <button
                          type="button"
                          onClick={() => void removeMember(m.participantId)}
                          className="shrink-0 rounded px-1 text-[11px] text-ink-400 hover:text-coral-deep"
                          title={t('ws.removeMember')}
                        >
                          ✕
                        </button>
                      )}
                    </div>
                  ))}
                  {canManage && !detail.unboundAt && (addingMember ? (
                    <div className="flex items-center gap-1.5">
                      <Input
                        autoFocus
                        value={memberId}
                        onChange={(e) => setMemberId(e.target.value)}
                        placeholder={t('ws.memberIdPh')}
                        className="flex-1 text-[12px]"
                        onKeyDown={(e) => {
                          if (e.key === 'Enter' && !e.nativeEvent.isComposing) void addMember()
                          if (e.key === 'Escape') { setAddingMember(false); setMemberId('') }
                        } }
                      />
                      <button type="button" onClick={() => void addMember()} className="rounded-lg bg-skype px-2 py-0.5 text-[11.5px] font-medium text-white">{t('ws.add')}</button>
                      <button type="button" onClick={() => { setAddingMember(false); setMemberId('') }} className="rounded-lg px-1.5 py-0.5 text-[11.5px] text-ink-500 hover:bg-cloud">{t('ws.cancel')}</button>
                    </div>
                  ) : (
                    <button type="button" onClick={() => setAddingMember(true)} className="self-start rounded-lg px-1.5 py-0.5 text-[11.5px] text-skype-deep hover:bg-cloud">
                      + {t('ws.addMember')}
                    </button>
                  ))}
                </div>
                <div className="mt-5 text-[12px] font-semibold text-stone-800">{t('ws.associations')}</div>
                <div className="mt-2 flex flex-col gap-1.5">
                  {detail.associations.length === 0 ? (
                    <div className="text-[12.5px] text-ink-400">{t('ws.none')}</div>
                  ) : (
                    detail.associations.map((a, i) => (
                      <div key={`${a.kind}:${a.targetId}:${i}`} className="flex items-center gap-2 text-[12.5px] text-stone-700" title={a.targetId}>
                        <span className="shrink-0 rounded-md bg-stone-100 px-1.5 py-0.5 text-[10.5px] text-stone-600">
                          {a.kind === 'project' ? t('ws.kindProject') : a.kind === 'board_card' ? t('ws.kindBoardCard') : t('ws.kindDocument')}
                        </span>
                        <span className="truncate">
                          {a.kind === 'document' ? (docTitles.get(a.targetId) ?? a.targetId) : a.targetId}
                        </span>
                        {canManage && !detail.unboundAt && (
                          <button
                            type="button"
                            onClick={() => void removeLink(a.kind, a.targetId)}
                            className="shrink-0 rounded px-1 text-[11px] text-ink-400 hover:text-coral-deep"
                            title={t('ws.removeLink')}
                          >
                            ✕
                          </button>
                        )}
                      </div>
                    ))
                  )}
                  {canManage && !detail.unboundAt && (addingLink ? (
                    <div className="flex flex-col gap-1.5">
                      <div className="flex items-center gap-1.5">
                        <select
                          value={linkKind}
                          onChange={(e) => setLinkKind(e.target.value as 'project' | 'board_card' | 'document')}
                          className="rounded-md border border-ink-100 px-1.5 py-1 text-[11.5px] text-stone-700 outline-none"
                        >
                          <option value="project">{t('ws.kindProject')}</option>
                          <option value="board_card">{t('ws.kindBoardCard')}</option>
                          <option value="document">{t('ws.kindDocument')}</option>
                        </select>
                        <Input
                          autoFocus
                          value={linkTarget}
                          onChange={(e) => setLinkTarget(e.target.value)}
                          placeholder={t('ws.targetIdPh')}
                          className="flex-1 font-mono text-[11.5px]"
                          onKeyDown={(e) => {
                            if (e.key === 'Enter' && !e.nativeEvent.isComposing) void addLink()
                            if (e.key === 'Escape') { setAddingLink(false); setLinkTarget('') }
                          } }
                        />
                      </div>
                      <div className="flex items-center gap-1.5">
                        <button type="button" onClick={() => void addLink()} className="rounded-lg bg-skype px-2 py-0.5 text-[11.5px] font-medium text-white">{t('ws.add')}</button>
                        <button type="button" onClick={() => { setAddingLink(false); setLinkTarget('') }} className="rounded-lg px-1.5 py-0.5 text-[11.5px] text-ink-500 hover:bg-cloud">{t('ws.cancel')}</button>
                      </div>
                    </div>
                  ) : (
                    <button type="button" onClick={() => setAddingLink(true)} className="self-start rounded-lg px-1.5 py-0.5 text-[11.5px] text-skype-deep hover:bg-cloud">
                      + {t('ws.addLink')}
                    </button>
                  ))}
                </div>
                {canManage && !detail.isDefault && !detail.unboundAt && (
                  <button
                    type="button"
                    onClick={() => void unbind()}
                    className="mt-6 w-full rounded-lg border border-coral-deep/30 px-2 py-1.5 text-[12px] text-coral-deep hover:bg-coral-soft"
                  >
                    {t('ws.unbind')}
                  </button>
                )}
              </aside>
            </div>
          </>
        )}
      </main>
    </div>
  )
}

function cnRow(active: boolean): string {
  return `w-full flex items-center rounded-lg px-2.5 py-1.5 text-left text-[13px] transition-colors ${
    active ? 'bg-skype/10 text-skype-deep' : 'hover:bg-stone-50 text-stone-700'
  }`
}

function cnPill(source: 'explicit' | 'implicit'): string {
  return `ml-auto shrink-0 rounded-full px-1.5 py-0.5 text-[10px] font-medium ${
    source === 'explicit' ? 'bg-gold/20 text-gold-deep' : 'bg-skype/10 text-skype-deep'
  }`
}
