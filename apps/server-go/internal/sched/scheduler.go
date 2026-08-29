// sched 包 scheduler —— 唤醒调度(#62):scheduler.ts 的 Go 等价。
// 事件驱动面:订阅 cumora:msg.new(claimAndWake 多副本去重 → 成员唤醒
// + steer 轮中注入)与 cumora:polls(投票/关闭 → 唤醒投票发起 agent)。
// 预算面:低优先级合成唤醒 20/min 进程级滚动窗;agent 驱动唤醒的
// turn-rate 地板(30/min,内容盲);steer 每 agent 30/min。
// #82 时代的 WakeAgent/WakeMentionedAgents(看板 manual 唤醒)保持原签名。
package sched

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/redis/go-redis/v9"
)

// WakeAgent 已迁 agent 包(#140):agent.Service.WakeAgent 经 SetWakeHook
// 钩子回到本包 WakeOne(runtime.New 接线),调用点签名不变。

/* ───────── 唤醒载荷 ───────── */

// SteerPayload: 轮中注入载荷(消息新建 handler 构建;busy 租约存在时
// 作为 steer 事件补发,运行中的引擎会话在下一 hop 边界吸收)。
type SteerPayload struct {
	MessageID      string
	ConversationID string
	AuthorName     string
	Body           string
	// Tenant tag (empty when the event has no companyId) — the steer-ack
	// typing broadcast relies on it to route to clients.
	CompanyID string
}

