// daemon 包 run —— 主循环(对齐 doRun):心跳、agent 同步(配置变化
// 重建 runner)、优雅停机(给在飞 turn 一个落完窗口)、日志轮转;
// 以及配对(doPair)、doctor、CLI 分发(RunComputerDaemon)。
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// errNoLocalEngine:PATH 上无任何引擎(退出码 70 的判据)。
var errNoLocalEngine = errors.New("no supported local agent engine found on PATH")

// RunComputerDaemon:`cumora agent computer <argv>` 的完整分发。
func RunComputerDaemon(argv []string) {
	opts := parseArgs(argv)
	switch {
	case opts.help:
		fmt.Print(HelpText())
	case opts.version:
		fmt.Println(currentVersion())
	case opts.doctor:
		doctor()
	case opts.pair != "":
		if err := doPair(opts.pair, opts.server, opts.engine); err != nil {
			fmt.Fprintf(os.Stderr, "cumora: %v\n", err)
			os.Exit(1)
		}
		// 已服务化的机器:重新配对后 reload,让受管 daemon 采新配置,
		// 而不是前台的第二个 daemon 与它竞跑(TS 同)。
		if serviceInstalled() {
			if err := reloadService(); err != nil {
				fmt.Fprintf(os.Stderr, "[computer] service reload failed: %v\n", err)
			} else {
				fmt.Println("[computer] background service reloaded with the new pairing")
			}
		}
	case opts.status:
		printStatus()
	case opts.installService:
		if err := installService(opts.server); err != nil {
			fmt.Fprintf(os.Stderr, "cumora: %v\n", err)
			os.Exit(1)
		}
	case opts.uninstallService:
		if err := uninstallService(); err != nil {
			fmt.Fprintf(os.Stderr, "cumora: %v\n", err)
			os.Exit(1)
		}
	case opts.restart:
		if err := restartService(); err != nil {
			fmt.Fprintf(os.Stderr, "cumora: %v\n", err)
			os.Exit(1)
		}
	case opts.stop:
		stopDaemon()
	case opts.logs:
		tailLogs()
	default:
		if err := doRun(context.Background(), opts.server); err != nil {
			if errors.Is(err, errNoLocalEngine) {
				os.Exit(70) // 对齐 TS:引擎缺失是可诊断的固定退出码
			}
			os.Exit(1)
		}
	}
}

type cliOptions struct {
	pair, server, engine string
	status, version      bool
	doctor, help         bool
	installService       bool
	uninstallService     bool
	restart              bool
	stop                 bool
	logs                 bool
}

