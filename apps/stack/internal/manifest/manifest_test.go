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
	m, err := Parse([]byte(`{"version":"0.4.0","files":{"cumora-server":"ab"},"deps":{"redis":{"version":"7.2.16","sourceSha256":"00"}}}`))
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
