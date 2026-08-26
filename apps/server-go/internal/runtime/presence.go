// runtime 包 presence —— 对齐 status.ts + inproc-client.ts 的在场面:
// 状态胶囊(participants.status + CH_STATUS 广播)、typing 节流广播、
// steering busy 租约、thinking-claim(ZSET)、worklog 反重复claim(HASH)、
// thinking-convos 反向索引。
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
	"github.com/redis/go-redis/v9"
)

const chStatus = "cumora:status"

// SetStatus:更新 participants.status 并按租户逐行广播(status.ts)。
// 一个 participant id 在多个租户可能有多行(human 跨租户共享 id)——
// 全部更新、每租户一条广播。
func (s *Service) SetStatus(ctx context.Context, participantID, status string) error {
	rows, err := s.DB.QueryContext(ctx, `
		UPDATE participants
		   SET status = $2, status_updated_at = NOW()
		 WHERE id = $1
		 RETURNING company_id, status_updated_at`, participantID, status)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var companyID string
		var at time.Time
		if err := rows.Scan(&companyID, &at); err != nil {
			return err
		}
		events.PublishRaw(ctx, chStatus, mustJSON(map[string]any{
			"type":            "participants.status",
			"participantId":   participantID,
			"status":          status,
			"statusUpdatedAt": httpx.ISOms(at),
			"companyId":       companyID,
		}))
	}
	return rows.Err()
}

