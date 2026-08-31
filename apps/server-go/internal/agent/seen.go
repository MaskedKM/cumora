// agent 包 seen —— 对齐 已退役 TS server 的 agents/seen-boundary.ts。
// 每 (agent, 会话) 的"已见 seq"边界(Redis 短 TTL):cumora reply 的新鲜度
// 预检靠它发现"compose 窗口期间同伴发了言"。为什么用 Redis 而非
// conversation_reads:last_read_at 同时是 loadInbox 的游标,动它会清空
// inbox(a6e69aa 事故);Redis 在事务图之外、Lua 原子单调、TTL 自清。
// 全部 fail-open:这是协调信号不是正确性不变量。
package agent

import (
	"log/slog"
	"regexp"
	"strconv"
	"time"
)

const seenTTLSeconds = 600 // 10 分钟——远超任何合理 compose 窗口

func seenKey(agentID, conversationID string) string {
	return "cumora:seen:" + agentID + ":" + conversationID
}

// monotonicSetScript:仅当新值 > 当前值(或键不存在)时 SET 并刷新 TTL,
// GET+SET 无竞态,并发记录不同 seq 恒收敛到更高值。
const monotonicSetScript = `
local cur = tonumber(redis.call('GET', KEYS[1])) or 0
local newv = tonumber(ARGV[1]) or 0
if newv > cur then
  redis.call('SET', KEYS[1], newv, 'EX', ARGV[2])
  return 1
end
return 0
`

// RecordSeen:记录本 agent 已被"展示"到(至少)某 seq。幂等/单调,热路径
// fire-and-forget,失败只记日志。
func (s *Service) RecordSeen(agentID, conversationID string, seq int64) {
	if agentID == "" || conversationID == "" || seq <= 0 {
		return
	}
	rdb := s.redis()
	if rdb == nil {
		return
	}
	err := rdb.Eval(ctxBG, monotonicSetScript, []string{seenKey(agentID, conversationID)},
		strconv.FormatInt(seq, 10), strconv.Itoa(seenTTLSeconds)).Err()
	if err != nil {
		slog.Warn("[seen-boundary] recordSeen failed — fail-open", "agent", agentID, "convo", conversationID, "err", err)
	}
}

