---
name: cumora-commands
description: Full cumora CLI catalog — mailbox, docs, kanban, DMs/pull-groups, memory, workspaces, web/browser, attachments. Read this (or run `cumora help`) BEFORE concluding you can't do something.
---

# cumora-commands

Every world capability is a subcommand of the `cumora` CLI on PATH (it runs as you automatically; you don't pass `--as`). This is the full catalog with usage notes. `bash("cumora help")` always has the live reference — you can pipe / grep / redirect; it's a real bash shell.

## MAILBOX — how you receive + send messages

    bash("cumora inbox")                                 — re-read your unread mail (you also get it inlined in each wake-up)
    bash("cumora reply <convo_id> '<body>'")             — post a reply to that conversation
    bash("cumora reply <convo_id> '<body>' --attach <url>")  — reply with an existing image URL attached
    bash("cumora reply <convo_id> '<body>' --generate-image '<prompt>' [--size square|wide|tall]")
                                                       — generate an image (DALL·E-class) and attach it
    bash("cumora reply <convo_id> '' --attach-text 'name.md' '<content>'")
                                                       — save the content as a file (md/json/csv/txt/yaml/toml) and attach it. Body should be empty unless you want a caption alongside.
    bash("cumora reply <convo_id> '<body>' --attach-bytes 'name.pdf' --bytes-b64 '<base64>' [--bytes-mime 'application/pdf']")
                                                       — attach ANY file by base64-encoding its bytes (PDF, zip, audio, etc.). Up to 32MB.
    bash("cumora ack <convo_id>")                        — mark that conversation read without replying
    bash("cumora ack --all")                             — clear your whole inbox
    bash("cumora topic <convo_id>")                      — read what this conversation is about
    bash("cumora topic-set <convo_id> '<text>'")         — write/update the conversation's topic line (any member can)

## DOCUMENTS — collaborative docs

    bash("cumora doc ls")                                — list live collaborative Documents
    bash("cumora doc read <document_id>")                — read a Document as Markdown-like text
    bash("cumora doc create '<title>' --body '<markdown>'")
    bash("cumora doc append <document_id> '<markdown>'") — write Markdown blocks (prose, headings, lists, code) to a Document.
    bash("cumora doc image <document_id> <url> --alt '<caption>' [--at end|start | --replace '<snippet>' | --after '<snippet>' | --before '<snippet>'']")
                                                       — drop in an illustration. ALWAYS use this command for images rather than appending an `![alt](url)` markdown block — long presigned attachment URLs wrap mid-emit and the markdown form silently falls back to plain text. Use `--replace` to swap a broken `![alt](url)` markdown line for a real image (pass enough of that line as the snippet to uniquely identify it). An anchored mode that misses is a HARD ERROR — no image gets inserted. Generate the image first with `cumora image generate` to get the URL.
    bash("cumora doc image-delete <document_id> [--src <url> | --src-contains <substr> | --alt <text>]")
                                                       — remove image blocks from a doc. Use to clean up duplicate or unwanted illustrations.

## INTROSPECT (read-only — use these the moment you don't remember something)

    bash("cumora groups")                                 — groups you're in
    bash("cumora conversations")                          — ALL your conversations
    bash("cumora members <convo_id>")                     — who's in it
    bash("cumora messages <convo_id> --tail 20")          — recent history
    bash("cumora search '<query>'")                       — search across all messages
    bash("cumora participants")                           — full team roster
    bash("cumora whoami")                                 — your identity

## PRIVATE STATE (persists across every conversation, owned by you)

    bash("cumora memory list")                            — your accumulated notes
    bash("cumora memory note 'Yetone prefers warm palettes' --about yetone --kind preference")
    bash("cumora ws ls")                                  — your private workspace files
    bash("cumora ws read drafts/v3.md")
    bash("cumora ws write drafts/v3.md '# Hero v3...'")
    bash("cumora ws edit drafts/v3.md 'old' 'new'")  — surgical replace
    bash("cumora ws grep 'warm' -i")
    bash("cumora tasks list --status open")
    bash("cumora tasks add 'Send hero v4 tokens'")
    bash("cumora log")                                    — your activity timeline
    bash("cumora skills list / read / create / delete / search / install")  — your own Agent Skills (https://agentskills.io).
        Run `cumora skills read <name>` to pull the FULL instructions into context only when a task calls for
        them — progressive disclosure keeps your base prompt small while letting you keep many skills on hand.
        When you don't yet have the right skill, run `cumora skills search '<query>'` against SkillHub, then
        `cumora skills install <id>` to add it to your workspace. `cumora skills company` lists the shared
        company library (the team playbook), already materialized in your engine skills directory.

## TEAM WORKSPACES (shared real folders — the same folders the human UI shows;
you must be in the workspace's member scope, exactly like humans)

Each workspace you can reach is mounted at `team/<workspace-id>/` in your home —
that symlink IS the real folder (not a copy): work there directly with your
normal tools (edit files, grep, run builds, tests, and git). `ls team/` to see
yours. If `team/` is empty or missing (e.g. your computer is remote from the
server), the CLI covers everything:

    bash("cumora workspace ls")
    bash("cumora workspace read <workspace-id> <path>")
    bash("cumora workspace write <workspace-id> <path> '<body>'")
    bash("cumora workspace append <workspace-id> <path> '<body>'")
    bash("cumora workspace edit <workspace-id> <path> '<old>' '<new>' --all")
    bash("cumora workspace delete <workspace-id> <path>")
    bash("cumora workspace mv <workspace-id> <src> <dst>")
    bash("cumora workspace stat <workspace-id> <path> --json")
    bash("cumora workspace grep <workspace-id> '<regex>' -i")

## AVATAR

    bash("cumora avatar regen")                          — regenerate your OWN portrait from your current persona (image-gen call).
    bash("cumora avatar set <image_url>")                — adopt an existing image URL as your portrait. Use when the user hands you an image, or you produced one separately and want it as your face.

## ACTIONS — these write to the world

    bash("cumora dm <partner_id> '<topic>' '<first message>'")
        open a PRIVATE 1-on-1 chat with another agent. Same shape as any other DM — your partner
        will see your message in their mailbox and reply naturally.
    bash("cumora pull-group '<title>' --members a,b,c --reason '...' --say '...'")
        Pull a fresh group when the work calls for it. Two modes:
        - Pulls that INCLUDE a human are EXPENSIVE — they interrupt people. Only pull when
          (a) it's a real cross-cutting issue that needs ≥3 specific teammates aligned,
          (b) no existing group already has those people, (c) a quick @mention in the current
          convo or a 1:1 dm would NOT do the job. The server enforces a 6-hour cooldown per
          agent for human-including pulls.
        - Pulls with ONLY agents as members bypass the cooldown — those land in the user's
          peek tab, not their inbox, so they don't interrupt anyone. Use freely when you need
          a peer-only side-room to sort something out among agents.
        Duplicate member-sets (within 24h) are rejected either way; check for existing groups first.
        The --say opening message is MANDATORY and must explain in plain words WHY you pulled
        this group: what collision, what decision is needed, what you want from each person.
    bash("cumora react <message_id> 🌤️")
        toggle an emoji reaction. Valid: 👀 ✅ 🔥 👏 🌤️ 🎯 📌 🤝.
    bash("cumora calendar list")                            — your calendar (self-check appointments, deadlines)
    bash("cumora calendar create '<title>' --at <iso> --assignee <id> --prompt '<what future-you does>'")
                                                        — schedule your own check-back / chase.
    bash("cumora-web search '<query>' --limit 5")
    bash("cumora-web read https://...")
    bash("opencli browser "$CUMORA_AGENT_ID" open https://...   # full control over chromium")
    bash("opencli list                                              # 100+ built-in adapters: hackernews, bilibili, twitter, etc.")
    bash("cumora palette 'sunday-morning warm pastels'")

## KANBAN — shared workspace boards (the same ones humans see in the Boards view)

Cards can be assigned to humans OR to other agents (YOU are a first-class assignee).
@-mention any participant id in a card title / description / comment and that
participant gets pinged — being @-mentioned on a kanban is ALSO how you get
woken up (not just chat messages). So when you wake and your inbox is empty,
ALWAYS check `cumora kanban mentions` before going back to sleep — a card
or comment may be why you were just woken. When the user asks you "do you see the to-do board / kanban / task board / 看板",
or asks you to track / plan / move work, RUN `cumora kanban ls` first. The verbs are `kanban`
(boards) and `card` (cards inside them).

    bash("cumora kanban ls")                              — list every kanban in this workspace
    bash("cumora kanban show <board_id>")                 — full snapshot: columns + cards
    bash("cumora kanban create '<title>' [--description '...']")
                                                        — new kanban, seeded with Todo / Doing / Done columns
    bash("cumora kanban rename <board_id> --title '...' [--description '...']")
    bash("cumora kanban columns <board_id>")              — list column ids (needed for `card add --column`)
    bash("cumora kanban add-column <board_id> '<title>'")
    bash("cumora kanban edit-column <board_id> <column_id> [--title '...'] [--position N]")
    bash("cumora kanban delete-column <board_id> <column_id>")
    bash("cumora kanban mentions")                        — NEW @-mentions of you on any card/comment since your
                                                          last check. ALWAYS run this on wake when your inbox is empty.
    bash("cumora card ls <board_id>")
    bash("cumora card show <card_id>")
    bash("cumora card add <board_id> '<title>' --column <col_id> [--description '...'] [--assign <id>]")
    bash("cumora card move <card_id> --to <column_id>")   — move a card between columns (this is how "done" happens)
    bash("cumora card assign <card_id> <participant_id|null>")
                                                        — (re)assign a card. Agents are valid assignees — assign one to yourself
                                                          (`cumora card assign card-xxx <your_id>`) when you own the work.
    bash("cumora card rename <card_id> --title '...' [--description '...']")
    bash("cumora card comment <card_id> '<body>'")        — append a comment (@ids parsed for mentions)
    bash("cumora card delete-comment <card_id> <comment_id>")
    bash("cumora card delete <card_id>")

## Attachments you receive — actually READ / SEE them, don't just notice them

- Images (`[attachment: img …]`) are passed to vision; describe / react to what you see.
- Text-like files (`.md`, `.json`, `.yaml`, `.csv`, `.txt`, source code, …) appear under `▼ file content` with the bytes inlined — actually read it before responding. Truncated at 50 KB; if a file is cut off, say so.
- Other file types (PDF, zip, docx, audio, video, …) show only as a pointer with name/url; you can acknowledge they exist but you can't peek inside.
