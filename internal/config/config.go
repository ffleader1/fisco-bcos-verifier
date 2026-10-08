// Package config loads the service configuration (JSON file + environment overrides).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sellergraph/fisco/internal/ledger"
	"sellergraph/fisco/internal/ledger/fisco"
)

// Keys are private keys given as 0x-hex, a path to a file holding hex, or a FISCO console PEM account file.
type Keys struct {
	Admin    string            `json:"admin"`
	Operator string            `json:"operator"`
	Officer  string            `json:"officer"`
	Monitor  string            `json:"monitor"`
	Funders  map[string]string `json:"funders"` // name -> key; a funder may reuse the operator key (WeBank-only mode)
}

type Config struct {
	Listen        string           `json:"listen"`
	APIToken      string           `json:"apiToken"`
	DisableDocs   bool             `json:"disableDocs"` // hide the /docs/ API reference page
	Ledger        string           `json:"ledger"` // "mock" | "fisco"
	RefSalt       string           `json:"refSalt"`
	DefaultFunder string           `json:"defaultFunder"`
	Node          fisco.NodeConfig `json:"node"`
	ContractsFile string           `json:"contractsFile"`
	Keys          Keys             `json:"keys"`
	Policy        ledger.Policy    `json:"policy"`

	dir string
}

// Default returns a config that runs the mock ledger in WeBank-only mode.
func Default() Config {
	return Config{
		Listen: ":12080", Ledger: "mock", DefaultFunder: "webank", ContractsFile: "chain/contracts.json",
		Node:   fisco.NodeConfig{NodeURL: "127.0.0.1:20200", GroupID: 1, ChainID: 1, CAFile: "chain/sdk/ca.crt", CertFile: "chain/sdk/sdk.crt", KeyFile: "chain/sdk/sdk.key"},
		Policy: ledger.Policy{MaxRateBps: 2400, OfficerThreshold: 50_000_000, MaxFutureSkewSec: 300},
		dir:    ".",
	}
}

// Load reads path (optional; "" = defaults) and applies SG_* environment overrides.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
		c.dir = filepath.Dir(path)
	}
	for env, dst := range map[string]*string{
		"SG_LISTEN": &c.Listen, "SG_API_TOKEN": &c.APIToken, "SG_LEDGER": &c.Ledger, "SG_REF_SALT": &c.RefSalt,
		"SG_NODE_URL": &c.Node.NodeURL, "SG_CONTRACTS_FILE": &c.ContractsFile,
		"SG_KEY_ADMIN": &c.Keys.Admin, "SG_KEY_OPERATOR": &c.Keys.Operator, "SG_KEY_OFFICER": &c.Keys.Officer, "SG_KEY_MONITOR": &c.Keys.Monitor,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	c.Ledger = strings.ToLower(c.Ledger)
	if c.Ledger != "mock" && c.Ledger != "fisco" {
		return c, fmt.Errorf("ledger must be mock or fisco, got %q", c.Ledger)
	}
	if c.Ledger == "fisco" && len(c.RefSalt) < 16 {
		return c, fmt.Errorf("refSalt (or SG_REF_SALT) of at least 16 chars is required with the fisco ledger")
	}
	return c, nil
}

// Path resolves p relative to the config file directory.
func (c Config) Path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

// ResolvedNode returns the node config with certificate paths resolved.
func (c Config) ResolvedNode() fisco.NodeConfig {
	n := c.Node
	n.CAFile, n.CertFile, n.KeyFile = c.Path(n.CAFile), c.Path(n.CertFile), c.Path(n.KeyFile)
	return n
}

func (c Config) parseKey(v string) ([]byte, error) {
	if v == "" {
		return nil, nil
	}
	if !strings.HasPrefix(v, "0x") && !isHex64(v) {
		v = c.Path(v)
	}
	return fisco.ParseKey(v)
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// LoadKeys parses every configured key. Missing keys are generated when generateMissing is true (mock only).
func (c Config) LoadKeys(generateMissing bool) (fisco.Keys, error) {
	var k fisco.Keys
	var err error
	one := func(name, v string) []byte {
		if err != nil {
			return nil
		}
		b, e := c.parseKey(v)
		if e != nil {
			err = fmt.Errorf("key %s: %w", name, e)
			return nil
		}
		if b == nil && generateMissing {
			b, err = fisco.GenerateKey()
		}
		return b
	}
	k.Admin = one("admin", c.Keys.Admin)
	k.Operator = one("operator", c.Keys.Operator)
	k.Officer = one("officer", c.Keys.Officer)
	k.Monitor = one("monitor", c.Keys.Monitor)
	k.Funders = map[string][]byte{}
	for n, v := range c.Keys.Funders {
		if v == "operator" { // shorthand: funder signs with the operator key (WeBank-only mode)
			k.Funders[n] = k.Operator
			continue
		}
		k.Funders[n] = one("funder "+n, v)
	}
	if len(k.Funders) == 0 && c.DefaultFunder != "" {
		k.Funders[c.DefaultFunder] = k.Operator // WeBank-only default
	}
	if err == nil && (k.Admin == nil || k.Operator == nil || k.Officer == nil || k.Monitor == nil) {
		err = fmt.Errorf("keys.admin, keys.operator, keys.officer and keys.monitor are required")
	}
	return k, err
}

// LoadAddresses reads the contracts file written by `sgchain deploy`.
func (c Config) LoadAddresses() (fisco.Addresses, error) {
	var a fisco.Addresses
	raw, err := os.ReadFile(c.Path(c.ContractsFile))
	if err != nil {
		return a, fmt.Errorf("read contracts file (run `sgchain deploy` first): %w", err)
	}
	return a, json.Unmarshal(raw, &a)
}

// SaveAddresses writes the contracts file.
func (c Config) SaveAddresses(a fisco.Addresses) error {
	p := c.Path(c.ContractsFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(a, "", "  ")
	return os.WriteFile(p, raw, 0o644)
}
