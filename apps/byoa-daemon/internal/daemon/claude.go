// daemon 包 claude —— Claude Code 适配器(#64):一次性 run(-p
// stream-json)+ 持久会话(stdin stream-json 进、stdout 事件出)+ 小脑
// classify + doctor 探针。对齐 engine.ts 的 ClaudeSession/ClaudeAdapter。
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// turnTimeoutMS:CUMORA_TURN_TIMEOUT_MS 的选配失控保险(默认关——墙钟
// 超时分不清真挂死与合法长任务,同轮 STEERING 才是响应性正解)。
func turnTimeoutMS() int64 {
	v := strings.TrimSpace(os.Getenv("CUMORA_TURN_TIMEOUT_MS"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		return int64(n)
	}
	// TS Number() 也吃 "1e4"——ParseFloat 兜底,非数值/负数归 0。
	if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
		return int64(f)
	}
	return 0
}

// doctorPrompt:探针的一次真实微任务(一个 token 的输出)。
const doctorPrompt = "Connectivity check. Reply with exactly: OK"

// claudeUserMsg:stream-json 用户消息(送引擎 stdin 的行)。
func claudeUserMsg(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": stripLoneSurrogates(text)}},
		},
	})
	return string(b) + "\n"
}

// claudeSession:单 agent 的常驻 Claude 进程(stream-json I/O 模式):
// 进程持续从 stdin 读换行分隔的用户消息,每轮在其 result 事件上结算。
type claudeSession struct {
	cmd             *exec.Cmd
	stdin           io.WriteCloser
	onLog           func(string)
	onHop           func(HopReport)
	carriesStanding bool

	mu                     sync.Mutex
	sid                    string
	curModel               string
	hopStart               int64 // unixMilli;0 = 无在飞 hop
	hopIndex               int
	exited                 bool
	exitCode               int
	pending                chan RunResult // 非 nil = 有在飞 turn(容量 1)
	pendStderr, pendStdout []string
	stderrTail, stdoutTail []string
	steerQueue             []string
	turnTimer              *time.Timer
	// pumps:stdout/stderr 读者;waitExit 须等它们排干再 Wait(StdoutPipe
	// 铁律——Wait 关管道 fd 会丢内核缓冲里的尾行,如最后一个 result 事件)。
	pumps sync.WaitGroup
}

// newClaudeSession:spawn 常驻进程并接好泵。die 路径统一在 waitExit。
func newClaudeSession(bin string, argv []string, args SessionArgs, carriesStanding bool) *claudeSession {
	plan := resolveSpawn(bin)
	cmd := buildCmd(plan, argv)
	cmd.Dir = args.Home
	cmd.Env = args.Env
	s := &claudeSession{
		onLog:           args.OnLog,
		onHop:           args.OnHopUsage,
		carriesStanding: carriesStanding,
		sid:             args.ResumeSessionID,
		pending:         nil,
	}
	s.cmd = cmd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.die(1, err.Error())
		return s
	}
	s.stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.die(1, err.Error())
		return s
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.die(1, err.Error())
		return s
	}
	if err := cmd.Start(); err != nil {
		s.die(1, err.Error())
		return s
	}
	s.pumps.Add(2)
	go s.pumpStdout(stdout)
	go s.pumpStderr(stderr)
	go s.waitExit()
	return s
}

func (s *claudeSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aliveLocked()
}

// aliveLocked:调用方已持锁时的活性判定(Alive 不可重入——Send/Steer
// 的持锁路径曾因此自死锁)。
func (s *claudeSession) aliveLocked() bool { return !s.exited && s.stdin != nil }

