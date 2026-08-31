// daemon 包 runner —— 每 agent 一个 AgentRunner(对齐 daemon.ts
// AgentRunner):runtime token 铸造/刷新、wake-stream 消费(SSE + 重连
// 退避)、轮询兜底、唤醒合并、run 生命周期上报(runs → run 心跳 →
// finish)、session id 落盘与恢复、持久引擎会话(懒孵化/复活/死亡即弃,
// standing prompt 带外投递或逐轮内联)、同轮 STEERING(直接 ping 注入
// 运行中的 turn)、逐跳轨迹台账。triage/agenda 的完整认知管线是 #61。
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// steerGroupInTurn:群消息的内容无关 mid-turn 提点(告知有新消息但不注入
// 正文)——默认开,CUMORA_BYOA_STEER_GROUP=0 关闭。
func steerGroupInTurn() bool { return os.Getenv("CUMORA_BYOA_STEER_GROUP") != "0" }

// groupSteerMinInterval:群提点节流(TS:Math.max(0, Number(env ?? 8000)))。
func groupSteerMinInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("CUMORA_BYOA_STEER_GROUP_INTERVAL_MS"))
	if v == "" {
		return 8 * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 8 * time.Second
	}
	if n < 0 {
		n = 0
	}
	return time.Duration(n) * time.Millisecond
}

// AgentRunner:单 agent 的常驻执行器。
type AgentRunner struct {
	cfg     *DaemonConfig
	agent   AgentInfo
	adapter EngineAdapter

	home        string
	binDir      string
	sessionFile string

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	busy     bool
	stopped  bool
	session  string
	// pendingRerun:turn 在飞时又有唤醒折入——本轮结束再跑一次
	// (对齐 TS 的 coalesce 语义;重跑会重读 inbox)。
	pendingRerun bool
	// minting:runtime token 铸造进行中(single-flight)。
	minting bool

	wakeDebounce  *time.Timer
	lastWakeConvo string

	// streamAlive:#134——SSE 最近一行(ping/事件)到达时刻(UnixNano;
	// 0=从未连上)。pollLoop 门控用;atomic:SSE 读循环与 pollLoop 并发。
	streamAlive atomic.Int64

	// 持久引擎会话(懒孵化、跨唤醒复用;死亡即弃、下一唤醒复活)。
	engineSession EngineSession

	// 同轮 steering 状态。
	sideSteering          bool
	lastSteeredMsgID      string
	lastGroupSteeredMsgID string
	lastGroupSteerAt      time.Time

	// 逐跳台账:当前在飞 run 的 id(hops 挂回 run)+ 缓冲上报器。
	currentRunID string
	reporter     *hopReporter
	// deltas:#210 流式增量上报器(引擎已产出前缀 → /runtime/message-delta)。
	deltas *deltaReporter

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newAgentRunner(cfg *DaemonConfig, agent AgentInfo, adapter EngineAdapter) *AgentRunner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &AgentRunner{
		cfg:         cfg,
		agent:       agent,
		adapter:     adapter,
		home:        filepath.Join(agentsRoot(), agent.ID),
		binDir:      filepath.Join(agentsRoot(), agent.ID, "bin"),
		sessionFile: filepath.Join(sessionsDir(), agent.ID+".session"),
		ctx:         ctx,
		cancel:      cancel,
	}
	r.reporter = newHopReporter(cfg.ServerURL, func(ctx context.Context) (string, error) {
		return r.ensureToken()
	})
	r.deltas = newDeltaReporter(cfg.ServerURL, func(ctx context.Context) (string, error) {
		return r.ensureToken()
	})
	return r
}

// ConfigMatches:agent 配置(engine/名字/角色/提示/模型)变化 → 重建 runner。
func (r *AgentRunner) ConfigMatches(agent AgentInfo, engine string) bool {
	return r.adapter.ID() == engine &&
		r.agent.Name == agent.Name &&
		strEqPtr(r.agent.Role, agent.Role) &&
		strEqPtr(r.agent.SystemPrompt, agent.SystemPrompt) &&
		strEqPtr(r.agent.Model, agent.Model) &&
		strEqPtr(r.agent.FastModel, agent.FastModel)
}

func strEqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Start:布置 home(shim + 人格)→ 载入既有 session id → 起 wake-stream +
// 轮询兜底。seedHome 失败不阻断启动(TS 同语义:下一同步还会再试)。
func (r *AgentRunner) Start() {
	if err := writeShim(r.binDir); err != nil {
		slog.Warn("[computer] writeShim failed", "agent", r.agent.ID, "err", err)
	}
	if err := r.adapter.SeedHome(r.home, Persona{ID: r.agent.ID, Name: r.agent.Name, Role: r.agent.Role, SystemPrompt: r.agent.SystemPrompt, Model: r.agent.Model, FastModel: r.agent.FastModel}); err != nil {
		slog.Warn("[computer] seedHome failed", "agent", r.agent.ID, "err", err)
	}
	if s, err := os.ReadFile(r.sessionFile); err == nil {
		r.session = strings.TrimSpace(string(s))
	}
	r.wg.Add(2)
	go r.wakeStreamLoop()
	go r.pollLoop()
}

// BeginStop:软停——不再接新唤醒(停轮询拍、拆防抖定时),但在飞 turn
// 及其引擎子进程继续跑完(persist session id、finalize run)。宽限窗内
// 靠它保住 turn 的 HTTP 生命周期(直接 cancel 会把在飞轮立即掐死,run
// 永不 finish,被服务端陈旧清扫误收)。
func (r *AgentRunner) BeginStop() {
	r.mu.Lock()
	r.stopped = true
	if r.wakeDebounce != nil {
		r.wakeDebounce.Stop()
		r.wakeDebounce = nil
	}
	sess := r.engineSession
	r.engineSession = nil
	r.mu.Unlock()
	if sess != nil {
		sess.Stop()
	}
}

// Stop:硬停——软停之上取消 ctx、拆台账上报器(最后一冲),在飞 turn
// 与引擎子进程随之中断。
func (r *AgentRunner) Stop() {
	r.BeginStop()
	if r.reporter != nil {
		r.reporter.stop()
	}
	r.cancel()
}

func (r *AgentRunner) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy
}

/* ───────── runtime token ───────── */

// ensureToken:短期 agent-runtime JWT;到期前 5min 刷新,并落到
// bin/.runtime-token(长期引擎进程从文件取新值,env 值会陈旧)。
func (r *AgentRunner) ensureToken() (string, error) {
	r.mu.Lock()
	if r.token != "" && time.Now().Before(r.tokenExp.Add(-tokenRefreshSkew)) {
		t := r.token
		r.mu.Unlock()
		return t, nil
	}
	// single-flight:并发路径(wake-stream 连接 + runTurn)只放一个铸造,
	// 其余等它完成后读缓存(否则双铸竞态,.runtime-token 后写覆盖)。
	if r.minting {
		r.mu.Unlock()
		for i := 0; i < 200; i++ {
			time.Sleep(10 * time.Millisecond)
			r.mu.Lock()
			if r.token != "" && time.Now().Before(r.tokenExp.Add(-tokenRefreshSkew)) {
				t := r.token
				r.mu.Unlock()
				return t, nil
			}
			r.mu.Unlock()
		}
		return "", fmt.Errorf("runtime token mint in progress")
	}
	r.minting = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.minting = false
		r.mu.Unlock()
	}()
	var minted struct {
		Token            string `json:"token"`
		ExpiresInSeconds int    `json:"expiresInSeconds"`
	}
	if err := apiCall(r.ctx, r.cfg.ServerURL, http.MethodPost,
		"/api/agents/"+r.agent.ID+"/runtime-token", r.cfg.DeviceToken,
		map[string]any{}, &minted); err != nil {
		return "", err
	}
	r.mu.Lock()
	r.token = minted.Token
	r.tokenExp = time.Now().Add(time.Duration(minted.ExpiresInSeconds) * time.Second)
	r.mu.Unlock()
	_ = os.MkdirAll(r.binDir, 0o755)
	_ = os.WriteFile(filepath.Join(r.binDir, ".runtime-token"), []byte(minted.Token), 0o600)
	return minted.Token, nil
}

/* ───────── 引擎环境 ───────── */