// BackgroundBrief: idle/scanner 合成唤醒携带的内部简报(渲染为普通模型输入)。
type BackgroundBrief struct {
	Source string `json:"source,omitempty"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// WakeOpts: 唤醒附加选项(idleReason 只渲染在 idle 合成唤醒上)。
type WakeOpts struct {
	IdleReason      string
	BackgroundBrief *BackgroundBrief
	PollBrief       map[string]any
}

// envIntRaw:符号感知的环境整数(0/-1 原样返回);缺键/非数 → ok=false。
// 与 envIntOr(0→默认)相反——间隔类 env 的 TS 语义是"0=禁用",
// 必须让 0 活着到达调用方的禁用分支。
func envIntRaw(name string) (int64, bool) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

/* ───────── 低优先级合成唤醒预算(FUSE-cap 事故) ───────── */

const lowPriorityWakeBudgetPerMin = 20

var lowPriMu sync.Mutex
var lowPriWindowStart = time.Now()
var lowPriUsed, lowPriDropped int

// consumeLowPriorityWakeBudget: 进程级 60s 滚动窗;idle/background_scan
// 超预算即丢弃(下一 tick 重评估),message.new/manual 永不节流。
func consumeLowPriorityWakeBudget() bool {
	now := time.Now()
	lowPriMu.Lock()
	defer lowPriMu.Unlock()
	if now.Sub(lowPriWindowStart) >= time.Minute {
		if lowPriDropped > 0 {
			slog.Warn("[scheduler] low-priority wake budget window closing",
				"used", lowPriUsed, "dropped", lowPriDropped)
		}
		lowPriWindowStart = now
		lowPriUsed, lowPriDropped = 0, 0
	}
	if lowPriUsed >= lowPriorityWakeBudgetPerMin {
		lowPriDropped++
		return false
	}
	lowPriUsed++
	return true
}

// ConsumeLowPriorityWakeBudgetForTests: 测试重置入口(生产不调用)。
func ResetLowPriorityWakeBudgetForTests() {
	lowPriMu.Lock()
	defer lowPriMu.Unlock()
	lowPriWindowStart = time.Unix(0, 0)
	lowPriUsed, lowPriDropped = 0, 0
}

/* ───────── agent turn 地板(内容盲成本上限) ───────── */

const agentTurnRatePerMinute = 30

// ConsumeAgentTurnToken: 每 agent 滚动 60s 激活预算(仅 agent 驱动唤醒
// 消费;人类驱动永不节流)。Redis 错误 fail-open。
func (s *S) ConsumeAgentTurnToken(agentID string) bool {
	rdb := s.RDB
	if rdb == nil {
		return true
	}
	count, err := rdb.Incr(ctxBG, "cumora:turn-rate:"+agentID).Result()
	if err != nil {
		return true // fail-open
	}
	if count == 1 {
		_ = rdb.Expire(ctxBG, "cumora:turn-rate:"+agentID, 60*time.Second).Err()
	}
	return count <= agentTurnRatePerMinute
}

/* ───────── steer 速率限制 ───────── */

const steerRatePerMinute = 30

func (s *S) consumeSteerRateToken(agentID string) bool {
	rdb := s.RDB
	if rdb == nil {
		return true
	}
	count, err := rdb.Incr(ctxBG, "cumora:steer-rate:"+agentID).Result()
	if err != nil {
		return true // fail-open
	}
	if count == 1 {
		_ = rdb.Expire(ctxBG, "cumora:steer-rate:"+agentID, 60*time.Second).Err()
	}
	return count <= steerRatePerMinute
}

var steerEnabledRe = regexp.MustCompile(`(?i)^(false|0|no|off)$`)

func steerEnabled() bool {
	return !steerEnabledRe.MatchString(getenv("STEER_ENABLED")) // TS 正则不 trim:带空格的 " false " = 启用
}

/* ───────── 唤醒核心 ───────── */

// wakeOne: scheduler.wakeOne 等价——预算闸 → Deliver(wake)→ busy 时
// 补发 steer(+steer-ack typing)→ 0 接收者记日志(inbox 持久兜底)。
func (s *S) WakeOne(agentID, reason string, conversationID *string, steer *SteerPayload, opts *WakeOpts) {
	if s.Bus == nil {
		slog.Info("[scheduler] daemon offline — wake deferred to reconnect", "agent", agentID, "reason", reason)
		return
	}
	if (reason == "idle" || reason == "background_scan") && !consumeLowPriorityWakeBudget() {
		slog.Warn("[scheduler] synthetic wake dropped: budget exceeded", "agent", agentID, "reason", reason)
		return
	}
	payload := map[string]any{
		"kind":           "wake",
		"reason":         reason,
		"conversationId": nil,
	}
	if conversationID != nil {
		payload["conversationId"] = *conversationID
	}
	if opts != nil {
		if opts.IdleReason != "" {
			payload["idleReason"] = opts.IdleReason
		}
		if opts.BackgroundBrief != nil {
			payload["backgroundBrief"] = opts.BackgroundBrief
		}
		if opts.PollBrief != nil {
			payload["pollBrief"] = opts.PollBrief
		}
	}
	delivered, err := s.Bus.Deliver(agentID, payload)
	if err != nil {
		slog.Warn("[scheduler] wake deliver failed", "agent", agentID, "err", err)
		return
	}

	// 轮中注入:busy 租约存在 → 同内容补发 steer;steer-ack typing 让
	// 人类立刻看到"<agent> 正在输入"(内容要等下一 hop 边界才真正入上下文)。
	if steer != nil && delivered > 0 && steerEnabled() {
		if s.busy(agentID) {
			allowed := s.consumeSteerRateToken(agentID)
			if !allowed {
				slog.Warn("[scheduler] steer rate-limited; falling back to wake-only", "agent", agentID)
			} else {
				if _, err := s.Bus.DeliverSteer(agentID, map[string]any{
					"conversationId": steer.ConversationID,
					"messageId":      steer.MessageID,
					"authorName":     steer.AuthorName,
					"body":           steer.Body,
				}); err != nil {
					// TS 语义:失败记日志,steer-ack typing 照发(部分故障下
					// 人类仍要看到"正在输入"——内容在 DB,下一 wake 兜底)。
					slog.Warn("[scheduler] deliverSteer failed", "agent", agentID, "err", err)
				}
				if steer.ConversationID != "" {
					events.Typing(ctxBG, steer.CompanyID, steer.ConversationID, agentID, false)
				}
			}
		}
	}

	if delivered == 0 {
		// BYOA 唯一执行层(ADR 0003):0 接收者 = daemon 未订阅(主机离线/
		// 休眠),无可拉起对象;唤醒经 inbox 持久,重连排水兜底。
		slog.Info("[scheduler] daemon offline — wake deferred to reconnect", "agent", agentID, "reason", reason)
	}
}

/* ───────── 静音投递契约 ───────── */

// shouldDeliverToMutedAgent: 静音房仅三豁免——direct、对本人消息的引用
// 回复、精确 @id 提及(边界保护:前邻非词非@、后继非词非连字符)。
// TS 侧为 `(^|[^\w@])@id(?![\w-])`/i;RE2 无前后视,改为手工边界扫描,
// JS \w = [A-Za-z0-9_]。
func shouldDeliverToMutedAgent(agentID, conversationKind, body string, quotedAuthorID *string) bool {
	if conversationKind == "direct" {
		return true
	}
	if quotedAuthorID != nil && *quotedAuthorID == agentID {
		return true
	}
	// 在原文上做 ASCII 大小写折叠匹配:strings.ToLower 的 Unicode 简单
	// 映射会把 İ(U+0130)→i、K(U+212A)→k,把 TS 判为合法边界的非 ASCII
	// 邻接字误折成词字节。原文字节 ≥0x80 一律非词(TS [^\w@] 对 CJK 同
	// 判 true),折叠只对 <0x80 的 ASCII 字母做。
	fold := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	needle := []byte("@" + agentID)
	for i := range needle {
		needle[i] = fold(needle[i])
	}
	for from := 0; from+len(needle) <= len(body); {
		match := true
		for i, nb := range needle {
			if fold(body[from+i]) != nb {
				match = false
				break
			}
		}
		if !match {
			from++
			continue
		}
		idx := from
		// 前邻阻塞 = ASCII 词字节或 @;非 ASCII 字节(CJK/İ 等)一律
		// 不阻塞(TS [^\w@] 同判);串首恒不阻塞。
		blockedBefore := idx > 0 && body[idx-1] < 0x80 &&
			(isJSWordRune(body[idx-1]) || body[idx-1] == '@')
		after := idx + len(needle)
		// 后继阻塞 = ASCII 词字节或 -;串尾恒不阻塞。
		blockedAfter := after < len(body) && body[after] < 0x80 &&
			(isJSWordRune(body[after]) || body[after] == '-')
		if !blockedBefore && !blockedAfter {
			return true
		}
		from = idx + 1
	}
	return false
}

func isJSWordRune(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

// ShouldDeliverToMutedAgent: 导出供测试(wire 级回归锚)。
func ShouldDeliverToMutedAgent(agentID, conversationKind, body string, quotedAuthorID *string) bool {
	return shouldDeliverToMutedAgent(agentID, conversationKind, body, quotedAuthorID)
}

/* ───────── author 名字缓存(Fix #10) ───────── */

const authorNameCacheTTLSec = 5 * 60
const authorNameCacheMax = 5000

type cachedName struct {
	name      string
	expiresAt time.Time
}

var authorNameMu sync.Mutex
var authorNameCache = map[string]cachedName{}
var authorNameOrder []string // 插入序,容量裁剪时淘汰最旧(TS Map 迭代序等价)

// resolveAuthorName: steer 前缀的显示名;DB 错误不缓存、回落 id。
func (s *S) resolveAuthorName(ctx context.Context, authorID string) string {
	now := time.Now()
	authorNameMu.Lock()
	if hit, ok := authorNameCache[authorID]; ok && hit.expiresAt.After(now) {
		authorNameMu.Unlock()
		return hit.name
	}
	authorNameMu.Unlock()
	var name sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT name FROM participants WHERE id = $1 LIMIT 1`, authorID).Scan(&name)
	if err != nil {
		return authorID
	}
	resolved := authorID
	if name.Valid && name.String != "" {
		resolved = name.String
	}
	authorNameMu.Lock()
	cacheAuthorNameLocked(authorID, resolved, now)
	authorNameMu.Unlock()
	return resolved
}

