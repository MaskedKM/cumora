// persona_test —— 人格头与 standing prompt 的字节平价:golden 基准由
// 脚本从 TS 源(PERSONA_HEADER / standingPrompt / GLANCE_YIELD_RULES /
// SKYPE_EMOTICONS_GUIDE / TWO_DOMAIN_PRIVACY_RULE)机械生成,钉死 Go 转录
// 不漂移。提示词就是本地引擎的边界内容——一字之差即行为之差。
package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return string(b)
}

// goldenUpdate:GOLDEN_UPDATE=1 时回写基准(#261b 起文案变更的再生路径;
// 须同时跑 docker 单测,产物落宿主挂载卷)。
func goldenUpdate(name, got string) {
	if os.Getenv("GOLDEN_UPDATE") != "1" {
		return
	}
	path := filepath.Join("testdata", name)
	if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
		panic(err)
	}
}

func strp(s string) *string { return &s }

func TestPersonaHeaderGolden(t *testing.T) {
	cases := []struct {
		name   string
		p      Persona
		file   string
		skills string
	}{
		{"persona_full.txt", Persona{ID: "a1", Name: "Atlas", Role: strp("Tester"), SystemPrompt: strp("Be terse.")}, "CLAUDE.md", ".claude/skills/"},
		{"persona_minimal.txt", Persona{ID: "a1", Name: "Iris"}, "CLAUDE.md", ".claude/skills/"},
		{"persona_codex.txt", Persona{ID: "a1", Name: "Atlas", Role: strp("Tester"), SystemPrompt: strp("Be terse.")}, "AGENTS.md", ".codex/skills/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := personaHeader(tc.p, tc.file, tc.skills)
			goldenUpdate(tc.name, got)
			want := mustGolden(t, tc.name)
			if got != want {
				t.Fatalf("personaHeader drift vs %s:\n--- want %d bytes\n%s\n--- got %d bytes\n%s", tc.name, len(want), want, len(got), got)
			}
		})
	}
}

func TestStandingPromptGolden(t *testing.T) {
	got := standingPrompt("test-agent")
	goldenUpdate("standing_prompt.txt", got)
	want := mustGolden(t, "standing_prompt.txt")
	if got != want {
		t.Fatalf("standingPrompt drift:\n--- want %d bytes\n%s\n--- got %d bytes\n%s", len(want), want, len(got), got)
	}
	// agentID 只应内插在 --assignee 位——换 id 不该动其他任何字节。
	other := strings.ReplaceAll(want, "--assignee test-agent ", "--assignee other-agent ")
	if got2 := standingPrompt("other-agent"); got2 != other {
		t.Fatalf("standingPrompt interpolation must be confined to the assignee slot")
	}
}
