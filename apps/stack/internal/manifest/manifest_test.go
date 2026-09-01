// manifest 单测(#283 PR-B):解析校验 + Verify 缺件/坏件/越界点名。
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestParseValidAndInvalid(t *testing.T) {
	m, err := Parse([]byte(`{"version":"0.4.0","files":{"cumora-server":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"deps":{"redis":{"version":"7.2.16","sourceSha256":"00"}}}`))
	if err != nil || m.Version != "0.4.0" || len(m.Files) != 1 || m.Deps["redis"].Version != "7.2.16" {
		t.Fatalf("合法清单应解析: %+v %v", m, err)
	}
	if s := m.SummaryOf(); s.Deps["redis"] != "7.2.16" {
		t.Fatalf("Summary 应带 dep 版本: %+v", s)
	}
	for name, body := range map[string]string{
		"坏 JSON":  `{`,
		"无版本":     `{"files":{"a":"b"}}`,
		"无 files": `{"version":"1"}`,
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("%s 应报错", name)
		}
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestVerifyAllGreen(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"cumora-server": "bin-a",
		"pg/bin/initdb": "bin-b",
	})
	m := Manifest{Version: "1", Files: map[string]string{
		"cumora-server": sha(t, filepath.Join(dir, "cumora-server")),
		"pg/bin/initdb": sha(t, filepath.Join(dir, "pg/bin/initdb")),
	}}
	if err := Verify(dir, m); err != nil {
		t.Fatalf("全绿不应报错: %v", err)
	}
}

func TestVerifyNamesTheCulprit(t *testing.T) {
	dir := writeTree(t, map[string]string{"cumora-server": "bin-a"})
	m := Manifest{Version: "1", Files: map[string]string{"cumora-server": strings.Repeat("0", 64)}}
	err := Verify(dir, m)
	if err == nil || !strings.Contains(err.Error(), "cumora-server") {
		t.Fatalf("坏件应点名: %v", err)
	}

	m2 := Manifest{Version: "1", Files: map[string]string{"cumora-daemon": strings.Repeat("0", 64)}}
	err2 := Verify(dir, m2)
	if err2 == nil || !strings.Contains(err2.Error(), "缺件") || !strings.Contains(err2.Error(), "cumora-daemon") {
		t.Fatalf("缺件应点名: %v", err2)
	}

	m3 := Manifest{Version: "1", Files: map[string]string{"../escape": strings.Repeat("0", 64)}}
	if err3 := Verify(dir, m3); err3 == nil || !strings.Contains(err3.Error(), "越界") {
		t.Fatalf("越界路径应拒绝: %v", err3)
	}
}

func TestParseRejectsShortSha(t *testing.T) {
	// 坏清单在解析口拒(#302 评审 P1-2:短 sha 交给 Verify 的切片会
	// panic——崩溃不是响亮报错)。
	body := `{"version":"1","files":{"cumora-server":"ab"}}`
	if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), "64 位") {
		t.Fatalf("短 sha 应在解析口拒: %v", err)
	}
	if _, err := Parse([]byte(`{"version":"1","files":{"a":"` + strings.Repeat("G", 64) + `"}}`)); err == nil {
		t.Fatal("非 hex sha 应拒")
	}
}

func TestVerifyMiddleDotDotEscape(t *testing.T) {
	// 只查前缀会放过中间 `..` 段(#302 评审 P1-1 容器实证逃逸:
	// a/../../x 会读到载荷目录之外做比对)。Clean 归一后拒。
	dir := writeTree(t, map[string]string{"a/f": "x"})
	m := Manifest{Version: "1", Files: map[string]string{"a/../../escaped": strings.Repeat("0", 64)}}
	err := Verify(dir, m)
	if err == nil || !strings.Contains(err.Error(), "越界") {
		t.Fatalf("中间 .. 段应拒: %v", err)
	}
	// Clean 后仍在载荷内的相对段(a/../b → b)无害。
	dir2 := writeTree(t, map[string]string{"b": "content"})
	sum := sha(t, filepath.Join(dir2, "b"))
	m2 := Manifest{Version: "1", Files: map[string]string{"a/../b": sum}}
	if err := Verify(dir2, m2); err != nil {
		t.Fatalf("归一后未越界应放行: %v", err)
	}
}
