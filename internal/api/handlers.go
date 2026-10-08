package api

import (
	"net/http"
	"strconv"

	"sellergraph/fisco/internal/hashing"
	"sellergraph/fisco/internal/ledger"
)

// ---------------------------------------------------------------- views

type FundingView struct {
	Name      string `json:"name,omitempty"`
	Address   string `json:"address"`
	Amount    int64  `json:"amount"`
	Confirmed bool   `json:"confirmed"`
}

type LoanView struct {
	ledger.Loan
	Funding []FundingView `json:"funding"`
}

func (s *Server) view(l ledger.Loan) LoanView {
	v := LoanView{Loan: l, Funding: []FundingView{}}
	for _, f := range l.Funding {
		v.Funding = append(v.Funding, FundingView{Name: s.funderName(f.Funder), Address: f.Funder, Amount: f.Amount, Confirmed: f.Confirmed})
	}
	return v
}

// respond returns the transaction plus the fresh loan snapshot.
func (s *Server) respond(w http.ResponseWriter, r *http.Request, code int, id string, extra map[string]any) {
	l, err := s.Ledger.GetLoan(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]any{"loan": s.view(l)}
	for k, v := range extra {
		out[k] = v
	}
	writeJSON(w, code, out)
}

// ---------------------------------------------------------------- chain

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
	writeJSON(w, 200, info)
}

// ---------------------------------------------------------------- origination

type fundingReq struct {
	Funder string `json:"funder"` // configured name ("webank") or 0x address
	Amount int64  `json:"amount"`
}

type createLoanReq struct {
	LoanID              string       `json:"loanId"`
	SellerID            string       `json:"sellerId,omitempty"`  // hashed with the salt, never stored in clear
	SellerRef           string       `json:"sellerRef,omitempty"` // or a precomputed reference
	Tier                uint8        `json:"tier"`
	Principal           int64        `json:"principal"`
	AnnualRateBps       int64        `json:"annualRateBps"`
	TenorDays           int64        `json:"tenorDays"`
	SweepRateBps        int64        `json:"sweepRateBps"`
	SweepCapBps         int64        `json:"sweepCapBps"`
	GraceDays           int64        `json:"graceDays"`
	HumanReviewRequired bool         `json:"humanReviewRequired"`
	Decision            *Doc         `json:"decision"` // approval package: ML output ids, memo, challenge, policy checks
	Funding             []fundingReq `json:"funding,omitempty"`
}

