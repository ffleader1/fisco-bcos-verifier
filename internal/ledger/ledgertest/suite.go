// Package ledgertest is a conformance suite run against every ledger.Ledger implementation
// (in-memory mock and a real FISCO BCOS chain). Identical expectations keep the mock faithful to the contracts.
package ledgertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"bouyup/fisco/internal/hashing"
	"bouyup/fisco/internal/ledger"
	"bouyup/fisco/internal/sealing"
)

// Env is one ledger under test (already bootstrapped).
type Env struct {
	L        ledger.Ledger
	Accounts ledger.Accounts
}

// Run executes all scenarios.
func Run(t *testing.T, env Env) {
	s := &suite{env: env, ctx: context.Background(), run: randHex(3)}
	t.Run("Withdrawal", s.withdrawal)
	t.Run("WithdrawalValidation", s.withdrawalValidation)
	t.Run("WithdrawalList", s.withdrawalList)
	t.Run("AuditTrail", s.audit)
	t.Run("Explorer", s.explorer)
}

type suite struct {
	env Env
	ctx context.Context
	run string
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *suite) id(name string) string { return "T" + s.run + "-" + name }

func h(s string) ledger.Hash { return hashing.Text(s) }

func expectKind(t *testing.T, err error, kind ledger.ErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error, got nil", kind)
	}
	if k := ledger.KindOf(err); k != kind {
		t.Fatalf("expected %s error, got %s (%v)", kind, k, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func countEvents(tx ledger.Tx, name string) int {
	n := 0
	for _, e := range tx.Events {
		if e.Name == name {
			n++
		}
	}
	return n
}

// sealed builds a real encrypted withdrawal: a summary readable by both banks and the master key, and one
// piece per bank readable by that bank and the master key.
func (s *suite) sealed(t *testing.T, id string) (ledger.WithdrawalRequest, map[string]sealing.KeyPair) {
	t.Helper()
	keys := map[string]sealing.KeyPair{}
	for _, n := range []string{"master", "VCB", "BIDV"} {
		k, err := sealing.NewKeyPair()
		must(t, err)
		keys[n] = k
	}
	out, err := sealing.Seal(id, []string{keys["master"].Public, keys["VCB"].Public, keys["BIDV"].Public}, []sealing.Piece{
		{Plain: []byte(`{"type":"summary","withdrawalId":"` + id + `","amount":1000,"salt":"` + sealing.Salt() + `"}`), Readers: []int{0, 1, 2}},
		{Plain: []byte(`{"type":"bank_share","bank":"VCB","ratioBps":6000,"amount":600,"salt":"` + sealing.Salt() + `"}`), Readers: []int{0, 1}},
		{Plain: []byte(`{"type":"bank_share","bank":"BIDV","ratioBps":4000,"amount":400,"salt":"` + sealing.Salt() + `"}`), Readers: []int{0, 2}},
	})
	must(t, err)
	return ledger.WithdrawalRequest{ID: id, Root: out.Root, Commitments: out.Commitments, Envelope: out.Raw}, keys
}

func (s *suite) withdrawal(t *testing.T) {
	id := s.id("WD1")
	req, keys := s.sealed(t, id)
	w, tx, err := s.env.L.RecordWithdrawal(s.ctx, req)
	must(t, err)
	if tx.Hash == "" || tx.BlockNumber <= 0 || countEvents(tx, "WithdrawalRecorded") != 1 {
		t.Fatalf("tx: %+v", tx)
	}
	got, err := s.env.L.GetWithdrawal(s.ctx, id)
	must(t, err)
	if got.Root != req.Root || len(got.Commitments) != 3 || got.Commitments[2] != req.Commitments[2] || string(got.Envelope) != string(req.Envelope) ||
		got.BlockNumber != tx.BlockNumber || got.TxHash != tx.Hash || got.Timestamp == 0 || got.Timestamp != w.Timestamp || got.Writer != s.env.Accounts.Operator {
		t.Fatalf("record mismatch:\n got %+v\n tx %+v", got, tx)
	}
	// What each key can read back from the chain copy.
	for name, want := range map[string]int{"master": 3, "VCB": 2, "BIDV": 2} {
		opened, err := sealing.Open(id, got.Envelope, keys[name].Private, got.Commitments)
		must(t, err)
		if len(opened) != want {
			t.Fatalf("%s reads %d pieces, want %d", name, len(opened), want)
		}
		for _, o := range opened {
			if !o.CommitmentOK {
				t.Fatalf("%s: piece %d does not match its on-chain commitment", name, o.Index)
			}
		}
	}
	_, _, err = s.env.L.RecordWithdrawal(s.ctx, req)
	expectKind(t, err, ledger.KindDuplicate)
	_, err = s.env.L.GetWithdrawal(s.ctx, s.id("NOPE"))
	expectKind(t, err, ledger.KindNotFound)
}

func (s *suite) withdrawalValidation(t *testing.T) {
	var many []ledger.Hash
	for i := 0; i < 22; i++ {
		many = append(many, h(s.run+string(rune('a'+i))))
	}
	cases := map[string]func(*ledger.WithdrawalRequest){
		"root mismatch":  func(r *ledger.WithdrawalRequest) { r.Root = h("other") },
		"one piece":      func(r *ledger.WithdrawalRequest) { r.Commitments = r.Commitments[:1]; r.Root = r.Commitments[0] },
		"22 pieces":      func(r *ledger.WithdrawalRequest) { r.Commitments = many; r.Root = ledger.MerkleRoot(many) },
		"empty envelope": func(r *ledger.WithdrawalRequest) { r.Envelope = nil },
		"zero commitment": func(r *ledger.WithdrawalRequest) {
			r.Commitments[1] = ledger.ZeroHash
			r.Root = ledger.MerkleRoot(r.Commitments)
		},
		"oversize envelope": func(r *ledger.WithdrawalRequest) { r.Envelope = make([]byte, 32769) },
	}
	i := 0
	for name, mut := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			r, _ := s.sealed(t, s.id("BAD"+string(rune('A'+i))))
			mut(&r)
			_, _, err := s.env.L.RecordWithdrawal(s.ctx, r)
			expectKind(t, err, ledger.KindInvalid)
		})
	}
}

