package mock_test

import (
	"context"
	"testing"

	"bouyup/fisco/internal/ledger"
	"bouyup/fisco/internal/ledger/ledgertest"
	"bouyup/fisco/internal/ledger/mock"
)

func TestConformance(t *testing.T) {
	now := int64(1_790_000_000)
	acc := ledger.Accounts{Admin: "0x00000000000000000000000000000000000000a1", Operator: "0x00000000000000000000000000000000000000b2"}
	m := mock.New(acc, mock.WithClock(func() int64 { return now }))
	if err := ledger.Bootstrap(context.Background(), m, acc); err != nil {
		t.Fatal(err)
	}
	ledgertest.Run(t, ledgertest.Env{L: m, Accounts: acc})
}

func TestUnauthorizedWithoutBootstrap(t *testing.T) {
	m := mock.New(ledger.Accounts{Admin: "0x01", Operator: "0x02"})
	_, _, err := m.RecordWithdrawal(context.Background(), ledger.WithdrawalRequest{ID: "X"})
	if ledger.KindOf(err) != ledger.KindUnauthorized {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}
