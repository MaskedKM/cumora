package help

import (
	"strings"

	agent "github.com/MaskedKM/cumora/apps/server-go/internal/agent"
)

// Domain:域子包接收器——嵌入 agent.Service(内核),方法体与拆包前逐字
// 对齐(#140 刀法)。
type Domain struct {
	*agent.Service
}

// cliHelpText:TS cli.ts cmdHelp 的静态正文,由构建侧脚本从源码机械
// 提取(模板字面量解除转义)生成,保证逐字节对齐;§ 是反引号占位符
// (Go 原始字符串不能内嵌反引号)。
const cliHelpText = `cumora — introspect the whole app + manage your private workspace, memory, tasks, log

USAGE:
  cumora <subcommand> [args...] [flags...]

MAILBOX  (this is how you receive + send messages):
  inbox [<convo_id>]                — list new messages directed at you, grouped by conversation
  ack <convo_id>                    — mark that conversation read up to NOW (clear from inbox)
  ack --all                         — ack every conversation currently in your inbox
  mute <convo_id> [--for 1h|1d|1w] — stop delivery from a group (direct @mentions and quote-replies still arrive)
  mute <convo_id> --until <iso>     — stop delivery until a wall-clock time; omit duration to mute forever
  mute list                         — show your active muted groups and expiry times
  follow <convo_id>                 — resume normal delivery from a muted group
  ship list                         — list feature contracts and evidence-square progress
  ship show <feature_id>            — inspect invariants, squares, releases, friction, regressions
  ship create "<title>" --problem "..." --outcome "..." --contract "..." [--builders a,b]
  ship square <feature_id> <square_id> <running|passed|failed|waived> [--evidence "..."] [--notes "..."]
  ship friction <feature_id|none> "<title>" [--description "..."] [--severity low|medium|high|critical]
  ship regression <feature_id> "<title>" [--command "..."] [--expected "..."]
  reply <convo_id> "<body>"         — post a message to that conversation as you
  reply <convo_id> "<body>" --quote <msg_id>
                                    — post as a quote-reply to <msg_id> (same convo); inbox + messages
                                      render the quoted-original inline so the room knows the context
  thread <convo_id> <root_msg_id>   — list every direct reply to one message (the thread under <root_msg_id>)
  topic <convo_id>                  — read the conversation's topic line
  topic-set <convo_id> "<text>"     — write/update the topic (any member can; empty body clears it)
  rename <convo_id> "<title>"       — rename a group (members only; groups only)

INTROSPECTION:
  whoami [--as <id>]
  participants [--kind agent|human]
  conversations [--as <id>]
  groups [--as <id>]
  directs [--as <id>]
  members <convo_id>
  messages <convo_id> [--tail N] [--thread <root_msg_id>]
  search <query> [--in <convo_id>] [--limit N]
  convening <convo_id>
  tools-log [--agent <id>] [--limit N]
  participants-status

TEAM WORKSPACES  (shared real folders; same membership as the human UI;
  mounted at team/<workspace-id>/ in your home when your computer is local):
  workspace ls [--as <id>]
  workspace read <workspace-id> <path> [--as <id>]
  workspace write <workspace-id> <path> <body> [--as <id>]
  workspace append <workspace-id> <path> <body> [--as <id>]
  workspace edit <workspace-id> <path> <old> <new> [--all] [--as <id>]
  workspace delete <workspace-id> <path> [--as <id>]
  workspace mv <workspace-id> <src> <dst> [--as <id>]
  workspace stat <workspace-id> <path> [--json] [--as <id>]
  workspace grep <workspace-id> <pattern> [-i] [--json] [--as <id>]

PRIVATE TO EACH AGENT  (these write/read state owned by --as):
  memory list [--as <id>] [--about <subject>] [--kind <kind>] [--limit N] [--in <convo>] [--all]
  memory note <body> [--as <id>] [--about <subject>] [--kind <kind>] [--in <convo>]
  memory pin <id>
  memory delete <id>

  log [--as <id>] [--limit N]
  log note <body> [--as <id>]

  ws ls [--as <id>]                            # your Private Area (agent-private files)
  ws read <path> [--as <id>]
  ws write <path> <body> [--as <id>]
  ws edit <path> <old> <new> [--all]           # surgical replace; default fails if old not unique
  ws grep <pattern> [-i]                       # regex across all your files
  ws delete <path> [--as <id>]

  tasks list [--as <id>] [--status open|doing|done]
  tasks add <title> [--as <id>]
  tasks set <task_id> <status>     # status ∈ open|doing|done|dropped

CALENDAR  (shared schedule + your own self-scheduling tool):
  # This is also how you SCHEDULE YOURSELF. Set --assignee to your own id
  # and the dispatcher will wake YOU at --at with --prompt as the brief.
  # Add --every for recurring schedules. Use this whenever you'd otherwise
  # tell a user "I'll do X later / tomorrow / every morning" — instead of
  # promising to come back, schedule the wake so future-you actually does.
  calendar list [--as <id>] [--all] [--status active|paused|done|cancelled]
                                   # default scope = events assigned to OR created by --as
  calendar create "<title>" --at <iso> [--assignee <id>] [--prompt "..."]
                                       [--in <convo_id>] [--every daily|weekly|monthly|yearly]
                                       [--interval N] [--byweekday 0,1,2,3,4]
                                       [--until <iso>] [--count N]
                                       [--kind personal|agent_task]
                                       [--remind <minutes>] [--remind-channel toast|email|both]
                                       [--private]
                                   # --assignee <self_id> + --prompt "..." = give future-you
                                   #   a wake-up with that prompt as the brief
                                   # --every daily|weekly|monthly|yearly = recurring schedule;
                                   #   pair with --interval / --byweekday / --until / --count
                                   # --remind 10 fires a heads-up 10 min before each occurrence
                                   # --private hides the row from everyone except its creator
                                   #   and assignee (company owner can still see private rows
                                   #   that involve an agent). Use it for personal reminders
                                   #   you don't want to clutter the shared calendar.
                                   # when start_at fires, the prompt is posted into <convo_id>
                                   # (or the assignee's DM with you) and the assignee is woken
  calendar update <event_id> [--title "..."] [--at <iso>] [--status active|cancelled|done]
                             [--private | --public]                # flip the privacy flag
  calendar run-now <event_id>      # dispatch an event immediately
  calendar dispatches <event_id>   # inspect dispatch history
  calendar cancel <event_id>       # stop firings without dropping history
  calendar delete <event_id>       # hard delete (also wipes dispatch history)

ACTIONS  (each writes to the world, not just your private state):
  dm <partner_id> <topic> <opening>                open a private 1-on-1 chat with another agent
  pull-group <title> --members a,b,c --reason "..." --say "..."   create a new group + post first msg
  invite <convo_id> <member_id>                    pull a teammate into a group you're in
  leave <convo_id>                                 leave a group (no-op for direct chats)
  kick <convo_id> <member_id>                      remove a member from a group you're in

KANBAN  (shared boards — the same ones humans see in the Boards view):
  kanban ls                                          list every kanban board in this workspace
  kanban show <board_id>                             full snapshot: columns + cards
  kanban create "<title>" [--description "..."]     new board, seeded with Todo/Doing/Done columns
  kanban rename <board_id> --title "..."             rename / re-describe a board
       [--description "..."]
  kanban columns <board_id>                          list column ids — needed for §card add --column§
  kanban add-column <board_id> "<title>"             append a new column to a board
  kanban edit-column <board_id> <column_id>          rename / reorder a column
       [--title "..."] [--position N]
  kanban delete-column <board_id> <column_id>        delete a column and its cards
  kanban delete <board_id>                           drop the board (and its columns + cards)
  kanban mentions [--peek] [--json]                  list NEW cards/comments where someone @ed YOU since
                                                     your last check. Run this on every wake when your
                                                     inbox is empty — you may have been pinged here.
                                                     Advances a read cursor unless --peek is passed.

  card ls <board_id>                                list every card in a board
  card show <card_id>                               full card detail + comments
  card add <board_id> "<title>" --column <col_id>   create a card
       [--description "..."] [--assign <id>]
  card move <card_id> --to <column_id>              move a card between columns (the way "done" happens)
  card claim <card_id>                              ATOMICALLY claim a card before working it (exclusive;
                                                     fails if someone else already holds it → move on)
  card assign <card_id> <participant_id|null>       (re)assign a card (agents and humans both work)
  card rename <card_id> --title "..."               edit a card's title
       [--description "..."]                        and/or its description
  card comment <card_id> "<body>"                   append a comment (Markdown OK, @ids parsed)
  card delete-comment <card_id> <comment_id>         delete your own comment
  card delete <card_id>                             drop a card

  @mention any participant id (§@iris§, §@yetone§) in a card title /
  description / comment — the renderer will chip them and toast the
  recipient(s) so a human or another agent gets pinged. Agents are
  first-class assignees too — assign a card to @iris and she'll see it
  via §cumora card show§ exactly the way a human does in the UI.

CONTACTS  (workspace + email — use BEFORE assuming you know a name):
  contacts [<query>] [--as <id>]                   list everyone in this workspace (agents +
                                                   humans + external mail correspondents),
                                                   with each teammate's role/function.
                                                   With a query: substring-search name/id/email/role
                                                   (e.g. "designer"). No match → ASK the user for the
                                                   address; DO NOT silently skip the request.

EMAIL  (real external mail — every agent has an address):
  email whoami [--as <id>]                         your own email address
  email contacts [<query>] [--as <id>]             same as top-level "contacts" (email-namespaced
                                                   alias kept for back-compat)
  email inbox [--unread] [--limit N] [--as <id>]   email threads you're on, latest first
  email show <conversation_id> [--tail N]          full thread (all messages in order)
  email send --to <addr|id>[,...] [--cc <...>] --subject "..." --body "..."
  email reply <message_id> --body "..." [--cc <...>]
                                                   threaded reply (sets In-Reply-To / References)

DOCUMENTS  (live collaborative docs — humans + agents edit the same page):
  doc ls [--as <id>]                               list docs in the workspace
  doc create "<title>" [--body "<markdown>"]       create a doc; body is Markdown
  doc read <document_id>                           read doc as Markdown-like text
  doc append <document_id> "<markdown>"            append Markdown blocks
  doc prepend <document_id> "<markdown>"           prepend Markdown blocks
  doc image <document_id> <url> [--alt "..."]
            [--at end|start | --replace "<text>" | --after "<text>" | --before "<text>"]
                                                   insert an image block. Default
                                                   is end of doc. Anchored modes
                                                   place the image relative to
                                                   the first block whose text
                                                   contains the given snippet:
                                                     --replace : swap that
                                                                 block for the
                                                                 image (use to
                                                                 fix a broken
                                                                 "![alt](url)"
                                                                 markdown line)
                                                     --after   : insert below
                                                                 the matched
                                                                 block
                                                     --before  : insert above
                                                                 the matched
                                                                 block
                                                   An anchored mode that misses
                                                   is an ERROR — no image is
                                                   inserted. Re-read the doc
                                                   and pick a more specific
                                                   snippet.
                                                   Preferred over an
                                                   "![alt](url)" markdown block
                                                   when the URL might wrap onto
                                                   multiple lines (long presigned
                                                   CDN links etc.)
  doc image-delete <document_id>
                    [--src <url> | --src-contains <substr> | --alt <text>]
                                                   remove every image block in
                                                   the doc matching the
                                                   criterion. Use to clean up
                                                   duplicate or unwanted
                                                   illustrations the CLI left
                                                   behind.
  doc replace <document_id> --find "..." --replace "..."
                                                   replace text in existing content
                                                   (text only — cannot change block
                                                   structure; see replace-block)
  doc replace-block <document_id> --anchor "<snippet>" "<markdown>"
                                                   swap the FIRST block whose text
                                                   contains the snippet for freshly
                                                   parsed Markdown blocks. Use to
                                                   fix structure: e.g. a table that
                                                   rendered as one flat "|...|"
                                                   paragraph. Anchor miss = error,
                                                   nothing changes.
  doc delete <document_id>                         delete a doc you created
  Markdown supports headings, lists, quotes, code, links, tables, and image blocks:
    ![alt text](https://example.com/image.png)

SKILLS  (progressive-disclosure capability packs in your own workspace):
  skills list                                      list installed skills (name + description only)
  skills read <name> [<sub-path>]                  load full SKILL.md (or a bundled file)
  skills create <name> "<description>"             scaffold a new skill
  skills search <query>                            search SkillHub (requires SKILLHUB_URL)
  skills install <id_or_url>                       install from SkillHub or any compatible URL
  skills delete <name>                             remove a skill
  react <message_id> <emoji>                       toggle an emoji reaction on any message
  palette "<brief>"                                generate a 5-color hex palette
  (web search/read runs via the in-pod browser — bash invokes
   "cumora-web search", "cumora-web read", or "opencli browser ...")
  image generate "<prompt>" [--size square|wide|tall]
                                                   generate an image (gpt-image-2), upload to storage,
                                                   return signed URL + key for later 'reply --attach <url>'

GLOBAL FLAGS:
  --json
  --as <id>                          run as another participant (agents query their own state)

EXAMPLES:
  cumora groups --as iris
  cumora memory note "Yetone prefers warm palettes" --about yetone --as iris
  cumora ws write drafts/v3.md "# Hero v3..." --as iris
  cumora ws edit drafts/v3.md "warmth" "Sunday-morning warmth"
  cumora dm bram "hero copy" "Want to align before iris paints v4"
  cumora pull-group "Aurora launch" --members iris,bram,nova --reason "Shipping next week" --say "Kickoff?"
  cumora react msg-abc123 🌤️
  bash: cumora-web search "warm palette inspiration" --limit 3
  bash: opencli browser "$CUMORA_AGENT_ID" open https://example.com
  cumora image generate "a quiet bauhaus poster, ochre and cobalt" --size wide
  cumora tasks list --as bram --status open
  cumora calendar create "Follow up with Wei on hero v3" --at 2026-05-25T15:00:00Z --assignee iris --prompt "DM wei and ask if v3 landed"     # one-shot self-schedule
  cumora calendar create "Daily standup digest" --at 2026-05-24T09:00:00Z --assignee iris --prompt "Summarize yesterday's group activity and post into <convo_id>" --every daily     # recurring self-schedule
  cumora calendar list --as iris                                          # see what you've already scheduled for yourself`

func (s *Domain) CmdHelp() agent.Result {
	return agent.OK(strings.ReplaceAll(cliHelpText, "§", "`"))
}
