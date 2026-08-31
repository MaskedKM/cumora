// daemon 包 zcode —— ZCode 适配器(#65):无独立 CLI 发行版,无头入口是
// ZCode 桌面安装内的 resources/glm/zcode.cjs(Linux AppImage 挂载点随版本
// 漂移,launcher 三级解析);唤醒形态为一次性 `-p --json`(轮末单信封:
// sessionId/response/usage),--resume 续会话 + 陈旧自愈;per-agent 模型经
// 项目级 .zcode/config.json 覆盖(provider 表不跨层合并,条目原样复制)。
// 对齐 engine.ts 的 ZcodeAdapter(2288–2743);契约细节见
// docs/byoa-zcode-notes.md。
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

/* ───────── launcher 三级解析 ───────── */

// zcodeLauncher:如何启动无头 zcode。
type zcodeLauncher struct {
	command string   // 要 spawn 的可执行
	prefix  []string // command 之后的固定 argv(zcode.cjs 路径等)
	source  string   // 来源(doctor/配对日志)
}

func zcodeMissingMessage() string {
	return "zcode headless entry not found. Install the ZCode desktop app (Linux AppImage) and log in once " +
		"(`node <app>/resources/glm/zcode.cjs login`), or point CUMORA_ZCODE_BIN at a zcode.cjs / wrapper. " +
		"See docs/byoa-zcode-notes.md."
}

// zcodeNodeCommand:运行 zcode.cjs 的 node(TS 用自身运行时 process.execPath;
// Go daemon 无内嵌 node,从 PATH 解析——结构性对齐,二进制差异注记于此)。
func zcodeNodeCommand() string {
	if p, err := exec.LookPath("node"); err == nil {
		return p
	}
	return "node"
}

// resolveZcodeLauncher:CUMORA_ZCODE_BIN → PATH 上的 zcode-cli(POSIX)→
// Linux 桌面 AppImage(解析 .desktop 的 Exec=,按 size+mtime 键控缓存一次性
// 抽出 zcode.cjs)。裸 `zcode`(Electron GUI)刻意不用。
func resolveZcodeLauncher(env []string) *zcodeLauncher {
	getenv := func(key string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if strings.HasPrefix(env[i], key+"=") {
				return env[i][len(key)+1:]
			}
		}
		return ""
	}
	if override := strings.TrimSpace(getenv("CUMORA_ZCODE_BIN")); override != "" {
		if strings.HasSuffix(override, ".cjs") {
			return &zcodeLauncher{command: zcodeNodeCommand(), prefix: []string{override}, source: "CUMORA_ZCODE_BIN"}
		}
		return &zcodeLauncher{command: override, prefix: nil, source: "CUMORA_ZCODE_BIN"}
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if dir == "" {
			continue
		}
		// 仅 POSIX 风格的 zcode-cli:无 Windows 发行版。
		candidate := filepath.Join(dir, "zcode-cli")
		if !isWindows() {
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				return &zcodeLauncher{command: candidate, prefix: nil, source: "PATH:zcode-cli"}
			}
		}
	}
	home := getenv("HOME")
	if home == "" {
		home = homeDir()
	}
	// TS 仅在 linux 走桌面 AppImage 分支(其余 OS 的同名文件不是它)。
	if runtime.GOOS == "linux" && home != "" && home != "." {
		desktop, err := os.ReadFile(filepath.Join(home, ".local", "share", "applications", "zcode.desktop"))
		if err == nil {
			var appimage string
			for _, line := range strings.Split(string(desktop), "\n") {
				if !strings.HasPrefix(line, "Exec=") {
					continue
				}
				v := strings.TrimSpace(line[len("Exec="):])
				if quoted := extractQuotedPath(v); quoted != "" {
					appimage = quoted
				} else {
					appimage = strings.Fields(v)[0]
				}
				break
			}
			// .mount_ 路径是运行中的挂载点,会漂移——不是稳定指针。
			if appimage != "" && strings.Contains(appimage, "/") && !strings.Contains(appimage, ".mount_") {
				if st, err := os.Stat(appimage); err == nil && !st.IsDir() {
					cacheRoot := getenv("XDG_CACHE_HOME")
					if cacheRoot == "" {
						cacheRoot = filepath.Join(home, ".cache")
					}
					cacheDir := filepath.Join(cacheRoot, "cumora", "zcode",
						fmt.Sprintf("%d-%d", st.Size(), st.ModTime().UnixMilli()))
					cjs := filepath.Join(cacheDir, "zcode.cjs")
					if _, err := os.Stat(cjs); err != nil {
						if err := extractZcodeCjs(appimage, cacheDir, cjs); err != nil {
							return nil
						}
					}
					return &zcodeLauncher{command: zcodeNodeCommand(), prefix: []string{cjs}, source: "appimage:" + appimage}
				}
			}
		}
	}
	return nil
}

