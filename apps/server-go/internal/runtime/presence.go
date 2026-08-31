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
	"strconv"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/events"
	"github.com/MaskedKM/cumora/apps/server-go/internal/httpx"
)

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
		events.PublishRaw(ctx, events.ChStatus, mustJSON(map[string]any{
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
	events.PublishRaw(ctx, events.ChStatus, mustJSON(map[string]any{
		"type":            "participants.status",
		"participantId":   participantID,
		"status":          status,
		"statusUpdatedAt": httpx.ISOms(at),
		"companyId":       companyID,
	}))
	return nil
}

// ResetHumanPresenceOnBoot:对齐 TS resetHumanPresenceOnBoot —— 启动时
// 把上次运行残留的 'avail' 人类降为 'resting' 并逐租户广播。半开连接
// (笔记本休眠/网断)的 close 永远不会来,不降就会一直挂"在线";
// agent 不动(自有租约 + GET /participants 自动过期)。
func (s *Service) ResetHumanPresenceOnBoot(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `
		UPDATE participants
		   SET status = 'resting', status_updated_at = NOW()
		 WHERE kind = 'human' AND status = 'avail'
		 RETURNING id, company_id, status_updated_at`)
	if err != nil {
		slog.Warn("[runtime] resetHumanPresenceOnBoot failed", "err", err)
		return
	}
	defer rows.Close()
	demoted := 0
	for rows.Next() {
		var id, companyID string
		var at time.Time
		if err := rows.Scan(&id, &companyID, &at); err != nil {
			continue
		}
		demoted++
		// 每条发布独立 5s 上限:TS 曾因逐条 publish 无界拖住 server.listen
		// 上线(Promise.race 兜底);关键面(UPDATE)已先行提交,这里只是
		// 通知面,不该有能力拖住启动。
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		events.PublishRaw(pctx, events.ChStatus, mustJSON(map[string]any{
			"type":            "participants.status",
			"participantId":   id,
			"status":          "resting",
			"statusUpdatedAt": httpx.ISOms(at),
			"companyId":       companyID,
		}))
		pcancel()
	}
	if demoted > 0 {
		slog.Info("[runtime] demoted stale 'avail' human(s) to 'resting' on boot", "count", demoted)
	}
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
