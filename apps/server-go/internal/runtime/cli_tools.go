// runtime 包 runTool 面 —— cli.ts 的 react/dm/pull-group/palette 四个
// "动作"子命令:buildToolArgs 组参 → executeTool(tools.ts)→ CLI 渲染。
// executeTool 落 tool_calls 行(先 pending 后回填),三个 DB 型工具的
// 广播(reactions/group.pulled/message.new)与展示文本逐字对齐 TS。
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

/* ───────────── ToolResult ───────────── */

// cliToolDisplay:展示块;icon 可缺省(部分错误路径无 icon)。
type cliToolDisplay struct {
	Name   string  `json:"name"`
	Arg    string  `json:"arg"`
	Status string  `json:"status"`
	Detail string  `json:"detail"`
	Icon   *string `json:"icon,omitempty"`
}

type cliToolResult struct {
	OK         bool
	Output     any
	Error      string
	DurationMS int64
	Display    cliToolDisplay
}

// supportModelEnv:OPENAI_MODEL_SUPPORT(TS 缺省 gpt-5.4-mini)。
func supportModelEnv() string {
	if m := os.Getenv("OPENAI_MODEL_SUPPORT"); m != "" {
		return m
	}
	return "gpt-5.4-mini"
}

// cliExecuteTool:tools.ts executeTool 的 CLI 子集(palette/dm_with/
// pull_group/react)——tool_calls 先 pending 落行,结束回填 result/status/
// error/duration。
func (s *Service) cliExecuteTool(ctx context.Context, agentID, name, argsJSON string) cliToolResult {
	t0 := time.Now()
	id := "t-" + jsUUID()
	parsed := map[string]any{}
	_ = json.Unmarshal([]byte(argsJSON), &parsed)
	argsJSONNorm, _ := json.Marshal(parsed)
	_, _ = s.DB.ExecContext(ctx, `
		INSERT INTO tool_calls (id, message_id, agent_id, name, args, status, run_id, company_id)
		VALUES ($1,$2,$3,$4,$5::jsonb,'pending',$6,$7)`,
		id, nil, agentID, name, string(argsJSONNorm), nil, nil)

	result := s.cliDispatchTool(ctx, name, parsed, agentID, t0)
	resultJSON, _ := json.Marshal(result.Output)
	if result.Output == nil {
		resultJSON = []byte("null")
	}
	status := "error"
	if result.OK {
		status = "ok"
	}
	var errArg any
	if result.Error != "" {
		errArg = result.Error
	}
	_, _ = s.DB.ExecContext(ctx, `
		UPDATE tool_calls
		   SET result = $2::jsonb, status = $3, error = $4, duration_ms = $5
		 WHERE id = $1`,
		id, string(resultJSON), status, errArg, result.DurationMS)
	return result
}

func (s *Service) cliDispatchTool(ctx context.Context, name string, args map[string]any, agentID string, t0 time.Time) cliToolResult {
	defer func() {
		// panic 已由上层 RunCli 兜底;这里保持与 TS 一致:异常 → error 行。
	}()
	var result cliToolResult
	var err error
	switch name {
	case "palette":
		result, err = s.cliTPalette(ctx, args, agentID, t0)
	case "dm_with":
		result, err = s.cliTDmWith(ctx, args, agentID, t0)
	case "pull_group":
		result, err = s.cliTPullGroup(ctx, args, agentID, t0)
	case "react":
		result, err = s.cliTReact(ctx, args, agentID, t0)
	default:
		err = nil
		result = cliToolResult{
			OK: false, Error: "unknown tool", DurationMS: msSince(t0),
			Display: cliToolDisplay{Name: name, Arg: "", Status: "unknown tool",
				Detail: fmt.Sprintf("tool not implemented: %s", name)},
		}
	}
	if err != nil {
		argPreview := ""
		if b, jerr := json.Marshal(args); jerr == nil {
			argPreview = truncateRunesSimple(string(b), 80)
		}
		// TS catch 用 String(err) —— Error 对象即 "Error: <msg>"。
		strErr := "Error: " + err.Error()
		result = cliToolResult{
			OK: false, Error: strErr, DurationMS: msSince(t0),
			Display: cliToolDisplay{Name: name, Arg: argPreview, Status: "error", Detail: strErr},
		}
	}
	return result
}

