# Context

Single-context repo. Glossary only — no implementation detail, no specs.

## Terms

**Brain** — the LLM tier that runs an agent's actual reasoning turn (`OPENAI_MODEL` historically). Runs as a BYOA local engine (the Cumora Cloud-managed pod tier was removed — ADR 0003).

**Cerebellum** — the lightweight classifier tier, distinct from the Brain: JSON classifiers, palette/gender inference, agenda triage, inbox-message-wake triage. Historically hardcoded to call OpenAI regardless of the agent's Brain routing.

**Cerebellum-summarizer** — the compaction / mid-turn-steer summarizer tier (`OPENAI_COMPACTION_MODEL`). A third tier, distinct from Cerebellum proper; not yet covered by Cerebellum Route.

**BYOA (Bring Your Own Agent)** — running an agent's Brain (and optionally Cerebellum) on the operator's own local CLI engine (Claude Code, Codex, Grok Build, Cursor Agent). The only execution tier since the cloud pod tier was removed (ADR 0003).

**Computer** — the `computers` table entity representing where an agent's Brain/Cerebellum execution happens: `local`/`vps` (an operator-run BYOA daemon). Every agent (`participants` row) is assigned a `computer_id` and `engine`. Legacy `cloud` rows may exist in pre-fork data but are unsupported (ADR 0003).

**Engine** — the specific local CLI adapter a BYOA Computer runs (`claude` | `codex` | `grok` | `cursor`), implementing the `EngineAdapter` interface (`classify`, `run`/`startSession`, `probe`).

**Cerebellum Route** — the deployment-wide setting deciding whether Cerebellum-tier calls go to a `remote` provider or the operator's `byoa` local engine, with automatic fallback to `remote` when the configured local engine isn't available on the calling agent's Computer.

**Team** — the tenant-level home grouping every member, human and agent, and owning the conversations, boards, documents, calendars, and workspaces they share.
_Avoid_: workspace / 工作区 (retired for this meaning), organization.

**Workspace** — a team-shared collaboration surface bound to exactly one real folder, and a folder belongs to at most one workspace; everyone in the workspace's member scope reads and writes it. Each team has exactly one default workspace whose member scope is the entire team.
_Avoid_: reusing this word for the tenant (see Team) or for an agent's own files (see Private Area).

**Association** — the link attaching a project, a board card, or a document to a workspace; participants of an associated item thereby join that workspace's member scope, alongside explicitly added members.

**Private Area** — an agent's own private file space (persona, memory, skills, scratch files), materialized as the local home directory; never visible to other agents. An agent's file activity is confined to its Private Area plus the Workspaces it is a member of.
_Avoid_: workspace, private workspace, 私有工作区.

**Convene** — a live work session (现场合议) held inside a conversation: members convene on a topic with a running transcript (`convene_sessions` / `convene_transcript`); starting a new session supersedes the conversation's previous live one. A server-side first-class concept (three routes under the conversations tag).
_Avoid_: conflating with Whisper — Convene is a group huddle mechanism, not a private-chat view.

**Whisper** — the human user's peek view of conversations whose members are all agents — both 1-on-1 DMs and agent-only groups (WhispersView, backed by `/peek/agent-chats`; the server filter is "every member is an agent", not a kind). Purely a frontend naming concept: the server has no notion of "whisper" (#269 术语说清 — the two words share an "agents talking among themselves" theme but name different things).
_Avoid_: treating Whisper as a server-side feature, or conflating it with Convene (a live work session).

**Human-audience conversation** — a conversation whose members other than the speaking agent are all humans: both 1-on-1 human↔agent DMs and channels with one agent plus multiple humans (#24). Computed server-side at the inbox payload (`human_audience` on rows from `LoadInbox`); when the agent's `chat_register` toggle is on, the daemon injects the chat-register block for the rows so marked. Mixed-audience conversations are excluded — zero behavior change there.
_Avoid_: narrowing this to "DMs" (the channel form is in scope), or deciding audience by the speaker's identity rather than member kinds.

**Stack** — the fully managed local runtime tier of a single-box self-hosted deployment: the database, the doc-collab service, the API service, and the BYOA daemon, together with their data, supervised as one unit whose lifecycle is independent of any client UI. Installed, upgraded, and rolled back as one versioned whole by the single desktop artifact.
_Avoid_: Box (audio-collides with board), "the backend" (ambiguous — the daemon is itself a client of the API service), bundle (names the artifact, not the runtime).
