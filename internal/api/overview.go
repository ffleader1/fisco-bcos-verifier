package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"bouyup/fisco/internal/sealing"
)

// decryptScript is the standalone Python decrypt script (served at /explorer/decrypt_withdrawal.py and quoted in
// the overview).
//
//go:embed explorer/decrypt_withdrawal.py
var decryptScript string

// overviewHTML is the team overview page (/overview/): reads /v1/overview and the explorer API in the browser.
//
//go:embed overview/index.html
var overviewHTML []byte

func overviewPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(overviewHTML)
}

func decryptScriptFile(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="decrypt_withdrawal.py"`)
	_, _ = w.Write([]byte(decryptScript))
}

type section struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

type secretEntry struct {
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Unlocks    string `json:"unlocks"`
	HeldBy     string `json:"heldBy"` // where it lives in this demo and in production
	PublicKey  string `json:"publicKey,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"` // only with showDemoKeys
	Value      string `json:"value,omitempty"`      // non-key secrets (salt, token), only with showDemoKeys
	ActiveFrom string `json:"activeFrom,omitempty"`
	Status     string `json:"status"` // current | retired
}

// overview explains this part of BouyUp for the team, lists every secret, and walks through decryption.
//
//	GET /v1/overview                  JSON: sections (markdown) + secrets + decode example
//	GET /v1/overview?format=markdown  the same as one Markdown document
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	secrets := s.secrets()
	example := s.decodeExample(r)
	secs := overviewSections(s, secrets, example)
	if strings.EqualFold(r.URL.Query().Get("format"), "markdown") || strings.EqualFold(r.URL.Query().Get("format"), "md") {
		var b strings.Builder
		b.WriteString("# BouyUp — chain service overview\n\n")
		b.WriteString(fmt.Sprintf("Generated %s.\n\n", s.now().UTC().Format(time.RFC3339)))
		for _, sec := range secs {
			b.WriteString("## " + sec.Title + "\n\n" + strings.TrimSpace(sec.Markdown) + "\n\n")
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
		return
	}
	writeJSON(w, 200, map[string]any{
		"title": "BouyUp — chain service overview", "generatedAt": s.now().UTC(), "demoKeysShown": s.ShowDemoKeys,
		"sections": secs, "secrets": secrets, "decodeExample": example,
	})
}

func (s *Server) secrets() []secretEntry {
	show := s.ShowDemoKeys
	out := []secretEntry{}
	m := s.Keys.Master()
	e := secretEntry{Name: "BouyUp master key", Owner: "BouyUp", Status: "current", PublicKey: m.PublicKey, ActiveFrom: m.ActiveFrom,
		Unlocks: "EVERY piece of EVERY withdrawal (the master key gets a slot in all of them)",
		HeldBy:  "demo: " + keyFileLabel(s.Keys.Path()) + " · production: an HSM/KMS, never on a server disk"}
	if show {
		e.PrivateKey = m.PrivateKey
	}
	out = append(out, e)
	for _, b := range s.Catalog.Banks("") {
		k, ok := s.Keys.Bank(b.Code)
		if !ok {
			continue
		}
		e := secretEntry{Name: b.Code + " encryption key", Owner: b.Name, Status: "current", PublicKey: k.PublicKey, ActiveFrom: k.ActiveFrom,
			Unlocks: "the summary and " + b.Code + "'s own share of withdrawals it funds — nothing about other banks",
			HeldBy:  "demo: " + keyFileLabel(s.Keys.Path()) + " · production: the bank itself (BouyUp only knows the public key)"}
		if show {
			e.PrivateKey = k.PrivateKey
			if e.PrivateKey == "" {
				e.PrivateKey = "(registered by the bank: BouyUp does not have it)"
			}
		}
		out = append(out, e)
		for _, old := range k.Retired {
			o := secretEntry{Name: b.Code + " encryption key (earlier)", Owner: b.Name, Status: "retired", PublicKey: old.PublicKey, ActiveFrom: old.ActiveFrom,
				Unlocks: b.Code + "'s withdrawals recorded before " + old.RetiredAt, HeldBy: e.HeldBy}
			if show {
				o.PrivateKey = old.PrivateKey
			}
			out = append(out, o)
		}
	}
	for _, role := range []string{"admin", "operator"} {
		if v, ok := s.ChainKeys[role]; ok {
			e := secretEntry{Name: "chain " + role + " signing key", Owner: "BouyUp", Status: "current",
				Unlocks: map[string]string{"admin": "deploying contracts and granting roles", "operator": "writing withdrawal records and audit anchors (cannot read them)"}[role],
				HeldBy:  "demo: keys/" + role + ".key or generated · production: HSM/KMS"}
			if show {
				e.PrivateKey = v
			}
			out = append(out, e)
		}
	}
	salt := secretEntry{Name: "reference salt (refSalt)", Owner: "BouyUp", Status: "current", HeldBy: "config refSalt / SG_REF_SALT",
		Unlocks: "matching an accountRef or staffRef back to a user/account or staff id (the references are HMAC-SHA256 with this salt)"}
	if show {
		salt.Value = string(s.Salt)
	}
	out = append(out, salt)
	if s.Token != "" {
		t := secretEntry{Name: "API token", Owner: "BouyUp", Status: "current", HeldBy: "config apiToken / SG_API_TOKEN",
			Unlocks: "calling /v1/* (creating withdrawals, reading them decrypted with the master key, this overview)"}
		if show {
			t.Value = s.Token
		}
		out = append(out, t)
	}
	return out
}

func keyFileLabel(p string) string {
	if p == "" {
		return "in memory (lost on restart)"
	}
	return p
}

type decodeExample struct {
	Available    bool            `json:"available"`
	Note         string          `json:"note,omitempty"`
	WithdrawalID string          `json:"withdrawalId,omitempty"`
	Bank         string          `json:"bank,omitempty"`
	BankKey      string          `json:"bankPrivateKey,omitempty"`
	Curl         string          `json:"curl,omitempty"`
	Python       string          `json:"python,omitempty"`
	BankSees     json.RawMessage `json:"bankSees,omitempty"`
	MasterSees   int             `json:"masterSeesPieces,omitempty"`
}

// decodeExample decrypts the latest withdrawal with the key of one of its banks, to show exactly what a bank gets.
func (s *Server) decodeExample(r *http.Request) decodeExample {
	items, _, err := s.Ledger.ListWithdrawals(r.Context(), 0, 1)
	if err != nil || len(items) == 0 {
		return decodeExample{Note: "No withdrawal recorded yet. Create one (POST /v1/withdrawals) and reload this overview to get a worked example."}
	}
	wd := items[0]
	d, err := s.openAll(wd)
	if err != nil || len(d.shares) == 0 {
		return decodeExample{Note: "The latest withdrawal cannot be opened with the current master key."}
	}
	bank := d.shares[0].Bank
	k, _ := s.Keys.Bank(bank)
	ex := decodeExample{Available: true, WithdrawalID: wd.ID, Bank: bank, MasterSees: len(d.opened)}
	key := k.PrivateKey
	if !s.ShowDemoKeys || key == "" {
		key = "<" + bank + " private key>"
	} else {
		ex.BankKey = key
		if opened, err := sealing.Open(wd.ID, wd.Envelope, key, wd.Commitments); err == nil {
			var shown []json.RawMessage
			for _, o := range opened {
				shown = append(shown, o.Plaintext)
			}
			ex.BankSees, _ = json.Marshal(shown)
		}
	}
	base := "http://" + r.Host
	if r.TLS != nil {
		base = "https://" + r.Host
	}
	ex.Curl = fmt.Sprintf(`curl -X POST %s/explorer/api/decrypt -H 'Content-Type: application/json' -d '{"withdrawalId":"%s","privateKey":"%s"}'`, base, wd.ID, key)
	ex.Python = fmt.Sprintf(`python3 decrypt_withdrawal.py %s %s %s`, base, wd.ID, key)
	return ex
}

func overviewSections(s *Server, secrets []secretEntry, ex decodeExample) []section {
	var secretRows strings.Builder
	secretRows.WriteString("| Secret | Owner | Unlocks | Held by | Status | Public key | Private key / value |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, e := range secrets {
		val := e.PrivateKey
		if val == "" {
			val = e.Value
		}
		if val == "" {
			val = "(hidden: showDemoKeys is off)"
		}
		pub := e.PublicKey
		if pub == "" {
			pub = "—"
		}
		secretRows.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | `%s` | `%s` |\n", e.Name, e.Owner, e.Unlocks, e.HeldBy, e.Status, pub, val))
	}
	banner := "**DEMO KEYS — test only.** These keys were generated for the demo. Change `showDemoKeys` to `false` in the config (or `SG_SHOW_DEMO_KEYS=false`) to hide them."
	if !s.ShowDemoKeys {
		banner = "Keys are hidden (`showDemoKeys` is off). Only public keys are listed."
	}

	var exText string
	if ex.Available {
		exText = fmt.Sprintf("Worked example with the latest withdrawal on this chain, **%s**, read with the key of **%s**, one of its banks:\n\n```bash\n%s\n```\n\nor, without trusting this server at all:\n\n```bash\n%s\n```\n", ex.WithdrawalID, ex.Bank, ex.Curl, ex.Python)
		if len(ex.BankSees) > 0 {
			var pretty any
			_ = json.Unmarshal(ex.BankSees, &pretty)
			b, _ := json.MarshalIndent(pretty, "", "  ")
			exText += fmt.Sprintf("\n%s gets exactly these pieces — the summary and its own share, nothing about the other banks:\n\n```json\n%s\n```\n\nThe master key opens all %d pieces of this withdrawal.\n", ex.Bank, string(b), ex.MasterSees)
		}
	} else {
		exText = ex.Note + "\n"
	}

	return []section{
		{ID: "what", Title: "What this part of BouyUp does", Markdown: `
BouyUp lends to e-commerce sellers through five AI agents. This service is the **blockchain part**: the shared,
tamper-proof record that the other parts of BouyUp, the partner banks and auditors rely on. It runs on
**FISCO BCOS v2.11.1**, a consortium (permissioned) blockchain, with 4 nodes.

| Feature | What it does | Used by |
| --- | --- | --- |
| Withdrawals | Pays a user's account (mock treasury) and records the payout on chain, **encrypted**, with the split between the funding banks | Agent 4 (Disbursement & Collection), or a human for a manual payout |
| Bank catalog | Regions (VN, CN) and provinces; 10 demo banks with numeric limits, a written credit policy and a contract with BouyUp (YAML files) | Agent 3 reads the policies; Agent 4 picks the banks |
| Audit trail | Anchors hashes of documents (applications, algorithm output, agent traces, decisions) in a per-case hash chain | Agents 1, 2, 3, 5 and the approval gate |
| Explorer | Public web page at /explorer/: blocks, transactions, node status, encrypted withdrawals, and a box to decrypt one with your key | Banks, auditors, the team |
| Overview | This document: what the service does, why it is secure, every secret, how to decrypt | The team |

BouyUp is the third party in the middle: it pays the user at once from its own funds and later claims each bank's
share back from that bank (the bank API calls are outside this service). The on-chain record is the evidence of
payment named in each bank's contract: it proves that bank X funded x% of withdrawal Y.
`},
		{ID: "flow", Title: "How a withdrawal works", Markdown: `
1. Agent 4 (or a person) calls POST /v1/withdrawals with the user, account, amount, region and province, the
   bank split in percent, and who is asking (actor).
2. The service checks the split: each percent has at most 2 decimals, the total is exactly 100.00%, every bank
   exists, is active, belongs to the region and has an encryption key.
3. BouyUp pays the account (mock treasury today).
4. The withdrawal is cut into pieces — a **summary** (amount, region, account reference, actor, how many banks)
   and **one piece per bank** (its percent and amount) — and each piece is encrypted (next section).
5. The encrypted record, a fingerprint (commitment) of each piece and their combined fingerprint (Merkle root)
   are written to the WithdrawalLedger contract. The contract recomputes the root and rejects a mismatch.
6. The response carries the readable record (for the caller), the transaction hash, block, sealing node and
   how many of the 4 nodes already hold it.
7. Each bank can later read its share from the chain with its own key and prove it to anyone.
`},
		{ID: "security", Title: "Security: what protects the data and why", Markdown: `
### 1. A consortium blockchain (FISCO BCOS)
- **Several independent copies.** Every member runs a node holding the full ledger. Our demo runs 4 nodes; the
  explorer shows, per transaction, which nodes already hold it.
- **No single party can rewrite history.** Blocks are agreed by PBFT consensus among the sealer nodes: a block
  needs a two-thirds majority, so one dishonest or broken node cannot change or delete a record.
- **Only allowed keys can write.** Transactions are signed. The contracts check roles: only the BouyUp
  operator key may record withdrawals; only the admin key may grant roles.
- **Benefit:** a bank, an auditor or an arbitrator can trust the record without trusting BouyUp's database.

### 2. Every withdrawal is encrypted on chain ("envelope encryption")
The chain is shared, so anything written there in clear would be visible to every member — e.g. every bank
would see every other bank's shares. So each withdrawal is encrypted in pieces:

| Piece | Readable by |
| --- | --- |
| Summary (amount, currency, region, province, account reference, payout reference, actor, number of banks) | every bank of that withdrawal + BouyUp |
| Bank X's share (percent, amount, contract no.) | bank X + BouyUp only |

How:
- Each piece gets its own random 256-bit key and is encrypted with **AES-256-GCM** (encryption + built-in
  tamper detection).
- The piece keys are handed out in **key slots**, one per reader. A slot is encrypted with a key that only that
  reader can compute: **X25519** key agreement between a one-time key of the withdrawal and the reader's public
  key, turned into an AES key with **HKDF-SHA256**.
- Slots carry **no names** and are **shuffled**: nobody can tell from the chain which banks took part. A reader
  finds its slot by trying its key; a wrong key fails cleanly (AES-GCM refuses it).
- Every slot and piece is bound to its withdrawal id, so pieces cannot be swapped between withdrawals.
- **Benefit:** the record is public and permanent, yet each bank sees only its own share; competitors learn
  nothing about each other's volumes.

### 3. Fingerprints of each piece (commitments) and their Merkle root
- For each piece, SHA-256 of its exact readable text is stored on chain (a **commitment**), plus the **Merkle
  root** of all of them. Each readable text includes a random salt, so a fingerprint cannot be guessed from
  likely values ("60%").
- **Prove a share without giving up the key:** a bank shows an arbitrator just its decrypted piece; the
  arbitrator hashes it and finds that fingerprint on chain. The bank never hands over its private key (which
  would open all its records).
- **BouyUp cannot cheat either:** AES-GCM is not "key-committing" — whoever encrypts could craft data that
  decrypts to different valid texts under different keys (show a bank 60% and an arbitrator 40%). The
  fingerprint is written in the same transaction and only one text matches it.
- The contract recomputes the Merkle root itself (SHA-256) and rejects the record if it does not match.

### 4. No personal data on chain
- User id + account number become an **accountRef** = HMAC-SHA256 with BouyUp's secret salt; a human actor's
  staff id becomes a **staffRef** the same way. Only BouyUp (holding the salt) can match them back. They sit
  inside the encrypted summary anyway.

### 5. Audit trail hash chain
- Agents anchor the hash of each document they produce. Each case keeps a chain: every new record's "head" is
  keccak256(previous head, kind, document hash, time). Deleting or reordering any off-chain record breaks the
  chain, and GET /v1/audit/anchors/{subject}/verify finds where.

### 6. Safe retries and one payment
- A requestId makes a withdrawal idempotent: a retry returns the same record instead of paying twice; reusing
  the id for different data is refused (409).

### 7. Keys and key changes
- Each bank owns its private key and may change it at any time; BouyUp only needs the bank's **current public
  key** (PUT /v1/banks/{code}/public-key). New withdrawals use the new key; older ones stay readable with the
  key that was current then — the withdrawal's timestamp tells the bank which one to try (at most two around
  the change). Nothing on chain reveals that a bank changed its key.