func msSince(t0 time.Time) int64 { return time.Since(t0).Milliseconds() }

/* ───────────── buildToolArgs ───────────── */

// cliBuildToolArgs:TS buildToolArgs —— (子命令参数)→ 工具 JSON 参数,
// 或错误文本。
func cliBuildToolArgs(toolName string, parsed cliParsed) (string, string, bool) {
	pos := parsed.positional
	f := parsed.flags
	switch toolName {
	case "react":
		messageID, emoji := "", ""
		if len(pos) > 0 {
			messageID = pos[0]
		}
		if len(pos) > 1 {
			emoji = pos[1]
		}
		if messageID == "" || emoji == "" {
			return "", "usage: react <message_id> <emoji>", false
		}
		b, _ := json.Marshal(map[string]any{"message_id": messageID, "emoji": emoji})
		return string(b), "", true
	case "dm_with":
		partnerID := ""
		if len(pos) > 0 {
			partnerID = pos[0]
		}
		topic := ""
		if len(pos) > 1 {
			topic = pos[1]
		} else if v, ok := f["topic"].(string); ok {
			topic = v
		}
		opening := ""
		if len(pos) > 2 {
			opening = pos[2]
		} else if v, ok := f["say"].(string); ok {
			opening = v
		} else if v, ok := f["message"].(string); ok {
			opening = v
		}
		if partnerID == "" {
			return "", "usage: dm <partner_id> <topic> <opening>  OR  dm <partner_id> --topic \"...\" --say \"...\"", false
		}
		if topic == "" || opening == "" {
			return "", "dm requires both topic and opening message (positional or --topic/--say)", false
		}
		b, _ := json.Marshal(map[string]any{"partner_id": partnerID, "topic": topic, "opening_message": opening})
		return string(b), "", true
	case "pull_group":
		title := ""
		if len(pos) > 0 {
			title = pos[0]
		}
		if title == "" {
			return "", "usage: pull-group <title> --members a,b,c --reason \"...\" --say \"...\"", false
		}
		members := []string{}
		if raw, ok := f["members"].(string); ok {
			for _, part := range strings.Split(raw, ",") {
				if t := strings.TrimSpace(part); t != "" {
					members = append(members, t)
				}
			}
		}
		if len(members) == 0 {
			return "", "pull-group requires --members a,b,c", false
		}
		reason, _ := f["reason"].(string)
		opening, _ := f["say"].(string)
		if v, ok := f["message"].(string); ok && opening == "" {
			opening = v
		}
		if reason == "" || opening == "" {
			return "", "pull-group requires --reason \"...\" and --say \"...\"", false
		}
		b, _ := json.Marshal(map[string]any{"title": title, "members": members, "reason": reason, "opening_message": opening})
		return string(b), "", true
	case "palette":
		brief := strings.TrimSpace(strings.Join(pos, " "))
		if brief == "" {
			if v, ok := f["brief"].(string); ok {
				brief = v
			}
		}
		if brief == "" {
			return "", "usage: palette <brief>", false
		}
		b, _ := json.Marshal(map[string]any{"brief": brief})
		return string(b), "", true
	default:
		return "", fmt.Sprintf("unknown tool: %s", toolName), false
	}
}

/* ───────────── runTool(命令面) ───────────── */

func (s *Service) cliRunTool(ctx context.Context, toolName string, parsed cliParsed) cliResult {
	argsJSON, buildErr, ok := cliBuildToolArgs(toolName, parsed)
	if !ok {
		return cliErr(buildErr)
	}
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErr(err.Error())
	}
	r := s.cliExecuteTool(ctx, me, toolName, argsJSON)
	sideEffects := cliToolSideEffects(toolName, r.Output, me)
	if parsed.flagTruey("json") {
		if r.OK {
			txt, jerr := cliJSONStringify(r.Output)
			if jerr != nil {
				return cliErrCode(fmt.Sprintf("error: %v", jerr), 2)
			}
			return cliResult{ok: true, text: txt, exitCode: 0, sideEffects: sideEffects}
		}
		errPayload := struct {
			Error   string         `json:"error"`
			Display cliToolDisplay `json:"display"`
		}{r.Error, r.Display}
		txt, jerr := cliJSONStringify(errPayload)
		if jerr != nil {
			return cliErrCode(fmt.Sprintf("error: %v", jerr), 2)
		}
		return cliResult{ok: false, text: txt, exitCode: 1}
	}
	// detail = display.detail || (output ? JSON.stringify(output, null, 2) : '(no output)')
	detail := r.Display.Detail
	if detail == "" {
		if r.Output != nil {
			detail, _ = cliJSONStringify(r.Output)
		} else {
			detail = "(no output)"
		}
	}
	if !r.OK {
		reason := r.Error
		if reason == "" {
			reason = r.Display.Status
		}
		return cliErr(fmt.Sprintf("%s failed: %s\n%s", r.Display.Name, reason, detail))
	}
	head := fmt.Sprintf("%s → %s", r.Display.Name, r.Display.Status)
	return cliResult{ok: true, text: cliStripLoneSurrogates(fmt.Sprintf("%s\n\n%s", head, detail)), exitCode: 0, sideEffects: sideEffects}
}

