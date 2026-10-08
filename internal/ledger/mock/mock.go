// Package mock is an in-memory Ledger that reproduces the LoanLedger / AuditTrail contract rules exactly
// (same validations, revert codes, accrual math, allocation order, events). It lets the service run and be
// tested without a FISCO BCOS node; the conformance suite (package ledgertest) runs the same scenarios against
// both this mock and a real chain to keep them in lock-step.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"sellergraph/fisco/internal/ledger"
)

const (
	bps          = 10000
	year         = 365 * 86400
	day          = 86400
	maxTenorDays = 3650
)

var accrualDenom = big.NewInt(bps * year)

type payment struct {
	payeeRef  ledger.Hash
	amount    int64
	deadline  int64
	delivered bool
	waybill   ledger.Hash
	shippedAt int64
}

type loan struct {
	id                                 ledger.Hash
	sellerRef                          ledger.Hash
	tier                               uint8
	principal, rateBps, tenorDays      int64
	sweepRateBps, sweepCapBps, grace   int64
	decisionHash                       ledger.Hash
	humanReview, officerApproved       bool
	createdAt                          int64
	status                             ledger.Status
	alert                              ledger.AlertLevel
	disbursedAt, graceEndsAt, maturity int64
	lastAccrual                        int64
	principalOut, interestAccrued      int64
	accrualRem                         int64
	overduePrincipal, overdueInterest  int64
	totalRepaid, principalRepaid       int64
	interestRepaid, floorCumulative    int64
	refundDue, pendingFunders          int64
	funders                            []string
	shares                             map[string]int64
	confirmed                          map[string]bool
	payments                           []payment
}

func (l *loan) clone() *loan {
	c := *l
	c.funders = append([]string(nil), l.funders...)
	c.shares = map[string]int64{}
	for k, v := range l.shares {
		c.shares[k] = v
	}
	c.confirmed = map[string]bool{}
	for k, v := range l.confirmed {
		c.confirmed[k] = v
	}
	c.payments = append([]payment(nil), l.payments...)
	return &c
}

type record struct {
	kind        ledger.AnchorKind
	contentHash ledger.Hash
	ts          int64
	writer      string
	head        ledger.Hash
}

// Mock implements ledger.Ledger and ledger.Admin.
type Mock struct {
	mu       sync.Mutex
	clock    func() int64
	accounts ledger.Accounts
	roles    map[string]uint64
	policy   ledger.Policy
	block    int64
	txSeq    uint64

	loans            map[ledger.Hash]*loan
	order            []ledger.Hash
	usedRef          map[ledger.Hash]bool
	totalOutstanding int64

	records     map[ledger.Hash][]record
	heads       map[ledger.Hash]ledger.Hash
	seen        map[ledger.Hash]bool
	firstAnchor map[ledger.Hash][2]any // contentHash -> {subject Hash, index int64}
}

// Option configures the mock.
type Option func(*Mock)

// WithClock injects a clock (unix seconds). Default: time.Now.
func WithClock(f func() int64) Option { return func(m *Mock) { m.clock = f } }

// New creates a mock "deployed" by accounts.Admin, with the default contract policy.
func New(acc ledger.Accounts, opts ...Option) *Mock {
	m := &Mock{
		clock:       func() int64 { return time.Now().Unix() },
		accounts:    normAccounts(acc),
		roles:       map[string]uint64{},
		policy:      ledger.Policy{MaxRateBps: 2400, OfficerThreshold: 50000000, MaxFutureSkewSec: 300},
		loans:       map[ledger.Hash]*loan{},
		usedRef:     map[ledger.Hash]bool{},
		records:     map[ledger.Hash][]record{},
		heads:       map[ledger.Hash]ledger.Hash{},
		seen:        map[ledger.Hash]bool{},
		firstAnchor: map[ledger.Hash][2]any{},
	}
	for _, o := range opts {
		o(m)
	}
	m.roles[m.accounts.Admin] = ledger.RoleAdmin
	return m
}

func normAccounts(a ledger.Accounts) ledger.Accounts {
	out := ledger.Accounts{
		Admin: strings.ToLower(a.Admin), Operator: strings.ToLower(a.Operator),
		Officer: strings.ToLower(a.Officer), Monitor: strings.ToLower(a.Monitor), Funders: map[string]string{},
	}
	for k, v := range a.Funders {
		out.Funders[k] = strings.ToLower(v)
	}
	return out
}

func (m *Mock) Close() {}

// ---------------------------------------------------------------- helpers

func rev(kind ledger.ErrorKind, msg string) error { return &ledger.Error{Kind: kind, Message: msg} }

func (m *Mock) require(sender string, role uint64) error {
	if m.roles[sender]&role == 0 {
		return rev(ledger.KindUnauthorized, "missing role")
	}
	return nil
}

