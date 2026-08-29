// sched 包 idle —— idle/heartbeat 调度(#62):idle.ts 的 Go 等价。
// 每租户挑一个安静 agent 合成唤醒;有 agenda 卡/事件时先问廉价分类器
// (remote 路由才问;byoa 路由由 daemon 自己的 /runtime/agenda 轮询负责,
// 本循环直接跳过该租户 tick)。分类器说 skip = 省脑;分类器故障 = 回落
// 通用 idle 唤醒(持续故障不许把带卡 agent 静默成 no-op)。
package sched

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/MaskedKM/cumora/apps/server-go/internal/agent"
)

// 间隔/阈值 env 用 envIntRaw:TS 语义 0=禁用(IDLE_INTERVAL_MS=0 关
// 调度器)、IDLE_MIN_QUIET_MIN=0 原样生效——envIntOr 的 0→默认会吞掉
// 这两个 kill-switch(PR #104 评审 MAJOR)。
func idleIntervalMS() int64 {
	if n, ok := envIntRaw("IDLE_INTERVAL_MS"); ok {
		return n
	}
	return 15 * 60_000
}

func idleMinQuietMin() int64 {
	if n, ok := envIntRaw("IDLE_MIN_QUIET_MIN"); ok {
		return n
	}
	return 25
}

type idleCandidate struct {
	id        string
	name      string
	status    string
	companyID string
	lastSpoke sql.NullString // PG ::text 原串(ref JSON 按原样携带)
}