// cliToolSideEffects:react/dm_with/pull_group 的副作用事件。
func cliToolSideEffects(toolName string, output any, agentID string) []cliSideEffect {
	m, ok := output.(map[string]any)
	if !ok {
		return nil
	}
	str := func(k string) string {
		v, _ := m[k].(string)
		return v
	}
	switch toolName {
	case "react":
		return []cliSideEffect{{
			"event":     "reaction.updated",
			"command":   "react",
			"visibleToUser": true,
			"actorId":   agentID,
			"messageId": str("messageId"),
			"emoji":     str("emoji"),
			"action":    str("action"),
		}}
	case "dm_with":
		return []cliSideEffect{{
			"event":          "conversation.created",
			"command":        "dm",
			"actorId":        agentID,
			"conversationId": str("conversationId"),
			"partnerId":      str("partnerId"),
			"topic":          str("topic"),
			"visibleToUser":  true,
		}}
	case "pull_group":
		members := []string{}
		if raw, ok := m["members"].([]any); ok {
			for _, v := range raw {
				members = append(members, fmt.Sprint(v))
			}
		}
		return []cliSideEffect{{
			"event":          "conversation.created",
			"command":        "pull-group",
			"actorId":        agentID,
			"conversationId": str("conversationId"),
			"members":        members,
			"visibleToUser":  true,
		}}
	default:
		return nil
	}
}

/* ───────────── tPalette ───────────── */

var paletteHexRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

func (s *Service) cliTPalette(ctx context.Context, args map[string]any, agentID string, t0 time.Time) (cliToolResult, error) {
	brief := strings.TrimSpace(fmt.Sprint(args["brief"]))
	model := supportModelEnv()
	agentArg, tenantArg := agentID, (*string)(nil)
	record := func(status string, errMsg *string, usage *TokenUsage) {
		s.RecordLlmCall(LlmCallRecord{
			Purpose: "palette", CompanyID: tenantArg, AgentID: &agentArg, Source: "cloud",
			Model: model, Usage: usage, LatencyMS: msSince(t0), Status: status, Error: errMsg,
			Extras: map[string]any{"brief": truncateRunesSimple(brief, 120)},
		})
	}
	res, err := s.cliResponsesCreate(ctx, "", cliResponsesArgs{
		Model:           model,
		Instructions:    `You produce 5-color hex palettes. Reply ONLY with JSON: {"colors":["#RRGGBB", ...]}. No prose.`,
		Input:           fmt.Sprintf("Design brief: %s\n\nReply with JSON only.", brief),
		MaxOutputTokens: 800,
		JSONMode:        true,
	})
	if err != nil {
		msg := err.Error()
		record("failed", &msg, nil)
		return cliToolResult{}, err
	}
	record("ok", nil, res.Usage)
	colors := []string{}
	var parsedCols struct {
		Colors []string `json:"colors"`
	}
	if json.Unmarshal([]byte(res.OutputText), &parsedCols) == nil {
		for _, c := range parsedCols.Colors {
			if paletteHexRe.MatchString(c) {
				colors = append(colors, c)
			}
			if len(colors) >= 5 {
				break
			}
		}
	}
	icon := "figma"
	return cliToolResult{
		OK:         len(colors) > 0,
		Output:     struct {
			Colors []string `json:"colors"`
			Brief  string   `json:"brief"`
		}{colors, brief},
		DurationMS: msSince(t0),
		Display: cliToolDisplay{
			Name:   "palette",
			Arg:    truncateRunesSimple(brief, 60),
			Status: fmt.Sprintf("%d colors", len(colors)),
			Detail: strings.Join(colors, "  "),
			Icon:   &icon,
		},
	}, nil
}