// extractZcodeCjs:一次性把 resources/glm/zcode.cjs 抽进内容键控缓存。
// 临时目录抽取 → staging 拷贝 → rename:抽取中途崩溃不会留下一个
// "存在即命中"的半个缓存文件。
func extractZcodeCjs(appimage, cacheDir, cjs string) error {
	tmp, err := os.MkdirTemp("", "cumora-zcode-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.Command(appimage, "--appimage-extract", "resources/glm/zcode.cjs")
	cmd.Dir = tmp
	if err := cmd.Run(); err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	staging := fmt.Sprintf("%s.tmp-%d", cjs, os.Getpid())
	data, err := os.ReadFile(filepath.Join(tmp, "squashfs-root", "resources", "glm", "zcode.cjs"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(staging, data, 0o755); err != nil {
		return err
	}
	return os.Rename(staging, cjs)
}

// extractQuotedPath:Exec= 值带引号路径(含空格)时取引号内;否则空。
func extractQuotedPath(v string) string {
	if !strings.HasPrefix(v, `"`) {
		return ""
	}
	if end := strings.Index(v[1:], `"`); end >= 0 {
		return v[1 : 1+end]
	}
	return ""
}

// zcodeEngineVersion:`zcode version` 尽力探测(配对时漂移诊断);失败为 nil。
func zcodeEngineVersion(env []string) string {
	launcher := resolveZcodeLauncher(env)
	if launcher == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, launcher.command, append(append([]string{}, launcher.prefix...), "version")...)
	cmd.Env = env
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	out := ansiRe.ReplaceAllString(stdout.String(), "")
	for _, tok := range strings.Fields(out) {
		if strings.HasPrefix(tok, "v") && isVersionLike(tok[1:]) {
			return tok[1:]
		}
		if isVersionLike(tok) {
			return tok
		}
	}
	if t := truncateRunes(strings.TrimSpace(out), 32); t != "" {
		return t
	}
	return ""
}

func isVersionLike(s string) bool {
	if s == "" {
		return false
	}
	digits, dots := 0, 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.':
			dots++
		case r == '-' || r == '+':
		default:
			if digits == 0 {
				return false
			}
		}
	}
	return digits > 0 && dots >= 2
}

/* ───────── 信封 ───────── */

// zcodeEnvelope:-p --json 的轮末单信封(usage 为 provider 原始驼峰)。
type zcodeEnvelope struct {
	SessionID string `json:"sessionId"`
	Response  string `json:"response"`
	Usage     *struct {
		InputTokens      *float64 `json:"inputTokens"`
		OutputTokens     *float64 `json:"outputTokens"`
		CacheReadTokens  *float64 `json:"cacheReadTokens"`
		CacheWriteTokens *float64 `json:"cacheWriteTokens"`
		ReasoningTokens  *float64 `json:"reasoningTokens"`
	} `json:"usage"`
}

// parseZcodeEnvelope:整个 stdout 即信封;非信封(自定义参数/未来 CLI 变化)
// 回退原文。判定用**键存在性**(TS 'response' in parsed || 'sessionId' in
// parsed):退化信封(response:""/sessionId:"")与字段类型不符(如 usage
// 是字符串)都仍是信封;逐字段宽容解码,坏字段取零值不整体拒绝。
func parseZcodeEnvelope(stdout string) (envelope *zcodeEnvelope, text string) {
	text = strings.TrimSpace(ansiRe.ReplaceAllString(stdout, ""))
	if !strings.HasPrefix(text, "{") {
		return nil, text
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, text
	}
	if _, ok := raw["response"]; !ok {
		if _, ok := raw["sessionId"]; !ok {
			return nil, text
		}
	}
	var parsed zcodeEnvelope
	_ = json.Unmarshal([]byte(text), &parsed) // 宽容:类型不符的字段留零值
	return &parsed, text
}

