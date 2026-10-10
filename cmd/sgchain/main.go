// Command sgchain runs the BuoyUp FISCO BCOS component.
//
//	sgchain keygen    -out keys -names admin,operator   # create role keys
//	sgchain deploy    -config config.json               # deploy contracts + grant the operator its roles
//	sgchain bootstrap -config config.json               # re-apply role grants
//	sgchain serve     -config config.json               # REST API + explorer (mock or fisco ledger)
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
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

	"buoyup/fisco/internal/api"
	"buoyup/fisco/internal/catalog"
	"buoyup/fisco/internal/config"
	"buoyup/fisco/internal/keystore"
	"buoyup/fisco/internal/ledger"
	"buoyup/fisco/internal/ledger/fisco"
	"buoyup/fisco/internal/ledger/mock"
	"buoyup/fisco/internal/payout"
	"buoyup/fisco/internal/sealing"
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
	case "decrypt":
		err = decryptCmd(args)
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
	fmt.Fprint(os.Stderr, `sgchain — BuoyUp FISCO BCOS component

usage:
  sgchain keygen    -out <dir> [-names admin,operator]
  sgchain deploy    -config <config.json>
  sgchain bootstrap -config <config.json>
  sgchain serve     -config <config.json>     (without -config: mock ledger, :12080)
  sgchain decrypt   -server <url> -id <withdrawal id or tx hash> -key <private key hex>
`)
}

func loadConfig(fs *flag.FlagSet, args []string) (config.Config, error) {
	path := fs.String("config", os.Getenv("SG_CONFIG"), "config file (JSON)")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, err
	}
	return config.Load(*path)
}

// openLedger builds the configured ledger.
func openLedger(ctx context.Context, cfg config.Config, keys fisco.Keys, log *slog.Logger) (ledger.Ledger, error) {
	acc, err := fisco.AccountsOf(keys)
	if err != nil {
		return nil, err
	}
	if cfg.Ledger == "mock" {
		m := mock.New(acc)
		if err := ledger.Bootstrap(ctx, m, acc); err != nil {
			return nil, err
		}
		log.Warn("using the IN-MEMORY MOCK ledger: state is lost on restart (set \"ledger\": \"fisco\" for a real chain)")
		return m, nil
	}
	addrs, err := cfg.LoadAddresses()
	if err != nil {
		return nil, err
	}
	return fisco.Dial(ctx, cfg.ResolvedNode(), addrs, keys)
}

func serve(args []string) error {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := loadConfig(flag.NewFlagSet("serve", flag.ExitOnError), args)
	if err != nil {
		return err
	}
	cat, err := catalog.Load(cfg.Path(cfg.CatalogDir))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	chainKeys, err := cfg.LoadKeys(cfg.Ledger == "mock")
	if err != nil {
		return err
	}
	l, err := openLedger(ctx, cfg, chainKeys, log)
	if err != nil {
		return err
	}
	defer l.Close()
	salt := cfg.RefSalt
	if salt == "" {
		salt = "bouyup-dev-salt-change-me" // frozen dev default (old spelling kept): changing it changes every account/staff reference
		log.Warn("refSalt not set: using a development salt")
	}
	var codes []string
	for _, b := range cat.Banks("") {
		codes = append(codes, b.Code)
	}
	keys, err := keystore.Open(cfg.Path(cfg.BankKeysFile), codes, time.Now())
	if err != nil {
		return err
	}
	srv := &api.Server{Ledger: l, Catalog: cat, Payer: payout.Mock{}, Keys: keys, ShowDemoKeys: cfg.DemoKeysShown(),
		ChainKeys: map[string]string{"admin": "0x" + hex.EncodeToString(chainKeys.Admin), "operator": "0x" + hex.EncodeToString(chainKeys.Operator)},
		Salt:      []byte(salt), Token: cfg.APIToken, DisableDocs: cfg.DisableDocs, DisableExplorer: cfg.DisableExplorer, Log: log}
	if srv.ShowDemoKeys {
		log.Warn("showDemoKeys is on: GET /v1/overview prints every private key (demo only)", "keysFile", keys.Path())
	}
	info, err := l.Info(ctx)
	if err != nil {
		return err
	}
	nodes, _ := l.Nodes(ctx)
	online := 0
	for _, n := range nodes {
		if n.Online {
			online++
		}
	}
	log.Info("ledger ready", "backend", info.Backend, "block", info.BlockNumber, "contracts", info.Contracts,
		"nodesOnline", fmt.Sprintf("%d/%d", online, len(nodes)), "banks", len(cat.Banks("")))

	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", cfg.Listen, "docs", !cfg.DisableDocs, "explorer", !cfg.DisableExplorer)
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
		fmt.Printf("WithdrawalLedger  %s\nAuditTrail        %s\nsaved to          %s\n", addrs.WithdrawalLedger, addrs.AuditTrail, cfg.Path(cfg.ContractsFile))
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
	if err := ledger.Bootstrap(ctx, l, acc); err != nil {
		return err
	}
	fmt.Printf("roles granted: operator=%s (OPERATOR on WithdrawalLedger, AUDIT_WRITER on AuditTrail)\n", acc.Operator)
	return nil
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "keys", "output directory")
	names := fs.String("names", "admin,operator", "comma-separated key names")
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

// decryptCmd downloads the public on-chain record from a server's explorer and decrypts it locally: the key
// never leaves this machine.
func decryptCmd(args []string) error {
	fs := flag.NewFlagSet("decrypt", flag.ExitOnError)
	server := fs.String("server", "http://localhost:12080", "BuoyUp server")
	id := fs.String("id", "", "withdrawal id or transaction hash")
	key := fs.String("key", "", "private key (hex)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *key == "" {
		return fmt.Errorf("-id and -key are required")
	}
	resp, err := http.Get(strings.TrimRight(*server, "/") + "/explorer/api/withdrawals/" + *id)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("server answered %s", resp.Status)
	}
	var body struct {
		Withdrawal struct {
			WithdrawalID string          `json:"withdrawalId"`
			Root         ledger.Hash     `json:"root"`
			Commitments  []ledger.Hash   `json:"commitments"`
			Envelope     json.RawMessage `json:"envelope"`
		} `json:"withdrawal"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	w := body.Withdrawal
	fmt.Printf("withdrawal %s: root matches commitments: %v\n", w.WithdrawalID, ledger.MerkleRoot(w.Commitments) == w.Root)
	opened, err := sealing.Open(w.WithdrawalID, w.Envelope, *key, w.Commitments)
	if err != nil {
		return err
	}
	for _, o := range opened {
		var pretty any
		_ = json.Unmarshal(o.Plaintext, &pretty)
		b, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Printf("\npiece %d (matches on-chain commitment: %v)\n%s\n", o.Index, o.CommitmentOK, b)
	}
	return nil
}
