# Context

Single-context repo. Glossary only — no implementation detail, no specs.

## Terms

**Brain** — the LLM tier that runs an agent's actual reasoning turn (`OPENAI_MODEL` historically). Can run as a Cumora Cloud-managed pod or as a BYOA local engine.

**Cerebellum** — the lightweight classifier tier, distinct from the Brain: JSON classifiers, palette/gender inference, agenda triage, inbox-message-wake triage. Historically hardcoded to call OpenAI regardless of the agent's Brain routing.

**Cerebellum-summarizer** — the compaction / mid-turn-steer summarizer tier (`OPENAI_COMPACTION_MODEL`). A third tier, distinct from Cerebellum proper; not yet covered by Cerebellum Route.

**BYOA (Bring Your Own Agent)** — running an agent's Brain (and optionally Cerebellum) on the operator's own local CLI engine (Claude Code, Codex, Grok Build, Cursor Agent) instead of a Cumora Cloud-managed pod.

**Computer** — the `computers` table entity representing where an agent's Brain/Cerebellum execution happens: `cloud` (Cumora-managed pod) or `local`/`vps` (an operator-run BYOA daemon). Every agent (`participants` row) is assigned a `computer_id` and `engine`.

**Engine** — the specific local CLI adapter a BYOA Computer runs (`claude` | `codex` | `grok` | `cursor`), implementing the `EngineAdapter` interface (`classify`, `run`/`startSession`, `probe`).

**Cerebellum Route** — the deployment-wide setting deciding whether Cerebellum-tier calls go to a `remote` provider or the operator's `byoa` local engine, with automatic fallback to `remote` when the configured local engine isn't available on the calling agent's Computer.

**Team** — the tenant-level home grouping every member, human and agent, and owning the conversations, boards, documents, calendars, and workspaces they share.
_Avoid_: workspace / 工作区 (retired for this meaning), organization.

**Workspace** — a team-shared collaboration surface bound to exactly one real folder, and a folder belongs to at most one workspace; everyone in the workspace's member scope reads and writes it. Each team has exactly one default workspace whose member scope is the entire team.
_Avoid_: reusing this word for the tenant (see Team) or for an agent's own files (see Private Area).

**Association** — the link attaching a project, a board card, or a document to a workspace; participants of an associated item thereby join that workspace's member scope, alongside explicitly added members.

**Private Area** — an agent's own private file space (persona, memory, skills, scratch files), materialized either as the cloud virtual filesystem or as the local home directory; never visible to other agents. An agent's file activity is confined to its Private Area plus the Workspaces it is a member of.
_Avoid_: workspace, private workspace, 私有工作区.
