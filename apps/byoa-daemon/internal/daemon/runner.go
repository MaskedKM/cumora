// daemon 包 runner —— 每 agent 一个 AgentRunner(对齐 daemon.ts
// AgentRunner 的协议骨架):runtime token 铸造/刷新、wake-stream 消费
// (SSE + 重连退避)、轮询兜底、唤醒合并、run 生命周期上报
// (runs → run 心跳 → finish)、session id 落盘与恢复。引擎调用走
// EngineAdapter(骨架期 stub;triage/agenda/steering 是 #64+ 的血肉)。
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
	"strings"
	"sync"
	"time"
)

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

	wakeDebounce  *time.Timer
	lastWakeConvo string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newAgentRunner(cfg *DaemonConfig, agent AgentInfo, adapter EngineAdapter) *AgentRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &AgentRunner{
		cfg:         cfg,
		agent:       agent,
		adapter:     adapter,
		home:        filepath.Join(agentsRoot(), agent.ID),
		binDir:      filepath.Join(agentsRoot(), agent.ID, "bin"),
		sessionFile: filepath.Join(sessionsDir(), agent.ID+".session"),
		ctx:         ctx,
		cancel:      cancel,
	}
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

// Start:载入既有 session id,起 wake-stream + 轮询兜底。
func (r *AgentRunner) Start() {
	if s, err := os.ReadFile(r.sessionFile); err == nil {
		r.session = strings.TrimSpace(string(s))
	}
	r.wg.Add(2)
	go r.wakeStreamLoop()
	go r.pollLoop()
}

// Stop:不再接新唤醒,取消在飞(引擎子进程应随 ctx 死掉)。
func (r *AgentRunner) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
}

func (r *AgentRunner) IsBusy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy
}

// BeginStop 与 Stop 同义(骨架:引擎无持久进程面,#64 起拆两段)。
func (r *AgentRunner) BeginStop() { r.Stop() }

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
	r.mu.Unlock()
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
			r.scheduleWake("sse-"+event, payload.ConversationID)
		}
		event, dataLines = "", nil
	}
	for sc.Scan() {
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

// pollLoop:wake-stream 断流时的兜底(每 INBOX_POLL_MS 拍一次;忙时跳过)。
func (r *AgentRunner) pollLoop() {
	defer r.wg.Done()
	t := time.NewTicker(inboxPollInterval())
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			if r.IsBusy() {
				continue
			}
			r.scheduleWake("poll", "")
		}
	}
}

/* ───────── 唤醒合并 + turn ───────── */

// scheduleWake:防抖 + 合并(对齐 TS):忙 → 折进在飞轮的重跑;空闲 →
// 首个唤醒武装 2.5s 定时器,窗口内的后续唤醒折入,触发时跑一轮读全部
// 未读的 turn(一阵爆发 = 一次引擎调用)。
func (r *AgentRunner) scheduleWake(reason, convo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if r.busy {
		// 在飞轮结束时会因 pending 重跑(骨架:直接标记重跑)。
		r.pendingRerun = true
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

// runTurn:骨架 turn——inbox 读取(同时供协议观测)→ runs 开行 →
// 引擎适配器 → finish → session 落盘 → mark-read。triage/agenda/
// steering 的血肉是 #64+。
func (r *AgentRunner) runTurn(reason string) error {
	r.mu.Lock()
	if r.stopped || r.busy {
		r.mu.Unlock()
		return nil
	}
	r.busy = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.busy = false
		pending := r.pendingRerun
		r.pendingRerun = false
		r.mu.Unlock()
		if pending && !r.IsStopped() {
			r.scheduleWake("rerun", "")
		}
	}()

	token, err := r.ensureToken()
	if err != nil {
		return err
	}
	// inbox:决定是否值得起引擎(骨架:有未读才跑)。
	var inbox struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := apiCall(r.ctx, r.cfg.ServerURL, http.MethodGet, "/runtime/inbox", token, nil, &inbox); err != nil {
		return fmt.Errorf("inbox fetch: %w", err)
	}
	if len(inbox.Rows) == 0 {
		return nil
	}

	runtimeBest(r.ctx, r.cfg.ServerURL, "/status", token, map[string]any{"status": "working"})
	var run struct {
		RunID string `json:"runId"`
	}
	_ = apiCall(r.ctx, r.cfg.ServerURL, http.MethodPost, "/runtime/runs", token,
		map[string]any{"trigger": map[string]any{"source": "byoa-wake", "engine": r.adapter.ID(), "reason": reason},
			"inboxCount": len(inbox.Rows)}, &run)
	stopBeat := r.beatRun(token, run.RunID)
	defer stopBeat()

	res := r.adapter.Run(r.ctx, TurnInput{
		Agent:           r.agent,
		RuntimeBaseURL:  r.cfg.ServerURL + "/runtime",
		RuntimeToken:    token,
		ResumeSessionID: r.currentSession(),
		WakeReason:      reason,
	})
	if res.SessionID != "" {
		r.setSessionID(res.SessionID)
	}

	status := "completed"
	if res.Err != nil {
		status = "failed"
	}
	runtimeBest(r.ctx, r.cfg.ServerURL, "/runs/"+run.RunID+"/finish", token,
		map[string]any{"status": status, "summary": truncate(res.Output, 2000)})
	runtimeBest(r.ctx, r.cfg.ServerURL, "/status", token, map[string]any{"status": "avail"})
	return res.Err
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
