#!/usr/bin/env bash
# Checks the edge limits in envoy.yaml against a running stack; RESPONDER=echo
# is enough. Run from the repo root: bash infra/envoy/check-edge.sh
set -u
BASE=${BASE:-http://localhost:8080}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf '\x00\x00\x00\x00\x00' > "$tmp/empty"         # one empty gRPC-Web frame
head -c 70000 /dev/zero > "$tmp/big"
fail=0

ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; fail=1; }

# post PATH FILE [curl args...] prints "<http code> <grpc-status header>".
# A trailers-only response puts grpc-status in the headers; success leaves it
# in the body, so an empty status means the call went through.
post() {
  local path=$1 file=$2; shift 2
  curl -s -o /dev/null -D "$tmp/h" -w '%{http_code}' -X POST "$BASE$path" \
    -H 'content-type: application/grpc-web+proto' -H 'x-grpc-web: 1' "$@" \
    --data-binary @"$file" > "$tmp/code"
  local st
  st=$(tr -d '\r' < "$tmp/h" | awk -F': ' 'tolower($1)=="grpc-status"{print $2}')
  echo "$(cat "$tmp/code") ${st:-none}"
}

# 1. Body limit, before the rate-limit bursts below empty the bucket.
r=$(post /chat.v1.ChatService/Chat "$tmp/big")
case $r in 413*|*\ 8|*\ 13|*\ 14) ok "70 KB body rejected ($r)";; *) bad "70 KB body not rejected ($r)";; esac

# 2. Parked order path is not routed.
r=$(post /orders.OrderService/SendPacket "$tmp/empty")
case $r in 404*|*\ 12) ok "order route removed ($r)";; *) bad "order route still served ($r)";; esac

# 3. CORS: the dev origin is allowed, a foreign one is not.
cors() {
  curl -s -o /dev/null -D - -X OPTIONS "$BASE/chat.v1.ChatService/Chat" \
    -H "Origin: $1" -H 'Access-Control-Request-Method: POST' \
    -H 'Access-Control-Request-Headers: content-type,x-grpc-web' | tr -d '\r' |
    grep -i '^access-control-allow-origin' || true
}
[ -n "$(cors http://localhost:5173)" ] && ok "dev origin allowed" || bad "dev origin not allowed"
[ -z "$(cors https://evil.example)" ] && ok "foreign origin refused" || bad "foreign origin allowed"

# 4. Burst past the per-IP bucket (10, refilling 1 per 2s).
limited=0
for _ in $(seq 1 15); do
  case $(post /chat.v1.ChatService/Warmup "$tmp/empty") in *\ 8) limited=$((limited + 1));; esac
done
[ "$limited" -gt 0 ] && ok "burst rate-limited ($limited of 15)" || bad "burst never rate-limited"

# 5. A forged X-Forwarded-For must not buy a fresh bucket.
spoofed=0
for i in $(seq 1 5); do
  case $(post /chat.v1.ChatService/Warmup "$tmp/empty" -H "X-Forwarded-For: 198.51.100.$i") in *\ 8) spoofed=$((spoofed + 1));; esac
done
[ "$spoofed" -eq 5 ] && ok "forged X-Forwarded-For ignored" || bad "forged X-Forwarded-For got through ($((5 - spoofed)) of 5)"

exit $fail
