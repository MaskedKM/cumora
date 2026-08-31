// daemon 包 engines —— EngineAdapter 接口与注册表(#64 起为完整协议面,
// 对齐 engine.ts):一次性 run、持久会话 startSession、小脑 classify、
// doctor 探针 probe/probeWake、seedHome 人格落盘。共享的 spawn/行清洗/
// 失败预览基建也在这里;claude/codex 适配器在 claude.go/codex.go,
// grok/cursor 是 #66。
package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// 已知引擎 id(对齐 TS ENGINE_IDS;线上契约用 id——服务端白名单、
// me/agents 的 engine 字段都是这些值)。
var EngineIDs = []string{"claude", "codex", "grok", "cursor", "zcode"}

// engineBin:引擎 id → PATH 可执行名(仅 cursor 的可执行叫 cursor-agent,
// 其余同名——id 与二进制名必须分离,否则 Cursor-only 机器配对报
// cursor-agent 被服务端白名单静默回退 claude)。
func engineBin(id string) string {
	if id == "cursor" {
		return "cursor-agent"
	}
	return id
}

/* ───────── 适配器协议类型(对齐 engine.ts 的导出接口) ───────── */

// Persona:适配器落盘人格所需的 agent 字段。Model/FastModel 是 per-agent
// 模型路由(zcode 经项目级配置钉住;claude/codex 走 --model/env)。
type Persona struct {
	ID           string
	Name         string
	Role         *string
	SystemPrompt *string
	Model        *string
	FastModel    *string
}

// EngineUsage:引擎原始 token 用量(Anthropic 字段名,逐字段透传——
// 定价在服务端;nil 字段 = 引擎未给)。Codex 适配器把 thread 累计量
// 折算成同形字段。
type EngineUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
}

// HopReport:一次出站模型调用(Claude 每条 assistant 消息 / Codex 每个
// turn-completed)。daemon 把它批进 llm_calls 台账。
type HopReport struct {
	Model     string
	Usage     EngineUsage
	LatencyMS *int64
	HopIndex  int
	ToolUses  int
	TextChars int
}

// RunArgs:一次性 turn 的全部输入。Env 是子进程完整环境(daemon 组装
// cumora shim 接线;适配器自行追加 MAX_THINKING_TOKENS 等)。
type RunArgs struct {
	Home            string
	Prompt          string
	Env             []string
	Model           string // "" = 不加 --model(引擎默认)
	FastModel       string // Claude: ANTHROPIC_SMALL_FAST_MODEL
	ResumeSessionID string // "" = 新会话
	OnLog           func(string)
	OnHopUsage      func(HopReport)
	// OnAssistantText:#210 —— 引擎流输出的文本前缀(Claude 逐跳
	// assistant 文本块)。nil = 该引擎/路径无增量可报,流式上屏自然降级。
	OnAssistantText func(text string)
}

// RunResult:一次性 turn 产物。SessionID 非空则落盘供下轮 resume。
type RunResult struct {
	ExitCode  int
	Err       string
	SessionID string
	Usage     *EngineUsage
	Model     string
}

// ClassifyArgs:本地小脑补全(inbox triage)——中性 cwd、无工具、纯文本进出。
type ClassifyArgs struct {
	Cwd    string
	Prompt string
	Env    []string
	Model  string // "" = 适配器默认小模型
	OnLog  func(string)
}

// ClassifyResult:小脑补全产物(text 为模型原文)。
type ClassifyResult struct {
	Text  string
	Err   string
	Usage *EngineUsage
	Model string
}

// ProbeArgs:doctor 一档探针(big=主脑默认模型 / small=廉价快模型)。
type ProbeArgs struct {
	Tier string // "big" | "small"
	Cwd  string
	Env  []string
}

// WakeProbeArgs:唤醒路径探针(验证真实唤醒用的协议路径)。
type WakeProbeArgs struct {
	Cwd string
	Env []string
}

// WakeProbeResult:唤醒路径探针结果。Skipped=本机唤醒折叠为一次性路径
// (自定义参数覆盖/显式退出/Windows codex),doctor 隐藏该行。
type WakeProbeResult struct {
	OK      bool
	Detail  string
	Skipped bool
}

