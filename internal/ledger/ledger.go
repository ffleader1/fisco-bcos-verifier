package ledger

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

// Ledger is the SellerGraph on-chain system of record. Every method maps 1:1 to a LoanLedger / AuditTrail
// contract function; the implementation picks the signing account that holds the required role
// (operator, officer, monitor, funder, audit writer).
type Ledger interface {
	Info(ctx context.Context) (ChainInfo, error)
	// Now returns chain time in unix seconds (latest block timestamp / mock clock).
	Now(ctx context.Context) (int64, error)

	// origination
	CreateLoan(ctx context.Context, req CreateLoan) (Tx, error)
	ConfirmFunding(ctx context.Context, loanID, funder string) (Tx, error)
	OfficerDecision(ctx context.Context, loanID string, approved bool, reviewHash Hash) (Tx, error)
	CancelLoan(ctx context.Context, loanID string, reasonHash Hash) (Tx, error)

	// disbursement & repayment
	RecordDisbursement(ctx context.Context, req Disbursement) (Tx, error)
	RecordRepayment(ctx context.Context, req Repayment) (RepaymentResult, error)
	CheckpointFloor(ctx context.Context, req FloorCheckpoint) (FloorResult, error)
	MarkMatured(ctx context.Context, loanID string, asOf int64) (Tx, error)
	Accelerate(ctx context.Context, loanID string, reasonHash Hash, asOf int64) (Tx, error)
	MarkDefault(ctx context.Context, loanID string, reasonHash Hash) (Tx, error)
	Restructure(ctx context.Context, req Restructure) (Tx, error)

	// monitoring
	RaiseAlert(ctx context.Context, loanID string, level AlertLevel, code string, evidence Hash) (Tx, error)
	RecordCovenantEvent(ctx context.Context, loanID, code string, breached bool, evidence Hash) (Tx, error)
	RecordDelivery(ctx context.Context, loanID string, line int, waybillHash Hash, shippedAt int64) (DeliveryResult, error)
	FlagMissingDelivery(ctx context.Context, loanID string, line int, asOf int64) (Tx, error)

	// reads
	GetLoan(ctx context.Context, loanID string) (Loan, error)
	ListLoanIDs(ctx context.Context, offset, limit int) (ids []string, total int, err error)
	PreviewPayoff(ctx context.Context, loanID string, asOf int64) (Payoff, error)

	// audit trail
	Anchor(ctx context.Context, req AnchorRequest) (AnchorRecord, error)
	ListAnchors(ctx context.Context, subject string) ([]AnchorRecord, error)
	LocateAnchor(ctx context.Context, contentHash Hash) (AnchorRecord, bool, error)

	Close()
}

// Admin covers deployment-time configuration (role grants, policy). Used by bootstrap code only.
type Admin interface {
	GrantRole(ctx context.Context, account string, role uint64) (Tx, error)
	SetPolicy(ctx context.Context, p Policy) (Tx, error)
	GetPolicy(ctx context.Context) (Policy, error)
	HasRole(ctx context.Context, account string, role uint64) (bool, error)
}

// Accounts are the 0x addresses of the signing identities.
type Accounts struct {
	Admin    string            `json:"admin"`
	Operator string            `json:"operator"`
	Officer  string            `json:"officer"`
	Monitor  string            `json:"monitor"`
	Funders  map[string]string `json:"funders"` // name -> address
}

// Bootstrap grants the standard roles and sets policy. Idempotent.
func Bootstrap(ctx context.Context, a Admin, acc Accounts, p Policy) error {
	grants := []struct {
		addr string
		role uint64
	}{
		{acc.Operator, RoleOperator | RoleAuditWriter},
		{acc.Officer, RoleOfficer | RoleAuditWriter},
		{acc.Monitor, RoleMonitor | RoleAuditWriter},
	}
	for _, f := range acc.Funders {
		grants = append(grants, struct {
			addr string
			role uint64
		}{f, RoleFunder})
	}
	for _, g := range grants {
		if g.addr == "" {
			continue
		}
		// grantRole ORs bits, so re-running bootstrap is harmless.
		if _, err := a.GrantRole(ctx, g.addr, g.role); err != nil {
			return fmt.Errorf("grant role %d to %s: %w", g.role, g.addr, err)
		}
	}
	if p.MaxRateBps > 0 {
		if _, err := a.SetPolicy(ctx, p); err != nil {
			return fmt.Errorf("set policy: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- identifiers & codes

var idRe = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,32}$`)
var codeRe = regexp.MustCompile(`^[A-Z0-9_]{1,32}$`)

// IDToBytes32 encodes a human-readable identifier (loan id, subject) left-aligned in a bytes32, so it stays
// readable in chain explorers and is reversible. Max 32 chars of [A-Za-z0-9._:-].
func IDToBytes32(id string) (Hash, error) {
	var h Hash
	if !idRe.MatchString(id) {
		return h, Errorf(KindInvalid, "id %q must be 1-32 chars of [A-Za-z0-9._:-]", id)
	}
	copy(h[:], id)
	return h, nil
}

// Bytes32ToID reverses IDToBytes32.
func Bytes32ToID(h Hash) string { return strings.TrimRight(string(h[:]), "\x00") }

// CodeToBytes32 encodes an UPPER_SNAKE code (alert / covenant code) as bytes32 ASCII.
func CodeToBytes32(code string) (Hash, error) {
	var h Hash
	if !codeRe.MatchString(code) {
		return h, Errorf(KindInvalid, "code %q must be 1-32 chars of [A-Z0-9_]", code)
	}
	copy(h[:], code)
	return h, nil
}

// ---------------------------------------------------------------- errors

// ErrorKind classifies ledger errors (taken from the contract revert prefix).
type ErrorKind string

const (
	KindUnauthorized ErrorKind = "unauthorized"
	KindNotFound     ErrorKind = "not_found"
	KindBadStatus    ErrorKind = "bad_status"
	KindInvalid      ErrorKind = "invalid"
	KindDuplicate    ErrorKind = "duplicate"
	KindChain        ErrorKind = "chain" // transport / node / unexpected
)

// Error is returned by Ledger implementations.
type Error struct {
	Kind    ErrorKind
	Message string
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Message }

func Errorf(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// FromRevert converts a contract revert reason ("E_BAD_STATUS: loan not active") to an *Error.
func FromRevert(reason string) *Error {
	prefixes := map[string]ErrorKind{
		"E_UNAUTHORIZED": KindUnauthorized,
		"E_NOT_FOUND":    KindNotFound,
		"E_BAD_STATUS":   KindBadStatus,
		"E_INVALID":      KindInvalid,
		"E_DUPLICATE":    KindDuplicate,
	}
	for p, k := range prefixes {
		if strings.HasPrefix(reason, p) {
			msg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(reason, p), ":"))
			return &Error{Kind: k, Message: msg}
		}
	}
	return &Error{Kind: KindChain, Message: reason}
}

// KindOf returns the ErrorKind of err (KindChain for foreign errors).
func KindOf(err error) ErrorKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindChain
}

// ChainHead computes the AuditTrail hash-chain link exactly like the contract:
// keccak256(abi.encodePacked(prevHead, uint8 kind, contentHash, uint64 timestamp)) — non-SM (ECDSA/keccak) chain.
func ChainHead(prev Hash, kind AnchorKind, content Hash, ts int64) Hash {
	var tsb [8]byte
	binary.BigEndian.PutUint64(tsb[:], uint64(ts))
	return Hash(crypto.Keccak256Hash(prev[:], []byte{byte(kind)}, content[:], tsb[:]))
}