// GetSeen:该 agent 在此会话已被展示到的最高 seq。未设/过期/Redis 错误
// 一律 0(FAIL-OPEN——预检跳过而非卡死)。
func (s *Service) GetSeen(agentID, conversationID string) int64 {
	if agentID == "" || conversationID == "" {
		return 0
	}
	rdb := s.redis()
	if rdb == nil {
		return 0
	}
	v, err := rdb.Get(ctxBG, seenKey(agentID, conversationID)).Result()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

/* ───────── compose anchor(轮始时间戳) ───────── */

func anchorKey(agentID, conversationID string) string {
	return "cumora:compose-anchor:" + agentID + ":" + conversationID
}

// RecordComposeAnchor:钉住本轮 compose 的起点时刻。每次轮始 OVERWRITE
// (与 recordSeen 的单调性不同);预检比较 messages.created_at > anchor,
// 因此 compose 期间同伴的发言必触发 HOLD,即使中途 glance 吸收了基线。
func (s *Service) RecordComposeAnchor(agentID, conversationID string, tsMS int64) {
	if agentID == "" || conversationID == "" || tsMS <= 0 {
		return
	}
	rdb := s.redis()
	if rdb == nil {
		return
	}
	if err := rdb.Set(ctxBG, anchorKey(agentID, conversationID),
		strconv.FormatInt(tsMS, 10), time.Duration(seenTTLSeconds)*time.Second).Err(); err != nil {
		slog.Warn("[seen-boundary] recordComposeAnchor failed — fail-open", "agent", agentID, "convo", conversationID, "err", err)
	}
}

// GetComposeAnchor:读 compose anchor(unix-ms);0 = 未设/过期/错误。
func (s *Service) GetComposeAnchor(agentID, conversationID string) int64 {
	if agentID == "" || conversationID == "" {
		return 0
	}
	rdb := s.redis()
	if rdb == nil {
		return 0
	}
	v, err := rdb.Get(ctxBG, anchorKey(agentID, conversationID)).Result()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// ClearComposeAnchor:成功发帖后清锚;失败靠 TTL 兜底。
func (s *Service) ClearComposeAnchor(agentID, conversationID string) {
	if agentID == "" || conversationID == "" {
		return
	}
	rdb := s.redis()
	if rdb == nil {
		return
	}
	_ = rdb.Del(ctxBG, anchorKey(agentID, conversationID)).Err()
}

/* ───────── hold token(HELD 确认门) ───────── */

const holdTTLSeconds = 120 // HELD 确认只在其同一口气内有效,长 TTL 会变弹药

func holdKey(agentID, scope string) string {
	return "cumora:held:" + agentID + ":" + scope
}

// consumeScript:GET+DEL 原子一步,两个竞争的 override 不可能都消费同一确认。
const consumeScript = `
local v = redis.call('GET', KEYS[1])
if v then
  redis.call('DEL', KEYS[1])
  return v
end
return false
`

var holdSeqRe = regexp.MustCompile(`^seq:(\d+)$`)

// HoldAcknowledgement:一枚被消费的 hold token 承认的内容。
type HoldAcknowledgement struct {
	// Armed:token 存在(或 Redis fail-open)——override 旗标已武装。
	Armed bool
	// HeldUpToSeq:HELD 信封展示给 agent 的最高同伴 seq(reply 预检路径);
	// nil = 无状态信息的武装(doc/calendar 标题域、legacy token、fail-open)。
	HeldUpToSeq *int64
}

// RecordHold:刚向该 agent 展示了 scope 的 HELD 信封。reply 域带
// heldUpToSeq 以便消费时校验新鲜度;fire-and-forget。
func (s *Service) RecordHold(agentID, scope string, heldUpToSeq *int64) {
	if agentID == "" || scope == "" {
		return
	}
	rdb := s.redis()
	if rdb == nil {
		return
	}
	value := "1"
	if heldUpToSeq != nil && *heldUpToSeq > 0 {
		value = "seq:" + strconv.FormatInt(*heldUpToSeq, 10)
	}
	if err := rdb.Set(ctxBG, holdKey(agentID, scope), value,
		time.Duration(holdTTLSeconds)*time.Second).Err(); err != nil {
		slog.Warn("[seen-boundary] recordHold failed — fail-open", "agent", agentID, "scope", scope, "err", err)
	}
}

// ConsumeHold:读取并删除 token。Redis 错误时 FAIL-OPEN 到武装态
// (降级为旧行为:旗标被尊重),绝不阻塞真实工作。
func (s *Service) ConsumeHold(agentID, scope string) HoldAcknowledgement {
	if agentID == "" || scope == "" {
		return HoldAcknowledgement{}
	}
	rdb := s.redis()
	if rdb == nil {
		return HoldAcknowledgement{}
	}
	r, err := rdb.Eval(ctxBG, consumeScript, []string{holdKey(agentID, scope)}).Result()
	if err != nil {
		slog.Warn("[seen-boundary] consumeHold failed — fail-open (honoring override)", "agent", agentID, "scope", scope, "err", err)
		return HoldAcknowledgement{Armed: true}
	}
	str, ok := r.(string)
	if !ok {
		if n, ok := r.(int64); ok {
			str = strconv.FormatInt(n, 10)
		} else {
			return HoldAcknowledgement{}
		}
	}
	m := holdSeqRe.FindStringSubmatch(str)
	if m != nil {
		if seq, err := strconv.ParseInt(m[1], 10, 64); err == nil && seq > 0 {
			return HoldAcknowledgement{Armed: true, HeldUpToSeq: &seq}
		}
	}
	return HoldAcknowledgement{Armed: true}
}

// ClearHold:无需 override 就成功提交后清掉残留 token——陈旧 token
// 不得武装后续的抢先绕过。TTL 覆盖泄漏场景。
func (s *Service) ClearHold(agentID, scope string) {
	if agentID == "" || scope == "" {
		return
	}
	rdb := s.redis()
	if rdb == nil {
		return
	}
	_ = rdb.Del(ctxBG, holdKey(agentID, scope)).Err()
}

// ConsumeTurnToken:对齐 scheduler.ts 的内容无关成本地板。60s 滚动
// 窗口内每 agent 最多 30 次激活;Redis 错误 fail-open(放行)。
func (s *Service) ConsumeTurnToken(agentID string) bool {
	rdb := s.redis()
	if rdb == nil {
		return true
	}
	key := "cumora:turn-rate:" + agentID
	n, err := rdb.Incr(ctxBG, key).Result()
	if err != nil {
		return true
	}
	if n == 1 {
		_ = rdb.Expire(ctxBG, key, 60*time.Second).Err()
	}
	return n <= 30
}
