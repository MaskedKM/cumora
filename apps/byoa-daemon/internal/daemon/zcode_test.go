// zcode_test —— #65 验收:zcode 适配器的协议级测试(假 launcher/假 CLI,
// 不碰真实 zcode;真机冒烟 env 门控见 smoke_zcode_test.go)。对齐 TS 的
// agents-computer-engine-zcode.test.ts 的用例面。
package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ───────── launcher 三级解析 ───────── */

func TestZcodeLauncherResolution(t *testing.T) {
	root := t.TempDir()
	// ① CUMORA_ZCODE_BIN=.cjs → node + prefix。
	l := resolveZcodeLauncher([]string{"CUMORA_ZCODE_BIN=" + root + "/zcode.cjs", "PATH="})
	if l == nil || len(l.prefix) != 1 || !strings.HasSuffix(l.prefix[0], "zcode.cjs") || l.source != "CUMORA_ZCODE_BIN" {
		t.Fatalf("cjs override: %+v", l)
	}
	if !strings.HasSuffix(l.command, "node") {
		t.Fatalf("cjs must run under node: %q", l.command)
	}
	// ② wrapper(非 .cjs)→ 直接执行。
	l = resolveZcodeLauncher([]string{"CUMORA_ZCODE_BIN=" + root + "/zcode-wrapper", "PATH="})
	if l == nil || l.command != root+"/zcode-wrapper" || len(l.prefix) != 0 {
		t.Fatalf("wrapper override: %+v", l)
	}
	// ③ PATH 上的 zcode-cli(POSIX)。
	bin := filepath.Join(root, "bin")
	_ = os.MkdirAll(bin, 0o755)
	_ = os.WriteFile(filepath.Join(bin, "zcode-cli"), []byte("#!/bin/sh\n"), 0o755)
	l = resolveZcodeLauncher([]string{"PATH=" + bin})
	if l == nil || l.command != filepath.Join(bin, "zcode-cli") || l.source != "PATH:zcode-cli" {
		t.Fatalf("zcode-cli on PATH: %+v", l)
	}
	// 都没有 → nil。
	if resolveZcodeLauncher([]string{"PATH=" + root + "/empty"}) != nil {
		t.Fatal("nothing resolvable must yield nil")
	}
}

func TestZcodeLauncherAppImageExtraction(t *testing.T) {
	home := t.TempDir()
	// 假 AppImage:按协议在 cwd 造出 squashfs-root/resources/glm/zcode.cjs。
	appimage := filepath.Join(home, "ZCode.AppImage")
	payload := "// fake zcode.cjs payload\n"
	writeScript(t, appimage, "#!/bin/sh\nmkdir -p \"$PWD/squashfs-root/resources/glm\"\nprintf '%s' '"+payload+"' > \"$PWD/squashfs-root/resources/glm/zcode.cjs\"\necho run >> \""+home+"/extract-count\"\n")
	applications := filepath.Join(home, ".local", "share", "applications")
	_ = os.MkdirAll(applications, 0o755)
	_ = os.WriteFile(filepath.Join(applications, "zcode.desktop"), []byte("[Desktop Entry]\nExec=\""+appimage+"\" %U\n"), 0o644)
	env := []string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "PATH="}

	l := resolveZcodeLauncher(env)
	if l == nil || l.source != "appimage:"+appimage {
		t.Fatalf("appimage launcher: %+v", l)
	}
	cjs := l.prefix[0]
	b, err := os.ReadFile(cjs)
	if err != nil || string(b) != payload {
		t.Fatalf("extracted payload: %q err=%v", string(b), err)
	}
	if got := readObsAt(t, home, "extract-count"); strings.Count(got, "run") != 1 {
		t.Fatalf("expected exactly one extraction, got %q", got)
	}
	// 二次解析命中缓存:不再抽取。
	_ = resolveZcodeLauncher(env)
	if got := readObsAt(t, home, "extract-count"); strings.Count(got, "run") != 1 {
		t.Fatalf("cache hit must not re-extract, got %q", got)
	}
	// .mount_ 路径(运行中的挂载点)不作稳定指针—— planted 一个**功能
	// 完好**的假 AppImage 在真实存在的 .mount_ 路径(不存在的路径 os.Stat
	// 先失败,删掉 .mount_ 守卫测试照样绿=空洞;评审突变证实)。
	mountDir := filepath.Join(home, "tmp", ".mount_zcode123")
	_ = os.MkdirAll(mountDir, 0o755)
	mountApp := filepath.Join(mountDir, "AppRun")
	writeScript(t, mountApp, "#!/bin/sh\nmkdir -p \"$PWD/squashfs-root/resources/glm\"\nprintf 'x' > \"$PWD/squashfs-root/resources/glm/zcode.cjs\"\n")
	_ = os.WriteFile(filepath.Join(applications, "zcode.desktop"), []byte("[Desktop Entry]\nExec=\""+mountApp+"\" %U\n"), 0o644)
	if resolveZcodeLauncher(env) != nil {
		t.Fatal(".mount_ exec must not resolve")
	}
}

