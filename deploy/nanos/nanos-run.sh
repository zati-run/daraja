#!/usr/bin/env bash
# Boots the Daraja runtime as a Nanos unikernel with ops/qemu and exercises
# it over MCP with daraja-mcpcheck (a client that does not use the MCP SDK).
# Runs inside a Linux host or pod that has ops, qemu-system-aarch64, curl
# and the bundle produced by scripts/nanos-bundle.sh. Emits EVIDENCE lines.
set -uo pipefail
cd "$(dirname "$0")"
W=$PWD
IMG=daraja-runtime
URL=http://127.0.0.1:8080/mcp
now_ms() { date +%s%3N; }
ev() { echo "EVIDENCE $*"; }
die() { echo "FAIL $*"; exit 1; }

ev "host=$(uname -srm) ops=$(ops version | tr '\n' ' ') qemu=\"$(qemu-system-aarch64 --version | head -1)\""
ACCEL=true
if [ -c /dev/kvm ] && [ -r /dev/kvm ] && [ -w /dev/kvm ]; then ev "accel=kvm"; else ACCEL=false; ev "accel=tcg (no usable /dev/kvm)"; fi

./notesfixture -listen 0.0.0.0:9443 -ips 127.0.0.1,10.0.2.2 -ca-out "$W/daraja/ca.pem" -slow-for 8s >fixture.log 2>&1 &
FIX=$!
sleep 1
kill -0 $FIX || die "fixture did not start: $(cat fixture.log)"
posts() { grep -c 'fixture POST /api/notes$' fixture.log || true; }

# Credentials and the journal live on volumes, not in the image.
mkdir -p vol-secrets vol-data
cp credentials.json vol-secrets/credentials.json && chmod 600 vol-secrets/credentials.json
ops volume create secrets -d vol-secrets -s 8m >volume.log 2>&1 || { cat volume.log; die "volume secrets"; }
ops volume create data -d vol-data -s 32m >>volume.log 2>&1 || { cat volume.log; die "volume data"; }
MOUNTS=(--mounts secrets:/secrets --mounts data:/data)
CFG=config.json

T0=$(now_ms)
# The release kernel for arm64 may be missing; fall back to "ops update
# --arm", then to the nightly kernel (-n). The mode used is recorded.
NANOS=()
if ! ops build ./daraja-runtime -c "$CFG" -i "$IMG" "${MOUNTS[@]}" >build.log 2>&1; then
  tail -3 build.log
  ops update --arm >update.log 2>&1 || tail -3 update.log
  if ops build ./daraja-runtime -c "$CFG" -i "$IMG" "${MOUNTS[@]}" >build.log 2>&1; then
    ev "kernel=release-after-update"
  else
    tail -3 build.log
    NANOS=(-n)
    ops build ./daraja-runtime -n -c "$CFG" -i "$IMG" "${MOUNTS[@]}" >build.log 2>&1 || { tail -5 build.log; find ~/.ops -maxdepth 3 | head -40; die "ops build"; }
    ev "kernel=nightly"
  fi
else
  ev "kernel=release"
fi
ev "build_ms=$(( $(now_ms) - T0 ))"
IMGFILE=$(ls -1 ~/.ops/images/${IMG}* 2>/dev/null | head -1)
ev "image_file=$IMGFILE image_bytes=$(stat -c %s "$IMGFILE") image_allocated_bytes=$(( $(stat -c %b "$IMGFILE") * 512 )) runtime_elf_bytes=$(stat -c %s daraja-runtime)"
ops image env "$IMG" 2>&1 | sed 's/^/IMAGE-ENV /'
ops image ls "$IMG" / 2>&1 | sed 's/^/IMAGE-LS /' | head -20

# in_image_fallback rebuilds without volumes when this ops/qemu pair cannot
# attach them (observed on arm64: the qemu virt machine has no scsi0.0 bus).
# Only the synthetic credentials exist here; the mode is recorded.
in_image_fallback() {
  ev "volumes=unsupported reason=\"$(grep -o "Bus 'scsi0.0' not found" "$1" | head -1)\" fallback=credentials_and_journal_in_image"
  mkdir -p daraja/secrets daraja/data
  cp credentials.json daraja/secrets/credentials.json && chmod 600 daraja/secrets/credentials.json
  sed -e 's#/secrets/credentials.json#/daraja/secrets/credentials.json#' -e 's#/data/journal.jsonl#/daraja/data/journal.jsonl#' config.json >config-in-image.json
  CFG=config-in-image.json
  MOUNTS=()
  ops build ./daraja-runtime "${NANOS[@]}" -c "$CFG" -i "$IMG" >build.log 2>&1 || { tail -5 build.log; die "ops build (in-image)"; }
}