func cacheAuthorNameLocked(id, name string, now time.Time) {
	if _, exists := authorNameCache[id]; !exists {
		authorNameOrder = append(authorNameOrder, id)
	}
	if len(authorNameOrder) >= authorNameCacheMax {
		// 先清过期,仍超则按插入序淘汰最旧。
		kept := authorNameOrder[:0]
		for _, k := range authorNameOrder {
			if c, ok := authorNameCache[k]; ok && c.expiresAt.After(now) {
				kept = append(kept, k)
			} else {
				delete(authorNameCache, k)
			}
		}
		authorNameOrder = kept
		for len(authorNameOrder) >= authorNameCacheMax {
			first := authorNameOrder[0]
			authorNameOrder = authorNameOrder[1:]
			delete(authorNameCache, first)
		}
	}
	authorNameCache[id] = cachedName{name: name, expiresAt: now.Add(authorNameCacheTTLSec * time.Second)}
}

// resolveParticipantNames: 批量显示名(poll brief 用);未解析回落 id。
func (s *S) resolveParticipantNames(ctx context.Context, ids []string) map[string]string {
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	now := time.Now()
	var missing []string
	authorNameMu.Lock()
	for _, id := range ids {
		if hit, ok := authorNameCache[id]; ok && hit.expiresAt.After(now) {
			out[id] = hit.name
		} else {
			missing = append(missing, id)
		}
	}
	authorNameMu.Unlock()
	if len(missing) == 0 {
		return out
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name FROM participants WHERE id = ANY($1::text[])`, pqArray(missing))
	if err != nil {
		for _, id := range missing {
			out[id] = id
		}
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name sql.NullString
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		if !id.Valid {
			continue
		}
		resolved := id.String
		if name.Valid && name.String != "" {
			resolved = name.String
		}
		authorNameMu.Lock()
		cacheAuthorNameLocked(id.String, resolved, now)
		authorNameMu.Unlock()
		out[id.String] = resolved
	}
	for _, id := range missing {
		if _, ok := out[id]; !ok {
			out[id] = id
		}
	}
	return out
}

// ResetAuthorNameCacheForTests: 测试隔离入口(生产不调用)。
func ResetAuthorNameCacheForTests() {
	authorNameMu.Lock()
	defer authorNameMu.Unlock()
	authorNameCache = map[string]cachedName{}
	authorNameOrder = nil
}

/* ───────── message.new → claimAndWake ───────── */

type schedulerMsgNew struct {
	Type           string `json:"type"`
	ConversationID string `json:"conversationId"`
	CompanyID      string `json:"companyId"`
	Message        struct {
		ID              string `json:"id"`
		AuthorID        string `json:"authorId"`
		Kind            string `json:"kind"`
		Body            string `json:"body"`
		QuotedMessageID string `json:"quotedMessageId"`
		Quoted          *struct {
			AuthorID string `json:"authorId"`
		} `json:"quoted"`
	} `json:"message"`
}

// claimAndWake: 多副本去重——SETNX per message id(TTL 60s),抢到的副本
// 才 fan-out;Redis 错误视为他副本持有(TS .catch(()=>null) 平价)。
func (s *S) claimAndWake(payload schedulerMsgNew) {
	rdb := s.RDB
	if rdb == nil {
		return
	}
	claimed, err := rdb.SetNX(ctxBG, "cumora:wake-claim:"+payload.Message.ID, "1", 60*time.Second).Result()
	if err != nil || !claimed {
		return
	}
	s.wakeFromMessage(payload)
}

// wakeFromMessage: scheduler.wake 等价——steer 载荷构建(非 system 且
// 有正文)、成员+静音查询、静音豁免、agent 作者的 turn-rate 地板、
// 并行 fan-out。
func (s *S) wakeFromMessage(payload schedulerMsgNew) {
	ctx := ctxBG
	conversationID := payload.ConversationID
	authorID := payload.Message.AuthorID

	var steer *SteerPayload
	if payload.Message.Kind != "system" && len(payload.Message.Body) > 0 {
		steer = &SteerPayload{
			MessageID:      payload.Message.ID,
			ConversationID: conversationID,
			AuthorName:     s.resolveAuthorName(ctx, authorID),
			Body:           payload.Message.Body,
			CompanyID:      payload.CompanyID,
		}
	}

	var (
		members   []string
		kind      sql.NullString
		mutedJSON []byte
	)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.members, c.kind,
		       COALESCE(to_json(array_agg(mu.user_id) FILTER (WHERE mu.user_id IS NOT NULL))::jsonb, '[]'::jsonb) AS muted_agent_ids
		  FROM conversations c
		  LEFT JOIN conversation_mutes mu ON mu.conversation_id = c.id
		   AND (mu.muted_until IS NULL OR mu.muted_until > NOW())
		 WHERE c.id = $1
		 GROUP BY c.id`, conversationID)
	if err == nil {
		defer rows.Close()
		if rows.Next() {
			var membersJSON []byte
			if err := rows.Scan(&membersJSON, &kind, &mutedJSON); err == nil {
				_ = json.Unmarshal(membersJSON, &members)
			}
		}
	}
	var mutedAgent []string
	_ = json.Unmarshal(mutedJSON, &mutedAgent)
	mutedSet := map[string]struct{}{}
	for _, m := range mutedAgent {
		mutedSet[m] = struct{}{}
	}

	var quotedAuthorID *string
	if payload.Message.Quoted != nil && payload.Message.Quoted.AuthorID != "" {
		qa := payload.Message.Quoted.AuthorID
		quotedAuthorID = &qa
	} else if payload.Message.QuotedMessageID != "" && len(mutedSet) > 0 {
		var qa sql.NullString
		if err := s.DB.QueryRowContext(ctx,
			`SELECT author_id FROM messages WHERE id = $1 AND conversation_id = $2`,
			payload.Message.QuotedMessageID, conversationID).Scan(&qa); err == nil && qa.Valid {
			quotedAuthorID = &qa.String
		}
	}

	var agentRecipients []string
	for _, m := range members {
		if m == authorID {
			continue
		}
		if !s.IsAgent(ctx, m) {
			continue
		}
		if _, muted := mutedSet[m]; muted {
			convKind := ""
			if kind.Valid {
				convKind = kind.String
			}
			if !shouldDeliverToMutedAgent(m, convKind, payload.Message.Body, quotedAuthorID) {
				continue
			}
		}
		agentRecipients = append(agentRecipients, m)
	}

	// 内容盲成本地板:agent 的消息唤醒同伴时,超预算的接收者丢弃;
	// 人类驱动永不节流。是否回应交给小模型。
	recipients := agentRecipients
	if s.IsAgent(ctx, authorID) {
		allowed := make([]string, 0, len(agentRecipients))
		for _, m := range agentRecipients {
			if s.ConsumeAgentTurnToken(m) {
				allowed = append(allowed, m)
			}
		}
		if dropped := len(agentRecipients) - len(allowed); dropped > 0 {
			slog.Warn("[scheduler] turn-rate floor: dropped agent-driven wake(s)",
				"convo", conversationID, "dropped", dropped)
		}
		recipients = allowed
	}

	// 并行扇出(信号量限流):像 Slack 房间一样同时唤醒所有订阅者——
	// 协调发生在 agent 层(glance/礼让),不在调度器串行。
	s.fanOutWake(recipients, conversationID, steer)
}

