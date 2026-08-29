// sched 包 triage —— triage-core.ts 的纯函数心脏:组 cerebellum 的
// prompt(instructions+input)并解析其 JSON 判定。无 LLM、无 DB、无 env。
// AI 原则:这里的每个"决策"都由小模型做,绝无按内容分类的正则;仅有的
// 非模型短路是"收件箱空"(计数不是分类)与对模型自身 JSON 答案的解析。
package sched

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"

	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

// dmAgentTriageEvery:agent↔agent DM 的回环检查节奏。DM 默认 ENGAGE
// (agent 该说话),只是不逐条付 triage 费——每 8 条(按 sequence)跑
// 一次小脑作死循环探测器。
const dmAgentTriageEvery = 8

// hardLoopCap:claimed 会话的 HIGH 兜底(≈4 人队的 4 轮)。真实被认领
// 的工作跑到这个高度几乎必已收尾或引来人类;小脑(喂了 claim 信号)是
// 主要的细腻收尾,30/min 的速率地板只限速率,慢乒乓永远不触发——这个
// 计数上限才是真正刹住失控的闸。回归警戒:此兜底被删过两次"为 AI 原生
// 优雅",回环立即复发——不许删。
const hardLoopCap = 20

/* ───────── map 行访问器(inbox/context 行来自 LoadInbox/LoadContext) ───────── */

func RowStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func RowInt(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

func parseISOms(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

/* ───────── compactMessages / 工作状态 ───────── */

// compactMessages:压缩最近 40 条。is_unread/is_self/human_reacted_at 是
// DB 字段(非内容分类),以朴素标记面给模型——模型自己决定它们意味着什么。
func compactMessages(rows []map[string]any) string {
	if len(rows) > 40 {
		rows = rows[len(rows)-40:]
	}
	lines := make([]string, 0, len(rows))
	for _, m := range rows {
		unread, reacted, selfMark := "", "", ""
		if v, ok := m["is_unread"].(bool); ok && v {
			unread = "NEW "
		}
		if v := RowStr(m, "human_reacted_at"); v != "" {
			reacted = "HUMAN-REACTED "
		}
		if v, ok := m["is_self"].(bool); ok && v {
			selfMark = "▸YOU "
		}
		convo := fmt.Sprintf("%s [%s]", RowStr(m, "conversation_id"), RowStr(m, "conversation_kind"))
		authorKind := RowStr(m, "author_kind")
		if authorKind == "" {
			authorKind = "unknown"
		}
		author := fmt.Sprintf("%s (%s)", RowStr(m, "author_name"), authorKind)
		body := RowStr(m, "body")
		if RowStr(m, "kind") == "system" {
			body = "[system]"
		}
		body = collapseWhitespace(body)
		body = httpx.UTF16Cap(body, 500) // TS .slice(0,500) 按 UTF-16 码元
		lines = append(lines, fmt.Sprintf("%s%s%s #%d %s%s: %s",
			unread, reacted, convo, RowInt(m, "sequence"), selfMark, author, body))
	}
	return strings.Join(lines, "\n")
}

var wsRun = regexp.MustCompile(`\s+`)

func collapseWhitespace(s string) string { return wsRun.ReplaceAllString(s, " ") }

type agentRun struct {
	sinceHuman int
	sawHuman   bool
	agents     map[string]bool
}

// agentRunByConvo:自人类上次"露面"以来的 per-会话 agent-only 状态。
// 人类露面 = 人类消息、人类 emoji 反应、或人类读过会话(读游标)。
// 反应/读按自身时间戳收缩 run——人类此刻在看,先前累积到那刻的都算
// 被人类目击,只有其后的才计入地板。
func agentRunByConvo(context []map[string]any) map[string]*agentRun {
	rowsByConvo := map[string][]map[string]any{}
	var convoOrder []string
	for _, m := range context {
		if RowStr(m, "kind") == "system" {
			continue
		}
		cid := RowStr(m, "conversation_id")
		if _, ok := rowsByConvo[cid]; !ok {
			convoOrder = append(convoOrder, cid)
		}
		rowsByConvo[cid] = append(rowsByConvo[cid], m)
	}
	out := map[string]*agentRun{}
	for _, cid := range convoOrder {
		rows := rowsByConvo[cid]
		var runRows []map[string]any
		sawHuman := false
		lastAttentionAt := int64(0)
		for _, m := range rows {
			if RowStr(m, "author_kind") == "human" {
				runRows = nil
				sawHuman = true
			} else {
				runRows = append(runRows, m)
			}
			if t := parseISOms(RowStr(m, "human_reacted_at")); t > lastAttentionAt {
				lastAttentionAt = t
			}
			if t := parseISOms(RowStr(m, "human_last_read_at")); t > lastAttentionAt {
				lastAttentionAt = t
			}
		}
		if lastAttentionAt > 0 {
			sawHuman = true
			filtered := runRows[:0]
			for _, m := range runRows {
				if parseISOms(RowStr(m, "created_at")) > lastAttentionAt {
					filtered = append(filtered, m)
				}
			}
			runRows = filtered
		}
		run := &agentRun{sinceHuman: len(runRows), sawHuman: sawHuman, agents: map[string]bool{}}
		for _, m := range runRows {
			run.agents[RowStr(m, "author_id")] = true
		}
		out[cid] = run
	}
	return out
}

// threadWorkState:"开工作状态"块——把 per-会话的 agent-only 跑动 + claim
// 事实喂给模型,让"engage 还是抑制"的判断基于事实而非措辞猜测。
func threadWorkState(context []map[string]any, claims map[string][]agent.WorklogEntry) string {
	var lines []string
	runs := agentRunByConvo(context)
	// TS 用 Map 迭代(插入序);此处按会话 id 排序取得稳定序——
	// 行序只影响可读性,不影响判定输入的字段集合。
	cids := make([]string, 0, len(runs))
	for cid := range runs {
		cids = append(cids, cid)
	}
	sort.Strings(cids)
	for _, cid := range cids {
		e := runs[cid]
		held := claims[cid]
		if e.sinceHuman < 1 && len(held) == 0 {
			continue
		}
		claimStr := "no active claim"
		if len(held) > 0 {
			parts := make([]string, 0, len(held))
			for _, c := range held {
				parts = append(parts, fmt.Sprintf("%q (%s)", c.Subject, c.AgentID))
			}
			claimStr = "ACTIVE CLAIM: " + strings.Join(parts, ", ")
		}
		noHuman := ""
		if !e.sawHuman {
			noHuman = " (no human attention in view)"
		}
		lines = append(lines, fmt.Sprintf("  %s: %d agent message(s) since a human last spoke, reacted, or read the room%s; %s",
			cid, e.sinceHuman, noHuman, claimStr))
	}
	return strings.Join(lines, "\n")
}

/* ───────── buildTriageRequest ───────── */

// TriageRequest:空收件箱判定(无输入可判、不调模型)或喂小模型的
// prompt 对。failClosed:纯 agent↔agent 唤醒(未读集合无人)时 daemon
// 的本地 triage 失败必须 FAIL-CLOSED(本地模型抖动不得放大回环);
// 有人在场 → 失败开放(绝不晾着人)。
type TriageRequest struct {
	Verdict      map[string]any `json:"verdict,omitempty"`
	Instructions *string        `json:"instructions,omitempty"`
	Input        *string        `json:"input,omitempty"`
	FailClosed   bool           `json:"failClosed,omitempty"`
}

func buildTriageInstructions(personaName string) string {
	// cerebellum 是直觉脑且只是闸门:判 actionable 与读空气。它绝不写内容,
	// 也不决定谁回、怎么回——大脑轮内自决。小脑保持纯闸门省 token 且免于
	// 脆弱的场景枚举——单一原则,不列清单。
	return `You are Cumora's inbox triage cerebellum — a fast gate in front of teammate "` + personaName + `"'s expensive main brain. Each message shows its conversation kind ([direct]/[group]), whether it is NEW (unread), and "▸YOU" for this agent's own messages.

Your ONE job is to keep NOISE off the big brain — that is ALL you suppress. Decide with a single PRINCIPLE, never a checklist of cases:

- If a HUMAN is involved or waiting → actionable=true, ALWAYS. ANY message from a human in a conversation you're in — a DM, a group message, an @mention, a greeting, an emoji, a directive, a question, a complaint, a "thanks" — deserves engagement, no matter how short, how casual, whether it repeats an earlier message, or whether the thread looks "wound down". A human EMOJI REACTION (marked HUMAN-REACTED) is involvement too: a human reacting to the thread is a human watching it live — treat the activity as human-attended, not agent-only. A human reaching out to silence is the single worst failure. Also engage: a human-started activity still in motion, and genuine unfinished work assigned to you — which INCLUDES a peer agent directly asking YOU to decide or do something: any message that needs YOUR answer or action to move forward. That is work you owe, not a loop — actionable=true even though both of you are agents.

- The ONLY thing you suppress (actionable=false): the unread is purely AGENT-to-AGENT with NO authoritative open work behind it. Check "Open work state" in the input — an agent-only thread with NO active claim, where peers are just acking / agreeing / restating / trading open-ended "what do you think?", is exactly the noise you exist to suppress. Suppress it EVEN IF a peer's message is phrased as a question: an open-ended question with no claimed work and no human waiting does NOT obligate a reply — real teammates let such a thread wind down instead of ping-ponging forever. ENGAGE only when a human is involved/waiting, OR an active claim is present and your reply ADVANCES it, OR a peer is genuinely blocked on YOUR specific decision/action to move real work forward.

- When unsure, choose actionable=true. Err toward engaging; never leave a human hanging. You only GATE here — the big brain decides who replies, how, and how briefly by reading the room itself, so you don't need to.

Reply ONLY as strict JSON: {"actionable": boolean, "reason": "short factual reason", "promptNote": "one sentence telling the main brain what's needed (e.g. 'human said hi in a DM — greet them back warmly')"}.
Output ONLY the JSON object — no markdown fences, no text before or after. Keep "reason" to ONE short sentence.`
}

func buildTriageInput(agentID string, persona *agent.Persona, inbox, context []map[string]any,
	claims map[string][]agent.WorklogEntry, humanActiveInCompany bool) string {
	workState := threadWorkState(context, claims)
	lines := []string{
		fmt.Sprintf("Agent: %s (%s)", persona.Name, agentID),
		"Role: " + persona.Role,
	}
	if humanActiveInCompany {
		lines = append(lines, "A human is actively watching this workspace right now (read activity within the last few minutes) — treat in-motion team activities as human-supervised even in rooms the human cannot join.")
	}
	lines = append(lines, "", "Unread inbox:", compactMessages(inbox), "", "Recent visible context:", compactMessages(context))
	if workState != "" {
		lines = append(lines, "", "Open work state — for conversations that have drifted agent-only (no human in between) or carry an active work claim. An ACTIVE CLAIM means a teammate owns real work here (a game/activity/deliverable in progress) — engage if your reply ADVANCES it; the task itself governs when it ends. An agent-only run with NO active claim, that is only acks / agreement / restating / open-ended \"what do you think?\", is a dead loop → actionable=false, EVEN IF the latest message is phrased as a question. This is overridden only when the latest message needs an answer or action from THIS agent (a direct request / your move), or a human is waiting:", workState)
	}
	// "json" 一词必须出现在 INPUT(不止 instructions)——否则 Responses API
	// 对 text.format json_object 报 400,triage 全走 fail-open,闸门失效。
	lines = append(lines, "", `Respond ONLY with a single JSON object: {"actionable": boolean, "responseMode": "me"|"each"|"one-of-us"|null, "reason": string, "promptNote": string}.`)
	return strings.Join(lines, "\n")
}

// systemPayloadOf:解析 SYSTEM 消息的线载荷(服务器自写的 JSON 信封——
// 日历派发、relay 等)。读的是线格式,不是内容分类。
func systemPayloadOf(raw string) map[string]any {
	var p map[string]any
	if json.Unmarshal([]byte(raw), &p) != nil {
		return nil
	}
	return p
}

// BuildTriageRequest:组 triage 请求。非模型短路:空箱、仅系统、人类
// 未读(人类神圣)、agent↔agent DM 节奏检查、硬回环上限。其余全交小脑。
func BuildTriageRequest(agentID string, persona *agent.Persona, inbox, context []map[string]any,
	claims map[string][]agent.WorklogEntry, humanActiveInCompany bool) TriageRequest {
	if len(inbox) == 0 {
		return TriageRequest{Verdict: map[string]any{
			"actionable": false,
			"reason":     "inbox empty",
			"promptNote": "Small-brain inbox triage: inbox is empty; do not reply unless you independently find new unread work.",
			"source":     "empty-inbox",
		}}
	}
	// 到点的日历派发(指派给本 agent)不是系统噪音,是自设闹钟——agent
	// 明确要求此刻被唤醒("唱票"、"收官阶段")。确定性线格式检查(该
	// JSON 是服务器写的),非内容分类;指派他人的仍落入仅系统闸。
	var dueAlarm map[string]any
	for _, m := range inbox {
		if RowStr(m, "kind") != "system" {
			continue
		}
		p := systemPayloadOf(RowStr(m, "body"))
		if p == nil || p["kind"] != "calendar_event" {
			continue
		}
		assignee, _ := p["assigneeId"].(string)
		if assignee == "" || assignee == agentID {
			dueAlarm = m
			break
		}
	}
	if dueAlarm != nil {
		p := systemPayloadOf(RowStr(dueAlarm, "body"))
		var noteParts []string
		if t, ok := p["title"].(string); ok && t != "" {
			noteParts = append(noteParts, t)
		}
		if ap, ok := p["agentPrompt"].(string); ok && ap != "" {
			noteParts = append(noteParts, ap)
		}
		note := strings.Join(noteParts, " — ")
		promptNote := "Your scheduled calendar alarm fired — do what future-you planned."
		if note != "" {
			promptNote = "Your scheduled alarm fired: " + note
		}
		return TriageRequest{Verdict: map[string]any{
			"actionable": true,
			"reason":     "calendar alarm due — a self-scheduled wake for this agent",
			"promptNote": promptNote,
			"source":     "calendar-due",
		}}
	}
	allSystem := true
	for _, m := range inbox {
		if RowStr(m, "kind") != "system" {
			allSystem = false
			break
		}
	}
	if allSystem {
		return TriageRequest{Verdict: map[string]any{
			"actionable": false,
			"reason":     "system-only inbox (no human/agent message) — informational, not a task",
			"promptNote": "",
			"source":     "system-only",
		}}
	}
	var realUnread []map[string]any
	for _, m := range inbox {
		if RowStr(m, "kind") != "system" {
			realUnread = append(realUnread, m)
		}
	}
	// 人类消息神圣——DM 或群消息一律不设闸:闸门对人类只能答"是",
	// 为其付小脑费纯属浪费延迟,且行为不安全的闸门可能摔落人类消息。
	var humanUnread []map[string]any
	for _, m := range realUnread {
		if RowStr(m, "author_kind") == "human" {
			humanUnread = append(humanUnread, m)
		}
	}
	if len(humanUnread) > 0 {
		anyDm := false
		for _, m := range humanUnread {
			if RowStr(m, "conversation_kind") == "direct" {
				anyDm = true
				break
			}
		}
		if anyDm {
			return TriageRequest{Verdict: map[string]any{
				"actionable": true,
				"reason":     "human DM — always engage, no triage gate",
				"promptNote": "A human messaged you directly — reply to them.",
				"source":     "human-dm",
			}}
		}
		return TriageRequest{Verdict: map[string]any{
			"actionable": true,
			"reason":     "human message in group — always engage, no triage gate",
			"promptNote": "A human posted to the group — read the room and respond if it is yours to answer.",
			"source":     "human-group",
		}}
	}
	// agent↔agent DM:默认 ENGAGE、不逐条付 triage 费;每 8 条一次小脑
	// 检查点可终止无意义往返。绝不把 agent 对队友变成哑巴(那是 bug),
	// 失控成本仍有每分钟激活率地板兜底。
	if len(realUnread) > 0 {
		allDirectAgent := true
		latestSeq := int64(0)
		for _, m := range realUnread {
			if RowStr(m, "conversation_kind") != "direct" || RowStr(m, "author_kind") != "agent" {
				allDirectAgent = false
				break
			}
			if s := RowInt(m, "sequence"); s > latestSeq {
				latestSeq = s
			}
		}
		if allDirectAgent && latestSeq%dmAgentTriageEvery != 0 {
			return TriageRequest{Verdict: map[string]any{
				"actionable": true,
				"reason":     fmt.Sprintf("agent↔agent DM — engage (loop-check runs every %d msgs; this one is between checks)", dmAgentTriageEvery),
				"promptNote": "Reply to your teammate in this DM.",
				"source":     "dm-agent-engage",
			}}
		}
		// 检查点上 → 落到 cerebellum,它能停掉死循环。
	}
	// 硬回环上限(确定性、无模型):每个未读会话都 agent-only 跑过地板才
	// 停。claimed → 固定 HIGH 兜底;unclaimed → 自缩放地板(消息数超过
	// 不同 agent 数即"套圈",随队宽伸缩);有人监工的 workspace → agent-only
	// 线程也用 HIGH 兜底(活动可能合法地跑在人类被排除的侧室)。仅当每个
	// 未读会话都过线才确定性抑制。
	runs := agentRunByConvo(context)
	unreadSet := map[string]bool{}
	var unreadConvos []string
	for _, m := range inbox {
		if RowStr(m, "kind") == "system" {
			continue
		}
		cid := RowStr(m, "conversation_id")
		if !unreadSet[cid] {
			unreadSet[cid] = true
			unreadConvos = append(unreadConvos, cid)
		}
	}
	pastFloor := func(cid string) bool {
		e := runs[cid]
		since := 0
		if e != nil {
			since = e.sinceHuman
		}
		if len(claims[cid]) > 0 {
			return since >= hardLoopCap
		}
		if humanActiveInCompany {
			return since >= hardLoopCap
		}
		agents := 0
		if e != nil {
			agents = len(e.agents)
		}
		return since > agents
	}
	if len(unreadConvos) > 0 {
		allPast := true
		for _, cid := range unreadConvos {
			if !pastFloor(cid) {
				allPast = false
				break
			}
		}
		if allPast {
			return TriageRequest{Verdict: map[string]any{
				"actionable": false,
				"reason":     "loop cap: every unread thread is a lapping agent-only run (or a claimed/supervised thread past the high backstop) with no human attention (message, reaction, or read) — staying silent until a human re-engages",
				"promptNote": "",
				"source":     "loop-cap",
			}}
		}
	}
	instructions := buildTriageInstructions(persona.Name)
	input := buildTriageInput(agentID, persona, inbox, context, claims, humanActiveInCompany)
	failClosed := true
	for _, m := range inbox {
		if RowStr(m, "kind") != "system" && RowStr(m, "author_kind") == "human" {
			failClosed = false
			break
		}
	}
	return TriageRequest{Instructions: &instructions, Input: &input, FailClosed: failClosed}
}
