/**
 * agenda-triage-core — the PURE, dependency-free heart of agenda triage
 * (ticket #20, mirrors `triage-core.ts`'s split for inbox triage).
 *
 * It builds the cerebellum's agenda-classifier prompt (instructions + input),
 * parses/coerces its JSON verdict, and holds the deterministic fail-closed
 * fallback used when the classifier is unavailable. It imports NOTHING with
 * side effects (no LLM client, no env, no DB) — only types — so it can be
 * bundled into the standalone BYOA daemon, which runs the SAME classification
 * on its LOCAL small model (via `EngineAdapter.classify()`) instead of a
 * cloud call, exactly as `triage-core.ts` already does for inbox triage.
 *
 * Two consumers share this core:
 *   - server `classifyAgendaActionable` (remote route): build → cloud/adapter
 *     model → parse.
 *   - BYOA daemon (byoa route): server builds the request (it has the DB) and
 *     hands it back via `/runtime/agenda`; the daemon runs its LOCAL small
 *     model on it and parses here, then posts the verdict back to
 *     `/runtime/agenda/verdict`. The big brain is never spent on triage.
 */

/** A Kanban card that the agent should plausibly act on. */
export interface AgendaCard {
  id: string
  board_id: string
  board_title: string
  column_id: string
  column_title: string
  title: string
  description: string | null
  assignee_id: string | null
  mentions: string[]
  updated_at: string
}

/** A calendar event in the current / imminent slot for this agent. */
export interface AgendaEvent {
  id: string
  title: string
  description: string | null
  start_at: string
  agent_prompt: string | null
  target_conversation_id: string | null
}

/** A conversation the agent is in that has gone quiet mid-flow. See
 *  `agenda.ts` for the full rationale — kept here verbatim since the shape is
 *  part of the pure classifier-request/fallback contract. */
export interface StalledConvo {
  conversationId: string
  kind: string
  title: string | null
  lastMessageId: string
  lastAuthorId: string
  lastAuthorName: string
  lastAuthorIsSelf: boolean
  lastBody: string
  minutesSilent: number
  recentTail: string
}

export interface AgentAgenda {
  cards: AgendaCard[]
  events: AgendaEvent[]
  stalls: StalledConvo[]
}

export interface AgendaVerdict {
  /** Whether the agent should wake right now to act on this agenda. */
  actionable: boolean
  /** One-line focus the brain should drive toward, if actionable. */
  focus: string
  /** Telemetry-only reason string; never shown to the agent. The literal
   *  string `'classifier error'` is a sentinel — callers should treat it
   *  as "triage failed, fall back to the safe path" rather than "no work
   *  to do," so a transient LLM outage doesn't silence every agent with
   *  agenda items. */
  reason: string
}

/** Sentinel value for {@link AgendaVerdict.reason} when the underlying
 *  classifier call threw (remote) or the local classify() failed (byoa).
 *  Exported so callers can detect it without string-matching. */
export const AGENDA_CLASSIFIER_ERROR = 'classifier error'

/** Minimal persona shape the classifier prompt needs. */
export interface AgendaClassifierPersona {
  name: string
  role: string
  style: string
}

export type AgendaClassifierRequest =
  | { verdict: AgendaVerdict; instructions?: undefined; input?: undefined }
  | { instructions: string; input: string; verdict?: undefined }

/** Patterns we treat as "the card is already finished, don't bug the
 *  agent." Matched case-insensitively against the column title. */
export const DONE_COLUMN_PATTERNS = /\b(done|complete|completed|shipped|archive|archived|closed|cancel|canceled|cancelled)\b/i

function byId<T extends { id: string }>(a: T, b: T): number {
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
}

/** Render the agenda as a compact text block for the classifier. See
 *  `agenda.ts`'s original doc comment for the prefix-cache-ordering
 *  rationale — unchanged by the split, just relocated. */
