import { createContext, memo, type ReactNode, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import Markdown, { type Components } from 'react-markdown'
import remarkBreaks from 'remark-breaks'
import remarkGfm from 'remark-gfm'
import { useT } from '@/lib/i18n'
import { remarkCumora } from '@/lib/remarkCumora'
import { cn } from '@/lib/utils'
import { useApp } from '@/stores/app'
import { useMe } from '@/stores/auth'
import { useMessages } from '@/stores/messages'
import { useParticipants } from '@/stores/participants'
import type { Message, Participant } from '@/types'
import { Avatar } from '../Avatar'
import { BoardLink } from '../BoardLink'
import { CalendarLink } from '../CalendarLink'
import { CardLink } from '../CardLink'
import { DocumentLink } from '../DocumentLink'
import { SkypeEmoji } from '../SkypeEmoji'
import { TwEmoji } from '../TwEmoji'





function MentionChip({ id }: { id: string }) {
  const t = useT()
  const byId = useParticipants((s) => s.byId)
  const openAgentInfo = useApp((s) => s.openAgentInfo)
  const meId = useMe()
  const [hoverPos, setHoverPos] = useState<{ x: number; y: number } | null>(null)
  const ref = useRef<HTMLSpanElement | null>(null)

  // `@all` is a broadcast token — no participant to resolve. Renders as a
  // coral chip with the shared community portrait inline, so it visually
  // matches participant mention chips (avatar + label) while still reading
  // as "addressed to the whole room".
  if (id === 'all') {
    return (
      <span
        className="inline-flex items-center justify-center gap-1 px-1.5 py-0.5 rounded-full font-semibold text-coral-deep bg-coral-soft"
        style={{ verticalAlign: '-0.15em' }}
      >
        <img src="/everyone.png" alt="" className="w-4 h-4 rounded-full object-cover" />
        <span style={{ lineHeight: '16px' }}>{t('msgview.atAll')}</span>
      </span>
    )
  }

  const p = byId[id]
  // Unknown reference — render as plain `@id` without chip styling, so
  // typos don't get fake-validated. Never resolves the id to a guessed name.
  if (!p) return <span className="text-ink-500">@{id}</span>

  const isMe = p.id === meId
  const isAgent = p.kind === 'agent'
  const label = isMe ? 'you' : p.name

  const enter = () => {
    if (!ref.current) return
    const r = ref.current.getBoundingClientRect()
    setHoverPos({ x: r.left + r.width / 2, y: r.bottom + 6 })
  }
  const leave = () => setHoverPos(null)
  // Open InfoPane for any participant — humans now have profile cards too
  // (their auth email is the most useful new piece). Self-mentions still
  // skip — clicking your own @you mention shouldn't open your own profile.
  const click = () => { if (!isMe) openAgentInfo(p.id) }

  return (
    <>
      <span
        ref={ref}
        onMouseEnter={enter}
        onMouseLeave={leave}
        onClick={click}
        className={cn(
          // Symmetric horizontal padding (avatar + text feel balanced in the
          // chip instead of the avatar grazing the left edge), 4px gap so
          // the @ sign reads as paired with the avatar. The chip is taller
          // than the 14px text line, so we pin its vertical-align with an em
          // offset to center it on the surrounding CJK glyphs. -0.15em was
          // measured against 14px/leading-1.55 CJK text (see RichInput, which
          // uses the same value): `baseline`/`middle` ride high, -0.25em (the
          // old value) sat visibly low. Keep this in sync with RichInput.
          'inline-flex items-center justify-center gap-1 px-1.5 py-0.5 rounded-full font-semibold cursor-pointer transition',
          isAgent ? 'text-skype-deep bg-sky-50 hover:bg-sky-100'
                  : 'text-coral-deep bg-coral-soft hover:brightness-95',
        )}
        style={{ verticalAlign: '-0.15em' }}
      >
        <Avatar p={p} size={16} ringColor="var(--cloud)" showStatus={false} />
        {/* Wrap the label in its own inline-flex box so the parent chip's
         *  \`items-center\` aligns the avatar's geometric center with the
         *  label's geometric center — not with the label's default line
         *  box, which positions glyphs LOWER than its midpoint because of
         *  the font's ascender/descender split. The inner flex re-centers
         *  the glyphs vertically within a 16-px tall slot that matches
         *  the avatar height. */}
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            height: 16,
            lineHeight: 1,
          }}
        >@{label}</span>
      </span>
      {hoverPos && createPortal(
        <MentionCard p={p} x={hoverPos.x} y={hoverPos.y} />,
        document.body,
      )}
    </>
  )
}

