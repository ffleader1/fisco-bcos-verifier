package fisco

import (
	"context"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"sellergraph/fisco/internal/ledger"
)

// ---------------------------------------------------------------- origination

func (l *Ledger) CreateLoan(ctx context.Context, r ledger.CreateLoan) (ledger.Tx, error) {
	id, err := idArg(r.LoanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	t := [7]*big.Int{bi(int64(r.Tier)), bi(r.Principal), bi(r.RateBps), bi(r.TenorDays), bi(r.SweepRateBps), bi(r.SweepCapBps), bi(r.GraceDays)}
	for _, v := range []int64{r.Principal, r.RateBps, r.TenorDays, r.SweepRateBps, r.SweepCapBps, r.GraceDays} {
		if v < 0 {
			return ledger.Tx{}, ledger.Errorf(ledger.KindInvalid, "negative loan term")
		}
	}
	funders := make([]common.Address, len(r.Funding))
	amounts := make([]*big.Int, len(r.Funding))
	for i, f := range r.Funding {
		if f.Amount < 0 {
			return ledger.Tx{}, ledger.Errorf(ledger.KindInvalid, "share amount")
		}
		funders[i], amounts[i] = common.HexToAddress(f.Funder), bi(f.Amount)
	}
	return l.transact(ctx, l.accounts.Operator, false, "createLoan", id, b32(r.SellerRef), t, r.HumanReviewRequired, b32(r.DecisionHash), funders, amounts)
}

func (l *Ledger) ConfirmFunding(ctx context.Context, loanID, funder string) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, funder, false, "confirmFunding", id)
}

func (l *Ledger) OfficerDecision(ctx context.Context, loanID string, approved bool, reviewHash ledger.Hash) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, l.accounts.Officer, false, "officerDecision", id, approved, b32(reviewHash))
}

func (l *Ledger) CancelLoan(ctx context.Context, loanID string, reasonHash ledger.Hash) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, l.accounts.Operator, false, "cancelLoan", id, b32(reasonHash))
}

// ---------------------------------------------------------------- disbursement & repayment

func (l *Ledger) RecordDisbursement(ctx context.Context, r ledger.Disbursement) (ledger.Tx, error) {
	id, err := idArg(r.LoanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	refs := make([][32]byte, len(r.Payments))
	amts := make([]*big.Int, len(r.Payments))
	for i, p := range r.Payments {
		if p.Amount < 0 {
			return ledger.Tx{}, ledger.Errorf(ledger.KindInvalid, "payment line")
		}
		refs[i], amts[i] = b32(p.PayeeRef), bi(p.Amount)
	}
	if r.DeliveryDays < 0 {
		return ledger.Tx{}, ledger.Errorf(ledger.KindInvalid, "deliveryDays")
	}
	return l.transact(ctx, l.accounts.Operator, false, "recordDisbursement", id, refs, amts, bi(r.DeliveryDays), b32(r.BankTxRef))
}

func (l *Ledger) RecordRepayment(ctx context.Context, r ledger.Repayment) (ledger.RepaymentResult, error) {
	var res ledger.RepaymentResult
	id, err := idArg(r.LoanID)
	if err != nil {
		return res, err
	}
	if r.Amount < 0 || r.AsOf < 0 {
		return res, ledger.Errorf(ledger.KindInvalid, "amount")
	}
	tx, err := l.transact(ctx, l.accounts.Operator, false, "recordRepayment", id, bi(r.Amount), uint8(r.Source), b32(r.ExtRef), bi(r.AsOf))
	if err != nil {
		return res, err
	}
	res.Tx = tx
	for _, e := range tx.Events {
		switch e.Name {
		case "RepaymentRecorded":
			a := e.Fields["allocation"].([]int64)
			res.Allocation = ledger.Allocation{ToOverdueInterest: a[0], ToOverduePrincipal: a[1], ToInterest: a[2], ToPrincipal: a[3], Excess: a[4]}
		case "StatusChanged":
			if e.Fields["toStatus"] == int64(ledger.StatusSettled) {
				res.Settled = true
			}
		}
	}
	return res, nil
}

func (l *Ledger) CheckpointFloor(ctx context.Context, r ledger.FloorCheckpoint) (ledger.FloorResult, error) {
	id, err := idArg(r.LoanID)
	if err != nil {
		return ledger.FloorResult{}, err
	}
	if r.FloorCumulative < 0 || r.AsOf < 0 {
		return ledger.FloorResult{}, ledger.Errorf(ledger.KindInvalid, "floor")
	}
	tx, err := l.transact(ctx, l.accounts.Operator, false, "checkpointFloor", id, bi(r.FloorCumulative), bi(r.AsOf), r.Milestone)
	if err != nil {
		return ledger.FloorResult{}, err
	}
	res := ledger.FloorResult{Tx: tx}
	for _, e := range tx.Events {
		if e.Name == "FloorCheckpoint" {
			res.Shortfall = e.Fields["shortfall"].(int64)
		}
	}
	return res, nil
}

func (l *Ledger) MarkMatured(ctx context.Context, loanID string, asOf int64) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, l.accounts.Operator, false, "markMatured", id, bi(asOf))
}

