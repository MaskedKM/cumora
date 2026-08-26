// daemon 包 service —— 服务化安装与守护(#67):per-user supervisor
// (macOS LaunchAgent / Linux systemd --user)。与 TS 的差异即本票本体:
// 单元 ExecStart 指向**二进制自身路径**(TS 指 npx cumora@latest——npm 通道
// 停版,自更新改由 daemon 的 Releases 自替换 + 服务管理器重启完成)。
// 单元名可用 CUMORA_SERVICE_NAME 覆盖(多实例/演练不与生产服务互踩)。
package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
)

// serviceName:服务单元名。默认 cumora;CUMORA_SERVICE_NAME 覆盖(多实例、
// 或本机演练不想覆盖既有生产服务)。
func serviceName() string {
	if v := strings.TrimSpace(os.Getenv("CUMORA_SERVICE_NAME")); v != "" {
		return v
	}
	return "cumora"
}

func darwinPlistPath() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", serviceName()+".plist")
}

func linuxUnitPath() string {
	return filepath.Join(homeDir(), ".config", "systemd", "user", serviceName()+".service")
}

func serviceInstalled() bool {
	switch runtime.GOOS {
	case "darwin":
		return pathExists(darwinPlistPath())
	case "linux":
		return pathExists(linuxUnitPath())
	}
	return false
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, truncate(string(out), 200))
	}
	return nil
}

