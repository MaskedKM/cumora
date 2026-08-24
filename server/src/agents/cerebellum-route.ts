/**
 * Cerebellum route resolution (spec #17, ticket #19).
 *
 * The "cerebellum" tier (agenda triage today; other lightweight JSON
 * classifiers later) can run against a remote OpenAI-Chat-Completions-
 * compatible provider, or against the operator's own local BYOA engine via
 * the daemon's `EngineAdapter.classify()` (see agents/computer/engine.ts).
 * `CEREBELLUM_ROUTE` picks the deployment-wide default; this module resolves
 * that default down to a PER-AGENT decision, because whether `byoa` is
 * actually usable depends on the specific agent's assigned Computer (a
 * Cloud-managed agent has none; a paired one might be offline or missing the
 * configured engine).
 *
 * This ticket wires no call sites — {@link resolveCerebellumRouteForAgent} is
 * the primitive a future BYOA daemon-poll path (and, per #17, the `agenda`
 * call site) will consume.
 */
import { pool } from '../db/pool.js'
import { env } from '../env.js'

export type CerebellumRoute = 'remote' | 'byoa'

/** Just enough about an agent's assigned Computer to decide cerebellum
 *  routing. `null` means the agent has no Computer to route local calls to
 *  (Cloud-managed, unassigned, or its Computer was revoked). */
export interface CerebellumComputerInfo {
  status: 'online' | 'offline' | 'busy'
  available_engines: string[]
}

/** Pure decision, per #17/#19's fallback rules:
 *  - `route !== 'byoa'` → always `remote`.
 *  - `byoa` resolves to `byoa` only if the agent has an ONLINE Computer that
 *    advertises `localEngine`; any other case (no Computer, offline/busy,
 *    engine missing) falls back to `remote` so a temporarily-unavailable
 *    local engine never silently starves an agent of its cerebellum calls. */
export function resolveCerebellumRoute(args: {
  route: CerebellumRoute
  localEngine: string
  computer: CerebellumComputerInfo | null
}): CerebellumRoute {
  if (args.route !== 'byoa') return 'remote'
  if (!args.computer) return 'remote'
  if (args.computer.status !== 'online') return 'remote'
  if (!args.computer.available_engines.includes(args.localEngine)) return 'remote'
  return 'byoa'
}

/** DB-backed wrapper future call sites use directly: looks up the agent's
 *  assigned (non-revoked) Computer, if any, and applies
 *  {@link resolveCerebellumRoute} against the deployment's CEREBELLUM_* env.
 *  Skips the DB round-trip entirely when the deployment isn't even on the
 *  `byoa` route. */
export async function resolveCerebellumRouteForAgent(agentId: string): Promise<CerebellumRoute> {
  if (env.CEREBELLUM_ROUTE !== 'byoa') return 'remote'
  const { rows } = await pool.query<CerebellumComputerInfo>(
    `SELECT c.status, c.available_engines
       FROM participants p
       JOIN computers c ON c.id = p.computer_id
      WHERE p.id = $1 AND p.kind = 'agent' AND c.revoked_at IS NULL
      LIMIT 1`,
    [agentId],
  )
  return resolveCerebellumRoute({
    route: env.CEREBELLUM_ROUTE,
    localEngine: env.CEREBELLUM_LOCAL_ENGINE,
    computer: rows[0] ?? null,
  })
}
