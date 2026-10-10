//go:build integration

// Runs the conformance suite against a live FISCO BCOS 2.x chain:
//
//	FISCO_SDK_DIR=./chain/sdk FISCO_NODE=127.0.0.1:20200 FISCO_PEERS=127.0.0.1:20201,127.0.0.1:20202,127.0.0.1:20203 \
//	  go test -tags integration ./internal/ledger/fisco/ -v
//
// Fresh contracts and fresh role keys are deployed on every run.
package fisco_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bouyup/fisco/internal/ledger"
	"bouyup/fisco/internal/ledger/fisco"
	"bouyup/fisco/internal/ledger/ledgertest"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestConformanceOnChain(t *testing.T) {
	sdk := env("FISCO_SDK_DIR", "../../../chain/sdk")
	node := fisco.NodeConfig{NodeURL: env("FISCO_NODE", "127.0.0.1:20200"), GroupID: 1, ChainID: 1,
		Peers:  strings.Split(env("FISCO_PEERS", "127.0.0.1:20201,127.0.0.1:20202,127.0.0.1:20203"), ","),
		CAFile: filepath.Join(sdk, "ca.crt"), CertFile: filepath.Join(sdk, "sdk.crt"), KeyFile: filepath.Join(sdk, "sdk.key")}
	gen := func() []byte {
		k, err := fisco.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	keys := fisco.Keys{Admin: gen(), Operator: gen()}
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
	if err := ledger.Bootstrap(ctx, l, acc); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	nodes, _ := l.Nodes(ctx)
	t.Logf("nodes: %+v", nodes)
	ledgertest.Run(t, ledgertest.Env{L: l, Accounts: acc})
}
