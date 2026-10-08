# SellerGraph — FISCO BCOS component (Go)

The blockchain layer of the SellerGraph PoC: the box **"FISCO BCOS Execution Layer"** in the architecture
diagrams. It is a permissioned, tamper-evident **system of record** for each loan. The ML core, the
policy engine and the human officers make the credit decisions. This component records those decisions
and enforces the bookkeeping rules on chain.

| | |
|---|---|
| Chain | FISCO BCOS **v2.11.1** (2.x LTS line, last release 2024-07-26), group 1, ECDSA (non-SM) |
| SDK | `github.com/FISCO-BCOS/go-sdk` **v1.1.1**: pure Go, no cgo, builds natively on Windows |
| Contracts | Solidity 0.6.10 (`contracts/`, compiled output embedded in the binary) |
| Service | `sgchain`: REST API, `net/http`, Go ≥ 1.22 |
| Ledgers | `fisco` (real chain) or `mock` (in-memory, same rules: runs with no node) |

## What goes on chain

The scope was agreed up front: loan record and status, the repayment ledger, the audit trail and
delivery proof. Disbursement is **recorded** (payment lines, bank reference, shipping deadline). Restricted
spending and a supplier whitelist are **not** enforced on chain; supplier vetting (checks 1–4) stays in the
off-chain scoring.

**LoanLedger.sol**
- **Loan record.** Holds the terms from the approved decision: tier, principal, rate, tenor, sweep rate and
  sweep cap, grace days, and `decisionHash` (the hash of the full approval package).
  - Lifecycle: `created → approved → active → settled`, with side paths to `cancelled`, `accelerated` and `defaulted`.
- **Configurable lenders.**
  - By default **WeBank is the sole lender**. The operator key is the WeBank funder key, so its share is auto-confirmed.
  - Multi-bank mode (the old diagram 02) still works: N funders whose shares must sum to the principal. Each funder confirms with its own key.
- **Human gates, enforced by the contract.**
  - A tier-2 loan above `officerThreshold` (default 500,000 CNY) **always** needs an officer signature, whatever the orchestrator sends.
  - The orchestrator can also force review: Agent 2 ≠ Agent 3, PD close to the cut-off, a fraud signal, a seller request, and so on.
  - Acceleration, default and restructuring are **officer-only**. The monitor role (the Agent 5 pipeline) can raise alerts but cannot do any of these.
- **One click, one payment.** A single disbursement whose payment lines must sum to the principal. A second disbursement is rejected.
- **Repayment ledger.**
  - Interest is simple interest on outstanding principal, accrued exactly to the second (the remainder is carried forward).
  - Each payment is allocated **overdue interest → overdue principal → current interest → principal**. Any excess goes to `refundDue`.
  - `extRef` is an idempotency key, so the same bank debit can never be booked twice.
  - Prepayment carries no fee (`previewPayoff`).
- **Cumulative floor** (sàn lũy kế). The floor can never decrease.
  - Below the floor → a YELLOW alert.
  - At a milestone, any remaining shortfall becomes overdue.
  - At maturity, everything still owed is overdue → a RED alert.
- **Delivery proof (check 5).** A waybill hash plus the ship date are checked against the deadline. Late shipment emits a `DELIVERY_LATE` covenant event. If nothing ships, `flagMissingDelivery` → `DELIVERY_MISSING` + a RED alert.
- **Covenant events and alerts** (green / yellow / red) carry evidence hashes.

**AuditTrail.sol** (F18)
- Anchors **hashes only** of: application, ML output, agent I/O, credit memo (Agent 2), risk challenge
  (Agent 3), human decision, offer/contract, Art. 24 explanation, sweep notice, decision package.
- Each subject keeps a **hash chain** (`head = keccak(prev, kind, hash, ts)`). If any off-chain record is
  deleted or reordered, `GET /v1/audit/anchors/{subject}/verify` detects it.

**Privacy:** no personal data on chain. Seller and supplier IDs become HMAC-SHA256 references (`refSalt`,
held by WeBank). Documents are hashed canonically (sorted-key JSON, SHA-256).

**Roles:** `admin`, `operator` (the fixed-code orchestrator), `officer` (a human), `monitor`, `funder`,
`audit writer`. Each role signs with its own key.

## Quick start: mock, no chain (Windows / any OS)

```bash
go mod tidy                                   # first time: resolves go.sum
go test ./...                                 # unit + API + conformance tests
go run ./cmd/sgchain serve -config configs/mock.json
```
Then open **`api.http`** in GoLand and run the requests. Or, from bash, run `./scripts/demo.sh`.

## Real chain

The chain runs on Linux. On Windows use **WSL2 (Ubuntu)**. `sgchain` itself can run on Windows: WSL2
forwards `127.0.0.1:20200`.