/* ───────────── tDmWith / startPrivateChat ───────────── */

func (s *Service) cliTDmWith(ctx context.Context, args map[string]any, agentID string, t0 time.Time) (cliToolResult, error) {
	partnerID := strings.TrimSpace(fmt.Sprint(args["partner_id"]))
	topic := strings.TrimSpace(fmt.Sprint(args["topic"]))
	opening := strings.TrimSpace(fmt.Sprint(args["opening_message"]))
	if partnerID == "" || partnerID == agentID {
		return cliToolResult{
			OK: false, Error: "invalid partner", DurationMS: msSince(t0),
			Display: cliToolDisplay{Name: "dm_with", Arg: partnerID, Status: "error", Detail: "Pick a different agent", Icon: strPtr("web")},
		}, nil
	}
	convoID, err := s.cliStartPrivateChat(ctx, agentID, partnerID, topic, opening)
	if err != nil {
		return cliToolResult{}, err
	}
	icon := "web"
	return cliToolResult{
		OK: true,
		Output: struct {
			ConversationID string `json:"conversationId"`
			PartnerID      string `json:"partnerId"`
			Topic          string `json:"topic"`
		}{convoID, partnerID, topic},
		DurationMS: msSince(t0),
		Display: cliToolDisplay{
			Name:   "dm_with",
			Arg:    fmt.Sprintf("%s · %s", partnerID, truncateRunesSimple(topic, 40)),
			Status: fmt.Sprintf("opened · %s", utf16Slice(convoID, 12)),
			Detail: fmt.Sprintf("→ \"%s\"\n\nDirect conversation opened with %s. Same shape as any 1-on-1 chat — your partner will see it in their mailbox and reply naturally.", truncateRunesSimple(opening, 200), partnerID),
			Icon:   &icon,
		},
	}, nil
}

func strPtr(s string) *string { return &s }

func (s *Service) cliParticipantBrief(ctx context.Context, id string) (name, kind string, ok bool) {
	err := s.DB.QueryRowContext(ctx,
		`SELECT name, kind FROM participants WHERE id = $1 AND departed_at IS NULL`, id).Scan(&name, &kind)
	return name, kind, err == nil
}