// zcodeUsageToEngineUsage:inputTokens 含缓存部分(POC 实证:input 21295
// cacheRead 17600)——非缓存部分才记 input,与 ClaudeSession 同口径;
// reasoning 并入 output(引擎用量无此字段,且属输出侧开销)。
func zcodeUsageToEngineUsage(e *zcodeEnvelope) *EngineUsage {
	if e == nil || e.Usage == nil {
		return nil
	}
	num := func(v *float64) int64 {
		if v == nil || *v != *v || *v > 1<<62 || *v < -(1<<62) {
			return 0
		}
		return int64(*v)
	}
	input := num(e.Usage.InputTokens)
	cacheRead := num(e.Usage.CacheReadTokens)
	out := maxInt64(0, input-cacheRead)
	in := &out
	return &EngineUsage{
		InputTokens:              in,
		OutputTokens:             int64p(num(e.Usage.OutputTokens) + num(e.Usage.ReasoningTokens)),
		CacheReadInputTokens:     int64p(cacheRead),
		CacheCreationInputTokens: int64p(num(e.Usage.CacheWriteTokens)),
	}
}

/* ───────── 配置双层归因 ───────── */

type zcodeUserConfig struct {
	Model *struct {
		Main *string `json:"main"`
		Lite *string `json:"lite"`
	} `json:"model"`
	Provider map[string]json.RawMessage `json:"provider"`
}

// readZcodeUserConfig:操作者级 CLI 配置(尽力而为)。
func readZcodeUserConfig(env []string) *zcodeUserConfig {
	home := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") {
			home = kv[len("HOME="):]
		}
	}
	if home == "" {
		home = homeDir()
	}
	b, err := os.ReadFile(filepath.Join(home, ".zcode", "cli", "config.json"))
	if err != nil {
		return nil
	}
	var cfg zcodeUserConfig
	if json.Unmarshal(b, &cfg) != nil {
		return nil
	}
	return &cfg
}

// readZcodeMainModel:本 agent 轮次实际所在模型(诚实归因——信封不带模型,
// 台账不能空)。两层:agent home 的项目配置优先,其次操作者用户级钉死。
func readZcodeMainModel(env []string, cwd string) string {
	if cwd != "" {
		if b, err := os.ReadFile(filepath.Join(cwd, ".zcode", "config.json")); err == nil {
			var proj struct {
				Model *struct {
					Main *string `json:"main"`
				} `json:"model"`
			}
			if json.Unmarshal(b, &proj) == nil && proj.Model != nil && proj.Model.Main != nil && *proj.Model.Main != "" {
				return *proj.Model.Main
			}
		}
	}
	if cfg := readZcodeUserConfig(env); cfg != nil && cfg.Model != nil && cfg.Model.Main != nil {
		return *cfg.Model.Main
	}
	return ""
}

/* ───────── 一次性 -p --json 调用 ───────── */

// zcodeDriftHint:zcode 帮助文本曾超前于参数解析器(0.16.3 列出解析器拒绝
// 的旗),升级后旗级断裂是现实失败模式——裸解析错误转成可行动诊断。
func zcodeDriftHint(errText string) string {
	if strings.Contains(strings.ToLower(errText), "unknown option") {
		return errText + "\n[zcode] CLI drift: this zcode build doesn't accept one of the flags cumora passes. " +
			"Update the ZCode desktop app, or pin a known-good entry via CUMORA_ZCODE_BIN, then re-pair."
	}
	return errText
}

