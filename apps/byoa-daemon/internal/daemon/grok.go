// daemon 包 grok —— Grok Build 适配器(#66):一次性 run(-p
// streaming-messages-json)+ 持久 ACP stdio 会话(grok agent
// --always-approve stdio:initialize → session/new|load[standing prompt 经
// _meta.rules] → 每唤醒 session/prompt)。ACP 面无 mid-turn steer——Steer 为
// 一次性告警的 no-op,ping 落下一轮合并。对齐 engine.ts GrokSession/
// GrokAdapter(1508–1933)。
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
	"strings"
	"sync"
	"sync/atomic"
)

// resolveGrokBin:官方安装躺在 ~/.grok/bin(PATH 常因 launchd/login-shell
// 错配而不含它);PATH 优先。
func resolveGrokBin(env []string) string {
	getenv := func(key string) string {
		for i := len(env) - 1; i >= 0; i-- {
			if strings.HasPrefix(env[i], key+"=") {
				return env[i][len(key)+1:]
			}
		}
		return ""
	}
	name := "grok"
	if isWindows() {
		name = "grok.exe"
	}
	for _, dir := range filepath.SplitList(getenv("PATH")) {
		if dir == "" {
			continue
		}
		if candidate := filepath.Join(dir, name); pathExists(candidate) {
			return candidate
		}
		if isWindows() {
			if candidate := filepath.Join(dir, "grok"); pathExists(candidate) {
				return candidate
			}
		}
	}
	home := getenv("HOME")
	if home == "" {
		home = homeDir()
	}
	homeBin := filepath.Join(home, ".grok", "bin", name)
	if pathExists(homeBin) {
		return homeBin
	}
	return ""
}

type acpMsg struct {
	ID     *int64 `json:"id"`
	Method string `json:"method"`
	Result *struct {
		SessionID any `json:"sessionId"`
		Usage     any `json:"usage"`
		Meta      *struct {
			Usage any `json:"usage"`
		} `json:"_meta"`
	} `json:"result"`
	Error *struct {
		Message any `json:"message"`
	} `json:"error"`
	Params map[string]any `json:"params"`
}

func (m *acpMsg) errMessage() string {
	if m.Error == nil {
		return ""
	}
	if s, ok := m.Error.Message.(string); ok {
		return s
	}
	return fmt.Sprint(m.Error.Message)
}

// extractAcpUsage:session/prompt 应答的 usage(result.usage 或
// result._meta.usage;snake/camel 双命名兼容)。input/output 皆缺 → nil。
func extractAcpUsage(result *struct {
	SessionID any `json:"sessionId"`
	Usage     any `json:"usage"`
	Meta      *struct {
		Usage any `json:"usage"`
	} `json:"_meta"`
}) *EngineUsage {
	if result == nil {
		return nil
	}
	raw, ok := result.Usage.(map[string]any)
	if !ok && result.Meta != nil {
		raw, _ = result.Meta.Usage.(map[string]any)
	}
	if raw == nil {
		return nil
	}
	numOr := func(keys ...string) *int64 {
		for _, k := range keys {
			if f, ok := raw[k].(float64); ok && f == f && f < 1<<62 && f > -(1<<62) {
				return int64p(int64(f))
			}
		}
		return nil
	}
	u := &EngineUsage{
		InputTokens:              numOr("input_tokens", "inputTokens"),
		OutputTokens:             numOr("output_tokens", "outputTokens"),
		CacheReadInputTokens:     numOr("cache_read_input_tokens", "cacheReadInputTokens"),
		CacheCreationInputTokens: numOr("cache_creation_input_tokens", "cacheCreationInputTokens"),
	}
	if u.InputTokens == nil && u.OutputTokens == nil {
		return nil
	}
	return u
}

// grokSession:ACP stdio 上的持久会话。
type grokSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	onLog func(string)
	onHop func(HopReport)

	mu               sync.Mutex
	sid              string
	model            string // pin(可空)
	curModel         string // ACP 流实际播报的模型(_x.ai/models/update)
	sessionNewParams map[string]any
	sessionReqID     *int64
	sessionWasLoad   bool
	initializeID     *int64
	ready            bool
	exited           bool
	exitCode         int
	reqID            int64
	pending          chan RunResult
	pendingID        *int64
	pendingStart     int64
	queuedPrompt     string
	steerWarned      bool
	carriesStanding  bool
	pumps            sync.WaitGroup
}

