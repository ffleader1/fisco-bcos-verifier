#!/usr/bin/env bash
# End-to-end walk-through of the SellerGraph blockchain API (works against the mock or a real chain).
#   ./scripts/demo.sh [base-url]        default http://localhost:12080 ; set SG_API_TOKEN if the server uses one
set -euo pipefail
BASE="${1:-http://localhost:12080}"
ID="LN-DEMO-$(date +%s)"
AUTH=()
[ -n "${SG_API_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer $SG_API_TOKEN")
call() { echo; echo "### $1 $2"; curl -sS -X "$1" "${AUTH[@]}" -H 'Content-Type: application/json' "$BASE$2" ${3:+-d "$3"}; echo; }

call GET /v1/chain
# Part 10 example: 100,000.00 CNY (amounts in fen), 4 months, 12%/yr, sweep 43% (cap 63%), WeBank sole lender.
call POST /v1/loans '{
  "loanId": "'"$ID"'", "sellerId": "seller-douyin-778899", "tier": 2,
  "principal": 10000000, "annualRateBps": 1200, "tenorDays": 120,
  "sweepRateBps": 4300, "sweepCapBps": 6300, "graceDays": 20,
  "decision": { "doc": { "pd": 0.031, "segment": "N1", "limitCeiling": 12000000,
                         "agent2MemoSha256": "…", "agent3ChallengeSha256": "…", "humanTriggers": [] },
                "uri": "s3://webank-sg/decisions/'"$ID"'.json" } }'
# One click, one payment: straight to the suppliers.
call POST "/v1/loans/$ID/disburse" '{ "bankTxRef": "WB-'"$ID"'", "deliveryDays": 10,
  "payments": [ { "supplierId": "91310000MA1FL0001X", "amount": 6000000 },
                { "supplierId": "91440300MA5F00002Y", "amount": 4000000 } ] }'
call POST "/v1/loans/$ID/deliveries" '{ "line": 0, "waybillNo": "SF1234567890" }'
call POST "/v1/loans/$ID/repayments" '{ "amount": 2150000, "source": "proportional_sweep", "extRef": "SWP-'"$ID"'-1",
  "notice": { "doc": { "payout": 5000000, "rate": "43%", "explanation": "Sweep of today'"'"'s Douyin payout" } } }'
call POST "/v1/loans/$ID/floor-checkpoints" '{ "floorCumulative": 3000000 }'
call POST "/v1/loans/$ID/alerts" '{ "level": "yellow", "code": "FLOOR_REVIEW", "evidence": { "doc": { "agent": 5, "cause": "seasonal dip" } } }'
call GET "/v1/loans/$ID/payoff"
TOTAL=$(curl -sS "${AUTH[@]}" "$BASE/v1/loans/$ID/payoff" | sed -n 's/.*"total": *\([0-9]*\).*/\1/p')
# small buffer for interest accruing between quote and booking; excess is recorded as refundDue
call POST "/v1/loans/$ID/repayments" '{ "amount": '"$((TOTAL + 100))"', "source": "prepayment", "extRef": "PREPAY-'"$ID"'" }'
call GET /v1/portfolio
call GET "/v1/audit/anchors/$ID"
call GET "/v1/audit/anchors/$ID/verify"
