#!/usr/bin/env bash
# scripts/smoke.sh — exercises every public endpoint against a running server.
# Usage: BASE=http://localhost:8081 bash scripts/smoke.sh

set -euo pipefail

BASE="${BASE:-http://localhost:8081}"

red()   { printf "\033[31m%s\033[0m\n" "$*"; }
green() { printf "\033[32m%s\033[0m\n" "$*"; }
blue()  { printf "\033[34m%s\033[0m\n" "$*"; }

hr() { printf "\n%s\n" "----------------------------------------"; }

assert_status() {
    local want="$1" got="$2" label="$3"
    if [ "$got" = "$want" ]; then
        green "OK   $label  (status $got)"
    else
        red   "FAIL $label  (status $got, want $want)"
        exit 1
    fi
}

hr; blue "[1] GET /healthz"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" "$BASE/healthz")
assert_status 200 "$code" "/healthz"

hr; blue "[2] GET /readyz"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" "$BASE/readyz")
assert_status 200 "$code" "/readyz"

hr; blue "[3] POST /api/v1/links with custom alias"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" -X POST "$BASE/api/v1/links" \
    -H 'Content-Type: application/json' \
    -d '{"url":"https://example.com/very/long/path","custom_alias":"hello"}')
assert_status 201 "$code" "POST custom alias"
cat /tmp/sm.body; echo

hr; blue "[4] POST /api/v1/links auto-code"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" -X POST "$BASE/api/v1/links" \
    -H 'Content-Type: application/json' \
    -d '{"url":"https://golang.org"}')
assert_status 201 "$code" "POST auto-code"
cat /tmp/sm.body; echo

hr; blue "[5] POST invalid url (expect 400)"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" -X POST "$BASE/api/v1/links" \
    -H 'Content-Type: application/json' \
    -d '{"url":"javascript:alert(1)"}')
assert_status 400 "$code" "POST invalid url"
cat /tmp/sm.body; echo

hr; blue "[6] GET /api/v1/links/hello"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" "$BASE/api/v1/links/hello")
assert_status 200 "$code" "GET by code"
cat /tmp/sm.body; echo

hr; blue "[7] GET /hello (redirect)"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" "$BASE/hello")
assert_status 301 "$code" "redirect"
grep -i '^location:' /tmp/sm.body || true

hr; blue "[8] GET /api/v1/links (list)"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" "$BASE/api/v1/links")
assert_status 200 "$code" "list"
cat /tmp/sm.body; echo

hr; blue "[9] DELETE /api/v1/links/hello"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" -X DELETE "$BASE/api/v1/links/hello")
assert_status 204 "$code" "delete"

hr; blue "[10] GET /hello after delete (expect 410)"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" "$BASE/hello")
assert_status 410 "$code" "redirect after delete"

hr; blue "[11] GET /nope (expect 404)"
code=$(curl -s -o /tmp/sm.body -w "%{http_code}" "$BASE/nope")
assert_status 404 "$code" "missing code"

hr; green "All smoke tests passed."