func newGrokSession(bin string, spawnArgs []string, args SessionArgs) *grokSession {
	meta := map[string]any{"yoloMode": true}
	if args.StandingPrompt != "" {
		meta["rules"] = args.StandingPrompt
	}
	s := &grokSession{
		onLog:            args.OnLog,
		onHop:            args.OnHopUsage,
		sid:              args.ResumeSessionID,
		model:            args.Model,
		carriesStanding:  args.StandingPrompt != "",
		sessionNewParams: map[string]any{"cwd": args.Home, "mcpServers": []any{}, "_meta": meta},
		sessionWasLoad:   args.ResumeSessionID != "",
	}
	cmd := exec.Command(bin, spawnArgs...)
	cmd.Dir = args.Home
	cmd.Env = withEnvDefault(args.Env, "GROK_DISABLE_AUTOUPDATER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		s.die(1, err.Error())
		return s
	}
	s.cmd = cmd
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
	go func() {
		defer s.pumps.Done()
		r := bufio.NewReader(stderr)
		for {
			line, rerr := r.ReadString('\n')
			if c := cleanLine(line); c != "" && s.onLog != nil {
				s.onLog(c)
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		s.pumps.Wait()
		if err := s.cmd.Wait(); err != nil {
			code, why := 1, err.Error()
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
				if code < 0 {
					code, why = 128, "terminated by "+signalNameOf(ee)
				} else {
					why = fmt.Sprintf("exited with code %d", code)
				}
			}
			s.die(code, why)
		} else {
			s.die(0, "exited with code 0")
		}
	}()
	s.mu.Lock()
	// initializeID 必须取自写出的请求(两次 nextID 即差一)。
	s.initializeID = int64p(s.writeReqLocked("initialize", map[string]any{
		"protocolVersion":    1,
		"clientInfo":         map[string]any{"name": "cumora-daemon", "version": "1.0.0"},
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}},
	}))
	s.mu.Unlock()
	return s
}

func (s *grokSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aliveLocked()
}

func (s *grokSession) aliveLocked() bool { return !s.exited && s.stdin != nil }

func (s *grokSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

func (s *grokSession) CarriesStandingPrompt() bool { return s.carriesStanding }

func (s *grokSession) Send(prompt string) RunResult {
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
		sid := s.sid
		s.mu.Unlock()
		return RunResult{ExitCode: exitCode, Err: "engine session is not alive (process gone)", SessionID: sid}
	}
	ch := make(chan RunResult, 1)
	s.pending = ch
	s.pendingStart = nowMS()
	if s.ready && s.sid != "" {
		s.startPromptLocked(prompt)
	} else {
		s.queuedPrompt = prompt
	}
	s.mu.Unlock()
	return <-ch
}

// Steer:ACP session/prompt 单飞——中轮注入会取消运行中的轮。ping 落下一轮
// 合并;首次调用留一条告警(后续静默)。
func (s *grokSession) Steer(text string) {
	_ = text
	s.mu.Lock()
	warned := s.steerWarned
	onLog := s.onLog
	s.steerWarned = true
	s.mu.Unlock()
	if !warned && onLog != nil {
		onLog("[grok] same-turn steer is not supported on ACP stdio — the ping rides the next wake")
	}
}

func (s *grokSession) Stop() {
	s.mu.Lock()
	s.exited = true
	stdin := s.stdin
	s.stdin = nil
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	_ = killProcess(s.cmd)
}

func (s *grokSession) nextIDLocked() int64 { s.reqID++; return s.reqID }

func (s *grokSession) writeJSONLocked(v any) {
	if s.stdin == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = s.stdin.Write(append(b, '\n'))
}

