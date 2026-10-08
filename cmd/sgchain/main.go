// Command sgchain runs the SellerGraph FISCO BCOS component.
//
//	sgchain keygen    -out keys -names admin,operator,officer,monitor   # create role keys
//	sgchain deploy    -config config.json                               # deploy contracts + grant roles + policy
//	sgchain bootstrap -config config.json                               # re-apply roles/policy (e.g. new funder)
//	sgchain serve     -config config.json                               # REST API (mock or fisco ledger)
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"sellergraph/fisco/internal/api"
	"sellergraph/fisco/internal/config"
	"sellergraph/fisco/internal/ledger"
	"sellergraph/fisco/internal/ledger/fisco"
	"sellergraph/fisco/internal/ledger/mock"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "deploy":
		err = deploy(args, true)
	case "bootstrap":
		err = deploy(args, false)
	case "keygen":
		err = keygen(args)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sgchain — SellerGraph FISCO BCOS component

usage:
  sgchain keygen    -out <dir> [-names admin,operator,officer,monitor]
  sgchain deploy    -config <config.json>
  sgchain bootstrap -config <config.json>
  sgchain serve     -config <config.json>     (without -config: mock ledger, WeBank-only, :12080)
`)
}

func loadConfig(fs *flag.FlagSet, args []string) (config.Config, error) {
	path := fs.String("config", os.Getenv("SG_CONFIG"), "config file (JSON)")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	return config.Load(*path)
}

// openLedger builds the configured ledger and the funder name -> address directory.
func openLedger(ctx context.Context, cfg config.Config, log *slog.Logger) (ledger.Ledger, map[string]string, error) {
	keys, err := cfg.LoadKeys(cfg.Ledger == "mock")
	if err != nil {
		return nil, nil, err
	}
	acc, err := fisco.AccountsOf(keys)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Ledger == "mock" {
		m := mock.New(acc)
		if err := ledger.Bootstrap(ctx, m, acc, cfg.Policy); err != nil {
			return nil, nil, err
		}
		log.Warn("using the IN-MEMORY MOCK ledger: state is lost on restart (set \"ledger\": \"fisco\" for a real chain)")
		return m, acc.Funders, nil
	}
	addrs, err := cfg.LoadAddresses()
	if err != nil {
		return nil, nil, err
	}
	l, err := fisco.Dial(ctx, cfg.ResolvedNode(), addrs, keys)
	if err != nil {
		return nil, nil, err
	}
	return l, acc.Funders, nil
}

func serve(args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := loadConfig(flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	l, funders, err := openLedger(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer l.Close()
	salt := cfg.RefSalt
	if salt == "" {
		salt = "sellergraph-dev-salt-change-me"
		log.Warn("refSalt not set: using a development salt")
	}
	if _, ok := funders[cfg.DefaultFunder]; !ok && cfg.DefaultFunder != "" {
		return fmt.Errorf("defaultFunder %q has no key under keys.funders", cfg.DefaultFunder)
	}
	srv := &api.Server{Ledger: l, Salt: []byte(salt), Funders: funders, DefaultFunder: cfg.DefaultFunder, Token: cfg.APIToken, DisableDocs: cfg.DisableDocs, Log: log}
	info, err := l.Info(ctx)
	if err != nil {
		return err
	}
	log.Info("ledger ready", "backend", info.Backend, "block", info.BlockNumber, "contracts", info.Contracts, "funders", funders)

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", cfg.Listen, "docs", !cfg.DisableDocs)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func deploy(args []string, fresh bool) error {
	cfg, err := loadConfig(flag.NewFlagSet("deploy", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	if cfg.Ledger != "fisco" {
		return fmt.Errorf("deploy/bootstrap need \"ledger\": \"fisco\"")
	}
	ctx := context.Background()
	keys, err := cfg.LoadKeys(false)
	if err != nil {
		return err
	}
	if fresh {
		addrs, err := fisco.Deploy(ctx, cfg.ResolvedNode(), keys.Admin)
		if err != nil {
			return err
		}
		if err := cfg.SaveAddresses(addrs); err != nil {
			return err
		}
		fmt.Printf("LoanLedger  %s\nAuditTrail  %s\nsaved to    %s\n", addrs.LoanLedger, addrs.AuditTrail, cfg.Path(cfg.ContractsFile))
	}
	addrs, err := cfg.LoadAddresses()
	if err != nil {
		return err
	}
	l, err := fisco.Dial(ctx, cfg.ResolvedNode(), addrs, keys)
	if err != nil {
		return err
	}
	defer l.Close()
	acc, _ := fisco.AccountsOf(keys)
	if err := ledger.Bootstrap(ctx, l.Admin(), acc, cfg.Policy); err != nil {
		return err
	}
	fmt.Printf("roles granted: operator=%s officer=%s monitor=%s funders=%v\npolicy: %+v\n", acc.Operator, acc.Officer, acc.Monitor, acc.Funders, cfg.Policy)
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "keys", "output directory")
	names := fs.String("names", "admin,operator,officer,monitor", "comma-separated key names")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	for _, n := range strings.Split(*names, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		p := filepath.Join(*out, n+".key")
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("%s exists, refusing to overwrite", p)
		}
		k, err := fisco.GenerateKey()
		if err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte("0x"+hex.EncodeToString(k)+"\n"), 0o600); err != nil {
			return err
		}
		a, _ := fisco.AddressOf(k)
		fmt.Printf("%-10s %s  -> %s\n", n, a, p)
	}
	return nil
}
