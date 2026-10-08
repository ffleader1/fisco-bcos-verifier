// Package ledgertest is a conformance suite run against every ledger.Ledger implementation
// (in-memory mock and a real FISCO BCOS chain). Identical expectations keep the mock faithful to the contracts.
//
// The environment must be bootstrapped with funders "webank" (same key as the operator, so its share is
// auto-confirmed — WeBank-only mode), "vcb" and "tcb", and a policy whose MaxFutureSkewSec is large enough
// (>= 400 days) to replay a full loan timeline with explicit asOf values on a live chain.
package ledgertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"sellergraph/fisco/internal/hashing"
	"sellergraph/fisco/internal/ledger"
)

const day = int64(86400)

// Env is one ledger under test.
type Env struct {
	L        ledger.Ledger
	Accounts ledger.Accounts
	Policy   ledger.Policy
}

// Run executes all scenarios.
func Run(t *testing.T, env Env) {
	s := &suite{env: env, ctx: context.Background(), run: randHex(3)}
	t.Run("DocExample_WeBankOnly_Tier2", s.docExample)
	t.Run("MultiFunderConfirmation", s.multiFunder)
	t.Run("HumanReviewGate", s.humanReview)
	t.Run("Validation", s.validation)
	t.Run("FloorOverdueAllocation", s.floorOverdue)
	t.Run("MaturityAccelerateDefault", s.maturityAccelerateDefault)
	t.Run("DeliveryProof", s.delivery)
	t.Run("Restructure", s.restructure)
	t.Run("PayoffSettlesExactly", s.payoff)
	t.Run("AuditTrail", s.audit)
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

func hasEvent(tx ledger.Tx, name string) bool {
	for _, e := range tx.Events {
		if e.Name == name {
			return true
		}
	}
	return false
}

func eventField(t *testing.T, tx ledger.Tx, name, field string) any {
	t.Helper()
	for _, e := range tx.Events {
		if e.Name == name {
			return e.Fields[field]
		}
	}
	t.Fatalf("event %s not found in %+v", name, tx.Events)
	return nil
}

// standard terms: 100,000.00 CNY, 12%/yr, 120 days, sweep 43% (cap 63%), 20 grace days — the Part 10 example.
func (s *suite) terms(id string, tier uint8, principal int64, funding []ledger.FundingShare) ledger.CreateLoan {
	return ledger.CreateLoan{
		LoanID: id, SellerRef: hashing.Ref([]byte("salt"), hashing.DomainSeller, "seller-"+id), Tier: tier,
		Principal: principal, RateBps: 1200, TenorDays: 120, SweepRateBps: 4300, SweepCapBps: 6300, GraceDays: 20,
		DecisionHash: h("decision-" + id), Funding: funding,
	}
}

func (s *suite) webankOnly(p int64) []ledger.FundingShare {
	return []ledger.FundingShare{{Funder: s.env.Accounts.Funders["webank"], Amount: p}}
}

// create + disburse a WeBank-only loan; returns the snapshot after disbursement.
func (s *suite) activeLoan(t *testing.T, id string, principal int64) ledger.Loan {
	t.Helper()
	_, err := s.env.L.CreateLoan(s.ctx, s.terms(id, 2, principal, s.webankOnly(principal)))
	must(t, err)
	_, err = s.env.L.RecordDisbursement(s.ctx, ledger.Disbursement{LoanID: id, DeliveryDays: 10, BankTxRef: h("bank-" + id),
		Payments: []ledger.PaymentLine{{PayeeRef: h("supA"), Amount: principal}}})
	must(t, err)
	l, err := s.env.L.GetLoan(s.ctx, id)
	must(t, err)
	return l
}

// expectedInterest is an independent re-implementation of the accrual formula.
func expectedInterest(principal, rateBps, seconds int64, rem *big.Int) int64 {
	num := new(big.Int).Mul(big.NewInt(principal), big.NewInt(rateBps))
	num.Mul(num, big.NewInt(seconds))
	num.Add(num, rem)
	q, r := new(big.Int).QuoRem(num, big.NewInt(10000*365*86400), new(big.Int))
	rem.Set(r)
	return q.Int64()
}

// ---------------------------------------------------------------- scenarios

func (s *suite) docExample(t *testing.T) {
	id := s.id("doc")
	const principal = 10_000_000 // 100,000.00 CNY
	tx, err := s.env.L.CreateLoan(s.ctx, s.terms(id, 2, principal, s.webankOnly(principal)))
	must(t, err)
	if !hasEvent(tx, "LoanCreated") || !hasEvent(tx, "FundingConfirmed") {
		t.Fatalf("missing events: %+v", tx.Events)
	}
	l, err := s.env.L.GetLoan(s.ctx, id)
	must(t, err)
	if l.Status != ledger.StatusApproved || l.HumanReviewRequired {
		t.Fatalf("WeBank-only below threshold should auto-approve, got status=%s review=%v", l.Status, l.HumanReviewRequired)
	}

	// one payment, two suppliers
	tx, err = s.env.L.RecordDisbursement(s.ctx, ledger.Disbursement{LoanID: id, DeliveryDays: 10, BankTxRef: h("WB-TX-1"),
		Payments: []ledger.PaymentLine{{PayeeRef: h("supplierA"), Amount: 6_000_000}, {PayeeRef: h("supplierB"), Amount: 4_000_000}}})
	must(t, err)
	if !hasEvent(tx, "Disbursed") {
		t.Fatalf("no Disbursed event")
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.Status != ledger.StatusActive || l.PrincipalOutstanding != principal || len(l.Payments) != 2 {
		t.Fatalf("bad state after disbursement: %+v", l)
	}
	if l.MaturityAt-l.DisbursedAt != 120*day || l.GraceEndsAt-l.DisbursedAt != 20*day {
		t.Fatalf("schedule wrong: %+v", l)
	}
	_, err = s.env.L.RecordDisbursement(s.ctx, ledger.Disbursement{LoanID: id, DeliveryDays: 10, BankTxRef: h("WB-TX-2"),
		Payments: []ledger.PaymentLine{{PayeeRef: h("supplierA"), Amount: principal}}})
	expectKind(t, err, ledger.KindBadStatus) // one click, one payment

	// proportional sweeps of 43% on weekly payouts after the grace period; verify interest independently
	t0 := l.DisbursedAt
	rem := new(big.Int)
	last := t0
	outstanding := int64(principal)
	interest := int64(0)
	payouts := []int64{3_000_000, 4_000_000, 5_000_000, 4_000_000, 3_000_000, 2_000_000}
	for i, p := range payouts {
		asOf := t0 + 21*day + int64(i)*7*day
		interest += expectedInterest(outstanding, 1200, asOf-last, rem)
		last = asOf
		sweep := p * 43 / 100
		res, err := s.env.L.RecordRepayment(s.ctx, ledger.Repayment{LoanID: id, Amount: sweep, Source: ledger.SourceProportionalSweep,
			ExtRef: h(id + "-sweep-" + string(rune('a'+i))), AsOf: asOf})
		must(t, err)
		a := res.Allocation
		if a.ToInterest != interest {
			t.Fatalf("sweep %d: interest allocation %d, independent calc %d", i, a.ToInterest, interest)
		}
		interest -= a.ToInterest
		outstanding -= a.ToPrincipal
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.PrincipalOutstanding != outstanding {
		t.Fatalf("outstanding %d, expected %d", l.PrincipalOutstanding, outstanding)
	}
	// final sweep over-collects: settled + refund due
	asOf := t0 + 70*day
	res, err := s.env.L.RecordRepayment(s.ctx, ledger.Repayment{LoanID: id, Amount: 2_000_000, Source: ledger.SourceProportionalSweep,
		ExtRef: h(id + "-sweep-final"), AsOf: asOf})
	must(t, err)
	if !res.Settled || res.Allocation.Excess <= 0 {
		t.Fatalf("expected settlement with excess, got %+v", res)
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.Status != ledger.StatusSettled || l.RefundDue != res.Allocation.Excess || l.TotalRepaid != l.PrincipalRepaid+l.InterestRepaid {
		t.Fatalf("bad settled state: %+v", l)
	}
	if l.PrincipalRepaid != principal {
		t.Fatalf("principal repaid %d", l.PrincipalRepaid)
	}
	// interest stays below the 4,000.00 CNY maximum of the Part 10 example because principal amortizes early
	if l.InterestRepaid <= 0 || l.InterestRepaid >= 400_000 {
		t.Fatalf("interest repaid %d out of expected range", l.InterestRepaid)
	}
	_, err = s.env.L.RecordRepayment(s.ctx, ledger.Repayment{LoanID: id, Amount: 1, Source: ledger.SourcePrepayment, ExtRef: h("x"), AsOf: asOf})
	expectKind(t, err, ledger.KindBadStatus)
}

func (s *suite) multiFunder(t *testing.T) {
	id := s.id("multi")
	f := s.env.Accounts.Funders
	tx, err := s.env.L.CreateLoan(s.ctx, s.terms(id, 1, 3_000_000, []ledger.FundingShare{
		{Funder: f["vcb"], Amount: 2_000_000}, {Funder: f["tcb"], Amount: 1_000_000}}))
	must(t, err)
	if hasEvent(tx, "FundingConfirmed") {
		t.Fatalf("no share should be auto-confirmed")
	}
	l, _ := s.env.L.GetLoan(s.ctx, id)
	if l.Status != ledger.StatusCreated || l.PendingFunders != 2 {
		t.Fatalf("got %s pending=%d", l.Status, l.PendingFunders)
	}
	_, err = s.env.L.ConfirmFunding(s.ctx, id, f["webank"])
	expectKind(t, err, ledger.KindUnauthorized) // registered funder, but not in this loan
	_, err = s.env.L.ConfirmFunding(s.ctx, id, f["vcb"])
	must(t, err)
	_, err = s.env.L.ConfirmFunding(s.ctx, id, f["vcb"])
	expectKind(t, err, ledger.KindDuplicate)
	_, err = s.env.L.RecordDisbursement(s.ctx, ledger.Disbursement{LoanID: id, DeliveryDays: 5, BankTxRef: h("b"),
		Payments: []ledger.PaymentLine{{PayeeRef: h("s"), Amount: 3_000_000}}})
	expectKind(t, err, ledger.KindBadStatus)
	tx, err = s.env.L.ConfirmFunding(s.ctx, id, f["tcb"])
	must(t, err)
	if !hasEvent(tx, "StatusChanged") {
		t.Fatalf("expected approval on last confirmation")
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.Status != ledger.StatusApproved || len(l.Funding) != 2 || !l.Funding[0].Confirmed || !l.Funding[1].Confirmed {
		t.Fatalf("bad funding state %+v", l)
	}
	if !strings.EqualFold(l.Funding[0].Funder, f["vcb"]) || l.Funding[0].Amount != 2_000_000 {
		t.Fatalf("funding order/amount: %+v", l.Funding)
	}
	_, err = s.env.L.CancelLoan(s.ctx, id, h("seller declined offer"))
	must(t, err)
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.Status != ledger.StatusCancelled {
		t.Fatalf("expected cancelled")
	}
}

func (s *suite) humanReview(t *testing.T) {
	amt := s.env.Policy.OfficerThreshold + 1
	id := s.id("hr")
	r := s.terms(id, 2, amt, s.webankOnly(amt))
	r.HumanReviewRequired = false // the contract must force it anyway
	_, err := s.env.L.CreateLoan(s.ctx, r)
	must(t, err)
	l, _ := s.env.L.GetLoan(s.ctx, id)
	if !l.HumanReviewRequired || l.Status != ledger.StatusCreated {
		t.Fatalf("tier-2 above threshold must wait for officer: %+v", l)
	}
	_, err = s.env.L.OfficerDecision(s.ctx, id, true, ledger.ZeroHash)
	expectKind(t, err, ledger.KindInvalid)
	tx, err := s.env.L.OfficerDecision(s.ctx, id, true, h("officer review memo"))
	must(t, err)
	if !hasEvent(tx, "OfficerDecision") {
		t.Fatalf("no OfficerDecision event")
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.Status != ledger.StatusApproved || !l.OfficerApproved {
		t.Fatalf("expected approved: %+v", l)
	}

	// orchestrator-triggered review (e.g. Agent 2 vs Agent 3 disagree) on a small tier-1 loan, then rejection
	id2 := s.id("hr2")
	r2 := s.terms(id2, 1, 500_000, s.webankOnly(500_000))
	r2.HumanReviewRequired = true
	_, err = s.env.L.CreateLoan(s.ctx, r2)
	must(t, err)
	_, err = s.env.L.OfficerDecision(s.ctx, id2, false, h("fraud signal confirmed"))
	must(t, err)
	l, _ = s.env.L.GetLoan(s.ctx, id2)
	if l.Status != ledger.StatusCancelled {
		t.Fatalf("expected cancelled, got %s", l.Status)
	}
	_, err = s.env.L.OfficerDecision(s.ctx, id2, true, h("again"))
	expectKind(t, err, ledger.KindBadStatus)
}

func (s *suite) validation(t *testing.T) {
	id := s.id("val")
	w := s.webankOnly
	cases := []struct {
		name string
		mod  func(*ledger.CreateLoan)
		kind ledger.ErrorKind
	}{
		{"shares != principal", func(r *ledger.CreateLoan) { r.Funding = w(r.Principal - 1) }, ledger.KindInvalid},
		{"rate above ceiling", func(r *ledger.CreateLoan) { r.RateBps = s.env.Policy.MaxRateBps + 1 }, ledger.KindInvalid},
		{"sweep above cap", func(r *ledger.CreateLoan) { r.SweepRateBps = r.SweepCapBps + 1 }, ledger.KindInvalid},
		{"grace >= tenor", func(r *ledger.CreateLoan) { r.GraceDays = r.TenorDays }, ledger.KindInvalid},
		{"bad tier", func(r *ledger.CreateLoan) { r.Tier = 3 }, ledger.KindInvalid},
		{"no decision hash", func(r *ledger.CreateLoan) { r.DecisionHash = ledger.ZeroHash }, ledger.KindInvalid},
		{"unregistered funder", func(r *ledger.CreateLoan) {
			r.Funding = []ledger.FundingShare{{Funder: "0x00000000000000000000000000000000000000aa", Amount: r.Principal}}
		}, ledger.KindInvalid},
		{"funder twice", func(r *ledger.CreateLoan) {
			f := s.env.Accounts.Funders["vcb"]
			r.Funding = []ledger.FundingShare{{Funder: f, Amount: r.Principal / 2}, {Funder: f, Amount: r.Principal - r.Principal/2}}
		}, ledger.KindDuplicate},
	}
	for _, c := range cases {
		r := s.terms(id, 1, 1_000_000, w(1_000_000))
		c.mod(&r)
		_, err := s.env.L.CreateLoan(s.ctx, r)
		if err == nil || ledger.KindOf(err) != c.kind {
			t.Errorf("%s: expected %s, got %v", c.name, c.kind, err)
		}
	}
	// nothing was persisted by the failed attempts
	_, err := s.env.L.GetLoan(s.ctx, id)
	expectKind(t, err, ledger.KindNotFound)

	_, err = s.env.L.CreateLoan(s.ctx, s.terms(id, 1, 1_000_000, w(1_000_000)))
	must(t, err)
	_, err = s.env.L.CreateLoan(s.ctx, s.terms(id, 1, 1_000_000, w(1_000_000)))
	expectKind(t, err, ledger.KindDuplicate)
	_, err = s.env.L.RecordDisbursement(s.ctx, ledger.Disbursement{LoanID: id, DeliveryDays: 5, BankTxRef: h("b"),
		Payments: []ledger.PaymentLine{{PayeeRef: h("s"), Amount: 999_999}}})
	expectKind(t, err, ledger.KindInvalid)
	_, err = s.env.L.RecordDisbursement(s.ctx, ledger.Disbursement{LoanID: id, DeliveryDays: 5, BankTxRef: ledger.ZeroHash,
		Payments: []ledger.PaymentLine{{PayeeRef: h("s"), Amount: 1_000_000}}})
	expectKind(t, err, ledger.KindInvalid)
	_, err = s.env.L.RecordRepayment(s.ctx, ledger.Repayment{LoanID: id, Amount: 1, Source: 1, ExtRef: h("r"), AsOf: 0})
	expectKind(t, err, ledger.KindBadStatus) // not disbursed
	_, err = s.env.L.GetLoan(s.ctx, s.id("nope"))
	expectKind(t, err, ledger.KindNotFound)
}

func (s *suite) floorOverdue(t *testing.T) {
	id := s.id("floor")
	l := s.activeLoan(t, id, 10_000_000)
	t0 := l.DisbursedAt

	// first sweep, then idempotency on the same reference
	r1 := ledger.Repayment{LoanID: id, Amount: 1_000_000, Source: ledger.SourceProportionalSweep, ExtRef: h(id + "-s1"), AsOf: t0 + 25*day}
	_, err := s.env.L.RecordRepayment(s.ctx, r1)
	must(t, err)
	r1.AsOf = t0 + 26*day
	_, err = s.env.L.RecordRepayment(s.ctx, r1)
	expectKind(t, err, ledger.KindDuplicate)

	// below floor, not a milestone: yellow, no overdue
	fr, err := s.env.L.CheckpointFloor(s.ctx, ledger.FloorCheckpoint{LoanID: id, FloorCumulative: 1_500_000, AsOf: t0 + 30*day})
	must(t, err)
	if fr.Shortfall != 500_000 || !hasEvent(fr.Tx, "AlertRaised") {
		t.Fatalf("shortfall %d events %+v", fr.Shortfall, fr.Tx.Events)
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.AlertLevel != ledger.AlertYellow || l.OverduePrincipal+l.OverdueInterest != 0 {
		t.Fatalf("expected yellow without overdue: %+v", l)
	}
	_, err = s.env.L.CheckpointFloor(s.ctx, ledger.FloorCheckpoint{LoanID: id, FloorCumulative: 1_000_000, AsOf: t0 + 31*day})
	expectKind(t, err, ledger.KindInvalid) // floor never decreases
	_, err = s.env.L.CheckpointFloor(s.ctx, ledger.FloorCheckpoint{LoanID: id, FloorCumulative: 2_000_000, AsOf: t0 + 29*day})
	expectKind(t, err, ledger.KindInvalid) // asOf before last accrual

	// milestone: shortfall becomes overdue, interest classified first
	fr, err = s.env.L.CheckpointFloor(s.ctx, ledger.FloorCheckpoint{LoanID: id, FloorCumulative: 2_000_000, AsOf: t0 + 40*day, Milestone: true})
	must(t, err)
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.OverdueInterest != l.InterestAccrued || l.OverduePrincipal != 1_000_000-l.InterestAccrued || fr.Shortfall != 1_000_000 {
		t.Fatalf("overdue buckets wrong: %+v (shortfall %d)", l, fr.Shortfall)
	}
	if !hasEvent(fr.Tx, "OverdueUpdated") {
		t.Fatalf("no OverdueUpdated")
	}
	ovI, ovP := l.OverdueInterest, l.OverduePrincipal

	// make-up debit: overdue interest -> overdue principal -> current interest -> principal
	res, err := s.env.L.RecordRepayment(s.ctx, ledger.Repayment{LoanID: id, Amount: 1_200_000, Source: ledger.SourceFloorMakeup,
		ExtRef: h(id + "-mk"), AsOf: t0 + 42*day})
	must(t, err)
	a := res.Allocation
	if a.ToOverdueInterest != ovI || a.ToOverduePrincipal != ovP {
		t.Fatalf("overdue not paid first: %+v (ovI %d ovP %d)", a, ovI, ovP)
	}
	if a.ToInterest <= 0 || a.ToPrincipal != 1_200_000-ovI-ovP-a.ToInterest || a.Excess != 0 {
		t.Fatalf("allocation order wrong: %+v", a)
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.OverdueInterest != 0 || l.OverduePrincipal != 0 || l.InterestAccrued != 0 {
		t.Fatalf("overdue/interest should be cleared: %+v", l)
	}
}

func (s *suite) maturityAccelerateDefault(t *testing.T) {
	id := s.id("mat")
	l := s.activeLoan(t, id, 2_000_000)
	t0 := l.DisbursedAt
	_, err := s.env.L.MarkMatured(s.ctx, id, t0+100*day)
	expectKind(t, err, ledger.KindInvalid)
	tx, err := s.env.L.MarkMatured(s.ctx, id, t0+121*day)
	must(t, err)
	if eventField(t, tx, "AlertRaised", "code") != "MATURED_UNPAID" {
		t.Fatalf("expected MATURED_UNPAID")
	}
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.AlertLevel != ledger.AlertRed || l.OverduePrincipal != 2_000_000 || l.OverdueInterest != l.InterestAccrued || l.InterestAccrued == 0 {
		t.Fatalf("matured state wrong: %+v", l)
	}
	_, err = s.env.L.MarkDefault(s.ctx, id, h("officer: unreachable seller"))
	must(t, err)
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.Status != ledger.StatusDefaulted {
		t.Fatalf("expected defaulted")
	}
	// recoveries are still recorded after default
	_, err = s.env.L.RecordRepayment(s.ctx, ledger.Repayment{LoanID: id, Amount: 300_000, Source: ledger.SourceRecovery, ExtRef: h(id + "-rec"), AsOf: t0 + 130*day})
	must(t, err)

	id2 := s.id("acc")
	l2 := s.activeLoan(t, id2, 1_000_000)
	_, err = s.env.L.Accelerate(s.ctx, id2, ledger.ZeroHash, l2.DisbursedAt+10*day)
	expectKind(t, err, ledger.KindInvalid)
	tx, err = s.env.L.Accelerate(s.ctx, id2, h("covenant: stopped sharing store data"), l2.DisbursedAt+10*day)
	must(t, err)
	l2, _ = s.env.L.GetLoan(s.ctx, id2)
	if l2.Status != ledger.StatusAccelerated || l2.OverduePrincipal != 1_000_000 {
		t.Fatalf("accelerate: %+v", l2)
	}
	_, err = s.env.L.Accelerate(s.ctx, id2, h("again"), l2.DisbursedAt+11*day)
	expectKind(t, err, ledger.KindBadStatus)
}

func (s *suite) delivery(t *testing.T) {
	id := s.id("dlv")
	l := s.activeLoan(t, id, 1_000_000)
	_, err := s.env.L.CreateLoan(s.ctx, s.terms(s.id("dlv2"), 1, 2_000_000, s.webankOnly(2_000_000)))
	must(t, err)
	_, err = s.env.L.RecordDisbursement(s.ctx, ledger.Disbursement{LoanID: s.id("dlv2"), DeliveryDays: 7, BankTxRef: h("b2"),
		Payments: []ledger.PaymentLine{{PayeeRef: h("supA"), Amount: 1_500_000}, {PayeeRef: h("supB"), Amount: 500_000}}})
	must(t, err)

	dl := l.Payments[0].Deadline
	if dl-l.DisbursedAt != 10*day {
		t.Fatalf("deadline %d disbursed %d", dl, l.DisbursedAt)
	}
	_, err = s.env.L.FlagMissingDelivery(s.ctx, id, 0, dl)
	expectKind(t, err, ledger.KindInvalid) // deadline not passed
	dr, err := s.env.L.RecordDelivery(s.ctx, id, 0, h("SF1234567"), l.DisbursedAt+3*day)
	must(t, err)
	if !dr.OnTime || hasEvent(dr.Tx, "CovenantEvent") {
		t.Fatalf("expected on-time delivery: %+v", dr)
	}
	_, err = s.env.L.RecordDelivery(s.ctx, id, 0, h("SF999"), l.DisbursedAt+4*day)
	expectKind(t, err, ledger.KindDuplicate)
	_, err = s.env.L.RecordDelivery(s.ctx, id, 5, h("SF999"), l.DisbursedAt+4*day)
	expectKind(t, err, ledger.KindNotFound)

	l2, _ := s.env.L.GetLoan(s.ctx, s.id("dlv2"))
	dr, err = s.env.L.RecordDelivery(s.ctx, s.id("dlv2"), 0, h("YT777"), l2.DisbursedAt+9*day)
	must(t, err)
	if dr.OnTime || eventField(t, dr.Tx, "CovenantEvent", "code") != "DELIVERY_LATE" {
		t.Fatalf("expected late delivery covenant event: %+v", dr.Tx.Events)
	}
	tx, err := s.env.L.FlagMissingDelivery(s.ctx, s.id("dlv2"), 1, l2.DisbursedAt+8*day)
	must(t, err)
	if eventField(t, tx, "CovenantEvent", "code") != "DELIVERY_MISSING" {
		t.Fatalf("expected DELIVERY_MISSING")
	}
	l2, _ = s.env.L.GetLoan(s.ctx, s.id("dlv2"))
	if l2.AlertLevel != ledger.AlertRed || !l2.Payments[0].Delivered || l2.Payments[1].Delivered {
		t.Fatalf("delivery state wrong: %+v", l2)
	}
	tx, err = s.env.L.RecordCovenantEvent(s.ctx, id, "STORE_DATA_SHARING", false, h("daily check ok"))
	must(t, err)
	if eventField(t, tx, "CovenantEvent", "breached") != false {
		t.Fatalf("covenant event fields: %+v", tx.Events)
	}
	_, err = s.env.L.RaiseAlert(s.ctx, id, ledger.AlertGreen, "DAILY_OK", ledger.ZeroHash)
	must(t, err)
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.AlertLevel != ledger.AlertGreen {
		t.Fatalf("alert level %s", l.AlertLevel)
	}
}

func (s *suite) restructure(t *testing.T) {
	id := s.id("rst")
	l := s.activeLoan(t, id, 4_000_000)
	t0 := l.DisbursedAt
	_, err := s.env.L.CheckpointFloor(s.ctx, ledger.FloorCheckpoint{LoanID: id, FloorCumulative: 1_000_000, AsOf: t0 + 40*day, Milestone: true})
	must(t, err)
	_, err = s.env.L.Restructure(s.ctx, ledger.Restructure{LoanID: id, ExtraDays: 30, NewSweepRateBps: 7000, ReasonHash: h("r"), AsOf: t0 + 41*day})
	expectKind(t, err, ledger.KindInvalid) // above 63% cap
	_, err = s.env.L.Restructure(s.ctx, ledger.Restructure{LoanID: id, ExtraDays: 30, NewSweepRateBps: 3000, ReasonHash: h("seasonal dip, Agent 6 plan"), AsOf: t0 + 41*day})
	must(t, err)
	l, _ = s.env.L.GetLoan(s.ctx, id)
	if l.MaturityAt-t0 != 150*day || l.TenorDays != 150 || l.SweepRateBps != 3000 || l.OverduePrincipal+l.OverdueInterest != 0 || l.FloorCumulative != 0 {
		t.Fatalf("restructure state: %+v", l)
	}
}

func (s *suite) payoff(t *testing.T) {
	id := s.id("pay")
	l := s.activeLoan(t, id, 5_000_000)
	asOf := l.DisbursedAt + 45*day
	p, err := s.env.L.PreviewPayoff(s.ctx, id, asOf)
	must(t, err)
	// 5,000,000 * 12% * 45/365 = 73,972.6 -> 73,972
	if p.Principal != 5_000_000 || p.Interest != 73_972 {
		t.Fatalf("payoff %+v", p)
	}
	res, err := s.env.L.RecordRepayment(s.ctx, ledger.Repayment{LoanID: id, Amount: p.Total, Source: ledger.SourcePrepayment, ExtRef: h(id + "-payoff"), AsOf: asOf})
	must(t, err)
	if !res.Settled || res.Allocation.Excess != 0 {
		t.Fatalf("payoff should settle exactly: %+v", res)
	}
}

func (s *suite) audit(t *testing.T) {
	subj := s.id("APP")
	var prev ledger.Hash
	var recs []ledger.AnchorRecord
	for i, k := range []ledger.AnchorKind{ledger.KindApplication, ledger.KindMLOutput, ledger.KindCreditMemo, ledger.KindRiskChallenge, ledger.KindDecisionPackage} {
		r, err := s.env.L.Anchor(s.ctx, ledger.AnchorRequest{Subject: subj, Kind: k, ContentHash: h(subj + k.String()), URI: "s3://webank-sg/" + k.String()})
		must(t, err)
		if r.Index != int64(i) {
			t.Fatalf("index %d want %d", r.Index, i)
		}
		recs = append(recs, r)
	}
	_, err := s.env.L.Anchor(s.ctx, ledger.AnchorRequest{Subject: subj, Kind: ledger.KindOther, ContentHash: h(subj + ledger.KindMLOutput.String())})
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
