# Workspace owns the word

When the shared-folder Workspace concept was designed (2026-08-25), "workspace" was already live in three unrelated meanings — the tenant in UI copy (`companies`, called 工作区 throughout the locales), the per-agent private virtual filesystem (`agent_workspace`, `cumora ws`), and the BYOA home's `workspace/` directory — with the same locale file using 工作区 for both the tenant (`zh-CN.ts` workspace-switcher strings) and the agent's private files (offboarding copy). We decided the new concept exclusively owns workspace / 工作区: the tenant is renamed 团队 (Team) and the agent-private three forms unify as one concept, 私有区 (Private Area).

## Considered Options

- **New concept avoids the word** (minimal churn) — rejected: the flagship collaborative concept deserves the word, and avoiding it would leave the three-way collision only half-fixed.
- **Just split the old meanings in the glossary first, decide later** — rejected: it duplicates the work the exclusive ownership decision settles once.

## Consequences

- User-facing copy must stop calling the tenant 工作区 (code identifiers like the `agent_workspace` table or the `cumora ws` command are implementation and may follow later).
- Old screenshots, issues, and git history will show 工作区 meaning the tenant; `CONTEXT.md` `_Avoid_` lines on Team / Workspace / Private Area are the enforcement point.