/** Elegant floating preview card shown on @mention hover. Renders via
 *  portal so it escapes scroll-container clipping. */
function MentionCard({ p, x, y }: { p: Participant; x: number; y: number }) {
  const t = useT()
  const ref = useRef<HTMLDivElement | null>(null)
  const [adjusted, setAdjusted] = useState<{ left: number; top: number } | null>(null)
  useLayoutEffect(() => {
    if (!ref.current) return
    const r = ref.current.getBoundingClientRect()
    // Center horizontally on the anchor; flip up if it would clip the bottom
    let left = x - r.width / 2
    let top = y
    const margin = 8
    if (left < margin) left = margin
    if (left + r.width > window.innerWidth - margin) left = window.innerWidth - r.width - margin
    if (top + r.height > window.innerHeight - margin) top = y - r.height - 18  // flip above the anchor
    setAdjusted({ left, top })
  }, [x, y])
  const role = p.role || (p.kind === 'human' ? t('mobchat.humanTeammate') : t('common.agent'))
  return (
    <div
      ref={ref}
      className="fixed z-[60] animate-rise"
      style={{
        left: adjusted?.left ?? x,
        top: adjusted?.top ?? y,
        visibility: adjusted ? 'visible' : 'hidden',
        pointerEvents: 'none',
      }}
    >
      <div
        className="bg-cloud rounded-[14px] py-3 px-3.5 flex items-start gap-3 min-w-[240px] max-w-[300px]"
        style={{
          border: '1px solid var(--ink-100)',
          boxShadow: '0 12px 32px -10px rgba(10, 30, 60, 0.22), 0 6px 14px -6px rgba(10, 30, 60, 0.14)',
        }}
      >
        <Avatar p={p} size={44} ringColor="var(--cloud)" showStatus={false} />
        <div className="min-w-0 flex-1">
          <div className="font-semibold text-[14px] text-ink-900 truncate">{p.name}</div>
          <div className="text-[11.5px] font-display italic text-ink-500 truncate mb-1">{role}</div>
          {p.bio && (
            <div className="text-[11.5px] text-ink-500 leading-[1.45] line-clamp-3">{p.bio}</div>
          )}
        </div>
      </div>
    </div>
  )
}

// highlight.js's common-language build is a large tokenizer most message
// lists never tokenize anything with; load it off the critical path
// (#144b). Every CodeBlock shares the single in-flight request, and
// blocks render as plain (React-escaped) text for the moment before the
// module resolves.
type Hljs = typeof import('highlight.js/lib/common')['default']
let hljsModule: Hljs | null = null
let hljsPromise: Promise<Hljs> | null = null


function useHljs(): Hljs | null {
  const [hljs, setHljs] = useState<Hljs | null>(hljsModule)
  useEffect(() => {
    if (hljsModule) return
    let alive = true
    if (!hljsPromise) {
      hljsPromise = import('highlight.js/lib/common').then((m) => {
        hljsModule = m.default
        return m.default
      })
    }
    hljsPromise.then((h) => { if (alive) setHljs(h) }).catch(() => { /* chunk failed — plain text stays */ })
    return () => { alive = false }
  }, [])
  return hljs
}

/** Restrained, paper-toned code block matching Cumora's overall light
 *  palette. Token colors map onto the brand: keywords use --skype-deep,
 *  strings use --coral-deep, numbers --gold-deep, types --whisper-deep,
 *  comments italic --ink-300. Deliberately NOT a heavy dark card — sits
 *  inside the bubble as a calm inset instead. */