// M4:AppImage 升级(size+mtime 变)→ 新缓存键 → 重抽;旧缓存不误命中。
func TestZcodeAppImageUpgradeReextracts(t *testing.T) {
	home := t.TempDir()
	appimage := filepath.Join(home, "ZCode.AppImage")
	writeScript(t, appimage, "#!/bin/sh\nmkdir -p \"$PWD/squashfs-root/resources/glm\"\nprintf 'v1' > \"$PWD/squashfs-root/resources/glm/zcode.cjs\"\necho run >> \""+home+"/extract-count\"\n")
	applications := filepath.Join(home, ".local", "share", "applications")
	_ = os.MkdirAll(applications, 0o755)
	_ = os.WriteFile(filepath.Join(applications, "zcode.desktop"), []byte("[Desktop Entry]\nExec=\""+appimage+"\"\n"), 0o644)
	env := []string{"HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, ".cache"), "PATH="}

	l1 := resolveZcodeLauncher(env)
	if l1 == nil {
		t.Fatal("first resolve failed")
	}
	if b, _ := os.ReadFile(l1.prefix[0]); string(b) != "v1" {
		t.Fatalf("payload v1: %q", string(b))
	}
	// "升级":改内容并 bump mtime → 新缓存键。
	time.Sleep(10 * time.Millisecond)
	writeScript(t, appimage, "#!/bin/sh\nmkdir -p \"$PWD/squashfs-root/resources/glm\"\nprintf 'v2' > \"$PWD/squashfs-root/resources/glm/zcode.cjs\"\necho run >> \""+home+"/extract-count\"\n")
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(appimage, future, future)
	l2 := resolveZcodeLauncher(env)
	if l2 == nil || l2.prefix[0] == l1.prefix[0] {
		t.Fatalf("upgrade must re-key the cache: %v vs %v", l1.prefix, l2.prefix)
	}
	if b, _ := os.ReadFile(l2.prefix[0]); string(b) != "v2" {
		t.Fatalf("payload v2: %q", string(b))
	}
	if got := readObsAt(t, home, "extract-count"); strings.Count(got, "run") != 2 {
		t.Fatalf("expected two extractions, got %q", got)
	}
}

// M4:非信封输出经 Run 全程回退原文(spawnZcodeJson 的 raw-text 路径)。
func TestZcodeRunRawTextFallback(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
printf 'not json at all\n'
`)
	var logs []string
	res := (zcodeAdapter{}).Run(context.Background(), RunArgs{
		Home: t.TempDir(), Prompt: "p", Env: os.Environ(), OnLog: func(l string) { logs = append(logs, l) },
	})
	if res.ExitCode != 0 {
		t.Fatalf("raw text with exit 0 is not an error: %+v", res)
	}
	if res.SessionID != "" || res.Usage != nil {
		t.Fatalf("no envelope → no session/usage: %+v", res)
	}
}

// M4:畸形项目配置 → 归因落穿到用户级(不炸、不静默丢)。
func TestZcodeAttributionMalformedProjectConfig(t *testing.T) {
	userHome := fakeZcodeUserConfig(t, map[string]string{"kimi": "https://kimi"})
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".zcode"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".zcode", "config.json"), []byte(`{broken json`), 0o600)
	if got := readZcodeMainModel(append(os.Environ(), "HOME="+userHome), home); got != "bigmodel/glm-5.1" {
		t.Fatalf("malformed project config must fall through to the user pin: %q", got)
	}
}

// M4:CLI 漂移提示(Unknown option → 可行动诊断)。
func TestZcodeDriftHint(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
printf 'Error: Unknown option --no-color\n' >&2
exit 64
`)
	res := (zcodeAdapter{}).Run(context.Background(), RunArgs{Home: t.TempDir(), Prompt: "p", Env: os.Environ(), OnLog: func(string) {}})
	if res.ExitCode == 0 {
		t.Fatal("unknown option must fail")
	}
	if !strings.Contains(res.Err, "Unknown option") || !strings.Contains(res.Err, "CLI drift") || !strings.Contains(res.Err, "CUMORA_ZCODE_BIN") {
		t.Fatalf("drift hint: %q", res.Err)
	}
}

// M1:退化信封(键存在但值空/类型错)仍是信封。
func TestZcodeDegenerateEnvelopes(t *testing.T) {
	if env, _ := parseZcodeEnvelope(`{"response":"","sessionId":""}`); env == nil {
		t.Fatal("empty-valued keys still form the envelope (TS 'in' semantics)")
	}
	if env, _ := parseZcodeEnvelope(`{"sessionId":123,"usage":"str"}`); env == nil {
		t.Fatal("wrong-typed fields still form the envelope (lenient decode)")
	}
}

func readObsAt(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

/* ───────── 信封解析与用量折算 ───────── */

func TestZcodeParseEnvelopeAndUsage(t *testing.T) {
	env, text := parseZcodeEnvelope("  \x1b[32m{\"sessionId\":\"s-1\",\"response\":\"hello\"}\x1b[0m  ")
	if env == nil || env.SessionID != "s-1" || text == "" {
		t.Fatalf("envelope: %+v text=%q", env, text)
	}
	// usage 折算:input 含 cacheRead → 非缓存部分;reasoning 并入 output。
	env, _ = parseZcodeEnvelope(`{"sessionId":"s-2","response":"r","usage":{"inputTokens":21295,"outputTokens":900,"cacheReadTokens":17600,"cacheWriteTokens":50,"reasoningTokens":120}}`)
	u := zcodeUsageToEngineUsage(env)
	if u == nil || *u.InputTokens != 21295-17600 || *u.OutputTokens != 1020 || *u.CacheReadInputTokens != 17600 || *u.CacheCreationInputTokens != 50 {
		t.Fatalf("usage mapping: %+v", u)
	}
	// 非信封原文透传。
	if env, text := parseZcodeEnvelope("plain text not json"); env != nil || text != "plain text not json" {
		t.Fatalf("non-envelope: %+v %q", env, text)
	}
	if env, _ := parseZcodeEnvelope(`{"unrelated":true}`); env != nil {
		t.Fatalf("json without response/sessionId is not the envelope: %+v", env)
	}
	if env, _ := parseZcodeEnvelope(`{"response":"only-response"}`); env == nil {
		t.Fatal("response-only is the envelope")
	}
}

/* ───────── Run:一次性 + 陈旧 resume 自愈 ───────── */

func fakeZcodeBin(t *testing.T, body string) string {
	t.Helper()
	bin, _ := fakeEngineDir(t)
	wrapper := filepath.Join(bin, "zcode-fake")
	writeScript(t, wrapper, body)
	t.Setenv("CUMORA_ZCODE_BIN", wrapper)
	return bin
}

func TestZcodeRunOneShot(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
echo "$@" > "$FAKE_T/zc-argv.txt"
printf '%s\n' '{"sessionId":"sess-z-1","response":"ZCODE-REPLY","usage":{"inputTokens":100,"outputTokens":40,"cacheReadTokens":60,"cacheWriteTokens":5}}'
`)
	var hops []HopReport
	res := (zcodeAdapter{}).Run(context.Background(), RunArgs{
		Home:            t.TempDir(),
		Prompt:          "do the thing",
		Env:             os.Environ(),
		ResumeSessionID: "sess-prev",
		OnLog:           func(string) {},
		OnHopUsage:      func(r HopReport) { hops = append(hops, r) },
	})
	if res.ExitCode != 0 || res.Err != "" {
		t.Fatalf("run: %+v", res)
	}
	argv := readObs(t, "zc-argv.txt")
	for _, want := range []string{"--cwd", "--mode", "yolo", "--no-color", "--json", "--resume", "sess-prev", "-p", "do the thing"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv missing %q: %s", want, argv)
		}
	}
	if res.SessionID != "sess-z-1" || res.Usage == nil || *res.Usage.InputTokens != 40 {
		t.Fatalf("envelope fold: %+v", res)
	}
	if len(hops) != 1 || hops[0].HopIndex != 1 || hops[0].Model == "" {
		t.Fatalf("hop: %+v", hops)
	}
}

func TestZcodeRunHonestModelAttribution(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
printf '%s\n' '{"sessionId":"s","response":"r","usage":{"inputTokens":10,"outputTokens":1}}'
`)
	// 项目级配置优先:home/.zcode/config.json 的 model.main。
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".zcode"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".zcode", "config.json"), []byte(`{"model":{"main":"kimi/k3"}}`), 0o600)
	var hops []HopReport
	res := (zcodeAdapter{}).Run(context.Background(), RunArgs{
		Home: home, Prompt: "p", Env: os.Environ(),
		OnHopUsage: func(r HopReport) { hops = append(hops, r) },
	})
	if res.ExitCode != 0 || res.Model != "kimi/k3" || hops[0].Model != "kimi/k3" {
		t.Fatalf("project-layer attribution: res=%+v hops=%+v", res, hops)
	}
	// 两层皆缺 → 可 grep 占位,不静默丢跳。
	res2 := (zcodeAdapter{}).Run(context.Background(), RunArgs{
		Home: t.TempDir(), Prompt: "p", Env: append(os.Environ(), "HOME="+t.TempDir()),
		OnHopUsage: func(r HopReport) { hops = append(hops, r) },
	})
	if res2.Model != "zcode-unknown-model" {
		t.Fatalf("fallback attribution: %+v", res2)
	}
}