// engineModel/EngineFastModel:CUMORA_ENGINE_MODEL 的逃生口——'local' 表示
// 完全不强加模型(CLI 自己配置什么就跑什么);具体值只替换大脑 pin。
func resolveEngineModel(configured *string, override string) string {
	o := strings.TrimSpace(override)
	if o == "" {
		if configured != nil {
			return *configured
		}
		return ""
	}
	if strings.EqualFold(o, "local") {
		return ""
	}
	return o
}

func resolveEngineFastModel(configured *string, override string) string {
	if strings.EqualFold(strings.TrimSpace(override), "local") {
		return ""
	}
	if configured != nil {
		return *configured
	}
	return ""
}

// engineEnv:引擎子进程环境 = daemon 环境 + PATH 前置 binDir(cumora
// shim)+ runtime 接线。token 文件优先于 env(持久进程的 env token 在
// 刷新后陈旧,shim 优先读文件、回退 env)。
func (r *AgentRunner) engineEnv(token string) []string {
	env := os.Environ()
	for i, kv := range env {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			env[i] = "PATH=" + prependAgentBinToPath(r.binDir, kv[len("PATH="):])
		}
	}
	env = append(env,
		"CUMORA_AGENT_RUNTIME_URL="+r.cfg.ServerURL+"/runtime",
		"CUMORA_AGENT_RUNTIME_TOKEN="+token,
		"CUMORA_AGENT_RUNTIME_TOKEN_FILE="+filepath.Join(r.binDir, ".runtime-token"),
		"CUMORA_AGENT_ID="+r.agent.ID,
	)
	return env
}

// ensureEngineSession:懒孵化 + 复活。adapter 无持久模式(或本机覆盖)
// 返回 nil → 调用方降级一次性 Run。前进程已死则带着 sessionId 重起
// (--resume/thread resume,上下文跨重启续命)。
func (r *AgentRunner) ensureEngineSession() EngineSession {
	r.mu.Lock()
	sess := r.engineSession
	r.mu.Unlock()
	if sess != nil && sess.Alive() {
		return sess
	}
	if sess != nil {
		sess.Stop() // 已死——清干净再孵化
	}
	// token 铸造失败时不孵化(空 token 的会话环境是半接线态;一次性路径
	// 随后也会在 token 上失败并进入正常错误处理)。
	token, err := r.ensureToken()
	if err != nil {
		slog.Warn("[computer] engine session spawn skipped (token mint failed)", "agent", r.agent.ID, "err", err)
		return nil
	}
	newSess := r.adapter.StartSession(SessionArgs{
		Home:            r.home,
		Env:             r.engineEnv(token),
		Model:           resolveEngineModel(r.agent.Model, os.Getenv("CUMORA_ENGINE_MODEL")),
		FastModel:       resolveEngineFastModel(r.agent.FastModel, os.Getenv("CUMORA_ENGINE_MODEL")),
		ResumeSessionID: r.currentSession(),
		StandingPrompt:  r.standingPrompt(),
		OnLog:           func(line string) { r.logEngineLine(line) },
		OnHopUsage:      func(rep HopReport) { r.onEngineHop(rep) },
		OnAssistantText: func(text string) { r.onEngineText(text) },
	})
	r.mu.Lock()
	r.engineSession = newSess
	r.mu.Unlock()
	if newSess != nil {
		resume := r.currentSession()
		if resume != "" {
			r.logEngineLine(fmt.Sprintf("[computer] %s engine session respawned (resume %s) — persistent, no per-wake cold start", r.agent.ID, short8(resume)))
		} else {
			r.logEngineLine(fmt.Sprintf("[computer] %s engine session spawned fresh — persistent, no per-wake cold start", r.agent.ID))
		}
	}
	return newSess
}

// visibleEngineError:引擎错误的服务端可见形态。原始 failurePreview 常含
// 绝对路径(agent home/tmp)——两域隐私纪律下这是刻意脱敏:home →
// <agent home>,操作者 HOME → ~;再包上引擎与退出码上下文。
func (r *AgentRunner) visibleEngineError(exitCode int, detail string) string {
	raw := detail
	if raw == "" {
		raw = fmt.Sprintf("process exited with code %d", exitCode)
	}
	clean := truncateRunes(strings.TrimSpace(strings.ReplaceAll(
		ansiRe.ReplaceAllString(raw, ""), "\r", "")), 900)
	// 顺序即语义:agent home 先替换(r.home 是 $HOME 的子路径,先替换
	// $HOME 会把它吞掉,<agent home> 永远命不中)。
	clean = strings.ReplaceAll(clean, r.home, "<agent home>")
	if home := homeDir(); home != "" && home != "." {
		clean = strings.ReplaceAll(clean, home, "~")
	}
	return fmt.Sprintf("local %s failed (exit %d): %s", r.adapter.ID(), exitCode, clean)
}

