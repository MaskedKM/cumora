// runtime 包 observability —— 对齐 server/src/agents/observability.ts 的
// 写面(runs/events/triage/touch)+ llm-ledger.ts 的 recordLlmCall。
// 台账是观测面不是调用路径的硬依赖:一切插入尽力而为,失败只记日志。
package runtime

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// uuidHex:crypto/rand 16 字节 → 8-4-4-4-12(与 TS randomUUID 同形)。
func uuidHex() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const (
	maxStringChars = 24_000
	maxJSONChars   = 160_000
)

// clipJSON:递归裁剪任意 JSON 值(string 截 24k、数组 50、对象 80 键、深 8),
// 再序列化;超 160k 落 {truncated:true,preview}。对齐 observability.ts clip。
func clipValue(v any, depth int) any {
	switch t := v.(type) {
	case string:
		if len(t) > maxStringChars {
			return t[:maxStringChars] + "…"
		}
		return t
	case nil, bool, float64, int, int64:
		return v
	case []any:
		if depth >= 8 {
			return "[truncated]"
		}
		if len(t) > 50 {
			t = t[:50]
		}
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = clipValue(item, depth+1)
		}
		return out
	case map[string]any:
		if depth >= 8 {
			return "[truncated]"
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortStrings(keys)
		if len(keys) > 80 {
			keys = keys[:80]
		}
		out := make(map[string]any, len(keys))
		for _, k := range keys {
			out[k] = clipValue(t[k], depth+1)
		}
		return out
	default:
		return v
	}
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func jsonForDB(v any) string {
	b, err := json.Marshal(clipValue(v, 0))
	if err == nil && len(b) <= maxJSONChars {
		return string(b)
	}
	if err == nil {
		preview := string(b)
		if len(preview) > maxJSONChars {
			preview = preview[:maxJSONChars]
		}
		out, merr := json.Marshal(map[string]any{"truncated": true, "preview": preview})
		if merr == nil {
			return string(out)
		}
	}
	return fmt.Sprintf(`{"value":%q}`, fmt.Sprintf("%v", v))
}

// CreateAgentRun:开一行 agent_runs,返回 runId。
func (s *Service) CreateAgentRun(ctx context.Context, args struct {
	AgentID         string
	CompanyID       *string
	Trigger         map[string]any
	InputMessageIDs []string
	InboxCount      int64
	Fingerprint     *string
}) (string, error) {
	id := "run-" + uuidHex()
	inboxCount := args.InboxCount
	var trigger any = args.Trigger
	if trigger == nil {
		trigger = map[string]any{}
	}
	var inputIDs any = args.InputMessageIDs
	if inputIDs == nil {
		inputIDs = []any{}
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO agent_runs (
		  id, agent_id, company_id, trigger, status, stage,
		  input_message_ids, inbox_count, fingerprint
		)
		VALUES ($1,$2,$3,$4::jsonb,'running','created',$5::jsonb,$6,$7)`,
		id, args.AgentID, args.CompanyID, jsonForDB(trigger), jsonForDB(inputIDs), inboxCount, args.Fingerprint)
	if err != nil {
		return "", err
	}
	return id, nil
}

// RecordAgentEvent:追加 agent_events 并顺带推进 run 的 stage。
func (s *Service) RecordAgentEvent(ctx context.Context, args struct {
	RunID     string
	AgentID   string
	CompanyID *string
	Kind      string
	Level     string
	Title     string
	Data      map[string]any
	Stage     *string
}) error {
	if args.Level == "" {
		args.Level = "info"
	}
	var data any = args.Data
	if data == nil {
		data = map[string]any{}
	}
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO agent_events (id, run_id, agent_id, company_id, kind, level, title, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`,
		"evt-"+uuidHex(), args.RunID, args.AgentID, args.CompanyID, args.Kind, args.Level, args.Title, jsonForDB(data))
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `
		UPDATE agent_runs SET updated_at = NOW(), stage = COALESCE($2, stage) WHERE id = $1`,
		args.RunID, args.Stage)
	return err
}

// FinishAgentRun:收尾 run(status/汇总/计数/usage 分解+成本)。usage 为
// nil 时仅写遗留字段(COALESCE 保住成本列默认)。tokenCount 优先于
// usage 求和(遗留 token_count = input+output 合,保后向兼容)。
func (s *Service) FinishAgentRun(ctx context.Context, runID, status string, summary, errMsg *string,
	toolCallCount, tokenCount *int64, usage *TokenUsage, model *string) error {
	var tokenCountV int64
	var cost *float64
	var costEstimated *bool
	if tokenCount != nil {
		tokenCountV = *tokenCount
	} else if usage != nil {
		tokenCountV = usage.InputTokens + usage.CachedInputTokens + usage.CacheCreationTokens + usage.OutputTokens
	}
	if usage != nil {
		usd, estimated := EffectiveCostUsd(deref(model), *usage)
		cost = &usd
		costEstimated = &estimated
	}
	var tools int64
	if toolCallCount != nil {
		tools = *toolCallCount
	}
	var in, cached, cacheW, out any
	if usage != nil {
		in, cached, cacheW, out = usage.InputTokens, usage.CachedInputTokens, usage.CacheCreationTokens, usage.OutputTokens
	}
	_, err := s.DB.ExecContext(ctx, `
		UPDATE agent_runs
		   SET status = $2,
		       stage = $2,
		       summary = $3,
		       error = $4,
		       tool_call_count = $5,
		       token_count = $6,
		       input_tokens          = COALESCE($7, input_tokens),
		       cached_input_tokens   = COALESCE($8, cached_input_tokens),
		       cache_creation_tokens = COALESCE($9, cache_creation_tokens),
		       output_tokens         = COALESCE($10, output_tokens),
		       cost_usd              = COALESCE($11, cost_usd),
		       cost_estimated        = COALESCE($12, cost_estimated),
		       model                 = COALESCE($13, model),
		       updated_at = NOW(),
		       finished_at = NOW()
		 WHERE id = $1`,
		runID, status, summary, errMsg, tools, tokenCountV, in, cached, cacheW, out, cost, costEstimated, model)
	return err
}