func (s *suite) withdrawalList(t *testing.T) {
	ids := []string{s.id("L1"), s.id("L2"), s.id("L3")}
	for _, id := range ids {
		r, _ := s.sealed(t, id)
		_, _, err := s.env.L.RecordWithdrawal(s.ctx, r)
		must(t, err)
	}
	list, total, err := s.env.L.ListWithdrawals(s.ctx, 0, 2)
	must(t, err)
	if total < 3 || len(list) != 2 || list[0].ID != ids[2] || list[1].ID != ids[1] {
		t.Fatalf("list newest first: total=%d %v", total, list)
	}
	list, _, err = s.env.L.ListWithdrawals(s.ctx, 2, 1)
	must(t, err)
	if len(list) != 1 || list[0].ID != ids[0] {
		t.Fatalf("offset: %v", list)
	}
}

func (s *suite) audit(t *testing.T) {
	subj := s.id("APP")
	var prev ledger.Hash
	var recs []ledger.AnchorRecord
	for i, k := range []ledger.AnchorKind{ledger.KindApplication, ledger.KindAlgorithmOutput, ledger.KindCreditMemo, ledger.KindRiskChallenge, ledger.KindDecisionPackage} {
		r, err := s.env.L.Anchor(s.ctx, ledger.AnchorRequest{Subject: subj, Kind: k, ContentHash: h(subj + k.String()), URI: "s3://webank-sg/" + k.String()})
		must(t, err)
		if r.Index != int64(i) {
			t.Fatalf("index %d want %d", r.Index, i)
		}
		recs = append(recs, r)
	}
	_, err := s.env.L.Anchor(s.ctx, ledger.AnchorRequest{Subject: subj, Kind: ledger.KindOther, ContentHash: h(subj + ledger.KindAlgorithmOutput.String())})
	expectKind(t, err, ledger.KindDuplicate)
	_, err = s.env.L.Anchor(s.ctx, ledger.AnchorRequest{Subject: subj, Kind: 99, ContentHash: h("x")})
	expectKind(t, err, ledger.KindInvalid)

	list, err := s.env.L.ListAnchors(s.ctx, subj)
	must(t, err)
	if len(list) != 5 {
		t.Fatalf("got %d anchors", len(list))
	}
	for i, r := range list {
		if r.ContentHash != recs[i].ContentHash || r.Head != recs[i].Head {
			t.Fatalf("record %d mismatch", i)
		}
		if got := ledger.ChainHead(prev, r.Kind, r.ContentHash, r.Timestamp); got != r.Head {
			t.Fatalf("hash chain broken at %d: recomputed %s on-chain %s", i, got, r.Head)
		}
		prev = r.Head
	}
	loc, ok, err := s.env.L.LocateAnchor(s.ctx, recs[2].ContentHash)
	must(t, err)
	if !ok || loc.Subject != subj || loc.Index != 2 || loc.Kind != ledger.KindCreditMemo {
		t.Fatalf("locate: %+v %v", loc, ok)
	}
	_, ok, err = s.env.L.LocateAnchor(s.ctx, h("never anchored "+s.run))
	must(t, err)
	if ok {
		t.Fatalf("unexpected locate hit")
	}
}

func (s *suite) explorer(t *testing.T) {
	id := s.id("EX1")
	r, _ := s.sealed(t, id)
	_, tx, err := s.env.L.RecordWithdrawal(s.ctx, r)
	must(t, err)
	d, err := s.env.L.GetTransaction(s.ctx, tx.Hash)
	must(t, err)
	if d.Hash != tx.Hash || d.BlockNumber != tx.BlockNumber || d.Status != "success" || d.Contract != "WithdrawalLedger" || d.Method != "record" {
		t.Fatalf("tx detail: %+v", d)
	}
	if d.FromRole != "operator" || d.Sealer == "" || len(d.Confirmations) == 0 || !d.Confirmations[0].Confirmed {
		t.Fatalf("tx detail roles/confirmations: %+v", d)
	}
	if countEvents(ledger.Tx{Events: d.Events}, "WithdrawalRecorded") != 1 {
		t.Fatalf("decoded events: %+v", d.Events)
	}
	b, err := s.env.L.GetBlock(s.ctx, tx.BlockNumber)
	must(t, err)
	found := false
	for _, h := range b.Transactions {
		found = found || h == tx.Hash
	}
	if !found || b.Number != tx.BlockNumber {
		t.Fatalf("block %+v does not hold %s", b, tx.Hash)
	}
	latest, err := s.env.L.LatestBlocks(s.ctx, 3)
	must(t, err)
	if len(latest) == 0 || latest[0].Number < tx.BlockNumber {
		t.Fatalf("latest blocks: %+v", latest)
	}
	nodes, err := s.env.L.Nodes(s.ctx)
	must(t, err)
	if len(nodes) == 0 || !nodes[0].Online {
		t.Fatalf("nodes: %+v", nodes)
	}
	_, err = s.env.L.GetTransaction(s.ctx, h("no such tx "+s.run).Hex())
	expectKind(t, err, ledger.KindNotFound)
}
