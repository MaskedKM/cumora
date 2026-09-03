// daemon 包 acp —— ACP stdio 传输/会话基类(#268):JSON-RPC 行协议、
// initialize → session/new|load 握手(load 失败回退 new)、turn 生命周期
// (pending 通道 + #259 活性看门狗)、session/update 流事件(tool_call/
// agent_message_chunk → #260 转录)、usage 对账(snake/camel 双命名)。
// ACP 族引擎 = 一个描述符条目(acpSessionConfig):spawn 形状、session
// 参数、引擎专属通知钩子;grok 是首个消费者(grok.go 的
// grokAdapter.StartSession)。行为与 #66 的 grokSession 逐段平价——本
// 文件即其上移,差异点全部收敛在描述符。
package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// acpSessionConfig:ACP 族引擎描述符。同协议 fork(新 CLI 兼容 ACP
// stdio)= 填一张此表 + 适配器壳,不再整份手写会话机。
type acpSessionConfig struct {
	EngineID  string // 日志前缀 + RunResult.Model 兜底
	Bin       string // 解析后的可执行路径
	SpawnArgs []string
	// SessionNewParams:session/new 与 session/load 的参数(cwd/mcpServers/
	// _meta 等引擎差异全在此)。
	SessionNewParams map[string]any
	// ClientInfo:initialize 的 clientInfo(nil = cumora-daemon 默认)。
	ClientInfo map[string]any
	// SteerWarnLine:非空 = 无中轮 steer,首次 Steer 留此告警。
	SteerWarnLine string
	// OnNotify:session/update 之外的引擎专属通知(如 grok 的
	// _x.ai/models/update 播报在跑模型)。nil = 无。
	// ⚠ 在 handleLocked 持 s.mu 期间同步调用:回调内严禁调用 s 的公有
	// 方法(Send/Stop/Alive… 均取 s.mu,不可重入即自锁死)、严禁阻塞
	// (会卡死 stdout 泵)——只做纯状态写(如 s.curModel = x)。
	OnNotify func(s *acpSession, method string, params map[string]any)
	// EnvKeepKey/EnvKeepValue:替换式 env 注入默认值(withEnvDefaultKeep
	// 语义;空键 = 无)。grok:GROK_DISABLE_AUTOUPDATER=1。
	EnvKeepKey   string
	EnvKeepValue string
}

// acpSession:ACP stdio 上的持久会话(原 grokSession 上移)。
type acpSession struct {
	cfg          acpSessionConfig
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	onLog        func(string)
	onHop        func(HopReport)
	onTranscript func(TranscriptEntry) // #260 执行转录

	mu sync.Mutex
	// wd:#259 活性看门狗(空闲/工具在飞/首声三层,判死不靠墙钟)。
	// turnsDone:首声层仅首个 turn(会话级语义,大上下文 prefill 不是病)。
	wd              *activityWatchdog
	turnsDone       bool
	sid             string
	model           string // pin(可空)
	curModel        string // 流实际播报的模型(引擎通知,如 _x.ai/models/update)
	sessionReqID    *int64
	sessionWasLoad  bool
	initializeID    *int64
	ready           bool
	exited          bool
	exitCode        int
	reqID           int64
	pending         chan RunResult
	pendingID       *int64
	pendingStart    int64
	queuedPrompt    string
	steerWarned     bool
	carriesStanding bool
	pumps           sync.WaitGroup
}

func (c acpSessionConfig) logPrefix() string {
	if c.EngineID == "" {
		return "acp"
	}
	return c.EngineID
}

