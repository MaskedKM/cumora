// workspaces 域 versions —— #337 写前快照与冲突副本(刀 2 防护三件套
// 的落盘半边)。
//
//   - Versions(写前快照):server 侧一切破坏性写(write 覆盖/append/
//     edit/delete/mv)与挂载写感知(watcher 上报的 mtime 变化)都先把
//     旧内容留档进 <folder>/.cumora/versions/<rel>/<unixnano>,每文件
//     保留最近 10 版 —— 与文件夹同生命周期,不进 DB(ADR 0006)。
//   - Conflicted copy(CAS 挑战者留底):expected-mtime 失配时挑战者的
//     新内容不丢弃,写成 <abs>.conflict-<principal>-<unixts> 与原文件
//     同目录双份并存;挑战者重读最新内容后自行合并 —— symlink 挂载是
//     同一 inode,无 Syncthing 式双端副本分叉,CAS 时刻是唯一可精确
//     检测的分叉点(设计修正见 #337 PR)。
package workspaces

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// versionsRoot:.cumora/versions(平台内部,CLI/HTTP 面全拒)。
func versionsRoot(folder string) string { return filepath.Join(folder, ".cumora", "versions") }

// reservedPrefix:.cumora 平台内部目录名。大小写不敏感比较 —— macOS
// 大小写不敏感盘上 .CUMORA 可绕纯前缀检查落进同目录(#339 评审余量,
// ADR 0006 信任域内,Linux 生产无虞,防御纵深)。
const reservedPrefix = ".cumora"

// RejectReserved:rel 命中平台内部目录(.cumora,大小写不敏感)时返回
// 拒绝文案。CLI 面(agent 包 cliRejectReserved)与 HTTP 面共用语义。
func RejectReserved(rel string) string {
	first := rel
	if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
		first = rel[:i]
	}
	if strings.EqualFold(first, reservedPrefix) {
		return "reserved path (.cumora is platform-internal)"
	}
	return ""
}

// maxFileVersions:每文件保留的快照版数(票面 #337)。
const maxFileVersions = 10

// SnapshotVersion:把 folder/rel 的当前内容留档进 versions/(目录或不
// 存在跳过;>maxFileBytes 跳过 —— 快照面与读面同帽)。尽力而为:失败
// 只记日志,不阻断触发它的主写(可用性优先;快照失败 ≠ 拒绝写入)。
func SnapshotVersion(folder, rel string) {
	if RejectReserved(rel) != "" {
		return
	}
	abs := filepath.Join(folder, filepath.FromSlash(rel))
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() || st.Size() > maxFileBytes {
		return
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	dir := filepath.Join(versionsRoot(folder), filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("[workspaces] snapshot mkdir failed", "rel", rel, "err", err)
		return
	}
	name := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		slog.Warn("[workspaces] snapshot write failed", "rel", rel, "err", err)
		return
	}
	trimVersions(dir)
}

// trimVersions:目录内版本文件按名字(=unixnano 字典序=时间序)排序,
// 超出保留数的删最旧。
func trimVersions(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= maxFileVersions {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.Trim(e.Name(), "0123456789") == "" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-maxFileVersions] {
		_ = os.Remove(filepath.Join(dir, n))
	}
}

// sanitizePrincipal:副本后缀里的作者标识只留安全字符集,防路径注入。
func sanitizePrincipal(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return "unknown"
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// SaveConflictCopy:CAS 失配时把挑战者内容写成与原文件同目录的
// <name>.conflict-<principal>-<unixts> 副本,返回副本的 rel 路径
// (写失败返回空串,调用方文案降级)。
func SaveConflictCopy(folder, rel, principal, content string) string {
	if RejectReserved(rel) != "" || len(content) > maxFileBytes {
		return ""
	}
	dir := filepath.Dir(filepath.Join(folder, filepath.FromSlash(rel)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	ext := filepath.Ext(rel)
	stem := strings.TrimSuffix(filepath.Base(rel), ext)
	name := stem + ".conflict-" + sanitizePrincipal(principal) + "-" +
		strconv.FormatInt(time.Now().Unix(), 10) + ext
	abs := filepath.Join(dir, name)
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		slog.Warn("[workspaces] conflict copy failed", "rel", rel, "err", err)
		return ""
	}
	relOut, err := filepath.Rel(folder, abs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(relOut)
}
