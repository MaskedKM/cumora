package avatar

import (
	"testing"
)

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
