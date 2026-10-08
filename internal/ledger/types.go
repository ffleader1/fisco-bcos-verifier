// Package ledger defines the SellerGraph blockchain ledger: domain types and the Ledger interface implemented by
// the FISCO BCOS adapter (package fisco) and the in-memory mock (package mock).
//
// Amounts are int64 minor currency units (fen). Times are unix seconds. Hash is a 32-byte value
// (references, content hashes). No personal data is ever passed to a Ledger: seller / supplier identifiers are
// turned into salted references by package hashing before they reach this layer.
package ledger

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Hash is a 32-byte value as stored on chain (bytes32).
type Hash [32]byte

// ZeroHash is the empty bytes32.
var ZeroHash Hash

func (h Hash) IsZero() bool                 { return h == ZeroHash }
func (h Hash) Hex() string                  { return "0x" + hex.EncodeToString(h[:]) }
func (h Hash) String() string               { return h.Hex() }
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.Hex()), nil }
func (h *Hash) UnmarshalText(b []byte) error {
	p, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = p
	return nil
}

// ParseHash parses a 0x-prefixed (or bare) 64-char hex string.
func ParseHash(s string) (Hash, error) {
	var h Hash
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) != 64 {
		return h, fmt.Errorf("hash must be 32 bytes hex, got %d hex chars", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("invalid hex: %w", err)
	}
	copy(h[:], b)
	return h, nil
}

// ---------------------------------------------------------------- enums

// Status mirrors LoanLedger S_* constants.
type Status uint8

const (
	StatusNone        Status = 0
	StatusCreated     Status = 1
	StatusApproved    Status = 2
	StatusActive      Status = 3
	StatusSettled     Status = 4
	StatusDefaulted   Status = 5
	StatusCancelled   Status = 6
	StatusAccelerated Status = 7
)

var statusNames = []string{"none", "created", "approved", "active", "settled", "defaulted", "cancelled", "accelerated"}

func (s Status) String() string {
	if int(s) < len(statusNames) {
		return statusNames[s]
	}
	return fmt.Sprintf("status(%d)", uint8(s))
}
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// AlertLevel mirrors LoanLedger A_* constants (Part 11: green / yellow / red).
type AlertLevel uint8

const (
	AlertNone   AlertLevel = 0
	AlertGreen  AlertLevel = 1
	AlertYellow AlertLevel = 2
	AlertRed    AlertLevel = 3
)

var alertNames = []string{"none", "green", "yellow", "red"}

func (a AlertLevel) String() string {
	if int(a) < len(alertNames) {
		return alertNames[a]
	}
	return fmt.Sprintf("alert(%d)", uint8(a))
}
func (a AlertLevel) MarshalText() ([]byte, error) { return []byte(a.String()), nil }
func (a *AlertLevel) UnmarshalText(b []byte) error {
	for i, n := range alertNames {
		if strings.EqualFold(n, string(b)) && i > 0 {
			*a = AlertLevel(i)
			return nil
		}
	}
	return fmt.Errorf("alert level must be green|yellow|red, got %q", b)
}

// RepaymentSource mirrors the `source` argument of LoanLedger.recordRepayment (Part 10 recovery mechanisms).
type RepaymentSource uint8

const (
	SourceWeBankSweep       RepaymentSource = 1 // debit of the seller's WeBank account at a schedule milestone
	SourceAuthorizedDebit   RepaymentSource = 2 // debit of another bank account the seller authorized
	SourceProportionalSweep RepaymentSource = 3 // % of a platform payout that landed at WeBank
	SourceFloorMakeup       RepaymentSource = 4 // make-up debit after a cumulative-floor shortfall
	SourcePrepayment        RepaymentSource = 5 // voluntary prepayment (no fee)
	SourceRecovery          RepaymentSource = 6 // collection / recovery after default
	SourceOther             RepaymentSource = 7
)

var sourceNames = []string{"", "webank_sweep", "authorized_debit", "proportional_sweep", "floor_makeup", "prepayment", "recovery", "other"}