- The BouyUp master key opens everything: in production it belongs in an HSM/KMS.

### 8. What is NOT hidden, and limits
- Visible to all members: that a withdrawal happened, when, its id, the number of pieces (= banks + 1) and the
  record size.
- If a key ever leaks, every record encrypted to it stays readable forever (a chain cannot be wiped). The master
  key is the most sensitive secret.
- A bank sees only its own share, so it cannot check by itself that all shares add up to 100%. The service
  checks it before writing, and anyone with the master key (an auditor) can re-check it:
  GET /v1/withdrawals/{id}/verify.
- The payout is a mock treasury; real bank transfers and reimbursement claims are outside this service.
`},
		{ID: "secrets", Title: "Secrets", Markdown: banner + `

` + secretRows.String() + `

- The **master key** and each **bank key** decrypt withdrawals (X25519, 32 bytes, hex).
- The **chain signing keys** write to the chain but cannot read encrypted withdrawals.
- The **reference salt** links accountRef / staffRef back to real ids.
- Keys are stored in the file named in config **bankKeysFile** (created with fresh demo keys on first start).
`},
		{ID: "decode", Title: "How to decrypt a withdrawal (step by step)", Markdown: `
You need: a withdrawal id (or its transaction hash) and a private key from the Secrets section.

1. **Find the withdrawal.** Open /explorer/, pick one under "Latest withdrawals" and copy its id (WD-…). Its
   page shows only the encrypted record: pieces, slots, fingerprints.
