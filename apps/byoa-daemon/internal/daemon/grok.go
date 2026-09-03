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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

// grokStartSession:#268 ACP 基类消费点 —— grok 会话机已上移 acp.go,
// 此处只剩描述符条目:spawn 形状、session 参数(_meta.rules 携 standing
// prompt)、_x.ai/models/update 播报钩子、Steer 告警与 env 钉住。
func grokStartSession(bin string, args SessionArgs) *acpSession {
	meta := map[string]any{"yoloMode": true}
	if args.StandingPrompt != "" {
		meta["rules"] = args.StandingPrompt
	}
	spawnArgs := []string{"agent", "--always-approve", "--no-leader"}
	if args.Model != "" {
		spawnArgs = append(spawnArgs, "--model", args.Model)
	}
	spawnArgs = append(spawnArgs, "stdio")
	return newAcpSession(acpSessionConfig{
		EngineID:         "grok",
		Bin:              bin,
		SpawnArgs:        spawnArgs,
		SessionNewParams: map[string]any{"cwd": args.Home, "mcpServers": []any{}, "_meta": meta},
		SteerWarnLine:    "[grok] same-turn steer is not supported on ACP stdio — the ping rides the next wake",
		OnNotify: func(s *acpSession, method string, params map[string]any) {
			// Grok 在此播报在跑模型(及会话中途切换)——没有它,每轮台账
			// 都按一个通常缺席的 pin 定价(curModel 语义同 ClaudeSession
			// 嗅探 message.model)。
			if method == "_x.ai/models/update" {
				if id, ok := params["currentModelId"].(string); ok && id != "" {
					s.curModel = id
				}
			}
		},
		EnvKeepKey:   "GROK_DISABLE_AUTOUPDATER",
		EnvKeepValue: "1",
	}, args)
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
	return spawnCapture(ctx, plan, argv, args.Cwd, withEnvDefaultKeep(args.Env, "GROK_DISABLE_AUTOUPDATER", "1"), nil, stdinText)
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
	cmd.Env = withEnvDefaultKeep(args.Env, "GROK_DISABLE_AUTOUPDATER", "1")
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
	var exitInfo atomic.Value // string;收割协程尽快填(死因的 exit/signal 段)
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
				var seg string
				for i := 0; i < 20; i++ { // EOF 时进程已退——至多等收割 200ms
					if v, ok := exitInfo.Load().(string); ok && v != "" {
						seg = v
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				detail := "grok agent stdio died " + stage
				if seg != "" {
					detail += " (" + seg + ")"
				}
				stderrMu.Lock()
				salient := salientError(string(stderrBuf))
				stderrMu.Unlock()
				if salient == "" {
					salient = "no stderr"
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

// SeedHome:AGENTS.md 仅缺失时写(write-once——grok 会把规则并入自身会话
// 状态,重写等于每轮重置;与 claude/codex/zcode 的恒重写相反)。
// 注:session/update 的 title 提取在"kind 来自 params.sessionUpdate 字符串
// 且无 params.update"的罕见形状下比 TS 多保留 title(TS 该形状静默丢弃)——
// 有意偏差:日志多一行信号无害,少一行才是损失。
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
	args.Env = withEnvDefaultKeep(args.Env, "GROK_DISABLE_AUTOUPDATER", "1")
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
	return grokStartSession(grokAdapter{}.command(args.Env), args)
}