// IsAgent: personas.isAgent 等价(getPersona ≠ null;含进程内缓存)。
func (s *S) IsAgent(ctx context.Context, id string) bool {
	p, err := s.GetPersona(ctx, id)
	return err == nil && p != nil
}

var wakeFanoutOnce sync.Once
var wakeFanoutSem chan struct{}

func (s *S) fanOutSem() chan struct{} {
	wakeFanoutOnce.Do(func() {
		n := 6
		if v, ok := envIntRaw("WAKE_FANOUT_CONCURRENCY"); ok {
			n = int(v)
			if n <= 0 {
				n = 1 // TS Semaphore 对 0/负钳 1(热循环护身)
			}
		}
		wakeFanoutSem = make(chan struct{}, n)
	})
	return wakeFanoutSem
}

// fanOutWake: 有界并发扇出;单个失败只告警,绝不冒泡进 Redis 回调。
func (s *S) fanOutWake(recipients []string, conversationID string, steer *SteerPayload) {
	var wg sync.WaitGroup
	for _, m := range recipients {
		wg.Add(1)
		go func(agentID string) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("[scheduler] wakeOne panicked", "agent", agentID, "recover", rec)
				}
			}()
			s.fanOutSem() <- struct{}{}
			defer func() { <-s.fanOutSem() }()
			convo := conversationID
			s.WakeOne(agentID, "message.new", &convo, steer, nil)
		}(m)
	}
	wg.Wait()
}

