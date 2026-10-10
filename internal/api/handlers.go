package api

import (
	"net/http"

	"buoyup/fisco/internal/hashing"
	"buoyup/fisco/internal/ledger"
)

// ---------------------------------------------------------------- system

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	info, err := s.Ledger.Info(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "backend": info.Backend, "blockNumber": info.BlockNumber})
}

func (s *Server) chainInfo(w http.ResponseWriter, r *http.Request) {
	info, err := s.Ledger.Info(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	nodes, err := s.Ledger.Nodes(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"chain": info, "nodes": nodes})
}

// ---------------------------------------------------------------- audit

func (s *Server) anchor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject     string            `json:"subject"`
		Kind        ledger.AnchorKind `json:"kind"`
		Content     *Doc              `json:"content"`
		ContentHash string            `json:"contentHash"`
		URI         string            `json:"uri"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	var h ledger.Hash
	var err error
	switch {
	case req.Content != nil && len(req.Content.Doc) > 0:
		h, err = hashing.Content(req.Content.Doc)
	case req.ContentHash != "":
		h, err = ledger.ParseHash(req.ContentHash)
	default:
		err = errf(400, "invalid", "content.doc or contentHash is required")
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	rec, err := s.Ledger.Anchor(r.Context(), ledger.AnchorRequest{Subject: req.Subject, Kind: req.Kind, ContentHash: h, URI: req.URI})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) listAnchors(w http.ResponseWriter, r *http.Request) {
	recs, err := s.Ledger.ListAnchors(r.Context(), r.PathValue("subject"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"subject": r.PathValue("subject"), "records": recs})
}

// verifyChain recomputes the subject's hash chain from the on-chain records.
func (s *Server) verifyChain(w http.ResponseWriter, r *http.Request) {
	recs, err := s.Ledger.ListAnchors(r.Context(), r.PathValue("subject"))
	if err != nil {
		s.fail(w, err)
		return
	}
	var prev ledger.Hash
	for _, rec := range recs {
		if ledger.ChainHead(prev, rec.Kind, rec.ContentHash, rec.Timestamp) != rec.Head {
			writeJSON(w, 200, map[string]any{"valid": false, "brokenAt": rec.Index, "records": len(recs)})
			return
		}
		prev = rec.Head
	}
	writeJSON(w, 200, map[string]any{"valid": true, "records": len(recs), "head": prev})
}

// verifyContent answers "was this exact document anchored, where and when?".
func (s *Server) verifyContent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content     *Doc   `json:"content"`
		ContentHash string `json:"contentHash"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	var h ledger.Hash
	var err error
	if req.Content != nil && len(req.Content.Doc) > 0 {
		h, err = hashing.Content(req.Content.Doc)
	} else {
		h, err = ledger.ParseHash(req.ContentHash)
	}
	if err != nil {
		s.fail(w, ledger.Errorf(ledger.KindInvalid, "%v", err))
		return
	}
	rec, found, err := s.Ledger.LocateAnchor(r.Context(), h)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"contentHash": h, "found": found}
	if found {
		out["record"] = rec
	}
	writeJSON(w, 200, out)
}
