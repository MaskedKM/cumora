// daemon 包 codex —— Codex 适配器(#64):一次性 run(codex exec)+
// 持久会话(codex app-server --listen stdio:// 的 JSON-RPC:initialize →
// initialized → thread/start|resume(带 developerInstructions standing
// prompt)→ 每唤醒 turn/start,在 turn/completed 结算)+ 小脑 classify
// (gpt-5.4-mini)+ doctor 探针。对齐 engine.ts 的 CodexSession/CodexAdapter。
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// codexLogRaw:CUMORA_CODEX_VERBOSE=1 时原样倾倒 app-server 原始事件流
// (默认静默——那是 daemon 不需要的消防水管)。
func codexLogRaw() bool { return os.Getenv("CUMORA_CODEX_VERBOSE") == "1" }

// ensureGitRepoForCodex:Codex 要求 cwd 是 git 仓库。一次性 exec 有
// --skip-git-repo-check,app-server 没有——给 agent home 垫一个一次性仓库:
// 仅 init + 一个空提交,不 git add 任何东西(操作者的令牌/文件永不入暂存)。
// 尽力而为:失败只是 app-server 可能拒绝,startSession 回退一次性 exec。
func ensureGitRepoForCodex(home string) error {
	if pathExists(home + string(os.PathSeparator) + ".git") {
		return nil
	}
	if _, err := exec.Command("git", "-C", home, "init").Output(); err != nil {
		return err
	}
	g := []string{"-C", home, "-c", "user.name=cumora", "-c", "user.email=cumora@local", "-c", "commit.gpgsign=false"}
	cmd := append(g, "commit", "--allow-empty", "-m", "cumora init")
	if _, err := exec.Command("git", cmd...).Output(); err != nil {
		return err
	}
	return nil
}

/* ───────── JSON-RPC 消息形状 ───────── */

type codexRpcMsg struct {
	ID     *int64 `json:"id"`
	Method string `json:"method"`
	Result *struct {
		Thread *struct{ ID any } `json:"thread"`
		Turn   *struct{ ID any } `json:"turn"`
		TurnID any               `json:"turnId"`
	} `json:"result"`
	Error *struct {
		Message any `json:"message"`
	} `json:"error"`
	Params map[string]any `json:"params"`
}

func (m *codexRpcMsg) errMessage() string {
	if m.Error == nil {
		return ""
	}
	if s, ok := m.Error.Message.(string); ok {
		return s
	}
	return fmt.Sprint(m.Error.Message)
}

// codexUsageAcc:Codex 报的是线程 RUNNING 总量;每轮用量取差值。
type codexUsageAcc struct{ input, cached, output int64 }

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// codexSession:app-server JSON-RPC 上的持久会话。
type codexSession struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	onLog func(string)
	onHop func(HopReport)

	mu sync.Mutex
	// 状态(handshake 状态机)
	threadID         string
	threadWasResume  bool
	baseThreadParams map[string]any
	threadReq        *struct {
		method string
		params map[string]any
	}
	threadReqID  *int64
	initializeID *int64
	ready        bool
	handshakeErr string
	// turn 状态
	exited          bool
	exitCode        int
	reqID           int64
	pending         chan RunResult
	queuedPrompt    string
	activeTurnID    string
	steerGate       bool
	model           string
	carriesStanding bool
	turnStartedAt   int64
	cum, turnStart  codexUsageAcc
	// pumps:stdout/stderr 读者;waitExit 须等它们排干再 Wait(StdoutPipe
	// 铁律——Wait 关管道 fd 会丢内核缓冲里的尾行)。
	pumps sync.WaitGroup
}