func (s *grokSession) writeReqLocked(method string, params map[string]any) int64 {
	id := s.nextIDLocked()
	s.writeJSONLocked(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id
}

func (s *grokSession) startPromptLocked(prompt string) {
	if s.sid == "" || s.pending == nil {
		return
	}
	id := s.writeReqLocked("session/prompt", map[string]any{
		"sessionId": s.sid,
		"prompt":    []any{map[string]any{"type": "text", "text": stripLoneSurrogates(prompt)}},
	})
	s.pendingID = &id
}

func (s *grokSession) pumpStdout(stdout io.Reader) {
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
					s.onLine(line)
				}
			} else {
				carry = text
			}
		}
		if rerr != nil {
			if carry != "" {
				s.onLine(carry)
			}
			return
		}
	}
}

func (s *grokSession) onLine(line string) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "{") {
		if c := cleanLine(line); c != "" && s.onLog != nil {
			s.onLog(c)
		}
		return
	}
	var msg acpMsg
	if json.Unmarshal([]byte(t), &msg) != nil {
		if c := cleanLine(line); c != "" && s.onLog != nil {
			s.onLog(c)
		}
		return
	}
	s.mu.Lock()
	effects := s.handleLocked(&msg)
	s.mu.Unlock()
	for _, fn := range effects {
		fn()
	}
}

// handleLocked:全程持锁的状态机;日志/逐跳等副作用以 effects 返回,
// 调用方解锁后执行(与 codex 会话同型的锁外回调纪律)。
func (s *grokSession) handleLocked(msg *acpMsg) []func() {
	var effects []func()
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		effects = append(effects, func() {
			if s.onLog != nil {
				s.onLog(line)
			}
		})
	}
	if msg.ID != nil && s.initializeID != nil && *msg.ID == *s.initializeID {
		s.initializeID = nil
		if msg.Error != nil {
			s.failPendingLocked(acpErrMsg(msg.errMessage(), "grok initialize failed"))
			return effects
		}
		if s.sessionWasLoad && s.sid != "" {
			params := map[string]any{"sessionId": s.sid}
			for k, v := range s.sessionNewParams {
				params[k] = v
			}
			id := s.writeReqLocked("session/load", params)
			s.sessionReqID = &id
		} else {
			id := s.writeReqLocked("session/new", s.sessionNewParams)
			s.sessionReqID = &id
		}
		return effects
	}
	if msg.Error != nil && msg.ID != nil && s.sessionReqID != nil && *msg.ID == *s.sessionReqID {
		if s.sessionWasLoad {
			logf("[grok] session/load failed (%s) — starting a fresh session", msg.errMessage())
			s.sessionWasLoad = false
			s.sid = ""
			id := s.writeReqLocked("session/new", s.sessionNewParams)
			s.sessionReqID = &id
			return effects
		}
		s.failPendingLocked(acpErrMsg(msg.errMessage(), "grok session start failed"))
		return effects
	}
	if msg.ID != nil && s.sessionReqID != nil && *msg.ID == *s.sessionReqID && msg.Result != nil {
		if sid, ok := msg.Result.SessionID.(string); ok && sid != "" {
			s.sid = sid
		}
		s.sessionReqID = nil
		s.ready = true
		if s.queuedPrompt != "" && s.pending != nil {
			p := s.queuedPrompt
			s.queuedPrompt = ""
			s.startPromptLocked(p)
		}
		return effects
	}
	// Grok 在此播报在跑模型(及会话中途切换)——没有它,每轮台账都按一个
	// 通常缺席的 pin 定价(curModel 语义同 ClaudeSession 嗅探 message.model)。
	if msg.Method == "_x.ai/models/update" {
		if id, ok := msg.Params["currentModelId"].(string); ok && id != "" {
			s.curModel = id
		}
		return effects
	}
	if msg.Method == "session/update" {
		update, _ := msg.Params["update"].(map[string]any)
		if update == nil {
			update, _ = msg.Params["sessionUpdate"].(map[string]any)
		}
		if update == nil {
			update = msg.Params
		}
		kind, _ := update["sessionUpdate"].(string)
		if kind == "" {
			kind, _ = msg.Params["sessionUpdate"].(string)
		}
		switch kind {
		case "tool_call":
			if title, ok := update["title"].(string); ok {
				logf("[grok] tool %s", title)
			}
		case "agent_message_chunk":
			if content, ok := update["content"].(map[string]any); ok {
				if text, ok := content["text"].(string); ok && strings.TrimSpace(text) != "" {
					logf("[grok] » %s", collapseWS(text, 200))
				}
			}
		}
		return effects
	}
	if s.pending != nil && s.pendingID != nil && msg.ID != nil && *msg.ID == *s.pendingID {
		if msg.Error != nil {
			s.failPendingLocked(acpErrMsg(msg.errMessage(), "grok prompt failed"))
			return effects
		}
		usage := acpUsageOf(msg.Result)
		hopModel := s.curModel
		if hopModel == "" {
			hopModel = s.model
		}
		if hopModel == "" {
			hopModel = "grok"
		}
		resModel := s.curModel
		if resModel == "" {
			resModel = s.model
		}
		sid := s.sid
		if s.onHop != nil && usage != nil {
			u := *usage
			latency := nowMS() - s.pendingStart
			hopFn, onHop := s.onHop, HopReport{Model: hopModel, Usage: u, LatencyMS: &latency, HopIndex: 1}
			effects = append(effects, func() { hopFn(onHop) })
		} else if s.onHop != nil {
			// usage 缺席也发跳(usage 空对象)——TS usage ?? {} 同型。
			latency := nowMS() - s.pendingStart
			hopFn, onHop := s.onHop, HopReport{Model: hopModel, Usage: EngineUsage{}, LatencyMS: &latency, HopIndex: 1}
			effects = append(effects, func() { hopFn(onHop) })
		}
		s.pendingID = nil
		ch := s.pending
		s.pending = nil
		effects = append(effects, func() {
			ch <- RunResult{ExitCode: 0, SessionID: sid, Usage: usage, Model: resModel}
		})
		return effects
	}
	if msg.Error != nil && msg.ID != nil {
		s.failPendingLocked(acpErrMsg(msg.errMessage(), "grok acp request failed"))
	}
	return effects
}

