// daemon 包 skills_sync —— #261 公司 Skills 物化:每 agent 同步周期把
// 公司手册按内容寻址键落进引擎原生 skills 目录(.claude/skills 等),
// 引擎加载器渐进披露直接可用。stamp 文件记 name→bundle_hash:哈希不变
// 跳过(零网络零写盘),变更整包重写,清单消失整目录回收。
package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
// 同内容共享;同轮多 agent 同公司不重复拉)。
type skillSyncer struct {
	ctx   context.Context
	cfg   *DaemonConfig
	mu    sync.Mutex
	cache map[string]*skillBundle // bundleHash → bundle(nil = 拉取失败)
}

func newSkillSyncer(ctx context.Context, cfg *DaemonConfig) *skillSyncer {
	return &skillSyncer{ctx: ctx, cfg: cfg, cache: map[string]*skillBundle{}}
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

// materializeAgent:把 refs 中属于 agent 公司的技能物化进 home。错误只
// 记日志——手册缺失不许影响唤醒主路径。
func (s *skillSyncer) materializeAgent(agent AgentInfo, home string, refs []companySkillRef) {
	dir := engineSkillsDir(engineOf(agent), home)
	if dir == "" || agent.CompanyID == "" {
		return
	}
	var mine []companySkillRef
	for _, ref := range refs {
		if ref.CompanyID == agent.CompanyID {
			mine = append(mine, ref)
		}
	}
	if err := materializeSkillDir(dir, mine, s.bundle); err != nil {
		slog.Warn("[computer] skill sync failed", "agent", agent.ID, "err", err)
	}
}

// materializeSkillDir:目录级物化(与 agent 解耦,便于单测直驱)。
// fetch 返回 nil = 拉取失败:该技能本轮跳过,stamp 保留旧值(下轮再试),
// 不清既有目录——宁可陈旧不可缺失。
func materializeSkillDir(dir string, refs []companySkillRef, fetch func(hash string) *skillBundle) error {
	stamps, err := readSkillStamps(dir)
	if err != nil {
		return err
	}
	next := map[string]string{}
	for _, ref := range refs {
		if stamps[ref.Name] == ref.BundleHash {
			next[ref.Name] = ref.BundleHash
			continue
		}
		b := fetch(ref.BundleHash)
		if b == nil || len(b.Files) == 0 {
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

func writeSkillStamps(dir string, stamps map[string]string) error {
	// encoding/json 对 map 键按字典序输出——落盘字节天然稳定。
	b, err := json.MarshalIndent(stamps, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, stampsFile), b, 0o644)
}

// engineOf:AgentInfo.Engine 的解引用(空引擎 → 空串 → 物化跳过)。
func engineOf(a AgentInfo) string {
	if a.Engine == nil {
		return ""
	}
	return *a.Engine
}