func newCodexSession(bin string, spawnArgs []string, args SessionArgs) *codexSession {
	params := map[string]any{
		"cwd":                   args.Home,
		"approvalPolicy":        "never",
		"sandbox":               "danger-full-access",
		"experimentalRawEvents": true,
	}
	if args.StandingPrompt != "" {
		params["developerInstructions"] = args.StandingPrompt
	}
	if args.Model != "" {
		params["model"] = args.Model
	}
	s := &codexSession{
		onLog:            args.OnLog,
		onHop:            args.OnHopUsage,
		threadID:         args.ResumeSessionID,
		model:            args.Model,
		carriesStanding:  args.StandingPrompt != "",
		baseThreadParams: params,
		threadWasResume:  args.ResumeSessionID != "",
	}
	var threadReq struct {
		method string
		params map[string]any
	}
	if args.ResumeSessionID != "" {
		p := map[string]any{"threadId": args.ResumeSessionID}
		for k, v := range params {
			p[k] = v
		}
		threadReq.method, threadReq.params = "thread/resume", p
	} else {
		threadReq.method, threadReq.params = "thread/start", params
	}
	s.threadReq = &threadReq

	cmd := exec.Command(bin, spawnArgs...)
	cmd.Dir = args.Home
	cmd.Env = args.Env
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
		err := s.cmd.Wait()
		code, why := 1, "process gone"
		if err == nil {
			code, why = 0, "exited with code 0"
		} else if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			if code < 0 {
				code = 128
				why = "terminated by " + signalNameOf(ee)
			} else {
				why = fmt.Sprintf("exited with code %d", code)
			}
		} else if err != nil {
			why = err.Error()
		}
		s.die(code, why)
	}()
	// 握手即刻开始(等 handler 就绪后)。
	s.mu.Lock()
	// initializeID 必须取自写出的请求本身(nextIDLocked 自增——两次调用
	// 即差一,应答永远配不上对)。
	s.initializeID = int64p(s.writeReqLocked("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "cumora-daemon", "version": "1.0.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}))
	s.mu.Unlock()
	return s
}

func int64p(v int64) *int64 { return &v }

func (s *codexSession) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aliveLocked()
}

// aliveLocked:调用方已持锁时的活性判定(Alive 不可重入——Send 的持锁
// 路径曾因此自死锁)。
func (s *codexSession) aliveLocked() bool { return !s.exited && s.stdin != nil }

