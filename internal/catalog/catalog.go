// Package catalog holds the demo bank catalog: regions (countries and provinces), the banks BuoyUp works
// with, each bank's numeric limits, its written credit policy (read by Agent 3) and its service agreement with
// BuoyUp. The data lives in YAML files: built into the binary from data/, or loaded from a folder
// (config "catalogDir") so the team can edit policies without rebuilding.
//
// Everything here is MOCK data for the demo. It is not the real policy, terms or limits of any bank.
package catalog

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"buoyup/fisco/internal/hashing"
	"buoyup/fisco/internal/ledger"
)

//go:embed data/regions.yaml data/banks/*.yaml
var builtin embed.FS

// Province is a province or province-level city.
type Province struct {
	Code   string   `yaml:"code" json:"code"`
	Name   string   `yaml:"name" json:"name"`
	Cities []string `yaml:"cities,omitempty" json:"cities,omitempty"`
	Note   string   `yaml:"note,omitempty" json:"note,omitempty"`
}

// Region is a country: it fixes the currency of a withdrawal and which banks can take part in it.
type Region struct {
	Code      string     `yaml:"code" json:"code"`
	Name      string     `yaml:"name" json:"name"`
	Currency  string     `yaml:"currency" json:"currency"`
	MinorUnit int        `yaml:"minorUnit" json:"minorUnit"` // decimals of the currency: amounts are integers in 10^-minorUnit units
	Note      string     `yaml:"note,omitempty" json:"note,omitempty"`
	Provinces []Province `yaml:"provinces" json:"provinces"`
}

// Limits are the numeric parameters of a bank, in the region's currency minor units and percent.
// They are information for the agents; the withdrawal endpoint does not enforce them.
type Limits struct {
	MinWithdrawal        int64   `yaml:"minWithdrawal" json:"minWithdrawal"`
	MaxWithdrawal        int64   `yaml:"maxWithdrawal" json:"maxWithdrawal"`
	MaxSharePercent      float64 `yaml:"maxSharePercent" json:"maxSharePercent"` // max part of one withdrawal the bank takes
	DailyCap             int64   `yaml:"dailyCap" json:"dailyCap"`
	ReimbursementDays    int     `yaml:"reimbursementDays" json:"reimbursementDays"` // business days to pay BuoyUp back
	FeePercent           float64 `yaml:"feePercent" json:"feePercent"`               // BuoyUp's fee on the bank's share
	MaxLoanAmount        int64   `yaml:"maxLoanAmount" json:"maxLoanAmount"`
	MinAnnualRatePercent float64 `yaml:"minAnnualRatePercent" json:"minAnnualRatePercent"`
	MaxAnnualRatePercent float64 `yaml:"maxAnnualRatePercent" json:"maxAnnualRatePercent"`
	MaxTenorDays         int     `yaml:"maxTenorDays" json:"maxTenorDays"`
	MinStoreAgeMonths    int     `yaml:"minStoreAgeMonths" json:"minStoreAgeMonths"`
	MinStoreRating       float64 `yaml:"minStoreRating" json:"minStoreRating"`
}

// Preference is one written preference: what, and why the bank feels that way.
type Preference struct {
	Code string `yaml:"code,omitempty" json:"code,omitempty"` // province code for regional preferences
	Name string `yaml:"name" json:"name"`
	Why  string `yaml:"why" json:"why"`
}

// PreferenceSet groups preferences by direction.
type PreferenceSet struct {
	Prefer   []Preference `yaml:"prefer" json:"prefer"`
	Cautious []Preference `yaml:"cautious" json:"cautious"`
	Avoid    []Preference `yaml:"avoid" json:"avoid"`
	Note     string       `yaml:"note,omitempty" json:"note,omitempty"`
}

