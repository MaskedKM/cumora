// smoke_grokcursor_test —— #66 验收:grok/cursor 的真机冒烟(环境变量门控,
// 默认跳过——本机未装两引擎;在装有引擎的机器上跑):
//
//	CUMORA_GROK_SMOKE=1   ./smoke.test -test.run TestRealGrokSmoke   -test.v
//	CUMORA_CURSOR_SMOKE=1 ./smoke.test -test.run TestRealCursorSmoke -test.v
//
// grok:ACP 握手(session/new 带 rules)→ session/prompt 轮(usage/实跑模型/
// 连续性:resume 第二轮复述)。cursor:一次性 stream-json(init 模型+会话 id、
// result usage、--resume 连续性)。
package daemon

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestRealGrokSmoke(t *testing.T) {
	if os.Getenv("CUMORA_GROK_SMOKE") != "1" {
		t.Skip("real-CLI smoke: set CUMORA_GROK_SMOKE=1 (host machine with grok installed)")
	}
	env := os.Environ()
	if resolveGrokBin(env) == "" {
		t.Fatal("grok binary did not resolve (PATH or ~/.grok/bin)")
	}
	sess := grokAdapter{}.StartSession(SessionArgs{
		Home:           t.TempDir(),
		Env:            env,
		StandingPrompt: "You are a Cumora SMOKE agent.",
		OnLog:          func(l string) { t.Log("[grok] " + l) },
		OnHopUsage:     func(r HopReport) { t.Logf("[grok] hop model=%s usage=%+v", r.Model, r.Usage) },
	})
	if sess == nil {
		t.Fatal("grok StartSession returned nil (CUMORA_GROK_ARGS / NO_ACP / windows?)")
	}
	defer sess.Stop()
	if !sess.CarriesStandingPrompt() {
		t.Fatal("ACP _meta.rules must carry the standing prompt")
	}
	marker := "GROK-SMOKE-OK"
	res1 := sess.Send("Reply with exactly this token and nothing else: " + marker)
	if res1.ExitCode != 0 || res1.Err != "" {
		t.Fatalf("turn 1: %+v", res1)
	}
	if sess.SessionID() == "" {
		t.Fatal("no session id after turn 1")
	}
	res2 := sess.Send("What exact token did I ask you to reply with? Reply with just the token.")
	if res2.ExitCode != 0 || res2.Err != "" {
		t.Fatalf("turn 2 (continuity): %+v", res2)
	}
	t.Logf("turn2 model=%s sid=%s usage=%+v", res2.Model, res2.SessionID, res2.Usage)
}

func TestRealCursorSmoke(t *testing.T) {
	if os.Getenv("CUMORA_CURSOR_SMOKE") != "1" {
		t.Skip("real-CLI smoke: set CUMORA_CURSOR_SMOKE=1 (host machine with cursor-agent installed)")
	}
	if _, err := exec.LookPath("cursor-agent"); err != nil {
		t.Fatal("cursor-agent not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env := os.Environ()
	home := t.TempDir()
	marker := "CURSOR-SMOKE-OK"
	res1, _ := (cursorAdapter{}).turn(ctx, "Reply with exactly this token and nothing else: "+marker, home, env, func(l string) { t.Log("[cursor] " + l) }, "", "", nil)
	if res1.ExitCode != 0 || res1.Err != "" {
		t.Fatalf("turn 1: %+v", res1)
	}
	if res1.SessionID == "" {
		t.Fatal("no session id in stream (resume chain broken)")
	}
	t.Logf("turn1 model=%s usage=%+v", res1.Model, res1.Usage)
	res2, _ := (cursorAdapter{}).turn(ctx, "What exact token did I ask you to reply with? Reply with just the token.", home, env, func(l string) { t.Log("[cursor] " + l) }, "", res1.SessionID, nil)
	if res2.ExitCode != 0 || res2.Err != "" {
		t.Fatalf("turn 2 (resume continuity): %+v", res2)
	}
	t.Logf("turn2 model=%s sid=%s", res2.Model, res2.SessionID)
}