export function renderAgendaForClassifier(agenda: AgentAgenda): string {
  const lines: string[] = []
  const cards = [...agenda.cards].sort(byId)
  const stalls = [...agenda.stalls].sort((a, b) => (
    a.conversationId < b.conversationId ? -1 : a.conversationId > b.conversationId ? 1 : 0
  ))
  if (cards.length > 0) {
    lines.push(`Kanban cards (${cards.length}):`)
    for (const c of cards) {
      const tag = c.assignee_id ? 'assigned' : 'mentioned'
      lines.push(`- [${tag}] "${c.title}" in ${c.board_title} / ${c.column_title} (id=${c.id}, updated ${c.updated_at})`)
    }
  }
  if (stalls.length > 0) {
    if (lines.length > 0) lines.push('')
    lines.push(`Conversations you're in that have gone quiet (${stalls.length}) — judge each: genuinely WAITING on someone, or already CONCLUDED/wound down?`)
    for (const s of stalls) {
      const who = s.lastAuthorIsSelf ? 'YOU spoke last, no reply since' : `${s.lastAuthorName} spoke last, you haven't responded`
      lines.push(`- [${s.kind}] ${s.title ?? s.conversationId} (${s.conversationId}) — ${who}. Last few messages:`)
      for (const ln of (s.recentTail || s.lastBody).split('\n')) lines.push(`    ${ln}`)
    }
  }
  if (agenda.events.length > 0) {
    if (lines.length > 0) lines.push('')
    lines.push(`Calendar events in current slot (${agenda.events.length}):`)
    for (const e of agenda.events) {
      const prompt = e.agent_prompt ? ` — prompt: ${e.agent_prompt.slice(0, 140)}` : ''
      lines.push(`- "${e.title}" at ${e.start_at}${prompt}`)
    }
  }
  if (stalls.length > 0) {
    if (lines.length > 0) lines.push('')
    lines.push('Current stall timing:')
    for (const s of stalls) lines.push(`- ${s.conversationId}: ${s.minutesSilent}m silent`)
  }
  if (lines.length === 0) return '(empty)'
  return lines.join('\n')
}

/** Build the classifier request: an immediate empty-agenda verdict (no model
 *  call needed), or the {instructions, input} prompt to feed a JSON classifier
 *  — cloud (remote route) or the operator's own local engine (byoa route). */
export function buildAgendaClassifierRequest(args: {
  persona: AgendaClassifierPersona
  agenda: AgentAgenda
}): AgendaClassifierRequest {
  const { persona, agenda } = args
  if (agenda.cards.length === 0 && agenda.events.length === 0 && agenda.stalls.length === 0) {
    return { verdict: { actionable: false, focus: '', reason: 'empty agenda' } }
  }
  const rendered = renderAgendaForClassifier(agenda)
  const styleHint = (persona.style ?? '').slice(0, 400)
  const instructions = `You are Cumora's heartbeat agenda triage. Given an agent's currently-assigned Kanban cards and their currently-due calendar events, decide whether the agent should wake up RIGHT NOW to act on something, or stay quiet.

Decide "actionable: true" only when at least one item is concrete, fresh, AND clearly belongs to this agent's role. Reject:
- vague brainstorming cards with no owner action
- cards already in a done/archive-style column
- events that are personal markers (no agent_prompt)
- duplicates of work the agent has obviously already started

ORDERING: list order is for prompt-cache stability, not priority. Judge urgency from each card's updated timestamp, each event's start time, and each stalled conversation's current silence duration and message content.

STALLED CONVERSATIONS: for each, READ the recent messages shown and judge whether it is genuinely WAITING or already CONCLUDED. actionable=true ONLY when the recent messages show a CONCRETE unanswered ask directed at someone, or an explicitly in-progress step plainly waiting on a next move. It is NOT actionable (false) when the thread has CONCLUDED or socially CLOSED — participants exchanging wrap-up / closing / acknowledgement remarks, a conclusion or result already reached, nothing pending and no one waiting on a specific next step — no matter how "quiet" it now is. A merely quiet conversation is NOT a stall; "someone spoke last and I didn't reply" is NOT by itself a reason (most messages need no reply). Resurrecting a finished conversation with a late reply is a failure — when in doubt that it's truly still waiting, choose false.

Reply ONLY as strict JSON: {"actionable": boolean, "focus": "one-line focus for the agent if actionable, else empty", "reason": "short factual reason"}.`
  const input = `Agent: ${persona.name}${persona.role ? `, ${persona.role}` : ''}
Persona / style:
${styleHint || '(none)'}

Current agenda for this agent:
${rendered}

Reply as strict JSON.`
  return { instructions, input }
}

