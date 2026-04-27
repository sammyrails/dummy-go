#!/usr/bin/env bash
# =============================================================================
# test.sh — Products CRUD API test runner
#
# Usage:
#   ./test.sh                        # happy-path CRUD only
#   ./test.sh error                  # error / edge-case endpoints only
#   ./test.sh loop                   # errors once, then happy-path loops forever
#   ./test.sh loop 5                 # errors once, then happy-path loops N times
#   ./test.sh http://localhost:9090  # custom base URL, happy path
#   ./test.sh error http://host:port # custom base URL, error mode
#   (arguments can appear in any order)
#
# Tracelit observability:
#   Every request generates traces, logs, and metrics in the Tracelit dashboard.
#   Error mode specifically exercises span.RecordError / RecordPanic so you can
#   see what the SDK surfaces in the Errors and Traces views.
#   Loop mode fires errors once then generates continuous happy-path traffic.
#
# Pre-requisites:
#   1. Fill in .env  (TRACELIT_API_KEY, DATABASE_URL)
#   2. createdb products_db
#   3. go run .
# =============================================================================

set -euo pipefail

# ── parse arguments (order-independent) ──────────────────────────────────────
MODE="happy"
BASE_URL="http://localhost:9090"
LOOP_COUNT=0   # 0 = infinite

for arg in "$@"; do
  case "$arg" in
    error) MODE="error" ;;
    loop)  MODE="loop" ;;
    http*) BASE_URL="$arg" ;;
    [0-9]*) LOOP_COUNT="$arg" ;;
    *) echo "Unknown argument: $arg  (expected 'error', 'loop', a URL, or a loop count)" >&2; exit 1 ;;
  esac
done

# ── colours ───────────────────────────────────────────────────────────────────
BOLD='\033[1m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
CYAN='\033[0;36m'
DIM='\033[2m'
RESET='\033[0m'

PASS=0
FAIL=0

# ── helpers ───────────────────────────────────────────────────────────────────
section() {
  echo "" >&2
  echo -e "${BOLD}${CYAN}══════════════════════════════════════════${RESET}" >&2
  echo -e "${BOLD}${CYAN}  $1${RESET}" >&2
  echo -e "${BOLD}${CYAN}══════════════════════════════════════════${RESET}" >&2
}

run() {
  local label="$1"
  local expected_status="$2"
  shift 2

  echo "" >&2
  echo -e "${BOLD}▶ ${label}${RESET}" >&2
  echo -e "${DIM}  $(echo "$@" | sed "s|${BASE_URL}||g")${RESET}" >&2

  local http_code body
  body=$(curl -s -w "\n__STATUS__%{http_code}__STATUS__" "$@" 2>&1)
  http_code=$(echo "$body" | grep -o '__STATUS__[0-9]*__STATUS__' | grep -o '[0-9]*')
  body=$(echo "$body" | sed 's/__STATUS__[0-9]*__STATUS__//')

  local pretty
  pretty=$(echo "$body" | python3 -m json.tool 2>/dev/null || echo "$body")

  if [ "$http_code" = "$expected_status" ]; then
    echo -e "  ${GREEN}✓ HTTP ${http_code}${RESET}" >&2
    PASS=$((PASS + 1))
  else
    echo -e "  ${RED}✗ Expected HTTP ${expected_status}, got ${http_code}${RESET}" >&2
    FAIL=$((FAIL + 1))
  fi

  echo -e "${DIM}${pretty}${RESET}" >&2
  echo "$body"
}

extract() {
  echo "$1" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$2',''))" 2>/dev/null || true
}

# ── wait for server ───────────────────────────────────────────────────────────
echo -e "${BOLD}Mode: ${YELLOW}${MODE}${RESET}  •  ${BOLD}URL: ${BASE_URL}${RESET}" >&2
echo -e "${BOLD}Waiting for server...${RESET}" >&2
for i in $(seq 1 15); do
  if curl -sf "${BASE_URL}/health" > /dev/null 2>&1; then
    echo -e "${GREEN}Server is up!${RESET}" >&2
    break
  fi
  if [ "$i" -eq 15 ]; then
    echo -e "${RED}Server did not respond. Is 'go run .' running?${RESET}" >&2
    exit 1
  fi
  sleep 1
done