func (s *Server) createLoan(w http.ResponseWriter, r *http.Request) {
	var req createLoanReq
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	ctx := r.Context()
	var sellerRef ledger.Hash
	switch {
	case req.SellerRef != "":
		h, err := ledger.ParseHash(req.SellerRef)
		if err != nil {
			s.fail(w, ledger.Errorf(ledger.KindInvalid, "sellerRef: %v", err))
			return
		}
		sellerRef = h
	case req.SellerID != "":
		sellerRef = hashing.Ref(s.Salt, hashing.DomainSeller, req.SellerID)
	default:
		s.fail(w, ledger.Errorf(ledger.KindInvalid, "sellerId or sellerRef is required"))
		return
	}
	if _, err := ledger.IDToBytes32(req.LoanID); err != nil {
		s.fail(w, err)
		return
	}
	if req.Decision.empty() {
		s.fail(w, ledger.Errorf(ledger.KindInvalid, "decision (doc, hash or text) is required"))
		return
	}
	decisionHash, anchor, err := s.resolve(ctx, req.Decision, req.LoanID, ledger.KindDecisionPackage)
	if err != nil {
		s.fail(w, err)
		return
	}
	shares := []ledger.FundingShare{}
	if len(req.Funding) == 0 {
		a, err := s.funderAddr(s.DefaultFunder)
		if err != nil {
			s.fail(w, err)
			return
		}
		shares = append(shares, ledger.FundingShare{Funder: a, Amount: req.Principal})
	}
	for _, f := range req.Funding {
		a, err := s.funderAddr(f.Funder)
		if err != nil {
			s.fail(w, err)
			return
		}
		shares = append(shares, ledger.FundingShare{Funder: a, Amount: f.Amount})
	}
	tx, err := s.Ledger.CreateLoan(ctx, ledger.CreateLoan{
		LoanID: req.LoanID, SellerRef: sellerRef, Tier: req.Tier, Principal: req.Principal, RateBps: req.AnnualRateBps,
		TenorDays: req.TenorDays, SweepRateBps: req.SweepRateBps, SweepCapBps: req.SweepCapBps, GraceDays: req.GraceDays,
		HumanReviewRequired: req.HumanReviewRequired, DecisionHash: decisionHash, Funding: shares,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, http.StatusCreated, req.LoanID, map[string]any{"tx": tx, "decisionAnchor": anchor})
}

func (s *Server) confirmFunding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Funder string `json:"funder"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	a, err := s.funderAddr(req.Funder)
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	tx, err := s.Ledger.ConfirmFunding(r.Context(), id, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx})
}

func (s *Server) officerDecision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Approved bool `json:"approved"`
		Review   *Doc `json:"review"` // officer's review: each anomaly the agents raised, confirmed one by one
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	if req.Review.empty() {
		s.fail(w, ledger.Errorf(ledger.KindInvalid, "review (doc, hash or text) is required"))
		return
	}
	h, anchor, err := s.resolve(r.Context(), req.Review, id, ledger.KindHumanDecision)
	if err != nil {
		s.fail(w, err)
		return
	}
	tx, err := s.Ledger.OfficerDecision(r.Context(), id, req.Approved, h)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx, "reviewAnchor": anchor})
}

type reasonReq struct {
	Reason *Doc `json:"reason"`
	AsOf   Time `json:"asOf"`
}

func (s *Server) reason(w http.ResponseWriter, r *http.Request, kind ledger.AnchorKind) (string, ledger.Hash, int64, *ledger.AnchorRecord, bool) {
	var req reasonReq
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return "", ledger.ZeroHash, 0, nil, false
	}
	id := r.PathValue("id")
	h, anchor, err := s.resolve(r.Context(), req.Reason, id, kind)
	if err != nil {
		s.fail(w, err)
		return "", h, 0, nil, false
	}
	asOf, err := s.asOf(r.Context(), req.AsOf)
	if err != nil {
		s.fail(w, err)
		return "", h, 0, nil, false
	}
	return id, h, asOf, anchor, true
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	id, h, _, anchor, ok := s.reason(w, r, ledger.KindOther)
	if !ok {
		return
	}
	tx, err := s.Ledger.CancelLoan(r.Context(), id, h)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx, "reasonAnchor": anchor})
}

// ---------------------------------------------------------------- disbursement & repayment

type paymentReq struct {
	SupplierID  string `json:"supplierId,omitempty"`  // hashed with the salt
	SupplierRef string `json:"supplierRef,omitempty"` // or precomputed
	Amount      int64  `json:"amount"`
}

func (s *Server) disburse(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoanID       string       `json:"loanId,omitempty"` // legacy alias only
		Payments     []paymentReq `json:"payments"`
		DeliveryDays int64        `json:"deliveryDays"`
		BankTxRef    string       `json:"bankTxRef"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	lines := []ledger.PaymentLine{}
	for _, p := range req.Payments {
		var ref ledger.Hash
		switch {
		case p.SupplierRef != "":
			h, err := ledger.ParseHash(p.SupplierRef)
			if err != nil {
				s.fail(w, ledger.Errorf(ledger.KindInvalid, "supplierRef: %v", err))
				return
			}
			ref = h
		case p.SupplierID != "":
			ref = hashing.Ref(s.Salt, hashing.DomainSupplier, p.SupplierID)
		}
		lines = append(lines, ledger.PaymentLine{PayeeRef: ref, Amount: p.Amount})
	}
	if req.DeliveryDays == 0 {
		req.DeliveryDays = 10
	}
	tx, err := s.Ledger.RecordDisbursement(r.Context(), ledger.Disbursement{LoanID: id, Payments: lines, DeliveryDays: req.DeliveryDays, BankTxRef: hashing.ExtRef(req.BankTxRef)})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx})
}

func (s *Server) repay(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LoanID string                 `json:"loanId,omitempty"` // legacy alias only
		Amount int64                  `json:"amount"`
		Source ledger.RepaymentSource `json:"source"`
		ExtRef string                 `json:"extRef"` // idempotency key: bank debit / sweep id
		AsOf   Time                   `json:"asOf"`   // value date; defaults to chain time
		Notice *Doc                   `json:"notice"` // per-debit explanation sent to the seller (anchored)
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	if req.Source == 0 {
		req.Source = ledger.SourceProportionalSweep
	}
	asOf, err := s.asOf(r.Context(), req.AsOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.Ledger.RecordRepayment(r.Context(), ledger.Repayment{LoanID: id, Amount: req.Amount, Source: req.Source, ExtRef: hashing.ExtRef(req.ExtRef), AsOf: asOf})
	if err != nil {
		s.fail(w, err)
		return
	}
	_, anchor, err := s.resolve(r.Context(), req.Notice, id, ledger.KindSweepNotice)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": res.Tx, "allocation": res.Allocation, "settled": res.Settled, "noticeAnchor": anchor})
}

func (s *Server) floor(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FloorCumulative int64 `json:"floorCumulative"`
		Milestone       bool  `json:"milestone"`
		AsOf            Time  `json:"asOf"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	asOf, err := s.asOf(r.Context(), req.AsOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.Ledger.CheckpointFloor(r.Context(), ledger.FloorCheckpoint{LoanID: id, FloorCumulative: req.FloorCumulative, AsOf: asOf, Milestone: req.Milestone})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": res.Tx, "shortfall": res.Shortfall})
}

func (s *Server) mature(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AsOf Time `json:"asOf"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	asOf, err := s.asOf(r.Context(), req.AsOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	tx, err := s.Ledger.MarkMatured(r.Context(), id, asOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx})
}

func (s *Server) accelerate(w http.ResponseWriter, r *http.Request) {
	id, h, asOf, anchor, ok := s.reason(w, r, ledger.KindHumanDecision)
	if !ok {
		return
	}
	tx, err := s.Ledger.Accelerate(r.Context(), id, h, asOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx, "reasonAnchor": anchor})
}