func (m *Mock) newTx(events []ledger.Event) ledger.Tx {
	m.block++
	m.txSeq++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], m.txSeq)
	h := sha256.Sum256(append([]byte("sellergraph-mock-tx"), b[:]...))
	if events == nil {
		events = []ledger.Event{}
	}
	return ledger.Tx{Hash: "0x" + hex.EncodeToString(h[:]), BlockNumber: m.block, Events: events}
}

func ev(name string, kv ...any) ledger.Event {
	f := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		f[kv[i].(string)] = kv[i+1]
	}
	return ledger.Event{Name: name, Fields: f}
}

func (m *Mock) getLoan(id string) (*loan, ledger.Hash, error) {
	key, err := ledger.IDToBytes32(id)
	if err != nil {
		return nil, key, err
	}
	l, ok := m.loans[key]
	if !ok {
		return nil, key, rev(ledger.KindNotFound, "loan")
	}
	return l, key, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (m *Mock) accrue(l *loan, asOf int64) error {
	if asOf < l.lastAccrual {
		return rev(ledger.KindInvalid, "asOf before last accrual")
	}
	if asOf > m.clock()+m.policy.MaxFutureSkewSec {
		return rev(ledger.KindInvalid, "asOf in the future")
	}
	if asOf == l.lastAccrual {
		return nil
	}
	num := new(big.Int).Mul(big.NewInt(l.principalOut), big.NewInt(l.rateBps))
	num.Mul(num, big.NewInt(asOf-l.lastAccrual))
	num.Add(num, big.NewInt(l.accrualRem))
	q, r := new(big.Int).QuoRem(num, accrualDenom, new(big.Int))
	l.interestAccrued += q.Int64()
	l.accrualRem = r.Int64()
	l.lastAccrual = asOf
	return nil
}

func setStatus(l *loan, to ledger.Status, reason ledger.Hash, evs *[]ledger.Event) {
	from := l.status
	l.status = to
	*evs = append(*evs, ev("StatusChanged", "loanId", ledger.Bytes32ToID(l.id), "fromStatus", int64(from), "toStatus", int64(to), "reasonHash", reason.Hex()))
}

func raise(l *loan, level ledger.AlertLevel, code string, evidence ledger.Hash, evs *[]ledger.Event) {
	if level > l.alert {
		l.alert = level
	}
	*evs = append(*evs, ev("AlertRaised", "loanId", ledger.Bytes32ToID(l.id), "level", int64(level), "code", code, "evidenceHash", evidence.Hex()))
}

func setOverdueAtLeast(l *loan, due int64, evs *[]ledger.Event) {
	owed := l.principalOut + l.interestAccrued
	if due > owed {
		due = owed
	}
	if due <= l.overduePrincipal+l.overdueInterest {
		return
	}
	oi := min64(due, l.interestAccrued)
	l.overdueInterest = oi
	l.overduePrincipal = due - oi
	*evs = append(*evs, ev("OverdueUpdated", "loanId", ledger.Bytes32ToID(l.id), "overduePrincipal", l.overduePrincipal, "overdueInterest", l.overdueInterest))
}

func maybeApprove(l *loan, evs *[]ledger.Event) {
	if l.status == ledger.StatusCreated && l.pendingFunders == 0 && (!l.humanReview || l.officerApproved) {
		setStatus(l, ledger.StatusApproved, l.decisionHash, evs)
	}
}

func shortfallHash(v int64) ledger.Hash {
	var h ledger.Hash
	big.NewInt(v).FillBytes(h[:])
	return h
}

// ---------------------------------------------------------------- admin

func (m *Mock) GrantRole(_ context.Context, account string, role uint64) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Admin, ledger.RoleAdmin); err != nil {
		return ledger.Tx{}, err
	}
	a := strings.ToLower(account)
	m.roles[a] |= role
	return m.newTx([]ledger.Event{ev("RoleGranted", "account", a, "role", int64(role))}), nil
}

func (m *Mock) SetPolicy(_ context.Context, p ledger.Policy) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Admin, ledger.RoleAdmin); err != nil {
		return ledger.Tx{}, err
	}
	if p.MaxRateBps <= 0 || p.MaxRateBps > bps {
		return ledger.Tx{}, rev(ledger.KindInvalid, "maxRateBps")
	}
	m.policy = p
	return m.newTx([]ledger.Event{ev("PolicyUpdated", "maxRateBps", p.MaxRateBps, "officerThreshold", p.OfficerThreshold, "maxFutureSkew", p.MaxFutureSkewSec)}), nil
}

func (m *Mock) GetPolicy(context.Context) (ledger.Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy, nil
}

func (m *Mock) HasRole(_ context.Context, account string, role uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.roles[strings.ToLower(account)]&role != 0, nil
}

// ---------------------------------------------------------------- info