func (s *claudeSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

func (s *claudeSession) CarriesStandingPrompt() bool { return s.carriesStanding }

// Send:喂一轮,阻塞到 result 事件/进程死亡。在飞或进程已死立即返回错。
func (s *claudeSession) Send(prompt string) RunResult {
	s.mu.Lock()
	if s.pending != nil {
		sid := s.sid
		s.mu.Unlock()
		return RunResult{ExitCode: 1, Err: "engine session busy — a turn is already in flight", SessionID: sid}
	}
	if !s.aliveLocked() {
		exitCode := s.exitCode
		if exitCode == 0 {
			exitCode = 1
		}
		// 持锁快照:早退分支的组合字面量在 Unlock 后求值会裸读泵侧
		// 并发写入的字段(race 实抓)。
		detail := failurePreview(exitCode, "", s.stderrTail, s.stdoutTail)
		sid := s.sid
		s.mu.Unlock()
		if detail == "" {
			detail = "engine session is not alive (process gone)"
		}
		return RunResult{ExitCode: exitCode, Err: detail, SessionID: sid}
	}
	ch := make(chan RunResult, 1)
	s.pending = ch
	s.pendStderr, s.pendStdout = nil, nil
	// 选配失控保险(默认关):超窗 abort + 重spawn(下一唤醒 --resume)。
	if to := turnTimeoutMS(); to > 0 {
		s.turnTimer = time.AfterFunc(time.Duration(to)*time.Millisecond, func() {
			s.settle(RunResult{ExitCode: 124,
				Err:       fmt.Sprintf("engine turn exceeded CUMORA_TURN_TIMEOUT_MS (%ds) — aborted; session will respawn", to/1000),
				SessionID: s.SessionID()})
			s.Stop() // 杀失控进程;daemon 在下一唤醒重spawn(--resume)
		})
	}
	msg := claudeUserMsg(prompt)
	stdin := s.stdin
	s.mu.Unlock()
	if _, err := stdin.Write([]byte(msg)); err != nil {
		s.settle(RunResult{ExitCode: 1, Err: "failed to write turn to engine: " + err.Error(), SessionID: s.SessionID()})
	}
	return <-ch
}

func (s *claudeSession) Stop() {
	s.mu.Lock()
	s.exited = true
	if s.turnTimer != nil {
		s.turnTimer.Stop()
		s.turnTimer = nil
	}
	stdin := s.stdin
	s.stdin = nil
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	_ = killProcess(s.cmd)
}

// Steer:同轮转向——排进队列,在下一个安全 stream-json 边界(user 事件,
// 即 tool_result 回显)注入运行中 turn;无在飞 turn 时 no-op。
func (s *claudeSession) Steer(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil || !s.aliveLocked() || strings.TrimSpace(text) == "" {
		return
	}
	s.steerQueue = append(s.steerQueue, text)
}

func (s *claudeSession) flushSteerLocked() {
	if s.pending == nil || !s.aliveLocked() {
		return
	}
	for len(s.steerQueue) > 0 {
		text := s.steerQueue[0]
		s.steerQueue = s.steerQueue[1:]
		if _, err := s.stdin.Write([]byte(claudeUserMsg(text))); err != nil {
			break
		}
	}
}

func (s *claudeSession) pumpStdout(stdout io.Reader) {
	defer s.pumps.Done()
	r := bufio.NewReader(stdout)
	carry := ""
	for {
		chunk, rerr := r.ReadBytes('\n')
		if len(chunk) > 0 {
			text := carry + string(chunk)
			if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
				carry = text[idx+1:]
				for _, line := range strings.Split(text[:idx+1], "\n") {
					s.onStdoutLine(line)
				}
			} else {
				carry = text
			}
		}
		if rerr != nil {
			if carry != "" {
				s.onStdoutLine(carry)
			}
			return
		}
	}
}

