// register_test —— #24 验收:human-audience 聊天体语域的注入矩阵与开关语义。
package daemon

import (
	"strings"
	"testing"
)

// golden:生成常量与 canonical txt 逐字节一致(改文案=改 txt+再生)。
func TestHumanRegisterGolden(t *testing.T) {
	golden := mustGolden(t, "human_register.txt")
	if tick(humanRegisterRaw) != golden {
		t.Fatal("humanRegisterRaw 与 testdata/human_register.txt 漂移(改 packages/prompt/human-register.txt + npm run prompt:gen)")
	}
}

// 注入矩阵:受众 × 开关 → 块在与不在。
func TestTurnDeltaRegisterMatrix(t *testing.T) {
	isolateHome(t)
	boolp := func(v bool) *bool { return &v }
	falseP := boolp(false)
	cases := []struct {
		name      string
		chatReg   *bool
		audience  bool
		wantBlock bool
	}{
		{"1:1 人类私聊·默认开", nil, true, true},
		{"人类频道·显式开", boolp(true), true, true},
		{"人类私聊·关", falseP, true, false},
		{"混合受众·开(不注入)", boolp(true), false, false},
	}
	rows := func(audience bool) []map[string]any {
		return []map[string]any{{
			"conversation_id": "cv-x",
			"author_name":     "Ann",
			"author_kind":     "human",
			"body":            "hello",
			"human_audience":  audience,
		}}
	}
	for _, c := range cases {
		adapter := &sessionAdapter{id: "claude"}
		r := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, AgentInfo{ID: "a1", Name: "M", Engine: strp("claude"), ChatRegister: c.chatReg}, adapter)
		got := r.turnDelta("poll", rows(c.audience))
		hasBlock := strings.Contains(got, "CHAT REGISTER")
		hasTag := strings.Contains(got, "[humans-only]")
		if hasBlock != c.wantBlock {
			t.Errorf("%s: 块注入=%v, want %v", c.name, hasBlock, c.wantBlock)
		}
		if c.audience && c.wantBlock && !hasTag {
			t.Errorf("%s: 受众行缺 [humans-only] 标", c.name)
		}
		if !c.audience && hasTag {
			t.Errorf("%s: 非人类受众行不得打标", c.name)
		}
	}
}

// chatRegisterOn 语义:nil/true=开,false=关。
func TestChatRegisterToggleSemantics(t *testing.T) {
	boolp := func(v bool) *bool { return &v }
	if !(AgentInfo{}).chatRegisterOn() {
		t.Fatal("nil 应回退为开(默认)")
	}
	if !(AgentInfo{ChatRegister: boolp(true)}).chatRegisterOn() {
		t.Fatal("true 应为开")
	}
	if (AgentInfo{ChatRegister: boolp(false)}).chatRegisterOn() {
		t.Fatal("false 应为关")
	}
}

// 开关变更要触发 runner 重建(下一轮生效的传播面)。
func TestConfigMatchesChatRegisterChange(t *testing.T) {
	boolp := func(v bool) *bool { return &v }
	off := AgentInfo{ID: "a1", Name: "M", Engine: strp("claude"), ChatRegister: boolp(false)}
	adapter := &sessionAdapter{id: "claude"}
	r := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, AgentInfo{ID: "a1", Name: "M", Engine: strp("claude")}, adapter)
	if r.ConfigMatches(off, "claude") {
		t.Fatal("开关变更必须判为不匹配(触发快照刷新)")
	}
	r2 := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, off, adapter)
	if !r2.ConfigMatches(off, "claude") {
		t.Fatal("开关一致应匹配")
	}
}
