# BouyUp — FISCO BCOS component (Go)

The blockchain service of BouyUp. It does these things:

| Feature | What it does | Who uses it |
|---|---|---|
| **Withdrawals** | Pays a user's account (mock treasury, unlimited funds) and writes the payout on chain **encrypted per bank**, with fingerprints that prove each share. Returns the transaction hash, block, sealing node and how many nodes hold it. | Agent 4, or a human for a manual payout |
| **Bank catalog** | Regions (VN, CN) and provinces; 10 demo banks with numeric limits, a written credit policy and a contract with BouyUp. Stored as YAML. | Agent 3 reads the policies; Agent 4 picks the banks |
| **Audit trail** | Anchors hashes of documents (applications, algorithm output, agent traces, decisions) in a per-subject hash chain. | Agents 1, 2, 3, 5 and the approval gate |
| **Explorer** | Public read-only web page at `/explorer/`: blocks, transactions, node status, encrypted withdrawals, and a box to decrypt one with your own key. | Banks, auditors, the team |
| **Overview** | **`/overview/`** — a friendly page for the team (open the server root, it redirects there): the design, who can read what, a 10-step animation of a withdrawal moving through the 4 nodes (with a demo form that replays your own withdrawal with real timings), a "test every key" bench, every secret with Show/Copy, and copy-paste decrypt commands. Same content as data at `GET /v1/overview` (`?format=markdown`): what this service does, its security design, **every secret** (demo keys), a step-by-step decryption guide with a worked example. | The team |

The loan ledger of earlier versions (loan records, repayments, floors, alerts, covenants, deliveries,
multi-bank funding) has been removed: this service only pays and records withdrawals.

| | |
|---|---|
| Chain | FISCO BCOS **v2.11.1**, 4 nodes, group 1, ECDSA (non-SM) |
| SDK | `github.com/FISCO-BCOS/go-sdk` **v1.1.1** (pure Go, builds on Windows) |
| Contracts | Solidity 0.6.10: `WithdrawalLedger`, `AuditTrail` (compiled output embedded in the binary) |
| Service | `sgchain`, Go ≥ 1.22, REST + Swagger page (`/docs/`) + explorer (`/explorer/`) |
| Ledgers | `fisco` (real chain) or `mock` (in-memory, same rules, for tests and quick demos) |

## Withdrawals

```
POST /v1/withdrawals
{
  "requestId": "agent4-req-0001",                 // optional idempotency key
  "userId": "U-778899", "accountNo": "0071000123456",
  "amount": 500000000,                            // VND in đồng; CNY in fen
  "region": "VN", "province": "VN-HN",
  "bankSplit": { "VCB": 60, "BIDV": 25.5, "TCB": 14.5 },
  "actor": { "type": "agent", "agentId": "agent-4", "version": "4.2.0" }
}
```

- **Bank split rules:** each ratio > 0 with **at most 2 decimals**; the total must be **exactly 100.00%**;
  banks must exist, belong to the request's region and have an active contract. Otherwise
  `400 invalid_bank_ratio` (or `unknown_bank`, `bank_region_mismatch`, `bank_unavailable`).
  The contract re-checks the 100.00% rule, so a bad split can never reach the chain.
- **Actor:** `agent` (only `agent-4` may withdraw, with its `version`) or `human` (`staffId`, `role`,
  and a `reason` are required). The chain records the actor type, agent id + version or the human role,
  a salted reference of the staff id, and the hash of the full actor object.
- **Amounts per bank:** `amount × ratio`, the largest share takes the rounding remainder, so the legs always
  add up. Each bank's share stays `reimbursement: pending`; claiming it from the bank is outside this service.
- **Privacy:** the chain stores the withdrawal **encrypted** (next section). Inside it, `userId` + `accountNo` are a salted `accountRef`.
- **Response:** the record plus `chain` = `txHash`, `blockNumber`, `blockHash`, `submittedTo`, `sealedBy`,
  `confirmedNodes / totalNodes`, per-node confirmations, `explorerUrl`, `verifyUrl`.