func (m *Mock) Info(context.Context) (ledger.ChainInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acc := map[string]string{"admin": m.accounts.Admin, "operator": m.accounts.Operator, "officer": m.accounts.Officer, "monitor": m.accounts.Monitor}
	for k, v := range m.accounts.Funders {
		acc["funder:"+k] = v
	}
	return ledger.ChainInfo{Backend: "mock", BlockNumber: m.block, ChainTime: m.clock(), Accounts: acc, Policy: m.policy}, nil
}

func (m *Mock) Now(context.Context) (int64, error) { return m.clock(), nil }

// ---------------------------------------------------------------- origination

func (m *Mock) CreateLoan(_ context.Context, r ledger.CreateLoan) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sender := m.accounts.Operator
	if err := m.require(sender, ledger.RoleOperator); err != nil {
		return ledger.Tx{}, err
	}
	key, err := ledger.IDToBytes32(r.LoanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	switch {
	case m.loans[key] != nil:
		return ledger.Tx{}, rev(ledger.KindDuplicate, "loan exists")
	case r.SellerRef.IsZero():
		return ledger.Tx{}, rev(ledger.KindInvalid, "sellerRef")
	case r.Tier != 1 && r.Tier != 2:
		return ledger.Tx{}, rev(ledger.KindInvalid, "tier")
	case r.Principal <= 0:
		return ledger.Tx{}, rev(ledger.KindInvalid, "principal")
	case r.RateBps < 0 || r.RateBps > m.policy.MaxRateBps:
		return ledger.Tx{}, rev(ledger.KindInvalid, "rate above policy ceiling")
	case r.TenorDays <= 0 || r.TenorDays > maxTenorDays:
		return ledger.Tx{}, rev(ledger.KindInvalid, "tenorDays")
	case r.SweepCapBps < 0 || r.SweepRateBps < 0 || r.SweepCapBps > bps || r.SweepRateBps > r.SweepCapBps:
		return ledger.Tx{}, rev(ledger.KindInvalid, "sweep rate must be <= sweep cap <= 100%")
	case r.GraceDays < 0 || r.GraceDays >= r.TenorDays:
		return ledger.Tx{}, rev(ledger.KindInvalid, "graceDays must be < tenorDays")
	case r.DecisionHash.IsZero():
		return ledger.Tx{}, rev(ledger.KindInvalid, "decisionHash")
	case len(r.Funding) == 0:
		return ledger.Tx{}, rev(ledger.KindInvalid, "funding shares")
	}
	l := &loan{
		id: key, sellerRef: r.SellerRef, tier: r.Tier, principal: r.Principal, rateBps: r.RateBps, tenorDays: r.TenorDays,
		sweepRateBps: r.SweepRateBps, sweepCapBps: r.SweepCapBps, grace: r.GraceDays, decisionHash: r.DecisionHash,
		humanReview: r.HumanReviewRequired || (r.Tier == 2 && r.Principal > m.policy.OfficerThreshold),
		createdAt:   m.clock(), status: ledger.StatusCreated, shares: map[string]int64{}, confirmed: map[string]bool{},
	}
	evs := []ledger.Event{ev("LoanCreated", "loanId", r.LoanID, "sellerRef", r.SellerRef.Hex(), "tier", int64(r.Tier),
		"principal", r.Principal, "humanReviewRequired", l.humanReview, "decisionHash", r.DecisionHash.Hex())}
	var sum int64
	for _, s := range r.Funding {
		f := strings.ToLower(s.Funder)
		switch {
		case m.roles[f]&ledger.RoleFunder == 0:
			return ledger.Tx{}, rev(ledger.KindInvalid, "not a registered funder")
		case s.Amount <= 0 || s.Amount > r.Principal:
			return ledger.Tx{}, rev(ledger.KindInvalid, "share amount")
		case l.shares[f] != 0:
			return ledger.Tx{}, rev(ledger.KindDuplicate, "funder listed twice")
		}
		l.shares[f] = s.Amount
		l.funders = append(l.funders, f)
		sum += s.Amount
		if f == sender {
			l.confirmed[f] = true
			evs = append(evs, ev("FundingConfirmed", "loanId", r.LoanID, "funder", f, "amount", s.Amount))
		} else {
			l.pendingFunders++
		}
	}
	if sum != r.Principal {
		return ledger.Tx{}, rev(ledger.KindInvalid, "funding shares must sum to principal")
	}
	maybeApprove(l, &evs)
	m.loans[key] = l
	m.order = append(m.order, key)
	return m.newTx(evs), nil
}

func (m *Mock) ConfirmFunding(_ context.Context, loanID, funder string) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sender := strings.ToLower(funder)
	if err := m.require(sender, ledger.RoleFunder); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if l0.status != ledger.StatusCreated {
		return ledger.Tx{}, rev(ledger.KindBadStatus, "not awaiting funding")
	}
	amt := l0.shares[sender]
	if amt == 0 {
		return ledger.Tx{}, rev(ledger.KindUnauthorized, "not a funder of this loan")
	}
	if l0.confirmed[sender] {
		return ledger.Tx{}, rev(ledger.KindDuplicate, "already confirmed")
	}
	l := l0.clone()
	l.confirmed[sender] = true
	l.pendingFunders--
	evs := []ledger.Event{ev("FundingConfirmed", "loanId", loanID, "funder", sender, "amount", amt)}
	maybeApprove(l, &evs)
	m.loans[key] = l
	return m.newTx(evs), nil
}