func (s *codexSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

func (s *codexSession) CarriesStandingPrompt() bool { return s.carriesStanding }

// Send:喂一轮,阻塞到 turn/completed/进程死亡/握手失败。
func (s *codexSession) Send(prompt string) RunResult {
	s.mu.Lock()
	if s.pending != nil {
		s.mu.Unlock()
		return RunResult{ExitCode: 1, Err: "engine session busy — a turn is already in flight", SessionID: s.threadID}
	}
	if !s.aliveLocked() {
		exitCode := s.exitCode
		if exitCode == 0 {
			exitCode = 1
		}
		err := "engine session is not alive (process gone)"
		if s.handshakeErr != "" {
			err = s.handshakeErr
		}
		s.mu.Unlock()
		return RunResult{ExitCode: exitCode, Err: err, SessionID: s.threadID}
	}
	ch := make(chan RunResult, 1)
	s.pending = ch
	s.turnStart = s.cum
	if s.ready && s.threadID != "" {
		s.startTurnLocked(prompt)
	} else {
		s.queuedPrompt = prompt
	}
	s.mu.Unlock()
	return <-ch
}

// Steer:turn/steer 注入运行中的 turn(expectedTurnId 防错轮);仅在安全
// 边界(item 完成)与已知 turnId 时生效,尽力而为。
func (s *codexSession) Steer(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || strings.TrimSpace(text) == "" || s.threadID == "" || s.activeTurnID == "" || s.steerGate {
		return
	}
	input := map[string]any{
		"jsonrpc": "2.0",
		"id":      s.nextIDLocked(),
		"method":  "turn/steer",
		"params": map[string]any{
			"threadId":       s.threadID,
			"expectedTurnId": s.activeTurnID,
			"input":          []any{map[string]any{"type": "text", "text": stripLoneSurrogates(text)}},
		},
	}
	s.writeJSONLocked(input)
}

func (s *codexSession) Stop() {
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

func (s *codexSession) nextIDLocked() int64 {
	s.reqID++
	return s.reqID
}

func (s *codexSession) writeJSONLocked(v any) {
	if s.stdin == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = s.stdin.Write(append(b, '\n'))
}

func (s *codexSession) writeReqLocked(method string, params map[string]any) int64 {
	id := s.nextIDLocked()
	s.writeJSONLocked(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return id
}

func (s *codexSession) notifyLocked(method string, params map[string]any) {
	s.writeJSONLocked(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *codexSession) startTurnLocked(prompt string) {
	if s.threadID == "" {
		return
	}
	s.turnStartedAt = nowMS()
	s.writeReqLocked("turn/start", map[string]any{
		"threadId": s.threadID,
		"input":    []any{map[string]any{"type": "text", "text": stripLoneSurrogates(prompt)}},
	})
}

func (s *codexSession) pumpStdout(stdout io.Reader) {
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

func (s *codexSession) onLine(line string) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "{") {
		if c := cleanLine(line); c != "" && s.onLog != nil {
			s.onLog(c)
		}
		return
	}
	var msg codexRpcMsg
	if err := json.Unmarshal([]byte(t), &msg); err != nil {
		if c := cleanLine(line); c != "" && s.onLog != nil {
			s.onLog(c)
		}
		return
	}
	// 原始事件流默认不进日志(daemon 不需要;handle 提炼的信号行足够);
	// CUMORA_CODEX_VERBOSE=1 全量倾倒。
	if codexLogRaw() {
		if c := cleanLine(line); c != "" && s.onLog != nil {
			s.onLog(c)
		}
	}
	s.handle(&msg)
}

func anyString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok // TS typeof === 'string':空串也算(逐语义对齐)
}

// handle:持锁跑状态机,解锁后执行收集到的副作用(日志/逐跳)——锁内
// 回调是潜在死锁陷阱(claudeSession 同纪律:快照后锁外调用)。
func (s *codexSession) handle(msg *codexRpcMsg) {
	s.mu.Lock()
	effects := s.handleLocked(msg)
	s.mu.Unlock()
	for _, fn := range effects {
		fn()
	}
}

func (s *codexSession) handleLocked(msg *codexRpcMsg) []func() {
	var effects []func()
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		effects = append(effects, func() {
			if s.onLog != nil {
				s.onLog(line)
			}
		})
	}
	// initialize 应答 → initialized 通知 + 开线程(start 或 resume)。
	if msg.ID != nil && s.initializeID != nil && *msg.ID == *s.initializeID {
		s.initializeID = nil
		if msg.Error != nil {
			if s.failPendingLocked(codexErrMsg(msg.errMessage(), "codex initialize failed")) {
				logf("[codex] " + codexErrMsg(msg.errMessage(), "codex initialize failed"))
			}
			return effects
		}
		s.notifyLocked("initialized", map[string]any{})
		if s.threadReq != nil {
			id := s.writeReqLocked(s.threadReq.method, s.threadReq.params)
			s.threadReqID = &id
			s.threadReq = nil
		}
		return effects
	}
	// resume 失败(陈旧 id/换引擎后的残留)→ 换全新线程,不楔死 agent。
	if msg.Error != nil && msg.ID != nil && s.threadReqID != nil && *msg.ID == *s.threadReqID {
		if s.threadWasResume {
			logf("[codex] thread/resume failed (%s) — starting a fresh thread", msg.errMessage())
			s.threadWasResume = false
			s.threadID = ""
			id := s.writeReqLocked("thread/start", s.baseThreadParams)
			s.threadReqID = &id
			return effects
		}
		if s.failPendingLocked(codexErrMsg(msg.errMessage(), "codex thread start failed")) {
			logf("[codex] " + codexErrMsg(msg.errMessage(), "codex thread start failed"))
		}
		return effects
	}
	// 线程就绪:thread/start|resume 应答,或 thread/started 通知。
	threadID := ""
	if msg.Result != nil && msg.Result.Thread != nil {
		if v, ok := anyString(msg.Result.Thread.ID); ok {
			threadID = v
		}
	}
	if threadID == "" && msg.Method == "thread/started" {
		if th, ok := msg.Params["thread"].(map[string]any); ok {
			if v, ok := anyString(th["id"]); ok {
				threadID = v
			}
		}
	}
	if threadID != "" {
		s.onThreadReadyLocked(threadID)
		return effects
	}
	// turn id(steering 用)——turn/start 应答或 turn/started 通知。
	turnID := ""
	if msg.Result != nil {
		if msg.Result.Turn != nil {
			if v, ok := anyString(msg.Result.Turn.ID); ok {
				turnID = v
			}
		}
		if turnID == "" {
			if v, ok := anyString(msg.Result.TurnID); ok {
				turnID = v
			}
		}
	}
	if turnID == "" && msg.Method == "turn/started" {
		if th, ok := msg.Params["turn"].(map[string]any); ok {
			if v, ok := anyString(th["id"]); ok {
				turnID = v
			}
		}
	}
	if turnID != "" {
		s.activeTurnID = turnID
		s.steerGate = false
	}
	// 线程 RUNNING token 总量。
	if msg.Method == "thread/tokenUsage/updated" {
		if tu, ok := msg.Params["tokenUsage"].(map[string]any); ok {
			s.updateUsageLocked(tu["total"])
		}
		return effects
	}
	// 账户限流只在吃紧时浮出(>=90%),平时静默。
	if msg.Method == "account/rateLimits/updated" {
		if rl, ok := msg.Params["rateLimits"].(map[string]any); ok {
			if prim, ok := rl["primary"].(map[string]any); ok {
				if pct, ok := prim["usedPercent"].(float64); ok && pct >= 90 {
					logf("[codex] ⚠️ account rate limit at %d%% — turns will start failing when it reaches 100%%", int(pct+0.5))
				}
			}
		}
		return effects
	}
	// items:原生压缩观测;命令与最终答复的简明信号;完成项短暂 gate steer。
	if msg.Method == "item/started" || msg.Method == "item/completed" {
		if item, ok := msg.Params["item"].(map[string]any); ok {
			ty, _ := item["type"].(string)
			switch {
			case ty == "contextCompaction":
				stage := "started"
				if msg.Method == "item/completed" {
					stage = "finished"
				}
				logf("[codex] native context compaction " + stage)
			case ty == "commandExecution" && msg.Method == "item/started":
				if cmd, ok := item["command"].(string); ok {
					logf("[codex] $ " + collapseWS(cmd, 200))
				}
			case ty == "agentMessage" && msg.Method == "item/completed":
				if text, ok := item["text"].(string); ok && strings.TrimSpace(text) != "" {
					logf("[codex] » " + collapseWS(text, 200))
				}
			}
		}
		if msg.Method == "item/completed" {
			s.steerGate = true
		}
		return effects
	}
	if msg.Method == "item/agentMessage/delta" || msg.Method == "item/reasoning/textDelta" || msg.Method == "item/reasoning/summaryTextDelta" {
		s.steerGate = false
		return effects
	}
	// 请求级错误(如 thread/start 失败)→ fail 在飞 turn。
	if msg.Error != nil && msg.ID != nil {
		msg2 := codexErrMsg(msg.errMessage(), "codex app-server request failed")
		if s.failPendingLocked(msg2) {
			logf("[codex] " + msg2)
		}
		return effects
	}
	// turn 结束 → 结算 pending。
	if msg.Method == "turn/completed" {
		failed := ""
		if turn, ok := msg.Params["turn"].(map[string]any); ok {
			if st, _ := turn["status"].(string); st == "failed" {
				failed = "codex turn failed"
				if em, ok := turn["error"].(map[string]any); ok {
					if m, ok := em["message"].(string); ok && m != "" {
						failed = m
					}
				}
			}
		}
		// 逐跳台账:app-server 只有线程级总量,最诚实的粒度是每轮一行
		// 差值——将来暴露逐 item 用量时无需改台账 schema。settle 前发射,
		// daemon 的 flush 窗口可靠包含本跳。
		if failed == "" && s.onHop != nil && s.model != "" {
			hop := HopReport{Model: s.model, Usage: s.turnUsageLocked(), HopIndex: 1}
			if s.turnStartedAt > 0 {
				v := nowMS() - s.turnStartedAt
				hop.LatencyMS = &v
			}
			hopFn, onHop := s.onHop, hop
			effects = append(effects, func() { hopFn(onHop) })
		}
		s.turnStartedAt = 0
		s.activeTurnID = ""
		s.steerGate = false
		s.settleLocked(failed)
		return effects
	}
	if msg.Method == "error" {
		detail := "codex app-server error"
		if m, ok := msg.Params["message"].(string); ok && m != "" {
			detail = m
		} else if em, ok := msg.Params["error"].(map[string]any); ok {
			if m, ok := em["message"].(string); ok && m != "" {
				detail = m
			}
		}
		if s.failPendingLocked(detail) {
			logf("[codex] " + detail)
		}
	}
	return effects
}

func codexErrMsg(msg, fallback string) string {
	if msg != "" {
		return msg
	}
	return fallback
}

func collapseWS(s string, n int) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), n)
}

