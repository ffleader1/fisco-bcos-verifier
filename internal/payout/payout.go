// Package payout moves money to the target account of a withdrawal. BuoyUp pays first from its own funds
// and claims each bank's share back later (outside this service).
//
// Only a mock is implemented: it always succeeds, as if BuoyUp had unlimited funds. A real implementation
// would call BuoyUp's paying bank and return that bank's transfer reference.
package payout

import (
	"context"
	"strings"
	"time"
)

// Request is one payout.
type Request struct {
	WithdrawalID string
	UserID       string
	AccountNo    string
	Amount       int64 // minor units of Currency
	Currency     string
}

// Result is what the paying system returns.
type Result struct {
	Ref      string    `json:"ref"`      // transfer reference (ASCII, <= 32 chars; recorded on chain)
	Provider string    `json:"provider"` // which system paid
	PaidAt   time.Time `json:"paidAt"`
}

// Payer pays a withdrawal. It must be idempotent per WithdrawalID.
type Payer interface {
	Pay(ctx context.Context, r Request) (Result, error)
	// RefFor returns the reference Pay returns (or returned) for this withdrawal, without paying.
	RefFor(withdrawalID string) string
}

// Mock always pays. Its reference is derived from the withdrawal id, so a retry gets the same reference.
type Mock struct{ Now func() time.Time }

func (m Mock) RefFor(withdrawalID string) string {
	return "MOCKPAY-" + strings.TrimPrefix(withdrawalID, "WD-")
}

func (m Mock) Pay(_ context.Context, r Request) (Result, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	return Result{Ref: m.RefFor(r.WithdrawalID), Provider: "mock-treasury", PaidAt: now().UTC().Truncate(time.Second)}, nil
}