func newAcpSession(cfg acpSessionConfig, args SessionArgs) *acpSession {
	s := &acpSession{
		cfg:             cfg,
		onLog:           args.OnLog,
		onHop:           args.OnHopUsage,
		onTranscript:    args.OnTranscript,
		sid:             args.ResumeSessionID,
		model:           args.Model,
		carriesStanding: args.StandingPrompt != "",
		sessionWasLoad:  args.ResumeSessionID != "",
	}
	// #259:判死动作——只在真有在飞 turn 被结算时才杀进程(撞窗时健康
	// 会话不得陪葬),下一唤醒 resume。
	s.wd = newActivityWatchdog(func(reason string) {
		s.mu.Lock()
		ch := s.pending
		s.pending = nil
		s.pendingID = nil
		sid := s.sid
		s.mu.Unlock()
		if ch == nil {
			return
		}
		ch <- RunResult{ExitCode: 124, Err: reason, SessionID: sid}
		s.Stop()
	})
	cmd := exec.Command(cfg.Bin, cfg.SpawnArgs...)
	cmd.Dir = args.Home
	env := args.Env
	if cfg.EnvKeepKey != "" {
		env = withEnvDefaultKeep(env, cfg.EnvKeepKey, cfg.EnvKeepValue)
	}
	cmd.Env = env
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
			if c := cleanLine(line); c != "" {
				s.wd.Activity(false, false) // #259 stderr 出声也算活
				if s.onLog != nil {
					s.onLog(c)
				}
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
	clientInfo := cfg.ClientInfo
	if clientInfo == nil {
		clientInfo = map[string]any{"name": "cumora-daemon", "version": "1.0.0"}
	}
	s.initializeID = int64p(s.writeReqLocked("initialize", map[string]any{
		"protocolVersion":    1,
		"clientInfo":         clientInfo,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}},
	}))
	s.mu.Unlock()
	return s
}

func (s *acpSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aliveLocked()
}

func (s *acpSession) aliveLocked() bool { return !s.exited && s.stdin != nil }

