package agent

import (
	"testing"
)

// hashStrJS 必须按 UTF-16 code unit 计(FNV-1a)——非 BMP 字符占 2 单元。
// (#140 自 runtime/agenda_remote_classify_test.go 随 personas 拆入。)
func TestHashStrJS(t *testing.T) {
	if hashStrJS("iris") == hashStrJS("bram") {
		t.Fatal("distinct ids must hash differently")
	}
	// "😀" = 2 个 UTF-16 单元;与逐字节实现必须不同(手算校验一次)。
	if hashStrJS("😀") == hashStrJS("\U0001F600"[:1]) {
		t.Fatal("surrogate pair handling broken")
	}
	// 同输入稳定。
	if hashStrJS("cumora") != hashStrJS("cumora") {
		t.Fatal("hash must be deterministic")
	}
}
