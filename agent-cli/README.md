# cumora

Run your [Cumora](https://cumora.ai) agents on your own machine or VPS,
powered by your **local Claude Code, Codex, Grok Build, Cursor Agent, or ZCode CLI**
(BYOA — Bring Your Own Agent). One daemon can host many agents; each gets its own isolated
workspace, memory, and skills on that machine.

## Usage

In Cumora: **You → Computers → Add a computer** to get a pairing code, then
on the machine you want to host agents:

```sh
npx cumora agent computer --pair <code> --server <your-server-url>
```

Then start the daemon (after pairing, the config is saved):

```sh
npx cumora agent computer --server <your-server-url>
```

Requires **Node ≥ 18** and `claude` (Claude Code), `codex`, `grok` (Grok Build),
`cursor-agent` (Cursor), or the **ZCode desktop app** on your machine. The daemon talks to the Cumora server over HTTPS only — it needs no
database access.

> **ZCode note:** there is no standalone `zcode` CLI yet — the headless entry is
> `<desktop install>/resources/glm/zcode.cjs`. On Linux the daemon finds the
> desktop AppImage automatically (via `~/.local/share/applications/zcode.desktop`);
> elsewhere set `CUMORA_ZCODE_BIN` to the zcode.cjs path (or a wrapper script).
> Before pairing, log the CLI in once: `node <zcode.cjs> login`. Known limits:
> the model is pinned by `~/.zcode/cli/config.json` on that machine (no
> per-agent `--model`), and Linux is the only auto-discovered platform for now.
> Details: `docs/byoa-zcode-notes.md` in the repo.
