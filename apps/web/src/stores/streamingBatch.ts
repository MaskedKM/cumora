/** Coalescing buffer for `message.delta` events (#143).
 *
 *  Every delta used to land as its own store `set`, so a token stream
 *  fanned out into dozens of synchronous zustand updates per second —
 *  each re-rendering the chat pane and re-parsing the growing body.
 *  The messages store instead pushes deltas here and flushes the whole
 *  batch as ONE set per animation frame. Pure data structure, no store
 *  or React imports, so the merge/drain rules are unit-testable. */

export interface BufferedDelta {
  messageId: string
  conversationId: string
  authorId: string
  sequence: number
  /** Delta text accumulated since the last drain — appended in arrival
   *  order, so a drain replays the stream verbatim. */
  text: string
}

export class StreamingDeltaBatch {
  private byMessage = new Map<string, BufferedDelta>()
  private chars = 0

  /** Append one delta event. Repeated events for the same message merge
   *  into a single entry (text concatenated, sequence overwritten with
   *  the latest). */
  push(messageId: string, conversationId: string, authorId: string, sequence: number, text: string): void {
    const cur = this.byMessage.get(messageId)
    if (cur) {
      cur.text += text
      cur.sequence = sequence
    } else {
      this.byMessage.set(messageId, { messageId, conversationId, authorId, sequence, text })
    }
    this.chars += text.length
  }

  /** Total buffered text length — the flush-cap axis. Counts down on
   *  drop/drain so a stalled scheduler can't grow the buffer unbounded
   *  without tripping the caller's cap check. */
  get bufferedChars(): number {
    return this.chars
  }

  get isEmpty(): boolean {
    return this.byMessage.size === 0
  }

  /** Remove a message's pending text without applying it — the terminal
   *  `done` / `message.new` path, where the completed body supersedes
   *  whatever tail is still sitting in the buffer. */
  drop(messageId: string): void {
    const cur = this.byMessage.get(messageId)
    if (!cur) return
    this.chars -= cur.text.length
    this.byMessage.delete(messageId)
  }

  /** Take every buffered delta (arrival order across messages is the
   *  Map's insertion order) and reset the buffer. */
  drain(): BufferedDelta[] {
    if (this.byMessage.size === 0) return []
    const out = Array.from(this.byMessage.values())
    this.byMessage.clear()
    this.chars = 0
    return out
  }

  /** Discard everything without applying — the WS reconnect path, where
   *  buffered deltas belong to a connection whose stream state was just
   *  reset. */
  clear(): void {
    this.byMessage.clear()
    this.chars = 0
  }
}

/** Streaming map entry shape the messages store keeps (structural, so
 *  this module stays free of store imports). */
export interface StreamingBody {
  body: string
  conversationId: string
  authorId: string
  sequence: number
}

/** Fold drained deltas into a streaming map (#143). Appends to live
 *  entries AND creates missing ones — a message's first flush is also
 *  its birth (the old per-delta path created the entry immediately, so
 *  the flush must too). Resurrection of TERMINATED messages can't happen
 *  through here because every termination path (done / message.new /
 *  reconnect reset) drops the buffer before removing the entry. Returns
 *  the same reference when there is nothing to apply. */
export function applyBufferedDeltas<M extends Record<string, StreamingBody>>(
  streaming: M,
  deltas: BufferedDelta[],
): M {
  if (deltas.length === 0) return streaming
  const next: Record<string, StreamingBody> = { ...streaming }
  for (const d of deltas) {
    const cur = next[d.messageId]
    next[d.messageId] = {
      body: (cur?.body ?? '') + d.text,
      conversationId: d.conversationId,
      authorId: d.authorId,
      sequence: d.sequence,
    }
  }
  return next as M
}
