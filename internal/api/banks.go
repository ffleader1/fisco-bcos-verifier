package api

import (
	"net/http"
	"strings"

	"bouyup/fisco/internal/catalog"
)

type bankSummary struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	LocalName      string `json:"localName"`
	Region         string `json:"region"`
	Currency       string `json:"currency"`
	Headquarters   string `json:"headquarters"`
	Type           string `json:"type"`
	Active         bool   `json:"active"`
	ContractStatus string `json:"contractStatus"`
	PolicyVersion  string `json:"policyVersion"`
	Mock           bool   `json:"mock"`
}

type bankFull struct {
	*catalog.Bank
	ContractStatus string       `json:"contractStatus"`
	EncryptionKey  *bankKeyView `json:"encryptionKey,omitempty"`
}

// bankKeyView is the bank's current public key: new withdrawals are locked to it.
type bankKeyView struct {
	PublicKey  string `json:"publicKey"`
	ActiveFrom string `json:"activeFrom"`
	Algorithm  string `json:"algorithm"`
}

func (s *Server) bankKey(code string) *bankKeyView {
	if s.Keys == nil {
		return nil
	}
	k, ok := s.Keys.Bank(code)
	if !ok {
		return nil
	}
	return &bankKeyView{PublicKey: k.PublicKey, ActiveFrom: k.ActiveFrom, Algorithm: "X25519"}
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"regions": s.Catalog.Regions})
}

func (s *Server) listBanks(w http.ResponseWriter, r *http.Request) {
	region := r.URL.Query().Get("region")
	if region != "" {
		if _, ok := s.Catalog.Region(region); !ok {
			s.fail(w, errf(400, "unknown_region", "region %q is not supported (use one of %s)", region, s.regionCodes()))
			return
		}
	}
	out := []bankSummary{}
	for _, b := range s.Catalog.Banks(region) {
		out = append(out, bankSummary{Code: b.Code, Name: b.Name, LocalName: b.LocalName, Region: b.Region, Currency: b.Currency,
			Headquarters: b.Headquarters, Type: b.Type, Active: b.Active, ContractStatus: b.ContractStatus(s.now()),
			PolicyVersion: b.Policy.Version, Mock: b.Mock})
	}
	writeJSON(w, 200, map[string]any{"banks": out, "disclaimer": catalog.Disclaimer})
}

func (s *Server) bank(w http.ResponseWriter, r *http.Request) (*catalog.Bank, bool) {
	b, ok := s.Catalog.Bank(r.PathValue("code"))
	if !ok {
		s.fail(w, errf(404, "unknown_bank", "unknown bank %q (see GET /v1/banks)", strings.ToUpper(r.PathValue("code"))))
	}
	return b, ok
}

func (s *Server) getBank(w http.ResponseWriter, r *http.Request) {
	if b, ok := s.bank(w, r); ok {
		writeJSON(w, 200, bankFull{Bank: b, ContractStatus: b.ContractStatus(s.now()), EncryptionKey: s.bankKey(b.Code)})
	}
}

func (s *Server) getBankPolicy(w http.ResponseWriter, r *http.Request) {
	if b, ok := s.bank(w, r); ok {
		writeJSON(w, 200, map[string]any{"bank": b.Code, "name": b.Name, "region": b.Region, "mock": b.Mock, "disclaimer": b.Disclaimer,
			"policy": b.Policy})
	}
}

func (s *Server) getBankContract(w http.ResponseWriter, r *http.Request) {
	if b, ok := s.bank(w, r); ok {
		writeJSON(w, 200, map[string]any{"bank": b.Code, "name": b.Name, "region": b.Region, "mock": b.Mock, "disclaimer": b.Disclaimer,
			"contractStatus": b.ContractStatus(s.now()), "contract": b.Contract})
	}
}

// putBankKey registers a bank's new public key. Withdrawals from now on are locked to it; earlier ones stay
// readable with the bank's earlier key (keeping that key is the bank's responsibility).
func (s *Server) putBankKey(w http.ResponseWriter, r *http.Request) {
	b, ok := s.bank(w, r)
	if !ok {
		return
	}
	var req struct {
		PublicKey string `json:"publicKey"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.Keys.SetBankPublic(b.Code, req.PublicKey, s.now()); err != nil {
		s.fail(w, errf(400, "invalid_key", "%v", err))
		return
	}
	writeJSON(w, 200, map[string]any{"bank": b.Code, "encryptionKey": s.bankKey(b.Code),
		"note": "New withdrawals are locked to this key. Earlier withdrawals stay readable only with the bank's earlier key."})
}
