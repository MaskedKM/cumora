/**
 * Heartbeat agenda pre-check — runs on the idle scheduler before we
 * wake an agent's brain.
 *
 * Why this exists:
 *   The plain idle wake hands a generic "is there anything worth
 *   doing?" prompt to the agent's main (brain) model. That call is
 *   expensive AND the agent has no agenda context — it has to query
 *   inbox / groups / contacts itself before it can even decide
 *   whether to act, burning a full reasoning turn on what often is
 *   "no, nothing to do."
 *
 *   This module front-runs that decision with a cheap (cerebellum)
 *   classifier. We collect the agent's actionable surface — Kanban
 *   cards assigned to or @-mentioning them in non-done columns, plus
 *   Calendar events due in the current/imminent slot — and feed it
 *   to a small JSON classifier that decides:
 *       (a) is anything genuinely worth waking the brain for? and
 *       (b) if so, what should it focus on first?
 *
 *   When the answer is yes, we wake with a focused
 *   `backgroundBrief` so the brain spends its turn EXECUTING, not
 *   deciding-whether-to-execute. When the answer is no, we skip the
 *   wake entirely — the brain never runs, no tokens spent.
 *
 *   Net effect: heartbeats become high-signal AND cheaper. The
 *   classifier is the cerebellum doing exactly what cerebellum is
 *   for: a fast yes/no on data, no real reasoning.
 */
import { pool } from '../db/pool.js'
import { env } from '../env.js'
import { getTrackedLlmClient } from './llm-ledger.js'
import { redis } from '../redis.js'
import { resolveCerebellumRouteForAgent } from './cerebellum-route.js'
import { cerebellumResponsesShim, cerebellumRemoteConfigured } from '../cerebellum-adapter.js'
import {
  DONE_COLUMN_PATTERNS,
  AGENDA_CLASSIFIER_ERROR,
  renderAgendaForClassifier,
  buildAgendaClassifierRequest,
  parseAgendaVerdict,
  coerceAgendaVerdict,
  agendaDeterministicFallback,
  type AgendaCard,
  type AgendaEvent,
  type StalledConvo,
  type AgentAgenda,
  type AgendaVerdict,
} from './agenda-triage-core.js'

export {
  AGENDA_CLASSIFIER_ERROR,
  parseAgendaVerdict,
  type AgendaCard,
  type AgendaEvent,
  type StalledConvo,
  type AgentAgenda,
  type AgendaVerdict,
}

/** How far ahead a calendar event can be and still count as "current
 *  slot" for the heartbeat. 30 min gives the agent enough time to
 *  load context without surfacing things they should think about
 *  hours from now. */
const CALENDAR_LOOKAHEAD_MS = 30 * 60_000
/** How far in the past a still-active calendar event can be and still
 *  count. Covers the gap between the calendar dispatcher's last fire
 *  and this heartbeat. */
const CALENDAR_LOOKBEHIND_MS = 15 * 60_000

/** Stall window: a conversation counts as "stalled" only if its last message is
 *  at least STALL_MIN_MS old (give people time to reply naturally — don't nag)
 *  and at most STALL_MAX_MS old (don't resurrect ancient threads). Env-overridable. */
const STALL_MIN_MS = Number(process.env.CUMORA_STALL_MIN_MS) || 5 * 60_000
const STALL_MAX_MS = Number(process.env.CUMORA_STALL_MAX_MS) || 6 * 60 * 60_000
/** Cooldown between nudges to the SAME conversation, across ALL agents. Keyed on
 *  conversation only (NOT last-message-id) — a nudge changes the last message, so
 *  a per-message key would re-arm 5min later and nag. This guarantees a stalled
 *  conversation is nudged at most ONCE per cooldown by exactly ONE agent. Env-
 *  overridable; set very large for "one nudge per stall episode, basically once". */
const NUDGE_COOLDOWN_MS = Number(process.env.CUMORA_NUDGE_COOLDOWN_MS) || 45 * 60_000
/** SHORTER cooldown for nudges driven by the DETERMINISTIC fallback
 *  (cerebellum unavailable). Rationale: the deterministic rule is conservative
 *  on input but doesn't know whether the WOKEN agent's big brain will actually
 *  decide to post — if it declines, the 45min cerebellum-grade cooldown locks
 *  out everyone else and a single "no" decides the stall's fate. A short TTL
 *  lets other members get a turn at the same stall with a different big-brain
 *  judgment. The 45min lock is right when the cerebellum SAID "yes nudge";
 *  it's wrong as the only knob for an outage-time fallback. */