func TestZcodeRunStaleResumeSelfHeals(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "sess-gone" ]; then
    printf '%s\n' 'Error: Session not found' >&2
    exit 1
  fi
done
printf '%s\n' '{"sessionId":"sess-fresh","response":"recovered"}'
`)
	var logs []string
	res := (zcodeAdapter{}).Run(context.Background(), RunArgs{
		Home: t.TempDir(), Prompt: "again", Env: os.Environ(),
		ResumeSessionID: "sess-gone",
		OnLog:           func(l string) { logs = append(logs, l) },
	})
	if res.ExitCode != 0 || res.SessionID != "sess-fresh" {
		t.Fatalf("self-heal: %+v", res)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "sess-gone not found — starting a fresh session") {
		t.Fatalf("self-heal must log; logs:\n%s", joined)
	}
}

func TestZcodeRunArgsOverrideKeepsResume(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
echo "$@" > "$FAKE_T/zc-override-argv.txt"
printf 'raw non-json output\n'
`)
	t.Setenv("CUMORA_ZCODE_ARGS", "--my-flag --other")
	res := (zcodeAdapter{}).Run(context.Background(), RunArgs{
		Home: t.TempDir(), Prompt: "override me", Env: os.Environ(),
		ResumeSessionID: "sess-x", OnLog: func(string) {},
	})
	argv := readObs(t, "zc-override-argv.txt")
	for _, want := range []string{"--my-flag", "--other", "--resume", "sess-x", "-p", "override me"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("override argv missing %q: %s", want, argv)
		}
	}
	if strings.Contains(argv, "--json") {
		t.Fatalf("user flag override must not assume the envelope: %s", argv)
	}
	_ = res // 一次性通用路径:无信封折算(嗅探键 session_id 不匹配驼峰)。
}

