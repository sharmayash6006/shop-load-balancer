#!/usr/bin/env bash
# Automated version of acceptance steps 9-14: runs a k6 load test while backends are disabled, re-enabled,
# stopped and restarted. Watch http://localhost:3001/admin/dashboard while it runs.
#   ./scripts/failure-test.sh            (needs: docker compose up already running, curl, python3)
set -euo pipefail
LB=${LB:-http://localhost:8080}
source .env 2>/dev/null || true
JAR=$(mktemp)
H='X-Requested-With: failure-test'
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
admin() { curl -fsS -b "$JAR" -H "$H" -X POST "$LB/_lb/api/$1" ${2:+-d "$2"} >/dev/null; }
show() { curl -fsS -b "$JAR" "$LB/_lb/api/backends" | python3 -c "
import sys,json
for b in json.load(sys.stdin): print('  %-10s %-11s requests=%-7d active=%-3d failed=%d' % (b['id'], b['status'], b['total_requests'], b['active_connections'], b['failed_requests']))"; }

curl -fsS -c "$JAR" -H "$H" -X POST "$LB/_lb/login" -d "{\"username\":\"${ADMIN_USER:-admin}\",\"password\":\"${ADMIN_PASSWORD:?}\"}" >/dev/null
step "Starting k6 (90s) in the background"
docker compose run --rm -T -e VUS=40 -e DURATION=90s -e RUN_ID=$RANDOM k6 run /tests/ecommerce-test.js >/tmp/k6-failure-test.log 2>&1 &
K6=$!
sleep 15; step "STEP 10: disable backend-3 (must receive no new requests, test continues)"
admin backends/backend-3/disable; sleep 2; show; sleep 8; show
step "STEP 11: enable backend-3 (health check -> HEALTHY -> traffic again)"
admin backends/backend-3/enable; sleep 6; show
step "STEP 12: stop backend-2 container (UNHEALTHY, traffic moves, test continues)"
docker compose stop backend-2 >/dev/null; sleep 8; show
step "STEP 13: stop ALL backends (HTTP 503, NO HEALTHY BACKENDS)"
docker compose stop backend-1 backend-3 >/dev/null; sleep 5; show
echo "  request -> HTTP $(curl -s -o /dev/null -w '%{http_code}' "$LB/api/products")"
step "STEP 14: start backends again"
docker compose start backend-1 backend-2 backend-3 >/dev/null; sleep 12; show
wait $K6 || true
step "k6 summary"; grep -E "http_req_failed|http_req_duration|http_reqs|checks|iterations" /tmp/k6-failure-test.log | head -12 || true
echo; echo "Failure reasons recorded by the load balancer:"
curl -fsS -b "$JAR" "$LB/_lb/api/stats" | python3 -c "import sys,json; print('  ', json.load(sys.stdin)['stats']['failure_reasons'])"
rm -f "$JAR"