func parseArgs(argv []string) cliOptions {
	var out cliOptions
	for i := 0; i < len(argv); i++ {
		switch {
		case argv[i] == "--help" || argv[i] == "-h" || argv[i] == "help":
			out.help = true
		case argv[i] == "--pair":
			if i+1 < len(argv) {
				out.pair = argv[i+1]
				i++
			}
		case hasPrefix(argv[i], "--pair="):
			out.pair = argv[i][len("--pair="):]
		case argv[i] == "--server":
			if i+1 < len(argv) {
				out.server = argv[i+1]
				i++
			}
		case hasPrefix(argv[i], "--server="):
			out.server = argv[i][len("--server="):]
		case argv[i] == "--engine":
			if i+1 < len(argv) {
				out.engine = argv[i+1]
				i++
			}
		case hasPrefix(argv[i], "--engine="):
			out.engine = argv[i][len("--engine="):]
		case argv[i] == "--status":
			out.status = true
		case argv[i] == "--doctor" || argv[i] == "doctor":
			out.doctor = true
		case argv[i] == "--version" || argv[i] == "-v":
			out.version = true
		case argv[i] == "--install-service":
			out.installService = true
		case argv[i] == "--uninstall-service":
			out.uninstallService = true
		case argv[i] == "--restart":
			out.restart = true
		case argv[i] == "--stop":
			out.stop = true
		case argv[i] == "--logs":
			out.logs = true
		}
	}
	return out
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// HelpText:用法面(对齐 agent-cli/cli.ts 的帮助文案要点)。
func HelpText() string {
	return "cumora — run your Cumora agents on this machine (BYOA)\n\n" +
		"Usage:\n" +
		"  cumora agent computer --pair <code> [--server <url>]   pair this machine\n" +
		"  cumora agent computer [--server <url>]                 start the daemon\n\n" +
		"Options:\n" +
		"  --server <url>   target Cumora server (default: CUMORA_SERVER_URL or https://api.cumora.ai)\n" +
		"  --engine <id>    preferred engine for pairing (claude / codex / grok / cursor)\n" +
		"  --install-service   install + start the background supervisor (binary-path unit)\n" +
		"  --uninstall-service remove the background supervisor\n" +
		"  --restart  restart the installed service (also applies a staged update)\n" +
		"  --stop     stop all running daemons (service untouched)\n" +
		"  --logs     how to follow the daemon log\n" +
		"  --status   pairing + running state\n" +
		"  --doctor   check engines, PATH, pairing\n" +
		"  --version  print the daemon version\n" +
		"  --help     this help\n"
}

/* ───────── 配对 ───────── */

// doPair:配对码换 computerId+设备令牌(对齐 TS:首选引擎排首位——
// 服务端把 available_engines[0] 当默认引擎;探测到的引擎按 PATH 存在性)。
func doPair(code, serverURL, preferredEngine string) error {
	if serverURL == "" {
		serverURL = defaultServerURL()
	}
	engines, err := requireLocalEngine()
	if err != nil {
		return err
	}
	if preferredEngine != "" {
		ok := false
		for _, id := range EngineIDs {
			if id == preferredEngine {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("--engine must be one of: %s (got %q)", joinStrings(EngineIDs, ", "), preferredEngine)
		}
		installed := false
		ordered := []string{preferredEngine}
		for _, e := range engines {
			if e == preferredEngine {
				installed = true
				continue
			}
			ordered = append(ordered, e)
		}
		if !installed {
			return fmt.Errorf("--engine %s chosen, but %s is not installed on this machine. Installed: %s or none.", preferredEngine, preferredEngine, joinStrings(engines, ", "))
		}
		engines = ordered
	}
	var paired struct {
		ComputerID  string `json:"computerId"`
		DeviceToken string `json:"deviceToken"`
	}
	if err := apiCall(context.Background(), serverURL, http.MethodPost, "/api/computers/pair", "",
		map[string]any{
			"code":       code,
			"hostName":   detectHostName(),
			"engines":    engines,
			"version":    currentVersion(),
			"supervised": supervised(),
		}, &paired); err != nil {
		return err
	}
	if err := saveConfig(&DaemonConfig{ServerURL: serverURL, ComputerID: paired.ComputerID, DeviceToken: paired.DeviceToken}); err != nil {
		return err
	}
	fmt.Printf("[computer] paired as %s (default engine: %s; available: %s) — starting…\n",
		paired.ComputerID, engines[0], joinStrings(engines, ", "))
	return nil
}

func joinStrings(xs []string, sep string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += sep
		}
		out += x
	}
	return out
}

// defaultServerURL:CUMORA_SERVER_URL 或官方云。
func defaultServerURL() string {
	if u := os.Getenv("CUMORA_SERVER_URL"); u != "" {
		return u
	}
	return "https://api.cumora.ai"
}

/* ───────── doctor / status ───────── */

func doctor() {
	fmt.Printf("cumora %s — doctor\n", currentVersion())
	engines := detectLocalEngines()
	if len(engines) == 0 {
		fmt.Println("engines: NONE found on PATH — install claude / codex / grok / cursor-agent / zcode")
	} else {
		fmt.Printf("engines on PATH: %s\n", joinStrings(engines, ", "))
		for _, id := range engines {
			if getAdapter(id) == nil {
				fmt.Printf("  %s: adapter not yet implemented in the Go daemon (#66)\n", id)
			}
		}
	}
	if cfg, err := loadConfig(); err == nil && cfg.ComputerID != "" {
		fmt.Printf("paired: computer %s @ %s\n", cfg.ComputerID, cfg.ServerURL)
	} else {
		fmt.Println("paired: NO — run: cumora agent computer --pair <code> [--server <url>]")
	}
}

func printStatus() {
	if cfg, err := loadConfig(); err == nil && cfg.ComputerID != "" {
		fmt.Printf("paired:  computer %s @ %s\n", cfg.ComputerID, cfg.ServerURL)
	} else {
		fmt.Println("paired:  NO — run: cumora agent computer --pair <code> [--server <url>]")
	}
	if b, err := os.ReadFile(runningPath()); err == nil {
		var st struct {
			PID     int    `json:"pid"`
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &st) == nil {
			fmt.Printf("running: pid %d (version %s)\n", st.PID, st.Version)
			return
		}
	}
	fmt.Println("running: no")
}

