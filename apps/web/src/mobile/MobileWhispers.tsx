/**
 * MobileWhisperRoom —— 纯 agent 会话的移动端房间(#370 刀3:列表/独立视图
 * 退役,入口 = 主列表「Agent 对话」分区,选中 id 复用
 * selectedConversationId,由 MobileApp 的聊天覆盖层换渲染本组件)。
 * 数据源 /peek/agent-chats;块渲染器与桌面 WhisperRoom 对齐。
 */
import { useEffect, useMemo } from 'react'
import { Pressable } from './Pressable'
import { useT } from '@/lib/i18n'
import { useWhispers, whisperMessages, type WhispersStateLike } from '@/stores/whispers'
import { useParticipants } from '@/stores/participants'
import { Avatar, AvatarStack } from '@/components/Avatar'
import { CodeBlock, SystemRow } from '@/components/Message'
import { BoardLink } from '@/components/BoardLink'
import { CardLink } from '@/components/CardLink'
import { DocumentLink } from '@/components/DocumentLink'
import { CalendarLink } from '@/components/CalendarLink'
import { SkypeEmoji } from '@/components/SkypeEmoji'
import { TwEmoji } from '@/components/TwEmoji'
import { IBack, IMore } from '@/components/icons'
import { parseBody, parseBlocks } from '@/lib/utils'
import type { ApiWhisperMessage } from '@/api/client'
import type { Participant } from '@/types'

function WhisperInline({ body }: { body: string }) {
  const tokens = parseBody(body)
  const byId = useParticipants((s) => s.byId)
  return (
    <>
      {tokens.map((t, i) => {
        if (t.kind === 'text') return <span key={i}>{t.value}</span>
        if (t.kind === 'document') return <DocumentLink key={i} id={t.id} />
        if (t.kind === 'board') return <BoardLink key={i} id={t.id} />
        if (t.kind === 'card') return <CardLink key={i} id={t.id} />
        if (t.kind === 'calendar') return <CalendarLink key={i} id={t.id} />
        if (t.kind === 'link') return (
          <a
            key={i}
            href={t.url}
            target="_blank"
            rel="noopener noreferrer"
            className="break-all underline decoration-coral/40 decoration-1 underline-offset-2"
            style={{ color: 'var(--coral-deep)' }}
          >{t.text}</a>
        )
        if (t.kind === 'bold') return <strong key={i} className="text-ink-900 font-semibold">{t.value}</strong>
        if (t.kind === 'code') return (
          <code
            key={i}
            className="font-mono text-[12.5px] py-px px-1.5 rounded-[5px] mx-px align-[0.05em]"
            style={{
              background: 'rgba(15, 30, 50, 0.06)',
              color: 'var(--ink-900)',
              border: '1px solid rgba(15, 30, 50, 0.08)',
            }}
          >{t.value}</code>
        )
        if (t.kind === 'emoji') return <TwEmoji key={i} emoji={t.value} size={16} />
        if (t.kind === 'skype') return <SkypeEmoji key={i} name={t.name} size={18} />
        // `#N` is a peek-only view here — no jump target, render as plain text.
        if (t.kind === 'msgref') return <span key={i}>#{t.n}</span>
        const p = byId[t.id]
        const label = p?.name ?? t.id
        return (
          <span key={i} className="px-1.5 rounded font-semibold border-b-[1.5px] border-dashed"
            style={{
              background: 'linear-gradient(135deg, var(--coral-soft), rgba(255, 217, 210, 0.5))',
              color: '#B23A2A',
              borderColor: 'var(--coral)',
            }}>@{label}</span>
        )
      })}
    </>
  )
}

function WhisperBody({ body }: { body: string }) {
  const blocks = parseBlocks(body)
  return (
    <>
      {blocks.map((b, i) => {
        if (b.kind === 'code-block') return <CodeBlock key={i} lang={b.lang} code={b.code} />
        return (
          <div key={i}>
            {b.text.split('\n').map((line, li) => (
              <div key={li}>{line ? <WhisperInline body={line} /> : ' '}</div>
            ))}
          </div>
        )
      })}
    </>
  )
}

function Bubble({ msg }: { msg: ApiWhisperMessage }) {
  const byId = useParticipants((s) => s.byId)
  if (msg.kind === 'system') return <SystemRow msg={msg} />
  if (msg.kind === 'tool') return null
  if (!msg.body || msg.body.trim() === '') return null
  const author = byId[msg.authorId]
  if (!author) return null

  const tsRaw = msg.createdAt ?? (msg as { at?: string }).at
  const tsDate = tsRaw ? new Date(tsRaw) : new Date()
  const tsLabel = Number.isNaN(tsDate.getTime())
    ? ''
    : tsDate.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })

  return (
    <div className="grid grid-cols-[28px_1fr] gap-2 items-start animate-rise mr-[12%]">
      <Avatar p={author} size={28} showStatus={false} />
      <div>
        <div className="text-[10.5px] mb-1 flex gap-1.5">
          <span className="font-bold text-ink-900 text-[11.5px]">{author.name}</span>
          {tsLabel && <span className="text-ink-300">{tsLabel}</span>}
        </div>
        <div className="inline-block py-2 px-3 text-[13px] leading-[1.55] rounded-tl-[4px] rounded-tr-[14px] rounded-br-[14px] rounded-bl-[14px]"
          style={{
            background: 'rgba(255, 255, 255, 0.85)',
            border: '1px solid var(--whisper-100)',
            color: 'var(--ink-700)',
          }}>
          <WhisperBody body={msg.body} />
        </div>
      </div>
    </div>
  )
}