# =============================================================================
# HAPPY PATH
# =============================================================================
if [ "$MODE" = "happy" ]; then

  section "1. HEALTH CHECK"

  run "Health check" "200" \
    -X GET "${BASE_URL}/health"

  # ── create ──────────────────────────────────────────────────────────────────
  section "2. CREATE PRODUCTS"

  BODY1=$(run "Create — Mechanical Keyboard" "201" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"name":"Mechanical Keyboard","description":"Full-size TKL with Cherry MX Blue switches","price":89.99,"stock":150}')

  BODY2=$(run "Create — Wireless Mouse" "201" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"name":"Wireless Mouse","description":"Ergonomic 2.4GHz wireless mouse, 6 buttons","price":34.50,"stock":200}')

  BODY3=$(run "Create — USB-C Hub" "201" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"name":"USB-C Hub","description":"7-in-1 USB-C hub with HDMI, USB 3.0, SD card reader","price":49.00,"stock":75}')

  BODY4=$(run "Create — Monitor Stand" "201" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"name":"Monitor Stand","description":"Adjustable aluminium monitor stand with cable management","price":29.99,"stock":0}')

  ID1=$(extract "$BODY1" "id")
  ID2=$(extract "$BODY2" "id")
  ID3=$(extract "$BODY3" "id")
  ID4=$(extract "$BODY4" "id")
  echo -e "\n  ${YELLOW}Created IDs: ${ID1}, ${ID2}, ${ID3}, ${ID4}${RESET}" >&2

  # ── list ────────────────────────────────────────────────────────────────────
  section "3. LIST PRODUCTS"

  run "List all products" "200" \
    -X GET "${BASE_URL}/products"

  run "List with pagination (limit=2, offset=0)" "200" \
    -X GET "${BASE_URL}/products?limit=2&offset=0"

  run "List page 2 (limit=2, offset=2)" "200" \
    -X GET "${BASE_URL}/products?limit=2&offset=2"

  # ── get ─────────────────────────────────────────────────────────────────────
  section "4. GET PRODUCT BY ID"

  run "Get product ${ID1}" "200" \
    -X GET "${BASE_URL}/products/${ID1}"

  run "Get product ${ID3}" "200" \
    -X GET "${BASE_URL}/products/${ID3}"

  # ── update ──────────────────────────────────────────────────────────────────
  section "5. UPDATE PRODUCT"

  run "Update price + stock for ${ID1}" "200" \
    -X PUT "${BASE_URL}/products/${ID1}" \
    -H "Content-Type: application/json" \
    -d '{"price":79.99,"stock":120}'

  run "Update name + description for ${ID2}" "200" \
    -X PUT "${BASE_URL}/products/${ID2}" \
    -H "Content-Type: application/json" \
    -d '{"name":"Wireless Mouse Pro","description":"Ergonomic wireless mouse with vertical grip"}'

  run "Verify update persisted for ${ID1}" "200" \
    -X GET "${BASE_URL}/products/${ID1}"

  # ── delete ──────────────────────────────────────────────────────────────────
  section "6. DELETE PRODUCT"

  run "Delete product ${ID4}" "204" \
    -X DELETE "${BASE_URL}/products/${ID4}"

  run "Confirm product ${ID4} is gone (404)" "404" \
    -X GET "${BASE_URL}/products/${ID4}"

  # ── slow search ─────────────────────────────────────────────────────────────
  section "7. SLOW SEARCH  (watch latency in Tracelit traces)"

  echo -e "\n  ${YELLOW}⏳ pg_sleep(1) simulates a full-table scan — ~1s per query${RESET}" >&2

  run "Search 'keyboard'" "200" \
    -X GET "${BASE_URL}/products/search?q=keyboard"

  run "Search 'usb'" "200" \
    -X GET "${BASE_URL}/products/search?q=usb"

fi  # end happy