// writeRunningState:--status 读取的运行态(pid+版本,跨日志轮转可靠)。
func writeRunningState() {
	_ = os.MkdirAll(configDir(), 0o755)
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "version": currentVersion()})
	_ = os.WriteFile(runningPath(), b, 0o600)
}

/* ───────── 主循环 ───────── */

// doRun:常驻面——心跳/同步/日志轮转/优雅停机。ctx 取消亦触发优雅停机
// (测试驱动面;进程形态走信号)。返回 error 仅表示启动失败(未配对/无
// 引擎——后者包 errNoLocalEngine,主函数落退出码 70)。
func doRun(ctx context.Context, serverOverride string) error {
	cfg, err := loadConfig()
	if err != nil || cfg.ComputerID == "" {
		fmt.Fprintln(os.Stderr, "[computer] not paired. Run: cumora agent computer --pair <code> [--server <url>]")
		return err
	}
	if serverOverride != "" {
		cfg.ServerURL = serverOverride
	}
	engines, err := requireLocalEngine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[computer] %v\n", err)
		return errNoLocalEngine
	}
	fmt.Printf("[computer] cumora %s · starting %s @ %s (engines: %s)\n",
		currentVersion(), cfg.ComputerID, cfg.ServerURL, joinStrings(engines, ", "))
	writeRunningState()

	// 从入参派生(而非 Background):外部取消/测试驱动能传播到主循环与
	// 全部 runner——此前 Background 直接遮蔽了参数 ctx,取消永远到不了
	// select(心跳测试挂死的根因)。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runners := map[string]*AgentRunner{}

	sync := func() {
		var agents []AgentInfo
		if err := apiCall(ctx, cfg.ServerURL, http.MethodGet, "/api/computers/me/agents",
			cfg.DeviceToken, nil, &agents); err != nil {
			slog.Warn("[computer] agent sync failed", "err", err)
			return
		}
		for _, agent := range agents {
			engine := ""
			if agent.Engine != nil {
				engine = *agent.Engine
			}
			if engine == "" || !containsString(engines, engine) {
				if len(engines) > 0 {
					engine = engines[0]
				}
			}
			adapter := getAdapter(engine)
			if adapter == nil {
				continue // 引擎适配器未实现(#64–#66)
			}
			if existing, ok := runners[agent.ID]; ok {
				if existing.ConfigMatches(agent, engine) {
					continue
				}
				slog.Info("[computer] agent config changed → restarting", "agent", agent.ID, "engine", engine)
				existing.Stop()
				delete(runners, agent.ID)
			}
			runner := newAgentRunner(cfg, agent, adapter)
			runners[agent.ID] = runner
			fmt.Printf("[computer] hosting agent %s (%s) on %s\n", agent.Name, agent.ID, engine)
			runner.Start()
		}
		live := map[string]bool{}
		for _, a := range agents {
			live[a.ID] = true
		}
		for id, runner := range runners {
			if !live[id] {
				runner.Stop()
				delete(runners, id)
			}
		}
	}

	heartbeat := func() {
		_ = apiCall(ctx, cfg.ServerURL, http.MethodPost, "/api/computers/heartbeat",
			cfg.DeviceToken, map[string]any{"version": currentVersion(), "supervised": supervised()}, nil)
	}

	heartbeat()
	sync()
	if len(runners) == 0 {
		fmt.Println("[computer] no agents assigned to this computer yet. Assign one in Cumora; polling…")
	}

	poll := time.NewTicker(agentPollInterval())
	defer poll.Stop()
	beat := time.NewTicker(heartbeatInterval())
	defer beat.Stop()
	logrot := time.NewTicker(logRotateEvery)
	defer logrot.Stop()

	// 自更新(对齐 TS 节拍:60s 首查、每 6h 复查、30s idle watch)。
	// 关键纪律:**绝不为(非紧急的)更新打断在飞 turn**——检测到更新即
	// 停用新二进制(自替换已就位),等全部 agent 空闲才干净退出,由
	// 服务管理器拉起新版。updateReady 只在主循环 goroutine 触碰(回调经
	// channel 送达——评审 M2:goroutine 直写 bool 是数据竞争);就位后
	// 闩住不再复查/重复下载(评审 m3)。
	updateReady := false
	updateReadyCh := make(chan struct{}, 1)
	updateFirst := time.NewTimer(updateFirstCheck)
	defer updateFirst.Stop()
	updateEvery := time.NewTicker(updateCheckEvery)
	defer updateEvery.Stop()
	updateIdle := time.NewTicker(updateIdleWatch)
	defer updateIdle.Stop()
	runUpdateCheck := func() {
		if updateReady {
			return // 已就位——重复下载/替换无益
		}
		go checkForUpdate(ctx, currentVersion(), func() {
			select {
			case updateReadyCh <- struct{}{}:
			default: // 已投递
			}
		})
	}
	allIdle := func() bool {
		for _, r := range runners {
			if r.IsBusy() {
				return false
			}
		}
		return true
	}

	// 优雅停机:停接新唤醒,给在飞 turn 一个落完窗口(保存 session id、
	// finalize run);超窗杀引擎,但 session id 已在盘上——重启后 resume。
	// 信号路径按进程语义 os.Exit(0)(服务管理器据此重启);ctx 取消路径
	// (测试驱动)干净返回——os.Exit 在测试进程里是框架级 panic。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig) // 评审 nit:Notify 后不停收,泄漏到进程末尾
	shutdown := func(why string, exitProcess bool) {
		poll.Stop()
		beat.Stop()
		for _, r := range runners {
			r.BeginStop() // 软停:在飞 turn 继续跑完(grace 窗的本体)
		}
		deadline := time.Now().Add(shutdownGrace())
		for time.Now().Before(deadline) {
			busy := false
			for _, r := range runners {
				if r.IsBusy() {
					busy = true
					break
				}
			}
			if !busy {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		for _, r := range runners {
			r.Stop() // 硬停:宽限窗外掐灭引擎子进程
		}
		fmt.Printf("[computer] shutting down (%s)\n", why)
		if exitProcess {
			os.Exit(0)
		}
	}

	for {
		select {
		case <-sig:
			shutdown("signal", true)
			return nil
		case <-ctx.Done():
			shutdown("context", false)
			return nil
		case <-beat.C:
			heartbeat()
		case <-poll.C:
			sync()
		case <-logrot.C:
			rotateLogsIfNeeded()
		case <-updateReadyCh:
			updateReady = true
			fmt.Println("[computer] update ready — will restart to apply it as soon as all agents are idle")
		case <-updateFirst.C:
			runUpdateCheck()
		case <-updateEvery.C:
			runUpdateCheck()
		case <-updateIdle.C:
			if updateReady && allIdle() {
				for _, r := range runners {
					r.BeginStop()
				}
				for _, r := range runners {
					r.Stop()
				}
				fmt.Println("[computer] shutting down (auto-update)")
				return nil // 受管形态:干净退出,Restart=always/KeepAlive 拉起新二进制
			}
		}
	}
}

// tailLogs:--logs 真流式跟随(TS 同:受管走 journalctl -f,前台 tail -f;
// execv 替换当前进程)。
func tailLogs() {
	if runtime.GOOS == "linux" && serviceInstalled() {
		fmt.Printf("[computer] following journalctl --user -u %s (Ctrl-C to detach)\n", serviceName())
		syscall.Exec(execLookPath("journalctl"), []string{"journalctl", "--user", "-u", serviceName(), "-n", "100", "-f"}, os.Environ())
		return
	}
	logPath := filepath.Join(configDir(), "daemon.log")
	fmt.Printf("[computer] tailing %s (Ctrl-C to detach)\n", logPath)
	syscall.Exec(execLookPath("tail"), []string{"tail", "-n", "100", "-f", logPath}, os.Environ())
}

func execLookPath(bin string) string {
	if p, err := execLookPath2(bin); err == nil {
		return p
	}
	return bin
}

func execLookPath2(bin string) (string, error) {
	return exec.LookPath(bin)
}

func containsString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// rotateLogsIfNeeded:copytruncate 轮转(保 inode——服务管理器以 APPEND
// 持有 fd,重命名会孤儿化它并静默停写)。daemon.log → daemon.log.1 后原地
// 截断,总量 ~2×上限。前台无日志文件时静默跳过。
func rotateLogsIfNeeded() {
	logPath := filepath.Join(configDir(), "daemon.log")
	st, err := os.Stat(logPath)
	if err != nil || st.Size() <= maxLogBytes {
		return
	}
	if err := os.WriteFile(logPath+".1", mustRead(logPath), 0o644); err != nil {
		return
	}
	_ = os.Truncate(logPath, 0)
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}
