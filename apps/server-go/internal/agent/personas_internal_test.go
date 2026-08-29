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

// visualSignatureFor:同 id+gender 稳定;gender 分池必须给出不同呈现。
func TestVisualSignatureFor(t *testing.T) {
	a := visualSignatureFor("iris", genderFeminine)
	b := visualSignatureFor("iris", genderFeminine)
	if a != b {
		t.Fatal("same id+gender must produce the same signature")
	}
	m := visualSignatureFor("iris", genderMasculine)
	if a.Presentation == m.Presentation && a.Wardrobe == m.Wardrobe {
		t.Fatal("gender pools must stratify presentation/wardrobe")
	}
	if a.Gender != genderFeminine || m.Gender != genderMasculine {
		t.Fatal("gender must round-trip on the signature")
	}
}
