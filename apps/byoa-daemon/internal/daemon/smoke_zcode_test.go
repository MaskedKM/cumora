// smoke_zcode_test —— #65 验收:真实 zcode 的引擎冒烟(环境变量门控,
// 默认跳过;宿主机跑——docker 工具容器里没有 zcode):
//
//	CUMORA_ZCODE_SMOKE=1 [CUMORA_ZCODE_BIN=<zcode.cjs 或 wrapper>] \
//	  ./smoke.test -test.run TestRealZcodeSmoke -test.v
//
// 对齐 TS 冒烟(CUMORA_ZCODE_SMOKE=1)的用例面:launcher 解析(真实桌面
// 安装)、-p --json 信封(sessionId/response/usage 折算)、--resume 跨轮
// 连续性(第二轮复述第一轮口令)、诚实模型归因。RunResult 不带轮文本
// (TS 同——daemon 只消费副作用),文本断言直调包内 spawnZcodeJson。
package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealZcodeSmoke(t *testing.T) {
	if os.Getenv("CUMORA_ZCODE_SMOKE") != "1" {
		t.Skip("real-CLI smoke: set CUMORA_ZCODE_SMOKE=1 (host machine with the ZCode desktop app or CUMORA_ZCODE_BIN)")
	}
	env := os.Environ()
	launcher := resolveZcodeLauncher(env)
	if launcher == nil {
		t.Fatal("zcode launcher did not resolve (CUMORA_ZCODE_BIN unset and no desktop install?)")
	}
	t.Logf("launcher: command=%s prefix=%v source=%s", launcher.command, launcher.prefix, launcher.source)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	home := t.TempDir()
	base := func(resume string, prompt string) []string {
		argv := []string{"--cwd", home, "--mode", "yolo", "--no-color", "--json"}
		if resume != "" {
			argv = append(argv, "--resume", resume)
		}
		return append(argv, "-p", stripLoneSurrogates(prompt))
	}

	// 轮 1:固定口令 → sessionId + 信封。
	marker := "ZCODE-SMOKE-OK"
	res1, text1 := spawnZcodeJson(ctx, launcher, base("", "Reply with exactly this token and nothing else: "+marker),
		home, env, func(l string) { t.Log("[zcode] " + l) }, nil)
	if res1.ExitCode != 0 || res1.Err != "" {
		t.Fatalf("smoke turn 1 failed: %+v", res1)
	}
	if res1.SessionID == "" {
		t.Fatal("no sessionId in envelope (resume chain broken)")
	}
	if res1.Usage == nil {
		t.Log("warning: envelope carried no usage")
	} else {
		t.Logf("usage: %+v", *res1.Usage)
	}
	if !strings.Contains(text1, marker) {
		t.Fatalf("turn 1 must echo the marker, got: %q", truncateRunes(text1, 200))
	}

	// 轮 2:--resume 连续性——复述第一轮口令。
	res2, text2 := spawnZcodeJson(ctx, launcher, base(res1.SessionID, "What exact token did I ask you to reply with in my previous message? Reply with just the token."),
		home, env, func(l string) { t.Log("[zcode] " + l) }, nil)
	if res2.ExitCode != 0 || res2.Err != "" {
		t.Fatalf("smoke turn 2 (resume continuity) failed: %+v", res2)
	}
	if !strings.Contains(text2, marker) {
		t.Fatalf("resume continuity broken — turn 2 never echoed %s; got: %q", marker, truncateRunes(text2, 200))
	}
	if res2.Model == "" || res2.Model == "zcode-unknown-model" {
		t.Logf("warning: model attribution fell back to %q (no ~/.zcode/cli/config.json pin?)", res2.Model)
	} else {
		t.Logf("model attribution: %s", res2.Model)
	}
}
