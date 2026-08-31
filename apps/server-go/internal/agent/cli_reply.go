// /runtime/cli reply(#89):cli.ts cmdReply 等价 —— 单口判语
// (monologue 门 → email 自动升格 → 新鲜度预检/HELD → 逐字重复门 →
// 事务内 seq 原子领取+复检+INSERT → 广播)。
package agent

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/email"
)

var (
	reToolCall     = regexp.MustCompile(`(?is)<tool_call>.*?</tool_call>`)
	reFunctionCall = regexp.MustCompile(`(?is)<function_call>.*?</function_call>`)
)

// heldPeerRow:HELD 信封里展示的同伴消息。
type heldPeerRow struct {
	sequence   int64
	authorID   string
	authorName string // 空则渲染 author_id
	body       string
}

// renderHeldLines:HELD 信封的 `  [seq=N] name: body…` 行(空白折叠 + 200 截断)。
func renderHeldLines(rows []heldPeerRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		name := r.authorName
		if name == "" {
			name = r.authorID
		}
		parts[i] = fmt.Sprintf("  [seq=%d] %s: %s", r.sequence, name, utf16Slice(collapseSpaces(r.body), 200))
	}
	return strings.Join(parts, "\n")
}

func collapseSpaces(s string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
}

func (s *Service) cliCmdReply(ctx context.Context, parsed cliParsed) cliResult {
	me, err := cliResolveAs(parsed)
	if err != nil {
		return cliErrThrow(err)
	}
	convoID := ""
	if len(parsed.positional) > 0 {
		convoID = parsed.positional[0]
	}
	// 入口也剥掉幻觉的 <tool_call> XML —— 纵深防御。
	body := strings.TrimSpace(reToolCall.ReplaceAllString(reFunctionCall.ReplaceAllString(parsed.joinBodyArgs(1), ""), ""))
	hasAttachFlag := parsed.flagTruey("attach") || parsed.flagTruey("generate-image") ||
		parsed.flagTruey("attach-text") || parsed.flagTruey("attach-bytes")
	// `--quote / -q <msg_id>`:引用同会话内的一条消息(跨会话引用会泄
	// 露内容,服务端强制同会话)。
	quoteFlag := ""
	if v, ok := parsed.flags["quote"]; ok {
		quoteFlag = fmt.Sprint(v)
	} else if v, ok := parsed.flags["q"]; ok {
		quoteFlag = fmt.Sprint(v)
	}
	quotedMessageID := ""
	if quoteFlag != "" && quoteFlag != "false" {
		quotedMessageID = strings.TrimSpace(quoteFlag)
	}
	if convoID == "" || (body == "" && !hasAttachFlag) {
		return cliErr(`usage: reply <convo_id> "<body>" [--quote <msg_id>] [--attach <url> | --generate-image "<prompt>" [--size square|wide|tall] | --attach-text "<filename>" "<content>" | --attach-bytes "<filename>" --bytes-b64 "<base64>" [--bytes-mime "<mime>"]]`)
	}

	// 成员资格 + actor 是否 agent + 会话基本信息一次查齐。
	var (
		members      cliStrArr
		companyID    string
		kind         string
		actorIsAgent bool
	)
	err = s.DB.QueryRowContext(ctx, `SELECT c.members, c.company_id, c.kind,
	        EXISTS (
	          SELECT 1 FROM participants p
	           WHERE p.id = $2 AND p.company_id = c.company_id
	             AND p.kind = 'agent' AND p.departed_at IS NULL
	        ) AS actor_is_agent
	   FROM conversations c WHERE c.id = $1`, convoID, me,
	).Scan(&members, &companyID, &kind, &actorIsAgent)
	if err == sql.ErrNoRows {
		return cliErr("unknown conversation " + convoID)
	}
	if err != nil {
		return cliErrThrow(err)
	}
	if !containsString(members, me) {
		return cliErr(me + " is not a member of " + convoID)
	}

	/* ─── 反独白门 ───
	 * 3+ 人会话里,别人还没说话前 agent 不能连发第二条。抓的是最常见
	 * 的失败环:agent 发计划 → 同一 agent 立刻发续篇 → 下一个 agent 发
	 * 他自己的同款计划 → 死循环。豁免:双人 DM(自然的补充话题)、
	 * 10 分钟逃生阀(自己的上一条已冷)、显式 --continue/--also。 */
	monologueBypass := parsed.flagTruey("continue") || parsed.flagTruey("also")
	if !monologueBypass && actorIsAgent && len(members) > 2 {
		var lastAuthor string
		var lastAt time.Time
		err := s.DB.QueryRowContext(ctx,
			`SELECT author_id, created_at FROM messages
			   WHERE conversation_id = $1
			   ORDER BY sequence DESC LIMIT 1`, convoID,
		).Scan(&lastAuthor, &lastAt)
		if err != nil && err != sql.ErrNoRows {
			return cliErrThrow(err)
		}
		if err == nil && lastAuthor == me {
			ageMs := time.Since(lastAt).Milliseconds()
			const minGapMs = 10 * 60 * 1000
			if ageMs < minGapMs {
				ageSec := ageMs / 1000
				if rem := ageMs % 1000; rem >= 500 {
					ageSec++
				}
				if ageSec < 1 {
					ageSec = 1
				}
				return cliErr(
					fmt.Sprintf("you already posted in %s %ds ago and nobody has replied yet — ", convoID, ageSec) +
						`you can't post again until someone else speaks. ` +
						`If you have more to say, fold it into your next message when someone responds. ` +
						`Right now: react on the relevant message (cumora react <message_id> 👀 / ✅ / 🎯), ` +
						`or set_turn_status done and step back. ` +
						`Override only if it's truly urgent: rerun with --continue.`)
			}
		}
	}

	/* ─── email 会话自动升格 ───
	 * 让 reply 两条面(chat/email)在 sendViaProvider 路径上收敛;autoSubmitted
	 * 恒真(CLI 回复天然是 agent 驱动)。 */
	if kind == "email" {
		if body == "" {
			return cliErr("email replies require a non-empty body")
		}
		result, e := email.ReplyInEmailConversation(ctx, s.DB, email.ReplyArgs{
			ConversationID: convoID, CompanyID: companyID, AuthorID: me, Body: body, AutoSubmitted: true,
		})
		if e != nil {
			return cliErr("email auto-promote failed: " + e.Error())
		}
		mockTag := ""
		if result.Mock {
			mockTag = " (mock)"
		}
		if result.TransportStatus != "sent" {
			return cliErr(fmt.Sprintf("email reply persisted as failed: %s · %s", tsNullStr(result.Error), result.MessageID))
		}
		effect := CliSideEffect{
			"event":           "message.posted",
			"command":         "reply",
			"medium":          "email",
			"conversationId":  convoID,
			"messageId":       result.MessageID,
			"authorId":        me,
			"companyId":       companyID,
			"visibleToUser":   true,
			"transportStatus": result.TransportStatus,
			"mock":            result.Mock,
		}
		return cliOK("replied via email"+mockTag+" · "+result.MessageID, effect)
	}

	/* ─── 新鲜度预检(SEEN-CURSOR 模型)─── */
	sendAnywayFlag := parsed.flagTruey("send-anyway")
	replyHoldScope := "reply:" + convoID
	preflightApplies := !monologueBypass && len(members) > 2
	heldAck := HoldAcknowledgement{}
	if sendAnywayFlag {
		heldAck = s.ConsumeHold(me, replyHoldScope)
	}
	sendAnywayArmed := heldAck.Armed
	if sendAnywayArmed && preflightApplies && heldAck.HeldUpToSeq != nil {
		// token 只承认"HELD 信封展示过的那个房间"。房间又动了 → 确认作废:
		// flag 永远不得跳过 agent 没看过的消息。
		newer, err := s.cliPeerMessagesAfter(ctx, convoID, me, *heldAck.HeldUpToSeq, 8)
		if err != nil {
			return cliErrThrow(err)
		}
		if len(newer) > 0 {
			sendAnywayArmed = false
			maxHeldSeq := newer[len(newer)-1].sequence
			// 展示过 ⇒ 世界状态的一部分:推进 seen 游标并按新的高水位
			// 重挂 token,深思熟虑后的重发不必对同样的行再 HOLD。
			s.RecordSeen(me, convoID, maxHeldSeq)
			s.RecordHold(me, replyHoldScope, &maxHeldSeq)
			return cliErrCode(
				fmt.Sprintf("HELD — your reply NOT sent. Your --send-anyway acknowledged an EARLIER hold, but the room has moved since: %d newer message(s) in %s you have not been shown:\n%s\n\n", len(newer), convoID, renderHeldLines(newer))+
					`Re-decide against THIS state — usually your draft is now wrong `+
					`(counting: post the next number after the latest, not the one you drafted; `+
					`relay/chain: continue from the latest entry; if a peer above already delivered what you were about to deliver, stand down or react instead). `+
					"Run `cumora reply <convoId> \"<revised body>\"` with your new decision, "+
					`or rerun with --send-anyway only if your draft is STILL correct despite these messages (rare).`, 2)
		}
	}
	if !sendAnywayArmed && preflightApplies {
		// seen 游标 = 该 (agent, convo) 实际被展示过的最高同伴 seq(wake
		// 简报 / glance / messages / HELD 信封都推进)。有没被展示过的同
		// 伴消息 ⇒ HOLD。baseline 0(从未读过 / TTL 过期 / Redis 挂)按
		// 原样 fail-open:wake 简报会在 turn 开始时重建游标。
		baseline := s.GetSeen(me, convoID)
		if baseline > 0 {
			newer, err := s.cliPeerMessagesAfter(ctx, convoID, me, baseline, 8)
			if err != nil {
				return cliErrThrow(err)
			}
			if len(newer) > 0 {
				maxHeldSeq := newer[len(newer)-1].sequence
				s.RecordSeen(me, convoID, maxHeldSeq)
				// 武装 token:此刻 agent 确实见过 HELD 上下文,后续
				// --send-anyway 是真确认而非抢占式跳过。
				s.RecordHold(me, replyHoldScope, &maxHeldSeq)
				ignoredNote := ""
				if sendAnywayFlag {
					ignoredNote = "(Your --send-anyway was IGNORED: it only acknowledges a HOLD you have already been shown — passing it preemptively does nothing.)\n"
				}
				return cliErrCode(
					fmt.Sprintf("HELD — your reply NOT sent. %d newer message(s) in %s you have not been shown:\n%s\n\n", len(newer), convoID, renderHeldLines(newer))+
						ignoredNote+
						"You have now seen these — re-decide against THIS state, then simply re-send: a plain `cumora reply <convoId> \"<revised body>\"` will go through (no flag needed). "+
						`Usually your draft is now wrong (counting: post the next number after the latest; relay/chain: continue from the latest entry; `+
						`if a peer above already delivered what you were about to deliver, the work is DONE — stand down or react instead).`, 2)
			}
		}
		/* ─── 逐字重复门 ───
		 * 预检抓"同伴在我基线之后发帖",但两个 agent 可以在互设基线前就
		 * 各自起草了同一条内容 —— 双双过预检,草稿与最近的同伴帖逐字相
		 * 同。真队友会说"哦,X 抢先了",而不是立刻复读最近一条。只查
		 * 最后一条同伴消息:最窄的有原则规则,不做情景例举。 */
		draftBodyTrimmed := strings.TrimSpace(body)
		if draftBodyTrimmed != "" {
			lastPeer, err := s.cliLastPeerTextMessage(ctx, convoID, me, false)
			if err != nil {
				return cliErrThrow(err)
			}
			if lastPeer != nil && strings.TrimSpace(lastPeer.body) == draftBodyTrimmed {
				s.RecordSeen(me, convoID, lastPeer.sequence)
				s.RecordHold(me, replyHoldScope, &lastPeer.sequence)
				return cliErrCode(
					fmt.Sprintf("HELD — your draft is VERBATIM IDENTICAL to the most recent peer post in %s:\n%s\n\n", convoID, renderHeldLines([]heldPeerRow{*lastPeer}))+
						`They beat you to it. Real teammates don't immediately repeat the same thing — pick a different angle, the NEXT item in a sequence, or stay silent if their post already covers what you wanted to say.`, 2)
			}
		}
	}

	// 引用目标必须存在于本会话 —— agent 应知道自己的引用指针坏了并修
	// 正调用,而不是悄悄发出一条无引用的回复。
	resolvedQuotedID := ""
	var quotedSummary *struct {
		id, authorID, authorName, body string
		sequence                       int64
	}
	if quotedMessageID != "" {
		var (
			qID, qAuthor, qBody string
			qName               sql.NullString
			qSeq                int64
		)
		err := s.DB.QueryRowContext(ctx,
			`SELECT m.id, m.author_id, m.body, m.sequence,
			        COALESCE(p.name, u.display_name) AS author_name
			   FROM messages m
			   LEFT JOIN participants p ON p.id = m.author_id
			   LEFT JOIN users u ON u.id = m.author_id
			  WHERE m.id = $1 AND m.conversation_id = $2`, quotedMessageID, convoID,
		).Scan(&qID, &qAuthor, &qBody, &qSeq, &qName)
		if err == sql.ErrNoRows {
			return cliErr("--quote target " + quotedMessageID + " not found in " + convoID)
		}
		if err != nil {
			return cliErrThrow(err)
		}
		resolvedQuotedID = qID
		name := qName.String
		if name == "" {
			name = qAuthor
		}
		quotedSummary = &struct {
			id, authorID, authorName, body string
			sequence                       int64
		}{qID, qAuthor, name, utf16Slice(qBody, 240), qSeq}
	}

	/* ─── 附件三味 + 图像生成 ─── */
	var attachment *agentAttachment
	if parsed.flagTruey("generate-image") {
		prompt, _ := parsed.flagStr("generate-image")
		prompt = strings.TrimSpace(prompt)
		if prompt == "" {
			return cliErr("--generate-image requires a non-empty prompt")
		}
		// 与 `image generate` 同一租户级 claim:图像模型不在乎调用来自哪
		// 个入口,但同伴 agent 在乎是否有人重复烧钱。
		if blocked := s.cliTryClaimTenantWork(companyID, me, "image-generate", prompt); blocked != nil {
			return *blocked
		}
		att, e := s.cliGenerateAndUploadImage(prompt, parsed.flagStrOr("size", "square"), companyID, me)
		s.ReleaseWork("tenant:"+companyID, me, "image-generate", prompt)
		if e != nil {
			return cliErr("image generation failed: " + e.Error())
		}
		attachment = &att
	} else if v, ok := parsed.flagStr("attach-text"); ok {
		filename := utf16Slice(strings.TrimSpace(v), 200)
		content, hasContent := parsed.flagStr("attach-text-content")
		if !hasContent {
			content = body
		}
		if filename == "" || content == "" {
			return cliErr("--attach-text requires a filename and content")
		}
		att, e := cliSaveTextAttachment(filename, content)
		if e != nil {
			return cliErrThrow(e)
		}
		attachment = &att
	} else if v, ok := parsed.flagStr("attach-bytes"); ok {
		filename := utf16Slice(strings.TrimSpace(v), 200)
		b64 := strings.TrimSpace(parsed.flagStrOr("bytes-b64", ""))
		mime := ""
		if m, ok := parsed.flagStr("bytes-mime"); ok {
			mime = strings.ToLower(strings.TrimSpace(m))
		}
		if filename == "" || b64 == "" {
			return cliErr("--attach-bytes requires a filename and --bytes-b64 \"<base64>\"")
		}
		att, e := cliSaveBytesAttachment(filename, b64, mime)
		if e != nil {
			return cliErr("attach-bytes failed: " + e.Error())
		}
		attachment = &att
	} else if v, ok := parsed.flagStr("attach"); ok {
		name := v
		if parts := strings.Split(v, "/"); len(parts) > 0 {
			name = parts[len(parts)-1]
		}
		if name == "" {
			name = "attachment"
		}
		if an, ok := parsed.flagStr("attach-name"); ok {
			name = an
		}
		attachment = &agentAttachment{URL: v, Name: name, Kind: "img"}
	}

	// 正文被当作文件内容消耗(未显式给 --attach-text-content)时,正文
	// 不随消息发出 —— 文件即消息。
	consumedAsTextContent := false
	if _, ok := parsed.flagStr("attach-text"); ok {
		if _, explicit := parsed.flagStr("attach-text-content"); !explicit {
			consumedAsTextContent = true
		}
	}
	finalBody := body
	if consumedAsTextContent {
		finalBody = ""
	}

	/* ─── 事务:seq 原子领取 + 锁内逐字复检 + INSERT ───
	 * conversation_counters 的 UPSERT 拿行锁并持到 COMMIT,把同一会话
	 * 的并发 reply 串行化过临界区;持锁期间重查最后一条同伴帖(已提交
	 * 可见),逐字重复则 ROLLBACK+HELD —— 关掉读-写窗口的 TOCTOU。 */
	messageID := "m-" + uuidHex()
	// 事务豁免(#213):锁内复检命中逐字重复时显式 tx.Rollback() 并保留
	// 其错误映射,回滚后还有 Redis 副作用(RecordSeen/RecordHold)与定制
	// HELD 文案;WithTx 吞回滚错误,无法等价重构。
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return cliErrThrow(err)
	}
	defer tx.Rollback()
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO conversation_counters (conversation_id, next_sequence)
		 VALUES ($1, 2)
		 ON CONFLICT (conversation_id) DO UPDATE SET next_sequence = conversation_counters.next_sequence + 1
		 RETURNING next_sequence - 1 AS seq`, convoID,
	).Scan(&sequence); err != nil {
		return cliErrThrow(err)
	}
	// 锁内复检无视 --send-anyway、DM 豁免与 --continue:逐字复读同伴刚
	// 发的内容没有任何正当用例(T9:agent 用 --send-anyway 强推了它自己
	// 已内化为"收官定式"的逐字重复 —— 必须由服务端强制)。
	if len(members) > 2 {
		draftBodyTrimmed := strings.TrimSpace(body)
		if draftBodyTrimmed != "" {
			var pSeq int64
			var pAuthor, pBody string
			err := tx.QueryRowContext(ctx,
				`SELECT sequence, author_id, body FROM messages
				  WHERE conversation_id = $1 AND author_id <> $2 AND kind = 'text'
				  ORDER BY sequence DESC LIMIT 1`, convoID, me,
			).Scan(&pSeq, &pAuthor, &pBody)
			if err != nil && err != sql.ErrNoRows {
				return cliErrThrow(err)
			}
			if err == nil && strings.TrimSpace(pBody) == draftBodyTrimmed {
				if err := tx.Rollback(); err != nil {
					return cliErrThrow(err)
				}
				s.RecordSeen(me, convoID, pSeq)
				s.RecordHold(me, replyHoldScope, &pSeq)
				bypassNote := ""
				if sendAnywayFlag {
					bypassNote = " (and --send-anyway does NOT bypass this check — verbatim-dup is never legitimate)"
				}
				return cliErrCode(
					fmt.Sprintf("HELD — verbatim duplicate of the immediately-prior peer post in %s:\n  [seq=%d] %s: %s\n\n", convoID, pSeq, pAuthor, utf16Slice(collapseSpaces(pBody), 200))+
						"They posted the exact same content"+bypassNote+". Real teammates don't immediately repeat the same word. Pick the NEXT item, a different angle, or stay silent if their post already covers what you wanted.", 2)
			}
		}
	}
	var attachmentJSON any
	if attachment != nil {
		attachmentJSON = compactJSON(attachment)
	}
	var quotedArg any
	if resolvedQuotedID != "" {
		quotedArg = resolvedQuotedID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, conversation_id, author_id, kind, body, sequence, attachment, quoted_message_id, company_id)
		 VALUES ($1,$2,$3,'text',$4,$5,$6::jsonb,$7,$8)`,
		messageID, convoID, me, finalBody, sequence, attachmentJSON, quotedArg, companyID); err != nil {
		return cliErrThrow(err)
	}
	if err := tx.Commit(); err != nil {
		return cliErrThrow(err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE conversations SET updated_at = NOW() WHERE id = $1`, convoID); err != nil {
		return cliErrThrow(err)
	}
	// 发帖即自动 ack:我显然看过我正在回复的消息。
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO conversation_reads (user_id, conversation_id, last_read_at)
		 VALUES ($1, $2, NOW())
		 ON CONFLICT (user_id, conversation_id) DO UPDATE SET last_read_at = NOW()`,
		me, convoID); err != nil {
		return cliErrThrow(err)
	}
	// seen 边界推到自己刚插入的 seq:下次预检只对 seq 更大的同伴消息
	// HOLD。纯 Redis 副作用,不碰 loadInbox 依赖的任何东西。
	s.RecordSeen(me, convoID, sequence)
	// 丢弃残留 token:本次发送没走 override,已确认但未用的 hold 不得
	// 武装后续的抢占式 --send-anyway。
	s.ClearHold(me, replyHoldScope)
	// 新消息改变了会话的停滞状态,旧的"放弃这个停滞状态"计数已过时。
	s.resetStallNudgeDeclines(convoID)

	// 广播:前端、调度器都听 CH_MESSAGE_NEW。
	msg := map[string]any{
		"id":             messageID,
		"conversationId": convoID,
		"authorId":       me,
		"kind":           "text",
		"body":           finalBody,
		"sequence":       sequence,
		"at":             isoNowMs(),
	}
	if attachment != nil {
		msg["attachment"] = attachment
	}
	if resolvedQuotedID != "" {
		msg["quotedMessageId"] = resolvedQuotedID
	}
	if quotedSummary != nil {
		msg["quoted"] = map[string]any{
			"id":         quotedSummary.id,
			"authorId":   quotedSummary.authorID,
			"authorName": quotedSummary.authorName,
			"kind":       "text",
			"body":       quotedSummary.body,
			"sequence":   quotedSummary.sequence,
		}
	}
	eventsPublishMessageNew(ctx, &companyID, convoID, msg)

	attachmentNote := ""
	if attachment != nil {
		attachmentNote = fmt.Sprintf(" · attached %s %q", attachment.Kind, attachment.Name)
	}
	quoteNote := ""
	if resolvedQuotedID != "" {
		quoteNote = " · quoted " + resolvedQuotedID
	}
	effect := CliSideEffect{
		"event":           "message.posted",
		"command":         "reply",
		"medium":          "chat",
		"conversationId":  convoID,
		"messageId":       messageID,
		"sequence":        sequence,
		"authorId":        me,
		"companyId":       companyID,
		"visibleToUser":   true,
		"attachment":      attachment != nil,
		"quotedMessageId": nilIfEmpty(resolvedQuotedID),
	}
	return cliOK(fmt.Sprintf("sent (%s, seq %d)%s%s", messageID, sequence, attachmentNote, quoteNote), effect)
}

