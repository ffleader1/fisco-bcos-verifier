package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"buoyup/fisco/internal/ledger"
)

// The overview's step-by-step demo: a manual withdrawal sent from the page, timed stage by stage, plus when each
// chain node wrote the block. It exists only while showDemoKeys is on, every record it makes is marked DEMO, and
// its amounts are capped because whatever it writes stays on the chain for good.

// demoCaps are the largest demo amounts, in minor units of each currency.
var demoCaps = map[string]int64{"VND": 10_000_000, "CNY": 5_000_000} // 10,000,000 VND; 50,000.00 CNY

const demoReason = "Demo: manual withdrawal sent from the overview page"

type demoReq struct {
	Amount    int64                  `json:"amount"` // minor units, like POST /v1/withdrawals
	Region    string                 `json:"region"`
	Province  string                 `json:"province"`
	BankSplit map[string]json.Number `json:"bankSplit"`
	Note      string                 `json:"note,omitempty"` // optional, appended to the reason
}

// tracer records when each stage of a withdrawal finished.
type tracer struct {
	start time.Time
	marks []traceStep
	at    map[string]time.Time
}

func newTracer() *tracer { return &tracer{start: time.Now(), at: map[string]time.Time{}} }

func (t *tracer) mark(step string) {
	if t == nil {
		return
	}
	now := time.Now()
	t.at[step] = now
	t.marks = append(t.marks, traceStep{Step: step, AtMs: t.ms(now)})
}

func (t *tracer) ms(at time.Time) float64 {
	return math.Round(float64(at.Sub(t.start).Microseconds())/100) / 10
}

type traceStep struct {
	Step string  `json:"step"` // checked | paid | sealed | sent | receipt
	AtMs float64 `json:"atMs"` // since the request arrived
}

type traceNode struct {
	Node   string   `json:"node"`
	Seen   bool     `json:"seen"`             // false: the node did not report this block within the wait
	AtMs   *float64 `json:"atMs,omitempty"`   // when its notice arrived, since the request arrived
	Order  int      `json:"order,omitempty"`  // 1 = first to report
	Leader bool     `json:"leader,omitempty"` // proposed this block
}

type traceView struct {
	Backend     string      `json:"backend"`
	Steps       []traceStep `json:"steps"`
	SubmittedTo string      `json:"submittedTo"`
	Leader      string      `json:"leader"`
	BlockNumber int64       `json:"blockNumber"`
	Nodes       []traceNode `json:"nodes"`  // in the order they reported; nodes not heard from last
	Quorum      int         `json:"quorum"` // votes PBFT needs: n - f, with n = 3f + 1
	Measured    string      `json:"measured"`
}

func quorum(n int) int {
	if n <= 1 {
		return n
	}
	return n - (n-1)/3
}

func (s *Server) traceOf(ctx context.Context, tr *tracer, c *chainView) *traceView {
	v := &traceView{Steps: tr.marks, Nodes: []traceNode{},
		Measured: "steps: timed by this service; nodes: when each node's block notice reached this service (loopback latency included)"}
	if c == nil {
		return v
	}
	v.Backend, v.BlockNumber, v.Leader = c.Backend, c.BlockNumber, c.SealedBy
	v.SubmittedTo = strings.TrimSpace(strings.SplitN(c.SubmittedTo, " ", 2)[0])
	if cw, ok := s.Ledger.(ledger.CommitWatcher); ok {
		for _, nc := range cw.BlockCommits(ctx, c.BlockNumber, 2*time.Second) {
			tn := traceNode{Node: nc.Node, Seen: nc.Seen, Leader: nc.Node == v.Leader}
			if nc.Seen {
				ms := tr.ms(nc.At)
				tn.AtMs = &ms
			}
			v.Nodes = append(v.Nodes, tn)
		}
	}
	sort.SliceStable(v.Nodes, func(i, j int) bool {
		a, b := v.Nodes[i], v.Nodes[j]
		if a.Seen != b.Seen {
			return a.Seen
		}
		if a.Seen {
			return *a.AtMs < *b.AtMs
		}
		return a.Node < b.Node
	})
	for i := range v.Nodes {
		if v.Nodes[i].Seen {
			v.Nodes[i].Order = i + 1
		}
	}
	v.Quorum = quorum(len(v.Nodes))
	return v
}

// demoWithdrawal is POST /v1/demo/withdrawals.
func (s *Server) demoWithdrawal(w http.ResponseWriter, r *http.Request) {
	if !s.ShowDemoKeys {
		s.fail(w, errf(404, "demo_off", "the demo is off on this server (showDemoKeys is false)"))
		return
	}
	var dr demoReq
	if err := decode(r, &dr); err != nil {
		s.fail(w, err)
		return
	}
	note := strings.TrimSpace(dr.Note)
	if utf8.RuneCountInString(note) > 120 {
		s.fail(w, errf(400, "invalid_note", "note is at most 120 characters"))
		return
	}
	reason := demoReason
	if note != "" {
		reason += ": " + note
	}
	req := withdrawalReq{UserID: "DEMO-USER", AccountNo: "DEMO-000000", Amount: dr.Amount, Region: dr.Region, Province: dr.Province,
		BankSplit: dr.BankSplit, Actor: &actorReq{Type: ledger.ActorHuman, StaffID: "DEMO", Role: "demo", Department: "Overview demo", Reason: reason}}
	tr := newTracer()
	v, err := s.validate(req)
	if err != nil {
		s.fail(w, err)
		return
	}
	if limit, ok := demoCaps[v.region.Currency]; ok && dr.Amount > limit {
		s.fail(w, errf(400, "demo_amount_too_large", "demo amounts are capped at %d (minor units of %s): demo records stay on the chain for good",
			limit, v.region.Currency))
		return
	}
	s.runWithdrawal(w, r, req, tr)
}