// cliStartPrivateChat:private_chat.ts startPrivateChat —— 复用既有 direct
// 会话(顺序无关),否则建 direct-<uuid12>;首发消息 + auto-ack + 广播。
func (s *Service) cliStartPrivateChat(ctx context.Context, instigatorID, partnerID, topic, opening string) (string, error) {
	if instigatorID == partnerID {
		return "", fmt.Errorf("cannot open a DM with yourself")
	}
	instName, _, instOK := s.cliParticipantBrief(ctx, instigatorID)
	_, _, partOK := s.cliParticipantBrief(ctx, partnerID)
	if !instOK {
		return "", fmt.Errorf("private-chat instigator not found: %s", instigatorID)
	}
	if !partOK {
		return "", fmt.Errorf("private-chat partner not found: %s", partnerID)
	}
	companyID, err := s.cliAgentCompany(ctx, instigatorID)
	if err != nil {
		return "", err
	}
	convoID, err := s.cliFindOrCreateDirect(ctx, instigatorID, partnerID, companyID, topic, instName)
	if err != nil {
		return "", err
	}

	messageID := "m-" + jsUUID()
	var sequence int
	err = s.DB.QueryRowContext(ctx, `
		INSERT INTO conversation_counters (conversation_id, next_sequence)
		VALUES ($1, 2)
		ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		RETURNING next_sequence - 1 AS seq`, convoID).Scan(&sequence)
	if err != nil {
		sequence = 1
	}
	var companyArg any
	if companyID != "" {
		companyArg = companyID
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		VALUES ($1, $2, $3, 'text', $4, $5, $6)`,
		messageID, convoID, instigatorID, opening, sequence, companyArg); err != nil {
		return "", err
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, convoID)
	_, _ = s.DB.ExecContext(ctx, `
		INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`, instigatorID, convoID)

	var companyPtr *string
	if companyID != "" {
		companyPtr = &companyID
	}
	eventsPublishMessageNew(ctx, companyPtr, convoID, map[string]any{
		"id":             messageID,
		"conversationId": convoID,
		"authorId":       instigatorID,
		"kind":           "text",
		"body":           opening,
		"sequence":       sequence,
		"at":             isoNowMs(),
	})
	return convoID, nil
}

func (s *Service) cliFindOrCreateDirect(ctx context.Context, aID, bID, companyID, topic, aName string) (string, error) {
	membersA, _ := jsonMarshalStrings([]string{aID})
	membersB, _ := jsonMarshalStrings([]string{bID})
	var existing string
	query := `
		SELECT id FROM conversations
		  WHERE kind = 'direct'
		    AND members @> $1::jsonb
		    AND members @> $2::jsonb
		    AND jsonb_array_length(members) = 2`
	args := []any{membersA, membersB}
	if companyID != "" {
		query += ` AND company_id = $3 ORDER BY created_at DESC LIMIT 1`
		args = append(args, companyID)
	} else {
		query += ` ORDER BY created_at DESC LIMIT 1`
	}
	err := s.DB.QueryRowContext(ctx, query, args...).Scan(&existing)
	if err == nil {
		if topic != "" {
			_, _ = s.DB.ExecContext(ctx,
				`UPDATE conversations SET topic = $2, updated_at = NOW() WHERE id = $1`, existing, topic)
		}
		return existing, nil
	}
	bName, _, _ := s.cliParticipantBrief(ctx, bID)
	if bName == "" {
		bName = bID
	}
	if aName == "" {
		aName = aID
	}
	id := "direct-" + jsUUID()[:12]
	members, _ := jsonMarshalStrings([]string{aID, bID})
	var topicArg any
	if topic != "" {
		topicArg = topic
	}
	var companyArg any
	if companyID != "" {
		companyArg = companyID
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO conversations (id, kind, title, members, company_id, topic)
		VALUES ($1, 'direct', $2, $3::jsonb, $4, $5)`,
		id, fmt.Sprintf("%s ↔ %s", aName, bName), members, companyArg, topicArg); err != nil {
		return "", err
	}
	return id, nil
}

/* ───────────── tPullGroup / startPulledGroup ───────────── */

func (s *Service) cliTPullGroup(ctx context.Context, args map[string]any, agentID string, t0 time.Time) (cliToolResult, error) {
	title := strings.TrimSpace(fmt.Sprint(args["title"]))
	if len(title) > 80 {
		title = title[:80]
	}
	if title == "" {
		title = "untitled"
	}
	reason := strings.TrimSpace(fmt.Sprint(args["reason"]))
	opening := strings.TrimSpace(fmt.Sprint(args["opening_message"]))
	members := []string{}
	if raw, ok := args["members"].([]any); ok {
		for _, m := range raw {
			if v := fmt.Sprint(m); v != "" {
				members = append(members, v)
			}
		}
	}
	if !containsString(members, agentID) {
		members = append(members, agentID)
	}
	convoID, err := s.cliStartPulledGroup(ctx, agentID, title, members, reason, opening)
	if err != nil {
		return cliToolResult{}, err
	}
	icon := "web"
	return cliToolResult{
		OK: true,
		Output: struct {
			ConversationID string   `json:"conversationId"`
			Members        []string `json:"members"`
		}{convoID, members},
		DurationMS: msSince(t0),
		Display: cliToolDisplay{
			Name:   "pull_group",
			Arg:    title,
			Status: fmt.Sprintf("created · %s", utf16Slice(convoID, 12)),
			Detail: fmt.Sprintf("members: %s\nreason: %s\n\n→ \"%s\"", strings.Join(members, ", "), reason, truncateRunesSimple(opening, 200)),
			Icon:   &icon,
		},
	}, nil
}

const pullCooldownHours = 6