func (m *Mock) OfficerDecision(_ context.Context, loanID string, approved bool, reviewHash ledger.Hash) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sender := m.accounts.Officer
	if err := m.require(sender, ledger.RoleOfficer); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if l0.status != ledger.StatusCreated {
		return ledger.Tx{}, rev(ledger.KindBadStatus, "not awaiting sign-off")
	}
	if reviewHash.IsZero() {
		return ledger.Tx{}, rev(ledger.KindInvalid, "reviewHash")
	}
	l := l0.clone()
	evs := []ledger.Event{ev("OfficerDecision", "loanId", loanID, "officer", sender, "approved", approved, "reviewHash", reviewHash.Hex())}
	if approved {
		l.officerApproved = true
		maybeApprove(l, &evs)
	} else {
		setStatus(l, ledger.StatusCancelled, reviewHash, &evs)
	}
	m.loans[key] = l
	return m.newTx(evs), nil
}

func (m *Mock) CancelLoan(_ context.Context, loanID string, reasonHash ledger.Hash) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Operator, ledger.RoleOperator); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if l0.status != ledger.StatusCreated && l0.status != ledger.StatusApproved {
		return ledger.Tx{}, rev(ledger.KindBadStatus, "cannot cancel after disbursement")
	}
	l := l0.clone()
	var evs []ledger.Event
	setStatus(l, ledger.StatusCancelled, reasonHash, &evs)
	m.loans[key] = l
	return m.newTx(evs), nil
}

// ---------------------------------------------------------------- disbursement & repayment

func (m *Mock) RecordDisbursement(_ context.Context, r ledger.Disbursement) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Operator, ledger.RoleOperator); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(r.LoanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	switch {
	case l0.status != ledger.StatusApproved:
		return ledger.Tx{}, rev(ledger.KindBadStatus, "loan not approved")
	case len(r.Payments) == 0:
		return ledger.Tx{}, rev(ledger.KindInvalid, "payment lines")
	case r.BankTxRef.IsZero():
		return ledger.Tx{}, rev(ledger.KindInvalid, "bankTxRef")
	case r.DeliveryDays <= 0 || r.DeliveryDays > 365:
		return ledger.Tx{}, rev(ledger.KindInvalid, "deliveryDays")
	}
	l := l0.clone()
	now := m.clock()
	deadline := now + r.DeliveryDays*day
	var sum int64
	var evs []ledger.Event
	for i, p := range r.Payments {
		if p.PayeeRef.IsZero() || p.Amount <= 0 || p.Amount > l.principal {
			return ledger.Tx{}, rev(ledger.KindInvalid, "payment line")
		}
		l.payments = append(l.payments, payment{payeeRef: p.PayeeRef, amount: p.Amount, deadline: deadline})
		sum += p.Amount
		evs = append(evs, ev("PayeePaid", "loanId", r.LoanID, "line", int64(i), "payeeRef", p.PayeeRef.Hex(), "amount", p.Amount, "deadline", deadline))
	}
	if sum != l.principal {
		return ledger.Tx{}, rev(ledger.KindInvalid, "payments must sum to principal (single, full disbursement)")
	}
	l.disbursedAt, l.lastAccrual = now, now
	l.graceEndsAt = now + l.grace*day
	l.maturity = now + l.tenorDays*day
	l.principalOut = l.principal
	evs = append(evs, ev("Disbursed", "loanId", r.LoanID, "amount", l.principal, "bankTxRef", r.BankTxRef.Hex(),
		"disbursedAt", l.disbursedAt, "graceEndsAt", l.graceEndsAt, "maturityAt", l.maturity))
	setStatus(l, ledger.StatusActive, r.BankTxRef, &evs)
	m.totalOutstanding += l.principal
	m.loans[key] = l
	return m.newTx(evs), nil
}