// spawnZcodeJson:一次 -p --json 调用,信封折进 RunResult。信封携带 usage 时
// 发一跳(zcode 每轮只报一次,多发即重复记账)。text 一并返回(classify 用)。
// 管道尾形与 spawnEngine 同款(StdoutPipe 铁律 + 两路 join 读者)。
func spawnZcodeJson(ctx context.Context, launcher *zcodeLauncher, argv []string, cwd string, env []string, onLog func(string), onHop func(HopReport)) (RunResult, string) {
	startedAt := nowMS()
	cmd := exec.Command(launcher.command, append(append([]string{}, launcher.prefix...), argv...)...)
	cmd.Dir = cwd
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}, ""
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}, ""
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}, ""
	}
	if err := cmd.Start(); err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}, ""
	}
	_ = stdin.Close()
	if ctx.Err() != nil {
		_ = killProcess(cmd)
	}
	// #259:一次性路径同受活性看门狗保护(与 spawnEngine 同款;判死 →
	// 杀进程,结果带 124 与原因,分类按 engine-timeout)。
	var wdReasonMu sync.Mutex
	wdReason := ""
	wd := newActivityWatchdog(func(reason string) {
		wdReasonMu.Lock()
		wdReason = reason
		wdReasonMu.Unlock()
		_ = killProcess(cmd)
	})
	wd.Arm()
	// 中止 watcher(与 spawnEngine 同款):排队中的 turn 在 runner 已停时
	// 才 spawn 的孤儿防护 + 运行中取消的实际杀灭——没有它,取消后
	// cmd.Wait 会等自然退出,被取消的轮烧完配额还按成功记账(评审实测)。
	abort := make(chan struct{})
	var abortOnce sync.Once
	stopWatcher := func() { abortOnce.Do(func() { close(abort) }) }
	go func() {
		select {
		case <-ctx.Done():
			_ = killProcess(cmd)
		case <-abort:
		}
	}()
	var stdoutBuf bytes.Buffer
	var stderrTail []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// 逐段拷贝而非 io.Copy:每段即一次 #259 出声信号。
		buf := make([]byte, 32*1024)
		for {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				wd.Activity(false, false)
				stdoutBuf.Write(buf[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		r := bufio.NewReader(stderr) // Reader 无界行(Scanner 64KB 帽会截断长 stderr)
		for {
			line, rerr := r.ReadString('\n')
			if c := cleanLine(line); c != "" {
				wd.Activity(false, false) // #259 stderr 出声也算活
				mu.Lock()
				pushTail(&stderrTail, c)
				mu.Unlock()
				if onLog != nil {
					onLog(c)
				}
			}
			if rerr != nil {
				return
			}
		}
	}()
	readersDone := make(chan struct{})
	go func() { wg.Wait(); close(readersDone) }()
	waitErr := make(chan error, 1)
	go func() {
		// 正常:先排干再 Wait(stdout 的最后一段就是整个信封);中止:
		// 进程一退即 Wait,残余输出本就被丢弃(TS exit-vs-close 同判)。
		select {
		case <-readersDone:
		case <-ctx.Done():
		}
		waitErr <- cmd.Wait()
	}()
	werr := <-waitErr
	stopWatcher()
	<-readersDone
	wd.Disarm()
	wdReasonMu.Lock()
	idleReason := wdReason
	wdReasonMu.Unlock()
	exitCode, signalName := 1, ""
	if werr == nil {
		exitCode = 0
	} else if ee, ok := werr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
		if exitCode < 0 {
			exitCode, signalName = 128, signalNameOf(ee)
		}
	}
	if idleReason != "" {
		// 判死:信封大概率残缺,直接按超时形态返回。
		return RunResult{ExitCode: 124, Err: idleReason}, ""
	}
	mu.Lock()
	tail := append([]string{}, stderrTail...)
	mu.Unlock()
	envelope, text := parseZcodeEnvelope(stdoutBuf.String())
	if envelope == nil {
		res := RunResult{ExitCode: exitCode}
		if exitCode != 0 {
			res.Err = zcodeDriftHint(failurePreview(exitCode, signalName, tail, []string{text}))
		}
		return res, text
	}
	usage := zcodeUsageToEngineUsage(envelope)
	// 诚实归因:信封不带模型 → 报本 agent 配置解析到的 id;两层皆不可读
	// 也要给台账一行可 grep 的占位,不能静默丢跳。
	model := readZcodeMainModel(env, cwd)
	if model == "" {
		model = "zcode-unknown-model"
	}
	if exitCode == 0 && usage != nil && onHop != nil {
		latency := nowMS() - startedAt
		onHop(HopReport{Model: model, Usage: *usage, LatencyMS: &latency, HopIndex: 1})
	}
	res := RunResult{ExitCode: exitCode, SessionID: envelope.SessionID, Usage: usage, Model: model}
	if exitCode != 0 {
		detail := text
		if envelope.Response != "" {
			detail = envelope.Response
		}
		res.Err = zcodeDriftHint(failurePreview(exitCode, signalName, tail, []string{detail}))
	}
	return res, envelope.Response
}

/* ───────── 适配器 ───────── */

type zcodeAdapter struct{}

func init() { RegisterAdapter(zcodeAdapter{}) }

func (zcodeAdapter) ID() string  { return "zcode" }
func (zcodeAdapter) Bin() string { return "zcode-cli" }

