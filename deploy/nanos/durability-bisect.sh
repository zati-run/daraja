#!/usr/bin/env bash
# Bisect why the in-image receipt journal did not survive a qemu kill
# (RFC A11 on Nanos). For each scenario it records the qemu command line,
# the image file's sha256 and mtime before boot, after the kill and after
# the restart, and the journal as read from the image file itself between
# the kill and the restart. Synthetic fixture and credentials only.
#
# Scenarios:
#   immediate - kill right after the write answers
#   delayed   - wait 10 s after the write, then kill (async guest flush?)
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
ops version 2>&1 | sed 's/^/OPS-VERSION /'
ops image --help 2>&1 | sed 's/^/OPS-IMAGE-HELP /' | head -30
call() { ./daraja-mcpcheck -url $URL -token-file token-agent -call "$1" -args "$2" 2>&1 | grep tools/call | cut -c1-300; }
imgpath() { find ~/.ops/images -maxdepth 1 -name "$1*" -type f | head -1; }
stamp() { local p; p=$(imgpath "$1"); echo "path=$p sha256=$(sha256sum "$p" | cut -c1-16) mtime=$(stat -c %Y.%N "$p" 2>/dev/null | cut -c1-24) size=$(stat -c %s "$p")"; }
wait_up() { for _ in $(seq 1 300); do ./daraja-mcpcheck -url $URL -token-file token-agent -timeout 2s >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
qemu_cmd() { ps -eo args | grep '[q]emu-system' | head -1; }
extract() {
  local img=$1 tag=$2
  ops image ls "$img" /daraja/data 2>&1 | sed "s/^/IMAGE-LS-$tag /" | head -10
  rm -rf "x-$tag" && mkdir -p "x-$tag"
  if ops image cp "$img" /daraja/data/journal.jsonl "x-$tag/" >"x-$tag.log" 2>&1 && [ -f "x-$tag/journal.jsonl" ]; then
    ev "scenario=$tag journal_in_image_bytes=$(wc -c <"x-$tag/journal.jsonl") lines=$(wc -l <"x-$tag/journal.jsonl")"
    # Synthetic receipts only: state and key prefix, no bodies.
    sed -e 's/.*"key":"\([0-9a-f]\{12\}\)[^"]*".*"state":"\([a-z_]*\)".*/JOURNAL-'"$tag"' key=\1 state=\2/' "x-$tag/journal.jsonl"
  else
    ev "scenario=$tag journal_in_image=unreadable"; sed "s/^/IMAGE-CP-$tag /" "x-$tag.log" | head -5
  fi
}
CFG=cfg-bisect.json
sed -e 's#/secrets/credentials.json#/daraja/secrets/credentials.json#' -e 's#/data/journal.jsonl#/daraja/data/journal.jsonl#' config.json >$CFG
for s in immediate delayed; do
  IMG=bis-$s
  ops build ./daraja-runtime -c $CFG -i $IMG >build-$s.log 2>&1 || { tail -3 build-$s.log; continue; }
  ev "scenario=$s before_boot1 $(stamp $IMG)"
  ops run ./daraja-runtime -s -c $CFG -i $IMG --accel=$ACCEL -p 8080 >run-$s-1.log 2>&1 &
  wait_up || ev "scenario=$s boot1=timeout"
  echo "QEMU-CMD-$s-1 $(qemu_cmd)"
  call synthetic-notes__create_note '{"title":"bisect '$s'","idempotency_key":"bis-'$s'"}' | sed "s/^/MCP $s-write /"
  [ $s = delayed ] && sleep 10
  pkill -9 -f qemu-system-aarch64; sleep 1
  ev "scenario=$s after_kill $(stamp $IMG)"
  extract $IMG $s
  ops run ./daraja-runtime -s -c $CFG -i $IMG --accel=$ACCEL -p 8080 >run-$s-2.log 2>&1 &
  wait_up || ev "scenario=$s boot2=timeout"
  echo "QEMU-CMD-$s-2 $(qemu_cmd)"
  ev "scenario=$s after_boot2 $(stamp $IMG)"
  head -5 run-$s-2.log | sed "s/^/RUN2-$s /"
  call daraja__receipt '{"tool":"synthetic-notes__create_note","idempotency_key":"bis-'$s'"}' | sed "s/^/MCP $s-receipt-after-reboot /"
  pkill -9 -f qemu-system-aarch64; sleep 1
done
ev done
