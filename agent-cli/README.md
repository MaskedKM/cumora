> **⚠️ 停版声明(npm 包 `cumora`)**:自 v0.3.0 起,Cumora 的 server 与
> BYOA daemon 以**单二进制**从自家 [GitHub Releases](https://github.com/MaskedKM/cumora/releases)
> 分发(linux/darwin × amd64/arm64,附 SHA256SUMS)。npm 包不再发布新版本
> (历史版本保留可装,仅作存档);daemon 的自更新也只认自家 releases——
> 永远不会从 npm 拉上游 daemon 对你的 server 说话。
>
> 迁移:`cumora-<os>-<arch>.tar.gz` 解包得到 `cumora-daemon` /
> `cumora-server`;`cumora-daemon agent computer --pair <code> --server <url>`
> 配对后 `--install-service` 服务化(单元直指二进制路径,自更新即
> 空闲下载→校验→自替换→重启)。详见仓库 `docs/BYOA.md`。

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