// Run:一次性唤醒(无持久 stdio 协议消费者;zcode app-server 是未来路径),
// --resume 跨轮续命。陈旧 resume(Session 被清/换引擎残留 id)不得楔死
// agent:去掉 --resume 重试一次(CodexSession 的 resume→fresh 同型自愈)。
func (zcodeAdapter) Run(ctx context.Context, args RunArgs) RunResult {
	launcher := resolveZcodeLauncher(args.Env)
	if launcher == nil {
		return RunResult{ExitCode: 1, Err: zcodeMissingMessage()}
	}
	prompt := stripLoneSurrogates(args.Prompt)
	flags := extraArgs("CUMORA_ZCODE_ARGS")
	resume := args.ResumeFlag()
	if len(flags) > 0 {
		// 用户整套旗覆盖 → 不透明 print 模式(与其余引擎同型逃生口):
		// 不能假设 --json 信封,无 usage/台账;保留 --resume + -p,
		// 会话连续性在覆盖下仍存活。
		argv := append(append(append([]string{}, launcher.prefix...), flags...), resume...)
		argv = append(argv, "-p", prompt)
		plan := spawnPlan{command: launcher.command}
		return spawnEngine(ctx, plan, argv, args, "")
	}
	base := []string{"--cwd", args.Home, "--mode", "yolo", "--no-color", "--json"}
	base = append(base, resume...)
	base = append(base, "-p", prompt)
	r, _ := spawnZcodeJson(ctx, launcher, base, args.Home, args.Env, args.OnLog, args.OnHopUsage)
	if r.Err != "" && args.ResumeSessionID != "" && strings.Contains(strings.ToLower(r.Err), "session not found") {
		if args.OnLog != nil {
			args.OnLog(fmt.Sprintf("[zcode] session %s not found — starting a fresh session", args.ResumeSessionID))
		}
		fresh := []string{"--cwd", args.Home, "--mode", "yolo", "--no-color", "--json", "-p", prompt}
		r2, _ := spawnZcodeJson(ctx, launcher, fresh, args.Home, args.Env, args.OnLog, args.OnHopUsage)
		return r2
	}
	return r
}

// ask:小脑/探针共用——只读 plan 模式 + 工具黑名单(POC:plan 模式本身
// 已挡住全部写尝试;黑名单是纵深防御)。zcode 无小模型旗且配置无隔离,
// 诚实跑默认模型并按配置归因(Cursor 的诚实规则同型)。
func (zcodeAdapter) ask(ctx context.Context, prompt string, cwd string, env []string, onLog func(string)) (RunResult, string) {
	flags := extraArgs("CUMORA_TRIAGE_ARGS")
	launcher := resolveZcodeLauncher(env)
	if launcher == nil {
		return RunResult{ExitCode: 1, Err: zcodeMissingMessage()}, ""
	}
	if len(flags) > 0 {
		// 用户自持 triage 旗集 → 纯 print 模式,原文返回(共享覆盖纪律;
		// 无信封可折)。
		argv := append(append([]string{}, launcher.prefix...), flags...)
		argv = append(argv, "-p", prompt)
		plan := spawnPlan{command: launcher.command}
		r := spawnCapture(ctx, plan, argv, cwd, env, onLog, "")
		res := RunResult{ExitCode: 0, SessionID: ""}
		if r.Err != "" {
			res.ExitCode = 1
			res.Err = r.Err
		}
		return res, r.Text
	}
	argv := []string{"--cwd", cwd, "--mode", "plan", "--no-color", "--json", "--disallowed-tools", "Bash Edit Write", "-p", stripLoneSurrogates(prompt)}
	return spawnZcodeJson(ctx, launcher, argv, cwd, env, onLog, nil)
}

func (a zcodeAdapter) Classify(ctx context.Context, args ClassifyArgs) ClassifyResult {
	r, text := a.ask(ctx, args.Prompt, args.Cwd, args.Env, args.OnLog)
	return ClassifyResult{Text: text, Err: r.Err, Usage: r.Usage, Model: r.Model}
}

// Probe:big/small 同模型(无小档)——探针如实报告实际运行的东西。
func (a zcodeAdapter) Probe(ctx context.Context, args ProbeArgs) ClassifyResult {
	r, text := a.ask(ctx, doctorPrompt, args.Cwd, args.Env, nil)
	return ClassifyResult{Text: text, Err: r.Err, Usage: r.Usage, Model: r.Model}
}

