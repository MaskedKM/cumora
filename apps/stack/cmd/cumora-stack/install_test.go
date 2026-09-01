// install 单测(#282 PR-B):UnitContent 纯函数对照面 —— 路径全部
// 解析展开(不留 %h)、关键指令行齐备。
package main

import (
	"strings"
	"testing"
)

func TestUnitContent(t *testing.T) {
	got := UnitContent("/home/u/.local/share/cumora/current", "/home/u/Code/cumora/.env", "/home/u/Code/cumora")
	for _, want := range []string{
		"ExecStart=/home/u/.local/share/cumora/current/cumora-stackd",
		"EnvironmentFile=/home/u/Code/cumora/.env",
		"WorkingDirectory=/home/u/Code/cumora",
		"Restart=always",
		"RestartSec=3",
		"After=network-online.target",
		"WantedBy=default.target",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("unit 内容缺 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "%h") {
		t.Fatalf("unit 内容不得残留 %%h(路径须展开):\n%s", got)
	}
	// 不再需要 ExecStartPost 探针(链式健康门在 stackd 内)。
	if strings.Contains(got, "ExecStartPost") {
		t.Fatalf("单 unit 形态不应有 ExecStartPost:\n%s", got)
	}
}