const NUDGE_COOLDOWN_FALLBACK_MS = Number(process.env.CUMORA_NUDGE_COOLDOWN_FALLBACK_MS) || 5 * 60_000

/** Claim the right to nudge a stalled conversation. Returns true for the FIRST
 *  caller (across all member agents); everyone else — and the same agent on a
 *  later heartbeat — gets false until the cooldown lapses. ONE key per
 *  CONVERSATION (NX + cooldown TTL).
 *
 *  Pass `source: 'fallback'` when the wake is from the deterministic agenda
 *  fallback (classifier 503), to use the shorter cooldown — a "no" from the
 *  chosen agent shouldn't permanently silence a stall the cerebellum can't
 *  even weigh in on. Default 'classified' preserves the 45min cooldown for
 *  the normal LLM-gated path. */
export async function claimStallNudge(
  conversationId: string,
  opts?: { source?: 'classified' | 'fallback' },
): Promise<boolean> {
  // Fallback path: after N successive nudges land without the stall
  // ACTUALLY advancing (new message in the convo), stop. The LLM judgment
  // has converged — every woken big brain is declining for the same
  // reason. Re-wakes every 5min after that point burn tokens for no
  // changed decision. The counter is the per-conversation declines
  // counter; the cap is small (3) so a stall the team has truly given
  // up on stops auto-firing — without permanently silencing it, because
  // the counter expires on the next NEW message in the convo (someone
  // posting resets the conversation state). Classified-path nudges
  // already use the 45min cooldown so they don't need a separate cap.
  if (opts?.source === 'fallback') {
    const declineKey = `cumora:nudge-declines:${conversationId}`
    const declines = await redis.get(declineKey).then((v) => Number(v) || 0).catch(() => 0)
    if (declines >= 3) return false
  }
  const key = `cumora:nudge:${conversationId}`
  const cooldownMs = opts?.source === 'fallback' ? NUDGE_COOLDOWN_FALLBACK_MS : NUDGE_COOLDOWN_MS
  const ttlSec = Math.ceil(cooldownMs / 1000)
  const res = await redis.set(key, '1', 'EX', ttlSec, 'NX').catch(() => null)
  if (res === 'OK' && opts?.source === 'fallback') {
    // Record the decline-attempt counter (TTL matches the stall window so
    // it auto-expires; resets on any actual conversation message via the
    // counter-reset path below). The naming is "declines" but it really
    // tracks "fallback claims granted without a follow-on post" — we can't
    // know whether the agent posted until later; the cleaner-on-post path
    // (resetStallNudgeDeclines) handles the truthing.
    const declineKey = `cumora:nudge-declines:${conversationId}`
    await redis.multi()
      .incr(declineKey)
      .expire(declineKey, Math.ceil(NUDGE_COOLDOWN_MS / 1000))
      .exec()
      .catch(() => null)
  }
  return res === 'OK'
}

/** Called when a NEW message lands in a conversation — clears the
 *  decline counter so future fallback nudges (if the convo stalls again
 *  LATER) get a fresh budget. The counter only existed to stop a single
 *  stalled state from being re-poked indefinitely; new activity = new
 *  state = new budget. Fire-and-forget. */
export async function resetStallNudgeDeclines(conversationId: string): Promise<void> {
  if (!conversationId) return
  await redis.del(`cumora:nudge-declines:${conversationId}`).catch(() => null)
}

/** Pull cards assigned to (or @-mentioning) the agent in non-done
 *  columns. Capped tightly so we never feed the classifier a wall of
 *  context. Stale cards (untouched for 30 days) are ignored — they're
 *  almost always abandoned work. */
async function loadAssignedCards(agentId: string, companyId: string): Promise<AgendaCard[]> {
  const { rows } = await pool.query<AgendaCard>(
    `SELECT c.id, c.board_id, b.title AS board_title,
            c.column_id, col.title AS column_title,
            c.title, c.description, c.assignee_id,
            COALESCE(c.mentions, '[]'::jsonb) AS mentions,
            c.updated_at::text AS updated_at
       FROM board_cards c
       JOIN boards b ON b.id = c.board_id
       JOIN board_columns col ON col.id = c.column_id
      WHERE b.company_id = $1
        AND (c.assignee_id = $2 OR c.mentions @> to_jsonb($2::text))
        AND c.updated_at > NOW() - INTERVAL '30 days'
      ORDER BY c.updated_at DESC
      LIMIT 20`,
    [companyId, agentId],
  )
  return rows.filter((r) => !DONE_COLUMN_PATTERNS.test(r.column_title ?? ''))
}