func (l *Ledger) Accelerate(ctx context.Context, loanID string, reasonHash ledger.Hash, asOf int64) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, l.accounts.Officer, false, "accelerate", id, b32(reasonHash), bi(asOf))
}

func (l *Ledger) MarkDefault(ctx context.Context, loanID string, reasonHash ledger.Hash) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, l.accounts.Officer, false, "markDefault", id, b32(reasonHash))
}

func (l *Ledger) Restructure(ctx context.Context, r ledger.Restructure) (ledger.Tx, error) {
	id, err := idArg(r.LoanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if r.ExtraDays < 0 || r.NewSweepRateBps < 0 {
		return ledger.Tx{}, ledger.Errorf(ledger.KindInvalid, "restructure terms")
	}
	return l.transact(ctx, l.accounts.Officer, false, "restructure", id, bi(r.ExtraDays), bi(r.NewSweepRateBps), b32(r.ReasonHash), bi(r.AsOf))
}

// ---------------------------------------------------------------- monitoring

func (l *Ledger) RaiseAlert(ctx context.Context, loanID string, level ledger.AlertLevel, code string, evidence ledger.Hash) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	c, err := ledger.CodeToBytes32(code)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, l.accounts.Monitor, false, "raiseAlert", id, uint8(level), b32(c), b32(evidence))
}

func (l *Ledger) RecordCovenantEvent(ctx context.Context, loanID, code string, breached bool, evidence ledger.Hash) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	c, err := ledger.CodeToBytes32(code)
	if err != nil {
		return ledger.Tx{}, err
	}
	return l.transact(ctx, l.accounts.Monitor, false, "recordCovenantEvent", id, b32(c), breached, b32(evidence))
}

func (l *Ledger) RecordDelivery(ctx context.Context, loanID string, line int, waybill ledger.Hash, shippedAt int64) (ledger.DeliveryResult, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.DeliveryResult{}, err
	}
	if line < 0 || shippedAt < 0 {
		return ledger.DeliveryResult{}, ledger.Errorf(ledger.KindNotFound, "payment line")
	}
	tx, err := l.transact(ctx, l.accounts.Operator, false, "recordDelivery", id, bi(int64(line)), b32(waybill), bi(shippedAt))
	if err != nil {
		return ledger.DeliveryResult{}, err
	}
	res := ledger.DeliveryResult{Tx: tx}
	for _, e := range tx.Events {
		if e.Name == "DeliveryRecorded" {
			res.OnTime = e.Fields["onTime"].(bool)
		}
	}
	return res, nil
}

func (l *Ledger) FlagMissingDelivery(ctx context.Context, loanID string, line int, asOf int64) (ledger.Tx, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if line < 0 {
		return ledger.Tx{}, ledger.Errorf(ledger.KindNotFound, "payment line")
	}
	return l.transact(ctx, l.accounts.Monitor, false, "flagMissingDelivery", id, bi(int64(line)), bi(asOf))
}

// ---------------------------------------------------------------- reads

func (l *Ledger) GetLoan(ctx context.Context, loanID string) (ledger.Loan, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Loan{}, err
	}
	sch, err := l.call(ctx, false, "getSchedule", id)
	if err != nil {
		return ledger.Loan{}, err
	}
	if ledger.Status(sch[0].(uint8)) == ledger.StatusNone {
		return ledger.Loan{}, ledger.Errorf(ledger.KindNotFound, "loan")
	}
	tm, err := l.call(ctx, false, "getTerms", id)
	if err != nil {
		return ledger.Loan{}, err
	}
	fl, err := l.call(ctx, false, "getFlags", id)
	if err != nil {
		return ledger.Loan{}, err
	}
	bal, err := l.call(ctx, false, "getBalances", id)
	if err != nil {
		return ledger.Loan{}, err
	}
	fund, err := l.call(ctx, false, "getFunding", id)
	if err != nil {
		return ledger.Loan{}, err
	}
	out := ledger.Loan{
		LoanID: loanID, SellerRef: h32(tm[0]), Tier: tm[1].(uint8), Principal: i64(tm[2]), RateBps: i64(tm[3]), TenorDays: i64(tm[4]),
		SweepRateBps: i64(tm[5]), SweepCapBps: i64(tm[6]), GraceDays: i64(tm[7]),
		DecisionHash: h32(fl[0]), HumanReviewRequired: fl[1].(bool), OfficerApproved: fl[2].(bool), CreatedAt: i64(fl[3]), PendingFunders: i64(fl[4]),
		Status: ledger.Status(sch[0].(uint8)), AlertLevel: ledger.AlertLevel(sch[1].(uint8)), DisbursedAt: i64(sch[2]),
		GraceEndsAt: i64(sch[3]), MaturityAt: i64(sch[4]), LastAccrualAt: i64(sch[5]),
		PrincipalOutstanding: i64(bal[0]), InterestAccrued: i64(bal[1]), OverduePrincipal: i64(bal[2]), OverdueInterest: i64(bal[3]),
		TotalRepaid: i64(bal[4]), PrincipalRepaid: i64(bal[5]), InterestRepaid: i64(bal[6]), FloorCumulative: i64(bal[7]), RefundDue: i64(bal[8]),
		Funding: []ledger.Funding{}, Payments: []ledger.Payment{},
	}
	addrs, amts, conf := fund[0].([]common.Address), fund[1].([]*big.Int), fund[2].([]bool)
	for i := range addrs {
		out.Funding = append(out.Funding, ledger.Funding{Funder: strings.ToLower(addrs[i].Hex()), Amount: amts[i].Int64(), Confirmed: conf[i]})
	}
	cnt, err := l.call(ctx, false, "paymentCount", id)
	if err != nil {
		return ledger.Loan{}, err
	}
	for i := int64(0); i < i64(cnt[0]); i++ {
		p, err := l.call(ctx, false, "getPayment", id, bi(i))
		if err != nil {
			return ledger.Loan{}, err
		}
		out.Payments = append(out.Payments, ledger.Payment{Line: int(i), PayeeRef: h32(p[0]), Amount: i64(p[1]), Deadline: i64(p[2]),
			Delivered: p[3].(bool), WaybillHash: h32(p[4]), ShippedAt: i64(p[5])})
	}
	return out, nil
}