// SessionArgs:持久会话的孵化参数。
type SessionArgs struct {
	Home            string
	Env             []string
	Model           string
	FastModel       string
	ResumeSessionID string // 仅首次孵化/重启后 resume;活进程内自续
	StandingPrompt  string // 不变量系统提示;空 = 无
	OnLog           func(string)
	OnHopUsage      func(HopReport)
	// OnAssistantText:#210 —— 见 RunArgs 同名字段。
	OnAssistantText func(text string)
}

// EngineSession:一个 agent 的长活引擎进程。Send 串行调用(一次一个
// turn);Steer 在安全边界注入运行中 turn;Stop 拆进程。
type EngineSession interface {
	// Send:喂一个 turn,阻塞到该 turn 的 result/turn-completed。
	Send(prompt string) RunResult
	// Steer:同 turn 转向——无在飞 turn 时 no-op。
	Steer(text string)
	Alive() bool
	SessionID() string
	// CarriesStandingPrompt:spawn 时已带外投递 standing prompt(Claude
	// 系统提示文件 / Codex developerInstructions)→ daemon 只发每轮增量;
	// 否则每轮内联。
	CarriesStandingPrompt() bool
	Stop()
}

// EngineAdapter:一个本地引擎。StartSession 返回 nil = 本机无持久模式
// (自定义参数覆盖等)→ daemon 降级一次性 Run。
type EngineAdapter interface {
	ID() string
	Bin() string
	// SeedHome:布置 agent home 让引擎原生读人格/记忆。幂等且不破坏
	// 既有记忆文件。
	SeedHome(home string, p Persona) error
	// Run:一次性 headless turn。
	Run(ctx context.Context, args RunArgs) RunResult
	// StartSession:持久进程;nil = 本机回退一次性 Run。
	StartSession(args SessionArgs) EngineSession
	// Classify:本地小脑补全,返回原文。
	Classify(ctx context.Context, args ClassifyArgs) ClassifyResult
	// Probe:doctor 一档存活探针。
	Probe(ctx context.Context, args ProbeArgs) ClassifyResult
	// ProbeWake:唤醒路径探针;绝不 panic。
	ProbeWake(ctx context.Context, args WakeProbeArgs) WakeProbeResult
}

var (
	enginesMu  sync.Mutex
	adapterReg = map[string]EngineAdapter{}
)

// RegisterAdapter:注册引擎适配器(测试注入 stub 用;真实引擎在各自
// 文件的 init 注册)。
func RegisterAdapter(a EngineAdapter) {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	adapterReg[a.ID()] = a
}

func getAdapter(id string) EngineAdapter {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	return adapterReg[id]
}