/** Pull calendar events for this agent that fall inside the current
 *  slot window. We deliberately keep `recurrence` off the result —
 *  the classifier doesn't need rule semantics, just the next concrete
 *  occurrence time, and the calendar dispatcher already handles the
 *  recurrence math for actual dispatches. */
async function loadDueEvents(agentId: string, companyId: string, now: Date): Promise<AgendaEvent[]> {
  const lookbehindFloor = new Date(now.getTime() - CALENDAR_LOOKBEHIND_MS)
  const lookaheadCeiling = new Date(now.getTime() + CALENDAR_LOOKAHEAD_MS)
  const { rows } = await pool.query<AgendaEvent>(
    `SELECT id, title, description, start_at::text AS start_at,
            agent_prompt, target_conversation_id
       FROM calendar_events
      WHERE company_id = $1
        AND assignee_id = $2
        AND status = 'active'
        AND start_at BETWEEN $3 AND $4
      ORDER BY start_at ASC
      LIMIT 10`,
    [companyId, agentId, lookbehindFloor, lookaheadCeiling],
  )
  return rows
}

/** Conversations the agent is a member of whose LATEST text message sits in the
 *  stall window [STALL_MIN_MS, STALL_MAX_MS] of silence. Cheap: members GIN +
 *  idx_messages_convo_created via a LATERAL "latest message" per conversation.
 *
 *  enable_seqscan=off (session-level, NO transaction — a single autocommit SELECT
 *  only takes ACCESS SHARE, can't deadlock) forces the members GIN; the planner
 *  otherwise seq-scans all conversations (~2s). Same approach as loadInbox. On
 *  error the connection is destroyed so the GUC never leaks back into the pool. */
async function loadStalledConversations(agentId: string, companyId: string): Promise<StalledConvo[]> {
  const client = await pool.connect()
  let rows: Array<{
    conversation_id: string; kind: string; title: string | null
    last_message_id: string; last_author_id: string; last_author_name: string
    last_author_is_self: boolean; last_body: string; minutes_silent: string; recent_tail: string | null
  }>
  try {
    await client.query('SET enable_seqscan = off')
    const res = await client.query<{
      conversation_id: string; kind: string; title: string | null
      last_message_id: string; last_author_id: string; last_author_name: string
      last_author_is_self: boolean; last_body: string; minutes_silent: string; recent_tail: string | null
    }>(
      `WITH convos AS (
         SELECT c.id, c.kind, c.title
           FROM conversations c
          WHERE c.company_id = $2 AND c.members @> to_jsonb(ARRAY[$1::text])
       )
       SELECT co.id AS conversation_id, co.kind, co.title,
              m.id AS last_message_id, m.author_id AS last_author_id,
              COALESCE(p.name, m.author_id) AS last_author_name,
              (m.author_id = $1) AS last_author_is_self,
              LEFT(m.body, 160) AS last_body,
              (EXTRACT(EPOCH FROM (NOW() - m.created_at)) / 60)::int::text AS minutes_silent,
              (
                SELECT string_agg(t.line, E'\n' ORDER BY t.created_at ASC)
                  FROM (
                    SELECT COALESCE(tp.name, tm.author_id) || ': ' || LEFT(tm.body, 100) AS line, tm.created_at
                      FROM messages tm
                      LEFT JOIN participants tp ON tp.id = tm.author_id AND tp.company_id = $2
                     WHERE tm.conversation_id = co.id AND tm.kind = 'text'
                     ORDER BY tm.created_at DESC
                     LIMIT 6
                  ) t
              ) AS recent_tail
         FROM convos co
         JOIN LATERAL (
           SELECT * FROM messages mm
            WHERE mm.conversation_id = co.id AND mm.kind = 'text'
            ORDER BY mm.created_at DESC
            LIMIT 1
         ) m ON true
         LEFT JOIN participants p ON p.id = m.author_id AND p.company_id = $2
        WHERE m.created_at <= NOW() - ($3::double precision * INTERVAL '1 millisecond')
          AND m.created_at >= NOW() - ($4::double precision * INTERVAL '1 millisecond')
        ORDER BY m.created_at DESC
        LIMIT 10`,
      [agentId, companyId, STALL_MIN_MS, STALL_MAX_MS],
    )
    await client.query('RESET enable_seqscan')
    rows = res.rows
  } catch (err) {
    client.release(true) // destroy — never return a connection in unknown GUC state
    throw err
  }
  client.release()
  return rows.map((r) => ({
    conversationId: r.conversation_id,
    kind: r.kind,
    title: r.title,
    lastMessageId: r.last_message_id,
    lastAuthorId: r.last_author_id,
    lastAuthorName: r.last_author_name,
    lastAuthorIsSelf: r.last_author_is_self,
    lastBody: r.last_body,
    minutesSilent: Number(r.minutes_silent),
    recentTail: r.recent_tail ?? '',
  }))
}

