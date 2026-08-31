// 长按 Tapback 菜单装配件(#219 ⑤)—— 从 MobileChat.tsx 原样搬移:
// buildMessageTapbackActions(Reply/Copy/Delete 动作行装配)。
import type { MessageKey } from '@/lib/i18n'
import type { Message } from '@/types'
import type { TapbackAction } from '../MobileMessageTapback'

/** Build the tapback action rows for a message. Reply is always
 *  available; Copy text appears when the message has a body; Delete
 *  is the user's own messages only. Future passes can layer on
 *  Quote, Thread, Forward, etc. without changing the menu chrome. */
export function buildMessageTapbackActions(
  msg: Message,
  convoId: string | null,
  setReplyingTo: (convoId: string, msgId: string | null) => void,
  meId: string | null,
  t: (key: MessageKey, vars?: Record<string, string | number>) => string,
): TapbackAction[] {
  const out: TapbackAction[] = []
  out.push({
    label: t('mobchat.tapbackReply'),
    onClick: () => { if (convoId) setReplyingTo(convoId, msg.id) },
  })
  if (msg.body && msg.body.trim()) {
    out.push({
      label: t('mobchat.tapbackCopy'),
      onClick: () => {
        // Best-effort — `navigator.clipboard.writeText` returns a
        // promise we don't await; failures fall through silently
        // (rare on iOS WKWebView, and there's no good recovery).
        void navigator.clipboard?.writeText(msg.body).catch(() => { /* ignore */ })
      },
    })
  }
  // Delete is only offered for the user's own messages. Mirrors the
  // desktop ChatPane permission model: agents own their own rows.
  if (meId && msg.authorId === meId) {
    out.push({
      label: t('common.delete'),
      destructive: true,
      onClick: () => {
        // Wire deletion later — for now just close, no destructive
        // call. Keeping the row in the menu so the affordance is
        // discoverable + we can light it up the moment the API
        // surface is ready.
      },
    })
  }
  return out
}