func short8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// standingPrompt:不变量操作脚手架(每会话一次,带外投递)。#64 版为
// daemon.ts standingPrompt 的逐字节转录;分级 delta 组装是 #61 的血肉。
func (r *AgentRunner) standingPrompt() string {
	return standingPrompt(r.agent.ID)
}

// turnPrompt:持久会话已带外投递 standing prompt → 只发增量;否则(一次性
// 路径,或带外投递不可用)内联——separator 逐字节对齐 TS。
func (r *AgentRunner) turnPrompt(sess EngineSession, delta string) string {
	if sess != nil && sess.CarriesStandingPrompt() {
		return delta
	}
	return r.standingPrompt() + "\n\n════════\n\n" + delta
}

func (r *AgentRunner) logEngineLine(line string) {
	if line == "" {
		return
	}
	slog.Info("[engine] "+line, "agent", r.agent.ID)
}

/* ───────── wake-stream(SSE) ───────── */

// wakeStreamLoop:长连 SSE;断流指数退避(1s×2 封顶 30s),稳定满
// streamStableMS 后退避归零。收 wake/steer → scheduleWake。
func (r *AgentRunner) wakeStreamLoop() {
	defer r.wg.Done()
	backoff := time.Second
	var connectedAt time.Time
	for {
		r.mu.Lock()
		stopped := r.stopped
		r.mu.Unlock()
		if stopped {
			return
		}
		up := time.Duration(0)
		if !connectedAt.IsZero() {
			up = time.Since(connectedAt)
		}
		err := r.consumeStream(&connectedAt)
		if up >= time.Duration(streamStableMS)*time.Millisecond {
			backoff = time.Second
		}
		if err != nil {
			slog.Warn("[computer] stream closed/error; retrying", "agent", r.agent.ID, "err", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-r.ctx.Done():
				return
			}
			backoff = minDuration(backoff*2, 30*time.Second)
		}
	}
}

// consumeStream:单次 SSE 会话——读到流断或 ctx 取消。
func (r *AgentRunner) consumeStream(connectedAt *time.Time) error {
	token, err := r.ensureToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet,
		r.cfg.ServerURL+"/runtime/wake-stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("wake-stream HTTP %d", res.StatusCode)
	}
	*connectedAt = time.Now()
	r.markStreamAlive()
	slog.Info("[computer] wake-stream connected", "agent", r.agent.ID, "engine", r.adapter.ID())
	// 冷启动/重连补拍:断流窗口的消息由 inbox 兜底拾起。
	r.scheduleWake("reconnect-catchup", "")

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var event string
	var dataLines []string
	handle := func() {
		if event == "" && len(dataLines) == 0 {
			return
		}
		if event == "wake" || event == "steer" {
			var payload struct {
				ConversationID string `json:"conversationId"`
			}
			_ = json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &payload)
			slog.Info("[computer] SSE received", "agent", r.agent.ID, "event", event, "convo", payload.ConversationID)
			// 每个 SSE 唤醒都记 convo(TS 语义:turn 中的 hop 归因用它;
			// runTurn 开头清空防陈旧)。scheduleWake 的 busy 分支不带 convo。
			r.mu.Lock()
			if payload.ConversationID != "" {
				r.lastWakeConvo = payload.ConversationID
			}
			r.mu.Unlock()
			r.scheduleWake("sse-"+event, payload.ConversationID)
		}
		event, dataLines = "", nil
	}
	for sc.Scan() {
		r.markStreamAlive() // 含 ping 注释——静默期也证明连接活着(#134)
		line := sc.Text()
		if line == "" {
			handle()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // ping 注释
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(line[len("event:"):])
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(line[5:], " "))
		}
	}
	handle()
	return fmt.Errorf("stream ended")
}

/* ───────── 轮询兜底 ───────── */

