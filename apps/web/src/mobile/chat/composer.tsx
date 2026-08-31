// 输入区件(#219 ⑤)—— 从 MobileChat.tsx 原样搬移:
// MentionEntry(@all/成员 tagged union)+ Composer(两排输入区:附件条/
// 上传态/回复条/提及选择器/RichInput+发送/工具排/表情面板)。
// JSX 体仅去一级缩进;草稿、附件、提及、表情等全部状态仍由壳持有,
// 经 props 透传(形参名刻意与壳的变量同名,JSX 体逐字节不变)。
import type React from 'react'
import type { Dispatch, RefObject, SetStateAction } from 'react'
import type { ApiAttachment } from '@/api/client'
import { Avatar } from '@/components/Avatar'
import { IAt, IClip, ISend, ISmile } from '@/components/icons'
import { TypingRow } from '@/components/Message'
import { RichInput, type RichInputHandle } from '@/components/RichInput'
import { SkypeEmoji } from '@/components/SkypeEmoji'
import { TwEmoji } from '@/components/TwEmoji'
import { COMPOSER_EMOJIS } from '@/lib/emoji'
import { useT } from '@/lib/i18n'
import { SKYPE_EMOJIS } from '@/lib/skypeEmojis'
import { cn } from '@/lib/utils'
import type { Message, Participant } from '@/types'
import { Pressable } from '../Pressable'

// Picker shows the broadcast token `@all` alongside individual members.
// Tagged union so insert / keyboard nav can tell them apart without a
// sentinel id collision (a participant could theoretically be named "all").
export type MentionEntry = { kind: 'all' } | { kind: 'participant'; p: Participant }

/** 输入区装配所需的壳侧依赖(状态全部留在壳,此处仅透传)。 */
interface ComposerProps {
  convoId: string
  draft: string
  typingNames: string[]
  attachment: ApiAttachment | null
  setAttachment: Dispatch<SetStateAction<ApiAttachment | null>>
  uploading: boolean
  uploadError: string | null
  replyingToId: string | undefined
  replyingToMsg: Message | undefined
  setReplyingTo: (convoId: string, messageId: string | null) => void
  byId: Record<string, Participant>
  meId: string | null
  mention: { start: number; query: string } | null
  filteredMentions: MentionEntry[]
  mentionIndex: number
  insertMention: (entry: MentionEntry) => void
  fileRef: RefObject<HTMLInputElement>
  onPickFile: (e: React.ChangeEvent<HTMLInputElement>) => Promise<void>
  editorRef: RefObject<RichInputHandle>
  setDraft: (text: string) => void
  updateMention: (text: string, caret: number) => void
  scrollToLatest: () => void
  setMention: Dispatch<SetStateAction<{ start: number; query: string } | null>>
  onKey: (e: React.KeyboardEvent<HTMLDivElement>) => void
  canSend: boolean
  send: () => void
  openMentionByButton: () => void
  emojiOpen: boolean
  setEmojiOpen: Dispatch<SetStateAction<boolean>>
  emojiTab: 'std' | 'skype'
  setEmojiTab: Dispatch<SetStateAction<'std' | 'skype'>>
  insertAtCursor: (s: string) => void
}