func (s *codexSession) onThreadReadyLocked(threadID string) {
	s.threadID = threadID
	s.ready = true
	if s.queuedPrompt != "" && s.pending != nil {
		p := s.queuedPrompt
		s.queuedPrompt = ""
		s.startTurnLocked(p)
	}
}

func num(v any) int64 {
	f, ok := v.(float64)
	if !ok || f != f || f > 1<<62 || f < -(1<<62) { // NaN/溢出防护
		return 0
	}
	return int64(f)
}

func (s *codexSession) updateUsageLocked(total any) {
	t, ok := total.(map[string]any)
	if !ok {
		return
	}
	// 取 max:迟到的/部分的更新不能让 RUNNING 总量回退。
	s.cum.input = maxInt64(s.cum.input, num(t["inputTokens"]))
	s.cum.cached = maxInt64(s.cum.cached, num(t["cachedInputTokens"]))
	s.cum.output = maxInt64(s.cum.output, num(t["outputTokens"])+num(t["reasoningOutputTokens"]))
}

func (s *codexSession) turnUsageLocked() EngineUsage {
	inputTotal := maxInt64(0, s.cum.input-s.turnStart.input)
	cached := maxInt64(0, s.cum.cached-s.turnStart.cached)
	u := EngineUsage{
		InputTokens:          int64p(maxInt64(0, inputTotal-cached)), // 非缓存部分(Claude 形字段)
		CacheReadInputTokens: int64p(cached),
	}
	out := maxInt64(0, s.cum.output-s.turnStart.output)
	u.OutputTokens = int64p(out)
	return u
}