# =============================================================================
# ERROR MODE
# =============================================================================
if [ "$MODE" = "error" ]; then

  echo -e "\n  ${YELLOW}All of the following requests are expected to fail.${RESET}" >&2
  echo -e "  ${YELLOW}Open Tracelit → Traces → Errors to see them captured by the SDK.${RESET}" >&2

  # ── validation errors ────────────────────────────────────────────────────────
  section "1. INPUT VALIDATION ERRORS"

  run "Create — missing name (422)" "422" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"description":"no name","price":10.00,"stock":5}'

  run "Create — negative price (422)" "422" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"name":"Bad Product","price":-5.00,"stock":10}'

  run "Create — malformed JSON (400)" "400" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{name: this is not json}'

  run "Update — malformed JSON (400)" "400" \
    -X PUT "${BASE_URL}/products/1" \
    -H "Content-Type: application/json" \
    -d 'not json at all'

  run "Search — missing query param (400)" "400" \
    -X GET "${BASE_URL}/products/search"

  # ── not found ────────────────────────────────────────────────────────────────
  section "2. NOT FOUND ERRORS"

  run "Get product 999999 (404)" "404" \
    -X GET "${BASE_URL}/products/999999"

  run "Update product 999999 (404)" "404" \
    -X PUT "${BASE_URL}/products/999999" \
    -H "Content-Type: application/json" \
    -d '{"price":1.00}'

  run "Delete product 999999 (404)" "404" \
    -X DELETE "${BASE_URL}/products/999999"

  # ── invalid IDs ───────────────────────────────────────────────────────────────
  section "3. INVALID ID ERRORS"

  run "Get  /products/abc  (400)" "400" \
    -X GET "${BASE_URL}/products/abc"

  run "Get  /products/-1   (400)" "400" \
    -X GET "${BASE_URL}/products/-1"

  run "Get  /products/0    (400)" "400" \
    -X GET "${BASE_URL}/products/0"

  run "PUT  /products/xyz  (400)" "400" \
    -X PUT "${BASE_URL}/products/xyz" \
    -H "Content-Type: application/json" \
    -d '{"price":1.00}'

  run "DELETE /products/foo (400)" "400" \
    -X DELETE "${BASE_URL}/products/foo"

  # ── intentional SDK error demos ───────────────────────────────────────────────
  section "4. SDK ERROR CAPTURE DEMOS  (/error/*)"

  run "/error/notfound   — RecordError on a 404" "404" \
    -X GET "${BASE_URL}/error/notfound"

  run "/error/validation — RecordError on validation failure" "422" \
    -X GET "${BASE_URL}/error/validation"

  run "/error/db         — RecordError on bad SQL query" "500" \
    -X GET "${BASE_URL}/error/db"

  run "/error/timeout    — RecordError on context deadline exceeded" "504" \
    -X GET "${BASE_URL}/error/timeout"

  echo -e "\n  ${YELLOW}⚠️  Next request panics. RecordPanic() is called by the recovery middleware.${RESET}" >&2
  echo -e "  ${YELLOW}   The server must stay alive after this.${RESET}\n" >&2

  set +e
  run "/error/panic      — RecordPanic, server survives" "500" \
    -X GET "${BASE_URL}/error/panic"
  set -e

  echo -e "\n  ${GREEN}✓ Server survived the panic!${RESET}" >&2

  run "Health check — confirm server is still up after panic" "200" \
    -X GET "${BASE_URL}/health"

fi  # end error

