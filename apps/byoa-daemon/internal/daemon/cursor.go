// daemon 包 cursor —— Cursor Agent 适配器(#66):纯一次性引擎(本版本
// 无持久 stdio 协议——每唤醒新进程,--resume 在新进程里重开同一会话 id)。
// 流为 stream-json:system/init(会话 id+模型)→ user/assistant/thinking →
// 终止 result(轮用量);**流即真相**——is_error:true 在退出码 0 下也算
// 失败轮。分类/探针走只读 --mode ask,绝不用 --force。对齐 engine.ts
// CursorAdapter/CursorTurnTracker(1935–2286)。
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// cursorEvent:Cursor stream-json 的行动子集;未来新事件类型只记日志不破轮。
type cursorEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	Usage     *struct {
		InputTokens      *float64 `json:"inputTokens"`
		OutputTokens     *float64 `json:"outputTokens"`
		CacheReadTokens  *float64 `json:"cacheReadTokens"`
		CacheWriteTokens *float64 `json:"cacheWriteTokens"`
	} `json:"usage"`
	Message *struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"message"`
}

// cursorUsageToEngineUsage:Cursor 的互斥计数器(未缓存 input 与缓存读分列)
// 直映射,cacheWrite → cache_creation。
func cursorUsageToEngineUsage(u *struct {
	InputTokens      *float64 `json:"inputTokens"`
	OutputTokens     *float64 `json:"outputTokens"`
	CacheReadTokens  *float64 `json:"cacheReadTokens"`
	CacheWriteTokens *float64 `json:"cacheWriteTokens"`
}) *EngineUsage {
	if u == nil {
		return nil
	}
	num := func(v *float64) *int64 {
		if v == nil || *v != *v || *v > 1<<62 || *v < -(1<<62) {
			return int64p(0)
		}
		return int64p(int64(*v))
	}
	return &EngineUsage{
		InputTokens:              num(u.InputTokens),
		OutputTokens:             num(u.OutputTokens),
		CacheReadInputTokens:     num(u.CacheReadTokens),
		CacheCreationInputTokens: num(u.CacheWriteTokens),
	}
}

// cursorTextOf:拼接 content 数组的 text 项(与 Claude 同 item 形)。
func cursorTextOf(content any) string {
	arr, ok := content.([]any)
	if !ok {
		return ""
	}
	var out strings.Builder
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "text" {
			if t, ok := m["text"].(string); ok {
				out.WriteString(t)
			}
		}
	}
	return out.String()
}

// cursorTurnTracker:把一轮的 stream-json 折成 daemon 要的东西——会话 id
// (每个事件都重复它;--resume 在逐唤醒的新进程间保连续)、模型
// (system/init 报告的实跑模型胜过 pin)、轮用量、拼接的助手文本(triage
// 读它)、result 事件的错误。轮级单跳在 result 发(Cursor 无逐消息用量,
// 这是诚实粒度——codex turn-completed 同契约)。
type cursorTurnTracker struct {
	sessionID string
	model     string
	usage     *EngineUsage
	text      string
	errMsg    string
	sawResult bool
	startedAt int64
}

// observe:喂一个事件;返回 true = 该事件终止本轮。
func (t *cursorTurnTracker) observe(ev *cursorEvent, onHop func(HopReport)) bool {
	if ev.SessionID != "" {
		t.sessionID = ev.SessionID
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			// init 的模型是 Cursor 实际开会话用的——胜过 pin(pin 只是
			// 我们要的东西)。
			if ev.Model != "" {
				t.model = ev.Model
			}
			if t.startedAt == 0 {
				t.startedAt = nowMS()
			}
		}
		return false
	case "assistant":
		if ev.Message != nil {
			t.text += cursorTextOf(ev.Message.Content)
		}
		return false
	case "result":
		t.sawResult = true
		t.usage = cursorUsageToEngineUsage(ev.Usage)
		if ev.IsError {
			if ev.Result != "" {
				t.errMsg = ev.Result
			} else {
				detail := "cursor turn error"
				if ev.Subtype != "" {
					detail += " (" + ev.Subtype + ")"
				}
				t.errMsg = detail
			}
		}
		if !ev.IsError && t.usage != nil && t.model != "" && onHop != nil {
			hop := HopReport{Model: t.model, Usage: *t.usage, HopIndex: 1, TextChars: len(t.text)}
			if t.startedAt > 0 {
				v := nowMS() - t.startedAt
				hop.LatencyMS = &v
			}
			onHop(hop) // 台账尽力而为,不破流
		}
		return true
	}
	return false
}

// spawnCursorStream:一次性 cursor-agent -p stream-json。流的错误折进结果
// (is_error:true 在退出码 0 下也失败);text = 拼接的助手文本。
func spawnCursorStream(ctx context.Context, plan spawnPlan, argv []string, cwd string, env []string, onLog func(string), stdinText string, onHop func(HopReport), pin string, requireResult bool) (RunResult, string) {
	cmd := buildCmd(plan, argv)
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
	if stdinText != "" {
		_, _ = stdin.Write([]byte(stdinText))
	}
	_ = stdin.Close()
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
	tracker := &cursorTurnTracker{model: pin}
	var mu sync.Mutex
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
				text := carry + string(chunk)
				var lines []string
				if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
					carry = text[idx+1:]
					lines = strings.Split(text[:idx+1], "\n")
				} else {
					carry = text
				}
				for _, line := range lines {
					cleaned := cleanLine(line)
					if cleaned == "" {
						continue
					}
					mu.Lock()
					pushTail(&stdoutTail, cleaned)
					mu.Unlock()
					if onLog != nil {
						onLog(cleaned)
					}
					if strings.HasPrefix(cleaned, "{") {
						var ev cursorEvent
						if json.Unmarshal([]byte(cleaned), &ev) == nil {
							tracker.observe(&ev, onHop)
						}
					}
				}
			}
			if rerr != nil {
				if carry != "" {
					if cleaned := cleanLine(carry); cleaned != "" {
						mu.Lock()
						pushTail(&stdoutTail, cleaned)
						mu.Unlock()
						if onLog != nil {
							onLog(cleaned)
						}
						if strings.HasPrefix(cleaned, "{") {
							var ev cursorEvent
							if json.Unmarshal([]byte(cleaned), &ev) == nil {
								tracker.observe(&ev, onHop)
							}
						}
					}
				}
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
				text := carry + string(chunk)
				var lines []string
				if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
					carry = text[idx+1:]
					lines = strings.Split(text[:idx+1], "\n")
				} else {
					carry = text
				}
				for _, line := range lines {
					if cleaned := cleanLine(line); cleaned != "" {
						mu.Lock()
						pushTail(&stderrTail, cleaned)
						mu.Unlock()
						if onLog != nil {
							onLog(cleaned)
						}
					}
				}
			}
			if rerr != nil {
				if cleaned := cleanLine(carry); cleaned != "" {
					mu.Lock()
					pushTail(&stderrTail, cleaned)
					mu.Unlock()
					if onLog != nil {
						onLog(cleaned)
					}
				}
				return
			}
		}
	}()
	go func() { wg.Wait(); close(readersDone) }()
	waitErr := make(chan error, 1)
	go func() {
		select {
		case <-readersDone:
		case <-ctx.Done():
		}
		waitErr <- cmd.Wait()
	}()
	werr := <-waitErr
	stopWatcher()
	<-readersDone
	procExit, signalName := 1, ""
	if werr == nil {
		procExit = 0
	} else if ee, ok := werr.(*exec.ExitError); ok {
		procExit = ee.ExitCode()
		if procExit < 0 {
			procExit, signalName = 128, signalNameOf(ee)
		}
	}
	mu.Lock()
	errTail, outTail := append([]string{}, stderrTail...), append([]string{}, stdoutTail...)
	mu.Unlock()
	// 流即真相:is_error:true 在退出码 0 下也失败;run() 要求干净退出却
	// 无 result 事件同样失败(没跑完的轮不是成功)。classify/probe 容忍
	// 有文本无 result。
	streamError := ""
	if tracker.errMsg != "" {
		streamError = "engine turn error: " + truncateRunes(tracker.errMsg, maxFailureChars)
	} else if requireResult && !tracker.sawResult {
		streamError = "engine stream ended without a result event (cursor-agent exited early)"
	}
	res := RunResult{SessionID: tracker.sessionID, Usage: tracker.usage, Model: tracker.model}
	if procExit != 0 {
		res.ExitCode = procExit
		res.Err = failurePreview(procExit, signalName, errTail, outTail)
	} else if streamError != "" {
		res.ExitCode = 1
		res.Err = streamError
	}
	return res, tracker.text
}

/* ───────── 适配器 ───────── */

type cursorAdapter struct{}

func init() { RegisterAdapter(cursorAdapter{}) }

func (cursorAdapter) ID() string  { return "cursor" }
func (cursorAdapter) Bin() string { return "cursor-agent" }

// turn:全工具一次性(--force = 隔离 home 内自动放行,Claude 的
// --dangerously-skip-permissions 同位物;--trust 跳过无头应答不了的
// workspace-trust 提示)。提示词是最后一个 argv 元素(Windows 走 stdin)。
func (a cursorAdapter) turn(ctx context.Context, prompt string, cwd string, env []string, onLog func(string), model, resumeSessionID string, onHop func(HopReport)) (RunResult, string) {
	plan := resolveSpawn("cursor-agent")
	base := []string{"-p"}
	if resumeSessionID != "" {
		base = append(base, "--resume", resumeSessionID)
	}
	if model != "" {
		base = append(base, "--model", model)
	}
	base = append(base, "--output-format", "stream-json", "--force", "--trust")
	var argv []string
	stdinText := ""
	if plan.wantsStdinPrompt {
		argv, stdinText = base, prompt
	} else {
		argv = append(base, prompt)
	}
	return spawnCursorStream(ctx, plan, argv, cwd, env, onLog, stdinText, onHop, model, true)
}

// ask:只读一问一答(--mode ask 保持 Q&A-only,分类永不改动任何东西;
// 此处绝不用 --force)。
func (a cursorAdapter) ask(ctx context.Context, prompt string, cwd string, env []string, onLog func(string), model string) (RunResult, string) {
	plan := resolveSpawn("cursor-agent")
	base := []string{"--mode", "ask", "-p", "--output-format", "stream-json"}
	if model != "" {
		base = append(base, "--model", model)
	}
	base = append(base, "--trust")
	var argv []string
	stdinText := ""
	if plan.wantsStdinPrompt {
		argv, stdinText = base, prompt
	} else {
		argv = append(base, prompt)
	}
	return spawnCursorStream(ctx, plan, argv, cwd, env, onLog, stdinText, nil, model, false)
}

func (a cursorAdapter) Classify(ctx context.Context, args ClassifyArgs) ClassifyResult {
	flags := extraArgs("CUMORA_TRIAGE_ARGS")
	if len(flags) > 0 {
		// 用户自持 triage 旗集 → 纯 print 模式原文返回(共享覆盖纪律)。
		plan := resolveSpawn("cursor-agent")
		base := append(append([]string{}, flags...), "-p")
		var argv []string
		stdinText := ""
		if plan.wantsStdinPrompt {
			argv, stdinText = base, args.Prompt
		} else {
			argv = append(base, args.Prompt)
		}
		return spawnCapture(ctx, plan, argv, args.Cwd, args.Env, args.OnLog, stdinText)
	}
	// Cursor 无固定廉价小脑 id(模型是账户门控别名);CUMORA_TRIAGE_MODEL
	// 未设 → Cursor 默认('Auto'),由流的 system/init 诚实回报进台账。
	r, text := a.ask(ctx, args.Prompt, args.Cwd, args.Env, args.OnLog, args.Model)
	return ClassifyResult{Text: text, Err: r.Err, Usage: r.Usage, Model: r.Model}
}

// Probe:small → triage 所在模型(CUMORA_TRIAGE_MODEL,否则 Cursor 默认——
// 与 big 同模型,如实报告);big → Cursor 默认。均只读 ask 模式。
func (a cursorAdapter) Probe(ctx context.Context, args ProbeArgs) ClassifyResult {
	model := ""
	if args.Tier == "small" {
		model = os.Getenv("CUMORA_TRIAGE_MODEL")
	}
	r, text := a.ask(ctx, doctorPrompt, args.Cwd, args.Env, nil, model)
	return ClassifyResult{Text: text, Err: r.Err, Usage: r.Usage, Model: r.Model}
}

// ProbeWake:无独立唤醒路径可探(本版本无持久协议)——唤醒即 probe 已
// 覆盖的同一一次性 spawn → skipped,doctor 隐藏冗余行。
func (cursorAdapter) ProbeWake(ctx context.Context, args WakeProbeArgs) WakeProbeResult {
	return WakeProbeResult{OK: true, Skipped: true}
}

// SeedHome:.cursor/skills 目录 + AGENTS.md 恒重写(人格编辑免新 home 生效;
// Cursor 从 cwd 发现 AGENTS.md)。
func (cursorAdapter) SeedHome(home string, p Persona) error {
	if err := ensureCommonHome(home); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, ".cursor", "skills"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte(personaHeader(p, "AGENTS.md", ".cursor/skills/")), 0o644)
}

// Run:一次性;CUMORA_CURSOR_ARGS 整套覆盖 → 不透明 print 模式(无流可折,
// 无台账;保 --resume + -p + 提示词,覆盖下会话连续性仍存活)。
func (a cursorAdapter) Run(ctx context.Context, args RunArgs) RunResult {
	flags := extraArgs("CUMORA_CURSOR_ARGS")
	plan := resolveSpawn("cursor-agent")
	if len(flags) > 0 {
		base := append(append([]string{}, flags...), args.ResumeFlag()...)
		base = append(base, "-p")
		var argv []string
		stdinText := ""
		if plan.wantsStdinPrompt {
			argv, stdinText = base, args.Prompt
		} else {
			argv = append(base, args.Prompt)
		}
		return spawnEngine(ctx, plan, argv, args, stdinText)
	}
	r, _ := a.turn(ctx, args.Prompt, args.Home, args.Env, args.OnLog, args.Model, args.ResumeSessionID, args.OnHopUsage)
	return r
}

// StartSession:无持久模式(本版本无持久 stdio 协议)——daemon 走一次性
// 路径,以流回报的会话 id --resume。
func (cursorAdapter) StartSession(args SessionArgs) EngineSession { return nil }
