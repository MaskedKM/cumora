// runtime 包 cost —— 对齐 server/src/agents/cost.ts 的 token→成本核算。
// 缓存感知定价:triage(冷会话,全额输入)对照大脑 turn(命中缓存,
// ~0.1×)才是诚实比较。价格按 1M token 计;仅运营方经
// CUMORA_MODEL_PRICES_JSON 提供的费率算 verified,种子默认一律 estimated。
package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

// TokenUsage:一次模型调用的缓存感知分解(input 不含缓存读;cacheRead 是
// 廉价的读命中;cacheWrite 是写入溢价)。
type TokenUsage struct {
	InputTokens         int64 `json:"inputTokens"`
	CachedInputTokens   int64 `json:"cachedInputTokens"`
	CacheCreationTokens int64 `json:"cacheCreationTokens"`
	OutputTokens        int64 `json:"outputTokens"`
}

var emptyUsage = TokenUsage{}

// ModelPrice:1M token 单价。
type ModelPrice struct {
	InPer1M       float64
	CachedInPer1M float64
	CacheWritePer float64
	OutPer1M      float64
	Verified      bool
}

// seedPrices:种子价全部视为估计。claude 分层匹配子串:旧版 Opus 4.0/4.1
// ($15/$75)排在通用 claude-opus 前,具体版本优先;现行 Opus(4.5–4.8)
// $5/$25;Haiku 4.5 $1/$5;Sonnet 4.x $3/$15。
var seedPrices = []struct {
	id    string
	price ModelPrice
}{
	{"gpt-5.5", ModelPrice{2.5, 0.25, 2.5, 10, false}},
	{"gpt-5.4-mini", ModelPrice{0.25, 0.025, 0.25, 2, false}},
	{"claude-opus-4-1", ModelPrice{15, 1.5, 18.75, 75, false}},
	{"claude-opus", ModelPrice{5, 0.5, 6.25, 25, false}},
	{"claude-sonnet", ModelPrice{3, 0.3, 3.75, 15, false}},
	{"claude-haiku", ModelPrice{1, 0.1, 1.25, 5, false}},
}

// fallbackPrice:未识别模型的兜底,恒为估计。
var fallbackPrice = ModelPrice{3, 0.3, 3.75, 15, false}

var (
	envOverridesOnce sync.Once
	envOverrides     []struct {
		id    string
		price ModelPrice
	}
)

func loadEnvOverrides() {
	raw := strings.TrimSpace(os.Getenv("CUMORA_MODEL_PRICES_JSON"))
	if raw == "" {
		return
	}
	// TS 经 Object.entries 按文档序遍历覆盖(priceTable 渲染序、重叠键
	// 子串匹配的胜者都依赖它);Go map 迭代随机,须用解码器按出现序走。
	// JSON.parse 语义:整份文档要么全收要么全弃;重复键后者覆盖前值、
	// 位置保持首见。
	dec := json.NewDecoder(strings.NewReader(raw))
	open, err := dec.Token()
	if err != nil || open != json.Delim('{') {
		slog.Warn("[cost] CUMORA_MODEL_PRICES_JSON is not a JSON object — ignoring", "err", err)
		return
	}
	type override = struct {
		id    string
		price ModelPrice
	}
	num := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	bail := func(err error) {
		envOverrides = nil
		slog.Warn("[cost] CUMORA_MODEL_PRICES_JSON is not valid JSON — ignoring", "err", err)
	}
	seen := map[string]int{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			bail(err)
			return
		}
		key, _ := keyTok.(string)
		var p struct {
			InPer1M       *float64 `json:"inPer1M"`
			CachedInPer1M *float64 `json:"cachedInPer1M"`
			CacheWritePer *float64 `json:"cacheWritePer1M"`
			OutPer1M      *float64 `json:"outPer1M"`
		}
		if err := dec.Decode(&p); err != nil {
			bail(err)
			return
		}
		entry := override{key, ModelPrice{num(p.InPer1M), num(p.CachedInPer1M), num(p.CacheWritePer), num(p.OutPer1M), true}}
		if i, ok := seen[key]; ok {
			envOverrides[i] = entry
			continue
		}
		seen[key] = len(envOverrides)
		envOverrides = append(envOverrides, entry)
	}
	if _, err := dec.Token(); err != nil { // 闭合 '}' 读不出 = 坏文档
		bail(err)
		return
	}
	if _, err := dec.Token(); err != io.EOF { // JSON.parse 不容尾随垃圾
		if err == nil {
			err = fmt.Errorf("trailing data after JSON object")
		}
		bail(err)
		return
	}
}

// priceFor:env 覆盖(精确)→ 种子精确 → 种子族子串 → 兜底。
// 双向包含匹配:完整 id "claude-sonnet-4-6" 含种子键 "claude-sonnet",
// 裸分层 id "haiku" 被种子键 "claude-haiku" 包含——只查一个方向会错价。
func priceFor(model string) ModelPrice {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return fallbackPrice
	}
	envOverridesOnce.Do(loadEnvOverrides)
	matches := func(key string) bool {
		k := strings.ToLower(key)
		return id == k || strings.Contains(id, k) || strings.Contains(k, id)
	}
	for _, kv := range envOverrides {
		if kv.id == id {
			return kv.price
		}
	}
	for _, kv := range envOverrides {
		if matches(kv.id) {
			return kv.price
		}
	}
	for _, kv := range seedPrices {
		if kv.id == id {
			return kv.price
		}
	}
	for _, kv := range seedPrices {
		if matches(kv.id) {
			return kv.price
		}
	}
	return fallbackPrice
}

// EffectiveCostUsd:缓存感知有效成本(USD)。estimated=true 表示种子
// 猜测/兜底价而非运营方实价,UI 须标注,美元数字不当真账单。
func EffectiveCostUsd(model string, usage TokenUsage) (usd float64, estimated bool) {
	p := priceFor(model)
	usd = (float64(usage.InputTokens)*p.InPer1M +
		float64(usage.CachedInputTokens)*p.CachedInPer1M +
		float64(usage.CacheCreationTokens)*p.CacheWritePer +
		float64(usage.OutputTokens)*p.OutPer1M) / 1_000_000
	return usd, !p.Verified
}