/** Parse the support model's verdict conservatively. JSON mode is not enough
 * for every OpenAI-compatible provider: DeepSeek sometimes wraps JSON, omits
 * key quotes, or truncates after all required fields. Recover only an explicit
 * actionable value. A malformed positive also needs a non-empty focus, so
 * salvage can never turn ambiguous output into a wake. */
export function parseAgendaVerdict(raw: string): {
  actionable?: unknown
  focus?: unknown
  reason?: unknown
} | null {
  const unfenced = raw.trim()
    .replace(/^```(?:json)?\s*/i, '')
    .replace(/\s*```$/i, '')
  const firstBrace = unfenced.indexOf('{')
  if (firstBrace < 0) return null
  const lastBrace = unfenced.lastIndexOf('}')
  const candidate = unfenced.slice(firstBrace, lastBrace > firstBrace ? lastBrace + 1 : undefined)
  try {
    return JSON.parse(candidate) as { actionable?: unknown; focus?: unknown; reason?: unknown }
  } catch { /* conservative field salvage below */ }

  const actionMatch = candidate.match(
    /(?:["']?actionable["']?)\s*:\s*(true|false|1|0|["']true["']|["']false["'])/i,
  )
  if (!actionMatch) return null
  const actionToken = actionMatch[1].replace(/["']/g, '').toLowerCase()
  const actionable = actionToken === 'true' || actionToken === '1'
  const stringField = (name: string): string => {
    const match = candidate.match(new RegExp(`(?:["']?${name}["']?)\\s*:\\s*(["'])([\\s\\S]*?)\\1(?:\\s*[,}]|$)`, 'i'))
    return match?.[2] ?? ''
  }
  const focus = stringField('focus')
  const reason = stringField('reason')
  if (actionable && !focus.trim()) {
    return { actionable: false, focus: '', reason: 'malformed positive verdict without focus' }
  }
  return { actionable, focus, reason }
}

/** Coerce a parsed classifier reply into a real {@link AgendaVerdict}.
 *  `Boolean("no")` is `true` because non-empty strings are truthy — so if the
 *  model replies {"actionable": "no"} (treating it as natural language) we'd
 *  wrongly wake the brain. Accept true/"true"/1 as actionable; treat
 *  everything else (including "no", "false", undefined, 0) as not
 *  actionable. Same for focus / reason — clamp via String + slice even if the
 *  model returns a number or an array. */
export function coerceAgendaVerdict(parsed: { actionable?: unknown; focus?: unknown; reason?: unknown }): AgendaVerdict {
  const a = parsed.actionable
  const actionable = a === true || a === 'true' || a === 1
  return {
    actionable,
    focus: String(parsed.focus ?? '').slice(0, 240),
    reason: String(parsed.reason ?? '').slice(0, 240),
  }
}

/** How far back a stall can be and still qualify for the deterministic
 *  fallback below (see rationale on {@link agendaDeterministicFallback}). */
const STALL_FALLBACK_MAX_MIN = 30

/** Deterministic fallback used when the classifier is unavailable (remote:
 *  the cloud/adapter call threw; byoa: the local engine's classify() failed
 *  or returned unparseable output). Default fail-closed BUT carve out a
 *  narrow, conservative case so the stall safety net isn't 100% broken during
 *  an outage: a SINGLE recent stall where SOMEONE ELSE spoke last and this
 *  agent owes a reply. See the original inline comment in `agenda.ts`
 *  (preserved in history) for the full rationale — behavior is unchanged by
 *  the extraction, just relocated so both routes can share it. */
export function agendaDeterministicFallback(agenda: AgentAgenda): AgendaVerdict {
  const recentAwaitingMe = agenda.stalls.filter(
    (s) => !s.lastAuthorIsSelf && s.minutesSilent <= STALL_FALLBACK_MAX_MIN,
  )
  if (
    agenda.cards.length === 0 &&
    agenda.events.length === 0 &&
    recentAwaitingMe.length === 1
  ) {
    const s = recentAwaitingMe[0]
    return {
      actionable: true,
      focus: `Reply to ${s.lastAuthorName} in ${s.title ?? s.conversationId} — they spoke last (${s.minutesSilent}m ago) and you haven't responded.`,
      reason: `${AGENDA_CLASSIFIER_ERROR} (deterministic fallback: single recent awaiting-you stall)`,
    }
  }
  return { actionable: false, focus: '', reason: AGENDA_CLASSIFIER_ERROR }
}
