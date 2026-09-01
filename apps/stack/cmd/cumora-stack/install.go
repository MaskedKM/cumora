// install —— `cumora-stack install/uninstall/logs`(#282 PR-B):
// 单 unit 切换的执行面。install = 写 unit + 停用旧三 unit(文件保留,
// 回滚路径)+ enable 新 unit;uninstall = 反向(恢复旧三 unit)。
// 顺序护栏:install 前置校验 current 制品含 cumora-stackd,否则拒绝
// ——旧 release 配新 unit 会把 ExecStart 打穿。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

var oldUnits = []string{"cumora-sidecar.service", "cumora-go.service", "cumora-daemon.service"}

const unitName = "cumora.service"

// UnitContent —— systemd user unit 内容(纯函数,install 的对照面)。
// 单 unit 只保证 stackd 存在;链式拉起/健康门/退避在 stackd 进程内。
func UnitContent(currentDir, envFile, workDir string) string {
	return fmt.Sprintf(`# Cumora Stack supervisor(#282,ADR 0005 阶段 1):
# 本 unit 只保证 stackd 存在(Restart=always + 开机入口);pg→redis→
# sidecar→server→daemon 的拉起、健康门、退避、熔断全在 stackd 进程内
# (apps/stack/internal/stackd)。旧三 unit 由 cumora-stack install 停用
# 禁用但文件保留 —— uninstall 即回滚。
[Unit]
Description=Cumora Stack supervisor (stackd)
After=network-online.target

[Service]
WorkingDirectory=%s
EnvironmentFile=%s
ExecStart=%s
Restart=always
RestartSec=3
# 健康门由 stackd 链式拉起承担(sidecar healthz → server livez),不再
# 需要启动后置探针;启动预算盖住整链最坏和:external 60+60 + sidecar 门
# 60 + server 门 120 = 300s,再留 spawn/慢起余量(评审 P2-4)。
TimeoutStartSec=420

[Install]
WantedBy=default.target
`, workDir, envFile, filepath.Join(currentDir, "cumora-stackd"))
}

func systemd(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	current := fs.String("current-dir",
		envOr("CUMORA_CURRENT_DIR", home(".local/share/cumora/current")), "release 制品目录")
	envFile := fs.String("env-file",
		envOr("CUMORA_ENV_FILE", home("Code/cumora/.env")), "主 .env(unit EnvironmentFile)")
	work := fs.String("work-dir", envOr("CUMORA_WORK_DIR", home("Code/cumora")), "子进程工作目录")
	_ = fs.Parse(args)

	// 顺序护栏:制品必须已含 stackd(否则 ExecStart 打穿)。
	for _, bin := range []string{"cumora-stackd", "cumora-sidecar", "cumora-server", "cumora-daemon"} {
		p := filepath.Join(*current, bin)
		if st, err := os.Stat(p); err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			fmt.Fprintf(os.Stderr, "install: %s 缺失/不可执行 —— 先 bash scripts/deploy/deploy-release.sh <tag>(制品需含 #282 后五件)\n", p)
			return 1
		}
	}

	dest := home(".config/systemd/user")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		return 1
	}
	unitPath := filepath.Join(dest, unitName)
	if err := os.WriteFile(unitPath, []byte(UnitContent(*current, *envFile, *work)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "install: 写 unit 失败: %v\n", err)
		return 1
	}
	if err := systemd("daemon-reload"); err != nil {
		fmt.Fprintf(os.Stderr, "install: daemon-reload 失败: %v\n", err)
		return 1
	}

	// 停用旧三 unit(文件保留 = 回滚);disable --now 幂等,未装也不报错。
	fmt.Println("install: 停用旧三 unit(文件保留,uninstall 可回滚)")
	if err := systemd(append([]string{"disable", "--now"}, oldUnits...)...); err != nil {
		fmt.Fprintf(os.Stderr, "install: 旧 unit 停用失败(可继续,但请核查): %v\n", err)
	}

	if err := systemd("enable", "--now", unitName); err != nil {
		fmt.Fprintf(os.Stderr, "install: enable %s 失败: %v\n", unitName, err)
		fmt.Fprintf(os.Stderr, "  此时旧三 unit 已停 —— 手动恢复:systemctl --user enable --now cumora-sidecar cumora-go cumora-daemon\n  (或排查后重试 cumora-stack install)\n")
		return 1
	}
	fmt.Printf("install: %s 已启用。状态:cumora-stack status;日志:cumora-stack logs -f\n", unitName)
	fmt.Println("回滚:cumora-stack uninstall")
	return 0
}

func cmdUninstall(args []string) int {
	_ = flag.NewFlagSet("uninstall", flag.ExitOnError).Parse(args)
	if err := systemd("disable", "--now", unitName); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: 停用 %s 失败(未装?): %v\n", unitName, err)
	}
	unitPath := filepath.Join(home(".config/systemd/user"), unitName)
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "uninstall: 删 unit 文件失败: %v\n", err)
	}
	_ = systemd("daemon-reload")
	// 恢复旧三 unit(#211 enable 链:sidecar → go → daemon)。
	if err := systemd(append([]string{"enable", "--now"}, oldUnits...)...); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: 恢复旧三 unit 失败: %v\n", err)
		return 1
	}
	fmt.Println("uninstall: 已回滚到三 unit 形态(旧 unit 文件从未被删)")
	return 0
}

// cmdLogs —— journalctl 透传:默认 -o cat + svc 标签原样(结构化行),
// 额外参数原样追加(-f、--since 等)。
func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "持续跟随")
	svc := fs.String("svc", "", "按 svc= 标签过滤(server/sidecar/daemon…)")
	_ = fs.Parse(args)

	jargs := []string{"--user", "-u", unitName, "-o", "cat"}
	if *follow {
		jargs = append(jargs, "-f")
	}
	if *svc != "" {
		// 结构化行形如 `msg=child log svc=server line=…`:grep 式过滤。
		jargs = append(jargs, "--grep", "svc="+*svc+"\\b")
	}
	jargs = append(jargs, fs.Args()...)
	cmd := exec.Command("journalctl", jargs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		return 1
	}
	return 0
}