/** Public entry point. Empty result = "nothing on this agent's
 *  plate"; callers fall back to the generic idle behavior. */
export async function gatherAgentAgenda(
  agentId: string,
  companyId: string,
  now: Date = new Date(),
): Promise<AgentAgenda> {
  const [cards, events, stalls] = await Promise.all([
    loadAssignedCards(agentId, companyId),
    loadDueEvents(agentId, companyId, now),
    loadStalledConversations(agentId, companyId).catch((e) => {
      console.warn('[agenda] loadStalledConversations failed', e instanceof Error ? e.message : e)
      return [] as StalledConvo[]
    }),
  ])
  return { cards, events, stalls }
}

/** Cerebellum classifier: given the agenda + persona, decide if a wake is
 *  justified. Strict JSON. Falls back to "actionable=false" (or the narrow
 *  deterministic fallback) on any error so a classifier outage doesn't burn
 *  brain calls on every heartbeat.
 *
 *  Routing (spec #17, ticket #20): resolves this agent's cerebellum route
 *  first. `remote` (the default, or a BYOA agent whose daemon/engine isn't
 *  currently available) calls a Chat-Completions-compatible model — the
 *  generalized `cerebellum-adapter.ts` when `CEREBELLUM_BASE_URL`/
 *  `CEREBELLUM_API_KEY` are configured, else today's plain OpenAI client
 *  (unchanged behavior). `byoa` is NEVER called out to from here — a
 *  byoa-routed agent's classification runs on the operator's own local
 *  engine via the daemon-poll path (`/runtime/agenda` + `/runtime/agenda/verdict`,
 *  mirroring `/runtime/inbox-triage/payload`); this function fails closed
 *  defensively if a caller reaches it anyway for a byoa-routed agent, rather
 *  than ever risking a remote call against an operator's unset OpenAI key. */
export async function classifyAgendaActionable(args: {
  persona: { name: string; role: string; style: string; model: string | null }
  companyId: string
  agenda: AgentAgenda
  /** Optional agent id — when provided, the ledger row is scoped to this
   *  agent so the per-agent spend rollup ("whose heartbeats burn the most")
   *  is queryable, AND it's what lets this function resolve the cerebellum
   *  route. Cloud callers (idle.ts) pass `personaId`; the BYOA endpoint
   *  passes `c.sub`. Older callers can omit it (always resolves `remote`). */
  agentId?: string
}): Promise<AgendaVerdict> {
  const { persona, companyId, agenda } = args
  const built = buildAgendaClassifierRequest({ persona, agenda })
  if (built.verdict) return built.verdict

  const route = args.agentId ? await resolveCerebellumRouteForAgent(args.agentId) : 'remote'
  if (route === 'byoa') {
    console.warn(`[agenda] classifyAgendaActionable called for byoa-routed agent ${args.agentId} — caller should use the /runtime/agenda daemon-poll path instead of the remote classifier`)
    return { actionable: false, focus: '', reason: AGENDA_CLASSIFIER_ERROR }
  }

  try {
    // Generalized remote adapter (#18) when configured — any OpenAI-Chat-
    // Completions-compatible provider, not just OpenAI. Unconfigured →
    // unchanged legacy behavior (plain OpenAI client via the ledger).
    const useCerebellumAdapter = cerebellumRemoteConfigured()
    const model = useCerebellumAdapter ? (env.CEREBELLUM_MODEL || env.OPENAI_MODEL_SUPPORT) : env.OPENAI_MODEL_SUPPORT
    const requestArgs = {
      model,
      instructions: built.instructions,
      input: built.input,
      text: { format: { type: 'json_object' } },
      // DeepSeek V4 Flash regularly spends the first ~300 tokens on hidden
      // reasoning. A 300-token cap then truncates the JSON to one character,
      // which made every minute-level BYOA agenda check fail closed. Leave
      // enough room for reasoning plus the small structured verdict.
      max_output_tokens: 2000,
      reasoning: { effort: 'minimal' },
    }
    // ponytail: the generalized-adapter path skips llm-ledger tracking (it
    // isn't a raw OpenAI client getTrackedLlmClient can wrap) — add if an
    // operator needs spend visibility for a non-default CEREBELLUM_* provider.
    let outputText: string
    if (useCerebellumAdapter) {
      const r = await cerebellumResponsesShim.create(requestArgs as never) as { output_text?: string }
      outputText = r.output_text ?? ''
    } else {
      const client = await getTrackedLlmClient({
        purpose: 'agenda',
        companyId,
        agentId: args.agentId ?? null,
        extras: {
          cards: agenda.cards.length, events: agenda.events.length, stalls: agenda.stalls.length,
          persona: persona.name,
        },
      })
      const r = await client.responses.create(requestArgs as never)
      outputText = r.output_text ?? ''
    }
    const parsed = parseAgendaVerdict(outputText)
    if (!parsed) throw new Error('agenda classifier returned no recoverable verdict')
    return coerceAgendaVerdict(parsed)
  } catch (e) {
    console.warn('[agenda] classifier failed', e instanceof Error ? e.message : e)
    return agendaDeterministicFallback(agenda)
  }
}

