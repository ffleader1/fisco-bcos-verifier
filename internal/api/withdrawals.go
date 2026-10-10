package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"bouyup/fisco/internal/catalog"
	"bouyup/fisco/internal/hashing"
	"bouyup/fisco/internal/ledger"
	"bouyup/fisco/internal/payout"
	"bouyup/fisco/internal/sealing"
)

// ---------------------------------------------------------------- request

// actorReq says who asked for the withdrawal.
type actorReq struct {
	Type ledger.ActorType `json:"type"` // agent | human
	// agent
	AgentID string `json:"agentId,omitempty"` // e.g. agent-4
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"` // e.g. 4.2.0
	Model   string `json:"model,omitempty"`
	RunID   string `json:"runId,omitempty"`
	// human
	StaffID     string `json:"staffId,omitempty"`
	Role        string `json:"role,omitempty"`
	Department  string `json:"department,omitempty"`
	Reason      string `json:"reason,omitempty"`
	ApprovalRef string `json:"approvalRef,omitempty"`
}

type withdrawalReq struct {
	RequestID string                 `json:"requestId,omitempty"` // idempotency key
	UserID    string                 `json:"userId"`
	AccountNo string                 `json:"accountNo"`
	Amount    int64                  `json:"amount"`
	Currency  string                 `json:"currency,omitempty"`
	Region    string                 `json:"region"`
	Province  string                 `json:"province"`
	BankSplit map[string]json.Number `json:"bankSplit"`
	Actor     *actorReq              `json:"actor"`
}

