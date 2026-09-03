// skills_sync 内部测试:物化三态(命中跳过/变更重写/消失回收)+ 拉取
// 失败保旧 + 哈希复核 + 引擎目录定址。纯文件系统直驱,不起 HTTP。
package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readSkillTest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// refOf:按文件集算真哈希(物化端按 sha256 复核整包,假哈希会被拒)。
func refOf(company, name string, files ...skillBundleFile) companySkillRef {
	return companySkillRef{CompanyID: company, Name: name, BundleHash: bundleHashDaemon(files)}
}

func bundleOf(name string, files ...skillBundleFile) *skillBundle {
	return &skillBundle{Name: name, Files: files}
}

var skillMdV1 = []skillBundleFile{
	{Path: "SKILL.md", Body: "---\nname: deploy-runbook\n---\n\nv1"},
	{Path: "references/rollback.md", Body: "rollback steps"},
}

func TestMaterializeSkillDirWritesAndSkips(t *testing.T) {
	dir := t.TempDir()
	ref := refOf("co", "deploy-runbook", skillMdV1...)
	refs := []companySkillRef{ref}
	fetch := func(hash string) *skillBundle {
		if hash != ref.BundleHash {
			t.Fatalf("fetch hash mismatch")
		}
		return bundleOf("deploy-runbook", skillMdV1...)
	}
	if err := materializeSkillDir(dir, refs, fetch); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, "deploy-runbook", "SKILL.md")); got != skillMdV1[0].Body {
		t.Fatalf("SKILL.md body = %q", got)
	}
	if got := readSkillTest(t, filepath.Join(dir, "deploy-runbook", "references", "rollback.md")); got != "rollback steps" {
		t.Fatalf("nested file missing: %q", got)
	}

	// 第二轮同哈希:stamp 命中 + 目录在 → 零拉取零写盘。
	stampBefore := readSkillTest(t, filepath.Join(dir, stampsFile))
	if err := materializeSkillDir(dir, refs, fetch); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, stampsFile)); got != stampBefore {
		t.Fatalf("stamps rewritten on no-op round")
	}

	// stamp 命中但目录被手删 → 重物化自愈。
	if err := os.RemoveAll(filepath.Join(dir, "deploy-runbook")); err != nil {
		t.Fatal(err)
	}
	if err := materializeSkillDir(dir, refs, fetch); err != nil {
		t.Fatalf("self-heal round: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, "deploy-runbook", "SKILL.md")); got != skillMdV1[0].Body {
		t.Fatalf("SKILL.md after self-heal = %q", got)
	}

	// 哈希变更:整包重写(嵌套新文件就位,旧内容不留)。
	v2 := []skillBundleFile{{Path: "SKILL.md", Body: "---\nname: deploy-runbook\n---\n\nv2"}}
	ref2 := refOf("co", "deploy-runbook", v2...)
	refs2 := []companySkillRef{ref2}
	if err := materializeSkillDir(dir, refs2, func(string) *skillBundle {
		return bundleOf("deploy-runbook", v2...)
	}); err != nil {
		t.Fatalf("rematerialize on change: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, "deploy-runbook", "SKILL.md")); got != v2[0].Body {
		t.Fatalf("SKILL.md after change = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy-runbook", "references")); !os.IsNotExist(err) {
		t.Fatalf("stale nested dir should be removed with the old bundle")
	}
}

func TestMaterializeSkillDirRemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	files := []skillBundleFile{{Path: "SKILL.md", Body: "A"}}
	refs := []companySkillRef{refOf("co", "a", files...)}
	fetch := func(string) *skillBundle { return bundleOf("a", files...) }
	if err := materializeSkillDir(dir, refs, fetch); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// 清单清空(公司删了技能)→ 目录回收 + stamp 清空。
	if err := materializeSkillDir(dir, nil, fetch); err != nil {
		t.Fatalf("materialize empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("deleted skill dir should be removed")
	}
	stamps, err := readSkillStamps(dir)
	if err != nil || len(stamps) != 0 {
		t.Fatalf("stamps after delete = %v, %v", stamps, err)
	}
}

func TestMaterializeSkillDirFetchFailureKeepsStale(t *testing.T) {
	dir := t.TempDir()
	files := []skillBundleFile{{Path: "SKILL.md", Body: "v1"}}
	ref := refOf("co", "a", files...)
	refs := []companySkillRef{ref}
	good := func(string) *skillBundle { return bundleOf("a", files...) }
	if err := materializeSkillDir(dir, refs, good); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// 哈希变了但整包拉取失败:旧目录保留、stamp 留旧值(宁可陈旧不可
	// 缺失),下一轮再试。
	refs[0].BundleHash = "deadbeef"
	fail := func(string) *skillBundle { return nil }
	if err := materializeSkillDir(dir, refs, fail); err != nil {
		t.Fatalf("materialize on fetch failure: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, "a", "SKILL.md")); got != "v1" {
		t.Fatalf("stale body lost on fetch failure: %q", got)
	}
	stamps, _ := readSkillStamps(dir)
	if stamps["a"] != ref.BundleHash {
		t.Fatalf("stamp on fetch failure = %v, want %s", stamps, ref.BundleHash)
	}
}

func TestMaterializeSkillDirRejectsTamperedBundle(t *testing.T) {
	dir := t.TempDir()
	// 清单哈希与整包内容不符(错包/中间人)→ 不落盘。
	claimed := []skillBundleFile{{Path: "SKILL.md", Body: "claimed"}}
	ref := refOf("co", "a", claimed...)
	refs := []companySkillRef{ref}
	tampered := bundleOf("a", skillBundleFile{Path: "SKILL.md", Body: "tampered"})
	if err := materializeSkillDir(dir, refs, func(string) *skillBundle { return tampered }); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("tampered bundle must not land on disk")
	}
}

func TestMaterializeSkillDirRejectsBadName(t *testing.T) {
	// 用未创建的目标路径:坏名字清单 → 早退不建目录、零落盘。
	dir := filepath.Join(t.TempDir(), "skills")
	refs := []companySkillRef{{CompanyID: "co", Name: "../../evil", BundleHash: "x"}}
	if err := materializeSkillDir(dir, refs, func(string) *skillBundle { return nil }); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("bad name must not even create the dir")
	}
}

func TestWriteSkillFilesRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"../evil.md", "/abs.md", ".", "a\x00b"} {
		if err := writeSkillFiles(filepath.Join(dir, "x"), []skillBundleFile{{Path: path, Body: "x"}}); err == nil {
			t.Fatalf("unsafe path %q accepted", path)
		}
	}
}

func TestEngineSkillsDir(t *testing.T) {
	cases := map[string]string{
		"claude": filepath.Join("h", ".claude", "skills"),
		"grok":   filepath.Join("h", ".claude", "skills"),
		"cursor": filepath.Join("h", ".cursor", "skills"),
		"zcode":  filepath.Join("h", "skills"),
		"codex":  filepath.Join("h", ".codex", "skills"),
	}
	for engine, want := range cases {
		if got := engineSkillsDir(engine, "h"); got != want {
			t.Fatalf("engineSkillsDir(%s) = %q, want %q", engine, got, want)
		}
	}
}

func TestReadSkillStampsBadFileSelfHeals(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stampsFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamps, err := readSkillStamps(dir)
	if err != nil || stamps == nil || len(stamps) != 0 {
		t.Fatalf("bad stamps file: %v, %v", stamps, err)
	}
}

func TestBuiltinSkillsEmbedAndMaterialize(t *testing.T) {
	// 内置技能:名字落在 cumora- 命名空间,SKILL.md 带 frontmatter。
	if len(builtinSkills) < 2 {
		t.Fatalf("builtin skills = %d, want ≥2", len(builtinSkills))
	}
	for _, b := range builtinSkills {
		if !strings.HasPrefix(b.ref.Name, "cumora-") {
			t.Fatalf("builtin %q outside the cumora- namespace", b.ref.Name)
		}
		if len(b.bundle.Files) != 1 || b.bundle.Files[0].Path != "SKILL.md" {
			t.Fatalf("builtin %q must be a single SKILL.md", b.ref.Name)
		}
		if !strings.Contains(b.bundle.Files[0].Body, "name: "+b.ref.Name) {
			t.Fatalf("builtin %q frontmatter name mismatch", b.ref.Name)
		}
	}
	// 物化:无公司 refs 也落内置技能(CompanyID 空不挡内置)。
	home := t.TempDir()
	dir := engineSkillsDir("claude", home)
	s := newSkillSyncer(context.Background(), &DaemonConfig{})
	s.materializeAgent(AgentInfo{ID: "a1"}, "claude", home, nil)
	for _, b := range builtinSkills {
		target := filepath.Join(dir, b.ref.Name, "SKILL.md")
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("builtin %s not materialized: %v", b.ref.Name, err)
		}
	}
	// 第二轮:零变化(stamps 稳定)。
	stampBefore := readSkillTest(t, filepath.Join(dir, stampsFile))
	s.materializeAgent(AgentInfo{ID: "a1"}, "claude", home, nil)
	if got := readSkillTest(t, filepath.Join(dir, stampsFile)); got != stampBefore {
		t.Fatalf("builtin-only round must be a no-op")
	}
}
