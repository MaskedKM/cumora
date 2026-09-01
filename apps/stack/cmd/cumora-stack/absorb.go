// absorb —— #283 PR-B:AppImage 制品面 → 本地 releases/ 布局的落盘与切换。
//
// 职责边界(票面"bootstrap 解包"项):
//
//	absorb 只管"获得":校验 → 落版本化目录 → 原子切 current symlink。
//	不重启任何服务(切形态=install,升级向导=#284;AppImage 装后可删,
//	栈寻址稳定路径 releases/<ver>/ + current —— ADR 0005 §3、#211 语义)。
//
// 拷贝后不二次哈希(tar 流同理:验源后信任拷贝;磁盘位腐不在此防)。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/MaskedKM/cumora/apps/stack/internal/manifest"
)

// CopyTree —— 递归拷贝保留文件模式(可执行位)。src 内容 → dst(自动建)。
func CopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		in, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		defer in.Close()
		out, cerr := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if cerr != nil {
			return cerr
		}
		if _, cerr = io.Copy(out, in); cerr != nil {
			out.Close()
			return cerr
		}
		return out.Close()
	})
}

// requiredExecutables —— 制品契约面(PR-C 装箱):五栈二进制 +
// redis-server。MANIFEST 校验之外的存在性/可执行位门——装箱 bug
// 缺件时消费侧不设防就是静默半栈(#302 评审 P2-4,deploy-release
// 五件校验的同款语义)。
var requiredExecutables = []string{
	"cumora-server", "cumora-daemon", "cumora-sidecar",
	"cumora-stack", "cumora-stackd", "redis-server",
}

// Absorb —— 校验 srcDir 的 MANIFEST → staging 拷贝 → 写 VERSION →
// 原子 mv 到 releases/<version> → 原子切 current(deploy-release 同款
// .new + mv -T)。返回落盘目录。
//
// 护栏:current 已指向同版本 = 拒绝(重铺会打穿在跑版本目录,ENOENT +
// Restart=always 循环;复验走 restart 即可);current 已存在且非
// symlink = 拒绝并点名(不覆盖非符号链接);非 current 的旧版本目录
// 存在 = 原地替换(不在跑,安全)。
func Absorb(srcDir, releasesDir, currentLink string) (string, error) {
	data, err := os.ReadFile(filepath.Join(srcDir, "MANIFEST"))
	if err != nil {
		return "", fmt.Errorf("源目录缺 MANIFEST: %w", err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return "", err
	}
	if err := manifest.Verify(srcDir, m); err != nil {
		return "", fmt.Errorf("源校验失败: %w", err)
	}
	for _, name := range requiredExecutables {
		fi, serr := os.Stat(filepath.Join(srcDir, name))
		if serr != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			return "", fmt.Errorf("载荷缺可执行件: %s(制品契约:五栈二进制+redis-server)", name)
		}
	}
	if fi, derr := os.Stat(filepath.Join(srcDir, "migrations")); derr != nil || !fi.IsDir() {
		return "", errors.New("载荷缺 migrations/ 目录(server 启动需要)")
	}

	if err := os.MkdirAll(releasesDir, 0o755); err != nil {
		return "", err
	}
	target := filepath.Join(releasesDir, m.Version)
	if fi, serr := os.Lstat(currentLink); serr == nil && fi.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("current 已存在且不是 symlink(%v)——先手动处理;absorb 不覆盖非符号链接", fi.Mode())
	}
	if cur, lerr := os.Readlink(currentLink); lerr == nil && filepath.Base(cur) == m.Version {
		return "", fmt.Errorf("current 已指向 %s —— 同版本重铺会打穿在跑目录;复验用 systemctl --user restart,确要重铺先切走别的版本", m.Version)
	}

	staging := filepath.Join(releasesDir, fmt.Sprintf(".staging-absorb-%s.%d", m.Version, os.Getpid()))
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := CopyTree(srcDir, staging); err != nil {
		return "", fmt.Errorf("落盘拷贝失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "VERSION"), []byte(m.Version+"\n"), 0o644); err != nil {
		return "", err
	}
	// 非 current 的旧版本目录:不在跑,原地替换(先删后 mv,防 mv 套娃)。
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	if err := os.Rename(staging, target); err != nil {
		return "", fmt.Errorf("版本目录就位失败: %w", err)
	}

	// symlink 指向按 current 所在目录相对计算(默认布局=releases/<ver>;
	// --releases-dir/--current-dir 拆到不同父目录时不断链——评审 P2-3)。
	linkTarget, rerr := filepath.Rel(filepath.Dir(currentLink), target)
	if rerr != nil {
		linkTarget = target // 极端布局退化绝对指向,仍可用
	}
	newLink := currentLink + ".new"
	if err := os.RemoveAll(newLink); err != nil {
		return "", err
	}
	if err := os.Symlink(linkTarget, newLink); err != nil {
		return "", err
	}
	if err := os.Rename(newLink, currentLink); err != nil {
		os.RemoveAll(newLink) // 切换失败不留 .new 残留(评审 P2-2)
		return "", fmt.Errorf("current 切换失败: %w", err)
	}
	return target, nil
}

func cmdAbsorb(args []string) int {
	fs := flag.NewFlagSet("absorb", flag.ExitOnError)
	releases := fs.String("releases-dir",
		envOr("CUMORA_RELEASES_DIR", home(".local/share/cumora/releases")), "releases 目录")
	current := fs.String("current-dir",
		envOr("CUMORA_CURRENT_DIR", home(".local/share/cumora/current")), "current symlink 本体")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "用法:cumora-stack absorb [flags] <制品载荷目录>(含 MANIFEST;flags 须在目录参数前)")
		return 2
	}
	target, err := Absorb(fs.Arg(0), *releases, *current)
	if err != nil {
		fmt.Fprintf(os.Stderr, "absorb: 错误:%v\n", err)
		return 1
	}
	fmt.Printf("absorb: 落盘 %s\ncurrent -> releases/%s(原子切换完成)\n", target, filepath.Base(target))
	fmt.Println("后续:cumora-stack install(新机器切单 unit)/ systemctl --user restart cumora(已装形态)")
	return 0
}