export function MobileWhisperRoom({ pairId, onBack }: { pairId: string; onBack: () => void }) {
  const t = useT()
  const list = useWhispers((s) => s.list)
  const byIdList = useWhispers((s) => s.byId)
  const streaming = useWhispers((s) => s.streaming)
  const whisper = useMemo(() => list.find((w) => w.id === pairId), [list, pairId])
  const messages = useMemo(
    () => whisperMessages({ byId: byIdList, streaming } as WhispersStateLike, pairId),
    [byIdList, streaming, pairId],
  )
  const byId = useParticipants((s) => s.byId)

  useEffect(() => {
    void useWhispers.getState().loadMessages(pairId)
  }, [pairId])

  if (!whisper) {
    return (
      <section className="flex flex-col h-full bg-paper">
        <header className="sticky top-0 z-10 backdrop-blur-md border-b border-whisper-100"
          style={{ paddingTop: 'env(safe-area-inset-top)', background: 'rgba(251, 253, 251, 0.95)' }}>
          <div className="px-2 py-2.5 flex items-center gap-2">
            <Pressable onClick={onBack} className="w-10 h-10 grid place-items-center text-ink-700 active:bg-whisper-50 rounded-full">
              <IBack className="w-[22px] h-[22px]" strokeWidth={2} />
            </Pressable>
          </div>
        </header>
        <div className="flex-1 grid place-items-center text-center px-6 text-[13px] text-ink-500 font-display italic">
          {t('mwhisp.closed')}
        </div>
      </section>
    )
  }

  const ms = whisper.members
    .map((id) => byId[id])
    .filter((p): p is Participant => Boolean(p))
  if (ms.length < 2) return null
  const isGroup = whisper.kind === 'group' || ms.length > 2

  return (
    <section className="flex flex-col h-full"
      style={{ background: 'radial-gradient(ellipse 80% 60% at 50% 0%, rgba(210, 201, 233, 0.45), transparent 60%), linear-gradient(180deg, #FBFAFE 0%, #F1ECF8 100%)' }}>
      <header className="sticky top-0 z-10 backdrop-blur-md border-b border-whisper-100"
        style={{ paddingTop: 'env(safe-area-inset-top)', background: 'rgba(251, 253, 251, 0.95)' }}>
        <div className="px-2 py-2.5 flex items-center gap-2">
          <Pressable onClick={onBack} className="w-10 h-10 grid place-items-center text-ink-700 active:bg-whisper-50 rounded-full">
            <IBack className="w-[22px] h-[22px]" strokeWidth={2} />
          </Pressable>
          <div className="flex items-center gap-2 flex-1 min-w-0">
            {isGroup ? (
              <AvatarStack ps={ms} size={28} max={3} />
            ) : (
              <div className="flex">
                <Avatar p={ms[0]} size={28} showStatus={false} ringColor="var(--paper)" />
                <div className="-ml-2"><Avatar p={ms[1]} size={28} showStatus={false} ringColor="var(--paper)" /></div>
              </div>
            )}
            <div className="min-w-0 flex-1">
              <div className="font-display font-medium text-[15px] text-ink-900 leading-tight truncate" style={{ letterSpacing: '-0.01em' }}>
                {isGroup ? (whisper.title || ms.map((m) => m.name).join(', ')) : (
                  <>
                    {ms[0].name} <em className="italic text-whisper-deep" style={{ fontWeight: 400 }}>↔</em> {ms[1].name}
                  </>
                )}
              </div>
              <div className="text-[10.5px] text-whisper-deep font-semibold mt-0.5 flex items-center gap-1">
                <span className="w-1.5 h-1.5 rounded-full bg-whisper animate-pulse-soft" />
                {t('mwhisp.observingCount', { n: whisper.msgCount })}
              </div>
            </div>
          </div>
          <Pressable haptic="medium" className="w-10 h-10 grid place-items-center text-ink-700 active:bg-whisper-50 rounded-full">
            <IMore className="w-[20px] h-[20px]" />
          </Pressable>
        </div>

        <div className="py-2 px-3 flex items-center gap-2 border-t border-whisper-100"
          style={{ background: 'linear-gradient(90deg, rgba(123, 108, 176, 0.10), rgba(123, 108, 176, 0.03))' }}>
          <div className="w-6 h-6 rounded-full grid place-items-center text-whisper" style={{ background: 'rgba(123, 108, 176, 0.18)' }}>
            <svg width="12" height="12" fill="none" stroke="currentColor" strokeWidth={2} viewBox="0 0 24 24"><path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z" /><circle cx="12" cy="12" r="3" /></svg>
          </div>
          <div className="text-[11px] text-whisper-deep flex-1">
            <b className="font-bold tracking-wider uppercase text-[10px]">{t('mwhisp.observerMode')}</b>
            <span className="font-display italic font-normal text-ink-500 ml-1.5">{t('mwhisp.observerSub')}</span>
          </div>
        </div>
      </header>

      <div className="flex-1 overflow-y-auto py-4 px-3 flex flex-col gap-3 relative">
        {messages.length === 0 && (
          <div className="text-center text-ink-300 text-[12px] font-display italic py-6">{t('mwhisp.previewNone')}</div>
        )}
        {messages.map((msg) => <Bubble key={msg.id} msg={msg} />)}
      </div>

      <div className="border-t border-whisper-100 bg-cloud px-3 pt-2.5 flex items-center gap-2 kb-aware">
        <div className="flex-1 bg-paper rounded-[20px] py-2.5 px-3.5 min-h-[40px] flex items-center"
          style={{ border: '1px solid var(--whisper-100)' }}>
          <span className="text-[13px] text-ink-300 italic flex-1">{t('mwhisp.inject')}</span>
        </div>
      </div>
    </section>
  )
}
