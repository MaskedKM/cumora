// daemon 包 skills_sync —— #261 公司 Skills 物化:每 agent 同步周期把
// 公司手册按内容寻址键落进引擎原生 skills 目录(.claude/skills 等),
// 引擎加载器渐进披露直接可用。stamp 文件记 name→bundle_hash:哈希不变
// 跳过(零网络零写盘),变更整包重写,清单消失整目录回收。
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// companySkillRef:/api/computers/me/skills 清单行。
type companySkillRef struct {
	CompanyID   string `json:"companyId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	BundleHash  string `json:"bundleHash"`
}

// skillBundleFile:/api/computers/me/skills/{hash} 包内文件。
type skillBundleFile struct {
	Path string `json:"path"`
	Body string `json:"body"`
}

type skillBundle struct {
	Name  string            `json:"name"`
	Files []skillBundleFile `json:"files"`
}

// stampsFile:物化状态名。点前缀 + 非 SKILL.md 形状,各引擎的一级目录
// 扫描都不把它当技能(claude/zcode 找的是 <name>/SKILL.md 子目录)。
const stampsFile = ".cumora-skill-stamps.json"

// engineSkillsDir:引擎原生 skills 目录(适配器 ID 定址)。五引擎全落
// Agent Skills 开放标准(SKILL.md 子目录):claude/.claude/skills、
// codex/.codex/skills(2025-12 起官方支持)、cursor/.cursor/skills、
// zcode/home 根 skills/(其 SeedHome 即此形)。grok 与 claude 同目录:
// 其人格头本就引用 .claude/skills/(SeedHome 传入的 skillsDir),物化
// 进同一处保持一致。
func engineSkillsDir(adapterID, home string) string {
	switch adapterID {
	case "claude", "grok":
		return filepath.Join(home, ".claude", "skills")
	case "codex":
		return filepath.Join(home, ".codex", "skills")
	case "cursor":
		return filepath.Join(home, ".cursor", "skills")
	case "zcode":
		return filepath.Join(home, "skills")
	default:
		return ""
	}
}

// skillSyncer:一轮同步的物化器。清单拉一次,整包按哈希记忆化(公司间
// 同内容共享;同轮多 agent 同公司不重复拉);内置技能(cumora-*)预填
// 缓存——零网络,与公司手册同一管线。
type skillSyncer struct {
	ctx   context.Context
	cfg   *DaemonConfig
	mu    sync.Mutex
	cache map[string]*skillBundle // bundleHash → bundle(nil = 拉取失败)
}

func newSkillSyncer(ctx context.Context, cfg *DaemonConfig) *skillSyncer {
	s := &skillSyncer{ctx: ctx, cfg: cfg, cache: map[string]*skillBundle{}}
	for _, b := range builtinSkills {
		bundle := b.bundle // 拷贝防共享可变底层数组
		s.cache[b.ref.BundleHash] = &bundle
	}
	return s
}

// builtinRefs:内置技能引用(每个 agent 都有,与公司无关)。
func builtinRefs() []companySkillRef {
	out := make([]companySkillRef, 0, len(builtinSkills))
	for _, b := range builtinSkills {
		out = append(out, b.ref)
	}
	return out
}

// list:公司 skills 清单(拉取失败返回 nil,调用方按无变化处理)。
func (s *skillSyncer) list() []companySkillRef {
	var refs []companySkillRef
	if err := apiCall(s.ctx, s.cfg.ServerURL, http.MethodGet, "/api/computers/me/skills",
		s.cfg.DeviceToken, nil, &refs); err != nil {
		return nil
	}
	return refs
}

// bundle:按哈希取整包(带同轮缓存;失败记忆化为 nil,不再重试到下一轮)。
func (s *skillSyncer) bundle(hash string) *skillBundle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.cache[hash]; ok {
		return b
	}
	var b skillBundle
	err := apiCall(s.ctx, s.cfg.ServerURL, http.MethodGet,
		"/api/computers/me/skills/"+hash, s.cfg.DeviceToken, nil, &b)
	if err != nil || len(b.Files) == 0 {
		b = skillBundle{}
	}
	s.cache[hash] = &b
	return &b
}

// materializeAgent:把内置技能 + refs 中属于 agent 公司的技能物化进 home
// (并成一张清单一次物化——分两次调用会让彼此的 stamp 互当"清单消失"
// 误回收)。错误只记日志——手册缺失不许影响唤醒主路径。物化目录按
// adapterID 定址(而非 agent 声明引擎:run.go 的同步循环在本机无声明
// 引擎时回落 engines[0],runner.adapter 才是实际在跑的引擎)。
func (s *skillSyncer) materializeAgent(agent AgentInfo, adapterID, home string, refs []companySkillRef) {
	dir := engineSkillsDir(adapterID, home)
	if dir == "" {
		return
	}
	mine := append([]companySkillRef{}, builtinRefs()...)
	for _, ref := range refs {
		if agent.CompanyID != "" && ref.CompanyID == agent.CompanyID {
			mine = append(mine, ref)
		}
	}
	if err := materializeSkillDir(dir, mine, s.bundle); err != nil {
		slog.Warn("[computer] skill sync failed", "agent", agent.ID, "err", err)
	}
}

// skillNameRe:服务端 create/update 同款名字规则。daemon 侧再校验一遍是
// 纵深防御——物化端直接 RemoveAll/写盘,不裸信网络回包。
var skillNameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// bundleMatchesHash:按请求哈希复核整包(sha256 重算),错内容不落盘。
func bundleMatchesHash(hash string, files []skillBundleFile) bool {
	return bundleHashDaemon(files) == hash
}

// materializeSkillDir:目录级物化(与 agent 解耦,便于单测直驱)。
// fetch 返回 nil = 拉取失败:该技能本轮跳过,stamp 保留旧值(下轮再试),
// 不清既有目录——宁可陈旧不可缺失。stamp 命中但目录被手删 → 重物化自愈。
func materializeSkillDir(dir string, refs []companySkillRef, fetch func(hash string) *skillBundle) error {
	stamps, err := readSkillStamps(dir)
	if err != nil {
		return err
	}
	next := map[string]string{}
	for _, ref := range refs {
		if !skillNameRe.MatchString(ref.Name) {
			continue // 防御:坏名字不许进 RemoveAll/Join
		}
		if stamps[ref.Name] == ref.BundleHash && pathExists(filepath.Join(dir, ref.Name)) {
			next[ref.Name] = ref.BundleHash
			continue
		}
		b := fetch(ref.BundleHash)
		if b == nil || len(b.Files) == 0 || !bundleMatchesHash(ref.BundleHash, b.Files) {
			next[ref.Name] = stamps[ref.Name] // 陈旧但保命
			continue
		}
		target := filepath.Join(dir, ref.Name)
		_ = os.RemoveAll(target)
		if err := writeSkillFiles(target, b.Files); err != nil {
			next[ref.Name] = stamps[ref.Name]
			continue
		}
		next[ref.Name] = ref.BundleHash
	}
	// 清单消失的技能整目录回收(删库是显式语义,不是拉取失败)。
	for name := range stamps {
		if _, ok := next[name]; !ok {
			_ = os.RemoveAll(filepath.Join(dir, name))
		}
	}
	if stampsEqual(stamps, next) {
		return nil // 无变化零写盘
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeSkillStamps(dir, next)
}

func writeSkillFiles(target string, files []skillBundleFile) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		// 纵深防御:服务端已校验,但写盘端不裸信网络回包。
		if f.Path == "" || f.Path == "." || strings.HasPrefix(f.Path, "/") ||
			strings.Contains(f.Path, "..") || strings.ContainsRune(f.Path, 0) {
			return fmt.Errorf("unsafe path in bundle: %q", f.Path)
		}
		p := filepath.Join(target, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(f.Body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// bundleHashDaemon:与 server domains/skills.bundleHash 同算法(文件按
// path 排序的长度前缀拼接体 sha256)——拉回的整包按请求哈希复核,错
// 内容不落盘。
func bundleHashDaemon(files []skillBundleFile) string {
	sorted := append([]skillBundleFile(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%d:%s%d:%s", len(f.Path), f.Path, len(f.Body), f.Body)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readSkillStamps(dir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, stampsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var stamps map[string]string
	if err := json.Unmarshal(b, &stamps); err != nil {
		// 坏 stamp 文件:当空处理,整目录按清单重物化自愈。
		return map[string]string{}, nil
	}
	return stamps, nil
}

// stampsEqual:浅比较(键值集相同)。stamp 未变就不重写——物化轮次里
// 命中路径零写盘。
func stampsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func writeSkillStamps(dir string, stamps map[string]string) error {
	// encoding/json 对 map 键按字典序输出——落盘字节天然稳定。
	b, err := json.MarshalIndent(stamps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, stampsFile), b, 0o644)
}