var (
	percentRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	tokenRe   = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,32}$`)
)

// parsePercent turns "25.5" into 2550 basis points. At most 2 decimals, strictly positive, at most 100.
func parsePercent(bank string, n json.Number) (int64, *apiError) {
	s := n.String()
	if !percentRe.MatchString(s) {
		return 0, errf(400, "invalid_bank_ratio", "%s: ratio %q must be a plain positive number such as 60 or 25.5", bank, s).with("bank", bank)
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, errf(400, "invalid_bank_ratio", "%s: ratio %s has more than 2 decimals", bank, s).with("bank", bank)
	}
	frac += strings.Repeat("0", 2-len(frac))
	if len(whole) > 3 {
		return 0, errf(400, "invalid_bank_ratio", "%s: ratio %s is above 100%%", bank, s).with("bank", bank)
	}
	w, _ := strconv.ParseInt(whole, 10, 64)
	f, _ := strconv.ParseInt(frac, 10, 64)
	bps := w*100 + f
	switch {
	case bps <= 0:
		return 0, errf(400, "invalid_bank_ratio", "%s: ratio must be greater than 0", bank).with("bank", bank)
	case bps > ledger.TotalBps:
		return 0, errf(400, "invalid_bank_ratio", "%s: ratio %s is above 100%%", bank, s).with("bank", bank)
	}
	return bps, nil
}

func pct(bps int64) string { return fmt.Sprintf("%d.%02d", bps/100, bps%100) }

// leg is one bank's share of a withdrawal.
type leg struct {
	Bank     string
	RatioBps int64
	Amount   int64
}

// validated is a checked request, ready to pay and record.
type validated struct {
	req      withdrawalReq
	region   catalog.Region
	province catalog.Province
	legs     []leg
	banks    map[string]*catalog.Bank
}

func (s *Server) validate(req withdrawalReq) (*validated, error) {
	v := &validated{req: req, banks: map[string]*catalog.Bank{}}
	switch {
	case strings.TrimSpace(req.UserID) == "":
		return nil, errf(400, "invalid", "userId is required")
	case strings.TrimSpace(req.AccountNo) == "":
		return nil, errf(400, "invalid", "accountNo is required")
	case req.Amount <= 0:
		return nil, errf(400, "invalid_amount", "amount must be a positive integer in the currency's minor unit")
	case req.Amount > 1e15:
		return nil, errf(400, "invalid_amount", "amount is too large")
	case req.RequestID != "" && len(req.RequestID) > 128:
		return nil, errf(400, "invalid", "requestId must be at most 128 characters")
	}
	reg, ok := s.Catalog.Region(req.Region)
	if !ok {
		return nil, errf(400, "unknown_region", "region %q is not supported (use one of %s)", req.Region, s.regionCodes())
	}
	v.region = reg
	prov, ok := reg.Province(req.Province)
	if !ok {
		return nil, errf(400, "unknown_province", "province %q is not a province of %s (see GET /v1/regions)", req.Province, reg.Code)
	}
	v.province = prov
	if req.Currency != "" && !strings.EqualFold(req.Currency, reg.Currency) {
		return nil, errf(400, "currency_mismatch", "region %s pays in %s, not %s", reg.Code, reg.Currency, req.Currency)
	}
	if err := s.validateActor(req.Actor); err != nil {
		return nil, err
	}
	if len(req.BankSplit) == 0 {
		return nil, errf(400, "invalid_bank_ratio", "bankSplit is required, e.g. {\"VCB\": 60, \"BIDV\": 40}")
	}
	if len(req.BankSplit) > 20 {
		return nil, errf(400, "invalid_bank_ratio", "at most 20 banks per withdrawal")
	}
	var sum int64
	for code, n := range req.BankSplit {
		b, ok := s.Catalog.Bank(code)
		if !ok {
			return nil, errf(400, "unknown_bank", "unknown bank %q (see GET /v1/banks?region=%s)", code, reg.Code).with("bank", code)
		}
		if !strings.EqualFold(b.Region, reg.Code) {
			return nil, errf(400, "bank_region_mismatch", "bank %s operates in %s, not %s", b.Code, b.Region, reg.Code).with("bank", b.Code)
		}
		if _, dup := v.banks[b.Code]; dup {
			return nil, errf(400, "invalid_bank_ratio", "bank %s appears twice", b.Code).with("bank", b.Code)
		}
		if st := b.ContractStatus(s.now()); !b.Active || st != "active" {
			if !b.Active {
				st = "inactive"
			}
			return nil, errf(400, "bank_unavailable", "bank %s cannot take part in withdrawals (contract %s)", b.Code, st).with("bank", b.Code)
		}
		bps, aerr := parsePercent(b.Code, n)
		if aerr != nil {
			return nil, aerr
		}
		v.banks[b.Code] = b
		v.legs = append(v.legs, leg{Bank: b.Code, RatioBps: bps})
		sum += bps
	}
	if sum != ledger.TotalBps {
		return nil, errf(400, "invalid_bank_ratio", "bank ratios add up to %s%%, they must add up to exactly 100%%", pct(sum)).
			with("sumPercent", pct(sum))
	}
	// Largest share first (ties by bank code), so the split is deterministic.
	sort.Slice(v.legs, func(i, j int) bool {
		if v.legs[i].RatioBps != v.legs[j].RatioBps {
			return v.legs[i].RatioBps > v.legs[j].RatioBps
		}
		return v.legs[i].Bank < v.legs[j].Bank
	})
	var allocated int64
	for i := range v.legs {
		v.legs[i].Amount = req.Amount * v.legs[i].RatioBps / ledger.TotalBps
		allocated += v.legs[i].Amount
	}
	v.legs[0].Amount += req.Amount - allocated // rounding remainder goes to the largest share
	for _, l := range v.legs {
		if l.Amount <= 0 {
			return nil, errf(400, "amount_too_small", "amount %d is too small to give bank %s a share of %s%%", req.Amount, l.Bank, pct(l.RatioBps)).with("bank", l.Bank)
		}
	}
	return v, nil
}

func (s *Server) regionCodes() string {
	var out []string
	for _, r := range s.Catalog.Regions {
		out = append(out, r.Code)
	}
	return strings.Join(out, ", ")
}

func (s *Server) allowedAgents() []string {
	if len(s.AllowedAgents) > 0 {
		return s.AllowedAgents
	}
	return DefaultAllowedAgents
}

func (s *Server) validateActor(a *actorReq) error {
	if a == nil {
		return errf(400, "invalid_actor", "actor is required: {\"type\": \"agent\", \"agentId\": \"agent-4\", \"version\": \"...\"} or {\"type\": \"human\", \"staffId\": \"...\", \"role\": \"...\", \"reason\": \"...\"}")
	}
	switch a.Type {
	case ledger.ActorAgent:
		switch {
		case !tokenRe.MatchString(a.AgentID):
			return errf(400, "invalid_actor", "actor.agentId is required (1-32 characters of letters, digits, . _ : -)")
		case !tokenRe.MatchString(a.Version):
			return errf(400, "invalid_actor", "actor.version is required (1-32 characters of letters, digits, . _ : -)")
		case a.StaffID != "" || a.Role != "" || a.Reason != "":
			return errf(400, "invalid_actor", "staffId, role and reason are for human actors")
		}
		for _, ok := range s.allowedAgents() {
			if strings.EqualFold(ok, a.AgentID) {
				a.AgentID = strings.ToLower(a.AgentID)
				return nil
			}
		}
		return errf(403, "actor_not_allowed", "%s may not withdraw; only %s can", a.AgentID, strings.Join(s.allowedAgents(), ", "))
	case ledger.ActorHuman:
		switch {
		case strings.TrimSpace(a.StaffID) == "":
			return errf(400, "invalid_actor", "actor.staffId is required for a human actor")
		case !tokenRe.MatchString(a.Role):
			return errf(400, "invalid_actor", "actor.role is required for a human actor (1-32 characters, e.g. credit_officer)")
		case strings.TrimSpace(a.Reason) == "":
			return errf(400, "invalid_actor", "actor.reason is required: say why this withdrawal is done by hand")
		case a.AgentID != "" || a.Version != "":
			return errf(400, "invalid_actor", "agentId and version are for agent actors")
		}
		return nil
	}
	return errf(400, "invalid_actor", "actor.type must be \"agent\" or \"human\"")
}

// ---------------------------------------------------------------- ids

// withdrawalID is derived from requestId (so a retry maps to the same record) or random.
func withdrawalID(requestID string) string {
	var b []byte
	if requestID != "" {
		h := sha256.Sum256([]byte("bouyup-withdrawal:" + requestID))
		b = h[:8]
	} else {
		b = make([]byte, 8)
		_, _ = rand.Read(b)
	}
	return "WD-" + strings.ToUpper(hex.EncodeToString(b))
}

// ---------------------------------------------------------------- the readable pieces

// actorInfo is the actor as written inside the encrypted summary.
type actorInfo struct {
	Type     ledger.ActorType `json:"type"`
	AgentID  string           `json:"agentId,omitempty"`
	Version  string           `json:"version,omitempty"`
	Role     string           `json:"role,omitempty"`
	StaffRef string           `json:"staffRef,omitempty"` // salted reference of the staff id
	Hash     ledger.Hash      `json:"hash"`               // SHA-256 of the full actor object as sent
}

// summaryPiece is piece 0: readable by every bank of the withdrawal and by BouyUp.
type summaryPiece struct {
	Type         string      `json:"type"` // "summary"
	WithdrawalID string      `json:"withdrawalId"`
	Amount       int64       `json:"amount"`
	Currency     string      `json:"currency"`
	Region       string      `json:"region"`
	Province     string      `json:"province"`
	AccountRef   ledger.Hash `json:"accountRef"`
	PayoutRef    string      `json:"payoutRef"`
	PaidAt       time.Time   `json:"paidAt"`
	Actor        actorInfo   `json:"actor"`
	Banks        int         `json:"banks"`       // how many banks share it (not which)
	RequestHash  ledger.Hash `json:"requestHash"` // identifies the request, for safe retries
	Salt         string      `json:"salt"`
}

// bankPiece is one bank's share: readable by that bank and by BouyUp only.
type bankPiece struct {
	Type         string  `json:"type"` // "bank_share"
	WithdrawalID string  `json:"withdrawalId"`
	Bank         string  `json:"bank"`
	BankName     string  `json:"bankName"`
	Percent      float64 `json:"percent"`
	RatioBps     int64   `json:"ratioBps"`
	Amount       int64   `json:"amount"`
	Currency     string  `json:"currency"`
	ContractNo   string  `json:"contractNo"`
	Salt         string  `json:"salt"`
}

// requestHash identifies what was asked (not the random salts), so a retry can be recognised.
func (s *Server) requestHash(v *validated, accountRef, actorHash ledger.Hash) ledger.Hash {
	legs := []map[string]any{}
	for _, l := range v.legs {
		legs = append(legs, map[string]any{"bank": l.Bank, "ratioBps": l.RatioBps})
	}
	h, _ := hashing.ContentOf(map[string]any{"accountRef": accountRef.Hex(), "amount": v.req.Amount, "currency": v.region.Currency,
		"region": v.region.Code, "province": v.province.Code, "legs": legs, "actorHash": actorHash.Hex()})
	return h
}

func (s *Server) actorInfo(a *actorReq) (actorInfo, error) {
	raw, _ := json.Marshal(a)
	h, err := hashing.Content(raw)
	if err != nil {
		return actorInfo{}, err
	}
	out := actorInfo{Type: a.Type, Hash: h}
	if a.Type == ledger.ActorAgent {
		out.AgentID, out.Version = a.AgentID, a.Version
	} else {
		out.Role, out.StaffRef = a.Role, hashing.Ref(s.Salt, hashing.DomainStaff, strings.TrimSpace(a.StaffID)).Hex()
	}
	return out, nil
}

// sealWithdrawal builds and encrypts the pieces: the summary for every bank + BouyUp, each share for its bank +
// BouyUp. Recipient 0 is the BouyUp master key.
func (s *Server) sealWithdrawal(id string, v *validated, sum summaryPiece) (sealing.Sealed, error) {
	recipients := []string{s.Keys.MasterPublic()}
	summaryReaders := []int{0}
	var pieces []sealing.Piece
	for _, l := range v.legs {
		pub, ok := s.Keys.BankPublic(l.Bank)
		if !ok {
			return sealing.Sealed{}, errf(400, "bank_unavailable", "bank %s has no encryption key registered", l.Bank)
		}
		recipients = append(recipients, pub)
		r := len(recipients) - 1
		summaryReaders = append(summaryReaders, r)
		b := v.banks[l.Bank]
		plain, _ := json.Marshal(bankPiece{Type: "bank_share", WithdrawalID: id, Bank: l.Bank, BankName: b.Name, Percent: float64(l.RatioBps) / 100,
			RatioBps: l.RatioBps, Amount: l.Amount, Currency: v.region.Currency, ContractNo: b.Contract.ContractNo, Salt: sealing.Salt()})
		pieces = append(pieces, sealing.Piece{Plain: plain, Readers: []int{0, r}})
	}
	sum.Salt = sealing.Salt()
	plain, _ := json.Marshal(sum)
	pieces = append([]sealing.Piece{{Plain: plain, Readers: summaryReaders}}, pieces...)
	return sealing.Seal(id, recipients, pieces)
}

// decoded is a withdrawal opened with the master key.
type decoded struct {
	summary summaryPiece
	shares  []bankPiece
	opened  []sealing.Opened
}

func (s *Server) openAll(w ledger.Withdrawal) (*decoded, error) {
	opened, err := sealing.Open(w.ID, w.Envelope, s.Keys.MasterPrivate(), w.Commitments)
	if err != nil {
		return nil, err
	}
	d := &decoded{opened: opened}
	for _, o := range opened {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(o.Plaintext, &probe)
		switch probe.Type {
		case "summary":
			_ = json.Unmarshal(o.Plaintext, &d.summary)
		case "bank_share":
			var b bankPiece
			_ = json.Unmarshal(o.Plaintext, &b)
			d.shares = append(d.shares, b)
		}
	}
	return d, nil
}

// ---------------------------------------------------------------- views

type legView struct {
	Bank          string  `json:"bank"`
	BankName      string  `json:"bankName,omitempty"`
	Percent       float64 `json:"percent"`
	RatioBps      int64   `json:"ratioBps"`
	Amount        int64   `json:"amount"`
	ContractNo    string  `json:"contractNo,omitempty"`
	Reimbursement string  `json:"reimbursement"` // pending: BouyUp still has to claim this share from the bank
}

type chainView struct {
	Backend        string                `json:"backend"`
	TxHash         string                `json:"txHash"`
	BlockNumber    int64                 `json:"blockNumber"`
	BlockHash      string                `json:"blockHash,omitempty"`
	Timestamp      time.Time             `json:"timestamp"`
	Contract       string                `json:"contract,omitempty"`
	SubmittedTo    string                `json:"submittedTo,omitempty"`
	SealedBy       string                `json:"sealedBy,omitempty"`
	SealerNodeID   string                `json:"sealerNodeId,omitempty"`
	ConfirmedNodes int                   `json:"confirmedNodes"`
	TotalNodes     int                   `json:"totalNodes"`
	Confirmations  []ledger.Confirmation `json:"confirmations"`
	ExplorerURL    string                `json:"explorerUrl"`
	VerifyURL      string                `json:"verifyUrl"`
}

// onChainView is exactly what the chain holds: readable by anyone, meaningful only with a key.
type onChainView struct {
	WithdrawalID  string          `json:"withdrawalId"`
	Root          ledger.Hash     `json:"root"`
	Commitments   []ledger.Hash   `json:"commitments"`
	Pieces        int             `json:"pieces"`
	Slots         int             `json:"slots"`
	EnvelopeBytes int             `json:"envelopeBytes"`
	Envelope      json.RawMessage `json:"envelope,omitempty"`
	Timestamp     time.Time       `json:"timestamp"`
	BlockNumber   int64           `json:"blockNumber"`
	TxHash        string          `json:"txHash,omitempty"`
	Writer        string          `json:"writer"`
}

func onChain(w ledger.Withdrawal, withEnvelope bool) onChainView {
	p, sl, _ := sealing.Counts(w.Envelope)
	v := onChainView{WithdrawalID: w.ID, Root: w.Root, Commitments: w.Commitments, Pieces: p, Slots: sl, EnvelopeBytes: len(w.Envelope),
		Timestamp: time.Unix(w.Timestamp, 0).UTC(), BlockNumber: w.BlockNumber, TxHash: w.TxHash, Writer: w.Writer}
	if withEnvelope {
		v.Envelope = json.RawMessage(w.Envelope)
	}
	return v
}

type withdrawalView struct {
	WithdrawalID string         `json:"withdrawalId"`
	Status       string         `json:"status"`
	Replayed     bool           `json:"replayed,omitempty"` // true when a retry with the same requestId returned the existing record
	Amount       int64          `json:"amount"`
	Currency     string         `json:"currency"`
	Region       string         `json:"region"`
	Province     string         `json:"province"`
	ProvinceName string         `json:"provinceName,omitempty"`
	AccountRef   ledger.Hash    `json:"accountRef"`
	Actor        actorInfo      `json:"actor"`
	ActorRequest *actorReq      `json:"actorRequest,omitempty"` // the actor object as sent (POST response only; not on chain)
	Payout       *payout.Result `json:"payout,omitempty"`       // POST response only
	PayoutRef    string         `json:"payoutRef"`
	Legs         []legView      `json:"legs"`
	OnChain      onChainView    `json:"onChain"`
	Chain        *chainView     `json:"chain,omitempty"`
	Trace        *traceView     `json:"trace,omitempty"` // demo endpoint only
}

func (s *Server) view(ctx context.Context, w ledger.Withdrawal, d *decoded, withChain bool) withdrawalView {
	sm := d.summary
	v := withdrawalView{WithdrawalID: w.ID, Status: "paid_out", Amount: sm.Amount, Currency: sm.Currency, Region: sm.Region, Province: sm.Province,
		AccountRef: sm.AccountRef, Actor: sm.Actor, PayoutRef: sm.PayoutRef, Legs: []legView{}, OnChain: onChain(w, false)}
	if reg, ok := s.Catalog.Region(sm.Region); ok {
		if p, ok := reg.Province(sm.Province); ok {
			v.ProvinceName = p.Name
		}
	}
	for _, b := range d.shares {
		v.Legs = append(v.Legs, legView{Bank: b.Bank, BankName: b.BankName, Percent: b.Percent, RatioBps: b.RatioBps, Amount: b.Amount,
			ContractNo: b.ContractNo, Reimbursement: "pending"})
	}
	if withChain {
		v.Chain = s.chainOf(ctx, w)
	}
	return v
}

func (s *Server) chainOf(ctx context.Context, w ledger.Withdrawal) *chainView {
	c := &chainView{TxHash: w.TxHash, BlockNumber: w.BlockNumber, Timestamp: time.Unix(w.Timestamp, 0).UTC(),
		Confirmations: []ledger.Confirmation{}, VerifyURL: "/v1/withdrawals/" + w.ID + "/verify", ExplorerURL: "/explorer/#/withdrawal/" + w.ID}
	if info, err := s.Ledger.Info(ctx); err == nil {
		c.Backend, c.Contract = info.Backend, info.Contracts["WithdrawalLedger"]
	}
	if w.TxHash == "" {
		return c
	}
	c.ExplorerURL = "/explorer/#/tx/" + w.TxHash
	if d, err := s.Ledger.GetTransaction(ctx, w.TxHash); err == nil {
		c.BlockHash, c.SubmittedTo, c.SealedBy, c.SealerNodeID = d.BlockHash, d.SubmittedTo, d.Sealer, d.SealerNodeID
		c.Confirmations, c.TotalNodes = d.Confirmations, len(d.Confirmations)
		for _, cf := range d.Confirmations {
			if cf.Confirmed {
				c.ConfirmedNodes++
			}
		}
	}
	return c
}

// ---------------------------------------------------------------- handlers

func (s *Server) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	var req withdrawalReq
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	s.runWithdrawal(w, r, req, nil)
}

// runWithdrawal validates, pays, seals and records one withdrawal. tr, when set, times each stage for the demo.
func (s *Server) runWithdrawal(w http.ResponseWriter, r *http.Request, req withdrawalReq, tr *tracer) {
	v, err := s.validate(req)
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx := r.Context()
	id := withdrawalID(req.RequestID)
	actor, err := s.actorInfo(req.Actor)
	if err != nil {
		s.fail(w, err)
		return
	}
	accountRef := hashing.Ref(s.Salt, hashing.DomainAccount, strings.TrimSpace(req.UserID)+"|"+strings.TrimSpace(req.AccountNo))
	tr.mark("checked")
	reqHash := s.requestHash(v, accountRef, actor.Hash)
	// Idempotency: a retry with the same requestId returns the existing record, if it is the same withdrawal.
	if req.RequestID != "" {
		if existing, err := s.Ledger.GetWithdrawal(ctx, id); err == nil {
			s.replay(w, r, existing, reqHash)
			return
		} else if ledger.KindOf(err) != ledger.KindNotFound {
			s.fail(w, err)
			return
		}
	}
	paid, err := s.Payer.Pay(ctx, payout.Request{WithdrawalID: id, UserID: req.UserID, AccountNo: req.AccountNo, Amount: req.Amount, Currency: v.region.Currency})
	if err != nil {
		s.fail(w, errf(502, "payout_failed", "%v", err))
		return
	}
	tr.mark("paid")
	sealed, err := s.sealWithdrawal(id, v, summaryPiece{Type: "summary", WithdrawalID: id, Amount: req.Amount, Currency: v.region.Currency,
		Region: v.region.Code, Province: v.province.Code, AccountRef: accountRef, PayoutRef: paid.Ref, PaidAt: paid.PaidAt, Actor: actor,
		Banks: len(v.legs), RequestHash: reqHash})
	if err != nil {
		s.fail(w, err)
		return
	}
	tr.mark("sealed")
	tr.mark("sent")
	saved, _, err := s.Ledger.RecordWithdrawal(ctx, ledger.WithdrawalRequest{ID: id, Root: sealed.Root, Commitments: sealed.Commitments, Envelope: sealed.Raw})
	tr.mark("receipt")
	if ledger.KindOf(err) == ledger.KindDuplicate && req.RequestID != "" {
		if existing, gerr := s.Ledger.GetWithdrawal(ctx, id); gerr == nil {
			s.replay(w, r, existing, reqHash)
			return
		}
	}
	if err != nil {
		if s.Log != nil {
			s.Log.Error("withdrawal paid but not recorded on chain", "withdrawalId", id, "payoutRef", paid.Ref, "err", err)
		}
		s.fail(w, err)
		return
	}
	d, err := s.openAll(saved)
	if err != nil {
		s.fail(w, errf(500, "sealing", "recorded %s but cannot read it back with the master key: %v", id, err))
		return
	}
	out := s.view(ctx, saved, d, true)
	out.ActorRequest = req.Actor
	out.Payout = &paid
	if tr != nil {
		out.Trace = s.traceOf(ctx, tr, out.Chain)
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) replay(w http.ResponseWriter, r *http.Request, existing ledger.Withdrawal, reqHash ledger.Hash) {
	d, err := s.openAll(existing)
	if err != nil || d.summary.RequestHash != reqHash {
		writeErr(w, http.StatusConflict, "duplicate_request", "requestId was already used for a different withdrawal",
			map[string]any{"withdrawalId": existing.ID})
		return
	}
	out := s.view(r.Context(), existing, d, true)
	out.Replayed = true
	writeJSON(w, http.StatusOK, out)
}

// lookupWithdrawal accepts a withdrawal id or the hash of the transaction that recorded it.
func (s *Server) lookupWithdrawal(ctx context.Context, key string) (ledger.Withdrawal, error) {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(key, "0x") && len(key) == 66 {
		d, err := s.Ledger.GetTransaction(ctx, key)
		if err != nil {
			return ledger.Withdrawal{}, err
		}
		for _, e := range d.Events {
			if e.Name == "WithdrawalRecorded" {
				if id, ok := e.Fields["withdrawalId"].(string); ok {
					return s.Ledger.GetWithdrawal(ctx, id)
				}
			}
		}
		return ledger.Withdrawal{}, errf(404, "not_found", "transaction %s did not record a withdrawal", key)
	}
	return s.Ledger.GetWithdrawal(ctx, key)
}

// getWithdrawal returns the withdrawal decrypted with the BouyUp master key.
func (s *Server) getWithdrawal(w http.ResponseWriter, r *http.Request) {
	wd, err := s.lookupWithdrawal(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	d, err := s.openAll(wd)
	if err != nil {
		s.fail(w, errf(500, "sealing", "the master key cannot read %s: %v", wd.ID, err))
		return
	}
	writeJSON(w, 200, s.view(r.Context(), wd, d, true))
}

func pageArgs(r *http.Request, def, max int) (int, int) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	return offset, limit
}

// listWithdrawals: decrypted list (BouyUp view, token-protected).
func (s *Server) listWithdrawals(w http.ResponseWriter, r *http.Request) {
	offset, limit := pageArgs(r, 20, 100)
	items, total, err := s.Ledger.ListWithdrawals(r.Context(), offset, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []withdrawalView{}
	for _, it := range items {
		d, err := s.openAll(it)
		if err != nil {
			continue
		}
		v := s.view(r.Context(), it, d, false)
		v.Chain = &chainView{TxHash: it.TxHash, BlockNumber: it.BlockNumber, Timestamp: time.Unix(it.Timestamp, 0).UTC(),
			Confirmations: []ledger.Confirmation{}, VerifyURL: "/v1/withdrawals/" + it.ID + "/verify", ExplorerURL: "/explorer/#/withdrawal/" + it.ID}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"total": total, "offset": offset, "limit": limit, "items": out})
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type verifyResult struct {
	WithdrawalID string      `json:"withdrawalId"`
	Valid        bool        `json:"valid"`
	Checks       []check     `json:"checks"`
	Chain        *chainView  `json:"chain"`
	Root         ledger.Hash `json:"root"`
}

// verify runs the public checks (anyone can repeat them from the chain) and, with withMaster, the checks that
// need the BouyUp master key (decrypt every piece, match the commitments, re-check the 100% split).
func (s *Server) verify(ctx context.Context, wd ledger.Withdrawal, withMaster bool) verifyResult {
	res := verifyResult{WithdrawalID: wd.ID, Root: wd.Root, Chain: s.chainOf(ctx, wd)}
	add := func(name string, ok bool, detail string) { res.Checks = append(res.Checks, check{name, ok, detail}) }
	add("recorded_on_chain", true, fmt.Sprintf("block %d, written by %s", wd.BlockNumber, wd.Writer))
	rr := ledger.MerkleRoot(wd.Commitments)
	add("root_matches_commitments", rr == wd.Root, fmt.Sprintf("Merkle root of %d commitments = %s", len(wd.Commitments), rr.Hex()))
	txOK, detail := false, "transaction not found"
	if wd.TxHash != "" {
		if d, err := s.Ledger.GetTransaction(ctx, wd.TxHash); err == nil {
			txOK, detail = d.Status == "success", fmt.Sprintf("%s in block %d (%s)", d.Hash, d.BlockNumber, d.Status)
		}
	}
	add("transaction_succeeded", txOK, detail)
	if withMaster {
		d, err := s.openAll(wd)
		if err != nil {
			add("master_key_decrypts", false, err.Error())
		} else {
			add("master_key_decrypts", len(d.opened) == len(wd.Commitments), fmt.Sprintf("%d of %d pieces decrypted", len(d.opened), len(wd.Commitments)))
			okAll := true
			for _, o := range d.opened {
				okAll = okAll && o.CommitmentOK
			}
			add("pieces_match_commitments", okAll, "SHA-256 of every decrypted piece equals its on-chain commitment")
			var bps, amt int64
			for _, b := range d.shares {
				bps += b.RatioBps
				amt += b.Amount
			}
			add("ratios_total_100", bps == ledger.TotalBps, "bank ratios total "+pct(bps)+"%")
			add("amounts_total", amt == d.summary.Amount, fmt.Sprintf("bank amounts total %d of %d", amt, d.summary.Amount))
		}
	}
	res.Valid = true
	for _, c := range res.Checks {
		res.Valid = res.Valid && c.OK
	}
	// Node confirmations are reported but do not change validity: a node that is offline right now does not
	// make the record wrong.
	res.Checks = append(res.Checks, check{"confirmed_on_nodes", res.Chain.ConfirmedNodes == res.Chain.TotalNodes && res.Chain.TotalNodes > 0,
		fmt.Sprintf("%d of %d nodes hold the transaction", res.Chain.ConfirmedNodes, res.Chain.TotalNodes)})
	return res
}

func (s *Server) verifyWithdrawal(w http.ResponseWriter, r *http.Request) {
	wd, err := s.lookupWithdrawal(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, s.verify(r.Context(), wd, true))
}

// ---------------------------------------------------------------- decrypt with any key

type readablePiece struct {
	Index             int             `json:"index"`
	Kind              string          `json:"kind"` // summary | bank_share
	Content           json.RawMessage `json:"content"`
	Commitment        ledger.Hash     `json:"commitment"`
	CommitmentMatches bool            `json:"commitmentMatches"`
}

// decrypt opens a withdrawal with the key the caller gives (a bank's or the master key). The server keeps
// nothing; scripts/decrypt_withdrawal.py does the same without trusting this server.
func (s *Server) decrypt(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WithdrawalID string `json:"withdrawalId"`
		PrivateKey   string `json:"privateKey"`
	}
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if req.WithdrawalID == "" || req.PrivateKey == "" {
		s.fail(w, errf(400, "invalid", "withdrawalId and privateKey are required"))
		return
	}
	wd, err := s.lookupWithdrawal(r.Context(), req.WithdrawalID)
	if err != nil {
		s.fail(w, err)
		return
	}
	opened, err := sealing.Open(wd.ID, wd.Envelope, req.PrivateKey, wd.Commitments)
	if err == sealing.ErrNoSlot {
		s.fail(w, errf(403, "no_access", "this key cannot read withdrawal %s (it opens none of its %d slots)", wd.ID, countSlots(wd)))
		return
	}
	if err != nil {
		s.fail(w, errf(400, "invalid_key", "%v", err))
		return
	}
	out := []readablePiece{}
	for _, o := range opened {
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(o.Plaintext, &probe)
		out = append(out, readablePiece{Index: o.Index, Kind: probe.Type, Content: o.Plaintext, Commitment: o.Commitment, CommitmentMatches: o.CommitmentOK})
	}
	writeJSON(w, 200, map[string]any{"withdrawalId": wd.ID, "piecesTotal": len(wd.Commitments), "readable": out,
		"root": wd.Root, "rootMatchesCommitments": ledger.MerkleRoot(wd.Commitments) == wd.Root})
}

func countSlots(wd ledger.Withdrawal) int {
	_, n, _ := sealing.Counts(wd.Envelope)
	return n
}
