// scanner 去重语义单测(#135):按 company|agent 只留最新一条指纹后,
// (a)同指纹二次命中去重不变;(b)同 agent 连续新指纹(≈每天新消息)
// map 条目有界;(c)条目数 = 活跃扫描 agent 数,与指纹代数无关。
package runtime

import (
	"fmt"
	"testing"
)

func TestScannerPrecedentLatestOnlyPerAgent(t *testing.T) {
	ResetBackgroundScannerForTests()
	defer ResetBackgroundScannerForTests()

	const agent = "co-1|ag-1"
	for i := 0; i < 500; i++ {
		if scannerSeenOrMark(agent, fmt.Sprintf("co-1|ag-1|fp-%d", i)) {
			t.Fatalf("fingerprint fp-%d 首见即被判重复", i)
		}
	}
	if got := len(scannerPrecedentScans); got != 1 {
		t.Fatalf("500 代新指纹后 map 条目 = %d, want 1(每 agent 只留最新)", got)
	}
	if !scannerSeenOrMark(agent, "co-1|ag-1|fp-499") {
		t.Fatal("最新指纹二次出现应命中去重")
	}

	// 多 agent(含跨 company 同名 agent id)各占一条
	scannerSeenOrMark("co-1|ag-2", "co-1|ag-2|x")
	scannerSeenOrMark("co-2|ag-1", "co-2|ag-1|y")
	if got := len(scannerPrecedentScans); got != 3 {
		t.Fatalf("3 个活跃 agent 后 map 条目 = %d, want 3", got)
	}
}