func (s *codexSession) settleLocked(errMsg string) {
	ch := s.pending
	s.pending = nil
	if ch == nil {
		return
	}
	res := RunResult{SessionID: s.threadID, Usage: usagePtr(s.turnUsageLocked()), Model: s.model}
	if errMsg != "" {
		res.ExitCode = 1
		res.Err = errMsg
	}
	ch <- res
}

func usagePtr(u EngineUsage) *EngineUsage { return &u }

// failPendingLocked:请求级失败。线程从未打开过的失败要杀掉整个 SESSION
// 而不只是本轮——握手是一次性的(threadReq 在 initialize ack 时已消费,
// 只有 resume 失败会重发 thread/start),此后 ready 永不翻转,后续 send
// 全部把提示词泊进 queuedPrompt 无人排空(daemon 永远 await,agent 静默
// 永久死亡、大脑槽位永不归还)。app-server 拒绝握手后进程还活着(畸形
// ~/.codex/config.toml、账户不可用模型、协议漂移),Alive 会继续谎报可用。
// 拆掉它:!alive 的会话被丢弃,下一唤醒起干净的。
// failPendingLocked:请求级失败。返回 true = 无在飞 turn(空闲失败),
// 调用方在锁外记日志。
func (s *codexSession) failPendingLocked(errMsg string) (idle bool) {
	if s.pending != nil {
		s.settleLocked(errMsg)
	} else {
		idle = true
	}
	if !s.ready {
		s.handshakeErr = errMsg
		go s.Stop()
	}
	return idle
}

func (s *codexSession) die(code int, why string) {
	s.mu.Lock()
	alreadyDown := s.exited
	s.exited = true
	s.exitCode = code
	onLog := s.onLog
	ch := s.pending
	s.pending = nil
	threadID := s.threadID
	s.mu.Unlock()
	if !alreadyDown && onLog != nil {
		if ch != nil {
			onLog(fmt.Sprintf("[session] engine process died MID-TURN: %s (exit %d)", why, code))
		} else {
			onLog(fmt.Sprintf("[session] engine process died while idle: %s (exit %d)", why, code))
		}
	}
	if ch != nil {
		ch <- RunResult{ExitCode: code, Err: why, SessionID: threadID}
	}
}

/* ───────── 适配器 ───────── */

type codexAdapter struct{}

func init() { RegisterAdapter(codexAdapter{}) }

func (codexAdapter) ID() string  { return "codex" }
func (codexAdapter) Bin() string { return "codex" }

