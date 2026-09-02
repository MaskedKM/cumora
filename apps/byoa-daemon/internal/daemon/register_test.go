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

// 混合受众多行唤醒(评审 #325 P2-2):块出现,且只有 human 行打标——
// 过度泛化由 [humans-only] 行标收窄(块内文案明确指向标记行)。
func TestTurnDeltaMixedWakeOnlyHumanRowsTagged(t *testing.T) {
	isolateHome(t)
	adapter := &sessionAdapter{id: "claude"}
	r := newAgentRunner(&DaemonConfig{ServerURL: "http://x"}, AgentInfo{ID: "a1", Name: "M", Engine: strp("claude")}, adapter)
	rows := []map[string]any{
		{"conversation_id": "cv-dm", "author_name": "Ann", "author_kind": "human", "body": "hi", "human_audience": true},
		{"conversation_id": "cv-grp", "author_name": "Bot", "author_kind": "agent", "body": "chatter", "human_audience": false},
	}
	got := r.turnDelta("sse-wake", rows)
	if !strings.Contains(got, "CHAT REGISTER") {
		t.Fatal("混合唤醒含 human 行 → 块必须在")
	}
	if !strings.Contains(got, "- [cv-dm] [humans-only]") {
		t.Fatal("human 行缺 [humans-only] 标")
	}
	if strings.Contains(got, "- [cv-grp] [humans-only]") {
		t.Fatal("非 human 行不得打标")
	}
}

// steer 措辞(评审 #325 P2-2,票面接缝二):human-audience 的直 ping
// steer 携带语域指令;非 human 受众不携带。
func TestMaybeSteerRegisterClause(t *testing.T) {
	isolateHome(t)
	t.Setenv("CUMORA_BYOA_STEER_GROUP_INTERVAL_MS", "60000")
	stub, cfg := newSessionTestStack(t, nil)
	sess := &fakeEngineSession{alive: true}
	adapter := &sessionAdapter{id: "claude", session: sess}
	r := newAgentRunner(cfg, AgentInfo{ID: "a1", Name: "S", Engine: strp("claude")}, adapter)
	r.mu.Lock()
	r.engineSession = sess
	r.busy = true
	r.mu.Unlock()
	t.Cleanup(func() { r.Stop(); r.wg.Wait() })

	steers := func() []string {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return append([]string{}, sess.steers...)
	}
	// human-audience 直 ping → steer 文本含语域指令。
	stub.mu.Lock()
	stub.inboxRows = []map[string]any{{"id": "m1", "conversation_id": "cv-dm", "conversation_kind": "direct", "author_kind": "human", "author_name": "Ann", "body": "ping", "human_audience": true}}
	stub.mu.Unlock()
	r.maybeSteer("cv-dm")
	if s := steers(); len(s) != 1 || !strings.Contains(s[0], "chat register") {
		t.Fatalf("human-audience steer 缺语域指令: %v", s)
	}
	// 非 human 受众(agent 直 ping)→ 不携带。
	stub.mu.Lock()
	stub.inboxRows = []map[string]any{{"id": "m2", "conversation_id": "cv-aa", "conversation_kind": "direct", "author_kind": "agent", "author_name": "Bot", "body": "yo", "human_audience": false}}
	stub.mu.Unlock()
	r.maybeSteer("cv-aa")
	all := steers()
	if len(all) != 2 || strings.Contains(all[1], "chat register") {
		t.Fatalf("非 human steer 不应携带语域指令: %v", all)
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
