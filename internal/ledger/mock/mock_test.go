package mock_test

import (
	"context"
	"testing"

	"sellergraph/fisco/internal/ledger"
	"sellergraph/fisco/internal/ledger/ledgertest"
	"sellergraph/fisco/internal/ledger/mock"
)

func TestConformance(t *testing.T) {
	now := int64(1_790_000_000)
	acc := ledger.Accounts{
		Admin: "0x00000000000000000000000000000000000000a1", Operator: "0x00000000000000000000000000000000000000b2",
		Officer: "0x00000000000000000000000000000000000000c3", Monitor: "0x00000000000000000000000000000000000000d4",
		Funders: map[string]string{
			"webank": "0x00000000000000000000000000000000000000b2", // same key as operator -> auto-confirm
			"vcb":    "0x00000000000000000000000000000000000000e5",
			"tcb":    "0x00000000000000000000000000000000000000f6",
		},
	}
	m := mock.New(acc, mock.WithClock(func() int64 { return now }))
	pol := ledger.Policy{MaxRateBps: 2400, OfficerThreshold: 50_000_000, MaxFutureSkewSec: 400 * 86400}
	if err := ledger.Bootstrap(context.Background(), m, acc, pol); err != nil {
		t.Fatal(err)
	}
	ledgertest.Run(t, ledgertest.Env{L: m, Accounts: acc, Policy: pol})
}

func TestUnauthorizedWithoutBootstrap(t *testing.T) {
	m := mock.New(ledger.Accounts{Admin: "0x01", Operator: "0x02"})
	_, err := m.CreateLoan(context.Background(), ledger.CreateLoan{LoanID: "X"})
	if ledger.KindOf(err) != ledger.KindUnauthorized {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}
