# 0003 — Hard fork, single-box self-hosting

## Status

Accepted (2026-08-25)

## Context

This repo is a fork of `yetone/cumora` that has been syncing upstream
(`scripts-personal/sync-upstream.sh`) — recent merges carried the Apple
auth fix, Windows BYOA fixes, and the v0.2.1/v0.2.2 releases. The codebase
also carries the full Cumora Cloud machinery: k8s pod orchestration for
managed Brain pods, GKE deployment, trial/billing/shipping maintenance.
In practice this deployment runs on a single box, self-hosted, with every
agent on a BYOA local engine; the cloud pod path is unused.

## Decision

Stop syncing upstream. The project is managed entirely on this fork, and
upstream fixes are adopted only by manual cherry-pick when they matter.
The deployment target is single-box self-hosting: one machine runs the
API, background jobs, and database; agents run exclusively through BYOA
local engines. The cloud machinery (k8s orchestration, GKE deploy, trial
sweep, shipping) is deleted rather than refactored. All client surfaces
stay first-class: web SPA, Electron, iOS/Android via Capacitor, and FCM
push.

Rejected alternatives: keep syncing upstream and constrain refactors to
merge-friendly moves (sacrifices the rewrite this repo actually needs);
freeze sync during the rewrite and reassess afterwards (the merge debt
comes due all at once and the outcome would be the same fork anyway).

## Consequences

- Upstream security/platform fixes no longer arrive; they must be
  tracked manually.
- `sync-upstream.sh` and the upstream remote become historical; CI that
  assumes the upstream relationship (if any) should be pruned with the
  cloud machinery.
- Deleting the cloud pod path means `cloud` computers are no longer a
  supported `Computer` form; BYOA (`local`/`vps`) is the only supported
  execution tier.
