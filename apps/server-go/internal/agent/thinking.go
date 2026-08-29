// agent 包 thinking —— thinking-claim(同伴可见的"我在想";#140 自
// runtime/presence 拆出,纯移动)。routes 的 HTTP 面(标记/心跳)与
// cli 面(记忆写盖章轮次真实所在)双端共用。
package agent

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

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
