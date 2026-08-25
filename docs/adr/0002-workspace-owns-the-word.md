# 0002 — Workspace owns the word "workspace"

## Status

Accepted

## Context

When the shared-folder Workspace concept was designed (2026-08-25),
"workspace" was already live in three unrelated meanings — the tenant in
UI copy (`companies`, called 工作区 throughout the locales), the per-agent
private virtual filesystem (`agent_workspace`, `cumora ws`), and the BYOA
home's `workspace/` directory — with the same locale file using 工作区 for
both the tenant (workspace-switcher strings) and the agent's private files
(offboarding copy).

## Decision

The new concept exclusively owns workspace / 工作区. The tenant is renamed
团队 (Team) in user-facing vocabulary, and the agent-private three forms
unify as one concept, 私有区 (Private Area). Renames stay at the copy
level: locale keys, table names, and CLI commands are implementation and
may follow later. The `CONTEXT.md` `_Avoid_` lines on Team / Workspace /
Private Area are the enforcement point.

Rejected alternatives: having the new concept avoid the word (minimal
churn, but leaves the three-way collision only half-fixed); splitting the
old meanings in the glossary first and deciding later (duplicates the work
that exclusive ownership settles once).

## Consequences

- User-facing copy must stop calling the tenant 工作区.
- Old screenshots, issues, and git history will show 工作区 meaning the
  tenant; the glossary `_Avoid_` lines are the pointer back to this ADR.