// SeedHome:ensureCommonHome + AGENTS.md(内容仍是默认 personaHeader——
// TS 同此:文件名按 Codex 约定,内容里的布局说明保持上游原文)。
func (codexAdapter) SeedHome(home string, p Persona) error {
	if err := ensureCommonHome(home); err != nil {
		return err
	}
	return os.WriteFile(home+string(os.PathSeparator)+"AGENTS.md", []byte(personaHeader(p, "CLAUDE.md", ".claude/skills/")), 0o644)
}

// Classify:ChatGPT 账户挑不动任意小模型(gpt-5-mini 被拒)但接受
// gpt-5.4-mini——本机小脑。CUMORA_TRIAGE_MODEL 可覆盖。
func (codexAdapter) Classify(ctx context.Context, args ClassifyArgs) ClassifyResult {
	triFlags := extraArgs("CUMORA_TRIAGE_ARGS")
	model := args.Model
	if model == "" {
		model = "gpt-5.4-mini"
	}
	plan := resolveSpawn("codex")
	var argv []string
	if len(triFlags) > 0 {
		argv = append(append([]string{"exec"}, triFlags...), "-")
	} else {
		argv = []string{"exec", "--model", model, "--skip-git-repo-check", "-"}
	}
	return spawnCapture(ctx, plan, argv, args.Cwd, args.Env, args.OnLog, args.Prompt)
}

// Probe:small → gpt-5.4-mini;big → 不加 --model(CLI 默认)。
func (codexAdapter) Probe(ctx context.Context, args ProbeArgs) ClassifyResult {
	plan := resolveSpawn("codex")
	argv := []string{"exec", "--skip-git-repo-check", "-"}
	if args.Tier == "small" {
		argv = []string{"exec", "--model", triageModel("gpt-5.4-mini"), "--skip-git-repo-check", "-"}
	}
	return spawnCapture(ctx, plan, argv, args.Cwd, args.Env, nil, doctorPrompt)
}

