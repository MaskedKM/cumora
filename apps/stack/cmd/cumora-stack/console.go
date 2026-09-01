// console —— 桌面管理面的命令面(#286,ADR 0005 §7):releases 清单 /
// form-aware restart / rollback 一键切回。升级本身 = absorb(已有)+
// restart —— 不另设 upgrade 命令,升级语义留在编排层(桌面确认按钮),
// CLI 保持小而正交。
//
// 回滚安全门(#286 AC):目标 release 的 migrations 文件数 < 当前
// release 时,数据可能已被新 schema 迁移过(pg 迁移不可逆)—— 拒绝
// 并说明。静态保守判定:宁可误拒(用户可自行切链)不可误放(旧 server
// 撞新 schema)。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MaskedKM/cumora/apps/stack/internal/stackconfig"
)

// ReleaseEntry —— releases 清单项(status/面板共用面)。
type ReleaseEntry struct {
	Version string `json:"version"`
	Current bool   `json:"current"`
	// Migrations 当前 release 的迁移文件数(回滚安全门的判定原料)。
	Migrations int `json:"migrations"`
	// RolloutBlocked 仅 rollback 语境填:目标比 current 旧且迁移数
	// 更少(pg schema 可能已前移,切回会撞)。
	RolloutBlocked string `json:"rolloutBlocked,omitempty"`
}

func cmdReleases(args []string) int {
	fs := flag.NewFlagSet("releases", flag.ExitOnError)
	releases := fs.String("releases-dir", "", "releases 目录(缺省 toml/内置)")
	currentDir := fs.String("current-dir", "", "current symlink(缺省 toml/内置)")
	jsonOut := fs.Bool("json", false, "JSON 输出")
	_ = fs.Parse(args)
	_, rel, cur, code := consolePaths(*releases, *currentDir)
	if code != 0 {
		return code
	}

	curVer := ""
	if link, err := os.Readlink(cur); err == nil {
		curVer = filepath.Base(link)
	}
	entries, err := listReleases(rel, curVer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "releases: %v\n", err)
		return 1
	}
	if *jsonOut {
		printJSON(entries)
		return 0
	}
	fmt.Println("[releases]")
	for _, e := range entries {
		mark := " "
		if e.Current {
			mark = "*"
		}
		fmt.Printf("  %s %-20s migrations=%d%s\n", mark, e.Version, e.Migrations, blockedSuffix(e.RolloutBlocked))
	}
	return 0
}

func blockedSuffix(s string) string {
	if s == "" {
		return ""
	}
	return "  ✗ " + s
}

// listReleases —— 扫 releases/ 目录:VERSION 文件为准(装好的 release
// 都有;staging 残留以 . 开头自然排除)。
func listReleases(releasesDir, currentVersion string) ([]ReleaseEntry, error) {
	dirs, err := os.ReadDir(releasesDir)
	if err != nil {
		return nil, fmt.Errorf("读 %s: %w", releasesDir, err)
	}
	entries := []ReleaseEntry{}
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		ver := d.Name()
		if data, err := os.ReadFile(filepath.Join(releasesDir, ver, "VERSION")); err == nil {
			ver = strings.TrimSpace(string(data))
		}
		e := ReleaseEntry{Version: ver, Current: ver == currentVersion}
		if ms, err := os.ReadDir(filepath.Join(releasesDir, d.Name(), "migrations")); err == nil {
			for _, m := range ms {
				if !m.IsDir() && strings.HasSuffix(m.Name(), ".sql") {
					e.Migrations++
				}
			}
		}
		if e.Current {
			// 当前版本无回滚概念,不参与安全门。
			e.RolloutBlocked = ""
		} else if currentVersion != "" {
			curMigrations := countMigrations(filepath.Join(releasesDir, currentVersion, "migrations"))
			if e.Migrations < curMigrations {
				e.RolloutBlocked = fmt.Sprintf("目标迁移数 %d < 当前 %d(pg schema 可能已前移,切回不可逆)", e.Migrations, curMigrations)
			}
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Version > entries[j].Version })
	return entries, nil
}

func countMigrations(dir string) int {
	ms, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, m := range ms {
		if !m.IsDir() && strings.HasSuffix(m.Name(), ".sql") {
			n++
		}
	}
	return n
}

// cmdRestart —— form-aware 重启(升级/回滚共用;deploy-release.sh 的
// systemctl 语义下沉):单 unit 形态 restart cumora;三 unit 形态
// restart go+sidecar(daemon 是被管子进程/独立 unit,形态各自处理)。
func cmdRestart(args []string) int {
	_ = args
	form, unit := detectForm()
	switch form {
	case "stackd":
		fmt.Printf("restart: 单 unit 形态(%s)\n", unit)
		if err := systemd("restart", unit); err != nil {
			fmt.Fprintf(os.Stderr, "restart: %v\n", err)
			return 1
		}
	case "legacy":
		fmt.Println("restart: 三 unit 形态(cumora-go + cumora-sidecar;daemon 亦 restart 以对齐制品)")
		failed := false
		for _, u := range []string{"cumora-go", "cumora-sidecar", "cumora-daemon"} {
			if err := systemd("restart", u); err != nil {
				fmt.Fprintf(os.Stderr, "restart: %s: %v\n", u, err)
				failed = true
			}
		}
		if failed {
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "restart: 无已装的 cumora unit(前台/沙箱形态)—— 手动重启你的 stackd 进程")
		return 1
	}
	fmt.Println("restart: 已下发(健康门由 stackd/门探针承担;确认用 cumora-stack doctor)")
	return 0
}