func (s *acpSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

func (s *acpSession) CarriesStandingPrompt() bool { return s.carriesStanding }

func (s *acpSession) Send(prompt string) RunResult {
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
	// #259 活性看门狗开表——首声层仅首个 turn,后续轮直接空闲窗。
	if s.turnsDone {
		s.wd.ArmIdle()
	} else {
		s.turnsDone = true
		s.wd.Arm()
	}
	s.pendingStart = nowMS()
	if s.ready && s.sid != "" {
		s.startPromptLocked(prompt)
	} else {
		s.queuedPrompt = prompt
	}
	s.mu.Unlock()
	return <-ch
}

// Steer:基类无中轮注入实现。描述符给了告警行 → 首次调用留一条告警
// (ping 落下一轮合并),后续静默;没给 = 引擎自带 steer,由子类覆写。
func (s *acpSession) Steer(text string) {
	_ = text
	if s.cfg.SteerWarnLine == "" {
		return
	}
	s.mu.Lock()
	warned := s.steerWarned
	onLog := s.onLog
	s.steerWarned = true
	s.mu.Unlock()
	if !warned && onLog != nil {
		onLog(s.cfg.SteerWarnLine)
	}
}

func (s *acpSession) Stop() {
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

func (s *acpSession) nextIDLocked() int64 { s.reqID++; return s.reqID }

func (s *acpSession) writeJSONLocked(v any) {
	if s.stdin == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = s.stdin.Write(append(b, '\n'))
}

func (s *acpSession) writeReqLocked(method string, params map[string]any) int64 {
	id := s.nextIDLocked()
	s.writeJSONLocked(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id
}

func (s *acpSession) startPromptLocked(prompt string) {
	if s.sid == "" || s.pending == nil {
		return
	}
	id := s.writeReqLocked("session/prompt", map[string]any{
		"sessionId": s.sid,
		"prompt":    []any{map[string]any{"type": "text", "text": stripLoneSurrogates(prompt)}},
	})
	s.pendingID = &id
}

func (s *acpSession) pumpStdout(stdout io.Reader) {
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

func (s *acpSession) onLine(line string) {
	t := strings.TrimSpace(line)
	if c := cleanLine(line); c != "" {
		s.wd.Activity(false, false) // #259 出声即活(任意行)
	}
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
func (s *acpSession) handleLocked(msg *acpMsg) []func() {
	var effects []func()
	prefix := "[" + s.cfg.logPrefix() + "]"
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
			s.failPendingLocked(acpErrMsg(msg.errMessage(), s.cfg.logPrefix()+" initialize failed"))
			return effects
		}
		if s.sessionWasLoad && s.sid != "" {
			params := map[string]any{"sessionId": s.sid}
			for k, v := range s.cfg.SessionNewParams {
				params[k] = v
			}
			id := s.writeReqLocked("session/load", params)
			s.sessionReqID = &id
		} else {
			id := s.writeReqLocked("session/new", s.cfg.SessionNewParams)
			s.sessionReqID = &id
		}
		return effects
	}
	if msg.Error != nil && msg.ID != nil && s.sessionReqID != nil && *msg.ID == *s.sessionReqID {
		if s.sessionWasLoad {
			logf("%s session/load failed (%s) — starting a fresh session", prefix, msg.errMessage())
			s.sessionWasLoad = false
			s.sid = ""
			id := s.writeReqLocked("session/new", s.cfg.SessionNewParams)
			s.sessionReqID = &id
			return effects
		}
		s.failPendingLocked(acpErrMsg(msg.errMessage(), s.cfg.logPrefix()+" session start failed"))
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
	// 引擎专属通知(session/update 之外)——如 grok 的 _x.ai/models/update。
	// 只吞通知帧(id==nil):带 id 又带 method 的畸形帧留给后续分支,
	// 不得挡 pendingID 结算(旧实现精确匹配方法名,此处收窄为形态匹配)。
	if msg.ID == nil && msg.Method != "" && msg.Method != "session/update" && s.cfg.OnNotify != nil {
		s.cfg.OnNotify(s, msg.Method, msg.Params)
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
			s.wd.Activity(true, false) // #259 工具在飞(换 toolBudget 窗口)
			if onT := s.onTranscript; onT != nil {
				title, _ := update["title"].(string)
				entry := TranscriptEntry{Type: "tool_use", Tool: title}
				effects = append(effects, func() { onT(entry) })
			}
			if title, ok := update["title"].(string); ok {
				logf("%s tool %s", prefix, title)
			}
		case "agent_message_chunk":
			if content, ok := update["content"].(map[string]any); ok {
				if text, ok := content["text"].(string); ok && strings.TrimSpace(text) != "" {
					if onT := s.onTranscript; onT != nil {
						entry := TranscriptEntry{Type: "text", Content: text}
						effects = append(effects, func() { onT(entry) })
					}
					logf("%s » %s", prefix, collapseWS(text, 200))
				}
			}
		}
		return effects
	}
	if s.pending != nil && s.pendingID != nil && msg.ID != nil && *msg.ID == *s.pendingID {
		if msg.Error != nil {
			s.failPendingLocked(acpErrMsg(msg.errMessage(), s.cfg.logPrefix()+" prompt failed"))
			return effects
		}
		usage := acpUsageOf(msg.Result)
		hopModel := s.curModel
		if hopModel == "" {
			hopModel = s.model
		}
		if hopModel == "" {
			hopModel = s.cfg.logPrefix()
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
		s.wd.Disarm() // #259 轮结算
		effects = append(effects, func() {
			ch <- RunResult{ExitCode: 0, SessionID: sid, Usage: usage, Model: resModel}
		})
		return effects
	}
	if msg.Error != nil && msg.ID != nil {
		s.failPendingLocked(acpErrMsg(msg.errMessage(), s.cfg.logPrefix()+" acp request failed"))
	}
	return effects
}

func (s *acpSession) failPendingLocked(errMsg string) {
	s.wd.Disarm() // #259
	if s.pending != nil {
		s.pendingID = nil
		ch := s.pending
		s.pending = nil
		sid := s.sid
		go func() { ch <- RunResult{ExitCode: 1, Err: errMsg, SessionID: sid} }()
	} else if s.onLog != nil {
		line := "[" + s.cfg.logPrefix() + "] " + errMsg
		go s.onLog(line)
	}
}

func (s *acpSession) die(code int, why string) {
	s.wd.Disarm() // #259
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

// EngineSession 接口满足性:acpSession 即通用 ACP 会话。
var _ EngineSession = (*acpSession)(nil)

// acpMsg:ACP JSON-RPC 帧的宽容解析(id/result/error/params)。
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