// tsNullStr:TS 模板字面量里 null 渲染为 "null"。
func tsNullStr(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// cliPeerMessagesAfter:某 seq 之后的同伴消息(升序,LIMIT n),含作者
// 显示名(participants 名或 users.display_name)。
func (s *Service) cliPeerMessagesAfter(ctx context.Context, convoID, me string, afterSeq int64, limit int) ([]heldPeerRow, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT m.sequence, m.author_id, m.body,
		        COALESCE(p.name, u.display_name) AS author_name
		   FROM messages m
		   LEFT JOIN participants p ON p.id = m.author_id
		   LEFT JOIN users u ON u.id = m.author_id
		  WHERE m.conversation_id = $1
		    AND m.author_id <> $2
		    AND m.sequence > $3
		  ORDER BY m.sequence ASC
		  LIMIT $4`, convoID, me, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []heldPeerRow
	for rows.Next() {
		var r heldPeerRow
		var name sql.NullString
		if err := rows.Scan(&r.sequence, &r.authorID, &r.body, &name); err != nil {
			return nil, err
		}
		r.authorName = name.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// cliLastPeerTextMessage:最后一条同伴 text 消息;withName=带作者显示名。
func (s *Service) cliLastPeerTextMessage(ctx context.Context, convoID, me string, withName bool) (*heldPeerRow, error) {
	var r heldPeerRow
	var name sql.NullString
	var err error
	if withName {
		err = s.DB.QueryRowContext(ctx,
			`SELECT m.sequence, m.author_id, m.body,
			        COALESCE(p.name, u.display_name) AS author_name
			   FROM messages m
			   LEFT JOIN participants p ON p.id = m.author_id
			   LEFT JOIN users u ON u.id = m.author_id
			  WHERE m.conversation_id = $1
			    AND m.author_id <> $2
			    AND m.kind = 'text'
			  ORDER BY m.sequence DESC
			  LIMIT 1`, convoID, me,
		).Scan(&r.sequence, &r.authorID, &r.body, &name)
		r.authorName = name.String
	} else {
		err = s.DB.QueryRowContext(ctx,
			`SELECT sequence, author_id, body FROM messages
			  WHERE conversation_id = $1 AND author_id <> $2 AND kind = 'text'
			  ORDER BY sequence DESC LIMIT 1`, convoID, me,
		).Scan(&r.sequence, &r.authorID, &r.body)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// resetStallNudgeDeclines:清停滞-放弃计数(fire-and-forget;失败最坏
// 情况是计数自然 TTL)。
func (s *Service) resetStallNudgeDeclines(conversationID string) {
	if conversationID == "" {
		return
	}
	if rdb := s.redis(); rdb != nil {
		go func() {
			_ = rdb.Del(context.Background(), "cumora:nudge-declines:"+conversationID).Err()
		}()
	}
}

// cliTryClaimTenantWork:租户级工作 claim;被同伴占住时返回 err 结果
// (调用方原样传播),拿到则返回 nil。
func (s *Service) cliTryClaimTenantWork(companyID, agentID, taskType, subject string) *cliResult {
	const claimTTL = 5 * 60 // TS inproc claimWork 的 5 分钟自动过期
	r := s.ClaimWork("tenant:"+companyID, agentID, taskType, subject, claimTTL)
	if r.Accepted {
		return nil
	}
	existing := r.Existing
	return &cliResult{ok: false, exitCode: 1, text: cliDuplicateWorkMessage(*existing)}
}

func cliDuplicateWorkMessage(existing WorklogEntry) string {
	ageSec := (time.Now().UnixMilli() - existing.StartedAt) / 1000
	if ageSec < 1 {
		ageSec = 1
	}
	return existing.AgentID + " started " + existing.TaskType + " on \"" + existing.Subject + "\" " +
		fmt.Sprintf("%ds", ageSec) + " ago — " +
		`don't duplicate. Wait for them to finish (claims auto-expire after 5 min if they stall), ` +
		`or react on the relevant message and step back. If your angle is genuinely different from theirs, ` +
		`rephrase your subject distinctly enough that the system sees it as a separate request.`
}
