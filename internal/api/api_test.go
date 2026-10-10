package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bouyup/fisco/internal/api"
	"bouyup/fisco/internal/catalog"
	"bouyup/fisco/internal/keystore"
	"bouyup/fisco/internal/ledger"
	"bouyup/fisco/internal/ledger/mock"
	"bouyup/fisco/internal/payout"
)

var testNow = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)

func newServer(t *testing.T, token string, opts ...func(*api.Server)) *httptest.Server {
	t.Helper()
	acc := ledger.Accounts{Admin: "0x00000000000000000000000000000000000000a1", Operator: "0x00000000000000000000000000000000000000b2"}
	m := mock.New(acc, mock.WithClock(func() int64 { return testNow.Unix() }))
	if err := ledger.Bootstrap(context.Background(), m, acc); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return testNow }
	var codes []string
	for _, b := range cat.Banks("") {
		codes = append(codes, b.Code)
	}
	keys, err := keystore.Open("", codes, testNow)
	if err != nil {
		t.Fatal(err)
	}
	testKeys = keys
	s := &api.Server{Ledger: m, Catalog: cat, Payer: payout.Mock{Now: now}, Keys: keys, ShowDemoKeys: true,
		ChainKeys: map[string]string{"admin": "0xaa", "operator": "0xbb"}, Salt: []byte("test-salt-0123456789"), Token: token, Now: now}
	for _, o := range opts {
		o(s)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// testKeys is the key store of the last server created (tests run one server at a time).
var testKeys *keystore.Store

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

// errKind asserts an error response.
func (c client) errKind(method, path string, body any, wantCode int, wantKind string) map[string]any {
	c.t.Helper()
	code, out := c.do(method, path, body)
	e, _ := out["error"].(map[string]any)
	if code != wantCode || e == nil || e["kind"] != wantKind {
		c.t.Fatalf("%s %s: got %d %v, want %d %s", method, path, code, out, wantCode, wantKind)
	}
	return e
}

func agent4() map[string]any {
	return map[string]any{"type": "agent", "agentId": "agent-4", "name": "Disbursement & Collection", "version": "4.2.0", "model": "claude-opus-5-5", "runId": "run-1"}
}

func vnWithdrawal() map[string]any {
	return map[string]any{
		"userId": "U-778899", "accountNo": "0071000123456", "amount": 500000000, "region": "VN", "province": "VN-HN",
		"bankSplit": map[string]any{"VCB": 60, "BIDV": 25.5, "TCB": 14.5}, "actor": agent4(),
	}
}

func with(base map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

func legs(out map[string]any) []map[string]any {
	var l []map[string]any
	for _, x := range out["legs"].([]any) {
		l = append(l, x.(map[string]any))
	}
	return l
}

func TestWithdrawalFlow(t *testing.T) {
	ts := newServer(t, "secret")
	c := client{t: t, base: ts.URL, token: "secret"}
	if code, _ := (client{t: t, base: ts.URL}).do("POST", "/v1/withdrawals", vnWithdrawal()); code != 401 {
		t.Fatalf("expected 401 without token, got %d", code)
	}

	out := c.ok("POST", "/v1/withdrawals", vnWithdrawal(), 201)
	id := out["withdrawalId"].(string)
	if !strings.HasPrefix(id, "WD-") || out["status"] != "paid_out" || out["currency"] != "VND" || out["provinceName"] != "Hà Nội" {
		t.Fatalf("withdrawal: %v", out)
	}
	l := legs(out)
	if len(l) != 3 || l[0]["bank"] != "VCB" || l[0]["amount"].(float64) != 300000000 || l[1]["bank"] != "BIDV" || l[1]["amount"].(float64) != 127500000 ||
		l[1]["ratioBps"].(float64) != 2550 || l[2]["amount"].(float64) != 72500000 || l[0]["reimbursement"] != "pending" || l[0]["contractNo"] != "BU-VCB-2026-01" {
		t.Fatalf("legs: %v", l)
	}
	if strings.Contains(toJSON(out), "0071000123456") || strings.Contains(toJSON(out), "U-778899") {
		t.Fatal("the account number must not appear in the record")
	}
	chain := out["chain"].(map[string]any)
	tx := chain["txHash"].(string)
	if !strings.HasPrefix(tx, "0x") || chain["blockNumber"].(float64) <= 0 || chain["confirmedNodes"].(float64) != 1 || chain["totalNodes"].(float64) != 1 ||
		chain["contract"] == "" || chain["explorerUrl"] != "/explorer/#/tx/"+tx {
		t.Fatalf("chain: %v", chain)
	}
	actor := out["actor"].(map[string]any)
	if actor["type"] != "agent" || actor["agentId"] != "agent-4" || actor["version"] != "4.2.0" || out["actorRequest"].(map[string]any)["runId"] != "run-1" {
		t.Fatalf("actor: %v", actor)
	}
	oc := out["onChain"].(map[string]any)
	if oc["pieces"].(float64) != 4 || oc["slots"].(float64) != 4 || len(oc["commitments"].([]any)) != 4 || !strings.HasPrefix(oc["root"].(string), "0x") {
		t.Fatalf("onChain: %v", oc)
	}
	if out["payout"].(map[string]any)["provider"] != "mock-treasury" || out["payoutRef"] != "MOCKPAY-"+strings.TrimPrefix(id, "WD-") {
		t.Fatalf("payout: %v", out)
	}

	// read back by id and by transaction hash
	got := c.ok("GET", "/v1/withdrawals/"+id, nil, 200)
	if got["onChain"].(map[string]any)["root"] != oc["root"] || got["actorRequest"] != nil || got["amount"].(float64) != 500000000 || len(legs(got)) != 3 {
		t.Fatalf("get: %v", got)
	}
	if byTx := c.ok("GET", "/v1/withdrawals/"+tx, nil, 200); byTx["withdrawalId"] != id {
		t.Fatalf("by tx: %v", byTx)
	}
	v := c.ok("GET", "/v1/withdrawals/"+id+"/verify", nil, 200)
	if v["valid"] != true || len(v["checks"].([]any)) != 8 {
		t.Fatalf("verify: %v", v)
	}
	list := c.ok("GET", "/v1/withdrawals?limit=5", nil, 200)
	if list["total"].(float64) != 1 || len(list["items"].([]any)) != 1 {
		t.Fatalf("list: %v", list)
	}
	c.errKind("GET", "/v1/withdrawals/WD-NOPE", nil, 404, "not_found")
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestWithdrawalHumanCN(t *testing.T) {
	c := client{t: t, base: newServer(t, "").URL}
	human := map[string]any{"type": "human", "staffId": "CO-17", "role": "credit_officer", "department": "SME Lending Ops",
		"reason": "Manual payout: agent run failed after approval", "approvalRef": "TICKET-4471"}
	out := c.ok("POST", "/v1/withdrawals", map[string]any{"userId": "U-1", "accountNo": "6222020200112233", "amount": 13990000,
		"currency": "CNY", "region": "CN", "province": "CN-GD", "bankSplit": map[string]any{"WEBANK": 100}, "actor": human}, 201)
	a := out["actor"].(map[string]any)
	if a["type"] != "human" || a["role"] != "credit_officer" || !strings.HasPrefix(a["staffRef"].(string), "0x") || strings.Contains(toJSON(a["staffRef"]), "CO-17") {
		t.Fatalf("human actor: %v", a)
	}
	if l := legs(out); len(l) != 1 || l[0]["amount"].(float64) != 13990000 {
		t.Fatalf("legs: %v", l)
	}
	// human actors need a reason
	h2 := with(human, "reason", "")
	c.errKind("POST", "/v1/withdrawals", with(vnWithdrawal(), "actor", h2), 400, "invalid_actor")
}

func TestWithdrawalValidation(t *testing.T) {
	c := client{t: t, base: newServer(t, "").URL}
	base := vnWithdrawal()
	split := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return with(base, "bankSplit", m)
	}
	e := c.errKind("POST", "/v1/withdrawals", split("VCB", 60, "TCB", 30), 400, "invalid_bank_ratio")
	if !strings.Contains(e["message"].(string), "90.00%") || e["details"].(map[string]any)["sumPercent"] != "90.00" {
		t.Fatalf("sum message: %v", e)
	}
	c.errKind("POST", "/v1/withdrawals", split("VCB", 60, "TCB", 40.01), 400, "invalid_bank_ratio")
	e = c.errKind("POST", "/v1/withdrawals", split("VCB", 66.667, "TCB", 33.333), 400, "invalid_bank_ratio")
	if !strings.Contains(e["message"].(string), "more than 2 decimals") {
		t.Fatalf("decimals message: %v", e)
	}
	c.errKind("POST", "/v1/withdrawals", split("VCB", 100, "TCB", 0), 400, "invalid_bank_ratio")
	c.errKind("POST", "/v1/withdrawals", split("VCB", 110, "TCB", -10), 400, "invalid_bank_ratio")
	c.errKind("POST", "/v1/withdrawals", split("VCB", "abc"), 400, "invalid")
	c.errKind("POST", "/v1/withdrawals", with(base, "bankSplit", map[string]any{}), 400, "invalid_bank_ratio")
	c.errKind("POST", "/v1/withdrawals", split("VCB", 50, "XYZ", 50), 400, "unknown_bank")
	c.errKind("POST", "/v1/withdrawals", split("VCB", 50, "ICBC", 50), 400, "bank_region_mismatch")
	c.errKind("POST", "/v1/withdrawals", with(base, "province", "CN-GD"), 400, "unknown_province")
	c.errKind("POST", "/v1/withdrawals", with(base, "region", "TH"), 400, "unknown_region")
	c.errKind("POST", "/v1/withdrawals", with(base, "currency", "CNY"), 400, "currency_mismatch")
	c.errKind("POST", "/v1/withdrawals", with(base, "amount", 0), 400, "invalid_amount")
	c.errKind("POST", "/v1/withdrawals", with(base, "accountNo", ""), 400, "invalid")
	c.errKind("POST", "/v1/withdrawals", with(base, "actor", nil), 400, "invalid_actor")
	c.errKind("POST", "/v1/withdrawals", with(base, "actor", with(agent4(), "agentId", "agent-3")), 403, "actor_not_allowed")
	c.errKind("POST", "/v1/withdrawals", with(base, "actor", with(agent4(), "version", "")), 400, "invalid_actor")
	c.errKind("POST", "/v1/withdrawals", with(base, "actor", map[string]any{"type": "robot"}), 400, "invalid")
	c.errKind("POST", "/v1/withdrawals", with(base, "amount", 1, "bankSplit", map[string]any{"VCB": 50, "TCB": 50}), 400, "amount_too_small")
	c.errKind("POST", "/v1/withdrawals", with(base, "surprise", true), 400, "invalid")

	// rounding: the remainder goes to the largest share, the legs always add up
	out := c.ok("POST", "/v1/withdrawals", with(base, "amount", 100, "bankSplit", map[string]any{"VCB": 33.33, "TCB": 33.33, "BIDV": 33.34}), 201)
	l := legs(out)
	if l[0]["bank"] != "BIDV" || l[0]["amount"].(float64) != 34 || l[1]["amount"].(float64) != 33 || l[2]["amount"].(float64) != 33 {
		t.Fatalf("rounding: %v", l)
	}
	// decimals given as strings and integers given as 60.00 are fine
	c.ok("POST", "/v1/withdrawals", with(base, "bankSplit", map[string]any{"vcb": "60.00", "TCB": "40"}), 201)
}

func TestWithdrawalIdempotency(t *testing.T) {
	c := client{t: t, base: newServer(t, "").URL}
	req := with(vnWithdrawal(), "requestId", "agent4-req-0001")
	first := c.ok("POST", "/v1/withdrawals", req, 201)
	again := c.ok("POST", "/v1/withdrawals", req, 200)
	if again["withdrawalId"] != first["withdrawalId"] || again["replayed"] != true || again["chain"].(map[string]any)["txHash"] != first["chain"].(map[string]any)["txHash"] {
		t.Fatalf("replay: %v", again)
	}
	e := c.errKind("POST", "/v1/withdrawals", with(req, "amount", 1000), 409, "duplicate_request")
	if e["details"].(map[string]any)["withdrawalId"] != first["withdrawalId"] {
		t.Fatalf("conflict details: %v", e)
	}
	if list := c.ok("GET", "/v1/withdrawals", nil, 200); list["total"].(float64) != 1 {
		t.Fatalf("a replay must not record twice: %v", list)
	}
}

func TestBanksAndRegions(t *testing.T) {
	c := client{t: t, base: newServer(t, "").URL}
	regs := c.ok("GET", "/v1/regions", nil, 200)["regions"].([]any)
	if len(regs) != 2 {
		t.Fatalf("regions: %v", regs)
	}
	vn := c.ok("GET", "/v1/banks?region=VN", nil, 200)["banks"].([]any)
	if len(vn) != 5 || vn[0].(map[string]any)["contractStatus"] != "active" {
		t.Fatalf("VN banks: %v", vn)
	}
	c.errKind("GET", "/v1/banks?region=XX", nil, 400, "unknown_region")
	all := c.ok("GET", "/v1/banks", nil, 200)["banks"].([]any)
	if len(all) != 10 {
		t.Fatalf("all banks: %d", len(all))
	}
	b := c.ok("GET", "/v1/banks/vcb", nil, 200)
	pol := b["policy"].(map[string]any)
	if b["code"] != "VCB" || b["mock"] != true || b["currency"] != "VND" || pol["summary"] == "" || !strings.HasPrefix(pol["hash"].(string), "0x") ||
		b["limits"].(map[string]any)["maxSharePercent"].(float64) != 70 || b["contract"].(map[string]any)["contractNo"] != "BU-VCB-2026-01" {
		t.Fatalf("bank: %v", b)
	}
	p := c.ok("GET", "/v1/banks/AGRI/policy", nil, 200)
	if p["policy"].(map[string]any)["sectors"].(map[string]any)["prefer"] == nil || p["disclaimer"] == "" {
		t.Fatalf("policy: %v", p)
	}
	k := c.ok("GET", "/v1/banks/WEBANK/contract", nil, 200)
	if k["contractStatus"] != "active" || k["contract"].(map[string]any)["disputeResolution"] != "Guangzhou Arbitration Commission" {
		t.Fatalf("contract: %v", k)
	}
	c.errKind("GET", "/v1/banks/NOPE", nil, 404, "unknown_bank")
}

func TestAuditTrail(t *testing.T) {
	c := client{t: t, base: newServer(t, "").URL}
	doc := map[string]any{"grade": "C", "limit": 139900, "rateBps": 1200}
	rec := c.ok("POST", "/v1/audit/anchors", map[string]any{"subject": "APP-2026-0001", "kind": "algorithm_output", "content": map[string]any{"doc": doc}}, 201)
	if rec["kind"] != "algorithm_output" || rec["index"].(float64) != 0 {
		t.Fatalf("anchor: %v", rec)
	}
	c.ok("POST", "/v1/audit/anchors", map[string]any{"subject": "APP-2026-0001", "kind": "ml_output", "contentHash": "0x" + strings.Repeat("ab", 32)}, 201)
	c.errKind("POST", "/v1/audit/anchors", map[string]any{"subject": "APP-2026-0001", "kind": "algorithm_output", "content": map[string]any{"doc": doc}}, 409, "duplicate")
	if v := c.ok("GET", "/v1/audit/anchors/APP-2026-0001/verify", nil, 200); v["valid"] != true || v["records"].(float64) != 2 {
		t.Fatalf("verify chain: %v", v)
	}
	found := c.ok("POST", "/v1/audit/verify", map[string]any{"content": map[string]any{"doc": map[string]any{"rateBps": 1200, "limit": 139900, "grade": "C"}}}, 200)
	if found["found"] != true {
		t.Fatalf("verify content: %v", found)
	}
}

func TestExplorerIsPublic(t *testing.T) {
	ts := newServer(t, "secret")
	c := client{t: t, base: ts.URL, token: "secret"}
	out := c.ok("POST", "/v1/withdrawals", vnWithdrawal(), 201)
	tx := out["chain"].(map[string]any)["txHash"].(string)
	id := out["withdrawalId"].(string)

	pub := client{t: t, base: ts.URL}
	ov := pub.ok("GET", "/explorer/api/overview", nil, 200)
	if ov["totalWithdrawals"].(float64) != 1 || len(ov["nodes"].([]any)) != 1 || len(ov["latestBlocks"].([]any)) == 0 {
		t.Fatalf("overview: %v", ov)
	}
	// The public view holds nothing readable: no amount, bank or account.
	d := pub.ok("GET", "/explorer/api/tx/"+tx, nil, 200)
	pubText := toJSON(d)
	for _, leak := range []string{"500000000", "VCB", "Vietcombank", "agent-4", "VN-HN", out["accountRef"].(string)} {
		if strings.Contains(pubText, leak) {
			t.Fatalf("public explorer leaks %q", leak)
		}
	}
	if d["withdrawal"].(map[string]any)["withdrawalId"] != id || d["verification"].(map[string]any)["valid"] != true {
		t.Fatalf("tx: %v", d)
	}
	pub.ok("GET", "/explorer/api/withdrawals/"+id, nil, 200)
	bn := int(d["transaction"].(map[string]any)["blockNumber"].(float64))
	pub.ok("GET", "/explorer/api/blocks/"+itoa(bn), nil, 200)
	for q, want := range map[string]string{tx: "tx", id: "withdrawal", strings.ToLower(id): "withdrawal", itoa(bn): "block"} {
		if s := pub.ok("GET", "/explorer/api/search?q="+q, nil, 200); s["type"] != want {
			t.Fatalf("search %s: %v", q, s)
		}
	}
	pub.errKind("GET", "/explorer/api/search?q=nothing", nil, 404, "not_found")
	for path, want := range map[string]string{"/explorer/": "BouyUp Chain Explorer", "/explorer/decrypt_withdrawal.py": "X25519PrivateKey",
		"/overview/": "BouyUp Chain Overview"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
}

func pieceKinds(d map[string]any) []string {
	var out []string
	for _, p := range d["readable"].([]any) {
		pm := p.(map[string]any)
		if pm["commitmentMatches"] != true {
			panic("commitment mismatch")
		}
		k := pm["kind"].(string)
		if k == "bank_share" {
			k = pm["content"].(map[string]any)["bank"].(string)
		}
		out = append(out, k)
	}
	return out
}

// Each key reads exactly its part: a bank the summary + its own share, the master key everything, an outsider nothing.
func TestDecryptPerKey(t *testing.T) {
	ts := newServer(t, "")
	c := client{t: t, base: ts.URL}
	id := c.ok("POST", "/v1/withdrawals", vnWithdrawal(), 201)["withdrawalId"].(string)
	priv := func(code string) string { k, _ := testKeys.Bank(code); return k.PrivateKey }
	dec := func(key string) map[string]any {
		return c.ok("POST", "/explorer/api/decrypt", map[string]any{"withdrawalId": id, "privateKey": key}, 200)
	}
	if got := strings.Join(pieceKinds(dec(priv("VCB"))), ","); got != "summary,VCB" {
		t.Fatalf("VCB reads %s", got)
	}
	if got := strings.Join(pieceKinds(dec(priv("TCB"))), ","); got != "summary,TCB" {
		t.Fatalf("TCB reads %s", got)
	}
	if got := pieceKinds(dec(testKeys.MasterPrivate())); len(got) != 4 {
		t.Fatalf("master reads %v", got)
	}
	c.errKind("POST", "/explorer/api/decrypt", map[string]any{"withdrawalId": id, "privateKey": priv("AGRI")}, 403, "no_access")
	c.errKind("POST", "/explorer/api/decrypt", map[string]any{"withdrawalId": id, "privateKey": "zz"}, 400, "invalid_key")

	// A bank changes its key: new withdrawals use the new key, the old one still opens the old withdrawal.
	oldKey := priv("VCB")
	fresh, err := sealingNewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	r := c.ok("PUT", "/v1/banks/VCB/public-key", map[string]any{"publicKey": fresh.Public}, 200)
	if r["encryptionKey"].(map[string]any)["publicKey"] != fresh.Public {
		t.Fatalf("key change: %v", r)
	}
	id2 := c.ok("POST", "/v1/withdrawals", vnWithdrawal(), 201)["withdrawalId"].(string)
	if got := strings.Join(pieceKinds(c.ok("POST", "/explorer/api/decrypt", map[string]any{"withdrawalId": id2, "privateKey": fresh.Private}, 200)), ","); got != "summary,VCB" {
		t.Fatalf("new key on new withdrawal: %s", got)
	}
	c.errKind("POST", "/explorer/api/decrypt", map[string]any{"withdrawalId": id2, "privateKey": oldKey}, 403, "no_access")
	if got := strings.Join(pieceKinds(dec(oldKey)), ","); got != "summary,VCB" {
		t.Fatalf("old key on old withdrawal: %s", got)
	}
	c.errKind("PUT", "/v1/banks/VCB/public-key", map[string]any{"publicKey": "nothex"}, 400, "invalid_key")
}

func TestOverview(t *testing.T) {
	ts := newServer(t, "")
	c := client{t: t, base: ts.URL}
	c.ok("POST", "/v1/withdrawals", vnWithdrawal(), 201)
	o := c.ok("GET", "/v1/overview", nil, 200)
	if o["demoKeysShown"] != true || len(o["sections"].([]any)) < 5 {
		t.Fatalf("overview: %v", o)
	}
	secrets := toJSON(o["secrets"])
	if !strings.Contains(secrets, testKeys.MasterPrivate()) || !strings.Contains(secrets, "BouyUp master key") {
		t.Fatal("the master key must be listed when showDemoKeys is on")
	}
	ex := o["decodeExample"].(map[string]any)
	if ex["available"] != true || ex["bankSees"] == nil || !strings.Contains(ex["curl"].(string), "/explorer/api/decrypt") {
		t.Fatalf("example: %v", ex)
	}
	resp, err := http.Get(ts.URL + "/v1/overview?format=markdown")
	if err != nil {
		t.Fatal(err)
	}
	md, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"# BouyUp", "## Security", "## Secrets", "## How to decrypt", "X25519", "Merkle", "PBFT"} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("markdown overview misses %q", want)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