// detectLocalEngines:PATH 探测(保 EngineIDs 序)。
func detectLocalEngines() []string {
	var out []string
	for _, id := range EngineIDs {
		if id == "zcode" {
			// zcode 的真实入口不是 PATH 二进制——launcher 三级解析覆盖
			// CUMORA_ZCODE_BIN / 未来的 zcode-cli / 桌面 AppImage。
			if resolveZcodeLauncher(os.Environ()) != nil {
				out = append(out, id)
			}
			continue
		}
		if _, err := exec.LookPath(engineBin(id)); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// requireLocalEngine:无任何引擎时与 TS 同错(退出码 70 由调用方落)。
func requireLocalEngine() ([]string, error) {
	engines := detectLocalEngines()
	if len(engines) == 0 {
		return nil, fmt.Errorf("no supported local agent engine found on PATH")
	}
	return engines, nil
}

/* ───────── 共享 spawn 基建(对齐 engine.ts 的行清洗/尾巴/失败预览) ───────── */

const (
	maxFailureLines = 30
	maxFailureChars = 4000
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")

func cleanLine(line string) string {
	return strings.TrimSpace(strings.ReplaceAll(ansiRe.ReplaceAllString(line, ""), "\r", ""))
}

// truncateRunes:rune 安全截断(字节截会把多字节字符劈成 U+FFFD)。
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// pushTail:有界尾巴(失败预览只看最后 maxFailureLines 行)。
func pushTail(lines *[]string, line string) {
	if line == "" {
		return
	}
	*lines = append(*lines, line)
	if len(*lines) > maxFailureLines {
		*lines = (*lines)[1:]
	}
}

// failurePreview:"process exited with code N" + stderr/stdout 尾巴(截 4000 字符)。
func failurePreview(exitCode int, signalName string, stderr, stdout []string) string {
	var parts []string
	if len(stderr) > 0 {
		parts = append(parts, strings.Join(stderr, "\n"))
	}
	if len(stdout) > 0 {
		parts = append(parts, strings.Join(stdout, "\n"))
	}
	detail := strings.TrimSpace(strings.Join(parts, "\n"))
	prefix := fmt.Sprintf("process exited with code %d", exitCode)
	if signalName != "" {
		prefix = fmt.Sprintf("process terminated by %s", signalName)
	}
	if detail == "" {
		return prefix
	}
	return truncateRunes(prefix+"\n"+detail, maxFailureChars)
}

// stripLoneSurrogates:边界净化(对齐 text-safety.ts 的意图)。TS 字符串是
// UTF-16,半截 emoji 会留下孤立代理项,毒化持久 transcript;Go 字符串是
// UTF-8,同类危害表现为无效字节/代理区码点,这里一并剔除,保证送进引擎
// stdin 的 JSON 永远干净。
func stripLoneSurrogates(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r >= 0xD800 && r <= 0xDFFF }) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == utf8.RuneError || (r >= 0xD800 && r <= 0xDFFF) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// spawnPlan:跨平台 spawn 解析(对齐 resolveSpawn)。POSIX 裸二进制
// shell:false;Windows 上 .cmd/.bat shim 必须经 shell 跑,且多行提示词
// 只能走 stdin(argv 经 shell 无法携带)。wantsStdinPrompt 即此意。
type spawnPlan struct {
	command          string
	shell            bool
	wantsStdinPrompt bool
}

func resolveSpawn(bin string) spawnPlan {
	if !isWindows() {
		return spawnPlan{command: bin}
	}
	pathExt := os.Getenv("PATHEXT")
	if pathExt == "" {
		pathExt = ".COM;.EXE;.BAT;.CMD"
	}
	exts := strings.FieldsFunc(pathExt, func(r rune) bool { return r == ';' })
	pathVar := os.Getenv("PATH")
	// 先找真实 .exe/.cmd/.bat(优先级按 PATHEXT);nvm-windows 的无扩展
	// POSIX shim 只能垫底(它必须经 shell)。
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, bin+ext)
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				lower := strings.ToLower(candidate)
				isBatch := strings.HasSuffix(lower, ".cmd") || strings.HasSuffix(lower, ".bat")
				return spawnPlan{command: candidate, shell: isBatch, wantsStdinPrompt: isBatch}
			}
		}
	}
	for _, dir := range filepath.SplitList(pathVar) {
		if dir == "" {
			continue
		}
		shim := filepath.Join(dir, bin)
		if fi, err := os.Stat(shim); err == nil && !fi.IsDir() {
			return spawnPlan{command: shim, shell: true, wantsStdinPrompt: true}
		}
	}
	return spawnPlan{command: bin, shell: true, wantsStdinPrompt: true}
}

func isWindows() bool { return os.PathSeparator == '\\' }

// buildCmd:按 spawnPlan 组 exec.Cmd(shell 形态经 cmd.exe /c)。
func buildCmd(plan spawnPlan, argv []string) *exec.Cmd {
	if !plan.shell {
		return exec.Command(plan.command, argv...)
	}
	full := append([]string{plan.command}, argv...)
	return exec.Command("cmd.exe", append([]string{"/c"}, full...)...)
}

/* ───────── 一次性 spawn:行泵 + session/usage/model 嗅探 + 逐跳台账 ───────── */

type sniffState struct {
	sessionID string
	usage     *EngineUsage
	model     string
	hopStart  int64 // unixMilli;0 = 无在飞 hop
	hopIndex  int
}

// pumpLines:按 \n 切行(携带跨 chunk 半行),逐行清洗进尾巴/日志;stdout
// 的 JSON 行做嗅探。flush=true 表示流终(冲出无尾换行的最后一行)。
func pumpLines(data []byte, carry *string, flush bool, fn func(line string)) {
	text := *carry + string(data)
	if flush {
		*carry = ""
	} else {
		if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
			text, *carry = text[:idx+1], text[idx+1:]
		} else {
			*carry = text
			return
		}
	}
	for _, line := range strings.Split(text, "\n") {
		fn(line)
	}
}

