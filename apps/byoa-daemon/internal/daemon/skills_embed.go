// daemon 包 skills_embed —— #261b 平台内置技能(cumora-*):canonical
// 源在 packages/prompt/skills/,prompt-gen 逐字拷贝到 skillsdata/(go:
// embed,字节一致),与公司手册走同一物化管线(同 stamp 机制、同目录)。
// 内置技能不占公司域(CompanyID 空),任何 agent 无条件物化。
package daemon

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed skillsdata
var skillsEmbedFS embed.FS

// builtinSkills:内置技能引用清单(名字 + 内容哈希),启动时解析一次。
type builtinSkill struct {
	ref    companySkillRef
	bundle skillBundle
}

var builtinSkills = parseEmbeddedSkills()

// cumoraSkillPrefix:平台命名空间。server 侧 create 同步保留该前缀
// (公司技能撞名会在物化端互相覆盖)。
const cumoraSkillPrefix = "cumora-"

func parseEmbeddedSkills() []builtinSkill {
	entries, err := fs.ReadDir(skillsEmbedFS, "skillsdata")
	if err != nil {
		return nil
	}
	var out []builtinSkill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		body, err := fs.ReadFile(skillsEmbedFS, fmt.Sprintf("skillsdata/%s/SKILL.md", name))
		if err != nil {
			continue
		}
		files := []skillBundleFile{{Path: "SKILL.md", Body: string(body)}}
		out = append(out, builtinSkill{
			ref:    companySkillRef{Name: name, BundleHash: bundleHashDaemon(files)},
			bundle: skillBundle{Name: name, Files: files},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ref.Name < out[j].ref.Name })
	return out
}
