// Package api exposes the BouyUp blockchain component as a JSON REST API: encrypted withdrawals (Agent 4 or a
// human), the demo bank catalog (regions, bank policies for Agent 3, bank contracts, encryption keys), the
// audit trail used by the other agents, a public read-only explorer of the chain and an overview for the team.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"bouyup/fisco/internal/catalog"
	"bouyup/fisco/internal/keystore"
	"bouyup/fisco/internal/ledger"
	"bouyup/fisco/internal/payout"
)

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	Ledger          ledger.Ledger
	Catalog         *catalog.Catalog
	Payer           payout.Payer
	Keys            *keystore.Store   // master + bank encryption keys
	ShowDemoKeys    bool              // print the full keys in GET /v1/overview (demo only)
	ChainKeys       map[string]string // chain signing keys by role (admin, operator), hex — listed in the overview's secrets
	Salt            []byte            // HMAC salt for account / staff references
	Token           string            // optional bearer token for /v1 (docs, explorer and /healthz stay public)
	AllowedAgents   []string
	DisableDocs     bool // hide /docs/ and /openapi.json
	DisableExplorer bool // hide /explorer/
	Now             func() time.Time
	Log             *slog.Logger
}

// DefaultAllowedAgents are the agents that may ask for a withdrawal (Agent 4: disbursement & collection).
var DefaultAllowedAgents = []string{"agent-4"}

// Route is one API endpoint (used by Handler and by the OpenAPI consistency test).
type Route struct {
	Method, Pattern string
	Handler         http.HandlerFunc
}

// Routes lists every JSON endpoint.
func (s *Server) Routes() []Route {
	return []Route{
		{"GET", "/healthz", s.health},
		{"GET", "/v1/chain", s.chainInfo},
		{"GET", "/v1/overview", s.overview},

		{"POST", "/v1/withdrawals", s.createWithdrawal},
		{"GET", "/v1/withdrawals", s.listWithdrawals},
		{"GET", "/v1/withdrawals/{id}", s.getWithdrawal},
		{"GET", "/v1/withdrawals/{id}/verify", s.verifyWithdrawal},
		{"POST", "/v1/demo/withdrawals", s.demoWithdrawal},

		{"GET", "/v1/regions", s.listRegions},
		{"GET", "/v1/banks", s.listBanks},
		{"GET", "/v1/banks/{code}", s.getBank},
		{"GET", "/v1/banks/{code}/policy", s.getBankPolicy},
		{"GET", "/v1/banks/{code}/contract", s.getBankContract},
		{"PUT", "/v1/banks/{code}/public-key", s.putBankKey},

		{"POST", "/v1/audit/anchors", s.anchor},
		{"GET", "/v1/audit/anchors/{subject}", s.listAnchors},
		{"GET", "/v1/audit/anchors/{subject}/verify", s.verifyChain},
		{"POST", "/v1/audit/verify", s.verifyContent},

		{"GET", "/explorer/api/overview", s.exOverview},
		{"GET", "/explorer/api/blocks", s.exBlocks},
		{"GET", "/explorer/api/blocks/{number}", s.exBlock},
		{"GET", "/explorer/api/tx/{hash}", s.exTx},
		{"GET", "/explorer/api/withdrawals", s.exWithdrawals},
		{"GET", "/explorer/api/withdrawals/{id}", s.exWithdrawal},
		{"GET", "/explorer/api/search", s.exSearch},
		{"POST", "/explorer/api/decrypt", s.decrypt},
	}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.Routes() {
		if s.DisableExplorer && strings.HasPrefix(rt.Pattern, "/explorer/") {
			continue
		}
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.Handler)
	}
	if !s.DisableDocs {
		mux.Handle("GET /docs/", docsHandler())
		mux.HandleFunc("GET /docs", redirect("docs/"))
		mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(OpenAPISpec())
		})
	}
	mux.HandleFunc("GET /overview/{$}", overviewPage)
	mux.HandleFunc("GET /overview", redirect("overview/"))
	mux.HandleFunc("GET /{$}", redirect("overview/"))
	if !s.DisableExplorer {
		mux.HandleFunc("GET /explorer/{$}", explorerPage)
		mux.HandleFunc("GET /explorer/decrypt_withdrawal.py", decryptScriptFile)
		mux.HandleFunc("GET /explorer", redirect("explorer/"))
	}
	return s.logging(s.auth(mux))
}

// redirect answers with a relative Location, which keeps a reverse-proxy sub-path intact.
func redirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", to)
		w.WriteHeader(http.StatusMovedPermanently)
	}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// ---------------------------------------------------------------- middleware

// isPublicPath reports whether a path needs no API token: health, docs, the read-only explorer and the
// overview page shell (the secrets it shows come from /v1/overview, which keeps the token check).
func isPublicPath(p string) bool {
	return p == "/" || p == "/healthz" || isDocsPath(p) || p == "/explorer" || strings.HasPrefix(p, "/explorer/") ||
		p == "/overview" || p == "/overview/"
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" && !isPublicPath(r.URL.Path) && r.Header.Get("Authorization") != "Bearer "+s.Token {
			writeErr(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid bearer token", nil)
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

// ---------------------------------------------------------------- errors & helpers

// apiError is an error with an HTTP status and a stable kind, e.g. invalid_bank_ratio.
type apiError struct {
	Status  int
	Kind    string
	Message string
	Details map[string]any
}

func (e *apiError) Error() string { return e.Kind + ": " + e.Message }

func errf(status int, kind, format string, args ...any) *apiError {
	return &apiError{Status: status, Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func (e *apiError) with(k string, v any) *apiError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind, msg string, details map[string]any) {
	body := map[string]any{"kind": kind, "message": msg}
	if len(details) > 0 {
		body["details"] = details
	}
	writeJSON(w, code, map[string]any{"error": body})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeErr(w, ae.Status, ae.Kind, ae.Message, ae.Details)
		return
	}
	var le *ledger.Error
	if !errors.As(err, &le) {
		writeErr(w, http.StatusBadRequest, "invalid", err.Error(), nil)
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
	writeErr(w, code, string(le.Kind), le.Message, nil)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return errf(400, "invalid", "invalid JSON body: %v", err)
	}
	return nil
}

// Doc wraps a JSON document that is hashed canonically (sorted keys, SHA-256).
type Doc struct {
	Doc json.RawMessage `json:"doc,omitempty"`
}