2. **Pick a key.** From the Secrets section: a bank's private key (that bank's view) or the BouyUp master key
   (everything).
3. **Decrypt — choose one way:**
   - **Explorer:** on the withdrawal's page, paste the key into "Decrypt with your key" and press Decrypt.
   - **API:** POST /explorer/api/decrypt with {"withdrawalId": "...", "privateKey": "..."}.
   - **Independent script** (does not trust our server; decrypts on your machine):
     download /explorer/decrypt_withdrawal.py, run pip install cryptography, then
     python3 decrypt_withdrawal.py <server> <withdrawal id> <private key>.
4. **Read the result.**
   - A bank key returns 2 pieces: the summary and that bank's share.
   - The master key returns every piece.
   - A key that is not part of the withdrawal gets "this key cannot read withdrawal …" — that is the
     protection working.
5. **Check it was not altered.** Each piece comes with commitmentMatches: true when SHA-256 of what you
   decrypted equals the fingerprint on chain. The script also checks the Merkle root.

**Test to run:** take one withdrawal with two banks and decrypt it three times — with bank A's key (summary +
A only), bank B's key (summary + B only) and the master key (all pieces). Then try a bank that is not in it:
it must be refused.

` + exText},
		{ID: "endpoints", Title: "Endpoints", Markdown: `
| Endpoint | Purpose | Token |
| --- | --- | --- |
| POST /v1/withdrawals | Pay and record an encrypted withdrawal | yes |
| GET /v1/withdrawals · /{id} · /{id}/verify | Read decrypted with the master key; full verification | yes |
| GET /v1/regions · /v1/banks · /v1/banks/{code} · /policy · /contract | Bank catalog (policies for Agent 3) | yes |
| PUT /v1/banks/{code}/public-key | A bank registers a new public key | yes |
| POST /v1/audit/anchors · GET /v1/audit/anchors/{subject}[/verify] · POST /v1/audit/verify | Audit trail | yes |
| GET /v1/overview[?format=markdown] | This overview | yes |
| GET /explorer/ · /explorer/api/… · POST /explorer/api/decrypt | Public explorer and decryption with your own key | no |
| GET /docs/ | API reference with examples | no |
`},
	}
}
