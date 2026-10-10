// Package config loads the service configuration (JSON file + environment overrides).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bouyup/fisco/internal/ledger/fisco"
)

// Keys are private keys given as 0x-hex, a path to a file holding hex, or a FISCO console PEM account file.
type Keys struct {
	Admin    string `json:"admin"`    // deploys contracts and grants roles
	Operator string `json:"operator"` // the service: writes withdrawal records and audit anchors
}

type Config struct {
	ShowDemoKeys    *bool            `json:"showDemoKeys"` // print full keys in GET /v1/overview (default true; demo only)
	BankKeysFile    string           `json:"bankKeysFile"` // encryption keys: BouyUp master + one per bank (created on first start)
	Listen          string           `json:"listen"`
	APIToken        string           `json:"apiToken"`
	DisableDocs     bool             `json:"disableDocs"`     // hide the /docs/ API reference page
	DisableExplorer bool             `json:"disableExplorer"` // hide the public /explorer/ page
	CatalogDir      string           `json:"catalogDir"`      // optional folder with regions.yaml + banks/*.yaml (default: built-in catalog)
	Ledger          string           `json:"ledger"`          // "mock" | "fisco"
	RefSalt         string           `json:"refSalt"`
	Node            fisco.NodeConfig `json:"node"`
	ContractsFile   string           `json:"contractsFile"`
	Keys            Keys             `json:"keys"`

	dir string
}

// Default returns a config that runs the mock ledger.
func Default() Config {
	return Config{
		Listen: ":12080", Ledger: "mock", ContractsFile: "chain/contracts.json", BankKeysFile: "keys/bank-keys.json",
		Node: fisco.NodeConfig{NodeURL: "127.0.0.1:20200", GroupID: 1, ChainID: 1, CAFile: "chain/sdk/ca.crt", CertFile: "chain/sdk/sdk.crt", KeyFile: "chain/sdk/sdk.key"},
		dir:  ".",
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
		"SG_KEY_ADMIN": &c.Keys.Admin, "SG_KEY_OPERATOR": &c.Keys.Operator, "SG_CATALOG_DIR": &c.CatalogDir,
	} {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	if v := os.Getenv("SG_BANK_KEYS_FILE"); v != "" {
		c.BankKeysFile = v
	}
	if v := os.Getenv("SG_SHOW_DEMO_KEYS"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		c.ShowDemoKeys = &b
	}
	if v := os.Getenv("SG_NODE_PEERS"); v != "" {
		c.Node.Peers = strings.Split(v, ",")
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

// LoadKeys parses the configured keys. Missing keys are generated when generateMissing is true (mock only).
func (c Config) LoadKeys(generateMissing bool) (fisco.Keys, error) {
	var k fisco.Keys
	for _, f := range []struct {
		name, v string
		dst     *[]byte
	}{{"admin", c.Keys.Admin, &k.Admin}, {"operator", c.Keys.Operator, &k.Operator}} {
		b, err := c.parseKey(f.v)
		if err != nil {
			return k, fmt.Errorf("key %s: %w", f.name, err)
		}
		if b == nil && generateMissing {
			if b, err = fisco.GenerateKey(); err != nil {
				return k, err
			}
		}
		*f.dst = b
	}
	if k.Admin == nil || k.Operator == nil {
		return k, fmt.Errorf("keys.admin and keys.operator are required")
	}
	return k, nil
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

// DemoKeysShown reports whether the overview prints full keys (default true).
func (c Config) DemoKeysShown() bool { return c.ShowDemoKeys == nil || *c.ShowDemoKeys }
