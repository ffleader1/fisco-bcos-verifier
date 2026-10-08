// Package api exposes the SellerGraph blockchain component as a JSON REST API for the orchestrator
// (fixed-code workflow engine), the officer console, the monitoring service and the dashboards.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sellergraph/fisco/internal/hashing"
	"sellergraph/fisco/internal/ledger"
)

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	Ledger        ledger.Ledger
	Salt          []byte            // HMAC salt for seller / supplier references
	Funders       map[string]string // funder name -> address
	DefaultFunder string
	Token         string // optional bearer token
	DisableDocs   bool   // hide /docs/ and /openapi.json
	Log           *slog.Logger
}

// Route is one API endpoint (used by Handler and by the OpenAPI consistency test).
type Route struct {
	Method, Pattern string
	Handler         http.HandlerFunc
}

// Routes lists every API endpoint.
func (s *Server) Routes() []Route {
	return []Route{
		{"GET", "/healthz", s.health},
		{"GET", "/v1/chain", s.chainInfo},

		{"POST", "/v1/loans", s.createLoan},
		{"GET", "/v1/loans", s.listLoans},
		{"GET", "/v1/loans/{id}", s.getLoan},
		{"GET", "/v1/loans/{id}/payoff", s.payoff},
		{"POST", "/v1/loans/{id}/funding/confirm", s.confirmFunding},
		{"POST", "/v1/loans/{id}/officer-decision", s.officerDecision},
		{"POST", "/v1/loans/{id}/cancel", s.cancel},
		{"POST", "/v1/loans/{id}/disburse", s.disburse},
		{"POST", "/v1/loans/{id}/repayments", s.repay},
		{"POST", "/v1/loans/{id}/floor-checkpoints", s.floor},
		{"POST", "/v1/loans/{id}/mature", s.mature},
		{"POST", "/v1/loans/{id}/accelerate", s.accelerate},
		{"POST", "/v1/loans/{id}/default", s.markDefault},
		{"POST", "/v1/loans/{id}/restructure", s.restructure},
		{"POST", "/v1/loans/{id}/alerts", s.alert},
		{"POST", "/v1/loans/{id}/covenant-events", s.covenant},
		{"POST", "/v1/loans/{id}/deliveries", s.delivery},
		{"POST", "/v1/loans/{id}/deliveries/{line}/missing", s.missingDelivery},
		{"GET", "/v1/portfolio", s.portfolio},

		{"POST", "/v1/audit/anchors", s.anchor},
		{"GET", "/v1/audit/anchors/{subject}", s.listAnchors},
		{"GET", "/v1/audit/anchors/{subject}/verify", s.verifyChain},
		{"POST", "/v1/audit/verify", s.verifyContent},

		// Aliases matching the endpoint names of the PoC architecture diagram (page 02).
		{"POST", "/blockchain/create-loan", s.createLoan},
		{"POST", "/blockchain/disburse", s.withBodyLoanID(s.disburse)},
		{"POST", "/repayment", s.withBodyLoanID(s.repay)},
		{"GET", "/portfolio", s.portfolio},
	}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.Routes() {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.Handler)
	}
	if !s.DisableDocs {
		mux.Handle("GET /docs/", docsHandler())
		mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "docs/") // relative: keeps a reverse-proxy sub-path intact
			w.WriteHeader(http.StatusMovedPermanently)
		})
		mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(OpenAPISpec())
		})
	}
	return s.logging(s.auth(mux))
}

// ---------------------------------------------------------------- middleware

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && r.URL.Path != "/healthz" && !isDocsPath(r.URL.Path) && r.Header.Get("Authorization") != "Bearer "+s.Token {
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		if s.Log != nil {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.code, "ms", time.Since(start).Milliseconds())
		}
	})
}

