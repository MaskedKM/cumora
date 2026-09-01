// manifest —— #283 制品版本清单(MANIFEST)的单源类型与校验。
//
// 打包链(PR-C)装箱时生成;absorb 消费它做完整性门(逐文件 sha256,
// 缺件/坏件点名报错——断言响亮纪律);status 读它做报告面(票面 AC
// "制品内每件二进制带版本清单,与 stack status 报告一致")。
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Manifest —— 制品根的 MANIFEST(JSON)。files 为相对路径 → sha256
// (正斜杠形态,与制品内路径无关平台);deps 与 ci/build-stack-deps.sh
// 落盘的 MANIFEST.deps components 同构(版本+源 tarball sha,可复验)。
type Manifest struct {
	Version   string                 `json:"version"`
	CreatedAt string                 `json:"createdAt,omitempty"`
	Files     map[string]string      `json:"files"`
	Deps      map[string]ManifestDep `json:"deps,omitempty"`
}

// ManifestDep —— 依赖物条目。
type ManifestDep struct {
	Version      string `json:"version"`
	SourceSha256 string `json:"sourceSha256"`
}

// Summary —— status 报告面的瘦身形态(不带 sha 全表)。
type Summary struct {
	Version string            `json:"version"`
	Deps    map[string]string `json:"deps,omitempty"` // 名 → 版本
}

// Parse —— 解析 + 基本校验(版本与 files 非空)。
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("MANIFEST 解析失败: %w", err)
	}
	if strings.TrimSpace(m.Version) == "" {
		return Manifest{}, errors.New("MANIFEST.version 为空")
	}
	if len(m.Files) == 0 {
		return Manifest{}, errors.New("MANIFEST.files 为空")
	}
	return m, nil
}

// SummaryOf —— 报告面瘦身。
func (m Manifest) SummaryOf() Summary {
	s := Summary{Version: m.Version, Deps: map[string]string{}}
	for k, v := range m.Deps {
		s.Deps[k] = v.Version
	}
	return s
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify —— 逐条核销 files:缺件/坏件/越界路径都点名(静默失败=排障
// 盲飞——stack-deps 首跑的教训)。
func Verify(dir string, m Manifest) error {
	for rel, want := range m.Files {
		if filepath.IsAbs(rel) || strings.HasPrefix(filepath.ToSlash(rel), "../") {
			return fmt.Errorf("MANIFEST.files 含越界路径: %q", rel)
		}
		got, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("MANIFEST 校验缺件: %s(%v)", rel, err)
		}
		if got != want {
			return fmt.Errorf("MANIFEST 校验不符: %s(期望 %s… 实得 %s…)", rel, want[:12], got[:12])
		}
	}
	return nil
}
