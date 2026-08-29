// agent 包 work —— worklog(反重复工作 claim;#140 自 runtime/presence
// 拆出,纯移动)。cli 面(doc/calendar 的"最近重复创建"检查、reply 的
// 工作让位)共用;Redis-only。
package agent

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

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
	// TS slice(0,80) 按 UTF-16 码元(长 CJK 主题不再过度相撞,#94)。
	s = utf16Slice(s, 80)
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
	if err != nil && err != redis.Nil {
		slog.Warn("[runtime] claimWork HGet failed — fail-open", "agent", agentID, "task", taskType, "err", err)
		return ClaimWorkResult{Accepted: true}
	}
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
