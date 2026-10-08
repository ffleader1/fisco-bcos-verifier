package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"sellergraph/fisco/internal/api"
	"sellergraph/fisco/internal/ledger"
	"sellergraph/fisco/internal/ledger/mock"
)

const t0 = int64(1_790_000_000)

func newServer(t *testing.T, token string) (*httptest.Server, *int64) {
	t.Helper()
	now := t0
	acc := ledger.Accounts{Admin: "0x00000000000000000000000000000000000000a1", Operator: "0x00000000000000000000000000000000000000b2",
		Officer: "0x00000000000000000000000000000000000000c3", Monitor: "0x00000000000000000000000000000000000000d4",
		Funders: map[string]string{"webank": "0x00000000000000000000000000000000000000b2", "vcb": "0x00000000000000000000000000000000000000e5"}}
	m := mock.New(acc, mock.WithClock(func() int64 { return now }))
	if err := ledger.Bootstrap(context.Background(), m, acc, ledger.Policy{MaxRateBps: 2400, OfficerThreshold: 50_000_000, MaxFutureSkewSec: 300}); err != nil {
		t.Fatal(err)
	}
	s := &api.Server{Ledger: m, Salt: []byte("test-salt-0123456789"), Funders: acc.Funders, DefaultFunder: "webank", Token: token}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, &now
}

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (c client) ok(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	code, out := c.do(method, path, body)
	if code != want {
		c.t.Fatalf("%s %s: status %d want %d: %v", method, path, code, want, out)
	}
	return out
}

func loanField(out map[string]any, k string) any { return out["loan"].(map[string]any)[k] }