/* ───────── classify / probe ───────── */

func TestZcodeClassifyPlanMode(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
echo "$@" > "$FAKE_T/zc-classify-argv.txt"
printf '%s\n' '{"sessionId":"s","response":"VERDICT","usage":{"inputTokens":10,"outputTokens":2}}'
`)
	res := (zcodeAdapter{}).Classify(context.Background(), ClassifyArgs{
		Cwd: t.TempDir(), Prompt: "triage this", Env: os.Environ(), OnLog: func(string) {},
	})
	if res.Err != "" || res.Text != "VERDICT" {
		t.Fatalf("classify: %+v", res)
	}
	argv := readObs(t, "zc-classify-argv.txt")
	for _, want := range []string{"--mode", "plan", "--no-color", "--json", "--disallowed-tools", "Bash Edit Write", "-p", "triage this"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("classify argv missing %q: %s", want, argv)
		}
	}
	if res.Usage == nil || *res.Usage.InputTokens != 10 {
		t.Fatalf("classify usage: %+v", res.Usage)
	}
}

func TestZcodeClassifyTriageFlagsOverride(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
echo "$@" > "$FAKE_T/zc-tri-argv.txt"
printf 'plain reply\n'
`)
	t.Setenv("CUMORA_TRIAGE_ARGS", "--custom-triage")
	res := (zcodeAdapter{}).Classify(context.Background(), ClassifyArgs{
		Cwd: t.TempDir(), Prompt: "p", Env: os.Environ(),
	})
	if res.Text != "plain reply" || res.Err != "" {
		t.Fatalf("triage override: %+v", res)
	}
	argv := readObs(t, "zc-tri-argv.txt")
	if !strings.Contains(argv, "--custom-triage") || strings.Contains(argv, "--mode") {
		t.Fatalf("triage override argv: %s", argv)
	}
}