// withBodyLoanID adapts the legacy endpoints that carry loanId in the body.
func (s *Server) withBodyLoanID(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, 400, "invalid", err.Error())
			return
		}
		var probe struct {
			LoanID string `json:"loanId"`
		}
		_ = json.Unmarshal(raw, &probe)
		if probe.LoanID == "" {
			writeErr(w, 400, "invalid", "loanId is required")
			return
		}
		r.SetPathValue("id", probe.LoanID)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		h(w, r)
	}
}

// ---------------------------------------------------------------- helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"kind": kind, "message": msg}})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var le *ledger.Error
	if !errors.As(err, &le) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	code := map[ledger.ErrorKind]int{
		ledger.KindInvalid: 400, ledger.KindUnauthorized: 403, ledger.KindNotFound: 404,
		ledger.KindBadStatus: 409, ledger.KindDuplicate: 409, ledger.KindChain: 502,
	}[le.Kind]
	if code == 0 {
		code = 500
	}
	if code >= 500 && s.Log != nil {
		s.Log.Error("ledger error", "err", err)
	}
	writeErr(w, code, string(le.Kind), le.Message)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// Time accepts RFC3339 strings or unix seconds.
type Time struct {
	Unix int64
	Set  bool
}

func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		t.Unix, t.Set = n, true
		return nil
	}
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("time must be RFC3339 or unix seconds: %q", s)
	}
	t.Unix, t.Set = tm.Unix(), true
	return nil
}

func (s *Server) asOf(ctx context.Context, t Time) (int64, error) {
	if t.Set {
		return t.Unix, nil
	}
	return s.Ledger.Now(ctx)
}

// Doc lets callers supply either a JSON document (hashed canonically and anchored in the AuditTrail),
// a precomputed hash, or a free-text value (hashed, not anchored).
type Doc struct {
	Doc  json.RawMessage `json:"doc,omitempty"`
	Hash string          `json:"hash,omitempty"`
	Text string          `json:"text,omitempty"`
	URI  string          `json:"uri,omitempty"`
}

func (d *Doc) empty() bool { return d == nil || (len(d.Doc) == 0 && d.Hash == "" && d.Text == "") }

// resolve returns the bytes32 for d, anchoring d.Doc under subject with the given kind.
// A document already anchored for this subject (client retry) is accepted.
func (s *Server) resolve(ctx context.Context, d *Doc, subject string, kind ledger.AnchorKind) (ledger.Hash, *ledger.AnchorRecord, error) {
	switch {
	case d == nil || d.empty():
		return ledger.ZeroHash, nil, nil
	case len(d.Doc) > 0:
		h, err := hashing.Content(d.Doc)
		if err != nil {
			return h, nil, ledger.Errorf(ledger.KindInvalid, "%v", err)
		}
		rec, err := s.Ledger.Anchor(ctx, ledger.AnchorRequest{Subject: subject, Kind: kind, ContentHash: h, URI: d.URI})
		if err != nil {
			if ledger.KindOf(err) == ledger.KindDuplicate {
				return h, nil, nil
			}
			return h, nil, err
		}
		return h, &rec, nil
	case d.Hash != "":
		h, err := ledger.ParseHash(d.Hash)
		if err != nil {
			return h, nil, ledger.Errorf(ledger.KindInvalid, "%v", err)
		}
		return h, nil, nil
	default:
		return hashing.Text(d.Text), nil, nil
	}
}

func (s *Server) funderAddr(nameOrAddr string) (string, error) {
	if strings.HasPrefix(nameOrAddr, "0x") && len(nameOrAddr) == 42 {
		return strings.ToLower(nameOrAddr), nil
	}
	if a, ok := s.Funders[nameOrAddr]; ok {
		return a, nil
	}
	return "", ledger.Errorf(ledger.KindInvalid, "unknown funder %q", nameOrAddr)
}

func (s *Server) funderName(addr string) string {
	for n, a := range s.Funders {
		if strings.EqualFold(a, addr) {
			return n
		}
	}
	return ""
}