// pickIdleAgent: 先随机挑 5 个 avail/resting agent,只对这 5 个算 last_spoke
// (旧形态对全量 agent 跑相关子查询 + 无索引全扫,曾 503 API)。安静 =
// 从未发言或最近 IDLE_MIN_QUIET_MIN 分钟内未发言。
func (s *S) pickIdleAgent(ctx context.Context, companyID string) *idleCandidate {
	rows, err := s.DB.QueryContext(ctx, `
		WITH picked AS (
		   SELECT p.id, p.name, p.status, p.company_id
		     FROM participants p
		    WHERE p.kind = 'agent'
		      AND p.departed_at IS NULL
		      AND p.company_id = $1
		      AND p.status IN ('avail', 'resting')
		    ORDER BY random()
		    LIMIT 5
		 )
		 SELECT picked.id, picked.name, picked.status, picked.company_id,
		        (SELECT MAX(m.created_at) FROM messages m WHERE m.author_id = picked.id)::text AS last_spoke
		   FROM picked`, companyID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	cutoff := time.Now().Add(-time.Duration(idleMinQuietMin()) * time.Minute)
	for rows.Next() {
		var c idleCandidate
		if err := rows.Scan(&c.id, &c.name, &c.status, &c.companyID, &c.lastSpoke); err != nil {
			continue
		}
		if !c.lastSpoke.Valid {
			return &c
		}
		// PG ::text 形态由 parseJSDate 家族消化(timestamptz 各漂移布局)。
		// 不可解析 = TS 的 NaN < cutoff(恒 false)→ 该候选不算安静,跳过。
		t, ok := agent.ParseJSDate(c.lastSpoke.String)
		if !ok || t.After(cutoff) {
			continue
		}
		return &c
	}
	return nil
}

// recordIdleWake: agent_log 落一行 note(company_id 让租户索引直取)。
// 上抛错误:TS 的 await INSERT 在 per-tenant try 内,失败即跳过唤醒。
func (s *S) recordIdleWake(agent idleCandidate, ref map[string]any) error {
	refJSON, _ := json.Marshal(ref)
	_, err := s.DB.ExecContext(ctxBG, `
		INSERT INTO agent_log (id, agent_id, company_id, kind, body, ref)
		VALUES ($1, $2, $3, 'note', $4, $5::jsonb)`,
		"log-"+RandHex12(), agent.id, agent.companyID,
		"idle wake queued for "+agent.name, string(refJSON))
	return err
}

func idleRef(agent idleCandidate, cards, events int, verdict string) map[string]any {
	var lastSpoke any
	if agent.lastSpoke.Valid {
		lastSpoke = agent.lastSpoke.String
	}
	return map[string]any{
		"source":        "idle_scheduler",
		"companyId":     agent.companyID,
		"status":        agent.status,
		"lastSpoke":     lastSpoke,
		"agendaCards":   cards,
		"agendaEvents":  events,
		"agendaVerdict": verdict,
	}
}

func idleMinQuietString() string { return strconv.FormatInt(idleMinQuietMin(), 10) }

// RunIdleTick: 一轮——遍历租户,挑人,agenda 判定,唤醒。
func (s *S) RunIdleTick(ctx context.Context) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM companies`)
	if err != nil {
		slog.Warn("[idle] tenant list failed", "err", err)
		return
	}
	var tenants []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			tenants = append(tenants, id)
		}
	}
	rows.Close()

	for _, companyID := range tenants {
		func(cid string) {
			defer func() {
				if rec := recover(); rec != nil {
					slog.Warn("[idle] tenant tick panicked", "company", cid, "recover", rec)
				}
			}()
			agent := s.pickIdleAgent(ctx, cid)
			if agent == nil {
				return
			}
			agenda, err := s.GatherAgentAgenda(ctx, agent.id, agent.companyID)
			if err != nil {
				slog.Warn("[idle] gather agenda failed", "agent", agent.id, "err", err)
				return
			}

			if len(agenda.Cards) == 0 && len(agenda.Events) == 0 {
				// 无卡无槽位事件——保留原 heartbeat 行为(自发动静)。
				if err := s.recordIdleWake(*agent, idleRef(*agent, 0, 0, "empty")); err != nil {
					slog.Warn("[idle] record wake failed", "agent", agent.id, "err", err)
					return
				}
				s.WakeOne(agent.id, "idle", nil, nil, &WakeOpts{
					IdleReason: "idle heartbeat after at least " + idleMinQuietString() + " quiet minute(s)",
				})
				return
			}

			// byoa 路由:agenda 分类跑在操作者自己机器上(daemon 的
			// /runtime/agenda 轮询)——本 tick 跳过,不得替它调 remote。
			if s.ResolveCerebellumRouteForAgent(ctx, agent.id) == "byoa" {
				return
			}
			persona, err := s.GetPersona(ctx, agent.id)
			if err != nil || persona == nil {
				return
			}
			verdict := s.ClassifyAgendaActionable(ctx, persona, agent.companyID, agent.id, agenda, time.Now().UnixMilli())

			if !verdict.Actionable {
				verdictLabel := "skip"
				if verdict.Reason == AgendaClassifierError {
					verdictLabel = "classifier_error"
				}
				if err := s.recordIdleWake(*agent, idleRef(*agent, len(agenda.Cards), len(agenda.Events), verdictLabel)); err != nil {
					slog.Warn("[idle] record wake failed", "agent", agent.id, "err", err)
					return
				}
				// 健康分类器说 skip = 省脑;分类器 ERROR(网络/配额)不得
				// 静默 agent——回落通用 idle 唤醒。
				if verdictLabel == "classifier_error" {
					s.WakeOne(agent.id, "idle", nil, nil, &WakeOpts{
						IdleReason: "idle heartbeat after at least " + idleMinQuietString() + " quiet minute(s) (agenda triage unavailable)",
					})
				}
				return
			}

			// agenda 驱动唤醒:把聚焦简报直接交给大脑,执行而非决策。
			ref := idleRef(*agent, len(agenda.Cards), len(agenda.Events), "actionable")
			if verdict.Focus != "" {
				ref["agendaFocus"] = verdict.Focus
			}
			if err := s.recordIdleWake(*agent, ref); err != nil {
				slog.Warn("[idle] record wake failed", "agent", agent.id, "err", err)
				return
			}
			focus := verdict.Focus
			if focus == "" {
				focus = "Heartbeat agenda"
			}
			s.WakeOne(agent.id, "background_scan", nil, nil, &WakeOpts{
				BackgroundBrief: &BackgroundBrief{
					Source: "agenda_scheduler",
					Title:  focus,
					Body:   RenderAgendaBrief(agenda, verdict.Focus),
				},
			})
		}(companyID)
	}
}

func RandHex12() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartIdleScheduler: 周期 tick;ENABLE_IDLE='false' 或 IDLE_INTERVAL_MS<=0
// 关闭(nil = 未启动)。TS 门控是字面 !== 'false'。
func (s *S) StartIdleScheduler() (stop func()) {
	if getenv("ENABLE_IDLE") == "false" {
		return nil
	}
	interval := idleIntervalMS()
	if interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	go func() {
		for range ticker.C {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						slog.Error("[idle] tick panicked", "recover", rec)
					}
				}()
				s.RunIdleTick(ctxBG)
			}()
		}
	}()
	slog.Info("[boot] idle scheduler running",
		"interval_ms", interval, "min_quiet_min", idleMinQuietMin())
	return ticker.Stop
}