func (s *claudeSession) pumpStderr(stderr io.Reader) {
	defer s.pumps.Done()
	r := bufio.NewReader(stderr)
	carry := ""
	for {
		chunk, rerr := r.ReadBytes('\n')
		if len(chunk) > 0 {
			text := carry + string(chunk)
			if idx := strings.LastIndexByte(text, '\n'); idx >= 0 {
				carry = text[idx+1:]
				for _, line := range strings.Split(text[:idx+1], "\n") {
					if cleaned := cleanLine(line); cleaned != "" {
						s.mu.Lock()
						pushTail(&s.stderrTail, cleaned)
						pushTail(&s.pendStderr, cleaned)
						onLog := s.onLog
						s.mu.Unlock()
						if onLog != nil {
							onLog(cleaned)
						}
					}
				}
			} else {
				carry = text
			}
		}
		if rerr != nil {
			if carry != "" {
				if cleaned := cleanLine(carry); cleaned != "" {
					s.mu.Lock()
					pushTail(&s.stderrTail, cleaned)
					pushTail(&s.pendStderr, cleaned)
					onLog := s.onLog
					s.mu.Unlock()
					if onLog != nil {
						onLog(cleaned)
					}
				}
			}
			return
		}
	}
}

// onStdoutLine:一行 stdout——尾巴/日志 + JSON 事件路由(session id、
// 逐跳台账、原生压缩观测、result 结算、user 边界 flushSteer)。
func (s *claudeSession) onStdoutLine(line string) {
	cleaned := cleanLine(line)
	if cleaned == "" {
		return
	}
	s.mu.Lock()
	pushTail(&s.stdoutTail, cleaned)
	pushTail(&s.pendStdout, cleaned)
	onLog := s.onLog
	s.mu.Unlock()
	if onLog != nil {
		onLog(cleaned)
	}
	if !strings.HasPrefix(cleaned, "{") {
		return
	}
	var ev struct {
		Type      string       `json:"type"`
		SessionID string       `json:"session_id"`
		IsError   bool         `json:"is_error"`
		Subtype   string       `json:"subtype"`
		Status    string       `json:"status"`
		Result    string       `json:"result"`
		Usage     *EngineUsage `json:"usage"`
		Model     string       `json:"model"`
		Message   *struct {
			Model   string       `json:"model"`
			Usage   *EngineUsage `json:"usage"`
			Content any          `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(cleaned), &ev); err != nil {
		return
	}
	s.mu.Lock()
	if ev.SessionID != "" {
		s.sid = ev.SessionID
	}
	evModel := ev.Model
	if ev.Message != nil && ev.Message.Model != "" {
		evModel = ev.Message.Model
	}
	if evModel != "" {
		s.curModel = evModel
	}
	onHop := s.onHop
	s.mu.Unlock()
	// 逐跳台账:每条 {assistant, message:{model, usage}} 是本轮一次出站
	// 模型调用;终止 result 事件带全轮总和(只作轮总账,不重复记账)。
	if ev.Type == "assistant" && ev.Message != nil && ev.Message.Usage != nil && evModel != "" && onHop != nil {
		s.mu.Lock()
		started := s.hopStart
		s.hopStart = 0
		s.hopIndex++
		s.mu.Unlock()
		toolUses, textChars := countAssistantContent(ev.Message.Content)
		hop := HopReport{Model: evModel, Usage: *ev.Message.Usage, HopIndex: s.hopIndex, ToolUses: toolUses, TextChars: textChars}
		if started > 0 {
			v := nowMS() - started
			hop.LatencyMS = &v
		}
		onHop(hop) // 台账尽力而为,不打断流
	} else {
		s.mu.Lock()
		if s.hopStart == 0 && (ev.Type == "assistant" || ev.Type == "user" || ev.Type == "system") {
			s.hopStart = nowMS()
		}
		s.mu.Unlock()
	}
	// 原生自动压缩观测(telemetry)。
	if ev.Subtype == "status" && ev.Status == "compacting" && onLog != nil {
		onLog("[claude] native context compaction started")
	} else if ev.Subtype == "compact_boundary" && onLog != nil {
		onLog("[claude] native context compaction finished")
	}
	if ev.Type == "result" {
		s.mu.Lock()
		s.hopStart = 0 // 轮界——丢弃半设的 hop 计时
		s.hopIndex = 0
		s.steerQueue = nil // 轮将终——未 flush 的 steer 落到 daemon 的合并重跑
		usage := ev.Usage
		sid := s.sid
		model := s.curModel
		s.mu.Unlock()
		res := RunResult{SessionID: sid, Usage: usage, Model: model}
		if ev.IsError {
			res.ExitCode = 1
			detail := "see log"
			if ev.Result != "" {
				detail = ev.Result
				if len(detail) > maxFailureChars {
					detail = detail[:maxFailureChars]
				}
			}
			sub := ""
			if ev.Subtype != "" {
				sub = " (" + ev.Subtype + ")"
			}
			res.Err = "engine turn error" + sub + ": " + detail
		}
		s.settle(res)
	} else if ev.Type == "user" {
		// tool_result 回显是安全 stream-json 边界(不在签名思考块中)→
		// 把排队的 steering 注入这轮运行中的 turn。
		s.mu.Lock()
		s.flushSteerLocked()
		s.mu.Unlock()
	}
}

// settle:结算在飞 turn(幂等;无在飞则丢弃)。
func (s *claudeSession) settle(r RunResult) {
	s.mu.Lock()
	timer := s.turnTimer
	s.turnTimer = nil
	ch := s.pending
	s.pending = nil
	s.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if ch != nil {
		ch <- r
	}
}

// die:进程死亡——标记死亡并 fail 在飞 turn。空闲死亡也留痕(整队会话
// 消失不能只有下一唤醒的 respawn 一行证据)。
func (s *claudeSession) die(code int, why string) {
	s.mu.Lock()
	alreadyDown := s.exited
	s.exited = true
	s.exitCode = code
	onLog := s.onLog
	pendStderr, pendStdout := s.pendStderr, s.pendStdout
	ch := s.pending
	s.pending = nil
	sid := s.sid
	s.mu.Unlock()
	if !alreadyDown && onLog != nil {
		if ch != nil {
			onLog(fmt.Sprintf("[session] engine process died MID-TURN: %s (exit %d)", why, code))
		} else {
			onLog(fmt.Sprintf("[session] engine process died while idle: %s (exit %d)", why, code))
		}
	}
	if ch != nil {
		detail := failurePreview(code, "", pendStderr, pendStdout)
		if detail == "" {
			detail = why
		}
		ch <- RunResult{ExitCode: code, Err: detail, SessionID: sid}
	}
}

func (s *claudeSession) waitExit() {
	s.pumps.Wait()
	err := s.cmd.Wait()
	code, why := 1, "process gone"
	if err == nil {
		code, why = 0, "exited with code 0"
	} else if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		if code < 0 {
			sig := signalNameOf(ee)
			code = 128
			why = "terminated by " + sig
		} else {
			why = fmt.Sprintf("exited with code %d", code)
		}
	} else {
		why = err.Error()
	}
	s.die(code, why)
}

/* ───────── 适配器 ───────── */

type claudeAdapter struct{}

func init() { RegisterAdapter(claudeAdapter{}) }

func (claudeAdapter) ID() string  { return "claude" }
func (claudeAdapter) Bin() string { return "claude" }

// SeedHome:ensureCommonHome + .claude/skills + CLAUDE.md(系统属主文件,
// 每次 start/重启重写——操作者在 Cumora 改人格就要生效)+ settings.json
// (仅缺失时写,留给用户自定义)。
func (claudeAdapter) SeedHome(home string, p Persona) error {
	if err := ensureCommonHome(home); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(home, "CLAUDE.md"), []byte(personaHeader(p, "CLAUDE.md", ".claude/skills/")), 0o644); err != nil {
		return err
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	if !pathExists(settings) {
		content := `{
  "permissions": {
    "allow": [
      "Bash"
    ]
  }
}`
		if err := os.WriteFile(settings, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Classify:小脑补全——claude -p 在自家廉价快模型(Haiku)上跑,无工具
// 无 MCP 无会话(--strict-mcp-config)、思考关、中性 cwd。--output-format
// json 的 result 信封带 token usage → 解包 .result 为文本、.usage 上送。
func (claudeAdapter) Classify(ctx context.Context, args ClassifyArgs) ClassifyResult {
	flags := extraArgs("CUMORA_TRIAGE_ARGS")
	model := args.Model
	if model == "" {
		model = "haiku"
	}
	plan := resolveSpawn("claude")
	usingJSON := len(flags) == 0
	var argv []string
	if len(flags) > 0 {
		argv = append(append([]string{}, flags...), "-p")
		if !plan.wantsStdinPrompt {
			argv = append(argv, args.Prompt)
		}
	} else {
		base := []string{"-p", "--model", model, "--output-format", "json", "--dangerously-skip-permissions", "--strict-mcp-config"}
		if plan.wantsStdinPrompt {
			argv = base
		} else {
			argv = append([]string{"-p", args.Prompt}, base[1:]...)
		}
	}
	env := withEnvDefault(args.Env, "MAX_THINKING_TOKENS=0")
	stdinText := ""
	if plan.wantsStdinPrompt {
		stdinText = args.Prompt
	}
	res := spawnCapture(ctx, plan, argv, args.Cwd, env, args.OnLog, stdinText)
	if res.Err != "" || !usingJSON {
		return res
	}
	var obj struct {
		Result *string      `json:"result"`
		Usage  *EngineUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(res.Text), &obj); err != nil {
		return res // 不是预期信封——原文返回
	}
	if obj.Result != nil {
		res.Text = *obj.Result // 含空串(TS obj.result 语义)
	}
	res.Usage = obj.Usage
	return res
}

// Probe:doctor 一档探针。small → haiku(小脑);big → 不加 --model(CLI
// 默认主脑)。一个 token 进、"OK" 出——零工具/零 MCP/零人格。
func (claudeAdapter) Probe(ctx context.Context, args ProbeArgs) ClassifyResult {
	plan := resolveSpawn("claude")
	base := []string{"-p", "--output-format", "text", "--dangerously-skip-permissions", "--strict-mcp-config"}
	if args.Tier == "small" {
		base = append([]string{"-p", "--model", triageModel("haiku")}, base[1:]...)
	}
	var argv []string
	stdinText := ""
	if plan.wantsStdinPrompt {
		argv = base
		stdinText = doctorPrompt
	} else {
		argv = append([]string{"-p", doctorPrompt}, base[1:]...)
	}
	env := withEnvDefault(args.Env, "MAX_THINKING_TOKENS=0")
	return spawnCapture(ctx, plan, argv, args.Cwd, env, nil, stdinText)
}

// ProbeWake:持久会话路径的真实断点在 --append-system-prompt-file 改名/
// 改拼法——用空文件跑一次 -p 即可检验 flag 接受度,无需完整 stream-json
// 往返。CUMORA_CLAUDE_ARGS 覆盖时 startSession 返回 nil(唤醒折叠为
// 一次性路径),此处标记 skipped。
func (claudeAdapter) ProbeWake(ctx context.Context, args WakeProbeArgs) WakeProbeResult {
	if len(extraArgs("CUMORA_CLAUDE_ARGS")) > 0 {
		return WakeProbeResult{OK: true, Skipped: true}
	}
	promptFile := filepath.Join(args.Cwd, ".cumora-doctor-standing.md")
	if err := os.WriteFile(promptFile, nil, 0o644); err != nil {
		return WakeProbeResult{Detail: "could not write standing-prompt probe file: " + err.Error()}
	}
	plan := resolveSpawn("claude")
	base := []string{"-p", "--output-format", "text", "--append-system-prompt-file", promptFile, "--dangerously-skip-permissions"}
	var argv []string
	stdinText := ""
	if plan.wantsStdinPrompt {
		argv = base
		stdinText = doctorPrompt
	} else {
		argv = append([]string{"-p", doctorPrompt}, base[1:]...)
	}
	env := withEnvDefault(args.Env, "MAX_THINKING_TOKENS=0")
	r := spawnCapture(ctx, plan, argv, args.Cwd, env, nil, stdinText)
	if r.Err != "" || strings.TrimSpace(r.Text) == "" {
		detail := strings.TrimSpace(r.Err + "\n" + r.Text)
		if detail == "" {
			detail = "no output"
		}
		return WakeProbeResult{Detail: salientError(detail)}
	}
	return WakeProbeResult{OK: true}
}

// Run:一次性 turn。-p + stream-json + verbose(行事件可日志)、
// --dangerously-skip-permissions(隔离 home、用户自有,非交互工具即意图)、
// --resume 跨轮续命;大脑 --model、小脑 ANTHROPIC_SMALL_FAST_MODEL;
// MAX_THINKING_TOKENS=0 默认(反应式短循环,扩展思考只添延迟与成本)。
func (claudeAdapter) Run(ctx context.Context, args RunArgs) RunResult {
	flags := extraArgs("CUMORA_CLAUDE_ARGS")
	plan := resolveSpawn("claude")
	var argv []string
	stdinText := ""
	if len(flags) > 0 {
		argv = append(append([]string{}, flags...), args.ResumeFlag()...)
		argv = append(argv, "-p")
		if !plan.wantsStdinPrompt {
			argv = append(argv, args.Prompt)
		} else {
			stdinText = args.Prompt
		}
	} else {
		base := []string{"-p"}
		if args.ResumeSessionID != "" {
			base = append(base, "--resume", args.ResumeSessionID)
		}
		if args.Model != "" {
			base = append(base, "--model", args.Model)
		}
		base = append(base, "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions")
		if plan.wantsStdinPrompt {
			argv = base
			stdinText = args.Prompt
		} else {
			argv = append([]string{"-p", args.Prompt}, base[1:]...)
		}
	}
	args.Env = claudeRunEnv(args)
	return spawnEngine(ctx, plan, argv, args, stdinText)
}

// claudeRunEnv:MAX_THINKING_TOKENS 缺省 0(用户可自设覆盖)+ 小脑模型。
func claudeRunEnv(args RunArgs) []string {
	env := append([]string{}, args.Env...)
	hasMax := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "MAX_THINKING_TOKENS=") {
			hasMax = true
			break
		}
	}
	if !hasMax {
		env = append(env, "MAX_THINKING_TOKENS=0")
	}
	if args.FastModel != "" {
		env = append(env, "ANTHROPIC_SMALL_FAST_MODEL="+args.FastModel)
	}
	return env
}

// ResumeFlag:辅助拼 --resume(仅一次性 run 用)。
func (r RunArgs) ResumeFlag() []string {
	if r.ResumeSessionID == "" {
		return nil
	}
	return []string{"--resume", r.ResumeSessionID}
}

// StartSession:持久会话。CUMORA_CLAUDE_ARGS 覆盖时返回 nil(那些参数
// 面向一次性 run,回退之)。standing prompt 写进 home 文件经
// --append-system-prompt-file 一次性带外投递(不逐轮重发——transcript
// 保持够小,原生自动压缩才跟得上);写失败则不带,daemon 逐轮内联。
func (claudeAdapter) StartSession(args SessionArgs) EngineSession {
	if len(extraArgs("CUMORA_CLAUDE_ARGS")) > 0 {
		return nil
	}
	argv := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if args.ResumeSessionID != "" {
		argv = append(argv, "--resume", args.ResumeSessionID)
	}
	carriesStanding := false
	if args.StandingPrompt != "" {
		file := filepath.Join(args.Home, ".cumora-standing-prompt.md")
		if err := os.WriteFile(file, []byte(args.StandingPrompt), 0o600); err == nil {
			argv = append(argv, "--append-system-prompt-file", file)
			carriesStanding = true
		}
	}
	if args.Model != "" {
		argv = append(argv, "--model", args.Model)
	}
	argv = append(argv, "--dangerously-skip-permissions")
	env := claudeRunEnv(RunArgs{Env: args.Env, FastModel: args.FastModel})
	return newClaudeSession("claude", argv, SessionArgs{
		Home: args.Home, Env: env, ResumeSessionID: args.ResumeSessionID,
		OnLog: args.OnLog, OnHopUsage: args.OnHopUsage,
	}, carriesStanding)
}