// HeartbeatStatus:不改语义状态、只续忙碌租约(status 匹配才 bump)。
func (s *Service) HeartbeatStatus(ctx context.Context, participantID, status string) error {
	var companyID string
	var at time.Time
	err := s.DB.QueryRowContext(ctx, `
		UPDATE participants
		   SET status_updated_at = NOW()
		 WHERE id = $1 AND status = $2
		 RETURNING company_id, status_updated_at`, participantID, status).Scan(&companyID, &at)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	events.PublishRaw(ctx, chStatus, mustJSON(map[string]any{
		"type":            "participants.status",
		"participantId":   participantID,
		"status":          status,
		"statusUpdatedAt": httpx.ISOms(at),
		"companyId":       companyID,
	}))
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// PublishTyping:发 "typing in convo X" 指示。keep-alive 节流:渲染侧
// 45s 可见、daemon 每 6s 重报 → 每 (agent, convo) 30s 内至多 1 条非 done
// 广播(扇出 ~5x 削减);done(停止)恒放行以即时清除。
func (s *Service) PublishTyping(ctx context.Context, conversationID, agentID string, done bool, companyID *string) {
	rdb := s.redis()
	if !done && rdb != nil {
		fresh, err := rdb.SetNX(ctx, "cumora:typ:"+agentID+":"+conversationID, "1", 30*time.Second).Result()
		if err == nil && !fresh {
			return // 节流命中——上一条 ping 的指示仍活着
		}
		// Redis 打嗝:落到直接广播
	}
	payload := map[string]any{
		"type":           "typing",
		"conversationId": conversationID,
		"agentId":        agentID,
		"done":           done,
	}
	if companyID != nil {
		payload["companyId"] = *companyID
	}
	if err := events.PublishRaw(ctx, events.ChTyping, mustJSON(payload)); err != nil {
		slog.Warn("[runtime] publishTyping failed — dropping", "err", err)
	}
}

/* ───────── steering busy 租约 ───────── */

func busyKey(agentID string) string { return "cumora:busy:" + agentID }

// RecordBusyHeartbeat:覆盖式 SET+TTL(值为时间戳便于排障)。失败容忍。
func (s *Service) RecordBusyHeartbeat(agentID string, ttlSec int) {
	rdb := s.redis()
	if rdb == nil {
		return
	}
	if err := rdb.Set(ctxBG, busyKey(agentID), strconv.FormatInt(time.Now().UnixMilli(), 10),
		time.Duration(ttlSec)*time.Second).Err(); err != nil {
		slog.Warn("[runtime] recordBusyHeartbeat failed — dropping", "agent", agentID, "err", err)
	}
}

// ClearBusyHeartbeat:干净结束提前释放;失败靠 TTL。
func (s *Service) ClearBusyHeartbeat(agentID string) {
	rdb := s.redis()
	if rdb == nil {
		return
	}
	if err := rdb.Del(ctxBG, busyKey(agentID)).Err(); err != nil {
		slog.Warn("[runtime] clearBusyHeartbeat failed — dropping", "agent", agentID, "err", err)
	}
}

// IsAgentBusy:消息路由用——busy → deliverSteer(轮中注入),空闲 →
// 常规 wake。Redis 不可达 fail-open 为"不忙"(退回 pre-steer 行为)。
func (s *Service) IsAgentBusy(agentID string) bool {
	rdb := s.redis()
	if rdb == nil {
		return false
	}
	v, err := rdb.Get(ctxBG, busyKey(agentID)).Result()
	return err == nil && v != ""
}

/* ───────── thinking-claim(同伴可见的"我在想") ───────── */

func thinkingKey(conversationID string) string { return "cumora:thinking:" + conversationID }

func agentThinkingConvosKey(agentID string) string {
	return "cumora:agent-thinking-convos:" + agentID
}

// MarkThinking:每会话一个 ZSET,分数=首次 claim 的 ms 时间戳,TTL ttlSec。
// ZADD NX 保住首次时刻——中途刷新不改变排队位。刷新节流:claim TTL 60s、
// daemon 每 6s 重报 → 每 agent 30s 至多 1 次(仍远早于 TTL 到期)。
func (s *Service) MarkThinking(agentID string, conversationIDs []string, ttlSec int) {
	if len(conversationIDs) == 0 {
		return
	}
	s.recordThinkingConversations(agentID, conversationIDs, ttlSec)
	rdb := s.redis()
	if rdb == nil {
		return
	}
	fresh, err := rdb.SetNX(ctxBG, "cumora:thk:"+agentID, "1", 30*time.Second).Result()
	if err == nil && !fresh {
		return // 节流命中——既有 claim 仍在 TTL 内有效
	}
	pipe := rdb.Pipeline()
	now := float64(time.Now().UnixMilli())
	for _, cid := range conversationIDs {
		pipe.ZAddNX(ctxBG, thinkingKey(cid), redis.Z{Score: now, Member: agentID})
		pipe.Expire(ctxBG, thinkingKey(cid), time.Duration(ttlSec)*time.Second)
	}
	if _, err := pipe.Exec(ctxBG); err != nil {
		slog.Warn("[runtime] markThinking failed — fail-open", "agent", agentID, "err", err)
	}
}

// UnmarkThinking:摘除 claim 并清反向索引;轮结束同时作废未用的 HELD
// 确认(reply:<cid> 域的 hold token)——放它活到后续轮会武装一次 3.5 分钟
// 后的抢先 --send-anyway(2026-07-08 数数游戏陈旧"6"重复)。
func (s *Service) UnmarkThinking(agentID string, conversationIDs []string) {
	if len(conversationIDs) == 0 {
		return
	}
	s.clearThinkingConversations(agentID)
	rdb := s.redis()
	if rdb != nil {
		pipe := rdb.Pipeline()
		for _, cid := range conversationIDs {
			pipe.ZRem(ctxBG, thinkingKey(cid), agentID)
		}
		if _, err := pipe.Exec(ctxBG); err != nil {
			slog.Warn("[runtime] unmarkThinking failed — TTL will clean", "agent", agentID, "err", err)
		}
	}
	for _, cid := range conversationIDs {
		s.ClearHold(agentID, "reply:"+cid)
	}
}

// PeekThinking:该会话正在构思的 agent,按先来后到排序(分数升序)。
func (s *Service) PeekThinking(conversationID string) []map[string]any {
	rdb := s.redis()
	if rdb == nil {
		return []map[string]any{}
	}
	raw, err := rdb.ZRangeWithScores(ctxBG, thinkingKey(conversationID), 0, -1).Result()
	if err != nil {
		slog.Warn("[runtime] peekThinking failed", "convo", conversationID, "err", err)
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, z := range raw {
		out = append(out, map[string]any{
			"agentId":   z.Member,
			"claimedAt": int64(z.Score),
		})
	}
	return out
}

// recordThinkingConversations:反向索引("该 agent 正在哪些会话想"),
// 供记忆写盖章轮次真实所在,免得每次都要 --in。fail-open。
func (s *Service) recordThinkingConversations(agentID string, conversationIDs []string, ttlSec int) {
	rdb := s.redis()
	if rdb == nil {
		return
	}
	body, _ := json.Marshal(conversationIDs)
	ttl := ttlSec + 30
	if ttl < 90 {
		ttl = 90
	}
	_ = rdb.Set(ctxBG, agentThinkingConvosKey(agentID), body, time.Duration(ttl)*time.Second).Err()
}

func (s *Service) clearThinkingConversations(agentID string) {
	rdb := s.redis()
	if rdb == nil {
		return
	}
	_ = rdb.Del(ctxBG, agentThinkingConvosKey(agentID)).Err()
}

/* ───────── worklog(反重复工作 claim) ───────── */

// WorklogEntry:一条在途工作 claim(agentId/任务类型/原始主题/起始 ms)。
type WorklogEntry struct {
	AgentID   string `json:"agentId"`
	TaskType  string `json:"taskType"`
	Subject   string `json:"subject"`
	StartedAt int64  `json:"startedAt"`
}

var workSubjectSpace = regexp.MustCompile(`\s+`)

// NormalizeWorkSubject:主题归一(小写/空白折叠/去尾/截 80),让
// "Warm Pastels  " 与 "warm pastels" 相撞。worklog 字段 id 与"最近重复
// 创建"检查(doc/calendar)共用,保证"同主题"只有一种含义。
func NormalizeWorkSubject(subject string) string {
	s := strings.ToLower(subject)
	s = workSubjectSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	// TS slice(0,80) 按 UTF-16 码元;此处按字节截断即可(80 字节 ≤ 80 码元,
	// 越界差异只可能让两个超长主题不撞,方向保守)。
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func worklogKey(scopeKey string) string { return "cumora:worklog:" + scopeKey }
func worklogField(taskType, subject string) string {
	return taskType + "::" + NormalizeWorkSubject(subject)
}

func parseWorklogEntry(raw string) *WorklogEntry {
	var e WorklogEntry
	if json.Unmarshal([]byte(raw), &e) != nil {
		return nil
	}
	if e.AgentID == "" || e.TaskType == "" || e.Subject == "" || e.StartedAt == 0 {
		return nil
	}
	return &e
}

// ClaimWorkResult:claim 结果——accepted=true 或持有者详情(调用方应让位)。
type ClaimWorkResult struct {
	Accepted bool
	Existing *WorklogEntry
}

// ClaimWork:HSETNX 原子闸门。过期条目(超 ttl 未刷新)逐出后重试;
// 竞态落败给出"<unknown>"占位让调用方仍能体面让位;Redis 故障
// fail-open(最坏 = 两个 agent 重复做一次,与无此机制时相同)。
func (s *Service) ClaimWork(scopeKey, agentID, taskType, subject string, ttlSec int) ClaimWorkResult {
	if ttlSec <= 0 {
		ttlSec = 300
	}
	rdb := s.redis()
	if rdb == nil {
		return ClaimWorkResult{Accepted: true}
	}
	key := worklogKey(scopeKey)
	field := worklogField(taskType, subject)
	now := time.Now().UnixMilli()
	entry, _ := json.Marshal(WorklogEntry{AgentID: agentID, TaskType: taskType, Subject: subject, StartedAt: now})
	synthetic := &WorklogEntry{AgentID: "<unknown>", TaskType: taskType, Subject: subject, StartedAt: now}

	written, err := rdb.HSetNX(ctxBG, key, field, string(entry)).Result()
	if err != nil {
		slog.Warn("[runtime] claimWork failed — fail-open", "agent", agentID, "task", taskType, "err", err)
		return ClaimWorkResult{Accepted: true}
	}
	if written {
		_ = rdb.Expire(ctxBG, key, time.Duration(ttlSec)*time.Second).Err()
		return ClaimWorkResult{Accepted: true}
	}
	raw, err := rdb.HGet(ctxBG, key, field).Result()
	if err != nil || raw == "" {
		// HSETNX 与 HGET 之间被人 release:再试一次。
		retry, _ := rdb.HSetNX(ctxBG, key, field, string(entry)).Result()
		if retry {
			_ = rdb.Expire(ctxBG, key, time.Duration(ttlSec)*time.Second).Err()
			return ClaimWorkResult{Accepted: true}
		}
		return ClaimWorkResult{Existing: synthetic}
	}
	existing := parseWorklogEntry(raw)
	if existing == nil {
		// 坏值——删字段,让下一个调用者成功;本次仍让位。
		_ = rdb.HDel(ctxBG, key, field).Err()
		return ClaimWorkResult{Existing: synthetic}
	}
	if now-existing.StartedAt > int64(ttlSec)*1000 {
		// 陈旧 claim——逐出并接管。
		_ = rdb.HDel(ctxBG, key, field).Err()
		retake, _ := rdb.HSetNX(ctxBG, key, field, string(entry)).Result()
		if retake {
			_ = rdb.Expire(ctxBG, key, time.Duration(ttlSec)*time.Second).Err()
			return ClaimWorkResult{Accepted: true}
		}
		if refreshed, err := rdb.HGet(ctxBG, key, field).Result(); err == nil {
			if re := parseWorklogEntry(refreshed); re != nil {
				return ClaimWorkResult{Existing: re}
			}
		}
		return ClaimWorkResult{Existing: existing}
	}
	return ClaimWorkResult{Existing: existing}
}

// ReleaseWork:仅当自己是持有者才删(别人的 claim 不被我们的 release 抹掉)。
func (s *Service) ReleaseWork(scopeKey, agentID, taskType, subject string) {
	rdb := s.redis()
	if rdb == nil {
		return
	}
	key, field := worklogKey(scopeKey), worklogField(taskType, subject)
	raw, err := rdb.HGet(ctxBG, key, field).Result()
	if err != nil || raw == "" {
		return
	}
	if e := parseWorklogEntry(raw); e != nil && e.AgentID == agentID {
		_ = rdb.HDel(ctxBG, key, field).Err()
	}
}

// PeekWorklog:该 scope 全部在途 claim;读时滤掉超 5 分钟的(写侧 TTL
// 的防御性镜像),按起手时间稳定排序(先做的人排前面)。
func (s *Service) PeekWorklog(scopeKey string) []WorklogEntry {
	rdb := s.redis()
	if rdb == nil {
		return []WorklogEntry{}
	}
	raw, err := rdb.HGetAll(ctxBG, worklogKey(scopeKey)).Result()
	if err != nil {
		slog.Warn("[runtime] peekWorklog failed", "scope", scopeKey, "err", err)
		return []WorklogEntry{}
	}
	now := time.Now().UnixMilli()
	out := make([]WorklogEntry, 0, len(raw))
	for _, v := range raw {
		e := parseWorklogEntry(v)
		if e == nil || now-e.StartedAt > 5*60*1000 {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out
}
