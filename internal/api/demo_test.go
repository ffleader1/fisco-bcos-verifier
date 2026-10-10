package api_test

import (
	"testing"

	"bouyup/fisco/internal/api"
)

func demoBody(amount int64) map[string]any {
	return map[string]any{"amount": amount, "region": "VN", "province": "VN-HN",
		"bankSplit": map[string]any{"VCB": 60, "BIDV": 25.5, "TCB": 14.5}, "note": "team walkthrough"}
}

func TestDemoWithdrawal(t *testing.T) {
	ts := newServer(t, "secret")
	c := client{t: t, base: ts.URL, token: "secret"}
	if code, _ := (client{t: t, base: ts.URL}).do("POST", "/v1/demo/withdrawals", demoBody(1_000_000)); code != 401 {
		t.Fatalf("the demo endpoint is under /v1 and needs the token, got %d", code)
	}
	c.errKind("POST", "/v1/demo/withdrawals", demoBody(10_000_001), 400, "demo_amount_too_large")
	c.errKind("POST", "/v1/demo/withdrawals", with(demoBody(1_000_000), "bankSplit", map[string]any{"VCB": 60}), 400, "invalid_bank_ratio")

	out := c.ok("POST", "/v1/demo/withdrawals", demoBody(1_000_000), 201)
	actor := out["actor"].(map[string]any)
	if actor["type"] != "human" || actor["role"] != "demo" {
		t.Fatalf("demo withdrawals must be marked as a human DEMO actor: %v", actor)
	}
	tr, ok := out["trace"].(map[string]any)
	if !ok {
		t.Fatalf("missing trace: %v", out)
	}
	steps := []string{}
	for _, s := range tr["steps"].([]any) {
		steps = append(steps, s.(map[string]any)["step"].(string))
	}
	if toJSON(steps) != `["checked","paid","sealed","sent","receipt"]` {
		t.Fatalf("steps = %v", steps)
	}
	nodes := tr["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["seen"] != true || tr["leader"] != "node0" || tr["quorum"].(float64) != 1 {
		t.Fatalf("mock trace should show one node that wrote the block and led it: %v", tr)
	}
	// the record is a normal withdrawal: readable with the master key, listed, verifiable
	id := out["withdrawalId"].(string)
	v := c.ok("GET", "/v1/withdrawals/"+id+"/verify", nil, 200)
	if v["valid"] != true {
		t.Fatalf("demo withdrawal does not verify: %v", v)
	}
}

func TestDemoOffWithoutDemoKeys(t *testing.T) {
	ts := newServer(t, "", func(s *api.Server) { s.ShowDemoKeys = false })
	c := client{t: t, base: ts.URL}
	c.errKind("POST", "/v1/demo/withdrawals", demoBody(1_000_000), 404, "demo_off")
}
