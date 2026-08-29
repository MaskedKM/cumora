// agent 包 skills —— skills.ts 的渐进披露索引面:只加载每个已装
// 技能的 name+description+path(~100 token/技能),完整 SKILL.md 由引擎
// 按需拉取。frontmatter 解析刻意只覆盖规范实际定义的子集(标量字段 +
// 扁平 metadata 映射)。
package agent

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// SkillFrontmatter:SKILL.md 头部字段。
type SkillFrontmatter struct {
	Name         string
	Description  string
	License      string
	Compat       string
	AllowedTools string
	Metadata     map[string]string
}

var (
	fmIndentedKV = regexp.MustCompile(`^ {2,}([\w.-]+)\s*:\s*(.*)$`)
	fmTopKV      = regexp.MustCompile(`^([a-zA-Z][\w-]*)\s*:\s*(.*)$`)
)

func scalar(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 &&
		((strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`)) ||
			(strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'"))) {
		v = v[1 : len(v)-1]
	}
	return v
}

// ParseSkillMd:解析 SKILL.md 为 {frontmatter, body}。缺失头部块或
// name/description 不全 → frontmatter=nil(body 为去头后的正文)。
func ParseSkillMd(content string) (*SkillFrontmatter, string) {
	if !strings.HasPrefix(content, "---\n") {
		return nil, content
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return nil, content
	}
	fmRaw := content[4 : 4+end]
	after := content[4+end+4:]
	after = strings.TrimPrefix(after, "\n")

	var fm SkillFrontmatter
	haveName, haveDesc := false, false
	inMetadata := false
	for _, line := range strings.Split(fmRaw, "\n") {
		if inMetadata {
			if m := fmIndentedKV.FindStringSubmatch(line); m != nil {
				if fm.Metadata == nil {
					fm.Metadata = map[string]string{}
				}
				fm.Metadata[m[1]] = scalar(m[2])
				continue
			}
			inMetadata = false
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "metadata:") {
			inMetadata = true
			if fm.Metadata == nil {
				fm.Metadata = map[string]string{}
			}
			continue
		}
		m := fmTopKV.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, raw := m[1], m[2]
		switch key {
		case "name":
			fm.Name, haveName = scalar(raw), true
		case "description":
			fm.Description, haveDesc = scalar(raw), true
		case "license":
			fm.License = scalar(raw)
		case "compatibility":
			fm.Compat = scalar(raw)
		case "allowed-tools":
			fm.AllowedTools = scalar(raw)
		}
	}
	if !haveName || fm.Name == "" || !haveDesc || fm.Description == "" {
		return nil, after
	}
	return &fm, after
}

// LoadSkillsIndex:agent_workspace 里 skills/<name>/SKILL.md 的索引;
// 坏 frontmatter 跳过(留警告供 agent 自查)。
func (s *Service) LoadSkillsIndex(ctx context.Context, agentID string) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT path, body FROM agent_workspace
		 WHERE agent_id = $1 AND path LIKE 'skills/%/SKILL.md'
		 ORDER BY path ASC`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var path, body string
		if err := rows.Scan(&path, &body); err != nil {
			return nil, err
		}
		fm, _ := ParseSkillMd(body)
		if fm == nil {
			slog.Warn("[skills] malformed SKILL.md — skipped", "agent", agentID, "path", path)
			continue
		}
		out = append(out, map[string]any{
			"name":        fm.Name,
			"description": fm.Description,
			"path":        path,
		})
	}
	return out, rows.Err()
}