// Policy is the bank's credit policy written for Agent 3: judgement, not thresholds.
type Policy struct {
	Version         string        `yaml:"version" json:"version"`
	UpdatedAt       string        `yaml:"updatedAt" json:"updatedAt"`
	Summary         string        `yaml:"summary" json:"summary"`
	RiskAppetite    string        `yaml:"riskAppetite" json:"riskAppetite"`
	Sectors         PreferenceSet `yaml:"sectors" json:"sectors"`
	Regions         PreferenceSet `yaml:"regions" json:"regions"`
	Customers       PreferenceSet `yaml:"customers" json:"customers"`
	UseOfFunds      []string      `yaml:"useOfFunds" json:"useOfFunds"`
	PositiveSignals []string      `yaml:"positiveSignals" json:"positiveSignals"`
	RedFlags        []string      `yaml:"redFlags" json:"redFlags"`
	EscalateWhen    []string      `yaml:"escalateWhen" json:"escalateWhen"`
	DealBreakers    []string      `yaml:"dealBreakers" json:"dealBreakers"`
	ReviewerNotes   string        `yaml:"reviewerNotes" json:"reviewerNotes"`
	Hash            ledger.Hash   `yaml:"-" json:"hash"` // SHA-256 of the canonical JSON of the policy (without this field)
}

// Contract is the (mock) service agreement between the bank and BuoyUp.
type Contract struct {
	ContractNo         string      `yaml:"contractNo" json:"contractNo"`
	Title              string      `yaml:"title" json:"title"`
	Parties            []string    `yaml:"parties" json:"parties"`
	Status             string      `yaml:"status" json:"status"` // active | suspended | expired
	EffectiveFrom      string      `yaml:"effectiveFrom" json:"effectiveFrom"`
	ExpiresOn          string      `yaml:"expiresOn" json:"expiresOn"`
	CreditLine         int64       `yaml:"creditLine" json:"creditLine"` // max outstanding amount the bank owes BuoyUp
	ReimbursementTerms string      `yaml:"reimbursementTerms" json:"reimbursementTerms"`
	FeeTerms           string      `yaml:"feeTerms" json:"feeTerms"`
	EvidenceTerms      string      `yaml:"evidenceTerms" json:"evidenceTerms"`
	DataTerms          string      `yaml:"dataTerms" json:"dataTerms"`
	GoverningLaw       string      `yaml:"governingLaw" json:"governingLaw"`
	DisputeResolution  string      `yaml:"disputeResolution" json:"disputeResolution"`
	Termination        string      `yaml:"termination" json:"termination"`
	KeyClauses         []string    `yaml:"keyClauses" json:"keyClauses"`
	Hash               ledger.Hash `yaml:"-" json:"hash"`
}

// Bank is one bank profile.
type Bank struct {
	Code         string   `yaml:"code" json:"code"`
	Name         string   `yaml:"name" json:"name"`
	LocalName    string   `yaml:"localName" json:"localName"`
	Region       string   `yaml:"region" json:"region"`
	Currency     string   `yaml:"-" json:"currency"`
	Headquarters string   `yaml:"headquarters" json:"headquarters"` // province code
	Type         string   `yaml:"type" json:"type"`
	Active       bool     `yaml:"active" json:"active"`
	Mock         bool     `yaml:"-" json:"mock"`
	Disclaimer   string   `yaml:"-" json:"disclaimer"`
	Profile      string   `yaml:"profile" json:"profile"`
	Limits       Limits   `yaml:"limits" json:"limits"`
	Policy       Policy   `yaml:"policy" json:"policy"`
	Contract     Contract `yaml:"contract" json:"contract"`
}

// Disclaimer is attached to every bank profile.
const Disclaimer = "Demo data for BuoyUp. Not the real policy, terms or limits of this bank."

// Catalog is the loaded data.
type Catalog struct {
	Regions []Region
	banks   map[string]*Bank
	order   []string
}

// Load reads the catalog from dir (regions.yaml + banks/*.yaml), or the built-in data when dir is "".
func Load(dir string) (*Catalog, error) {
	var fsys fs.FS
	root := "data"
	if dir != "" {
		fsys, root = os.DirFS(dir), "."
	} else {
		fsys = builtin
	}
	raw, err := fs.ReadFile(fsys, path.Join(root, "regions.yaml"))
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	var rf struct {
		Regions []Region `yaml:"regions"`
	}
	if err := strictYAML(raw, &rf); err != nil {
		return nil, fmt.Errorf("catalog regions.yaml: %w", err)
	}
	c := &Catalog{Regions: rf.Regions, banks: map[string]*Bank{}}
	files, err := fs.Glob(fsys, path.Join(root, "banks", "*.yaml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		var b Bank
		if err := strictYAML(raw, &b); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", f, err)
		}
		if err := c.add(&b); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", f, err)
		}
	}
	if len(c.banks) == 0 {
		return nil, fmt.Errorf("catalog: no banks")
	}
	return c, nil
}