func (s *Server) markDefault(w http.ResponseWriter, r *http.Request) {
	id, h, _, anchor, ok := s.reason(w, r, ledger.KindHumanDecision)
	if !ok {
		return
	}
	tx, err := s.Ledger.MarkDefault(r.Context(), id, h)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx, "reasonAnchor": anchor})
}

func (s *Server) restructure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExtraDays       int64 `json:"extraDays"`
		NewSweepRateBps int64 `json:"newSweepRateBps"`
		Reason          *Doc  `json:"reason"` // Agent 6 draft plan + officer approval
		AsOf            Time  `json:"asOf"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	h, anchor, err := s.resolve(r.Context(), req.Reason, id, ledger.KindHumanDecision)
	if err != nil {
		s.fail(w, err)
		return
	}
	asOf, err := s.asOf(r.Context(), req.AsOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	tx, err := s.Ledger.Restructure(r.Context(), ledger.Restructure{LoanID: id, ExtraDays: req.ExtraDays, NewSweepRateBps: req.NewSweepRateBps, ReasonHash: h, AsOf: asOf})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx, "reasonAnchor": anchor})
}

// ---------------------------------------------------------------- monitoring

func (s *Server) alert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Level    ledger.AlertLevel `json:"level"`
		Code     string            `json:"code"`
		Evidence *Doc              `json:"evidence"` // Agent 5 cause analysis (anchored)
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	h, anchor, err := s.resolve(r.Context(), req.Evidence, id, ledger.KindAgentIO)
	if err != nil {
		s.fail(w, err)
		return
	}
	tx, err := s.Ledger.RaiseAlert(r.Context(), id, req.Level, req.Code, h)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx, "evidenceAnchor": anchor})
}

func (s *Server) covenant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code     string `json:"code"`
		Breached bool   `json:"breached"`
		Evidence *Doc   `json:"evidence"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	h, anchor, err := s.resolve(r.Context(), req.Evidence, id, ledger.KindAgentIO)
	if err != nil {
		s.fail(w, err)
		return
	}
	tx, err := s.Ledger.RecordCovenantEvent(r.Context(), id, req.Code, req.Breached, h)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx, "evidenceAnchor": anchor})
}

func (s *Server) delivery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Line      int    `json:"line"`
		WaybillNo string `json:"waybillNo"`
		ShippedAt Time   `json:"shippedAt"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	at, err := s.asOf(r.Context(), req.ShippedAt)
	if err != nil {
		s.fail(w, err)
		return
	}
	res, err := s.Ledger.RecordDelivery(r.Context(), id, req.Line, hashing.Text(req.WaybillNo), at)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": res.Tx, "onTime": res.OnTime})
}

func (s *Server) missingDelivery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AsOf Time `json:"asOf"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("id")
	line, err := strconv.Atoi(r.PathValue("line"))
	if err != nil {
		s.fail(w, ledger.Errorf(ledger.KindInvalid, "line must be an integer"))
		return
	}
	asOf, err := s.asOf(r.Context(), req.AsOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	tx, err := s.Ledger.FlagMissingDelivery(r.Context(), id, line, asOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respond(w, r, 200, id, map[string]any{"tx": tx})
}

// ---------------------------------------------------------------- reads

func (s *Server) getLoan(w http.ResponseWriter, r *http.Request) {
	l, err := s.Ledger.GetLoan(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, s.view(l))
}

func (s *Server) listLoans(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ids, total, err := s.Ledger.ListLoanIDs(r.Context(), offset, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	loans := []LoanView{}
	for _, id := range ids {
		l, err := s.Ledger.GetLoan(r.Context(), id)
		if err != nil {
			s.fail(w, err)
			return
		}
		loans = append(loans, s.view(l))
	}
	writeJSON(w, 200, map[string]any{"total": total, "offset": offset, "limit": limit, "loans": loans})
}

func (s *Server) payoff(w http.ResponseWriter, r *http.Request) {
	var t Time
	if q := r.URL.Query().Get("asOf"); q != "" {
		if err := t.UnmarshalJSON([]byte(q)); err != nil {
			s.fail(w, err)
			return
		}
	}
	asOf, err := s.asOf(r.Context(), t)
	if err != nil {
		s.fail(w, err)
		return
	}
	p, err := s.Ledger.PreviewPayoff(r.Context(), r.PathValue("id"), asOf)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, p)
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
		err = ledger.Errorf(ledger.KindInvalid, "content.doc or contentHash is required")
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