```bash
# in WSL2 / Linux, from the repo:
CHAIN_DIR=~/sg-chain ./scripts/start_chain.sh    # 4 nodes, certs copied to ./chain/sdk
go run ./cmd/sgchain keygen -out keys -names admin,operator,officer,monitor,vcb,tcb
#   edit configs/fisco.json: set refSalt (≥16 chars)
go run ./cmd/sgchain deploy -config configs/fisco.json   # deploy + roles + policy -> chain/contracts.json
go run ./cmd/sgchain serve  -config configs/fisco.json   # http://localhost:12080
./scripts/demo.sh
```
- Multi-bank: use `configs/fisco.multibank.json` (funders webank, vcb, tcb).
- After adding a funder, run `sgchain bootstrap`.
- Keys can be 0x-hex, a file containing hex, or a FISCO console PEM account file.
- `SG_*` environment variables override the config (`SG_REF_SALT`, `SG_API_TOKEN`, `SG_KEY_OPERATOR`, …).

## API reference page

Open **`http://<host>:12080/docs/`** in a browser. It's a Swagger UI page listing every endpoint, with
field descriptions and example bodies. "Try it out" sends real requests; click **Authorize** and paste the
API token first.

- Everything is built into the binary (Swagger UI 5.33.1 is vendored), so it works with no internet access.
- The machine-readable spec is at `/openapi.json` (also `/docs/openapi.json`).
- The page and the spec need no token. Every API call still does.
- Set `"disableDocs": true` in the config to turn the page off.
- It also works behind a reverse proxy, including under a sub-path such as `https://example.com/sgapi/docs/`.

## API (JSON, amounts in fen, times are RFC3339 or unix seconds)

| Method & path | Purpose |
|---|---|
| `POST /v1/loans` | register an approved decision (`decision.doc` is anchored and becomes `decisionHash`) |
| `POST /v1/loans/{id}/funding/confirm` | funder confirms its share (multi-bank) |
| `POST /v1/loans/{id}/officer-decision` | human sign-off / rejection (`review` anchored) |
| `POST /v1/loans/{id}/cancel` | offer declined or expired |
| `POST /v1/loans/{id}/disburse` | the one payment, to the suppliers |
| `POST /v1/loans/{id}/repayments` | sweep / debit / prepayment / recovery; returns the allocation |
| `POST /v1/loans/{id}/floor-checkpoints` | cumulative floor check (`milestone` → overdue) |
| `POST /v1/loans/{id}/mature` · `accelerate` · `default` · `restructure` | lifecycle; the last three are officer-only |
| `POST /v1/loans/{id}/alerts` · `covenant-events` | monitoring |
| `POST /v1/loans/{id}/deliveries` · `deliveries/{line}/missing` | delivery proof (check 5) |
| `GET /v1/loans/{id}` · `/payoff?asOf=` · `GET /v1/loans` · `GET /v1/portfolio` | reads, portfolio, per-funder exposure |
| `POST /v1/audit/anchors` · `GET /v1/audit/anchors/{subject}[/verify]` · `POST /v1/audit/verify` | audit trail |
| `POST /blockchain/create-loan` · `/blockchain/disburse` · `/repayment` · `GET /portfolio` | aliases named after the endpoints in diagram 02 |

Errors come back as `{"error":{"kind","message"}}`. Status codes: `invalid` 400, `unauthorized` 403,
`not_found` 404, `bad_status` / `duplicate` 409, `chain` 502. The kind comes from the contract's revert code.

## Tests

- `go test ./...`: hashing, the HTTP API end to end, and the **conformance suite** on the mock.
- `make chain && make test-integration`: the **same conformance suite on a live chain**. It deploys fresh contracts and keys.
  - It covers 10 scenarios: the Part 10 example, multi-funder, human gate, validation, floor/overdue/allocation, maturity/acceleration/default, delivery proof, restructuring, exact payoff, and the audit hash chain.
  - The suite checks interest against an independent calculation, and the mock must pass exactly the same assertions as the chain.

## Notes and limits

- **asOf / time.** Repayments and checkpoints carry a value date (`asOf`). The contract only accepts one up to
  `maxFutureSkewSec` (default 300 s) ahead of block time. The integration tests set a skew of 400 days to replay
  a whole loan in seconds. Never do that in production.
- FISCO BCOS 2.x `block.timestamp` is in **milliseconds**. The contracts divide by 1000.
- **SM (国密) chains are not supported yet.** That needs SM2 keys and the SM3 hash-chain formula.
- **Upgrading to FISCO 3.x:** add an adapter for go-sdk v3 (cgo) that implements `ledger.Ledger`. The contracts and the API stay the same.
- The mock is in-memory. `/v1/portfolio` scans every loan, which is fine at PoC scale; production would use an event indexer.
- For production keys, use an HSM or KMS instead of key files.

## Layout

```
contracts/            Roles.sol, LoanLedger.sol, AuditTrail.sol, build/ (ABI+BIN, embedded)
cmd/sgchain/          keygen | deploy | bootstrap | serve
internal/ledger/      domain types + Ledger interface
  fisco/              go-sdk v1.1.1 adapter (+ integration test)
  mock/               in-memory implementation of the same rules
  ledgertest/         conformance suite shared by both
internal/api/         REST handlers, portfolio
internal/hashing/     salted refs, canonical JSON hashing
configs/ scripts/     configs, chain start/stop, demo, solc build
api.http              GoLand HTTP client requests
```
