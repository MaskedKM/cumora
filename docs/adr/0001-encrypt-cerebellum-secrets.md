# 0001 — Encrypt Cerebellum secret settings with a dedicated master key

## Status

Accepted

## Context

Cerebellum Route settings (remote provider, base URL, API key, model, local-engine
preference) are moving from `.env`-only configuration into a UI-editable, DB-backed
`app_settings` store — the same table that already backs `waitlist_enabled` and
`signups_paused`.

`app_settings` stores those existing values in plaintext. One of the new fields,
the remote provider's API key, is a credential rather than a toggle.

## Decision

The API key field is encrypted at rest (AES-256-GCM) before being written to
`app_settings`, using a new deployment-level master key (`CUMORA_SECRETS_KEY`,
an env var). The other four Cerebellum Route fields (provider, base URL, model,
local-engine) remain plaintext — they carry no credential value.

The API key is write-only from the UI's perspective: reads return only whether a
key is configured (plus a masked suffix), never the decrypted value.

## Consequences

- A new env var (`CUMORA_SECRETS_KEY`) is required at deploy time — the one
  remaining piece of secret configuration that can't move into the UI, since the
  encryption key itself can't live next to the ciphertext it protects.
- Losing `CUMORA_SECRETS_KEY` (e.g. rotating it without a migration) makes any
  previously-saved API key unrecoverable — the UI will need to prompt for
  re-entry in that case rather than silently failing.
- This is deliberately simpler than a KMS/Vault integration: self-hosted,
  single-admin deployments have no external secret-management service to
  delegate to, and the DB is already inside the same trust boundary as `.env`.
  Revisit if/when this project takes on multi-tenant or compliance requirements.