func acpUsageOf(result *struct {
	SessionID any `json:"sessionId"`
	Usage     any `json:"usage"`
	Meta      *struct {
		Usage any `json:"usage"`
	} `json:"_meta"`
}) *EngineUsage {
	return extractAcpUsage(result)
}

func acpErrMsg(msg, fallback string) string {
	if msg != "" {
		return msg
	}
	return fallback
}

func (s *grokSession) failPendingLocked(errMsg string) {
	if s.pending != nil {
		s.pendingID = nil
		ch := s.pending
		s.pending = nil
		sid := s.sid
		go func() { ch <- RunResult{ExitCode: 1, Err: errMsg, SessionID: sid} }()
	} else if s.onLog != nil {
		line := "[grok] " + errMsg
		go s.onLog(line)
	}
}

func (s *grokSession) die(code int, why string) {
	s.mu.Lock()
	alreadyDown := s.exited
	s.exited = true
	s.exitCode = code
	onLog := s.onLog
	ch := s.pending
	s.pending = nil
	s.pendingID = nil
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
		ch <- RunResult{ExitCode: code, Err: why, SessionID: sid}
	}
}

/* ───────── 适配器 ───────── */

type grokAdapter struct{}

func init() { RegisterAdapter(grokAdapter{}) }

func (grokAdapter) ID() string  { return "grok" }
func (grokAdapter) Bin() string { return "grok" }

func (grokAdapter) command(env []string) string {
	if b := resolveGrokBin(env); b != "" {
		return b
	}
	return "grok"
}