// markStreamAlive/streamHealthy:#134——SSE 活性:任一行(含 25s ping
// 注释)到达即记;健康 = 最近 sseHealthGrace 内有过行。未连上(0)恒不
// 健康 → 冷启动/未连接期轮询兜底照旧。
func (r *AgentRunner) markStreamAlive() { r.streamAlive.Store(time.Now().UnixNano()) }

func (r *AgentRunner) streamHealthy() bool {
	last := r.streamAlive.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < sseHealthGrace
}

// pollLoop:wake-stream 断流时的兜底(每 INBOX_POLL_MS 拍一次;忙时
// 跳过)。#134 门控:流健康(最近有帧/ping)时静默——事件驱动已覆盖,
// 不再每拍打全库最重的 LoadInbox + status/runs 写(N 个 idle agent =
// N×3 次/分钟独占池连接);断流/未连上才轮询,恢复后即回事件驱动。
func (r *AgentRunner) pollLoop() {
	defer r.wg.Done()
	t := time.NewTicker(inboxPollInterval())
	defer t.Stop()
	safety := time.NewTicker(healthyPollInterval())
	defer safety.Stop()
	wasHealthy := false
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			healthy := r.streamHealthy()
			if healthy != wasHealthy {
				wasHealthy = healthy
				if healthy {
					slog.Info("[computer] wake-stream healthy — polling fallback silent", "agent", r.agent.ID)
				} else {
					slog.Warn("[computer] wake-stream unhealthy — polling fallback active", "agent", r.agent.ID)
				}
			}
			if healthy || r.IsBusy() {
				continue
			}
			r.scheduleWake("poll", "")
		case <-safety.C:
			// 健康期安全网(#134 评审 P2):流判健康但服务端事件链可能
			// 已聋(Redis 断连时 ping 照流、Deliver 全丢)。不健康段由
			// 主拍兜底,这里只补健康段的拾取封顶;忙时跳过。
			if r.streamHealthy() && !r.IsBusy() {
				r.scheduleWake("poll-safety-net", "")
			}
		}
	}
}

/* ───────── 唤醒合并 + steer + turn ───────── */

// scheduleWake:防抖 + 合并(对齐 TS):忙 → 折进在飞轮的重跑,且若带
// 会话则尝试同轮 steer(直接 ping 不等轮终);空闲 → 首个唤醒武装 2.5s
// 定时器,窗口内的后续唤醒折入,触发时跑一轮读全部未读的 turn。
func (r *AgentRunner) scheduleWake(reason, convo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if r.busy {
		// 在飞轮结束时会因 pending 重跑;直接 ping 则立刻注入运行中 turn。
		r.pendingRerun = true
		if convo != "" {
			go r.maybeSteer(convo)
		}
		return
	}
	if r.wakeDebounce != nil {
		return
	}
	if convo != "" {
		r.lastWakeConvo = convo
	}
	r.wakeDebounce = time.AfterFunc(wakeDebounce, func() {
		r.mu.Lock()
		r.wakeDebounce = nil
		r.mu.Unlock()
		r.kickTurn(reason)
	})
}

func (r *AgentRunner) kickTurn(reason string) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if err := r.runTurn(reason); err != nil {
			slog.Error("[computer] runTurn failed (swallowed)", "agent", r.agent.ID, "err", err)
		}
	}()
}

