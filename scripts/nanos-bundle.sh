#!/usr/bin/env bash
# Builds a self-contained bundle for booting the runtime on Nanos with the
# synthetic notes fixture: linux/arm64 binaries, the ops config, a synthetic
# integration pointed at the fixture (https://10.0.2.2:9443 from the guest),
# synthetic principals/credentials, and nanos-run.sh. No real credentials.
#
#   scripts/nanos-bundle.sh [out-dir]     (default: private/nanos-bundle)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=${1:-$ROOT/private/nanos-bundle}
ARCH=${GOARCH:-arm64}
REV=$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)
rm -rf "$OUT" && mkdir -p "$OUT/b/daraja/integrations"
B=$OUT/b
for cmd in daraja-runtime daraja-mcpcheck; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -C "$ROOT" -trimpath -ldflags "-s -w -X main.version=$REV" -o "$B/$cmd" ./cmd/$cmd
done
CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -C "$ROOT" -trimpath -o "$B/notesfixture" ./internal/cmd/notesfixture
sed 's#http://127.0.0.1:1#https://10.0.2.2:9443#' "$ROOT/integration/testdata/notes.json" >"$B/daraja/integrations/notes.json"
# The browser-provider artifact is executed by a capability on the owner's
# Mac against a synthetic app there; the runtime only brokers it. The
# capability must hold the byte-identical artifact, so keep a copy outside
# the bundle too.
CAP_ORIGIN=${CAP_ORIGIN:-http://127.0.0.1:9555}
sed "s#http://127.0.0.1:1#$CAP_ORIGIN#" "$ROOT/integration/testdata/notes-browser.json" >"$B/daraja/integrations/notes-browser.json"
mkdir -p "$OUT/mac/integrations" && cp "$B/daraja/integrations/notes-browser.json" "$OUT/mac/integrations/"
cp "$ROOT/deploy/nanos/config.json" "$ROOT/deploy/nanos/nanos-run.sh" "$ROOT/deploy/nanos/durability-exp.sh" "$ROOT/deploy/nanos/durability-bisect.sh" "$B/"
tok() { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
A=$(tok); R=$(tok); C=$(tok)
printf %s "$A" >"$B/token-agent"; printf %s "$R" >"$B/token-reader"
printf %s "$C" >"$OUT/mac/token-capability"; cp "$B/token-agent" "$OUT/mac/token-agent"
cat >"$B/daraja/capabilities.json" <<EOF
{"capabilities":[{"id":"mac","token_sha256":"$(printf %s "$C" | shasum -a 256 | cut -d' ' -f1)","bindings":["mac-notes"]}]}
EOF
cat >"$OUT/mac/bindings.json" <<EOF
{"bindings":[{"name":"mac-notes","profile":"$OUT/mac/profile","grants":{"synthetic-notes-browser":["get_note","find_note_by_ref","create_note"]}}]}
EOF
echo '{"bindings":{"mac-notes":{"synthetic":true,"values":{"username":"synthetic-user","password":"synthetic-pass"}}}}' >"$OUT/mac/credentials.json"
chmod 600 "$OUT/mac/credentials.json"
sha() { printf %s "$1" | shasum -a 256 | cut -d' ' -f1; }
cat >"$B/daraja/principals.json" <<EOF
{"principals":[
 {"id":"agent","token_sha256":"$(sha "$A")","grants":[{"integration":"synthetic-notes","operations":["get_note","find_note_by_ref","create_note"],"credential":"notes"},{"integration":"synthetic-notes-browser","operations":["get_note","find_note_by_ref","create_note"],"credential":"mac-notes"}]},
 {"id":"reader","token_sha256":"$(sha "$R")","grants":[{"integration":"synthetic-notes","operations":["get_note"],"credential":"notes"}]}
]}
EOF
echo '{"bindings":{"notes":{"synthetic":true,"values":{"username":"synthetic-user","password":"synthetic-pass"}}}}' >"$B/credentials.json"
chmod 600 "$B/credentials.json"
COPYFILE_DISABLE=1 tar -C "$B" -czf "$OUT/bundle.tgz" .
echo "bundle: $OUT/bundle.tgz (rev $REV, $ARCH)"