/* ───────── 订阅环(startScheduler) ───────── */

var schedulerStarted sync.Once

// StartScheduler: 订阅 cumora:msg.new + cumora:polls;幂等。消息事件 →
// claimAndWake(fire-and-forget,断言不逃逸);poll 事件 → handlePollUpdated。
func (s *S) StartScheduler() {
	schedulerStarted.Do(func() {
		rdb := s.RDB
		if rdb == nil {
			slog.Info("[scheduler] redis unavailable — mailbox scheduler idle")
			return
		}
		go func() {
			for {
				err := s.pumpScheduler(rdb)
				if ctxBG.Err() != nil {
					return
				}
				slog.Warn("[scheduler] pubsub loop exited — reconnecting in 1s", "err", err)
				time.Sleep(time.Second)
			}
		}()
	})
}

func (s *S) pumpScheduler(rdb redis.UniversalClient) error {
	sub := rdb.Subscribe(ctxBG, events.ChMessageNew, chPolls)
	defer sub.Close()
	slog.Info("[scheduler] mailbox scheduler listening", "channels", []string{events.ChMessageNew, chPolls}, "runtime", "byoa-only")
	ch := sub.Channel()
	for msg := range ch {
		raw := []byte(msg.Payload)
		switch msg.Channel {
		case events.ChMessageNew:
			var payload schedulerMsgNew
			if json.Unmarshal(raw, &payload) != nil || payload.Type != "message.new" {
				continue
			}
			go func(p schedulerMsgNew) {
				defer func() {
					if rec := recover(); rec != nil {
						slog.Error("[scheduler] claimAndWake panicked", "msg", p.Message.ID, "recover", rec)
					}
				}()
				s.claimAndWake(p)
			}(payload)
		case chPolls:
			var payload pollUpdatedEvent
			if json.Unmarshal(raw, &payload) != nil || payload.Type != "poll.updated" {
				continue
			}
			go func(p pollUpdatedEvent) {
				defer func() {
					if rec := recover(); rec != nil {
						slog.Error("[scheduler] handlePollUpdated panicked", "msg", p.MessageID, "recover", rec)
					}
				}()
				s.handlePollUpdated(p)
			}(payload)
		}
	}
	return nil
}