boot() { # $1 = log name
  boot_once "$1" && return 0
  if [ ${#MOUNTS[@]} -gt 0 ] && grep -q "scsi0.0" "$1"; then
    in_image_fallback "$1"
    boot_once "$1" && return 0
  fi
  cat "$1"; die "VM did not serve MCP"
}

boot_once() { # $1 = log name
  T0=$(now_ms)
  ops run ./daraja-runtime "${NANOS[@]}" -s -c "$CFG" -i "$IMG" "${MOUNTS[@]}" --accel=$ACCEL -p 8080 >"$1" 2>&1 &
  VM=$!
  for _ in $(seq 1 600); do
    if ./daraja-mcpcheck -url $URL -token-file token-agent -timeout 2s >/dev/null 2>&1; then
      ev "boot=$1 cold_start_to_mcp_initialize_ms=$(( $(now_ms) - T0 ))"
      return 0
    fi
    kill -0 $VM 2>/dev/null || { tail -5 "$1"; echo "VM exited during boot"; return 1; }
    sleep 0.1
  done
  tail -20 "$1"; echo "VM did not serve MCP within 60s"; return 1
}
stop_vm() { pkill -9 -f qemu-system-aarch64; wait $VM 2>/dev/null; sleep 1; }

call() { ./daraja-mcpcheck -url $URL -token-file "$1" -call "$2" -args "$3" 2>&1; }

boot boot1.log
./daraja-mcpcheck -url $URL -token-file token-agent -list | sed 's/^/MCP /'

for i in 1 2 3 4 5; do call token-agent synthetic-notes__get_note '{"id":1}' | sed "s/^/MCP read$i /"; done
call token-agent synthetic-notes__get_note '{"id":0}' | sed 's/^/MCP invalid-input /'
call token-reader synthetic-notes__create_note '{"title":"denied","idempotency_key":"r-1"}' | sed 's/^/MCP scope-denied /'
ev "posts_before_write=$(posts)"
call token-agent synthetic-notes__create_note '{"title":"Nanos synthetic note","idempotency_key":"nanos-w1"}' | sed 's/^/MCP write /'
call token-agent synthetic-notes__create_note '{"title":"Nanos synthetic note","idempotency_key":"nanos-w1"}' | sed 's/^/MCP duplicate /'
ev "posts_after_write_and_duplicate=$(posts)"

# Kill the VM while a write is in flight (the fixture commits, then waits).
call token-agent synthetic-notes__create_note '{"title":"Slow synthetic note","idempotency_key":"nanos-w2"}' >slow.out 2>&1 &
for _ in $(seq 1 100); do [ "$(posts)" -ge 2 ] && break; sleep 0.1; done
ev "posts_when_killed=$(posts)"
stop_vm
ev "vm_killed_during_write=true"

boot boot2.log
call token-agent daraja__receipt '{"tool":"synthetic-notes__create_note","idempotency_key":"nanos-w1"}' | sed 's/^/MCP receipt-w1-after-restart /'
grep -o '"recovered_uncertain":[0-9]*' boot2.log | sed 's/^/EVIDENCE boot2_/'
call token-agent daraja__receipt '{"tool":"synthetic-notes__create_note","idempotency_key":"nanos-w2"}' | sed 's/^/MCP receipt-after-restart /'
call token-agent synthetic-notes__create_note '{"title":"Slow synthetic note","idempotency_key":"nanos-w2"}' | sed 's/^/MCP retry-after-restart /'
call token-agent daraja__reconcile '{"tool":"synthetic-notes__create_note","idempotency_key":"nanos-w2"}' | sed 's/^/MCP reconcile /'
ev "posts_final=$(posts)"
sed 's/^/RUNTIME-LOG /' boot2.log | grep -v '^RUNTIME-LOG *$' | tail -20
stop_vm

# A13: the runtime on Nanos brokers a browser-provider operation to an
# execution capability on the owner's Mac, which dials in through the pod's
# published port. The Mac side drives the calls; this side waits for its
# done marker (MAC_DONE_URL) and records what the runtime saw.
if [ -n "${MAC_DONE_URL:-}" ]; then
  boot boot3.log
  ev "a13_ready=true"
  for _ in $(seq 1 ${A13_WAIT_S:-1200}); do
    curl -fsS -m 3 "$MAC_DONE_URL" >/dev/null 2>&1 && break
    sleep 1
  done
  ev "a13_mac_done=$(curl -fsS -m 3 "$MAC_DONE_URL" 2>/dev/null | tr -d '\n' || echo missing)"
  sed 's/^/RUNTIME-LOG3 /' boot3.log | grep -E 'capability|write|read|error' | tail -30
  stop_vm
fi
kill $FIX 2>/dev/null
ev "done"