/** Render the agenda as the `backgroundBrief.body` we hand to the
 *  brain's turn. Includes enough specificity (ids, titles, slot
 *  times, agent_prompts) that the agent can act without re-querying,
 *  but stays terse — the agent will pull full card / event detail
 *  via the normal CLI tools if it needs them.
 *
 *  Note: the brain's default `background_scan` framing in turn.ts
 *  says "default to doing nothing," which is the right stance for
 *  generic activity scans. For agenda wakes that doctrine is too
 *  cautious — the cerebellum has already decided this is worth a
 *  brain call — so we open the body with an explicit "the system
 *  already triaged this; pick the most timely item and progress it"
 *  override. */
export function renderAgendaBrief(agenda: AgentAgenda, focus: string): string {
  const lines: string[] = [
    `This wake comes from your own agenda — Kanban cards assigned to you and Calendar slots that are due now. The system has already triaged that there is real work to progress here, so act: pick the most timely item, move it forward (write the card comment, ship the deliverable, send the DM, draft the doc), and call set_turn_status when done. Override the generic "default to doing nothing" stance for this wake.`,
    '',
  ]
  if (focus) {
    lines.push(`Focus from agenda triage: ${focus}`)
    lines.push('')
  }
  if (agenda.cards.length > 0) {
    lines.push(`Your active Kanban cards (${agenda.cards.length}):`)
    for (const c of agenda.cards) {
      const tag = c.assignee_id ? 'assigned to you' : '@you mentioned'
      lines.push(`- ${c.id}  [${c.board_title} / ${c.column_title}]  ${c.title}  (${tag})`)
      if (c.description) lines.push(`    ${c.description.replace(/\n/g, ' ').slice(0, 200)}`)
    }
    lines.push('')
    lines.push(`Inspect with: bash("cumora kanban show <board_id>") or bash("cumora card show <card_id>").`)
  }
  if (agenda.events.length > 0) {
    if (lines.length > 0) lines.push('')
    lines.push(`Calendar events in your current slot (${agenda.events.length}):`)
    for (const e of agenda.events) {
      lines.push(`- ${e.id}  at ${e.start_at}  ${e.title}`)
      if (e.agent_prompt) lines.push(`    prompt: ${e.agent_prompt.slice(0, 240)}`)
    }
  }
  if (agenda.stalls.length > 0) {
    if (lines.length > 0) lines.push('')
    lines.push(`Conversations that have gone quiet — possible follow-ups (${agenda.stalls.length}). FIRST decide if each is genuinely WAITING or already DONE. If the recent messages show the activity CONCLUDED or the thread socially closed (wrap-ups, closing / acknowledgement remarks, a result already reached, nothing pending, no one waiting on a specific next step) → it is FINISHED: do NOT post. Reviving a finished thread with a late message is noise, not teamwork. Only act when someone is plainly waiting:`)
    for (const s of agenda.stalls) {
      lines.push(`- ${s.conversationId} [${s.kind}] "${s.title ?? ''}" — ${s.lastAuthorIsSelf ? 'YOU spoke last' : `${s.lastAuthorName} spoke last`}, ${s.minutesSilent}m ago. Recent:`)
      for (const ln of (s.recentTail || s.lastBody).split('\n')) lines.push(`    ${ln}`)
    }
    lines.push(`If (and ONLY if) one is genuinely unfinished and someone is waiting: read the room (\`cumora messages <id>\`), then send AT MOST ONE message — answer it if it needs your answer, or a single brief nudge. Never nag, never pile on, never revive a finished thread.`)
  }
  return lines.join('\n')
}

/** Exported for unit-test access — never call from production code. */
export const __test = {
  DONE_COLUMN_PATTERNS,
  renderAgendaForClassifier,
}