# =============================================================================
# LOOP MODE  — errors once, then happy-path in a loop
# =============================================================================
if [ "$MODE" = "loop" ]; then

  echo -e "\n  ${YELLOW}Loop mode: firing error scenarios once, then looping happy path.${RESET}" >&2
  if [ "$LOOP_COUNT" -eq 0 ]; then
    echo -e "  ${YELLOW}Press Ctrl-C to stop.${RESET}" >&2
  else
    echo -e "  ${YELLOW}Will run happy path ${LOOP_COUNT} time(s).${RESET}" >&2
  fi

  # ── errors: run exactly once ───────────────────────────────────────────────
  section "ERROR SCENARIOS  (once)"

  echo -e "\n  ${YELLOW}All of the following requests are expected to fail.${RESET}" >&2

  run "Create — missing name (422)" "422" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"description":"no name","price":10.00,"stock":5}'

  run "Create — negative price (422)" "422" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{"name":"Bad Product","price":-5.00,"stock":10}'

  run "Create — malformed JSON (400)" "400" \
    -X POST "${BASE_URL}/products" \
    -H "Content-Type: application/json" \
    -d '{name: this is not json}'

  run "Get product 999999 (404)" "404" \
    -X GET "${BASE_URL}/products/999999"

  run "Get  /products/abc  (400)" "400" \
    -X GET "${BASE_URL}/products/abc"

  run "/error/notfound   — RecordError on a 404" "404" \
    -X GET "${BASE_URL}/error/notfound"

  run "/error/validation — RecordError on validation failure" "422" \
    -X GET "${BASE_URL}/error/validation"

  run "/error/db         — RecordError on bad SQL query" "500" \
    -X GET "${BASE_URL}/error/db"

  run "/error/timeout    — RecordError on context deadline exceeded" "504" \
    -X GET "${BASE_URL}/error/timeout"

  echo -e "\n  ${YELLOW}⚠️  Next request panics. RecordPanic() is called by the recovery middleware.${RESET}" >&2
  set +e
  run "/error/panic      — RecordPanic, server survives" "500" \
    -X GET "${BASE_URL}/error/panic"
  set -e
  echo -e "\n  ${GREEN}✓ Server survived the panic!${RESET}" >&2

  # ── happy path: loop ───────────────────────────────────────────────────────
  _iteration=0
  while true; do
    _iteration=$((_iteration + 1))

    section "HAPPY PATH — iteration ${_iteration}"

    run "Health check" "200" -X GET "${BASE_URL}/health"

    BODY1=$(run "Create — Mechanical Keyboard" "201" \
      -X POST "${BASE_URL}/products" \
      -H "Content-Type: application/json" \
      -d '{"name":"Mechanical Keyboard","description":"Full-size TKL with Cherry MX Blue switches","price":89.99,"stock":150}')

    BODY2=$(run "Create — Wireless Mouse" "201" \
      -X POST "${BASE_URL}/products" \
      -H "Content-Type: application/json" \
      -d '{"name":"Wireless Mouse","description":"Ergonomic 2.4GHz wireless mouse, 6 buttons","price":34.50,"stock":200}')

    BODY3=$(run "Create — USB-C Hub" "201" \
      -X POST "${BASE_URL}/products" \
      -H "Content-Type: application/json" \
      -d '{"name":"USB-C Hub","description":"7-in-1 USB-C hub with HDMI, USB 3.0, SD card reader","price":49.00,"stock":75}')

    BODY4=$(run "Create — Monitor Stand" "201" \
      -X POST "${BASE_URL}/products" \
      -H "Content-Type: application/json" \
      -d '{"name":"Monitor Stand","description":"Adjustable aluminium monitor stand with cable management","price":29.99,"stock":0}')

    ID1=$(extract "$BODY1" "id")
    ID2=$(extract "$BODY2" "id")
    ID3=$(extract "$BODY3" "id")
    ID4=$(extract "$BODY4" "id")
    echo -e "\n  ${YELLOW}Created IDs: ${ID1}, ${ID2}, ${ID3}, ${ID4}${RESET}" >&2

    run "List all products" "200" -X GET "${BASE_URL}/products"

    run "Get product ${ID1}" "200" -X GET "${BASE_URL}/products/${ID1}"

    run "Update price + stock for ${ID1}" "200" \
      -X PUT "${BASE_URL}/products/${ID1}" \
      -H "Content-Type: application/json" \
      -d '{"price":79.99,"stock":120}'

    run "Update name for ${ID2}" "200" \
      -X PUT "${BASE_URL}/products/${ID2}" \
      -H "Content-Type: application/json" \
      -d '{"name":"Wireless Mouse Pro","description":"Ergonomic wireless mouse with vertical grip"}'

    run "Delete product ${ID4}" "204" -X DELETE "${BASE_URL}/products/${ID4}"

    run "Confirm ${ID4} gone (404)" "404" -X GET "${BASE_URL}/products/${ID4}"

    run "Search 'keyboard'" "200" -X GET "${BASE_URL}/products/search?q=keyboard"

    echo -e "\n  ${DIM}── iteration ${_iteration} complete ──${RESET}" >&2

    if [ "$LOOP_COUNT" -gt 0 ] && [ "$_iteration" -ge "$LOOP_COUNT" ]; then
      break
    fi

    sleep 2
  done

fi  # end loop

# =============================================================================
# SUMMARY
# =============================================================================
section "SUMMARY"

echo -e "${BOLD}  Mode: ${YELLOW}${MODE}${RESET}  •  ${BOLD}${GREEN}${PASS} passed${RESET}  ${RED}${FAIL} failed${RESET}" >&2
echo "" >&2
echo -e "  ${CYAN}Open https://app.tracelit.io to see:${RESET}" >&2
if [ "$MODE" = "happy" ]; then
  echo -e "  ${DIM}  • Traces for each CRUD request with nested DB spans${RESET}" >&2
  echo -e "  ${DIM}  • Slow search latency visible in the trace waterfall${RESET}" >&2
elif [ "$MODE" = "loop" ]; then
  echo -e "  ${DIM}  • Errors tab: RecordError / RecordPanic from the one-time error pass${RESET}" >&2
  echo -e "  ${DIM}  • Continuous happy-path traces and metrics from the loop${RESET}" >&2
else
  echo -e "  ${DIM}  • Errors tab: every span.RecordError and RecordPanic captured${RESET}" >&2
  echo -e "  ${DIM}  • Trace IDs in each error response body link back to the span${RESET}" >&2
fi
echo -e "  ${DIM}  • Logs correlated to trace IDs via the slog bridge${RESET}" >&2
echo -e "  ${DIM}  • Runtime metrics: goroutines, heap, GC (automatic from the SDK)${RESET}" >&2
echo "" >&2
