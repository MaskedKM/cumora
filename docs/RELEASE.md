# Release Manual

How to cut a new desktop release of Cumora.

## TL;DR

```bash
# 1. Bump the version in package.json
npm version patch       # → 0.1.0 → 0.1.1   (creates the tag locally)

# 2. Push the tag — GitHub Actions does the rest
git push origin main --tags
```

The push to a `v*` tag fires the desktop release workflow:

- **`.github/workflows/release.yml`** in this repo → dispatches to
  `yetone/cumora-releases`, which builds + signs + publishes the
  Electron app for macOS (arm64 + Intel), Windows, and Linux. Final
  artifacts land at https://github.com/yetone/cumora-releases/releases.

It does **not** deploy the API server. Backend production deploys are an
explicit, separately approved action; a desktop tag must never silently mutate
the backend.

The auto-updater in the desktop app reads from `cumora-releases`, so
once the release workflow finishes (~15–20 minutes), running clients
will pick it up on their next periodic update check.

## Backend release (self-hosted)

Every push to `main` runs `.github/workflows/build.yml`: both TypeScript
projects, the big-brain and tracked-LLM guards, unit tests, and the
Postgres/Redis integration suite must pass. It does not build or push any
images — the GKE deploy pipeline was removed with the rest of the Cumora
Cloud machinery (ADR 0003). Deploy the server on your own box however you
run it today (tsx from a checkout, or a locally built image from
`server/docker/cumora-server.Dockerfile`); a Go single-binary release
pipeline replaces this arrangement during the migration (ADR 0004).
## What the release workflow does

1. Matrix-builds the Electron app on four runners (macOS arm64,
   macOS Intel, Windows, Linux).
2. On macOS, imports the Developer ID cert into a temporary keychain,
   signs the app bundle, and notarises via the Apple credentials in
   GitHub Secrets.
3. Uploads platform-specific artifacts (DMG, ZIP, EXE, AppImage, DEB,
   `latest*.yml` autoupdate feeds, blockmaps).
4. Merges the per-arch `latest-mac.yml` files so one feed advertises
   both arm64 and Intel.
5. Generates a user-friendly changelog via the OpenAI API from the
   commit list between the previous tag and this one.
6. Mirrors everything to the `cumora-updates` Cloudflare R2 bucket
   (only when R2 secrets are configured — optional).
7. Creates the GitHub Release with the artifacts attached and the
   generated changelog as the body.
8. Posts an announcement to the Discord release channel (only when the
   webhook is configured — optional).

## One-time setup (already done; reference only)

### Required GitHub Secrets

On `yetone/cumora`:

| Name | Purpose |
|------|---------|
| `RELEASES_REPO_TOKEN` | Fine-grained PAT scoped to `yetone/cumora-releases`. Needs `Actions: write`. |

On `yetone/cumora-releases`:

| Name | Purpose |
|------|---------|
| `CUMORA_REPO_TOKEN`             | Fine-grained PAT scoped to `yetone/cumora`. Needs `Contents: read`. |
| `MAC_CERTIFICATE_P12`           | Base64-encoded Developer ID Application cert (`.p12`). `base64 -i Certificates.p12 \| pbcopy`. |
| `MAC_CERTIFICATE_PASSWORD`      | Password protecting the `.p12` above. |
| `APPLE_ID`                      | Apple Developer account email. |
| `APPLE_APP_SPECIFIC_PASSWORD`   | App-specific password for notarisation. Generated at appleid.apple.com → Sign-In and Security → App-Specific Passwords. |
| `APPLE_TEAM_ID`                 | 10-char Team ID from developer.apple.com → Account → Membership. |
| `OPENAI_API_KEY`                | Used to generate the changelog. |
| `R2_ACCESS_KEY_ID`              | (optional) R2 mirror for the `cumora-updates` bucket. |
| `R2_SECRET_ACCESS_KEY`          | (optional) R2 mirror credential. |
| `CLOUDFLARE_ACCOUNT_ID`         | (optional) R2 endpoint scope. |
| `DISCORD_RELEASE_WEBHOOK_URL`   | (optional) Discord channel webhook for release announcements. |

If any of the optional secrets are unset, the workflow skips that step
and still succeeds.

### One-time Cumora-side wiring

- `build.publish` in `package.json` points at `yetone/cumora-releases`,
  so the in-app auto-updater knows where to look.
- `build.mac.notarize.teamId` reads `APPLE_TEAM_ID` from the workflow
  environment.
- `build/entitlements.mac.plist` declares the hardened-runtime
  entitlements Electron needs (JIT, network access, dyld vars).

## Manual rebuild of a past release

If a previous release needs a re-roll (signing failed, missing artifact,
etc.), use the `workflow_dispatch` form on `yetone/cumora-releases`:

1. Go to https://github.com/yetone/cumora-releases/actions/workflows/release.yml
2. **Run workflow** → enter:
   - `ref` = the tag from this repo (e.g. `v0.1.0`)
   - `version` = the bare version (e.g. `0.1.0`)
3. The workflow re-builds and overwrites the existing release artifacts.

## Common issues

- **macOS notarisation fails.** Most commonly `APPLE_APP_SPECIFIC_PASSWORD`
  was rotated or the cert is expired. Check
  `https://appleid.apple.com` and `Keychain Access` on a Mac with the
  cert installed.
- **Build runs but no GitHub Release is created.** The publish job
  requires `permissions: contents: write` which is already set in the
  workflow. If you forked the repos, make sure that permission is also
  granted on your fork.
- **`latest-mac.yml` mentions only one architecture.** One of the two
  Mac runners failed before producing the yml. Look at the `Upload
  build artifacts` step on `build-mac-arm64` / `build-mac-x64`.
- **The desktop app doesn't see the update.** The autoupdater polls
  every 10 minutes (default for `electron-builder`). Force it from the
  app menu or restart.