// maybeSteer:同轮转向——turn 在飞时收到直接 ping(DM / @我 / 人类发
// 言)→ 注入活会话,让 agent 简答后继续原任务,而非等轮终。尽力而为;
// 仅直接 ping、按消息 id 去重、绝不打断主任务。群消息的内容无关提点为
// 选配(默认开,节流 + 最新 id 去重)。
func (r *AgentRunner) maybeSteer(convo string) {
	r.mu.Lock()
	if r.sideSteering {
		r.mu.Unlock()
		return
	}
	r.sideSteering = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.sideSteering = false
		r.mu.Unlock()
	}()
	sess := func() EngineSession {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.engineSession
	}()
	if sess == nil || !sess.Alive() {
		return
	}
	token, err := r.ensureToken()
	if err != nil {
		return
	}
	// PROBE 读:只判断"要不要 steer"(~20% 的探查才真转向),不推进
	// freshness-preflight 基线;真转向时 agent 经 steer 正文看到消息行,
	// 重复冲突仍由 cmdReply 的 preflight 兜住。
	var inbox struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := apiCall(r.ctx, r.cfg.ServerURL, http.MethodGet, "/runtime/inbox?probe=1", token, nil, &inbox); err != nil {
		return
	}
	var rows []map[string]any
	for _, row := range inbox.Rows {
		if row["conversation_id"] == convo {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return
	}
	direct := false
	for _, row := range rows {
		if row["conversation_kind"] == "direct" || row["author_kind"] == "human" {
			direct = true
			break
		}
		if body, ok := row["body"].(string); ok && strings.Contains(body, "@"+r.agent.ID) {
			direct = true
			break
		}
	}
	if !direct {
		if !steerGroupInTurn() {
			return
		}
		// 内容无关群提点(选配):告知有活动但不注入正文——节流 + 按
		// 最新 id 去重。agent 经服务端各道闸正常 glance+post;合并重跑
		// 仍是协调安全的兜底,这里只会更快。
		latest := rows[len(rows)-1]
		latestID, _ := latest["id"].(string)
		if latestID == "" {
			return
		}
		r.mu.Lock()
		if latestID == r.lastGroupSteeredMsgID || time.Since(r.lastGroupSteerAt) < groupSteerMinInterval() {
			r.mu.Unlock()
			return
		}
		r.lastGroupSteeredMsgID = latestID
		r.lastGroupSteerAt = time.Now()
		r.mu.Unlock()
		sess.Steer(fmt.Sprintf("⚡ %d new message(s) arrived in %s while you work — bodies withheld to keep you focused. At a natural pause, `cumora glance %s` and handle it if it's yours (yield/claim as usual); otherwise keep going. Do NOT drop your current task.", len(rows), convo, convo))
		r.logEngineLine(fmt.Sprintf("[computer] %s GROUP-NOTICE → live turn (%d msg(s) in %s, content-free)", r.agent.ID, len(rows), convo))
		return
	}
	latest := rows[len(rows)-1]
	latestID, _ := latest["id"].(string)
	if latestID == "" {
		return
	}
	r.mu.Lock()
	if latestID == r.lastSteeredMsgID {
		r.mu.Unlock()
		return
	}
	r.lastSteeredMsgID = latestID
	r.mu.Unlock()
	who, _ := latest["author_name"].(string)
	if who == "" {
		who = "someone"
	}
	body := collapseWS(str(latest["body"]), 300)
	sess.Steer(fmt.Sprintf("⚡ A direct message arrived while you work — answer it BRIEFLY, then resume your current task (do NOT drop it). %s in %s: \"%s\". Reply one line now: `cumora reply %s 'text'` — a quick answer, or \"on it, mid-task, will follow up\". Then continue what you were doing.", who, convo, body, convo))
	r.logEngineLine(fmt.Sprintf("[computer] %s STEER → live turn (direct msg in %s from %s)", r.agent.ID, convo, who))
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// turnDelta:每轮增量(#64 版:唤醒原因 + 时钟 + 未读摘要;triage/
// memory/roster 全量注入是 #61)。时钟是 --at/期限算术的唯一可信源。
func (r *AgentRunner) turnDelta(reason string, rows []map[string]any) string {
	var b strings.Builder
	b.WriteString("You've been woken because there's new activity in your Cumora conversations — your job is to DO the work (write the reply / take the action), not to re-judge whether to. Follow your standing instructions for HOW.\n\n")
	fmt.Fprintf(&b, "Current time (UTC): %s — use this for any --at / deadline math.\n\n", time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	if len(rows) > 0 {
		b.WriteString("Your unread messages (ALREADY FETCHED — no need to re-run `cumora inbox` / `cumora messages` to re-read these; but DO `cumora glance` before posting in a group, to catch anything posted while you compose):\n")
		for _, row := range rows {
			fmt.Fprintf(&b, "- [%s] %s: %s\n", str(row["conversation_id"]), authorLabel(row), collapseWS(str(row["body"]), 200))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Run `cumora inbox`, then `cumora messages <conversationId> --tail 30`, to catch up.\n\n")
	}
	return strings.TrimRight(b.String(), " \n")
}

func authorLabel(row map[string]any) string {
	who := str(row["author_name"])
	if who == "" {
		who = "someone"
	}
	if k := str(row["author_kind"]); k != "" {
		return who + " (" + k + ")"
	}
	return who
}

// runTurn:一轮 turn——inbox 读取(决定是否值得起引擎)→ runs 开行 →
// 引擎(持久会话优先,无则一次性 Run 且内联 standing prompt)→
// finish → session 落盘。triage/agenda/seen-ack 的完整管线是 #61。
func (r *AgentRunner) runTurn(reason string) error {
	r.mu.Lock()
	if r.stopped || r.busy {
		r.mu.Unlock()
		return nil
	}
	r.busy = true
	// 消费本唤醒的 convo(TS:clear 防"轮与轮之间漏下的旧值"在后续
	// poll 轮的 hop 归因上闪烁)。
	r.lastWakeConvo = ""
	r.mu.Unlock()
	defer func() {
		// #210:turn 终结——冲净 delta 尾帧并补 done(终局仍以 cli reply
		// 的 message.new 为准;done 只兜"turn 没回帖"的退场)。
		if r.deltas != nil {
			r.deltas.finish()
		}
		r.mu.Lock()
		r.busy = false
		pending := r.pendingRerun && !r.stopped
		r.pendingRerun = false
		r.currentRunID = ""
		r.mu.Unlock()
		// 折入的重跑立即执行(重读本就是重读 inbox 的完整轮)。
		if pending {
			r.kickTurn("rerun")
		}
	}()

	token, err := r.ensureToken()
	if err != nil {
		return err
	}
	var inbox struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := apiCall(r.ctx, r.cfg.ServerURL, http.MethodGet, "/runtime/inbox", token, nil, &inbox); err != nil {
		return fmt.Errorf("inbox fetch: %w", err)
	}
	if len(inbox.Rows) == 0 {
		return nil
	}

	runtimeBest(r.ctx, r.cfg.ServerURL, "/status", token, map[string]any{"status": "thinking"})
	var run struct {
		RunID string `json:"runId"`
	}
	_ = apiCall(r.ctx, r.cfg.ServerURL, http.MethodPost, "/runtime/runs", token,
		map[string]any{"trigger": map[string]any{"source": "byoa", "engine": r.adapter.ID(), "reason": reason},
			"inboxCount": len(inbox.Rows)}, &run)
	r.mu.Lock()
	r.currentRunID = run.RunID
	r.mu.Unlock()
	stopBeat := r.beatRun(token, run.RunID)
	defer stopBeat()

	sess := r.ensureEngineSession()
	var res RunResult
	if sess != nil {
		// 轮中每 2s 捕获会话 id(TS:第一轮被硬杀后盘上仍留可 resume 的
		// id——Send 返回后再持久化的窗口不够)。
		captureStop := make(chan struct{})
		go func() {
			tick := time.NewTicker(2 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-captureStop:
					return
				case <-tick.C:
					if sid := sess.SessionID(); sid != "" {
						r.setSessionID(sid)
					}
				}
			}
		}()
		res = sess.Send(r.turnPrompt(sess, r.turnDelta(reason, inbox.Rows)))
		close(captureStop)
		if sid := sess.SessionID(); sid != "" {
			r.setSessionID(sid)
		}
		if !sess.Alive() {
			// 进程在轮中/轮后死亡 → 弃会话,下一唤醒复活(resume)。
			r.mu.Lock()
			r.engineSession = nil
			r.mu.Unlock()
		}
	} else {
		args := RunArgs{
			Home:            r.home,
			Prompt:          r.turnPrompt(nil, r.turnDelta(reason, inbox.Rows)),
			Env:             r.engineEnv(token),
			Model:           resolveEngineModel(r.agent.Model, os.Getenv("CUMORA_ENGINE_MODEL")),
			FastModel:       resolveEngineFastModel(r.agent.FastModel, os.Getenv("CUMORA_ENGINE_MODEL")),
			ResumeSessionID: r.currentSession(),
			OnLog:           func(line string) { r.logEngineLine(line) },
			OnHopUsage:      func(rep HopReport) { r.onEngineHop(rep) },
			OnAssistantText: func(text string) { r.onEngineText(text) },
		}
		res = r.adapter.Run(r.ctx, args)
		if res.SessionID != "" {
			r.setSessionID(res.SessionID)
		}
	}

	status := "completed"
	summary := fmt.Sprintf("byoa %s run (exit %d)", r.adapter.ID(), res.ExitCode)
	visibleErr := ""
	if res.Err != "" {
		status = "failed"
		visibleErr = r.visibleEngineError(res.ExitCode, res.Err)
		summary = truncate(visibleErr, 2000)
	}
	if run.RunID != "" {
		runtimeBest(r.ctx, r.cfg.ServerURL, "/runs/"+run.RunID+"/finish", token,
			map[string]any{"status": status, "summary": summary})
	}
	runtimeBest(r.ctx, r.cfg.ServerURL, "/status", token, map[string]any{"status": "avail"})
	if r.reporter != nil {
		r.reporter.flush()
	}
	if visibleErr != "" {
		return fmt.Errorf("%s", visibleErr)
	}
	return nil
}

// onEngineHop:引擎逐跳 → 台账行。extras 携带富化提示(hop 序/工具数/
// 文本量)——正是回答"这跳为什么贵"所需的列,无需拉提示词本体。
func (r *AgentRunner) onEngineHop(rep HopReport) {
	if r.reporter == nil {
		return
	}
	extras := map[string]any{
		"hopIndex":  rep.HopIndex,
		"toolUses":  rep.ToolUses,
		"textChars": rep.TextChars,
	}
	r.mu.Lock()
	hop := pendingHop{
		Source:         "byoa-" + r.adapter.ID(),
		Purpose:        "agent-turn",
		RunID:          r.currentRunID,
		ConversationID: r.lastWakeConvo,
		Model:          rep.Model,
		LatencyMS:      rep.LatencyMS,
		Status:         "ok",
		Extras:         extras,
	}
	r.mu.Unlock()
	hop.Usage = &rep.Usage
	r.reporter.push(hop)
}

// onEngineText:#210 引擎已产出文本前缀 → delta 上报。归因与 hop 台账
// 同款:锚定当前 wake 会话(轮中 steer 会重锚);无锚定会话(纯 poll
// 轮)丢弃——宁缺勿把独白错投到不相干会话。段落分隔由文本源负责
// (Claude 逐事件补空行;codex token 增量原样)。
func (r *AgentRunner) onEngineText(text string) {
	if r.deltas == nil || text == "" {
		return
	}
	convo := func() string {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.lastWakeConvo
	}()
	if convo == "" {
		return
	}
	r.deltas.push(convo, text)
}

// beatRun:长 turn 保活——先一拍再每 RUN_HEARTBEAT_MS(10min 陈旧清扫
// 不误收合法长轮)。返回停止函数。
func (r *AgentRunner) beatRun(token, runID string) func() {
	if runID == "" {
		return func() {}
	}
	beat := func() {
		runtimeBest(r.ctx, r.cfg.ServerURL, "/runs/"+runID+"/heartbeat", token, map[string]any{})
	}
	beat()
	t := time.NewTicker(runHeartbeat)
	// 独立 cancel(而非裸 channel):goroutine 自退路径与外部停止路径
	// 都只调 cancel,避免双 close 竞态。
	beatCtx, cancel := context.WithCancel(context.Background())
	go func() {
		defer t.Stop()
		select {
		case <-beatCtx.Done():
		case <-r.ctx.Done():
		case <-t.C:
			beat()
			for {
				select {
				case <-beatCtx.Done():
					return
				case <-r.ctx.Done():
					return
				case <-t.C:
					beat()
				}
			}
		}
	}()
	return cancel
}

/* ───────── session 持久化(会话恢复) ───────── */

func (r *AgentRunner) currentSession() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session
}

// setSessionID:记录引擎会话 id(下轮 --resume,跨重启续跑);变化才写盘。
func (r *AgentRunner) setSessionID(id string) {
	r.mu.Lock()
	if id == r.session {
		r.mu.Unlock()
		return
	}
	r.session = id
	r.mu.Unlock()
	_ = os.MkdirAll(sessionsDir(), 0o755)
	_ = os.WriteFile(r.sessionFile, []byte(id), 0o600)
}

func (r *AgentRunner) IsStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
