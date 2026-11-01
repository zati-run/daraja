#!/usr/bin/env bash
# Experiment: does a receipt journal on the Nanos image survive qemu being
# killed right after a write? Variant A: fsync only. Variant B: fsync plus
# sync(2). Uses the synthetic fixture only. Emits EVIDENCE lines.
set -uo pipefail
cd "$(dirname "$0")"
URL=http://127.0.0.1:8080/mcp
ev() { echo "EVIDENCE $*"; }
ACCEL=false; [ -w /dev/kvm ] && ACCEL=true
./notesfixture -listen 0.0.0.0:9443 -ips 127.0.0.1,10.0.2.2 -ca-out "$PWD/daraja/ca.pem" >fixture.log 2>&1 &
sleep 1
mkdir -p daraja/secrets daraja/data
cp credentials.json daraja/secrets/credentials.json
ops update --arm >/dev/null 2>&1
call() { ./daraja-mcpcheck -url $URL -token-file token-agent -call "$1" -args "$2" 2>&1 | grep tools/call | cut -c1-300; }
for variant in fsync syncall; do
  IMG=dur-$variant
  sed -e 's#/secrets/credentials.json#/daraja/secrets/credentials.json#' -e 's#/data/journal.jsonl#/daraja/data/journal.jsonl#' config.json >cfg-$variant.json
  if [ $variant = syncall ]; then
    python3 - cfg-$variant.json <<'PY' 2>/dev/null || sed -i 's#"-unikernel-file-modes"#"-unikernel-file-modes", "-journal-sync-all"#' cfg-$variant.json
import json,sys; p=sys.argv[1]; c=json.load(open(p)); c["Args"].append("-journal-sync-all"); json.dump(c,open(p,"w"))
PY
  fi
  ops build ./daraja-runtime -c cfg-$variant.json -i $IMG >build-$variant.log 2>&1 || { tail -3 build-$variant.log; continue; }
  for boot in 1 2; do
    ops run ./daraja-runtime -s -c cfg-$variant.json -i $IMG --accel=$ACCEL -p 8080 >run-$variant-$boot.log 2>&1 &
    for _ in $(seq 1 300); do ./daraja-mcpcheck -url $URL -token-file token-agent -timeout 2s >/dev/null 2>&1 && break; sleep 0.1; done
    if [ $boot = 1 ]; then
      call synthetic-notes__create_note '{"title":"durability","idempotency_key":"dur-1"}' | sed "s/^/MCP $variant-write /"
      pkill -9 -f qemu-system-aarch64; sleep 1
      ev "variant=$variant killed_immediately_after_write=true"
    else
      call daraja__receipt '{"tool":"synthetic-notes__create_note","idempotency_key":"dur-1"}' | sed "s/^/MCP $variant-receipt-after-reboot /"
      pkill -9 -f qemu-system-aarch64; sleep 1
    fi
  done
done
ev done