func (s RepaymentSource) String() string {
	if int(s) < len(sourceNames) && s > 0 {
		return sourceNames[s]
	}
	return fmt.Sprintf("source(%d)", uint8(s))
}
func (s RepaymentSource) MarshalText() ([]byte, error) { return []byte(s.String()), nil }
func (s *RepaymentSource) UnmarshalText(b []byte) error {
	for i, n := range sourceNames {
		if i > 0 && strings.EqualFold(n, string(b)) {
			*s = RepaymentSource(i)
			return nil
		}
	}
	return fmt.Errorf("unknown repayment source %q (want one of %s)", b, strings.Join(sourceNames[1:], ", "))
}

// AnchorKind mirrors AuditTrail K_* constants.
type AnchorKind uint8

const (
	KindApplication     AnchorKind = 1
	KindMLOutput        AnchorKind = 2
	KindAgentIO         AnchorKind = 3
	KindCreditMemo      AnchorKind = 4
	KindRiskChallenge   AnchorKind = 5
	KindHumanDecision   AnchorKind = 6
	KindOfferContract   AnchorKind = 7
	KindExplanation     AnchorKind = 8
	KindSweepNotice     AnchorKind = 9
	KindDecisionPackage AnchorKind = 10
	KindOther           AnchorKind = 11
)

var kindNames = []string{"", "application", "ml_output", "agent_io", "credit_memo", "risk_challenge", "human_decision",
	"offer_contract", "explanation", "sweep_notice", "decision_package", "other"}