export function CodeBlock({ lang, code }: { lang: string; code: string }) {
  const [copied, setCopied] = useState(false)
  const hljs = useHljs()
  const onCopy = () => {
    void navigator.clipboard?.writeText(code).then(() => {
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1400)
    }).catch(() => { /* ignore — clipboard denied */ })
  }
  // Tokenize with highlight.js. Common-lang build covers ts/js/py/go/rust/
  // json/bash/sql/css/html/md/etc. Unknown lang → auto-detect rather than
  // refuse, so unannotated fences still get color. Null until the lazy
  // module lands — plain text in the meantime.
  const html = useMemo(() => {
    if (!hljs) return null
    try {
      if (lang && hljs.getLanguage(lang)) {
        return hljs.highlight(code, { language: lang, ignoreIllegals: true }).value
      }
      return hljs.highlightAuto(code).value
    } catch {
      // hljs throws on certain malformed inputs — fall back to escaped raw.
      const div = document.createElement('div')
      div.textContent = code
      return div.innerHTML
    }
  }, [code, lang, hljs])

  return (
    <div
      className="my-1.5 rounded-[8px] overflow-hidden font-mono"
      style={{
        background: 'rgba(15, 30, 50, 0.035)',
        border: '1px solid var(--ink-100)',
        maxWidth: '100%',
      }}
    >
      <div
        className="flex items-center px-3 py-1 text-[10px] tracking-[0.14em] uppercase"
        style={{
          color: 'var(--ink-300)',
          borderBottom: '1px solid var(--ink-100)',
        }}
      >
        <span className="font-bold">{lang || 'text'}</span>
        <button
          type="button"
          onClick={onCopy}
          className="ml-auto text-[9.5px] font-bold py-0.5 px-1.5 rounded transition tracking-[0.12em]"
          style={{
            color: copied ? 'var(--avail)' : 'var(--ink-500)',
            background: copied ? 'rgba(110, 197, 106, 0.10)' : 'transparent',
          }}
        >
          {copied ? 'COPIED' : 'COPY'}
        </button>
      </div>
      <pre
        className="cumora-code overflow-x-auto py-2.5 px-3.5 text-[12.5px] leading-[1.6]"
        style={{ color: 'var(--ink-700)', margin: 0, whiteSpace: 'pre' }}
      >
        {/* biome-ignore lint/security/noDangerouslySetInnerHtml: `html` is
            highlight.js output over the message's own code text — hljs
            HTML-escapes the source, and the catch fallback escapes via
            textContent→innerHTML. No untrusted raw HTML reaches the DOM. */}
        {html === null ? <code>{code}</code> : <code dangerouslySetInnerHTML={{ __html: html }} />}
      </pre>
    </div>
  )
}

// Read a string property off a custom mdast→hast node (set via remarkCumora's
// hProperties). Reading from node.properties is the reliable path across
// react-markdown's prop transforms.
function nodeProp(node: unknown, key: string): string {
  const v = (node as { properties?: Record<string, unknown> } | undefined)?.properties?.[key]
  if (typeof v === 'string') return v
  if (Array.isArray(v)) return v.join(' ')
  return v == null ? '' : String(v)
}

// Raw text of a code node — prefer the mdast/hast children values; fall back to
// the rendered children. Used to decide inline vs block and to feed CodeBlock.
function codeText(node: unknown, children: ReactNode): string {
  const kids = (node as { children?: Array<{ value?: string }> } | undefined)?.children
  if (kids && kids.length) {
    const joined = kids.map((c) => c.value ?? '').join('')
    if (joined) return joined
  }
  return Array.isArray(children) ? children.join('') : String(children ?? '')
}


const TABLE_BORDER = '1px solid rgba(15, 30, 50, 0.1)'

/** A backtick-wrapped value that is exactly an artifact id renders as the
 *  matching artifact link (which resolves git-style short ids) instead of a
 *  plain code chip — preserving the pre-react-markdown behavior where
 *  `` `doc_…` `` / `` `ce-…` `` were clickable. */