- `GET /v1/withdrawals/{id|txHash}` reads it back from the chain and decrypts it with the BouyUp master key;
  `.../verify` recomputes the Merkle root, decrypts every piece, matches the fingerprints, re-checks the 100%
  split and asks every node whether it holds the transaction.

## Encryption: each bank sees only its own share

The chain is shared by every member, so nothing readable goes on it:

| Piece | Readable by |
|---|---|
| Summary: amount, currency, region, province, account ref, payout ref, actor, number of banks | every bank of that withdrawal + BouyUp |
| Bank X's share: percent, amount, contract no. | bank X + BouyUp only |

- Each piece is encrypted with its own random key (AES-256-GCM). The piece keys are handed out in unnamed,
  shuffled **key slots**, one per reader, each locked with X25519 + HKDF-SHA256 to that reader's public key.
  A reader finds its slot by trying its key. Format: `internal/sealing/sealing.go`.
- On chain: the envelope, a SHA-256 **commitment** of each piece's plaintext (salted) and their **Merkle root**,
  which the contract recomputes. A bank proves its share to anyone by showing the piece; nobody (BouyUp included)
  can show different contents later.
- Keys: `bankKeysFile` (default `keys/bank-keys.json`) is created with fresh **demo** key pairs for the master
  and every bank on first start. A bank changes its key with `PUT /v1/banks/{code}/public-key`; older
  withdrawals stay readable with its older key.
- `showDemoKeys` (default **true**) prints every private key in `GET /v1/overview` so the team can test
  decryption. Set it to `false` (or `SG_SHOW_DEMO_KEYS=false`) when real keys are used.
- Decrypt with any key: the explorer's "Decrypt with your key" box, `POST /explorer/api/decrypt`,
  `sgchain decrypt -server <url> -id <WD-…> -key <hex>`, or the standalone
  `internal/api/explorer/decrypt_withdrawal.py` (also served at `/explorer/decrypt_withdrawal.py`; needs
  `pip install cryptography`), which does not trust the server at all.

## Bank catalog (YAML)

`internal/catalog/data/regions.yaml` and `internal/catalog/data/banks/*.yaml` are built into the binary.
To edit them without rebuilding, copy the `data/` folder and set `"catalogDir"` (or `SG_CATALOG_DIR`).
Unknown YAML fields are rejected at startup, so typos fail loudly.

- VN: Vietcombank (VCB), Techcombank (TCB), BIDV, Agribank (AGRI), MB Bank (MBB). Currency VND.
- CN: WeBank, ICBC, China Construction Bank (CCB), Bank of China (BOC), China Merchants Bank (CMB). Currency CNY.
- Each bank has `limits` (numbers; not enforced by the withdrawal endpoint), `policy` (text for Agent 3:
  sector / province / customer preferences with reasons, red flags, when to escalate, deal breakers) and
  `contract` (its mock agreement with BouyUp). Policy and contract carry a SHA-256 `hash`.
- **All bank data is mock data for the demo**, not the real policy or terms of these banks.

## Quick start: mock, no chain (Windows / any OS)

```bash
go mod tidy                                   # first time: resolves go.sum
go test ./...                                 # unit, API and conformance tests
go run ./cmd/sgchain serve -config configs/mock.json
```
Then open `http://localhost:12080/docs/` (API reference) and `http://localhost:12080/explorer/`,
or run the requests in **`api.http`** (GoLand) or `./scripts/demo.sh`.

## Real chain (4 nodes)

The chain runs on Linux. On Windows use **WSL2 (Ubuntu)**; `sgchain` itself can run on Windows.

```bash
CHAIN_DIR=~/sg-chain ./scripts/start_chain.sh            # 4 nodes: channel ports 20200-20203, SDK certs -> ./chain/sdk
go run ./cmd/sgchain keygen -out keys                     # admin + operator keys
#   edit configs/fisco.json: set refSalt (>= 16 chars)
go run ./cmd/sgchain deploy -config configs/fisco.json   # deploy WithdrawalLedger + AuditTrail, grant roles -> chain/contracts.json
go run ./cmd/sgchain serve  -config configs/fisco.json   # http://localhost:12080
./scripts/demo.sh
```
- `node.nodeURL` is the node transactions are sent to; `node.peers` lists the other nodes. The service
  connects to all of them read-only to show node status and per-transaction confirmations. An offline peer
  is shown as offline, it does not stop the service.
