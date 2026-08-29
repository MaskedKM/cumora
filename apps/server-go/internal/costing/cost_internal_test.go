// cost 环境价覆盖单测(#108 R2/F12):CUMORA_MODEL_PRICES_JSON 的文档序
// 语义。TS 侧 Object.entries 按文档序遍历——priceTable 渲染序与重叠键
// 子串匹配的胜者都依赖它;JSON.parse 全有或全无、重复键后值覆盖且位置
// 保持首见。纯逻辑,无 DB/Redis。
package costing

import (
	"sync"
	"testing"
)

func forceReloadEnvOverrides() {
	envOverrides = nil
	envOverridesOnce = sync.Once{}
	envOverridesOnce.Do(loadEnvOverrides)
}

func reloadEnvPrices(t *testing.T, raw string) {
	t.Helper()
	// 先注册 cleanup(LIFO 后执行):届时 t.Setenv 已还原原值,重载回净态。
	t.Cleanup(forceReloadEnvOverrides)
	t.Setenv("CUMORA_MODEL_PRICES_JSON", raw)
	forceReloadEnvOverrides()
}

func TestEnvOverridesDocumentOrder(t *testing.T) {
	reloadEnvPrices(t, `{"zeta-model":{"inPer1M":1,"outPer1M":2},"alpha-model":{"inPer1M":3,"outPer1M":4}}`)
	if len(envOverrides) != 2 || envOverrides[0].id != "zeta-model" || envOverrides[1].id != "alpha-model" {
		t.Fatalf("expected document order [zeta-model, alpha-model], got %+v", envOverrides)
	}
}

func TestEnvOverridesSubstringFirstInDocumentOrderWins(t *testing.T) {
	// "zeta-family-x" 同时子串命中两键;文档序在前的 zeta-family 必须胜出
	// (map 随机迭代会翻转胜者)。
	reloadEnvPrices(t, `{"zeta-family":{"inPer1M":1},"zeta":{"inPer1M":2}}`)
	if got := priceFor("zeta-family-x"); got.InPer1M != 1 {
		t.Fatalf("first-in-document-order key must win, got %+v", got)
	}
	// 反转文档序则胜者随之反转。
	reloadEnvPrices(t, `{"zeta":{"inPer1M":2},"zeta-family":{"inPer1M":1}}`)
	if got := priceFor("zeta-family-x"); got.InPer1M != 2 {
		t.Fatalf("reversed document order must flip winner, got %+v", got)
	}
}

func TestEnvOverridesDuplicateKeyLastValueFirstPosition(t *testing.T) {
	reloadEnvPrices(t, `{"dup":{"inPer1M":1},"x":{"inPer1M":9},"dup":{"inPer1M":5}}`)
	if len(envOverrides) != 2 {
		t.Fatalf("duplicate key must collapse to one entry, got %+v", envOverrides)
	}
	if envOverrides[0].id != "dup" || envOverrides[0].price.InPer1M != 5 || envOverrides[1].id != "x" {
		t.Fatalf("dup key: last value at first-seen position, got %+v", envOverrides)
	}
}

func TestEnvOverridesInvalidJsonAllOrNothing(t *testing.T) {
	cases := []string{
		`{"a":{"inPer1M":1},invalid}`,
		`{"a":{"inPer1M":1}} trailing`,
		`[]`,
		`{"a":{"inPer1M":1`,
	}
	for _, raw := range cases {
		reloadEnvPrices(t, raw)
		if len(envOverrides) != 0 {
			t.Fatalf("invalid JSON %q must yield zero overrides, got %+v", raw, envOverrides)
		}
		if got := priceFor("a"); got.Verified {
			t.Fatalf("invalid JSON must not mark prices verified, got %+v", got)
		}
	}
}