func (m *Mock) RecordRepayment(_ context.Context, r ledger.Repayment) (ledger.RepaymentResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var res ledger.RepaymentResult
	if err := m.require(m.accounts.Operator, ledger.RoleOperator); err != nil {
		return res, err
	}
	l0, key, err := m.getLoan(r.LoanID)
	if err != nil {
		return res, err
	}
	switch {
	case l0.status != ledger.StatusActive && l0.status != ledger.StatusAccelerated && l0.status != ledger.StatusDefaulted:
		return res, rev(ledger.KindBadStatus, "loan not repayable")
	case r.Amount <= 0:
		return res, rev(ledger.KindInvalid, "amount")
	case r.Source < 1 || r.Source > 7:
		return res, rev(ledger.KindInvalid, "source")
	case r.ExtRef.IsZero():
		return res, rev(ledger.KindInvalid, "extRef")
	}
	refKey := ledger.Hash(crypto.Keccak256Hash(key[:], r.ExtRef[:]))
	if m.usedRef[refKey] {
		return res, rev(ledger.KindDuplicate, "repayment reference already recorded")
	}
	l := l0.clone()
	if err := m.accrue(l, r.AsOf); err != nil {
		return res, err
	}
	var a [5]int64
	rest := r.Amount
	a[0] = min64(rest, l.overdueInterest)
	rest -= a[0]
	a[1] = min64(rest, l.overduePrincipal)
	rest -= a[1]
	a[2] = min64(rest, l.interestAccrued-a[0])
	rest -= a[2]
	a[3] = min64(rest, l.principalOut-a[1])
	rest -= a[3]
	a[4] = rest

	l.overdueInterest -= a[0]
	l.overduePrincipal -= a[1]
	l.interestAccrued -= a[0] + a[2]
	l.principalOut -= a[1] + a[3]
	l.interestRepaid += a[0] + a[2]
	l.principalRepaid += a[1] + a[3]
	l.totalRepaid += r.Amount - a[4]
	l.refundDue += a[4]

	evs := []ledger.Event{ev("RepaymentRecorded", "loanId", r.LoanID, "extRef", r.ExtRef.Hex(), "source", int64(r.Source),
		"amount", r.Amount, "allocation", []int64{a[0], a[1], a[2], a[3], a[4]}, "asOf", r.AsOf)}
	if a[0]+a[1] > 0 {
		evs = append(evs, ev("OverdueUpdated", "loanId", r.LoanID, "overduePrincipal", l.overduePrincipal, "overdueInterest", l.overdueInterest))
	}
	if l.principalOut == 0 && l.interestAccrued == 0 {
		l.accrualRem = 0
		l.alert = ledger.AlertNone
		setStatus(l, ledger.StatusSettled, r.ExtRef, &evs)
	}
	m.usedRef[refKey] = true
	m.totalOutstanding -= a[1] + a[3]
	m.loans[key] = l
	res.Tx = m.newTx(evs)
	res.Allocation = ledger.Allocation{ToOverdueInterest: a[0], ToOverduePrincipal: a[1], ToInterest: a[2], ToPrincipal: a[3], Excess: a[4]}
	res.Settled = l.status == ledger.StatusSettled
	return res, nil
}

func (m *Mock) CheckpointFloor(_ context.Context, r ledger.FloorCheckpoint) (ledger.FloorResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var res ledger.FloorResult
	if err := m.require(m.accounts.Operator, ledger.RoleOperator|ledger.RoleMonitor); err != nil {
		return res, err
	}
	l0, key, err := m.getLoan(r.LoanID)
	if err != nil {
		return res, err
	}
	if l0.status != ledger.StatusActive && l0.status != ledger.StatusAccelerated {
		return res, rev(ledger.KindBadStatus, "loan not active")
	}
	if r.FloorCumulative < l0.floorCumulative {
		return res, rev(ledger.KindInvalid, "floor cannot decrease")
	}
	l := l0.clone()
	if err := m.accrue(l, r.AsOf); err != nil {
		return res, err
	}
	l.floorCumulative = r.FloorCumulative
	var shortfall int64
	if r.FloorCumulative > l.totalRepaid {
		shortfall = r.FloorCumulative - l.totalRepaid
	}
	evs := []ledger.Event{ev("FloorCheckpoint", "loanId", r.LoanID, "floorCumulative", r.FloorCumulative, "totalRepaid", l.totalRepaid,
		"shortfall", shortfall, "milestone", r.Milestone, "asOf", r.AsOf)}
	if shortfall > 0 {
		raise(l, ledger.AlertYellow, "FLOOR_SHORTFALL", shortfallHash(shortfall), &evs)
		if r.Milestone {
			setOverdueAtLeast(l, shortfall, &evs)
		}
	}
	m.loans[key] = l
	return ledger.FloorResult{Tx: m.newTx(evs), Shortfall: shortfall}, nil
}

func (m *Mock) MarkMatured(_ context.Context, loanID string, asOf int64) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Operator, ledger.RoleOperator|ledger.RoleMonitor); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if l0.status != ledger.StatusActive && l0.status != ledger.StatusAccelerated {
		return ledger.Tx{}, rev(ledger.KindBadStatus, "loan not active")
	}
	if asOf < l0.maturity {
		return ledger.Tx{}, rev(ledger.KindInvalid, "not matured yet")
	}
	l := l0.clone()
	if err := m.accrue(l, asOf); err != nil {
		return ledger.Tx{}, err
	}
	var evs []ledger.Event
	setOverdueAtLeast(l, l.principalOut+l.interestAccrued, &evs)
	raise(l, ledger.AlertRed, "MATURED_UNPAID", ledger.ZeroHash, &evs)
	m.loans[key] = l
	return m.newTx(evs), nil
}