/* ───────── poll 更新 → 唤醒发起者 ───────── */

const chPolls = "cumora:polls"

const pollVoteWakeDebounceSec = 8
const pollCloseWakeClaimSec = 600

type pollUpdatedEvent struct {
	Type           string `json:"type"`
	CompanyID      string `json:"companyId"`
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Poll           struct {
		Question     string                      `json:"question"`
		Mode         string                      `json:"mode"`
		Options      []struct{ ID, Text string } `json:"options"`
		ExpiresAt    *string                     `json:"expiresAt"`
		ClosedAt     *string                     `json:"closedAt"`
		ClosedReason *string                     `json:"closedReason"`
	} `json:"poll"`
	Tallies []struct {
		OptionID string   `json:"optionId"`
		Count    int64    `json:"count"`
		VoterIDs []string `json:"voterIds"`
	} `json:"tallies"`
	ActorID *string `json:"actorId"`
}

// handlePollUpdated: 投票/关闭 → 唤醒发起 poll 的 agent(自己操作不
// 自唤);投票 8s 去重、关闭 600s 幂等;构建实时票数简报随唤醒携带。
func (s *S) handlePollUpdated(event pollUpdatedEvent) {
	ctx := ctxBG
	if event.CompanyID == "" {
		return
	}
	var authorID string
	var membersJSON []byte
	err := s.DB.QueryRowContext(ctx, `
		SELECT m.author_id, c.members
		  FROM messages m
		  JOIN conversations c ON c.id = m.conversation_id
		 WHERE m.id = $1 AND m.company_id = $2`,
		event.MessageID, event.CompanyID).Scan(&authorID, &membersJSON)
	if err != nil {
		return
	}
	var members []string
	_ = json.Unmarshal(membersJSON, &members)
	if !s.IsAgent(ctx, authorID) {
		return
	}
	if event.ActorID != nil && *event.ActorID == authorID {
		return
	}

	isClose := event.Poll.ClosedAt != nil
	claimKey := "cumora:poll-vote-wake-claim:" + event.MessageID
	claimTTL := pollVoteWakeDebounceSec * time.Second
	if isClose {
		claimKey = "cumora:poll-close-wake-claim:" + event.MessageID
		claimTTL = pollCloseWakeClaimSec * time.Second
	}
	if rdb := s.RDB; rdb != nil {
		claimed, err := rdb.SetNX(ctx, claimKey, "1", claimTTL).Result()
		if err != nil || !claimed {
			return
		}
	}

	voterSet := map[string]struct{}{}
	var totalVotes int64
	for _, t := range event.Tallies {
		totalVotes += t.Count
		for _, v := range t.VoterIDs {
			voterSet[v] = struct{}{}
		}
	}
	var pendingIDs []string
	for _, m := range members {
		if m == authorID {
			continue
		}
		if _, voted := voterSet[m]; voted {
			continue
		}
		pendingIDs = append(pendingIDs, m)
	}
	idsToResolve := append(append([]string{}, pendingIDs...), event.TalliesVoterIDs()...)
	if event.ActorID != nil {
		idsToResolve = append(idsToResolve, *event.ActorID)
	}
	names := s.resolveParticipantNames(ctx, idsToResolve)

	tallies := make([]map[string]any, 0, len(event.Poll.Options))
	for _, opt := range event.Poll.Options {
		var count int64
		var voterIDs []string
		for _, t := range event.Tallies {
			if t.OptionID == opt.ID {
				count = t.Count
				voterIDs = t.VoterIDs
			}
		}
		voters := make([]map[string]any, 0, len(voterIDs))
		for _, id := range voterIDs {
			voters = append(voters, map[string]any{"id": id, "name": names[id]})
		}
		tallies = append(tallies, map[string]any{
			"optionId": opt.ID, "text": opt.Text, "count": count, "voters": voters,
		})
	}
	pending := make([]map[string]any, 0, len(pendingIDs))
	for _, id := range pendingIDs {
		pending = append(pending, map[string]any{"id": id, "name": names[id]})
	}
	actor := map[string]any{"id": nil, "name": nil}
	if event.ActorID != nil {
		actor = map[string]any{"id": *event.ActorID, "name": names[*event.ActorID]}
	}
	status, phase := "open", "vote"
	if isClose {
		status, phase = "closed", "close"
	}
	var closedReason, expiresAt any
	if event.Poll.ClosedReason != nil {
		closedReason = *event.Poll.ClosedReason
	}
	if event.Poll.ExpiresAt != nil {
		expiresAt = *event.Poll.ExpiresAt
	}
	brief := map[string]any{
		"messageId":      event.MessageID,
		"conversationId": event.ConversationID,
		"question":       event.Poll.Question,
		"mode":           event.Poll.Mode,
		"status":         status,
		"closedReason":   closedReason,
		"expiresAt":      expiresAt,
		"totalVotes":     totalVotes,
		"tallies":        tallies,
		"pending":        pending,
		"actor":          actor,
		"phase":          phase,
	}
	convo := event.ConversationID
	s.WakeOne(authorID, "poll.updated", &convo, nil, &WakeOpts{PollBrief: brief})
}

