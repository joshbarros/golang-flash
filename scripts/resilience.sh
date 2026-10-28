#!/usr/bin/env bash
# What Kubernetes buys: kill an API replica and the RabbitMQ pod while holds are pending and check that nothing is lost or done
# twice. A hold that is confirmed afterwards is sold exactly once with its pieces still held; a hold left alone through the same
# failures expires exactly once and its pieces go back on the shelf. Needs the cluster from `make cluster-up`.
set -euo pipefail

API="${API:-http://127.0.0.1:5232}"
CTX="${CTX:-kind-golang-flash}"
K="kubectl --context $CTX -n flash"
RUN="$(date +%s)$((RANDOM % 900 + 100))"
JSON='content-type: application/json'
say() { printf '  %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
jget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d$1)"; }
token_of() { python3 -c "import sys,json; print(json.load(sys.stdin)['accessToken'])"; }
# every call retries a few times: for a moment after a pod dies the Service may still send a request to it
retry() { for _ in $(seq 1 20); do if out="$("$@" 2>/dev/null)"; then printf '%s' "$out"; return 0; fi; sleep 1; done; return 1; }

login() { retry curl -sf -X POST "$API/api/auth/login" -H "$JSON" -d "{\"email\":\"admin@flash.local\",\"password\":\"${ADMIN_PASSWORD:-FlashAdmin2026}\"}" | token_of; }
register() { retry curl -sf -X POST "$API/api/auth/register" -H "$JSON" -d "{\"email\":\"$1-$RUN@example.com\",\"password\":\"Resil-Passw0rd1\"}" | token_of; }
ADM="Authorization: Bearer $(login)"
A="Authorization: Bearer $(register a)"
B="Authorization: Bearer $(register b)"
field() { body="$(retry curl -sf -H "$1" "$API/api/orders/$2")" && printf '%s' "$body" | jget "['$3']" || echo unknown; }
left() { retry curl -sf -H "$ADM" "$API/api/deals/RES-$RUN" | jget "['piecesLeft']"; }

echo "1. a deal of 10 pieces, and two pending holds of 2 pieces each (the demo holds for 12 seconds)"
retry curl -sf -X POST "$API/api/deals" -H "$JSON" -H "$ADM" -d "{\"code\":\"RES-$RUN\",\"name\":\"Resilience\",\"aisle\":\"Electronics\",\"brand\":\"Nimbus\",\"piecesTotal\":10,\"priceCents\":5000,\"maxPerShopper\":4}" >/dev/null || fail "could not open the deal"
hold() { retry curl -sf -X POST "$API/api/orders" -H "$JSON" -H "$1" -d "{\"dealCode\":\"RES-$RUN\",\"reference\":\"SHP-$RUN-$2\",\"pieces\":2,\"hold\":true}" | jget "['id']"; }
IDA="$(hold "$A" a)"; IDB="$(hold "$B" b)"
for _ in $(seq 1 30); do [ "$(field "$A" "$IDA" status)" = Held ] && [ "$(field "$B" "$IDB" status)" = Held ] && break; sleep 1; done
[ "$(field "$A" "$IDA" status)" = Held ] && [ "$(field "$B" "$IDB" status)" = Held ] || fail "both holds should be pending"
[ "$(left)" = 6 ] || fail "4 pieces are held, so 6 are left"
say "two pending holds, 6 pieces left"

echo "2. confirm one hold, then kill an API replica and the RabbitMQ pod while the other is still pending"
retry curl -sf -X POST -H "$A" "$API/api/orders/$IDA/confirm" >/dev/null || fail "the confirm should be accepted"
for _ in $(seq 1 20); do
  [ "$($K exec postgres-0 -- psql -U flash -d flash -tAc "select confirmed from deals.piece_holds where order_id = '$IDA'" 2>/dev/null)" = t ] && break
  sleep 0.5
done
$K delete "$($K get pods -l app.kubernetes.io/name=api -o name | head -1)" --wait=false | sed 's/^/  /'
$K delete pod rabbitmq-0 --wait=false | sed 's/^/  /'
say "the hold was pinned, then the replica and the broker died with the other hold still pending"

echo "3. everything comes back: the confirmed hold is sold once, the other one expires once"
$K rollout status statefulset/rabbitmq --timeout=180s >/dev/null
$K rollout status deployment/api --timeout=180s >/dev/null
for _ in $(seq 1 120); do
  [ "$(field "$A" "$IDA" status)" = Sold ] && [ "$(field "$B" "$IDB" status)" = Expired ] && break
  sleep 1
done
[ "$(field "$A" "$IDA" status)" = Sold ] || fail "the confirmed hold should be sold, got $(field "$A" "$IDA" status)"
[ "$(field "$B" "$IDB" status)" = Expired ] || fail "the unconfirmed hold should have expired, got $(field "$B" "$IDB" status)"
[ "$(left)" = 8 ] || fail "2 pieces are sold and 8 are on the shelf, got $(left)"
CERTS=0
for id in "$IDA" "$IDB"; do
  if curl -sf -H "$ADM" "$API/api/receipts/$id" >/dev/null 2>&1; then CERTS=$((CERTS + 1)); fi
done
[ "$CERTS" = 1 ] || fail "exactly one receipt may exist (the confirmed one), found $CERTS"
say "one sold with its receipt, one expired, 8 pieces left"
echo "resilience passed"
