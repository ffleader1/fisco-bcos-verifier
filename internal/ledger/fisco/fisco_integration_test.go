//go:build integration

// Runs the conformance suite against a live FISCO BCOS 2.x chain:
//
//	FISCO_SDK_DIR=./chain/nodes/127.0.0.1/sdk FISCO_NODE=127.0.0.1:20200 go test -tags integration ./internal/ledger/fisco/ -v
//
// Fresh contracts and fresh role keys are deployed on every run.
package fisco_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"sellergraph/fisco/internal/ledger"
	"sellergraph/fisco/internal/ledger/fisco"
	"sellergraph/fisco/internal/ledger/ledgertest"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestConformanceOnChain(t *testing.T) {
	sdk := env("FISCO_SDK_DIR", "../../../chain/nodes/127.0.0.1/sdk")
	node := fisco.NodeConfig{NodeURL: env("FISCO_NODE", "127.0.0.1:20200"), GroupID: 1, ChainID: 1,
		CAFile: filepath.Join(sdk, "ca.crt"), CertFile: filepath.Join(sdk, "sdk.crt"), KeyFile: filepath.Join(sdk, "sdk.key")}
	gen := func() []byte {
		k, err := fisco.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	operator := gen()
	keys := fisco.Keys{Admin: gen(), Operator: operator, Officer: gen(), Monitor: gen(),
		Funders: map[string][]byte{"webank": operator, "vcb": gen(), "tcb": gen()}}
	ctx := context.Background()
	addrs, err := fisco.Deploy(ctx, node, keys.Admin)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("deployed %+v", addrs)
	l, err := fisco.Dial(ctx, node, addrs, keys)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer l.Close()
	acc, _ := fisco.AccountsOf(keys)
	pol := ledger.Policy{MaxRateBps: 2400, OfficerThreshold: 50_000_000, MaxFutureSkewSec: 400 * 86400}
	if err := ledger.Bootstrap(ctx, l.Admin(), acc, pol); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	info, err := l.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("chain info: block=%d time=%d policy=%+v", info.BlockNumber, info.ChainTime, info.Policy)
	ledgertest.Run(t, ledgertest.Env{L: l, Accounts: acc, Policy: pol})
}