func (m *Mock) Accelerate(_ context.Context, loanID string, reasonHash ledger.Hash, asOf int64) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Officer, ledger.RoleOfficer); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if l0.status != ledger.StatusActive {
		return ledger.Tx{}, rev(ledger.KindBadStatus, "loan not active")
	}
	if reasonHash.IsZero() {
		return ledger.Tx{}, rev(ledger.KindInvalid, "reasonHash")
	}
	l := l0.clone()
	if err := m.accrue(l, asOf); err != nil {
		return ledger.Tx{}, err
	}
	var evs []ledger.Event
	setOverdueAtLeast(l, l.principalOut+l.interestAccrued, &evs)
	raise(l, ledger.AlertRed, "ACCELERATED", reasonHash, &evs)
	setStatus(l, ledger.StatusAccelerated, reasonHash, &evs)
	m.loans[key] = l
	return m.newTx(evs), nil
}

func (m *Mock) MarkDefault(_ context.Context, loanID string, reasonHash ledger.Hash) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Officer, ledger.RoleOfficer); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if l0.status != ledger.StatusActive && l0.status != ledger.StatusAccelerated {
		return ledger.Tx{}, rev(ledger.KindBadStatus, "loan not active")
	}
	if reasonHash.IsZero() {
		return ledger.Tx{}, rev(ledger.KindInvalid, "reasonHash")
	}
	l := l0.clone()
	var evs []ledger.Event
	setStatus(l, ledger.StatusDefaulted, reasonHash, &evs)
	m.loans[key] = l
	return m.newTx(evs), nil
}

func (m *Mock) Restructure(_ context.Context, r ledger.Restructure) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Officer, ledger.RoleOfficer); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(r.LoanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	switch {
	case l0.status != ledger.StatusActive && l0.status != ledger.StatusAccelerated:
		return ledger.Tx{}, rev(ledger.KindBadStatus, "loan not active")
	case r.ReasonHash.IsZero():
		return ledger.Tx{}, rev(ledger.KindInvalid, "reasonHash")
	case r.ExtraDays < 0 || r.ExtraDays > 365:
		return ledger.Tx{}, rev(ledger.KindInvalid, "extraDays")
	case r.NewSweepRateBps < 0 || r.NewSweepRateBps > l0.sweepCapBps:
		return ledger.Tx{}, rev(ledger.KindInvalid, "sweep rate above cap")
	}
	l := l0.clone()
	if err := m.accrue(l, r.AsOf); err != nil {
		return ledger.Tx{}, err
	}
	l.maturity += r.ExtraDays * day
	l.tenorDays += r.ExtraDays
	l.sweepRateBps = r.NewSweepRateBps
	l.overduePrincipal, l.overdueInterest = 0, 0
	l.floorCumulative = l.totalRepaid
	l.alert = ledger.AlertYellow
	evs := []ledger.Event{
		ev("OverdueUpdated", "loanId", r.LoanID, "overduePrincipal", int64(0), "overdueInterest", int64(0)),
		ev("Restructured", "loanId", r.LoanID, "newMaturityAt", l.maturity, "newSweepRateBps", r.NewSweepRateBps, "reasonHash", r.ReasonHash.Hex()),
	}
	if l.status != ledger.StatusActive {
		setStatus(l, ledger.StatusActive, r.ReasonHash, &evs)
	}
	m.loans[key] = l
	return m.newTx(evs), nil
}

// ---------------------------------------------------------------- monitoring

func (m *Mock) RaiseAlert(_ context.Context, loanID string, level ledger.AlertLevel, code string, evidence ledger.Hash) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Monitor, ledger.RoleMonitor); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if level < ledger.AlertGreen || level > ledger.AlertRed {
		return ledger.Tx{}, rev(ledger.KindInvalid, "level")
	}
	if _, err := ledger.CodeToBytes32(code); err != nil {
		return ledger.Tx{}, err
	}
	l := l0.clone()
	l.alert = level
	m.loans[key] = l
	return m.newTx([]ledger.Event{ev("AlertRaised", "loanId", loanID, "level", int64(level), "code", code, "evidenceHash", evidence.Hex())}), nil
}

func (m *Mock) RecordCovenantEvent(_ context.Context, loanID, code string, breached bool, evidence ledger.Hash) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Monitor, ledger.RoleMonitor); err != nil {
		return ledger.Tx{}, err
	}
	l, _, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	if l.status != ledger.StatusActive && l.status != ledger.StatusAccelerated {
		return ledger.Tx{}, rev(ledger.KindBadStatus, "loan not active")
	}
	if _, err := ledger.CodeToBytes32(code); err != nil {
		return ledger.Tx{}, err
	}
	return m.newTx([]ledger.Event{ev("CovenantEvent", "loanId", loanID, "code", code, "breached", breached, "evidenceHash", evidence.Hex())}), nil
}