// ProbeWake:真唤醒起 `codex app-server --listen stdio://` 走 JSON-RPC 握手
// (initialize → thread/start)。真实断点:app-server 子命令改名、协议字段
// 改名(approvalPolicy/sandbox/experimentalRawEvents)、cwd 的 git 仓库垫底
// 失败。最小握手全查,然后拆进程。折叠为一次性 exec 时标记 skipped。
func (codexAdapter) ProbeWake(ctx context.Context, args WakeProbeArgs) WakeProbeResult {
	if len(extraArgs("CUMORA_CODEX_ARGS")) > 0 || os.Getenv("CUMORA_CODEX_NO_APP_SERVER") == "1" || isWindows() {
		return WakeProbeResult{OK: true, Skipped: true}
	}
	if err := ensureGitRepoForCodex(args.Cwd); err != nil {
		return WakeProbeResult{Detail: "git init failed for app-server cwd: " + err.Error()}
	}
	plan := resolveSpawn("codex")
	cmd := buildCmd(plan, []string{"app-server", "--listen", "stdio://"})
	cmd.Dir = args.Cwd
	cmd.Env = args.Env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return WakeProbeResult{Detail: "stdin pipe: " + err.Error()}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return WakeProbeResult{Detail: "stdout pipe: " + err.Error()}
	}
	var stderrBuf []byte
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return WakeProbeResult{Detail: "stderr pipe: " + err.Error()}
	}
	if err := cmd.Start(); err != nil {
		return WakeProbeResult{Detail: "spawn error: " + err.Error()}
	}
	type probeResult struct {
		ok     bool
		detail string
	}
	out := make(chan probeResult, 1)
	var stderrMu sync.Mutex
	var exitInfo atomic.Value // string;收割协程尽快填(死因的 exit/signal 段)
	finish := func(r probeResult) {
		_ = stdin.Close()
		_ = killProcess(cmd)
		// M1:必须收割——不 Wait 每次探测漏一个僵尸(TS/Node 自动收)。
		go func() {
			werr := cmd.Wait()
			seg := ""
			if werr == nil {
				seg = "exit 0"
			} else if ee, ok := werr.(*exec.ExitError); ok {
				if ee.ExitCode() < 0 {
					seg = "terminated by " + signalNameOf(ee)
				} else {
					seg = fmt.Sprintf("exit %d", ee.ExitCode())
				}
			} else {
				seg = werr.Error()
			}
			exitInfo.Store(seg)
		}()
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
	const initID, threadIDReq = int64(1), int64(2)
	writeRpc(map[string]any{"jsonrpc": "2.0", "id": initID, "method": "initialize",
		"params": map[string]any{
			"clientInfo":   map[string]any{"name": "cumora-doctor", "version": "1.0.0"},
			"capabilities": map[string]any{"experimentalApi": true},
		}})
	go func() {
		r := bufio.NewReader(stdout)
		carry := ""
		initialized, threadAcked := false, false
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
					var msg codexRpcMsg
					if json.Unmarshal([]byte(t), &msg) != nil {
						continue
					}
					if msg.Error != nil && msg.errMessage() != "" {
						detail := msg.errMessage()
						if len(detail) > 240 {
							detail = detail[:240]
						}
						finish(probeResult{detail: "app-server rejected handshake: " + detail})
						return
					}
					if !initialized && msg.ID != nil && *msg.ID == initID && msg.Result != nil {
						initialized = true
						writeRpc(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
						writeRpc(map[string]any{"jsonrpc": "2.0", "id": threadIDReq, "method": "thread/start",
							"params": map[string]any{
								"cwd": args.Cwd, "approvalPolicy": "never",
								"sandbox": "danger-full-access", "experimentalRawEvents": true,
							}})
						continue
					}
					if initialized && !threadAcked && msg.ID != nil && *msg.ID == threadIDReq && msg.Result != nil {
						threadAcked = true
						finish(probeResult{ok: true})
						return
					}
				}
			}
			if rerr != nil {
				stage := "after handshake"
				if !initialized {
					stage = "before initialize ack"
				} else if !threadAcked {
					stage = "before thread/start ack"
				}
				stderrMu.Lock()
				salient := salientError(string(stderrBuf))
				stderrMu.Unlock()
				if salient == "" {
					salient = "no stderr"
				}
				// EOF 时进程已退——等收割协程至多 200ms,拿到 exit/signal 段
				// 再拼死因(best-effort,超时则省段)。
				var seg string
				for i := 0; i < 20; i++ {
					if v, ok := exitInfo.Load().(string); ok && v != "" {
						seg = v
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				detail := "app-server died " + stage
				if seg != "" {
					detail += " (" + seg + ")"
				}
				finish(probeResult{detail: detail + ": " + salient})
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

// Run:一次性 turn。codex exec 非交互;agent 在用户自有配对机上要全权
// 跑 cumora shim(网络)、clone 仓库、写文件——Codex 版的
// --dangerously-skip-permissions 是 --dangerously-bypass-approvals-and-sandbox;
// --skip-git-repo-check 让它在 agent home(非 git 仓库)可跑;提示词走
// stdin('-')。CUMORA_CODEX_ARGS 可整套覆盖。
func (codexAdapter) Run(ctx context.Context, args RunArgs) RunResult {
	flags := extraArgs("CUMORA_CODEX_ARGS")
	base := flags
	if len(base) == 0 {
		base = []string{"--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"}
	}
	argv := []string{"exec"}
	if args.Model != "" {
		argv = append(argv, "--model", args.Model)
	}
	argv = append(append(argv, base...), "-")
	plan := resolveSpawn("codex")
	return spawnEngine(ctx, plan, argv, args, args.Prompt)
}

// StartSession:持久 app-server 会话。逃生口(回退一次性 exec):自定义
// 参数覆盖、显式退出(CUMORA_CODEX_NO_APP_SERVER)、Windows(.cmd shell
// 上的 JSON-RPC 脆弱)。standing prompt 随线程 developerInstructions 带外
// 投递;审批/沙箱按线程设置,无需全局 bypass 旗。
func (codexAdapter) StartSession(args SessionArgs) EngineSession {
	if len(extraArgs("CUMORA_CODEX_ARGS")) > 0 {
		return nil
	}
	if os.Getenv("CUMORA_CODEX_NO_APP_SERVER") == "1" {
		return nil
	}
	if isWindows() {
		return nil
	}
	if err := ensureGitRepoForCodex(args.Home); err != nil {
		if args.OnLog != nil {
			args.OnLog("[codex] could not init git repo for app-server (" + err.Error() + ") — falling back to one-shot exec")
		}
		return nil
	}
	// app-server 一律直跑(TS 同:spawn(bin, { shell: false });Windows 已在上面回退)。
	return newCodexSession("codex", []string{"app-server", "--listen", "stdio://"}, args)
}
