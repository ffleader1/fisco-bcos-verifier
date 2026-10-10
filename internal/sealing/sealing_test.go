package sealing

import (
	"strings"
	"testing"

	"bouyup/fisco/internal/ledger"
)

func TestSealOpen(t *testing.T) {
	master, _ := NewKeyPair()
	a, _ := NewKeyPair()
	b, _ := NewKeyPair()
	stranger, _ := NewKeyPair()
	pieces := []Piece{
		{Plain: []byte(`{"type":"summary","amount":100}`), Readers: []int{0, 1, 2}},
		{Plain: []byte(`{"type":"bank_share","bank":"A","amount":60}`), Readers: []int{0, 1}},
		{Plain: []byte(`{"type":"bank_share","bank":"B","amount":40}`), Readers: []int{0, 2}},
	}
	s, err := Seal("WD-1", []string{master.Public, a.Public, b.Public}, pieces)
	if err != nil {
		t.Fatal(err)
	}
	if s.Root != ledger.MerkleRoot(s.Commitments) || len(s.Envelope.Slots) != 3 {
		t.Fatal("root / slots")
	}
	read := func(priv string) []int {
		out, err := Open("WD-1", s.Raw, priv, s.Commitments)
		if err != nil {
			t.Fatal(err)
		}
		var idx []int
		for _, o := range out {
			if !o.CommitmentOK {
				t.Fatal("commitment mismatch")
			}
			idx = append(idx, o.Index)
		}
		return idx
	}
	if got := read(master.Private); len(got) != 3 {
		t.Fatalf("master reads %v", got)
	}
	if got := read(a.Private); len(got) != 2 || got[1] != 1 {
		t.Fatalf("A reads %v", got)
	}
	if got := read(b.Private); len(got) != 2 || got[1] != 2 {
		t.Fatalf("B reads %v", got)
	}
	if _, err := Open("WD-1", s.Raw, stranger.Private, nil); err != ErrNoSlot {
		t.Fatalf("stranger: %v", err)
	}
	if _, err := Open("WD-2", s.Raw, a.Private, nil); err != ErrNoSlot {
		t.Fatal("envelope must be bound to its withdrawal id")
	}
	tampered := strings.Replace(string(s.Raw), s.Envelope.Pieces[1].CT[:8], "AAAAAAAA", 1)
	if _, err := Open("WD-1", []byte(tampered), a.Private, nil); err == nil {
		t.Fatal("tampered piece must not decrypt")
	}
}

func TestMerkle(t *testing.T) {
	h := func(b byte) ledger.Hash { var x ledger.Hash; x[0] = b; return x }
	if ledger.MerkleRoot([]ledger.Hash{h(1)}) != h(1) {
		t.Fatal("single leaf")
	}
	r3 := ledger.MerkleRoot([]ledger.Hash{h(1), h(2), h(3)})
	r2 := ledger.MerkleRoot([]ledger.Hash{h(1), h(2)})
	if r3 == r2 || ledger.MerkleRoot([]ledger.Hash{r2, h(3)}) != r3 {
		t.Fatal("odd leaf moves up unchanged")
	}
}