func strictYAML(raw []byte, v any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	return dec.Decode(v)
}

func (c *Catalog) add(b *Bank) error {
	b.Code = strings.ToUpper(strings.TrimSpace(b.Code))
	if _, err := ledger.TextToBytes32("bank code", b.Code); err != nil || strings.ContainsAny(b.Code, " ") {
		return fmt.Errorf("bank code %q must be 1-32 characters without spaces", b.Code)
	}
	if _, dup := c.banks[b.Code]; dup {
		return fmt.Errorf("duplicate bank code %s", b.Code)
	}
	reg, ok := c.Region(b.Region)
	if !ok {
		return fmt.Errorf("bank %s: unknown region %q", b.Code, b.Region)
	}
	if b.Headquarters != "" {
		if _, ok := reg.Province(b.Headquarters); !ok {
			return fmt.Errorf("bank %s: headquarters %q is not a province of %s", b.Code, b.Headquarters, reg.Code)
		}
	}
	for _, p := range [][]Preference{b.Policy.Regions.Prefer, b.Policy.Regions.Cautious, b.Policy.Regions.Avoid} {
		for _, pr := range p {
			if pr.Code != "" {
				if _, ok := reg.Province(pr.Code); !ok {
					return fmt.Errorf("bank %s: regional preference %q is not a province of %s", b.Code, pr.Code, reg.Code)
				}
			}
		}
	}
	b.Currency, b.Mock, b.Disclaimer = reg.Currency, true, Disclaimer
	var err error
	if b.Policy.Hash, err = hashOf(b.Policy); err != nil {
		return err
	}
	if b.Contract.Hash, err = hashOf(b.Contract); err != nil {
		return err
	}
	c.banks[b.Code] = b
	c.order = append(c.order, b.Code)
	return nil
}

// hashOf hashes v canonically with its "hash" field left out.
func hashOf(v any) (ledger.Hash, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return ledger.Hash{}, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ledger.Hash{}, err
	}
	delete(m, "hash")
	return hashing.ContentOf(m)
}

// Region returns a region by code (case-insensitive).
func (c *Catalog) Region(code string) (Region, bool) {
	for _, r := range c.Regions {
		if strings.EqualFold(r.Code, code) {
			return r, true
		}
	}
	return Region{}, false
}

// Province returns a province of the region by code (case-insensitive).
func (r Region) Province(code string) (Province, bool) {
	for _, p := range r.Provinces {
		if strings.EqualFold(p.Code, code) {
			return p, true
		}
	}
	return Province{}, false
}

// Bank returns a bank by code (case-insensitive).
func (c *Catalog) Bank(code string) (*Bank, bool) {
	b, ok := c.banks[strings.ToUpper(strings.TrimSpace(code))]
	return b, ok
}

// Banks returns the banks of a region ("" = all), in file order.
func (c *Catalog) Banks(region string) []*Bank {
	out := []*Bank{}
	for _, code := range c.order {
		b := c.banks[code]
		if region == "" || strings.EqualFold(b.Region, region) {
			out = append(out, b)
		}
	}
	return out
}

// ContractStatus is the contract status as of t: "active" only when the contract says active and t is inside
// [effectiveFrom, expiresOn]; "not_started" / "expired" otherwise.
func (b *Bank) ContractStatus(t time.Time) string {
	st := strings.ToLower(b.Contract.Status)
	if st != "active" {
		return st
	}
	day := t.UTC().Format("2006-01-02")
	if b.Contract.EffectiveFrom != "" && day < b.Contract.EffectiveFrom {
		return "not_started"
	}
	if b.Contract.ExpiresOn != "" && day > b.Contract.ExpiresOn {
		return "expired"
	}
	return "active"
}
