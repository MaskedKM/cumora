// skills_sync 内部测试:物化的三态(命中跳过/变更重写/消失回收)+
// 拉取失败保旧 + 引擎目录定址。纯文件系统直驱,不起 HTTP。
package daemon

import (
	"os"
	"path/filepath"
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

func TestMaterializeSkillDirWritesAndSkips(t *testing.T) {
	dir := t.TempDir()
	refs := []companySkillRef{{
		CompanyID: "co", Name: "deploy-runbook", Description: "d", BundleHash: "h1",
	}}
	b1 := &skillBundle{Name: "deploy-runbook", Files: []skillBundleFile{
		{Path: "SKILL.md", Body: "---\nname: deploy-runbook\n---\n\nv1"},
		{Path: "references/rollback.md", Body: "rollback steps"},
	}}
	var fetches int
	fetch := func(hash string) *skillBundle {
		fetches++
		return b1
	}
	if err := materializeSkillDir(dir, refs, fetch); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, "deploy-runbook", "SKILL.md")); got != b1.Files[0].Body {
		t.Fatalf("SKILL.md body = %q", got)
	}
	if got := readSkillTest(t, filepath.Join(dir, "deploy-runbook", "references", "rollback.md")); got != "rollback steps" {
		t.Fatalf("nested file missing: %q", got)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}

	// 第二轮同哈希:零拉取零写盘(stamp 命中)。
	if err := materializeSkillDir(dir, refs, fetch); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	if fetches != 1 {
		t.Fatalf("fetches after no-op round = %d, want 1", fetches)
	}

	// 哈希变更:整包重写(嵌套新文件就位,旧内容不留)。
	b2 := &skillBundle{Name: "deploy-runbook", Files: []skillBundleFile{
		{Path: "SKILL.md", Body: "---\nname: deploy-runbook\n---\n\nv2"},
	}}
	refs[0].BundleHash = "h2"
	fetch2 := func(hash string) *skillBundle {
		if hash != "h2" {
			t.Fatalf("fetch hash = %s, want h2", hash)
		}
		return b2
	}
	if err := materializeSkillDir(dir, refs, fetch2); err != nil {
		t.Fatalf("rematerialize on change: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, "deploy-runbook", "SKILL.md")); got != b2.Files[0].Body {
		t.Fatalf("SKILL.md after change = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy-runbook", "references")); !os.IsNotExist(err) {
		t.Fatalf("stale nested dir should be removed with the old bundle")
	}
}

func TestMaterializeSkillDirRemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	refs := []companySkillRef{{CompanyID: "co", Name: "a", BundleHash: "h1"}}
	fetch := func(string) *skillBundle {
		return &skillBundle{Name: "a", Files: []skillBundleFile{{Path: "SKILL.md", Body: "A"}}}
	}
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
	refs := []companySkillRef{{CompanyID: "co", Name: "a", BundleHash: "h1"}}
	good := func(string) *skillBundle {
		return &skillBundle{Name: "a", Files: []skillBundleFile{{Path: "SKILL.md", Body: "v1"}}}
	}
	if err := materializeSkillDir(dir, refs, good); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// 哈希变了但整包拉取失败:旧目录保留、stamp 留旧值(宁可陈旧不可
	// 缺失),下一轮再试。
	refs[0].BundleHash = "h2"
	fail := func(string) *skillBundle { return nil }
	if err := materializeSkillDir(dir, refs, fail); err != nil {
		t.Fatalf("materialize on fetch failure: %v", err)
	}
	if got := readSkillTest(t, filepath.Join(dir, "a", "SKILL.md")); got != "v1" {
		t.Fatalf("stale body lost on fetch failure: %q", got)
	}
	stamps, _ := readSkillStamps(dir)
	if stamps["a"] != "h1" {
		t.Fatalf("stamp on fetch failure = %v, want h1", stamps)
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