export function Composer({
  convoId, draft, typingNames, attachment, setAttachment, uploading, uploadError,
  replyingToId, replyingToMsg, setReplyingTo, byId, meId,
  mention, filteredMentions, mentionIndex, insertMention,
  fileRef, onPickFile, editorRef, setDraft, updateMention, scrollToLatest, setMention, onKey,
  canSend, send, openMentionByButton, emojiOpen, setEmojiOpen, emojiTab, setEmojiTab, insertAtCursor,
}: ComposerProps) {
  const t = useT()
  return (
    <div
      className="border-t border-ink-100 bg-cloud px-3 pt-1.5 kb-aware"
    >
      <div className="px-1 pb-1">
        <TypingRow names={typingNames} />
      </div>
      {attachment && (
        <div className="mb-2 inline-flex items-center gap-2.5 py-1.5 px-2 bg-sky2-50 border border-sky2-100 rounded-lg max-w-full">
          {attachment.kind === 'img' ? (
            <img src={attachment.url} alt={attachment.name}
              className="w-10 h-10 object-cover rounded-md shrink-0" />
          ) : (
            <div className="w-10 h-10 rounded-md grid place-items-center shrink-0"
              style={{ background: 'linear-gradient(135deg, #2A2A35, #1A1A22)' }}>
              <IClip className="w-4 h-4 text-white/85" strokeWidth={1.8} />
            </div>
          )}
          <div className="min-w-0">
            <div className="text-[12px] font-semibold text-ink-700 truncate max-w-[220px]">{attachment.name}</div>
            <div className="text-[10.5px] text-ink-500 truncate">{attachment.mime ?? attachment.kind}{attachment.size ? ` · ${Math.round(attachment.size / 1024)}KB` : ''}</div>
          </div>
          <button
            onClick={() => setAttachment(null)}
            className="ml-1 w-6 h-6 rounded-md grid place-items-center text-ink-500 active:bg-cloud transition shrink-0"
            aria-label={t('mobchat.removeAttachment')}
          >×</button>
        </div>
      )}
      {uploading && (
        <div className="mb-2 text-[11.5px] text-ink-500 italic">{t('mobchat.uploading')}</div>
      )}
      {uploadError && (
        <div className="mb-2 text-[11.5px] py-1 px-2 rounded-md text-coral-deep bg-coral-soft inline-block max-w-full truncate">
          {uploadError}
        </div>
      )}
      {replyingToId && convoId && (
        <div className="mb-2 flex items-stretch gap-2 rounded-md bg-sky2-50 border border-sky2-100 pl-2 pr-1 py-1.5">
          <div className="w-[3px] rounded bg-skype shrink-0" />
          <div className="min-w-0 flex-1 flex flex-col gap-0.5">
            <div className="text-[10.5px] font-bold uppercase tracking-wider text-skype-deep">
              {t('mobchat.replyingTo', { name: byId[replyingToMsg?.authorId ?? '']?.name ?? replyingToMsg?.authorId ?? '…' })}
            </div>
            <div className="text-[12px] text-ink-500 truncate">
              {replyingToMsg ? replyingToMsg.body.slice(0, 140).replace(/\n/g, ' ') : t('mobchat.replyLoading')}
            </div>
          </div>
          <button
            onClick={() => setReplyingTo(convoId, null)}
            className="w-6 h-6 rounded-md grid place-items-center text-ink-500 active:bg-cloud transition shrink-0 self-center"
            aria-label={t('mobchat.cancelReply')}
          >×</button>
        </div>
      )}
      {/* Mention picker. Lives ABOVE the composer row so the keyboard +
          textarea stay anchored at the bottom and the picker grows up.
          onMouseDown preventDefault keeps the textarea from blurring
          when a row is tapped (which would close the picker first). */}
      {mention && filteredMentions.length > 0 && (
        <div
          className="mb-2 rounded-[12px] bg-paper animate-rise overflow-hidden"
          style={{
            border: '1px solid var(--ink-100)',
            boxShadow: '0 12px 28px -8px rgba(10, 30, 60, 0.20), 0 4px 10px -4px rgba(10, 30, 60, 0.12)',
            maxHeight: 240,
            overflowY: 'auto',
          }}
          onMouseDown={(e) => e.preventDefault()}
        >
          <div className="px-3 pt-2 pb-1 text-[10px] font-bold uppercase tracking-[0.12em] text-ink-300">
            {mention.query ? `${t('mobchat.mentionHeader')} ${t('mobchat.mentionQuerySuffix', { query: mention.query })}` : t('mobchat.mentionHeader')}
          </div>
          {filteredMentions.map((entry, i) => {
            const active = i === mentionIndex
            if (entry.kind === 'all') {
              return (
                <button
                  key="__all"
                  type="button"
                  onClick={() => insertMention(entry)}
                  className={cn(
                    'w-full text-left flex items-center gap-2.5 py-2 px-3 transition',
                    active ? 'bg-sky2-50' : 'active:bg-sky2-50',
                  )}
                >
                  <img src="/everyone.png" alt="" className="w-[28px] h-[28px] rounded-full object-cover" />
                  <div className="flex-1 min-w-0">
                    <div className="text-[13px] font-semibold text-ink-900 truncate">{t('mobchat.everyone')}</div>
                    <div className="text-[11px] text-ink-500 truncate">{t('mobchat.notifyAll')}</div>
                  </div>
                </button>
              )
            }
            const p = entry.p
            return (
              <button
                key={p.id}
                type="button"
                onClick={() => insertMention(entry)}
                className={cn(
                  'w-full text-left flex items-center gap-2.5 py-2 px-3 transition',
                  active ? 'bg-sky2-50' : 'active:bg-sky2-50',
                )}
              >
                <Avatar p={p} size={28} ringColor="var(--paper)" showStatus={false} />
                <div className="flex-1 min-w-0">
                  <div className="text-[13px] font-semibold text-ink-900 truncate">{p.name}</div>
                  <div className="text-[11px] text-ink-500 truncate">@{p.id}{p.role ? ` · ${p.role}` : ''}</div>
                </div>
              </button>
            )
          })}
        </div>
      )}
      <input
        ref={fileRef}
        type="file"
        // No `accept` filter — server's MIME whitelist is the source
        // of truth. Browser-side accept is only a hint anyway.
        className="hidden"
        onChange={onPickFile}
      />
      {/* Two-row composer: textarea + send on top so the input gets
          the full width, action buttons (attach / mention) below. */}
      <div className="flex items-end gap-2">
        <div
          className="flex-1 bg-paper rounded-[20px] py-2 px-3.5 min-h-[40px] flex items-center"
          style={{ border: '1px solid var(--ink-100)' }}
        >
          <RichInput
            key={convoId}
            ref={editorRef}
            defaultValue={draft}
            placeholder={t('mobchat.typeAtToSummon')}
            ariaLabel={t('mobchat.composerAria')}
            className="rich-input flex-1 whitespace-pre-wrap bg-transparent outline-none text-[14px] text-ink-900 leading-[1.4]"
            style={{ minHeight: '1.4em' }}
            maxHeight={120}
            enterKeyHint="send"
            autoCapitalize="sentences"
            autoCorrect="on"
            onChange={(value, caret) => {
              setDraft(value)
              updateMention(value, caret)
            }}
            onFocus={() => {
              // Snap on focus AND on visualViewport resize (the
              // useEffect above). Focus runs immediately, the
              // resize runs once the keyboard finishes animating
              // in — together they cover both fast and slow
              // keyboard paths across iOS versions.
              requestAnimationFrame(scrollToLatest)
            }}
            onBlur={() => setTimeout(() => setMention(null), 120)}
            onKeyDown={onKey}
            resolveMention={(id) => {
              if (id === 'all') {
                return { name: 'all', initial: '@', avatarBg: 'var(--ink-300)', kind: 'human' }
              }
              const p = byId[id]
              if (!p) return null
              return {
                name: p.id === meId ? 'you' : p.name,
                initial: p.initial || p.name.charAt(0).toUpperCase(),
                avatarBg: typeof p.avatarBg === 'string' ? p.avatarBg : 'var(--ink-300)',
                kind: p.kind,
                avatarUrl: typeof p.avatarUrl === 'string' ? p.avatarUrl : undefined,
              }
            }}
          />
        </div>
        <Pressable
          onClick={send}
          disabled={!canSend}
          scale={0.9}
          className="w-10 h-10 grid place-items-center rounded-full text-white shrink-0 disabled:opacity-40"
          style={{
            background: canSend
              ? 'linear-gradient(135deg, var(--skype), var(--skype-deep))'
              : 'var(--ink-200)',
            boxShadow: canSend ? '0 4px 12px -3px rgba(0, 168, 240, 0.5)' : 'none',
          }}
          aria-label={t('mobchat.sendAria')}
        >
          <ISend className="w-[18px] h-[18px]" strokeWidth={2} />
        </Pressable>
      </div>
      <div className="flex items-center gap-1 pt-1.5 pb-0.5">
        <Pressable
          onClick={() => fileRef.current?.click()}
          className="w-9 h-9 grid place-items-center text-ink-500 rounded-full active:bg-sky2-50"
          aria-label={t('mobchat.attachFile')}
        >
          <IClip className="w-[20px] h-[20px]" />
        </Pressable>
        <Pressable
          onMouseDown={(e) => e.preventDefault()}
          onClick={openMentionByButton}
          className={cn(
            'w-9 h-9 grid place-items-center rounded-full active:bg-sky2-50',
            mention ? 'text-skype-deep bg-sky2-50' : 'text-ink-500',
          )}
          aria-label={t('mobchat.mentionAria')}
        >
          <IAt className="w-[20px] h-[20px]" />
        </Pressable>
        <Pressable
          onMouseDown={(e) => e.preventDefault()}
          onClick={() => setEmojiOpen((v) => !v)}
          className={cn(
            'w-9 h-9 grid place-items-center rounded-full active:bg-sky2-50',
            emojiOpen ? 'text-skype-deep bg-sky2-50' : 'text-ink-500',
          )}
          aria-label={t('mobchat.emojiAria')}
        >
          <ISmile className="w-[20px] h-[20px]" />
        </Pressable>
      </div>
      {/* Emoji picker — inline above the composer's tool row so the
          keyboard doesn't cover it. Tabs match the desktop popover
          (Standard Twemoji + Skype emoticons). */}
      {emojiOpen && (
        <div
          className="mt-2 mb-1 rounded-[12px] bg-paper overflow-hidden animate-rise"
          style={{
            border: '1px solid var(--ink-100)',
            boxShadow: '0 -8px 20px -8px rgba(10, 30, 60, 0.10)',
          }}
          onMouseDown={(e) => e.preventDefault()}
        >
          <div className="flex gap-1 px-2 pt-2">
            {(['std', 'skype'] as const).map((k) => (
              <button
                key={k}
                onClick={() => setEmojiTab(k)}
                className={cn(
                  'flex-1 text-[11px] font-semibold uppercase tracking-wider py-1.5 rounded-[6px] transition',
                  emojiTab === k ? 'bg-sky2-100 text-skype-deep' : 'text-ink-500 active:bg-sky2-50',
                )}
              >{k === 'std' ? t('mobchat.emojiStd') : t('mobchat.emojiSkype')}</button>
            ))}
          </div>
          <div className="px-2 py-2 max-h-[220px] overflow-y-auto">
            {emojiTab === 'std' ? (
              <div className="grid grid-cols-8 gap-0.5">
                {COMPOSER_EMOJIS.map((e) => (
                  <button
                    key={e}
                    onClick={() => insertAtCursor(e)}
                    className="h-9 grid place-items-center rounded active:bg-sky2-50 transition"
                    aria-label={e}
                  ><TwEmoji emoji={e} size={22} /></button>
                ))}
              </div>
            ) : (
              <div className="grid grid-cols-7 gap-0.5">
                {SKYPE_EMOJIS.map((e) => (
                  <button
                    key={e.key}
                    onClick={() => insertAtCursor(e.shortcodes[0])}
                    className="h-10 grid place-items-center rounded active:bg-sky2-50 transition"
                    aria-label={e.label}
                  ><SkypeEmoji name={e.key} size={28} autoPlaySound={false} /></button>
                ))}
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}
