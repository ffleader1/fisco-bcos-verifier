package api

import (
	"net/http"

	"sellergraph/fisco/internal/ledger"
)

// Portfolio is computed from on-chain state (PoC scale: scans all loans).
type Portfolio struct {
	Loans                int64                     `json:"loans"`
	ByStatus             map[string]int64          `json:"byStatus"`
	ByAlert              map[string]int64          `json:"byAlert"`
	ByTier               map[string]TierStats      `json:"byTier"`
	PrincipalDisbursed   int64                     `json:"principalDisbursed"`
	PrincipalOutstanding int64                     `json:"principalOutstanding"`
	InterestAccrued      int64                     `json:"interestAccrued"`
	Overdue              int64                     `json:"overdue"`
	TotalRepaid          int64                     `json:"totalRepaid"`
	RefundsDue           int64                     `json:"refundsDue"`
	FunderExposure       map[string]FunderExposure `json:"funderExposure"`
	PendingHumanReview   []string                  `json:"pendingHumanReview"`
	RedAlerts            []string                  `json:"redAlerts"`
}

type TierStats struct {
	Loans       int64 `json:"loans"`
	Outstanding int64 `json:"outstanding"`
}

// FunderExposure is the pro-rata share of outstanding principal of each funder (multi-bank mode).
type FunderExposure struct {
	Address     string `json:"address"`
	Committed   int64  `json:"committed"`
	Outstanding int64  `json:"outstanding"`
}

func (s *Server) portfolio(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ids, _, err := s.Ledger.ListLoanIDs(ctx, 0, 0)
	if err != nil {
		s.fail(w, err)
		return
	}
	p := Portfolio{ByStatus: map[string]int64{}, ByAlert: map[string]int64{}, ByTier: map[string]TierStats{},
		FunderExposure: map[string]FunderExposure{}, PendingHumanReview: []string{}, RedAlerts: []string{}}
	for _, id := range ids {
		l, err := s.Ledger.GetLoan(ctx, id)
		if err != nil {
			s.fail(w, err)
			return
		}
		p.Loans++
		p.ByStatus[l.Status.String()]++
		if l.DisbursedAt > 0 {
			p.PrincipalDisbursed += l.Principal
		}
		live := l.Status == ledger.StatusActive || l.Status == ledger.StatusAccelerated || l.Status == ledger.StatusDefaulted
		if live {
			p.ByAlert[l.AlertLevel.String()]++
			p.PrincipalOutstanding += l.PrincipalOutstanding
			p.InterestAccrued += l.InterestAccrued
			p.Overdue += l.OverduePrincipal + l.OverdueInterest
			if l.AlertLevel == ledger.AlertRed {
				p.RedAlerts = append(p.RedAlerts, id)
			}
		}
		p.TotalRepaid += l.TotalRepaid
		p.RefundsDue += l.RefundDue
		tier := map[uint8]string{1: "tier1_working_capital", 2: "tier2_growth_capital"}[l.Tier]
		ts := p.ByTier[tier]
		ts.Loans++
		if live {
			ts.Outstanding += l.PrincipalOutstanding
		}
		p.ByTier[tier] = ts
		if l.Status == ledger.StatusCreated && l.HumanReviewRequired && !l.OfficerApproved {
			p.PendingHumanReview = append(p.PendingHumanReview, id)
		}
		for _, f := range l.Funding {
			name := s.funderName(f.Funder)
			if name == "" {
				name = f.Funder
			}
			fe := p.FunderExposure[name]
			fe.Address = f.Funder
			if l.Status != ledger.StatusCancelled {
				fe.Committed += f.Amount
			}
			if live && l.Principal > 0 {
				fe.Outstanding += l.PrincipalOutstanding * f.Amount / l.Principal
			}
			p.FunderExposure[name] = fe
		}
	}
	writeJSON(w, 200, p)
}