func TestEndToEndFlow(t *testing.T) {
	ts, now := newServer(t, "secret")
	c := client{t: t, base: ts.URL, token: "secret"}

	// auth
	if code, _ := (client{t: t, base: ts.URL}).do("GET", "/v1/loans", nil); code != 401 {
		t.Fatalf("expected 401 without token, got %d", code)
	}
	c.ok("GET", "/healthz", nil, 200)

	decision := map[string]any{"doc": map[string]any{
		"pd": 0.031, "segment": "N1", "limitCeiling": 12000000, "rateBps": 1200, "sweepRateBps": 4300,
		"agent2Memo": "memo-hash-abc", "agent3Challenge": "challenge-hash-def", "checks": []string{"price_deviation_ok", "supplier_checks_4_of_4"}},
		"uri": "s3://webank-sg/decisions/LN-2026-0001.json"}
	out := c.ok("POST", "/v1/loans", map[string]any{
		"loanId": "LN-2026-0001", "sellerId": "seller-douyin-778899", "tier": 2, "principal": 10000000,
		"annualRateBps": 1200, "tenorDays": 120, "sweepRateBps": 4300, "sweepCapBps": 6300, "graceDays": 20, "decision": decision,
	}, 201)
	if loanField(out, "status") != "approved" || out["decisionAnchor"] == nil {
		t.Fatalf("create: %v", out)
	}
	if f := loanField(out, "funding").([]any)[0].(map[string]any); f["name"] != "webank" || f["confirmed"] != true {
		t.Fatalf("funding view: %v", f)
	}
	// retry with the same decision -> loan duplicate (decision anchor duplicate is tolerated)
	code, e := c.do("POST", "/v1/loans", map[string]any{"loanId": "LN-2026-0001", "sellerId": "x", "tier": 2, "principal": 10000000,
		"annualRateBps": 1200, "tenorDays": 120, "sweepRateBps": 4300, "sweepCapBps": 6300, "graceDays": 20, "decision": decision})
	if code != 409 || e["error"].(map[string]any)["kind"] != "duplicate" {
		t.Fatalf("expected 409 duplicate, got %d %v", code, e)
	}

	// legacy alias from the architecture diagram
	out = c.ok("POST", "/blockchain/disburse", map[string]any{"loanId": "LN-2026-0001", "bankTxRef": "WB202610040001", "deliveryDays": 10,
		"payments": []map[string]any{{"supplierId": "91310000MA1FL0001X", "amount": 6000000}, {"supplierId": "91440300MA5F00002Y", "amount": 4000000}}}, 200)
	if loanField(out, "status") != "active" {
		t.Fatalf("disburse: %v", out)
	}

	*now += 22 * 86400
	out = c.ok("POST", "/v1/loans/LN-2026-0001/repayments", map[string]any{"amount": 2150000, "source": "proportional_sweep",
		"extRef": "SWEEP-0001", "notice": map[string]any{"text": "43% of payout 5,000,000 swept"}}, 200)
	alloc := out["allocation"].(map[string]any)
	// 10,000,000 * 12% * 22/365 = 72,328.7 -> 72,328
	if alloc["toInterest"].(float64) != 72328 || alloc["toPrincipal"].(float64) != 2150000-72328 {
		t.Fatalf("allocation %v", alloc)
	}
	code, e = c.do("POST", "/repayment", map[string]any{"loanId": "LN-2026-0001", "amount": 1, "extRef": "SWEEP-0001"})
	if code != 409 {
		t.Fatalf("idempotency: %d %v", code, e)
	}

	out = c.ok("POST", "/v1/loans/LN-2026-0001/floor-checkpoints", map[string]any{"floorCumulative": 3000000}, 200)
	if out["shortfall"].(float64) != 850000 || loanField(out, "alertLevel") != "yellow" {
		t.Fatalf("floor: %v", out)
	}
	out = c.ok("POST", "/v1/loans/LN-2026-0001/deliveries", map[string]any{"line": 0, "waybillNo": "SF1234567890", "shippedAt": t0 + 3*86400}, 200)
	if out["onTime"] != true {
		t.Fatalf("delivery: %v", out)
	}
	c.ok("POST", "/v1/loans/LN-2026-0001/deliveries/1/missing", map[string]any{}, 200)
	out = c.ok("GET", "/v1/loans/LN-2026-0001", nil, 200)
	if out["alertLevel"] != "red" {
		t.Fatalf("expected red after missing delivery: %v", out["alertLevel"])
	}
	c.ok("POST", "/v1/loans/LN-2026-0001/alerts", map[string]any{"level": "yellow", "code": "SALES_DROP_SEASONAL",
		"evidence": map[string]any{"doc": map[string]any{"agent": 5, "cause": "seasonal"}}}, 200)

	pay := c.ok("GET", fmt.Sprintf("/v1/loans/LN-2026-0001/payoff?asOf=%d", *now+60), nil, 200)
	if pay["total"].(float64) <= 0 {
		t.Fatalf("payoff %v", pay)
	}
	out = c.ok("POST", "/v1/loans/LN-2026-0001/repayments", map[string]any{"amount": pay["total"], "source": "prepayment", "extRef": "PREPAY-1", "asOf": *now + 60}, 200)
	if out["settled"] != true || loanField(out, "status") != "settled" {
		t.Fatalf("prepayment should settle: %v", out)
	}

	// officer gate on a large tier-2 loan, multi-funder
	c.ok("POST", "/v1/loans", map[string]any{"loanId": "LN-2026-0002", "sellerId": "seller-2", "tier": 2, "principal": 60000000,
		"annualRateBps": 1000, "tenorDays": 150, "sweepRateBps": 3000, "sweepCapBps": 6000, "graceDays": 30, "decision": map[string]any{"text": "pkg-2"},
		"funding": []map[string]any{{"funder": "webank", "amount": 40000000}, {"funder": "vcb", "amount": 20000000}}}, 201)
	code, _ = c.do("POST", "/v1/loans/LN-2026-0002/disburse", map[string]any{"bankTxRef": "X", "payments": []map[string]any{{"supplierId": "s", "amount": 60000000}}})
	if code != 409 {
		t.Fatalf("disbursement before sign-off must be 409, got %d", code)
	}
	c.ok("POST", "/v1/loans/LN-2026-0002/funding/confirm", map[string]any{"funder": "vcb"}, 200)
	out = c.ok("POST", "/v1/loans/LN-2026-0002/officer-decision", map[string]any{"approved": true,
		"review": map[string]any{"doc": map[string]any{"officer": "credit-officer-17", "anomaliesConfirmed": []string{"price +12% vs 3m"}}}}, 200)
	if loanField(out, "status") != "approved" || out["reviewAnchor"] == nil {
		t.Fatalf("officer approval: %v", out)
	}

	pf := c.ok("GET", "/portfolio", nil, 200)
	if pf["loans"].(float64) != 2 || pf["byStatus"].(map[string]any)["settled"].(float64) != 1 {
		t.Fatalf("portfolio %v", pf)
	}
	fe := pf["funderExposure"].(map[string]any)["vcb"].(map[string]any)
	if fe["committed"].(float64) != 20000000 {
		t.Fatalf("vcb exposure %v", fe)
	}

	// audit trail: verify chain and content
	out = c.ok("GET", "/v1/audit/anchors/LN-2026-0001/verify", nil, 200)
	if out["valid"] != true || out["records"].(float64) != 2 {
		t.Fatalf("verify chain: %v", out)
	}
	out = c.ok("POST", "/v1/audit/verify", map[string]any{"content": decision}, 200)
	if out["found"] != true || out["record"].(map[string]any)["kind"] != "decision_package" {
		t.Fatalf("verify content: %v", out)
	}
	tampered := map[string]any{"doc": map[string]any{"pd": 0.01}}
	out = c.ok("POST", "/v1/audit/verify", map[string]any{"content": tampered}, 200)
	if out["found"] != false {
		t.Fatalf("tampered doc must not verify")
	}
	c.ok("POST", "/v1/audit/anchors", map[string]any{"subject": "APP-77", "kind": "ml_output", "content": map[string]any{"doc": map[string]any{"pd": 0.2}}}, 201)
	list := c.ok("GET", "/v1/audit/anchors/APP-77", nil, 200)
	if len(list["records"].([]any)) != 1 {
		t.Fatalf("anchors: %v", list)
	}
}

func TestErrorMapping(t *testing.T) {
	ts, _ := newServer(t, "")
	c := client{t: t, base: ts.URL}
	cases := []struct {
		method, path string
		body         any
		code         int
	}{
		{"GET", "/v1/loans/NOPE", nil, 404},
		{"GET", "/v1/loans/bad%20id", nil, 400},
		{"POST", "/v1/loans", map[string]any{"loanId": "A", "sellerId": "s", "tier": 1, "principal": 100, "annualRateBps": 9999,
			"tenorDays": 30, "sweepRateBps": 1, "sweepCapBps": 2, "decision": map[string]any{"text": "d"}}, 400},
		{"POST", "/v1/loans", map[string]any{"loanId": "A", "tier": 1}, 400},
		{"POST", "/v1/loans", map[string]any{"unknownField": 1}, 400},
		{"POST", "/v1/loans/A/repayments", map[string]any{"amount": 1, "source": "bogus", "extRef": "x"}, 400},
		{"POST", "/repayment", map[string]any{"amount": 1}, 400},
	}
	for _, cs := range cases {
		if code, out := c.do(cs.method, cs.path, cs.body); code != cs.code {
			t.Errorf("%s %s: got %d want %d (%v)", cs.method, cs.path, code, cs.code, out)
		}
	}
}
