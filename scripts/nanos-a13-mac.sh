#!/usr/bin/env bash
# Mac side of acceptance row A13: a synthetic app on loopback, a capability
# host on this Mac that dials out to the runtime on Nanos, and MCP calls to
# that runtime from a client that does not use the MCP SDK. Prints EVIDENCE
# lines and writes the done marker the pod waits for.
#
#   scripts/nanos-a13-mac.sh RUNTIME_URL [bundle-dir] [serve-dir]
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
RT=${1:?runtime URL, for example http://192.168.1.20:18080}
B=${2:-$ROOT/private/nanos-bundle}
SERVE=${3:-$ROOT/private/nanos-serve}
M=$B/mac
W=$(mktemp -d)
ev() { echo "EVIDENCE $*"; }
cleanup() { kill $FIX $CAP 2>/dev/null; wait 2>/dev/null; }
trap cleanup EXIT

"$ROOT/bin/notesfixture" -plain -listen 127.0.0.1:9555 >"$W/fixture.log" 2>&1 &
FIX=$!
"$ROOT/bin/daraja" capability -id mac -runtime "$RT" -allow-insecure-runtime -token-file "$M/token-capability" \
  -integrations "$M/integrations" -bindings "$M/bindings.json" -credentials "$M/credentials.json" -headless >"$W/capability.log" 2>&1 &
CAP=$!
call() { "$ROOT/bin/daraja-mcpcheck" -url "$RT/mcp" -token-file "$M/token-agent" -call "$1" -args "$2" 2>&1; }
posts() { grep -c 'fixture POST /api/notes$' "$W/fixture.log" || true; }
for _ in $(seq 1 60); do grep -q 'capability registered\|serving' "$W/capability.log" && break; sleep 0.5; done
sleep 2
for i in 1 2 3; do call synthetic-notes-browser__get_note '{"id":1}' | sed "s/^/MCP a13-read$i /"; done
ev "a13_posts_before=$(posts)"
call synthetic-notes-browser__create_note '{"title":"Brokered from Nanos","idempotency_key":"a13-w1"}' | sed 's/^/MCP a13-write /'
call synthetic-notes-browser__create_note '{"title":"Brokered from Nanos","idempotency_key":"a13-w1"}' | sed 's/^/MCP a13-duplicate /'
ev "a13_posts_after=$(posts)"
ev "a13_capability_log=$(grep -c 'executed' "$W/capability.log") executed lines"
grep -E 'executed|registered|poll failed' "$W/capability.log" | sed 's/^/CAPLOG /' | tail -10
date -u +%FT%TZ >"$SERVE/done"
