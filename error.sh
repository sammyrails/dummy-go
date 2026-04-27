#!/usr/bin/env bash
# Fires a single 404 request so Tracelit captures one RecordError span.

BASE_URL="${1:-http://localhost:9090}"

curl -s -o /dev/null -w "HTTP %{http_code}\n" \
  -X GET "${BASE_URL}/products/999999"