// TalliesVoterIDs: 简报名字解析收集的辅助(去重前的全量 voter id)。
func (e pollUpdatedEvent) TalliesVoterIDs() []string {
	var out []string
	for _, t := range e.Tallies {
		out = append(out, t.VoterIDs...)
	}
	return out
}

/* ───────── 看板 @ 提及唤醒(#82 原面,保持不变) ───────── */

// WakeMentionedAgents:卡创建/更新、评论、指派变化的 fire-and-forget 唤醒
// (对齐 router.ts wakeMentionedAgents)。过滤发起者自己,只留本租户的
// 在册 agent;逐个 goroutine 投递,单点失败只告警——看板请求路径绝不被
// 唤醒基建拖挂(TS void + catch 的对等物 + defer recover)。
func (s *S) WakeMentionedAgents(companyID string, mentions []string, actorID string) {
	if len(mentions) == 0 {
		return
	}
	targets := make([]string, 0, len(mentions))
	for _, id := range mentions {
		if id != actorID {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Warn("[boards] wake mentioned agents panicked", "recover", rec)
			}
		}()
		rows, err := s.DB.QueryContext(ctxBG, `
			SELECT id FROM participants
			 WHERE kind = 'agent'
			   AND company_id = $1
			   AND id = ANY($2::text[])
			   AND departed_at IS NULL`, companyID, targets)
		if err != nil {
			slog.Warn("[boards] wake lookup failed", "err", err)
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				slog.Warn("[boards] wake row scan failed", "err", err)
				rows.Close()
				return
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			// 迭代中途失败 → 列表被截断,按 TS 语义(reject → catch → warn)
			// 放弃本轮唤醒;消息在卡面上,下个事件仍会触发。
			slog.Warn("[boards] wake lookup rows failed", "err", err)
			rows.Close()
			return
		}
		rows.Close() // 投递前归还连接——publish 不占池
		for _, id := range ids {
			s.WakeAgent(id, "manual", nil)
		}
	}()
}