func (m *Mock) RecordDelivery(_ context.Context, loanID string, line int, waybill ledger.Hash, shippedAt int64) (ledger.DeliveryResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var res ledger.DeliveryResult
	if err := m.require(m.accounts.Operator, ledger.RoleOperator|ledger.RoleMonitor); err != nil {
		return res, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return res, err
	}
	switch {
	case l0.status < ledger.StatusActive || l0.status == ledger.StatusCancelled:
		return res, rev(ledger.KindBadStatus, "not disbursed")
	case line < 0 || line >= len(l0.payments):
		return res, rev(ledger.KindNotFound, "payment line")
	case waybill.IsZero():
		return res, rev(ledger.KindInvalid, "waybillHash")
	case l0.payments[line].delivered:
		return res, rev(ledger.KindDuplicate, "delivery already recorded")
	case shippedAt < l0.disbursedAt || shippedAt > m.clock()+m.policy.MaxFutureSkewSec:
		return res, rev(ledger.KindInvalid, "shippedAt")
	}
	l := l0.clone()
	p := &l.payments[line]
	p.delivered, p.waybill, p.shippedAt = true, waybill, shippedAt
	onTime := shippedAt <= p.deadline
	evs := []ledger.Event{ev("DeliveryRecorded", "loanId", loanID, "line", int64(line), "waybillHash", waybill.Hex(), "shippedAt", shippedAt, "onTime", onTime)}
	if !onTime {
		evs = append(evs, ev("CovenantEvent", "loanId", loanID, "code", "DELIVERY_LATE", "breached", true, "evidenceHash", waybill.Hex()))
	}
	m.loans[key] = l
	return ledger.DeliveryResult{Tx: m.newTx(evs), OnTime: onTime}, nil
}

func (m *Mock) FlagMissingDelivery(_ context.Context, loanID string, line int, asOf int64) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Monitor, ledger.RoleMonitor); err != nil {
		return ledger.Tx{}, err
	}
	l0, key, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Tx{}, err
	}
	switch {
	case l0.status != ledger.StatusActive && l0.status != ledger.StatusAccelerated:
		return ledger.Tx{}, rev(ledger.KindBadStatus, "loan not active")
	case line < 0 || line >= len(l0.payments):
		return ledger.Tx{}, rev(ledger.KindNotFound, "payment line")
	case asOf > m.clock()+m.policy.MaxFutureSkewSec:
		return ledger.Tx{}, rev(ledger.KindInvalid, "asOf in the future")
	case l0.payments[line].delivered:
		return ledger.Tx{}, rev(ledger.KindInvalid, "already delivered")
	case asOf <= l0.payments[line].deadline:
		return ledger.Tx{}, rev(ledger.KindInvalid, "deadline not passed")
	}
	l := l0.clone()
	p := l.payments[line]
	evs := []ledger.Event{ev("CovenantEvent", "loanId", loanID, "code", "DELIVERY_MISSING", "breached", true, "evidenceHash", p.payeeRef.Hex())}
	raise(l, ledger.AlertRed, "DELIVERY_MISSING", p.payeeRef, &evs)
	m.loans[key] = l
	return m.newTx(evs), nil
}

// ---------------------------------------------------------------- reads

func (m *Mock) GetLoan(_ context.Context, loanID string) (ledger.Loan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, _, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Loan{}, err
	}
	out := ledger.Loan{
		LoanID: loanID, SellerRef: l.sellerRef, Tier: l.tier, Principal: l.principal, RateBps: l.rateBps, TenorDays: l.tenorDays,
		SweepRateBps: l.sweepRateBps, SweepCapBps: l.sweepCapBps, GraceDays: l.grace, DecisionHash: l.decisionHash,
		HumanReviewRequired: l.humanReview, OfficerApproved: l.officerApproved, CreatedAt: l.createdAt, PendingFunders: l.pendingFunders,
		Status: l.status, AlertLevel: l.alert, DisbursedAt: l.disbursedAt, GraceEndsAt: l.graceEndsAt, MaturityAt: l.maturity,
		LastAccrualAt: l.lastAccrual, PrincipalOutstanding: l.principalOut, InterestAccrued: l.interestAccrued,
		OverduePrincipal: l.overduePrincipal, OverdueInterest: l.overdueInterest, TotalRepaid: l.totalRepaid,
		PrincipalRepaid: l.principalRepaid, InterestRepaid: l.interestRepaid, FloorCumulative: l.floorCumulative, RefundDue: l.refundDue,
		Funding: []ledger.Funding{}, Payments: []ledger.Payment{},
	}
	for _, f := range l.funders {
		out.Funding = append(out.Funding, ledger.Funding{Funder: f, Amount: l.shares[f], Confirmed: l.confirmed[f]})
	}
	for i, p := range l.payments {
		out.Payments = append(out.Payments, ledger.Payment{Line: i, PayeeRef: p.payeeRef, Amount: p.amount, Deadline: p.deadline,
			Delivered: p.delivered, WaybillHash: p.waybill, ShippedAt: p.shippedAt})
	}
	return out, nil
}