function artifactLinkForCode(value: string): ReactNode | null {
  const v = value.trim()
  if (/^doc_[A-Za-z0-9]+$/.test(v)) return <DocumentLink id={v} />
  if (/^board-[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$/.test(v)) return <BoardLink id={v} />
  if (/^card-[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$/.test(v)) return <CardLink id={v} />
  if (/^ce-[A-Za-z0-9-]+$/.test(v)) return <CalendarLink id={v} />
  return null
}

/* eslint-disable @typescript-eslint/no-explicit-any */
/** The Cumora look for every Markdown element — standard grammar styled to
 *  match the app's paper-toned palette + chat-tight spacing, plus the custom
 *  elements remarkCumora emits (mentions, artifact cards, Twemoji, Skype). */
// The current conversation, provided by RichBody so an inline `#N` chip knows
// which conversation's `sequence` numbers to resolve against.
const ConversationIdContext = createContext<string | null>(null)

/** `#N` — a per-conversation message-sequence reference. Clickable: scrolls to
 *  the referenced message via useApp.jumpToMessage. Hover: shows a small peek
 *  card (author + body preview). If the conversation context is missing OR the
 *  message hasn't been loaded yet, falls back to plain `#N` text so the
 *  reference is never silently dropped. */
function MessageRefChip({ n }: { n: number }) {
  const convoId = useContext(ConversationIdContext)
  const target = useMessages((s) => {
    if (!convoId) return null
    const list = s.byConvo[convoId]
    if (!list) return null
    for (const m of list) {
      if ((m as { sequence?: number }).sequence === n) return m
    }
    return null
  })
  const jumpToMessage = useApp((s) => s.jumpToMessage)
  const byId = useParticipants((s) => s.byId)
  const [hoverPos, setHoverPos] = useState<{ x: number; y: number } | null>(null)
  const ref = useRef<HTMLSpanElement | null>(null)

  if (!target) return <span className="text-ink-400">#{n}</span>

  const author = byId[target.authorId] ?? null
  const onClick = () => jumpToMessage(target.id)
  const enter = () => {
    if (!ref.current) return
    const r = ref.current.getBoundingClientRect()
    setHoverPos({ x: r.left + r.width / 2, y: r.bottom + 6 })
  }
  const leave = () => setHoverPos(null)

  return (
    <>
      <span
        ref={ref}
        onClick={onClick}
        onMouseEnter={enter}
        onMouseLeave={leave}
        className="inline-flex items-center px-1.5 py-0.5 rounded-full font-semibold cursor-pointer transition text-skype-deep bg-sky-50 hover:bg-sky-100"
        // Measured against 14px / line-height 1.55 CJK text — -0.05em centers
        // the chip on the CJK glyph center; -0.15em (the MentionChip value)
        // visibly sits low here because there's no avatar to anchor it.
        style={{ verticalAlign: '-0.05em' }}
        title={`Jump to message #${n}`}
        role="link"
      >
        {/* Cap the inner height + line-height so the chip is a stable 20px
            tall — without this, it inherits the bubble's 1.55 leading and
            grows taller than expected, which is what made the older offset
            misalign. */}
        <span style={{ display: 'inline-flex', alignItems: 'center', height: 16, lineHeight: 1 }}>#{n}</span>
      </span>
      {hoverPos && createPortal(
        <MessagePeekCard msg={target} author={author} x={hoverPos.x} y={hoverPos.y} />,
        document.body,
      )}
    </>
  )
}

/** Floating preview card shown when the user hovers a `#N` chip — mirrors the
 *  MentionCard pattern (portalled, ink-tone palette). Shows author + a short
 *  body excerpt so users can read the referenced message without jumping. */
function MessagePeekCard(
  { msg, author, x, y }: { msg: Message; author: Participant | null; x: number; y: number },
) {
  const t = useT()
  const ARROW = 8
  const W = 320
  const left = Math.max(8, Math.min(window.innerWidth - W - 8, x - W / 2))
  const top = y + ARROW
  const bodyPreview = (msg.body ?? '').replace(/\n/g, ' ').slice(0, 220)
  return (
    <div
      role="tooltip"
      className="fixed z-[80] animate-rise"
      style={{ left, top, width: W, pointerEvents: 'none' }}
    >
      <div className="rounded-[12px] bg-cloud border border-ink-100 shadow-[0_22px_44px_-22px_rgba(0,80,140,0.35)] px-3.5 py-2.5">
        <div className="flex items-center gap-2 mb-1.5">
          {author ? <Avatar p={author} size={20} ringColor="var(--cloud)" showStatus={false} /> : null}
          <div className="min-w-0 flex-1 flex items-baseline gap-2">
            <span className="font-display font-semibold text-[12.5px] text-ink-900 truncate">{author?.name ?? msg.authorId}</span>
            {author?.role ? <span className="text-[10px] text-ink-400 truncate uppercase tracking-[0.08em]">{author.role}</span> : null}
          </div>
        </div>
        <div className="text-[12.5px] text-ink-700 leading-[1.55] line-clamp-5 break-words">
          {bodyPreview || <span className="italic text-ink-400">{t('msgview.noText')}</span>}
        </div>
      </div>
    </div>
  )
}


const cumoraMarkdownComponents = {
  // ── Cumora custom inline tokens (emitted by remarkCumora) ──
  cmention: ({ node }: any) => <MentionChip id={nodeProp(node, 'cid')} />,
  cmsgref: ({ node }: any) => <MessageRefChip n={Number(nodeProp(node, 'cn')) || 0} />,
  cartifact: ({ node }: any) => {
    const id = nodeProp(node, 'cid')
    switch (nodeProp(node, 'ckind')) {
      case 'document': return <DocumentLink id={id} />
      case 'board': return <BoardLink id={id} />
      case 'card': return <CardLink id={id} />
      case 'calendar': return <CalendarLink id={id} />
      default: return <>{id}</>
    }
  },
  cemoji: ({ node }: any) => <TwEmoji emoji={nodeProp(node, 'cchar')} size={18} />,
  cskype: ({ node }: any) => <SkypeEmoji name={nodeProp(node, 'cname')} size={20} />,

  // ── Standard block grammar ──
  p: ({ children }: any) => <p className="m-0 mt-2 first:mt-0 leading-[1.55]">{children}</p>,
  // Headings use Cumora's display font + tracking-tight, matching how titles
  // read everywhere else in the app (e.g. `font-display ... tracking-tight`).
  h1: ({ children }: any) => <h1 className="font-display mt-3 first:mt-0 mb-1 text-[18px] font-semibold tracking-tight leading-snug text-ink-900">{children}</h1>,
  h2: ({ children }: any) => <h2 className="font-display mt-3 first:mt-0 mb-1 text-[16px] font-semibold tracking-tight leading-snug text-ink-900">{children}</h2>,
  h3: ({ children }: any) => <h3 className="font-display mt-2.5 first:mt-0 mb-0.5 text-[14.5px] font-semibold tracking-tight leading-snug text-ink-900">{children}</h3>,
  h4: ({ children }: any) => <h4 className="font-display mt-2 first:mt-0 mb-0.5 text-[13.5px] font-semibold text-ink-800">{children}</h4>,
  h5: ({ children }: any) => <h5 className="font-display mt-2 first:mt-0 text-[12px] font-semibold uppercase tracking-[0.08em] text-ink-500">{children}</h5>,
  h6: ({ children }: any) => <h6 className="font-display mt-2 first:mt-0 text-[11.5px] font-semibold uppercase tracking-[0.1em] text-ink-400">{children}</h6>,
  ul: ({ children }: any) => <ul className="my-1 first:mt-0 last:mb-0 pl-5 list-disc marker:text-ink-300 space-y-0.5">{children}</ul>,
  ol: ({ children }: any) => <ol className="my-1 first:mt-0 last:mb-0 pl-5 list-decimal marker:text-ink-400 space-y-0.5">{children}</ol>,
  li: ({ children }: any) => <li className="leading-[1.5] pl-0.5">{children}</li>,
  // No italic — Cumora messages are CJK-heavy and synthetic-slanted CJK looks
  // bad. A calm neutral rule + muted text reads as a quote without the slant.
  blockquote: ({ children }: any) => (
    <blockquote className="my-1.5 first:mt-0 last:mb-0 border-l-[3px] border-ink-200 pl-3 text-ink-500">{children}</blockquote>
  ),
  hr: () => <hr className="my-2.5 border-0 border-t border-ink-100" />,
  a: ({ href, children }: any) => (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="break-all underline decoration-skype-200 decoration-1 underline-offset-2 hover:decoration-skype-deep transition-colors"
      style={{ color: 'var(--skype-deep)' }}
    >{children}</a>
  ),
  strong: ({ children }: any) => <strong className="font-semibold text-ink-900">{children}</strong>,
  // Inline markdown images (`![alt](url)`) arrive with no intrinsic
  // dimensions, so a bare <img> paints at 0×0 then snaps to natural size on
  // load — pushing every row below it downward. Inside the virtualized mobile
  // list that height change re-anchors the scroll and is one of the causes of
  // the "message I'm reading gets replaced" jitter. Reserve a fixed box up
  // front (same tactic as AttachmentCard) so the row height is stable from the
  // first paint; the image letterboxes into it once it loads.
  img: ({ src, alt }: any) => (
    src ? (
      <span
        className="block my-1.5 first:mt-0 last:mb-0 rounded-[10px] border border-ink-100 bg-cloud overflow-hidden"
        style={{ aspectRatio: '4 / 3', width: '100%', maxWidth: 420, maxHeight: 360 }}
      >
        <img
          src={src}
          alt={alt ?? ''}
          className="w-full h-full object-contain"
          loading="lazy"
          decoding="async"
          draggable={false}
        />
      </span>
    ) : null
  ),
  em: ({ children }: any) => <em className="italic">{children}</em>,
  del: ({ children }: any) => <del className="line-through text-ink-400">{children}</del>,
  input: ({ checked }: any) => (
    <input type="checkbox" checked={!!checked} readOnly className="mr-1.5 align-[-0.1em] accent-skype" />
  ),

  // ── Tables (remark-gfm) ──
  table: ({ children }: any) => (
    <div className="my-1.5 first:mt-0 last:mb-0 overflow-x-auto">
      <table className="border-collapse text-[13px] leading-[1.45]" style={{ border: TABLE_BORDER }}>{children}</table>
    </div>
  ),
  th: ({ children, style }: any) => (
    <th
      className="px-2.5 py-1 font-semibold text-ink-900 whitespace-nowrap"
      style={{ border: TABLE_BORDER, background: 'rgba(15, 30, 50, 0.045)', textAlign: style?.textAlign ?? 'left' }}
    >{children}</th>
  ),
  td: ({ children, style }: any) => (
    <td
      className="px-2.5 py-1 text-ink-700 align-top"
      style={{ border: TABLE_BORDER, textAlign: style?.textAlign ?? 'left' }}
    >{children}</td>
  ),

  // ── Code: fenced → CodeBlock (highlight.js); inline → paper chip ──
  pre: ({ children }: any) => <>{children}</>,
  code: ({ className, children, node }: any) => {
    const lang = /language-([\w-]+)/.exec(className || '')?.[1] ?? ''
    const raw = codeText(node, children)
    if (lang || raw.includes('\n')) {
      return <CodeBlock lang={lang} code={raw.replace(/\n$/, '')} />
    }
    // A backtick-wrapped artifact id becomes its clickable link (short-id aware).
    const artifact = artifactLinkForCode(raw)
    if (artifact) return artifact
    return (
      <code
        className="font-mono text-[12.5px] py-px px-1.5 rounded-[5px] mx-px align-[0.05em]"
        style={{ background: 'rgba(15, 30, 50, 0.06)', color: 'var(--ink-900)', border: '1px solid rgba(15, 30, 50, 0.08)' }}
      >{children}</code>
    )
  },
} as Components

/* eslint-enable @typescript-eslint/no-explicit-any */

const REMARK_PLUGINS = [remarkGfm, remarkBreaks, remarkCumora]

/** Renders a message body as Markdown — full CommonMark + GFM via react-markdown,
 *  plus Cumora's own tokens (mentions / artifacts / emoji) — all styled to the
 *  app's look (see cumoraMarkdownComponents). `skipHtml` drops any raw HTML in
 *  agent/user content so messages can't inject markup.
 *
 *  Memoized on (body, conversationId) (#143): react-markdown re-parses the
 *  whole body on every render and that parse is O(body), so a row that
 *  re-renders for unrelated reasons (reactions pill update, roster churn)
 *  must not pay the parse again for an unchanged body. */
export const RichBody = memo(function RichBody({ body, conversationId }: { body: string; conversationId?: string | null }) {
  return (
    <ConversationIdContext.Provider value={conversationId ?? null}>
      <Markdown remarkPlugins={REMARK_PLUGINS} components={cumoraMarkdownComponents} skipHtml>
        {body}
      </Markdown>
    </ConversationIdContext.Provider>
  )
})