- The contracts changed: run `deploy` again on an existing chain.
- `SG_*` environment variables override the config (`SG_REF_SALT`, `SG_API_TOKEN`, `SG_KEY_OPERATOR`,
  `SG_NODE_PEERS`, `SG_CATALOG_DIR`, …).

## API (JSON)

| Method & path | Purpose |
|---|---|
| `POST /v1/withdrawals` | pay an account and record the withdrawal on chain |
| `GET /v1/withdrawals` · `GET /v1/withdrawals/{id}` · `GET /v1/withdrawals/{id}/verify` | list, read (id or tx hash), verify against the chain |
| `POST /explorer/api/decrypt` | decrypt a withdrawal with your own key (public) |
| `POST /v1/demo/withdrawals` | demo only (needs `showDemoKeys`): a manual withdrawal marked `DEMO`, capped at 10,000,000 VND / 50,000 CNY, returned with a `trace` of each stage and when every node wrote the block |
| `GET /overview/` | team overview page (public; the secrets on it load from `/v1/overview`, which needs the token when one is set) |
| `GET /v1/overview[?format=markdown]` | security design, secrets, decryption guide |
| `GET /v1/regions` | countries and provinces |
| `GET /v1/banks?region=` · `GET /v1/banks/{code}` · `/policy` · `/contract` | bank catalog |
| `PUT /v1/banks/{code}/public-key` | a bank registers a new encryption key |
| `POST /v1/audit/anchors` · `GET /v1/audit/anchors/{subject}[/verify]` · `POST /v1/audit/verify` | audit trail |
| `GET /explorer/api/overview` · `blocks` · `blocks/{n}` · `tx/{hash}` · `withdrawals[/{id}]` · `search?q=` | public explorer data |
| `GET /healthz` · `GET /v1/chain` | health, contracts, accounts, nodes |

Full reference with examples: `/docs/` (Swagger UI, works offline) or `/openapi.json`.
`/v1/*` needs `Authorization: Bearer <apiToken>` when a token is set; `/healthz`, `/docs/` and `/explorer/` are public.
Errors: `{"error":{"kind","message","details"}}` — branch on `kind` (see the table at the top of `/docs/`).

## Tests

- `go test ./...`: hashing, catalog, the HTTP API end to end, and the **conformance suite** on the mock.
- `make chain && make test-integration`: the **same conformance suite on the live 4-node chain** (fresh contracts
  and keys each run): withdrawals, on-chain split validation, listing, audit hash chain, explorer reads and
  per-node confirmations.

## Layout

```
contracts/            Roles.sol, WithdrawalLedger.sol, AuditTrail.sol, build/ (ABI+BIN, embedded)
cmd/sgchain/          keygen | deploy | bootstrap | serve | decrypt
internal/api/         REST handlers, explorer page (explorer/index.html), OpenAPI + Swagger UI (docs/)
internal/catalog/     regions + banks (data/*.yaml)
internal/payout/      paying system (mock treasury)
internal/sealing/     per-bank encryption (X25519 + HKDF + AES-GCM), commitments, Merkle root
internal/keystore/    master + bank encryption keys (demo key file)
internal/ledger/      domain types + Ledger interface
  fisco/              go-sdk v1.1.1 adapter (+ integration test)
  mock/               in-memory implementation of the same rules
  ledgertest/         conformance suite shared by both
internal/hashing/     salted refs, canonical JSON hashing
configs/ scripts/     configs, chain start/stop, demo, solc build
api.http              GoLand HTTP client requests
```

## Notes and limits

- FISCO BCOS 2.x `block.timestamp` is in milliseconds; the contracts divide by 1000.
- The payout is a mock. A real paying system plugs in behind `payout.Payer` (it must be idempotent per withdrawal id).
- If the chain write fails after the mock payout, the error is logged with the withdrawal id and payout reference.
- SM (国密) chains are not supported yet. For production keys, use an HSM or KMS instead of key files.