func (k AnchorKind) String() string {
	if int(k) < len(kindNames) && k > 0 {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}
func (k AnchorKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }
func (k *AnchorKind) UnmarshalText(b []byte) error {
	for i, n := range kindNames {
		if i > 0 && strings.EqualFold(n, string(b)) {
			*k = AnchorKind(i)
			return nil
		}
	}
	return fmt.Errorf("unknown anchor kind %q (want one of %s)", b, strings.Join(kindNames[1:], ", "))
}

// Role bits mirror Roles.sol.
const (
	RoleAdmin       uint64 = 1
	RoleOperator    uint64 = 2
	RoleOfficer     uint64 = 4
	RoleMonitor     uint64 = 8
	RoleFunder      uint64 = 16
	RoleAuditWriter uint64 = 32
)

// ---------------------------------------------------------------- requests

// FundingShare is one lender's share of a loan. Funder is a 0x address.
type FundingShare struct {
	Funder string `json:"funder"`
	Amount int64  `json:"amount"`
}

// CreateLoan registers an approved credit decision on chain.
type CreateLoan struct {
	LoanID              string
	SellerRef           Hash
	Tier                uint8
	Principal           int64
	RateBps             int64
	TenorDays           int64
	SweepRateBps        int64
	SweepCapBps         int64
	GraceDays           int64
	HumanReviewRequired bool
	DecisionHash        Hash
	Funding             []FundingShare
}

// PaymentLine is one supplier payment inside the single disbursement.
type PaymentLine struct {
	PayeeRef Hash
	Amount   int64
}

type Disbursement struct {
	LoanID       string
	Payments     []PaymentLine
	DeliveryDays int64
	BankTxRef    Hash
}

type Repayment struct {
	LoanID string
	Amount int64
	Source RepaymentSource
	ExtRef Hash
	AsOf   int64
}

type FloorCheckpoint struct {
	LoanID          string
	FloorCumulative int64
	AsOf            int64
	Milestone       bool
}

type Restructure struct {
	LoanID          string
	ExtraDays       int64
	NewSweepRateBps int64
	ReasonHash      Hash
	AsOf            int64
}

type AnchorRequest struct {
	Subject     string
	Kind        AnchorKind
	ContentHash Hash
	URI         string
}

type Policy struct {
	MaxRateBps       int64 `json:"maxRateBps"`
	OfficerThreshold int64 `json:"officerThreshold"`
	MaxFutureSkewSec int64 `json:"maxFutureSkewSec"`
}

// ---------------------------------------------------------------- results

// Event is a decoded contract event. Field names follow the Solidity ABI; values are JSON-friendly
// (int64 for integers that fit, 0x-hex strings for bytes32/addresses, bool, []int64 for arrays,
// ASCII strings for code fields).
type Event struct {
	Name   string         `json:"name"`
	Fields map[string]any `json:"fields"`
}

// Tx is the result of a state-changing call.
type Tx struct {
	Hash        string  `json:"hash"`
	BlockNumber int64   `json:"blockNumber"`
	Events      []Event `json:"events"`
}

// Allocation of one repayment (Part 10.3: overdue -> interest -> principal).
type Allocation struct {
	ToOverdueInterest  int64 `json:"toOverdueInterest"`
	ToOverduePrincipal int64 `json:"toOverduePrincipal"`
	ToInterest         int64 `json:"toInterest"`
	ToPrincipal        int64 `json:"toPrincipal"`
	Excess             int64 `json:"excess"`
}

type RepaymentResult struct {
	Tx         Tx         `json:"tx"`
	Allocation Allocation `json:"allocation"`
	Settled    bool       `json:"settled"`
}

type FloorResult struct {
	Tx        Tx    `json:"tx"`
	Shortfall int64 `json:"shortfall"`
}

type DeliveryResult struct {
	Tx     Tx   `json:"tx"`
	OnTime bool `json:"onTime"`
}

type Payment struct {
	Line        int   `json:"line"`
	PayeeRef    Hash  `json:"payeeRef"`
	Amount      int64 `json:"amount"`
	Deadline    int64 `json:"deadline"`
	Delivered   bool  `json:"delivered"`
	WaybillHash Hash  `json:"waybillHash"`
	ShippedAt   int64 `json:"shippedAt"`
}

type Funding struct {
	Funder    string `json:"funder"`
	Amount    int64  `json:"amount"`
	Confirmed bool   `json:"confirmed"`
}

// Loan is a full on-chain snapshot.
type Loan struct {
	LoanID              string     `json:"loanId"`
	SellerRef           Hash       `json:"sellerRef"`
	Tier                uint8      `json:"tier"`
	Principal           int64      `json:"principal"`
	RateBps             int64      `json:"rateBps"`
	TenorDays           int64      `json:"tenorDays"`
	SweepRateBps        int64      `json:"sweepRateBps"`
	SweepCapBps         int64      `json:"sweepCapBps"`
	GraceDays           int64      `json:"graceDays"`
	DecisionHash        Hash       `json:"decisionHash"`
	HumanReviewRequired bool       `json:"humanReviewRequired"`
	OfficerApproved     bool       `json:"officerApproved"`
	CreatedAt           int64      `json:"createdAt"`
	PendingFunders      int64      `json:"pendingFunders"`
	Status              Status     `json:"status"`
	AlertLevel          AlertLevel `json:"alertLevel"`
	DisbursedAt         int64      `json:"disbursedAt"`
	GraceEndsAt         int64      `json:"graceEndsAt"`
	MaturityAt          int64      `json:"maturityAt"`
	LastAccrualAt       int64      `json:"lastAccrualAt"`

	PrincipalOutstanding int64 `json:"principalOutstanding"`
	InterestAccrued      int64 `json:"interestAccrued"`
	OverduePrincipal     int64 `json:"overduePrincipal"`
	OverdueInterest      int64 `json:"overdueInterest"`
	TotalRepaid          int64 `json:"totalRepaid"`
	PrincipalRepaid      int64 `json:"principalRepaid"`
	InterestRepaid       int64 `json:"interestRepaid"`
	FloorCumulative      int64 `json:"floorCumulative"`
	RefundDue            int64 `json:"refundDue"`

	Funding  []Funding `json:"funding"`
	Payments []Payment `json:"payments"`
}

type Payoff struct {
	Principal int64 `json:"principal"`
	Interest  int64 `json:"interest"`
	Total     int64 `json:"total"`
	AsOf      int64 `json:"asOf"`
}

type AnchorRecord struct {
	Subject     string     `json:"subject"`
	Index       int64      `json:"index"`
	Kind        AnchorKind `json:"kind"`
	ContentHash Hash       `json:"contentHash"`
	Timestamp   int64      `json:"timestamp"`
	Writer      string     `json:"writer"`
	Head        Hash       `json:"head"`
	Tx          *Tx        `json:"tx,omitempty"`
}

// ChainInfo describes the backend.
type ChainInfo struct {
	Backend     string            `json:"backend"` // "fisco-bcos" | "mock"
	BlockNumber int64             `json:"blockNumber"`
	ChainTime   int64             `json:"chainTime"`
	Contracts   map[string]string `json:"contracts,omitempty"`
	Accounts    map[string]string `json:"accounts,omitempty"`
	Policy      Policy            `json:"policy"`
}
