import type { ApiParticipant } from '@/api/client'
import type { Participant } from '@/types'

function sameTools(a: string[] | null | undefined, b: string[] | null | undefined): boolean {
  if (a == null || b == null) return a == null && b == null
  if (a.length !== b.length) return false
  return a.every((v, i) => v === b[i])
}

/** Field-level roster comparison (#143). Every Participant field is a
 *  primitive except `tools` (string[]), so "unchanged" means all
 *  primitives identical and the tools arrays element-wise identical.
 *  Used against freshly mapped rows so a quiet refresh can keep the old
 *  object reference instead of churning every identity in byId — the
 *  churn is what punched through every MessageRow memo each 60s tick. */
export function sameParticipant(a: Participant, b: Participant): boolean {
  return a.id === b.id
    && a.kind === b.kind
    && a.name === b.name
    && a.role === b.role
    && a.initial === b.initial
    && a.avatarBg === b.avatarBg
    && a.avatarUrl === b.avatarUrl
    && a.status === b.status
    && a.statusUpdatedAt === b.statusUpdatedAt
    && a.bio === b.bio
    && sameTools(a.tools, b.tools)
    && a.systemPrompt === b.systemPrompt
    && a.model === b.model
    && a.fastModel === b.fastModel
    && a.email === b.email
    && a.departedAt === b.departedAt
    && a.computerId === b.computerId
    && a.engine === b.engine
}

/** Merge a freshly fetched roster into the current byId, PRESERVING the
 *  old object reference for any participant whose fields are unchanged
 *  and dropping rows the fetch no longer returns. Returns the SAME
 *  record reference when nothing changed at all — zustand's selector
 *  equality then skips notifying subscribers entirely, so a steady-state
 *  60s refresh costs zero renders. `map` is the store's ApiParticipant →
 *  Participant normalization (fromApi), injected to keep this module
 *  free of store and client imports. */
export function mergeRoster(
  current: Record<string, Participant>,
  incoming: ApiParticipant[],
  map: (p: ApiParticipant) => Participant,
): Record<string, Participant> {
  const next: Record<string, Participant> = {}
  let changed = false
  for (const p of incoming) {
    const prev = current[p.id]
    const fresh = map(p)
    if (prev && sameParticipant(prev, fresh)) {
      next[p.id] = prev
    } else {
      next[p.id] = fresh
      changed = true
    }
  }
  if (!changed) {
    for (const id of Object.keys(current)) {
      if (!(id in next)) {
        changed = true
        break
      }
    }
  }
  return changed ? next : current
}
