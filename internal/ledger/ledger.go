package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// Ledger is the BuoyUp on-chain system of record. Write methods map 1:1 to a WithdrawalLedger / AuditTrail
// contract function and are signed by the operator (service) key. The explorer reads cover blocks,
// transactions and the state of every configured node.
type Ledger interface {
	Info(ctx context.Context) (ChainInfo, error)
	// Now returns chain time in unix seconds (latest block timestamp / mock clock).
	Now(ctx context.Context) (int64, error)

	// withdrawals
	RecordWithdrawal(ctx context.Context, req WithdrawalRequest) (Withdrawal, Tx, error)
	GetWithdrawal(ctx context.Context, id string) (Withdrawal, error)
	// ListWithdrawals returns the newest first.
	ListWithdrawals(ctx context.Context, offset, limit int) (items []Withdrawal, total int, err error)

	// audit trail
	Anchor(ctx context.Context, req AnchorRequest) (AnchorRecord, error)
	ListAnchors(ctx context.Context, subject string) ([]AnchorRecord, error)
	LocateAnchor(ctx context.Context, contentHash Hash) (AnchorRecord, bool, error)

	// explorer
	Nodes(ctx context.Context) ([]NodeInfo, error)
	LatestBlocks(ctx context.Context, limit int) ([]Block, error)
	GetBlock(ctx context.Context, number int64) (Block, error)
	GetTransaction(ctx context.Context, hash string) (TxDetail, error)

	Close()
}

// CommitWatcher is implemented by ledgers that hear from every node when it writes a block (FISCO BCOS block
// notifications, one subscription per node). Used by the overview's step-by-step demo.
type CommitWatcher interface {
	// BlockCommits reports when each configured node said it had written block n, waiting up to wait for the nodes
	// still missing. Nodes that did not report in time come back with Seen=false.
	BlockCommits(ctx context.Context, n int64, wait time.Duration) []NodeCommit
}

// NodeCommit is one node's "I wrote block N" notice, timed when this service received it.
type NodeCommit struct {
	Node string    `json:"node"`
	At   time.Time `json:"-"`
	Seen bool      `json:"seen"`
}

// Admin covers deployment-time configuration (role grants). Used by bootstrap code only.
type Admin interface {
	GrantRole(ctx context.Context, account string, role uint64) (Tx, error)
	HasRole(ctx context.Context, account string, role uint64) (bool, error)
}

// Accounts are the 0x addresses of the signing identities.
type Accounts struct {
	Admin    string `json:"admin"`
	Operator string `json:"operator"`
}

// Bootstrap grants the operator its roles. Idempotent (grantRole ORs bits).
func Bootstrap(ctx context.Context, a Admin, acc Accounts) error {
	if acc.Operator == "" {
		return fmt.Errorf("operator account is required")
	}
	if _, err := a.GrantRole(ctx, acc.Operator, RoleOperator|RoleAuditWriter); err != nil {
		return fmt.Errorf("grant roles to operator %s: %w", acc.Operator, err)
	}
	return nil
}

// ---------------------------------------------------------------- identifiers & codes

var idRe = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,32}$`)
var codeRe = regexp.MustCompile(`^[A-Z0-9_]{1,32}$`)

// IDToBytes32 encodes a human-readable identifier (withdrawal id, audit subject) left-aligned in a bytes32, so it stays
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

// CodeToBytes32 encodes an UPPER_SNAKE code as bytes32 ASCII.
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

// FromRevert converts a contract revert reason ("E_NOT_FOUND: withdrawal") to an *Error.
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

// TextToBytes32 encodes printable ASCII text of at most 32 bytes (codes, versions, references) left-aligned.
func TextToBytes32(field, s string) (Hash, error) {
	var h Hash
	if s == "" || len(s) > 32 {
		return h, Errorf(KindInvalid, "%s must be 1-32 characters", field)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return h, Errorf(KindInvalid, "%s must be printable ASCII", field)
		}
	}
	copy(h[:], s)
	return h, nil
}

// MerkleRoot folds commitments into one root exactly like WithdrawalLedger._merkle: pairs are hashed with
// SHA-256(left || right); an odd node at the end of a level moves up unchanged. One leaf is its own root.
func MerkleRoot(leaves []Hash) Hash {
	if len(leaves) == 0 {
		return ZeroHash
	}
	level := append([]Hash(nil), leaves...)
	for len(level) > 1 {
		next := make([]Hash, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			if i+1 == len(level) {
				next = append(next, level[i])
				continue
			}
			next = append(next, Hash(sha256.Sum256(append(level[i][:], level[i+1][:]...))))
		}
		level = next
	}
	return level[0]
}