func (m *Mock) ListLoanIDs(_ context.Context, offset, limit int) ([]string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := len(m.order)
	ids := []string{}
	for i := offset; i < total && (limit <= 0 || i < offset+limit); i++ {
		if i >= 0 {
			ids = append(ids, ledger.Bytes32ToID(m.order[i]))
		}
	}
	return ids, total, nil
}

func (m *Mock) PreviewPayoff(_ context.Context, loanID string, asOf int64) (ledger.Payoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, _, err := m.getLoan(loanID)
	if err != nil {
		return ledger.Payoff{}, err
	}
	p := ledger.Payoff{Principal: l.principalOut, Interest: l.interestAccrued, AsOf: asOf}
	if (l.status == ledger.StatusActive || l.status == ledger.StatusAccelerated || l.status == ledger.StatusDefaulted) && asOf > l.lastAccrual {
		num := new(big.Int).Mul(big.NewInt(l.principalOut), big.NewInt(l.rateBps))
		num.Mul(num, big.NewInt(asOf-l.lastAccrual))
		num.Add(num, big.NewInt(l.accrualRem))
		p.Interest += new(big.Int).Quo(num, accrualDenom).Int64()
	}
	p.Total = p.Principal + p.Interest
	return p, nil
}

// ---------------------------------------------------------------- audit trail

func (m *Mock) Anchor(_ context.Context, r ledger.AnchorRequest) (ledger.AnchorRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writer := m.accounts.Operator
	if err := m.require(writer, ledger.RoleAuditWriter); err != nil {
		return ledger.AnchorRecord{}, err
	}
	subj, err := ledger.IDToBytes32(r.Subject)
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	switch {
	case r.Kind < ledger.KindApplication || r.Kind > ledger.KindOther:
		return ledger.AnchorRecord{}, rev(ledger.KindInvalid, "kind")
	case r.ContentHash.IsZero():
		return ledger.AnchorRecord{}, rev(ledger.KindInvalid, "contentHash")
	case len(r.URI) > 256:
		return ledger.AnchorRecord{}, rev(ledger.KindInvalid, "uri too long")
	}
	seenKey := ledger.Hash(crypto.Keccak256Hash(subj[:], r.ContentHash[:]))
	if m.seen[seenKey] {
		return ledger.AnchorRecord{}, rev(ledger.KindDuplicate, "already anchored for this subject")
	}
	ts := m.clock()
	head := ledger.ChainHead(m.heads[subj], r.Kind, r.ContentHash, ts)
	idx := int64(len(m.records[subj]))
	m.records[subj] = append(m.records[subj], record{kind: r.Kind, contentHash: r.ContentHash, ts: ts, writer: writer, head: head})
	m.heads[subj] = head
	m.seen[seenKey] = true
	if _, ok := m.firstAnchor[r.ContentHash]; !ok {
		m.firstAnchor[r.ContentHash] = [2]any{subj, idx}
	}
	tx := m.newTx([]ledger.Event{ev("Anchored", "subject", r.Subject, "index", idx, "kind", int64(r.Kind), "contentHash", r.ContentHash.Hex(),
		"head", head.Hex(), "timestamp", ts, "uri", r.URI)})
	return ledger.AnchorRecord{Subject: r.Subject, Index: idx, Kind: r.Kind, ContentHash: r.ContentHash, Timestamp: ts, Writer: writer, Head: head, Tx: &tx}, nil
}

func (m *Mock) ListAnchors(_ context.Context, subject string) ([]ledger.AnchorRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subj, err := ledger.IDToBytes32(subject)
	if err != nil {
		return nil, err
	}
	out := []ledger.AnchorRecord{}
	for i, r := range m.records[subj] {
		out = append(out, ledger.AnchorRecord{Subject: subject, Index: int64(i), Kind: r.kind, ContentHash: r.contentHash, Timestamp: r.ts, Writer: r.writer, Head: r.head})
	}
	return out, nil
}

func (m *Mock) LocateAnchor(_ context.Context, contentHash ledger.Hash) (ledger.AnchorRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.firstAnchor[contentHash]
	if !ok {
		return ledger.AnchorRecord{}, false, nil
	}
	subj, idx := f[0].(ledger.Hash), f[1].(int64)
	r := m.records[subj][idx]
	return ledger.AnchorRecord{Subject: ledger.Bytes32ToID(subj), Index: idx, Kind: r.kind, ContentHash: r.contentHash, Timestamp: r.ts, Writer: r.writer, Head: r.head}, true, nil
}

var _ ledger.Ledger = (*Mock)(nil)
var _ ledger.Admin = (*Mock)(nil)