func (l *Ledger) ListLoanIDs(ctx context.Context, offset, limit int) ([]string, int, error) {
	n, err := l.call(ctx, false, "loanCount")
	if err != nil {
		return nil, 0, err
	}
	total := int(i64(n[0]))
	ids := []string{}
	if offset < 0 {
		offset = 0
	}
	for i := offset; i < total && (limit <= 0 || i < offset+limit); i++ {
		v, err := l.call(ctx, false, "loanIds", bi(int64(i)))
		if err != nil {
			return nil, 0, err
		}
		ids = append(ids, ledger.Bytes32ToID(h32(v[0])))
	}
	return ids, total, nil
}

func (l *Ledger) PreviewPayoff(ctx context.Context, loanID string, asOf int64) (ledger.Payoff, error) {
	id, err := idArg(loanID)
	if err != nil {
		return ledger.Payoff{}, err
	}
	if asOf < 0 {
		return ledger.Payoff{}, ledger.Errorf(ledger.KindInvalid, "asOf")
	}
	v, err := l.call(ctx, false, "previewPayoff", id, bi(asOf))
	if err != nil {
		return ledger.Payoff{}, err
	}
	return ledger.Payoff{Principal: i64(v[0]), Interest: i64(v[1]), Total: i64(v[2]), AsOf: asOf}, nil
}

// ---------------------------------------------------------------- audit

func (l *Ledger) Anchor(ctx context.Context, r ledger.AnchorRequest) (ledger.AnchorRecord, error) {
	subj, err := idArg(r.Subject)
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	tx, err := l.transact(ctx, l.accounts.Operator, true, "anchor", subj, uint8(r.Kind), b32(r.ContentHash), r.URI)
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	for _, e := range tx.Events {
		if e.Name == "Anchored" {
			head, _ := ledger.ParseHash(e.Fields["head"].(string))
			return ledger.AnchorRecord{Subject: r.Subject, Index: e.Fields["index"].(int64), Kind: r.Kind, ContentHash: r.ContentHash,
				Timestamp: e.Fields["timestamp"].(int64), Writer: l.accounts.Operator, Head: head, Tx: &tx}, nil
		}
	}
	return ledger.AnchorRecord{}, ledger.Errorf(ledger.KindChain, "Anchored event missing")
}

func (l *Ledger) record(ctx context.Context, subj [32]byte, idx int64) (ledger.AnchorRecord, error) {
	v, err := l.call(ctx, true, "getRecord", subj, bi(idx))
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	return ledger.AnchorRecord{Subject: ledger.Bytes32ToID(ledger.Hash(subj)), Index: idx, Kind: ledger.AnchorKind(v[0].(uint8)),
		ContentHash: h32(v[1]), Timestamp: i64(v[2]), Writer: strings.ToLower(v[3].(common.Address).Hex()), Head: h32(v[4])}, nil
}

func (l *Ledger) ListAnchors(ctx context.Context, subject string) ([]ledger.AnchorRecord, error) {
	subj, err := idArg(subject)
	if err != nil {
		return nil, err
	}
	n, err := l.call(ctx, true, "recordCount", subj)
	if err != nil {
		return nil, err
	}
	out := []ledger.AnchorRecord{}
	for i := int64(0); i < i64(n[0]); i++ {
		r, err := l.record(ctx, subj, i)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (l *Ledger) LocateAnchor(ctx context.Context, contentHash ledger.Hash) (ledger.AnchorRecord, bool, error) {
	v, err := l.call(ctx, true, "locate", b32(contentHash))
	if err != nil {
		return ledger.AnchorRecord{}, false, err
	}
	if !v[0].(bool) {
		return ledger.AnchorRecord{}, false, nil
	}
	r, err := l.record(ctx, v[1].([32]byte), i64(v[2]))
	return r, err == nil, err
}
