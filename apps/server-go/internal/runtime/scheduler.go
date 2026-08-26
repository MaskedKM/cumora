// runtime 包 scheduler —— 唤醒面(#82):wakeAgent 的 Go 落地与
// wakeMentionedAgents(看板 @提及/指派唤醒)。完整 scheduler
// (CH_MESSAGE_NEW 消费、idle/background 合成唤醒、低优先级预算、
// steer 分支)属 #62;本面只覆盖 boards 域的 'manual' 唤醒——
// wakeMentionedAgents 全部 5 个调用点(reason='manual',无 steer 载荷)。
package runtime

import (
	"log/slog"
)

// WakeAgent:向 agent 的 daemon 推一枚 SSE 唤醒(对齐 scheduler.wakeAgent
// 的无 steer 分支)。payload 形状对齐 wake-bus:conversationId 键恒在
// (null 或串)。接收端数为 0 = daemon 离线/休眠——BYOA 无可拉起对象,
// 唤醒经 inbox 持久,重连排水兜底(只记日志)。
func (s *Service) WakeAgent(agentID, reason string, conversationID *string) {
	if s.Bus == nil {
		slog.Info("[scheduler] daemon offline — wake deferred to reconnect", "agent", agentID, "reason", reason)
		return
	}
	var convo any
	if conversationID != nil {
		convo = *conversationID
	}
	delivered, err := s.Bus.Deliver(agentID, map[string]any{
		"kind":           "wake",
		"reason":         reason,
		"conversationId": convo,
	})
	if err != nil {
		slog.Warn("[scheduler] wake deliver failed", "agent", agentID, "err", err)
		return
	}
	if delivered == 0 {
		slog.Info("[scheduler] daemon offline — wake deferred to reconnect", "agent", agentID, "reason", reason)
	}
}

// WakeMentionedAgents:卡创建/更新、评论、指派变化的 fire-and-forget 唤醒
// (对齐 router.ts wakeMentionedAgents)。过滤发起者自己,只留本租户的
// 在册 agent;逐个 goroutine 投递,单点失败只告警——看板请求路径绝不被
// 唤醒基建拖挂(TS void + catch 的对等物 + defer recover)。
func (s *Service) WakeMentionedAgents(companyID string, mentions []string, actorID string) {
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
