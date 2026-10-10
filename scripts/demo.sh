#!/usr/bin/env bash
# End-to-end walk-through of the BouyUp chain API (works against the mock or a real chain).
#   ./scripts/demo.sh [base-url]        default http://localhost:12080 ; set SG_API_TOKEN if the server uses one
set -euo pipefail
BASE="${1:-http://localhost:12080}"
RUN="$(date +%s)"
AUTH=()
[ -n "${SG_API_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer $SG_API_TOKEN")
call() { echo; echo "### $1 $2"; curl -sS -X "$1" "${AUTH[@]}" -H 'Content-Type: application/json' "$BASE$2" ${3:+-d "$3"}; echo; }

call GET /v1/chain
call GET "/v1/banks?region=VN"
call GET /v1/banks/VCB/policy

# Agent 4 pays 500,000,000 VND to a seller in Hà Nội, funded 60 / 25.5 / 14.5 by three banks.
call POST /v1/withdrawals '{
  "requestId": "demo-'"$RUN"'-vn", "userId": "U-778899", "accountNo": "0071000123456",
  "amount": 500000000, "region": "VN", "province": "VN-HN",
  "bankSplit": { "VCB": 60, "BIDV": 25.5, "TCB": 14.5 },
  "actor": { "type": "agent", "agentId": "agent-4", "name": "Disbursement & Collection", "version": "4.2.0", "runId": "demo-'"$RUN"'" } }'

# Same requestId again: returns the existing record (replayed), nothing is paid twice.
call POST /v1/withdrawals '{
  "requestId": "demo-'"$RUN"'-vn", "userId": "U-778899", "accountNo": "0071000123456",
  "amount": 500000000, "region": "VN", "province": "VN-HN",
  "bankSplit": { "VCB": 60, "BIDV": 25.5, "TCB": 14.5 },
  "actor": { "type": "agent", "agentId": "agent-4", "name": "Disbursement & Collection", "version": "4.2.0", "runId": "demo-'"$RUN"'" } }'

# A manual payout by a credit officer in China: 139,900.00 CNY (amounts in fen).
call POST /v1/withdrawals '{
  "userId": "U-102938", "accountNo": "6222020200112233", "amount": 13990000, "currency": "CNY",
  "region": "CN", "province": "CN-GD", "bankSplit": { "WEBANK": 70, "CMB": 30 },
  "actor": { "type": "human", "staffId": "CO-17", "role": "credit_officer", "reason": "Manual payout: agent run failed after approval" } }'

# Rejected: the split adds up to 90%.
call POST /v1/withdrawals '{
  "userId": "U-1", "accountNo": "1", "amount": 1000000, "region": "VN", "province": "VN-HN",
  "bankSplit": { "VCB": 60, "TCB": 30 }, "actor": { "type": "agent", "agentId": "agent-4", "version": "4.2.0" } }'

ID=$(curl -sS "${AUTH[@]}" "$BASE/v1/withdrawals?limit=1" | sed -n 's/.*"withdrawalId": *"\([^"]*\)".*/\1/p' | head -1)
call GET "/v1/withdrawals/$ID/verify"

# What the public sees (encrypted), then what different keys can read. The latest withdrawal is the CN one
# (WeBank 70 / CMB 30): WeBank's key reads the summary + its share, VCB's key is refused, the master key reads all.
call GET "/explorer/api/withdrawals/$ID"
OVERVIEW=$(curl -sS "${AUTH[@]}" "$BASE/v1/overview")
key_of() { printf '%s' "$OVERVIEW" | python3 -c "import json,sys;d=json.load(sys.stdin);print(next(s['privateKey'] for s in d['secrets'] if s['name']==sys.argv[1]))" "$1"; }
call POST /explorer/api/decrypt '{ "withdrawalId": "'"$ID"'", "privateKey": "'"$(key_of 'WEBANK encryption key')"'" }'
call POST /explorer/api/decrypt '{ "withdrawalId": "'"$ID"'", "privateKey": "'"$(key_of 'VCB encryption key')"'" }'
call POST /explorer/api/decrypt '{ "withdrawalId": "'"$ID"'", "privateKey": "'"$(key_of 'BouyUp master key')"'" }'

# Another agent anchors the hash of its output in the audit trail.
call POST /v1/audit/anchors '{ "subject": "APP-DEMO-'"$RUN"'", "kind": "algorithm_output",
  "content": { "doc": { "grade": "C", "limit": 13990000, "rateBps": 1200, "sweepRateBps": 6300 } } }'
call GET "/v1/audit/anchors/APP-DEMO-$RUN/verify"

echo; echo "Explorer: $BASE/explorer/#/withdrawal/$ID"