// TouchAgentRun:长引擎轮的心跳——只 bump updated_at 且只碰 running 行,
// 10 分钟陈旧清扫不会误收长轮,也不会复活已完结的行。
func (s *Service) TouchAgentRun(ctx context.Context, runID string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE agent_runs SET updated_at = NOW() WHERE id = $1 AND status = 'running'`, runID)
	return err
}

// RecordTriage:记一笔 inbox-triage(小脑门)及其缓存感知成本。尽力而为。
func (s *Service) RecordTriage(agentID string, companyID *string, source, model *string,
	actionable bool, reason *string, usage *TokenUsage) {
	measured := usage != nil
	u := emptyUsage
	if usage != nil {
		u = *usage
	}
	usd, estimated := EffectiveCostUsd(deref(model), u)
	var cost float64
	if measured {
		cost = usd
	}
	var reasonStr any
	if reason != nil {
		r := *reason
		if len(r) > 500 {
			r = r[:500]
		}
		reasonStr = r
	}
	if _, err := s.DB.ExecContext(ctxBG, `
		INSERT INTO agent_triages (
		  id, agent_id, company_id, source, model, actionable, reason,
		  input_tokens, cached_input_tokens, cache_creation_tokens, output_tokens,
		  cost_usd, cost_estimated, measured
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		"tri-"+uuidHex(), agentID, companyID, source, model, actionable, reasonStr,
		u.InputTokens, u.CachedInputTokens, u.CacheCreationTokens, u.OutputTokens,
		cost, estimated, measured); err != nil {
		slog.Warn("[observability] recordTriage failed — dropping", "err", err)
	}
}

// LlmCallRecord:一次 LLM 调用的台账行(对齐 llm-ledger.ts LlmCallRecord)。
type LlmCallRecord struct {
	Purpose        string
	CompanyID      *string
	AgentID        *string
	RunID          *string
	ConversationID *string
	Source         string
	Model          string
	Usage          *TokenUsage
	LatencyMS      int64
	Status         string
	Error          *string
	Extras         map[string]any
	DaemonVersion  *string
}

// knownPurposes:daemon 可申报的 purpose 白名单,其余一律 coerce 成
// agent-turn——未来 daemon 版本的新名字不得走私自由串进 rollup。
var knownPurposes = map[string]bool{
	"agent-turn": true, "inbox-triage": true, "synthetic-wake-gate": true, "agenda": true,
	"compaction": true, "completion-verify": true, "steer-summary": true,
}

// RecordLlmCall:llm_calls 单行 INSERT。永不抛错——台账不是调用路径的
// 硬依赖。BYOA 本地引擎调用也经 /runtime/llm-calls 走到这里(镜像进
// 通用台账,agent_triages 保留判定侧视图)。
func (s *Service) RecordLlmCall(rec LlmCallRecord) {
	measured := rec.Usage != nil
	u := emptyUsage
	if rec.Usage != nil {
		u = *rec.Usage
	}
	usd, estimated := EffectiveCostUsd(rec.Model, u)
	var cost float64
	if measured {
		cost = usd
	}
	if rec.Source == "" {
		rec.Source = "cloud"
	}
	if rec.Status == "" {
		rec.Status = "ok"
	}
	var errStr any
	if rec.Error != nil {
		e := *rec.Error
		if len(e) > 500 {
			e = e[:500]
		}
		errStr = e
	}
	var extras any
	if rec.Extras != nil {
		extras = jsonForDB(rec.Extras)
	}
	if _, err := s.DB.ExecContext(ctxBG, `
		INSERT INTO llm_calls (
		  id, company_id, agent_id, run_id, conversation_id,
		  purpose, source, model,
		  input_tokens, cached_input_tokens, cache_creation_tokens,
		  output_tokens, reasoning_tokens,
		  cost_usd, cost_estimated, measured,
		  latency_ms, status, error, extras, daemon_version
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,0,$13,$14,$15,$16,$17,$18,$19::jsonb,$20)`,
		"llm-"+uuidHex(), rec.CompanyID, rec.AgentID, rec.RunID, rec.ConversationID,
		rec.Purpose, rec.Source, rec.Model,
		u.InputTokens, u.CachedInputTokens, u.CacheCreationTokens, u.OutputTokens,
		cost, estimated, measured,
		rec.LatencyMS, rec.Status, errStr, extras, rec.DaemonVersion); err != nil {
		slog.Warn("[llm-ledger] insert failed — dropping", "err", err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// errorText:对齐 observability.ts errorText(带栈);Go 侧退化为错误串。
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// isRateLimitedText:错误串是否为限流/配额/过载信号(基础设施错误分类,
// 不是内容分类)。决定 triage 失败时 FAIL-CLOSED 而非 fail-open。
var rateLimitMarkers = []string{
	"429", "503", "too many requests", "rate limit", "rate.limit", "ratelimit",
	"quota", "resource_exhausted", "usage limit", "session limit", "overloaded",
	"insufficient_quota", "service unavailable", "service temporarily unavailable",
}

func isRateLimitedText(text string) bool {
	l := strings.ToLower(text)
	for _, m := range rateLimitMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}