// ProbeWake:本适配器无独立唤醒路径(无持久协议),唤醒即 probe 已覆盖的
// 同一一次性 spawn → skipped。
func (zcodeAdapter) ProbeWake(ctx context.Context, args WakeProbeArgs) WakeProbeResult {
	return WakeProbeResult{OK: true, Skipped: true}
}

// SeedHome:ensureCommonHome + skills/ 目录(人格头指向它)+ AGENTS.md 恒重写
// (人格编辑要生效;zcode 同时读 AGENTS.md 与 CLAUDE.md,AGENTS.md 是跨引擎
// 约定)+ 项目级模型配置。
func (zcodeAdapter) SeedHome(home string, p Persona) error {
	if err := ensureCommonHome(home); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, "skills"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte(personaHeader(p, "AGENTS.md", "skills/")), 0o644); err != nil {
		return err
	}
	return writeZcodeModelConfig(home, p)
}

// writeZcodeModelConfig:UI 的 model/fastModel 经项目级配置钉住(实证可覆盖
// 用户级钉死)。provider 表不跨层合并 → 被引用的 provider 条目(含 apiKey)
// 从操作者配置原样复制。无模型 → 移除陈旧覆盖,让机器级钉死(及 UI 清空
// 字段)按预期工作。
func writeZcodeModelConfig(home string, p Persona) error {
	projPath := filepath.Join(home, ".zcode", "config.json")
	model, fastModel := "", ""
	if p.Model != nil {
		model = strings.TrimSpace(*p.Model)
	}
	if p.FastModel != nil {
		fastModel = strings.TrimSpace(*p.FastModel)
	}
	env := os.Environ()
	var providerID string
	if model != "" && strings.Contains(model, "/") {
		providerID = model[:strings.IndexByte(model, '/')]
	}
	if model == "" || providerID == "" {
		if model != "" && providerID == "" {
			slog.Warn(`[zcode] agent model is not in provider/model form (e.g. kimi/k3) — leaving the machine-level pin in place`, "model", model)
		}
		if model == "" && fastModel != "" {
			slog.Warn("[zcode] fast model set without a main model — zcode pins lite only alongside a main pin; ignoring it")
		}
		_ = os.Remove(projPath)
		return nil
	}
	user := readZcodeUserConfig(env)
	if user == nil || user.Provider == nil || len(user.Provider[providerID]) == 0 {
		slog.Warn("[zcode] provider not found in ~/.zcode/cli/config.json — add it there first (or run the CLI login), leaving the machine-level pin in place", "provider", providerID)
		_ = os.Remove(projPath)
		return nil
	}
	// lite 可引用不同 provider——其条目也要复制,否则 zcode 整配置报
	// "provider X is missing baseURL"(表不跨层合并)。fast 的 provider 缺失
	// 时丢 lite 并告警,不破坏主钉。
	lite := ""
	providers := map[string]json.RawMessage{providerID: user.Provider[providerID]}
	if fastModel != "" && strings.Contains(fastModel, "/") {
		fastProvider := fastModel[:strings.IndexByte(fastModel, '/')]
		if entry, ok := user.Provider[fastProvider]; ok && len(entry) > 0 {
			lite = fastModel
			providers[fastProvider] = entry
		} else {
			slog.Warn("[zcode] fast model's provider not found in ~/.zcode/cli/config.json — dropping the lite pin (main pin unaffected)", "fastModel", fastModel)
		}
	}
	type projModel struct {
		Main string  `json:"main"`
		Lite *string `json:"lite,omitempty"`
	}
	proj := struct {
		Model    projModel                  `json:"model"`
		Provider map[string]json.RawMessage `json:"provider"`
	}{Model: projModel{Main: model}, Provider: providers}
	if lite != "" {
		proj.Model.Lite = &lite
	}
	b, err := json.MarshalIndent(proj, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, ".zcode"), 0o755); err != nil {
		return err
	}
	// 0600:此文件携带操作者的 provider apiKey(runtime token、standing
	// prompt 同级的机密文件纪律)。
	return os.WriteFile(projPath, b, 0o600)
}

// StartSession:无持久模式(zcode app-server 的 JSON-RPC 会话是后续工作,
// 见追踪票)——daemon 走一次性 Run + --resume 保上下文。
func (zcodeAdapter) StartSession(args SessionArgs) EngineSession { return nil }