func TestZcodeProbeAndWakeProbe(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
printf '%s\n' '{"sessionId":"s","response":"OK"}'
`)
	res := (zcodeAdapter{}).Probe(context.Background(), ProbeArgs{Tier: "small", Cwd: t.TempDir(), Env: os.Environ()})
	if res.Text != "OK" || res.Err != "" {
		t.Fatalf("probe: %+v", res)
	}
	pw := (zcodeAdapter{}).ProbeWake(context.Background(), WakeProbeArgs{})
	if !pw.OK || !pw.Skipped {
		t.Fatalf("probeWake: %+v", pw)
	}
}

/* ───────── seedHome + 项目级模型配置 ───────── */

func fakeZcodeUserConfig(t *testing.T, providers map[string]string) string {
	t.Helper()
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".zcode", "cli"), 0o755)
	prov := map[string]any{}
	for id, baseURL := range providers {
		prov[id] = map[string]any{"options": map[string]any{"apiKey": "k-" + id, "baseURL": baseURL}}
	}
	b, _ := json.Marshal(map[string]any{
		"model":    map[string]any{"main": "bigmodel/glm-5.1"},
		"provider": prov,
	})
	_ = os.WriteFile(filepath.Join(home, ".zcode", "cli", "config.json"), b, 0o600)
	t.Setenv("HOME", home)
	return home
}

func TestZcodeSeedHomeGoldenAndModelConfig(t *testing.T) {
	userHome := fakeZcodeUserConfig(t, map[string]string{"kimi": "https://kimi", "bigmodel": "https://bm"})
	home := t.TempDir()
	p := Persona{ID: "a1", Name: "Atlas", Role: strp("Tester"), SystemPrompt: strp("Be terse."),
		Model: strp("kimi/k3"), FastModel: strp("bigmodel/glm-4.7")}
	if err := (zcodeAdapter{}).SeedHome(home, p); err != nil {
		t.Fatal(err)
	}
	// AGENTS.md golden(opts:AGENTS.md + skills/)。
	b, _ := os.ReadFile(filepath.Join(home, "AGENTS.md"))
	gotAgents := string(b)
	goldenUpdate("persona_zcode.txt", gotAgents)
	if want := mustGolden(t, "persona_zcode.txt"); gotAgents != want {
		t.Fatalf("AGENTS.md drift (%d vs %d bytes)", len(gotAgents), len(want))
	}
	if !pathExists(filepath.Join(home, "skills")) {
		t.Fatal("skills/ dir missing")
	}
	// 项目级配置:main 钉 kimi/k3,lite 钉 bigmodel/glm-4.7,两 provider 条目
	// 均复制(含 apiKey;表不跨层合并)。
	proj := filepath.Join(home, ".zcode", "config.json")
	raw, err := os.ReadFile(proj)
	if err != nil {
		t.Fatalf("project config not written: %v", err)
	}
	var parsed struct {
		Model struct {
			Main string  `json:"main"`
			Lite *string `json:"lite"`
		} `json:"model"`
		Provider map[string]struct {
			Options struct {
				APIKey  string `json:"apiKey"`
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		t.Fatalf("project config not json: %s", raw)
	}
	if parsed.Model.Main != "kimi/k3" || parsed.Model.Lite == nil || *parsed.Model.Lite != "bigmodel/glm-4.7" {
		t.Fatalf("model pins: %+v", parsed.Model)
	}
	if parsed.Provider["kimi"].Options.APIKey != "k-kimi" || parsed.Provider["bigmodel"].Options.BaseURL != "https://bm" {
		t.Fatalf("provider entries not copied verbatim: %+v", parsed.Provider)
	}
	fi, _ := os.Stat(proj)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("project config must be 0600 (carries provider apiKey): %v", fi.Mode())
	}
	// 双层归因:项目层胜过用户层(bigmodel/glm-5.1)。
	if got := readZcodeMainModel(append(os.Environ(), "HOME="+userHome), home); got != "kimi/k3" {
		t.Fatalf("two-layer attribution: %q", got)
	}
}

func TestZcodeModelConfigDegradations(t *testing.T) {
	fakeZcodeUserConfig(t, map[string]string{"kimi": "https://kimi"})
	// 坏形态(无 provider 前缀)→ 不写、清陈旧。
	home := t.TempDir()
	if err := writeZcodeModelConfig(home, Persona{Model: strp("just-a-model")}); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(home, ".zcode", "config.json")) {
		t.Fatal("malformed model must not pin")
	}
	// provider 缺失 → 不写。
	if err := writeZcodeModelConfig(home, Persona{Model: strp("nope/m1")}); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(home, ".zcode", "config.json")) {
		t.Fatal("unknown provider must not pin")
	}
	// fast 的 provider 缺失 → 丢 lite,主钉保留。
	if err := writeZcodeModelConfig(home, Persona{Model: strp("kimi/k3"), FastModel: strp("ghost/lite")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".zcode", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "lite") || strings.Contains(string(raw), "ghost") {
		t.Fatalf("orphan fast provider must drop the lite pin: %s", raw)
	}
	if !strings.Contains(string(raw), "kimi/k3") {
		t.Fatalf("main pin must survive: %s", raw)
	}
	// 清空模型(UI 清字段)→ 移除覆盖,机器级钉死恢复生效。
	if err := writeZcodeModelConfig(home, Persona{}); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(home, ".zcode", "config.json")) {
		t.Fatal("cleared model must remove the override")
	}
}

func TestZcodeVersionProbe(t *testing.T) {
	fakeZcodeBin(t, `#!/bin/sh
printf 'zcode 0.16.3 (build abc)\n'
`)
	if v := zcodeEngineVersion(os.Environ()); v != "0.16.3" {
		t.Fatalf("version probe: %q", v)
	}
}

/* ───────── 探测面 ───────── */

func TestZcodeDetection(t *testing.T) {
	t.Setenv("CUMORA_ZCODE_BIN", "/nonexistent/zcode.cjs")
	t.Setenv("PATH", "/nonexistent-path")
	if got := detectLocalEngines(); len(got) != 1 || got[0] != "zcode" {
		t.Fatalf("zcode must be detected via launcher: %v", got)
	}
	t.Setenv("CUMORA_ZCODE_BIN", "")
	if got := detectLocalEngines(); len(got) != 0 {
		t.Fatalf("nothing on PATH: %v", got)
	}
}

func TestZcodeNoPersistentSession(t *testing.T) {
	// 无持久模式:StartSession 恒 nil,runner 走一次性降级(standing prompt
	// 逐轮内联)。
	if (zcodeAdapter{}).StartSession(SessionArgs{}) != nil {
		t.Fatal("zcode has no persistent session")
	}
}

// B1 回归:运行中取消必须杀进程并立刻带错结算——没有中止 watcher 时,
// 取消的轮会跑到自然退出且按成功记账(评审实测 5s 空跑 + 覆盖 resume id)。
func TestZcodeAbortKillsPromptly(t *testing.T) {
	bin, _ := fakeEngineDir(t)
	wrapper := filepath.Join(bin, "zcode-slow")
	writeScript(t, wrapper, "#!/bin/sh\nsleep 5\nprintf '%s\\n' '{\"sessionId\":\"s\",\"response\":\"late\"}'\n")
	t.Setenv("CUMORA_ZCODE_BIN", wrapper)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	done := make(chan RunResult, 1)
	go func() {
		done <- (zcodeAdapter{}).Run(ctx, RunArgs{Home: t.TempDir(), Prompt: "slow", Env: os.Environ(), OnLog: func(string) {}})
	}()
	select {
	case r := <-done:
		if r.ExitCode == 0 {
			t.Fatal("aborted turn must not report success")
		}
		if r.SessionID != "" {
			t.Fatal("aborted turn must not poison the stored session id")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("abort must settle promptly (watcher missing = runs to completion)")
	}
	cancel()
}