// Classify:grok-4.5 小脑;--output-format json 的 {text, usage} 信封解包。
func (a grokAdapter) Classify(ctx context.Context, args ClassifyArgs) ClassifyResult {
	flags := extraArgs("CUMORA_TRIAGE_ARGS")
	model := args.Model
	if model == "" {
		model = "grok-4.5"
	}
	plan := resolveSpawn("grok")
	plan.command = a.command(args.Env) // 解析到的 ~/.grok/bin 路径优先于 PATH
	usingJSON := len(flags) == 0
	var argv []string
	stdinText := ""
	if len(flags) > 0 {
		argv = append(append([]string{}, flags...), "-p")
		if !plan.wantsStdinPrompt {
			argv = append(argv, args.Prompt)
		} else {
			stdinText = args.Prompt
		}
	} else {
		base := []string{"-p", "--model", model, "--output-format", "json", "--always-approve", "--no-auto-update"}
		if plan.wantsStdinPrompt {
			argv, stdinText = base, args.Prompt
		} else {
			argv = append([]string{"-p", args.Prompt}, base[1:]...)
		}
	}
	res := spawnCapture(ctx, plan, argv, args.Cwd, withEnvDefault(args.Env, "GROK_DISABLE_AUTOUPDATER=1"), args.OnLog, stdinText)
	if res.Err != "" || !usingJSON {
		return res
	}
	var obj struct {
		Text  *string      `json:"text"`
		Usage *EngineUsage `json:"usage"`
	}
	if json.Unmarshal([]byte(res.Text), &obj) != nil {
		return res
	}
	if obj.Text != nil {
		res.Text = *obj.Text
	}
	res.Usage = obj.Usage
	return res
}

// Probe:small → grok-4.5;big → CLI 默认。
func (a grokAdapter) Probe(ctx context.Context, args ProbeArgs) ClassifyResult {
	plan := resolveSpawn("grok")
	plan.command = a.command(args.Env)
	base := []string{"-p", "--output-format", "json", "--always-approve", "--no-auto-update"}
	if args.Tier == "small" {
		base = append([]string{"-p", "--model", "grok-4.5"}, base[1:]...)
	}
	var argv []string
	stdinText := ""
	if plan.wantsStdinPrompt {
		argv, stdinText = base, doctorPrompt
	} else {
		argv = append([]string{"-p", doctorPrompt}, base[1:]...)
	}
	return spawnCapture(ctx, plan, argv, args.Cwd, withEnvDefault(args.Env, "GROK_DISABLE_AUTOUPDATER=1"), nil, stdinText)
}