// installService:安装并启动 per-user supervisor。必须先配对(单元里钉
// --server,重启不依赖 shell 历史)。ExecStart= <自身二进制> —— systemd/
// launchd 不经 PATH 解析,升级后的二进制在同路径原地生效。
func installService(serverURL string) error {
	if _, err := loadConfig(); err != nil {
		return fmt.Errorf("pair this computer first: cumora agent computer --pair <code>")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	logPath := filepath.Join(configDir(), "daemon.log")
	_ = os.MkdirAll(configDir(), 0o755)

	if runtime.GOOS == "darwin" {
		dir := filepath.Join(homeDir(), "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		plistPath := darwinPlistPath()
		args := []string{self, "agent", "computer", "--server", serverURL}
		plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + serviceName() + `</string>
  <key>ProgramArguments</key><array>` + plistArgs(args) + `</array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + logPath + `</string>
  <key>StandardErrorPath</key><string>` + logPath + `</string>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>` + os.Getenv("PATH") + `</string>
    <key>CUMORA_SUPERVISED</key><string>1</string>
  </dict>
</dict></plist>
`
		if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
			return err
		}
		_ = runCmd("launchctl", "unload", plistPath) // 未加载时静默
		if err := runCmd("launchctl", "load", plistPath); err != nil {
			return err
		}
		fmt.Printf("[computer] installed LaunchAgent %s (binary: %s) — auto-start, auto-restart, auto-update. Logs: %s\n", serviceName(), self, logPath)
		fmt.Println("[computer] you can now close this terminal; the service is running in the background.")
		return nil
	}

	if runtime.GOOS == "linux" {
		dir := filepath.Join(homeDir(), ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		unitPath := linuxUnitPath()
		if err := os.WriteFile(unitPath, []byte(linuxUnit(self, serverURL)), 0o644); err != nil {
			return err
		}
		if err := runCmd("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := runCmd("systemctl", "--user", "enable", "--now", serviceName()); err != nil {
			return err
		}
		fmt.Printf("[computer] installed systemd --user service '%s' (binary: %s) — auto-start, auto-restart, auto-update. Logs: journalctl --user -u %s -f\n", serviceName(), self, serviceName())
		return nil
	}
	return fmt.Errorf("--install-service supports macOS and Linux (not %s)", runtime.GOOS)
}

func plistArgs(args []string) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteString("<string>" + strings.ReplaceAll(a, "&", "&amp;") + "</string>")
	}
	return b.String()
}

func uninstallService() error {
	if runtime.GOOS == "darwin" {
		plistPath := darwinPlistPath()
		_ = runCmd("launchctl", "unload", plistPath)
		_ = os.Remove(plistPath)
		fmt.Printf("[computer] removed LaunchAgent %s\n", serviceName())
		return nil
	}
	if runtime.GOOS == "linux" {
		_ = runCmd("systemctl", "--user", "disable", "--now", serviceName())
		_ = os.Remove(linuxUnitPath())
		_ = runCmd("systemctl", "--user", "daemon-reload")
		fmt.Printf("[computer] removed systemd --user service '%s'\n", serviceName())
		return nil
	}
	return fmt.Errorf("--uninstall-service supports macOS and Linux (not %s)", runtime.GOOS)
}

// reloadService:重启已安装的服务,使其重读 computer.json(重新配对后)。
func reloadService() error {
	if runtime.GOOS == "darwin" {
		p := darwinPlistPath()
		_ = runCmd("launchctl", "unload", p)
		return runCmd("launchctl", "load", p)
	}
	if runtime.GOOS == "linux" {
		return runCmd("systemctl", "--user", "restart", serviceName())
	}
	return fmt.Errorf("reload supports macOS and Linux (not %s)", runtime.GOOS)
}

// restartService:--restart 的友好包装(也是"立即应用更新"按钮——服务重
// 启即拉起替换后的新二进制)。
func restartService() error {
	if !serviceInstalled() {
		fmt.Println("[computer] service not installed — run: cumora agent computer --install-service")
		return nil
	}
	if runtime.GOOS == "darwin" {
		uid := os.Getuid()
		if err := runCmd("launchctl", "kickstart", "-k", fmt.Sprintf("gui/%d/%s", uid, serviceName())); err != nil {
			return reloadService() // 未在加载态 → load 它
		}
	} else if runtime.GOOS == "linux" {
		if err := runCmd("systemctl", "--user", "restart", serviceName()); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("--restart supports macOS and Linux (not %s)", runtime.GOOS)
	}
	fmt.Println("[computer] service restarted — it relaunches the current binary (also applies any staged update). Check: cumora agent computer --status")
	return nil
}

// oneShotFlagRe:一次性 CLI 调用(做完即退,不是 --stop 要杀的长驻
// daemon)。锚定 -- 使词不会误配命令行他处。
var oneShotFlagRe = regexp.MustCompile(`--(stop|status|restart|logs|version|install-service|uninstall-service|pair)\b`)

// isStoppableDaemonCommand:ps 报告的命令行是否为 --stop 应杀的长驻
// daemon——仅 "agent computer" 且无一次性旗。
func isStoppableDaemonCommand(cmd string) bool {
	return strings.Contains(cmd, "agent computer") && !oneShotFlagRe.MatchString(cmd)
}

// stopDaemon:杀掉在跑的 daemon(受管的卸载后、前台的、或孤儿)。来源:
// running.json 的 pid + "agent computer" 的 pgrep 扫描;逐候选核实命令行
// 真是长驻 daemon 且不是本 --stop 一次性进程,再 SIGTERM。
func stopDaemon() {
	kill := func(pid int, cmdline string) {
		if pid <= 0 || pid == os.Getpid() {
			return
		}
		if !isStoppableDaemonCommand(cmdline) {
			return
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		_ = p.Signal(syscall.SIGTERM)
		fmt.Printf("[computer] stopped daemon pid %d (%s)\n", pid, truncate(cmdline, 80))
	}
	if b, err := os.ReadFile(runningPath()); err == nil {
		var st struct {
			PID int `json:"pid"`
		}
		if jsonUnmarshal(string(b), &st) == nil && st.PID > 0 {
			if cl := procCmdline(st.PID); cl != "" {
				kill(st.PID, cl)
			}
		}
	}
	if out, err := exec.Command("pgrep", "-f", "agent computer").Output(); err == nil {
		seen := map[int]bool{}
		for _, field := range strings.Fields(string(out)) {
			var pid int
			if _, err := fmt.Sscanf(field, "%d", &pid); err != nil || pid <= 0 || seen[pid] {
				continue
			}
			seen[pid] = true
			if cl := procCmdline(pid); cl != "" {
				kill(pid, cl)
			}
		}
	}
	_ = os.Remove(runningPath())
	fmt.Println("[computer] stopped all running daemons (service not touched — reinstall with --install-service)")
}

// procCmdline:/proc/<pid>/cmdline(Linux);非 Linux 走 ps。空 = 不存在。
func procCmdline(pid int) string {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.ReplaceAll(string(b), "\x00", " ")
	}
	if out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "command=").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// linuxUnit:systemd --user 单元模板。ExecStart 指向二进制自身(不经
// PATH/npx——自替换后同路径原地生效);Restart=always 让"空闲干净退出
// 应用更新"由管理器拉起完成。
func linuxUnit(self, serverURL string) string {
	return "[Unit]\n" +
		"Description=Cumora BYOA daemon\n" +
		"After=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=" + self + " agent computer --server " + serverURL + "\n" +
		"Restart=always\n" +
		"RestartSec=5\n" +
		"Environment=PATH=" + os.Getenv("PATH") + "\n" +
		"Environment=CUMORA_SUPERVISED=1\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
}