func (s *Service) cliStartPulledGroup(ctx context.Context, instigatorID, title string, members []string, reason, opening string) (string, error) {
	// 含 human 的拉群才受冷却限制(纯 agent 群只进 peek,不打扰人)。
	var includesHuman bool
	err := s.DB.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM participants p
		   WHERE p.id = ANY ($1::text[]) AND p.kind <> 'agent'
		) AS has_human`, members).Scan(&includesHuman)
	if err != nil {
		includesHuman = true
	}
	if includesHuman {
		var cooldownID, cooldownTitle string
		var cooldownAt time.Time
		qerr := s.DB.QueryRowContext(ctx, `
			SELECT c.id, c.title, c.created_at AS at
			  FROM conversations c
			 WHERE c.kind = 'group'
			   AND c.pulled_by ->> 'agentId' = $1
			   AND c.created_at > NOW() - ($2 || ' hours')::interval
			   AND EXISTS (
			     SELECT 1 FROM jsonb_array_elements_text(c.members) m
			       LEFT JOIN participants p ON p.id = m AND p.company_id = c.company_id
			      WHERE p.kind <> 'agent'
			   )
			 ORDER BY c.created_at DESC LIMIT 1`, instigatorID, fmt.Sprint(pullCooldownHours)).
			Scan(&cooldownID, &cooldownTitle, &cooldownAt)
		if qerr == nil {
			minsAgo := int(time.Since(cooldownAt).Minutes() + 0.5)
			return "", fmt.Errorf(
				"pull-group rate-limited: you (%s) already pulled %q %d minutes ago (id: %s). "+
					"Cooldown is %dh for pulls that include a human. Send a message in that group, or @mention people in an existing conversation, instead of pulling a fresh one. "+
					"(If you'd like a peer-only thread, pull a group with agent members only — those bypass the cooldown.)",
				instigatorID, cooldownTitle, minsAgo, cooldownID, pullCooldownHours)
		}
	}

	convoID := "pulled-" + jsUUID()[:8]
	companyID, err := s.cliAgentCompany(ctx, instigatorID)
	if err != nil {
		return "", err
	}
	var companyArg any
	if companyID != "" {
		companyArg = companyID
	}
	membersJSON, _ := jsonMarshalStrings(members)
	pulledBy, _ := json.Marshal(map[string]any{"agentId": instigatorID, "at": isoNowMs(), "reason": reason})
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO conversations (id, kind, title, subtitle, members, pinned, tag, pulled_by, company_id)
		VALUES ($1, 'group', $2, $3, $4::jsonb, FALSE, 'fresh-pulled', $5::jsonb, $6)`,
		convoID, title, fmt.Sprintf("cross-project · %d", len(members)), membersJSON, string(pulledBy), companyArg); err != nil {
		return "", err
	}
	_, _ = s.DB.ExecContext(ctx,
		`INSERT INTO conversation_counters (conversation_id, next_sequence) VALUES ($1, 2)`, convoID)

	// convening_info:右侧 Why-this-group 面板的数据源。
	personaName := instigatorID
	if p, perr := s.GetPersona(ctx, instigatorID); perr == nil && p != nil && p.Name != "" {
		personaName = p.Name
	}
	whoAndWhy, _ := json.Marshal(mapList(members, func(pid string) map[string]any {
		return map[string]any{"pid": pid, "reason": ""}
	}))
	evidence, _ := json.Marshal(map[string]any{"tail": map[string]any{"tag": "context", "copy": reason}})
	asks, _ := json.Marshal([]any{})
	trigger, _ := json.Marshal(map[string]any{
		"when": nodeLocaleString(time.Now()),
		"what": fmt.Sprintf("%s pulled this together via tool call.", personaName),
	})
	reasoning, _ := jsonMarshalStrings([]string{reason})
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO convening_info
		  (conversation_id, pulled_by_id, headline_lead, headline_tail, subhead,
		   who_and_why, evidence, asks, trigger, reasoning)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8::jsonb, $9::jsonb, $10::jsonb)`,
		convoID, instigatorID, title, "", reason,
		string(whoAndWhy), string(evidence), string(asks), string(trigger), reasoning); err != nil {
		return "", err
	}

	messageID := "m-" + jsUUID()
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, company_id)
		VALUES ($1, $2, $3, 'text', $4, 1, $5)`,
		messageID, convoID, instigatorID, opening, companyArg); err != nil {
		return "", err
	}

	var companyPtr *string
	if companyID != "" {
		companyPtr = &companyID
	}
	groupPayload := map[string]any{
		"type":           "group.pulled",
		"conversationId": convoID,
		"pulledById":     instigatorID,
	}
	if companyID != "" {
		groupPayload["companyId"] = companyID
	}
	_ = s.publishRaw("cumora:group.pulled", mustJSON(groupPayload))
	eventsPublishMessageNew(ctx, companyPtr, convoID, map[string]any{
		"id":             messageID,
		"conversationId": convoID,
		"authorId":       instigatorID,
		"kind":           "text",
		"body":           opening,
		"sequence":       1,
		"at":             isoNowMs(),
	})
	return convoID, nil
}

