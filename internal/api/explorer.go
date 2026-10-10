package api

import (
	_ "embed"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// explorerHTML is the public, read-only chain explorer (single page, no external assets).
//
//go:embed explorer/index.html
var explorerHTML []byte

func explorerPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(explorerHTML)
}

func (s *Server) exOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info, err := s.Ledger.Info(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	nodes, err := s.Ledger.Nodes(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	blocks, err := s.Ledger.LatestBlocks(ctx, 10)
	if err != nil {
		s.fail(w, err)
		return
	}
	items, total, err := s.Ledger.ListWithdrawals(ctx, 0, 10)
	if err != nil {
		s.fail(w, err)
		return
	}
	wds := []onChainView{}
	for _, it := range items {
		wds = append(wds, onChain(it, false))
	}
	writeJSON(w, 200, map[string]any{"chain": map[string]any{"backend": info.Backend, "blockNumber": info.BlockNumber, "chainTime": info.ChainTime,
		"groupId": info.GroupID, "contracts": info.Contracts}, "nodes": nodes, "latestBlocks": blocks, "latestWithdrawals": wds, "totalWithdrawals": total})
}

func (s *Server) exBlocks(w http.ResponseWriter, r *http.Request) {
	_, limit := pageArgs(r, 20, 50)
	blocks, err := s.Ledger.LatestBlocks(r.Context(), limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"blocks": blocks})
}

func (s *Server) exBlock(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil {
		s.fail(w, errf(400, "invalid", "block number must be an integer"))
		return
	}
	b, err := s.Ledger.GetBlock(r.Context(), n)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, b)
}

var txHashRe = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

func (s *Server) exTx(w http.ResponseWriter, r *http.Request) {
	h := r.PathValue("hash")
	if !txHashRe.MatchString(h) {
		s.fail(w, errf(400, "invalid", "transaction hash must be 0x followed by 64 hex characters"))
		return
	}
	d, err := s.Ledger.GetTransaction(r.Context(), h)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"transaction": d}
	for _, e := range d.Events {
		if e.Name == "WithdrawalRecorded" {
			if id, ok := e.Fields["withdrawalId"].(string); ok {
				if wd, err := s.Ledger.GetWithdrawal(r.Context(), id); err == nil {
					out["withdrawal"] = onChain(wd, true)
					out["verification"] = s.verify(r.Context(), wd, false)
				}
			}
		}
	}
	writeJSON(w, 200, out)
}

// exWithdrawals lists what the chain shows: ids, roots, sizes. Nothing readable without a key.
func (s *Server) exWithdrawals(w http.ResponseWriter, r *http.Request) {
	offset, limit := pageArgs(r, 20, 100)
	items, total, err := s.Ledger.ListWithdrawals(r.Context(), offset, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []onChainView{}
	for _, it := range items {
		out = append(out, onChain(it, false))
	}
	writeJSON(w, 200, map[string]any{"total": total, "offset": offset, "limit": limit, "items": out})
}

func (s *Server) exWithdrawal(w http.ResponseWriter, r *http.Request) {
	wd, err := s.lookupWithdrawal(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"withdrawal": onChain(wd, true), "chain": s.chainOf(r.Context(), wd), "verification": s.verify(r.Context(), wd, false)})
}

// exSearch resolves a block number, a transaction hash or a withdrawal id.
func (s *Server) exSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	ctx := r.Context()
	if n, err := strconv.ParseInt(q, 10, 64); err == nil {
		if _, err := s.Ledger.GetBlock(ctx, n); err == nil {
			writeJSON(w, 200, map[string]any{"type": "block", "target": q})
			return
		}
	}
	if txHashRe.MatchString(q) {
		if _, err := s.Ledger.GetTransaction(ctx, q); err == nil {
			writeJSON(w, 200, map[string]any{"type": "tx", "target": strings.ToLower(q)})
			return
		}
	}
	for _, id := range []string{q, strings.ToUpper(q)} {
		if _, err := s.Ledger.GetWithdrawal(ctx, id); err == nil {
			writeJSON(w, 200, map[string]any{"type": "withdrawal", "target": id})
			return
		}
	}
	s.fail(w, errf(404, "not_found", "nothing matches %q (try a block number, a transaction hash or a withdrawal id)", q))
}