// detectForm —— 与 deploy-release.sh 同语义:stackd unit LoadState=loaded
// = 单 unit;三 unit 任一 loaded = legacy;否则 none。
func detectForm() (form, unit string) {
	if unitLoaded("cumora.service") {
		return "stackd", "cumora.service"
	}
	for _, u := range []string{"cumora-go.service", "cumora-sidecar.service"} {
		if unitLoaded(u) {
			return "legacy", u
		}
	}
	return "none", ""
}

// unitLoaded —— systemctl --user show 的只读探测(容器/无 systemd 环境
// 返回 false,由调用方按 none 形态呈现)。
func unitLoaded(unit string) bool {
	out, err := exec.Command("systemctl", "--user", "show", unit, "-p", "LoadState", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "loaded"
}

// cmdRollback —— current 切回指定 release + form-aware 重启。安全门:
// migrations 数回退即拒(见包注释)。
func cmdRollback(args []string) int {
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	releases := fs.String("releases-dir", "", "releases 目录(缺省 toml/内置)")
	currentDir := fs.String("current-dir", "", "current symlink(缺省 toml/内置)")
	noRestart := fs.Bool("no-restart", false, "只切链不重启(排障用)")
	jsonOut := fs.Bool("json", false, "JSON 输出")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "用法:cumora-stack rollback [flags] <版本号>")
		return 2
	}
	want := fs.Arg(0)
	_, rel, cur, code := consolePaths(*releases, *currentDir)
	if code != 0 {
		return code
	}

	curVer := ""
	if link, err := os.Readlink(cur); err == nil {
		curVer = filepath.Base(link)
	}
	if curVer == want {
		fmt.Fprintf(os.Stderr, "rollback: current 已是 %s\n", want)
		return 1
	}
	target := filepath.Join(rel, want)
	if _, err := os.Stat(filepath.Join(target, "cumora-stackd")); err != nil {
		fmt.Fprintf(os.Stderr, "rollback: %s 不是可用 release(缺 cumora-stackd): %v\n", target, err)
		return 1
	}
	// 安全门:migrations 回退 = pg schema 可能已前移。
	if curVer != "" {
		if n, c := countMigrations(filepath.Join(target, "migrations")),
			countMigrations(filepath.Join(rel, curVer, "migrations")); n < c {
			fmt.Fprintf(os.Stderr,
				"rollback: 拒绝 —— 目标迁移数 %d < 当前 %d;数据已被新 schema 迁移过(pg 迁移不可逆),旧 server 会撞新 schema。确需回退:恢复数据或手动切链自担风险\n", n, c)
			return 1
		}
	}

	// 原子切链(absorb 同款:.new + rename;相对指向)。
	linkTarget, rerr := filepath.Rel(filepath.Dir(cur), target)
	if rerr != nil {
		linkTarget = target
	}
	newLink := cur + ".new"
	if err := os.RemoveAll(newLink); err != nil {
		fmt.Fprintf(os.Stderr, "rollback: %v\n", err)
		return 1
	}
	if err := os.Symlink(linkTarget, newLink); err != nil {
		fmt.Fprintf(os.Stderr, "rollback: 建链失败: %v\n", err)
		return 1
	}
	if err := os.Rename(newLink, cur); err != nil {
		os.RemoveAll(newLink)
		fmt.Fprintf(os.Stderr, "rollback: 切链失败: %v\n", err)
		return 1
	}
	fmt.Printf("rollback: current -> %s\n", want)

	if *noRestart {
		fmt.Println("rollback: --no-restart,链已切未重启")
		return 0
	}
	rc := cmdRestart(nil)
	if *jsonOut {
		printJSON(map[string]any{"rolledBackTo": want, "restartCode": rc})
	}
	return rc
}

// consolePaths —— 三命令共用的路径解析(toml → 内置缺省)。
func consolePaths(releases, current string) (stackconfig.Config, string, string, int) {
	cfg, _, _, cfgErr := loadCfg()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "stack.toml 非法: %v\n", cfgErr)
		return cfg, "", "", 1
	}
	if releases == "" {
		releases = envOr("CUMORA_RELEASES_DIR", cfg.ReleasesDir())
	}
	if current == "" {
		current = envOr("CUMORA_CURRENT_DIR", cfg.CurrentDir())
	}
	return cfg, releases, current, 0
}