func mapList[T any, R any](xs []T, fn func(T) R) []R {
	out := make([]R, 0, len(xs))
	for _, x := range xs {
		out = append(out, fn(x))
	}
	return out
}

/* ───────────── tReact ───────────── */

func (s *Service) cliTReact(ctx context.Context, args map[string]any, agentID string, t0 time.Time) (cliToolResult, error) {
	messageID := strings.TrimSpace(fmt.Sprint(args["message_id"]))
	emoji := strings.TrimSpace(fmt.Sprint(args["emoji"]))
	if messageID == "" || emoji == "" {
		return cliToolResult{
			OK: false, Error: "message_id and emoji required", DurationMS: msSince(t0),
			Display: cliToolDisplay{Name: "react", Arg: "", Status: "error", Detail: "missing args"},
		}, nil
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*)::int AS count FROM message_reactions
		 WHERE message_id = $1 AND user_id = $2 AND emoji = $3`,
		messageID, agentID, emoji).Scan(&count); err != nil {
		return cliToolResult{}, err
	}
	action := "added"
	if count > 0 {
		action = "removed"
		if _, err := s.DB.ExecContext(ctx,
			`DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3`,
			messageID, agentID, emoji); err != nil {
			return cliToolResult{}, err
		}
	} else {
		if _, err := s.DB.ExecContext(ctx, `
			INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, messageID, agentID, emoji); err != nil {
			return cliToolResult{}, err
		}
	}

	// 聚合 + 广播(不推进 conversation_reads —— 反应不是"已读")。
	rows, err := s.DB.QueryContext(ctx, `
		SELECT emoji, COUNT(*)::int AS count,
		       array_to_json(array_agg(user_id ORDER BY user_id)) AS users
		  FROM message_reactions WHERE message_id = $1
		 GROUP BY emoji ORDER BY count DESC, emoji ASC`, messageID)
	if err != nil {
		return cliToolResult{}, err
	}
	defer rows.Close()
	type reaction struct {
		Emoji  string   `json:"emoji"`
		Count  int      `json:"count"`
		Users  []string `json:"users"`
	}
	reactions := []reaction{}
	for rows.Next() {
		var r reaction
		var users cliStrArr
		if rows.Scan(&r.Emoji, &r.Count, &users) == nil {
			r.Users = users
			if r.Users == nil {
				r.Users = []string{}
			}
			reactions = append(reactions, r)
		}
	}
	var convoID string
	var rowCompany sql.NullString
	_ = s.DB.QueryRowContext(ctx, `
		SELECT m.conversation_id, c.company_id
		  FROM messages m JOIN conversations c ON c.id = m.conversation_id
		 WHERE m.id = $1`, messageID).Scan(&convoID, &rowCompany)

	payload := map[string]any{
		"type":           "message.reactions",
		"conversationId": convoID,
		"messageId":      messageID,
		"reactions":      reactions,
	}
	if rowCompany.Valid {
		payload["companyId"] = rowCompany.String
	}
	_ = s.publishRaw("cumora:reactions", mustJSON(payload))

	detail := fmt.Sprintf("%s %s %s", agentID, action, emoji)
	if emoji == "👀" && action == "added" {
		detail = fmt.Sprintf("%s %s %s\n\n👀 is only an acknowledgement for long work. Continue in this same turn and choose the appropriate next step: complete the task, ask a concrete clarifying question, or report a clear failure. Only generate an image when the user clearly asked for one.", agentID, action, emoji)
	}
	icon := "web"
	return cliToolResult{
		OK: true,
		Output: struct {
			MessageID string     `json:"messageId"`
			Emoji     string     `json:"emoji"`
			Action    string     `json:"action"`
			Reactions []reaction `json:"reactions"`
		}{messageID, emoji, action, reactions},
		DurationMS: msSince(t0),
		Display: cliToolDisplay{
			Name:   "react",
			Arg:    fmt.Sprintf("%s %s", emoji, utf16Slice(messageID, 12)),
			Status: action,
			Detail: detail,
			Icon:   &icon,
		},
	}, nil
}