// ProbeWake:与 startSession 同门(参数覆盖/显式退出/Windows 折叠为一次性
// -p,probe 已覆盖);否则驱动 ACP 最小握手(initialize→session/new)。
func (a grokAdapter) ProbeWake(ctx context.Context, args WakeProbeArgs) WakeProbeResult {
	if len(extraArgs("CUMORA_GROK_ARGS")) > 0 || os.Getenv("CUMORA_GROK_NO_ACP") == "1" || isWindows() {
		return WakeProbeResult{OK: true, Skipped: true}
	}
	command := a.command(args.Env)
	cmd := exec.Command(command, "agent", "--always-approve", "--no-leader", "stdio")
	cmd.Dir = args.Cwd
	cmd.Env = withEnvDefault(args.Env, "GROK_DISABLE_AUTOUPDATER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return WakeProbeResult{Detail: "stdin pipe: " + err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return WakeProbeResult{Detail: "stdout pipe: " + err.Error()}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return WakeProbeResult{Detail: "stderr pipe: " + err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return WakeProbeResult{Detail: "spawn error: " + err.Error()}
	}
	var stderrMu sync.Mutex
	var stderrBuf []byte
	type probeResult struct {
		ok     bool
		detail string
	}
	out := make(chan probeResult, 1)
	var reaped atomic.Bool
	go func() { _ = cmd.Wait() }()
	finish := func(r probeResult) {
		if reaped.Swap(true) {
			return
		}
		_ = stdin.Close()
		_ = killProcess(cmd)
		select {
		case out <- r:
		default:
		}
	}
	writeRpc := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = stdin.Write(append(b, '\n'))
	}
	writeRpc(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": 1, "clientInfo": map[string]any{"name": "cumora-doctor", "version": "1.0.0"},
			"clientCapabilities": map[string]any{},
		}})
	go func() {
		r := bufio.NewReader(stdout)
		carry := ""
		initialized := false
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
					t := strings.TrimSpace(line)
					if t == "" || !strings.HasPrefix(t, "{") {
						continue
					}
					var msg acpMsg
					if json.Unmarshal([]byte(t), &msg) != nil {
						continue
					}
					if msg.Error != nil && msg.errMessage() != "" {
						detail := msg.errMessage()
						if len(detail) > 240 {
							detail = truncateRunes(detail, 240)
						}
						finish(probeResult{detail: "acp rejected handshake: " + detail})
						return
					}
					if !initialized && msg.ID != nil && *msg.ID == 1 && msg.Result != nil {
						initialized = true
						writeRpc(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new",
							"params": map[string]any{"cwd": args.Cwd, "mcpServers": []any{}, "_meta": map[string]any{"yoloMode": true}}})
						continue
					}
					if initialized && msg.ID != nil && *msg.ID == 2 && msg.Result != nil {
						finish(probeResult{ok: true})
						return
					}
				}
			}
			if rerr != nil {
				stage := "before session/new ack"
				if !initialized {
					stage = "before initialize ack"
				}
				stderrMu.Lock()
				salient := salientError(string(stderrBuf))
				stderrMu.Unlock()
				if salient == "" {
					salient = "no stderr"
				}
				finish(probeResult{detail: "grok agent stdio died " + stage + ": " + salient})
				return
			}
		}
	}()
	go func() {
		b := make([]byte, 4096)
		for {
			n, rerr := stderr.Read(b)
			if n > 0 {
				stderrMu.Lock()
				stderrBuf = append(stderrBuf, b[:n]...)
				if len(stderrBuf) > 2000 {
					stderrBuf = stderrBuf[len(stderrBuf)-2000:]
				}
				stderrMu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		finish(probeResult{detail: "aborted (timeout)"})
	}()
	r := <-out
	return WakeProbeResult{OK: r.ok, Detail: r.detail}
}

// SeedHome:AGENTS.md 仅缺失时写(write-once——grok 会把规则并入自身会话
// 状态,重写等于每轮重置;与 claude/codex/zcode 的恒重写相反)。
func (grokAdapter) SeedHome(home string, p Persona) error {
	if err := ensureCommonHome(home); err != nil {
		return err
	}
	agentsMD := filepath.Join(home, "AGENTS.md")
	if !pathExists(agentsMD) {
		return os.WriteFile(agentsMD, []byte(personaHeader(p, "CLAUDE.md", ".claude/skills/")), 0o644)
	}
	return nil
}

// Run:一次性 -p(streaming-messages-json)。
func (grokAdapter) Run(ctx context.Context, args RunArgs) RunResult {
	flags := extraArgs("CUMORA_GROK_ARGS")
	command := grokAdapter{}.command(args.Env)
	plan := spawnPlan{command: command, shell: false}
	if plan.command == "grok" {
		plan = resolveSpawn("grok")
	}
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
		base = append(base, args.ResumeFlag()...)
		if args.Model != "" {
			base = append(base, "--model", args.Model)
		}
		base = append(base, "--output-format", "streaming-messages-json", "--always-approve", "--no-auto-update")
		if plan.wantsStdinPrompt {
			argv, stdinText = base, args.Prompt
		} else {
			argv = append([]string{"-p", args.Prompt}, base[1:]...)
		}
	}
	args.Env = withEnvDefault(args.Env, "GROK_DISABLE_AUTOUPDATER=1")
	return spawnEngine(ctx, plan, argv, args, stdinText)
}

// StartSession:参数覆盖/显式退出/Windows(JSON-RPC over .cmd shim 与 codex
// 同陷阱)→ nil 降级;否则 ACP stdio 持久会话。
func (grokAdapter) StartSession(args SessionArgs) EngineSession {
	if len(extraArgs("CUMORA_GROK_ARGS")) > 0 {
		return nil
	}
	if os.Getenv("CUMORA_GROK_NO_ACP") == "1" {
		return nil
	}
	if isWindows() {
		return nil
	}
	spawnArgs := []string{"agent", "--always-approve", "--no-leader"}
	if args.Model != "" {
		spawnArgs = append(spawnArgs, "--model", args.Model)
	}
	spawnArgs = append(spawnArgs, "stdio")
	return newGrokSession(grokAdapter{}.command(args.Env), spawnArgs, args)
}