// countAssistantContent:assistant 消息 content 数组里的 tool_use 计数与
// 文本总长(台账富化提示;畸形载荷回零,不抛)。
func countAssistantContent(content any) (toolUses, textChars int) {
	arr, ok := content.([]any)
	if !ok {
		return 0, 0
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "tool_use":
			toolUses++
		case "text":
			if t, ok := m["text"].(string); ok {
				textChars += len(t)
			}
		}
	}
	return toolUses, textChars
}

// assistantTextBlocks:assistant 消息 content 数组里的文本块(thinking
// /tool_use 不算——只报模型真正"说出口"的前缀);多块以空行拼接。
// 畸形载荷返回空串(不抛)。#210 delta 上报的文本源。
func assistantTextBlocks(content any) string {
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t == "text" {
			if txt, ok := m["text"].(string); ok {
				// 块级 Trim:块首尾空白是 markdown 段落边距,无语义;
				// 块内原文保留。
				if trimmed := strings.TrimSpace(txt); trimmed != "" {
					parts = append(parts, trimmed)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// sniffStdoutLine:一次性路径的 JSON 嗅探(session_id / result usage /
// model / assistant 逐跳 + #210 assistant 文本前缀),与 ClaudeSession
// 的持久路径同形。
func sniffStdoutLine(line string, st *sniffState, onHop func(HopReport), onText func(string)) {
	if !strings.HasPrefix(line, "{") {
		return
	}
	if !strings.Contains(line, `"session_id"`) && !strings.Contains(line, `"usage"`) && !strings.Contains(line, `"model"`) {
		return
	}
	var obj struct {
		SessionID string       `json:"session_id"`
		Type      string       `json:"type"`
		Usage     *EngineUsage `json:"usage"`
		Model     string       `json:"model"`
		Message   *struct {
			Model   string       `json:"model"`
			Usage   *EngineUsage `json:"usage"`
			Content any          `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return // 半行/非 JSON——忽略
	}
	if obj.SessionID != "" {
		st.sessionID = obj.SessionID
	}
	if obj.Type == "result" && obj.Usage != nil {
		st.usage = obj.Usage
	}
	m := obj.Model
	if obj.Message != nil && obj.Message.Model != "" {
		m = obj.Message.Model
	}
	if m != "" {
		st.model = m
	}
	if obj.Type == "assistant" && obj.Message != nil && obj.Message.Usage != nil && m != "" && onHop != nil {
		started := st.hopStart
		st.hopStart = 0
		st.hopIndex++
		toolUses, textChars := countAssistantContent(obj.Message.Content)
		hop := HopReport{Model: m, Usage: *obj.Message.Usage, HopIndex: st.hopIndex, ToolUses: toolUses, TextChars: textChars}
		if started > 0 {
			v := nowMS() - started
			hop.LatencyMS = &v
		}
		onHop(hop)
	} else if st.hopStart == 0 && (obj.Type == "assistant" || obj.Type == "user" || obj.Type == "system") {
		st.hopStart = nowMS()
	}
	if obj.Type == "assistant" && obj.Message != nil && onText != nil {
		if txt := assistantTextBlocks(obj.Message.Content); txt != "" {
			onText(txt + "\n\n") // 逐事件段落分隔(持久路径同款)
		}
	}
	if obj.Type == "result" {
		st.hopStart = 0
		st.hopIndex = 0
	}
}

// spawnEngine:一次性 turn 的共享 spawn——行泵进日志,stdout JSON 嗅探
// session/usage/model/逐跳,abort 杀进程,close 语义冲尾后结算。
func spawnEngine(ctx context.Context, plan spawnPlan, argv []string, args RunArgs, stdinText string) RunResult {
	cmd := buildCmd(plan, argv)
	cmd.Dir = args.Home
	cmd.Env = args.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return RunResult{ExitCode: 1, Err: err.Error()}
	}
	if stdinText != "" {
		_, _ = stdin.Write([]byte(stdinText))
	}
	_ = stdin.Close()

	st := &sniffState{}
	var stderrTail, stdoutTail []string
	var wg sync.WaitGroup
	readersDone := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		r := bufio.NewReader(stdout)
		carry := ""
		for {
			chunk, rerr := r.ReadBytes('\n')
			if len(chunk) > 0 {
				pumpLines(chunk, &carry, false, func(line string) {
					cleaned := cleanLine(line)
					if cleaned == "" {
						return
					}
					pushTail(&stdoutTail, cleaned)
					sniffStdoutLine(cleaned, st, args.OnHopUsage, args.OnAssistantText)
					if args.OnLog != nil {
						args.OnLog(cleaned)
					}
				})
			}
			if rerr != nil {
				pumpLines(nil, &carry, true, func(line string) {
					cleaned := cleanLine(line)
					if cleaned == "" {
						return
					}
					pushTail(&stdoutTail, cleaned)
					sniffStdoutLine(cleaned, st, args.OnHopUsage, args.OnAssistantText)
					if args.OnLog != nil {
						args.OnLog(cleaned)
					}
				})
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		r := bufio.NewReader(stderr)
		carry := ""
		for {
			chunk, rerr := r.ReadBytes('\n')
			if len(chunk) > 0 {
				pumpLines(chunk, &carry, false, func(line string) {
					cleaned := cleanLine(line)
					if cleaned == "" {
						return
					}
					pushTail(&stderrTail, cleaned)
					if args.OnLog != nil {
						args.OnLog(cleaned)
					}
				})
			}
			if rerr != nil {
				pumpLines(nil, &carry, true, func(line string) {
					cleaned := cleanLine(line)
					if cleaned == "" {
						return
					}
					pushTail(&stderrTail, cleaned)
					if args.OnLog != nil {
						args.OnLog(cleaned)
					}
				})
				return
			}
		}
	}()
	// abort:排队中的 turn 在 runner 已停时才 spawn 的孤儿防护——注册晚于
	// abort 事件的监听永不再触发,这里显式补杀。watcher 在进程退出后自退
	// (once 防双 close)。
	if ctx.Err() != nil {
		_ = killProcess(cmd)
	}
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
	go func() { wg.Wait(); close(readersDone) }()
	waitErr := make(chan error, 1)
	go func() {
		// StdoutPipe 铁律:cmd.Wait 在进程退出后关闭管道 fd,内核缓冲里
		// 未读的数据随 fd 关闭而丢——正常路径必须先排干再 Wait(最后一段
		// stdout 常是携带 usage 的 result 事件)。中止路径(ctx)不等排干
		// 直接 Wait,残余输出本就被丢弃(TS 的 exit-vs-close 区分:引擎
		// 自己的子进程可能持有管道不放 EOF,死等 turn 永不结算)。
		select {
		case <-readersDone:
		case <-ctx.Done():
		}
		waitErr <- cmd.Wait()
	}()
	werr := <-waitErr
	stopWatcher()
	// B1:两路都 join 读者——正常路径经由通道关闭天然同步;abort 路径
	// Wait 已关读端,阻塞中的 Read 立刻 ErrFileClosed 返回(孙进程挂住
	// 的是写端,不影响)。不 join 则主流程读尾巴与读者的最后冲刷构成
	// 数据竞争(race 实抓)。
	<-readersDone
	exitCode, signalName := 1, ""
	if werr == nil {
		exitCode = 0
	} else if ee, ok := werr.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
		if exitCode < 0 {
			exitCode, signalName = 128, signalNameOf(ee)
		}
	}
	out := RunResult{ExitCode: exitCode, SessionID: st.sessionID, Usage: st.usage, Model: st.model}
	if exitCode != 0 {
		out.Err = failurePreview(exitCode, signalName, stderrTail, stdoutTail)
	}
	return out
}

// spawnCapture:一次性补全——收全部 stdout 当文本,无嗅探无 run 接线。
func spawnCapture(ctx context.Context, plan spawnPlan, argv []string, cwd string, env []string, onLog func(string), stdinText string) ClassifyResult {
	cmd := buildCmd(plan, argv)
	cmd.Dir = cwd
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return ClassifyResult{Err: err.Error()}
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return ClassifyResult{Err: err.Error()}
	}
	if stdinText != "" {
		_, _ = stdin.Write([]byte(stdinText))
	}
	_ = stdin.Close()
	if ctx.Err() != nil {
		_ = killProcess(cmd)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		exitCode, signalName := 0, ""
		if err != nil {
			exitCode = 1
			if ee, ok := err.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
				if exitCode < 0 {
					exitCode, signalName = 128, signalNameOf(ee)
				}
			}
		}
		text := strings.TrimSpace(ansiRe.ReplaceAllString(stdoutBuf.String(), ""))
		if onLog != nil {
			for _, l := range strings.Split(stdoutBuf.String(), "\n") {
				if c := cleanLine(l); c != "" {
					onLog(c)
				}
			}
		}
		res := ClassifyResult{Text: text}
		if exitCode != 0 {
			var tail []string
			for _, l := range strings.Split(stderrBuf.String(), "\n") {
				pushTail(&tail, cleanLine(l))
			}
			res.Err = failurePreview(exitCode, signalName, tail, nil)
		}
		return res
	case <-ctx.Done():
		_ = killProcess(cmd)
		<-waitDone
		return ClassifyResult{Err: "aborted"}
	}
}

// killProcess:SIGTERM 子进程(Windows 下用 Kill——无信号语义)。
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if isWindows() {
		return cmd.Process.Kill()
	}
	return cmd.Process.Signal(syscall.SIGTERM)
}

func nowMS() int64 { return time.Now().UnixMilli() }

// signalNameOf:从 ExitError 提取致死信号名(非信号死亡返回空)。
func signalNameOf(ee *exec.ExitError) string {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	return ws.Signal().String()
}

/* ───────── env 旋钮 ───────── */

// extraArgs:空格切分的用户自定义参数覆盖(CUMORA_*_ARGS)。
func extraArgs(envVar string) []string {
	raw := os.Getenv(envVar)
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}

// salientError:从原始输出提炼用户可读的显著错误(限流/配额/鉴权类
// 模式优先;否则整段压平),doctor 与探针用它给出一行诊断。
var salientErrorRe = regexp.MustCompile(`(?i)((?:error|fatal)\b[:\- ].*|you'?ve hit your usage limit.*|usage limit.*|rate.?limit.*|quota.*|over(?:loaded|capacity).*|insufficient (?:credit|quota).*|unauthor\w*.*|forbidden.*|invalid (?:api )?key.*|not (?:logged in|authenticated|signed in).*|(?:please )?(?:sign|log) ?in.*|authentication .*)`)

func salientError(raw string) string {
	clean := strings.TrimSpace(ansiRe.ReplaceAllString(strings.ReplaceAll(raw, "\r", ""), ""))
	m := salientErrorRe.FindString(clean)
	if m != "" {
		clean = m
	}
	return truncateRunes(strings.Join(strings.Fields(clean), " "), 280)
}

// withEnvDefault:替换式 env 注入(TS 的 {...env, K:V} 语义)。重复键下
// getenv 取首值——纯追加会让操作者自设的旧值继续生效。
//
// withEnvDefaultKeep:TS 的 env.K ?? '1' 语义——键已存在(含显式空串)
// 则保留操作者的值,仅缺键时补默认(grok 的 GROK_DISABLE_AUTOUPDATER)。
func withEnvDefaultKeep(env []string, key, def string) []string {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return env
		}
	}
	return append(append([]string{}, env...), key+"="+def)
}
func withEnvDefault(env []string, kv string) []string {
	key := kv[:strings.IndexByte(kv, '=')]
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, kv)
}

// triageModel:CUMORA_TRIAGE_MODEL 覆盖,否则适配器各自的缺省小模型。
func triageModel(fallback string) string {
	if v := strings.TrimSpace(os.Getenv("CUMORA_TRIAGE_MODEL")); v != "" {
		return v
	}
	return fallback
}

/* ───────── 公共 home 布置(对齐 ensureCommonHome) ───────── */

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func ensureCommonHome(home string) error {
	for _, sub := range []string{"memory", "notes", "workspace"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o755); err != nil {
			return err
		}
	}
	// 记忆索引仅在缺失时播种——绝不覆盖 agent 自己写下的内容。
	memoryIndex := filepath.Join(home, "memory", "MEMORY.md")
	if !pathExists(memoryIndex) {
		content := "# Memory index\n\n" +
			"One line per durable fact, pointing at the file that holds it:\n" +
			"`- [Title](file.md) — one-line hook`\n\n" +
			"Write the fact itself in its own `memory/<topic>.md` file; keep this index short.\n"
		if err := os.WriteFile(memoryIndex, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
