#!/usr/bin/env bash
# Runs six nodes on this machine's Yggdrasil address, stores a random file,
# stops two nodes, restores the file and checks it matches.
# Usage: scripts/local-demo.sh [ygg|loopback]
set -euo pipefail
TRANSPORT=${1:-ygg}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/bin/yggstore"
(cd "$ROOT" && go build -o bin/yggstore ./cmd/yggstore)

WORK=$(mktemp -d)
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$WORK"' EXIT
cd "$WORK"

IP=$("$BIN" id -transport "$TRANSPORT" | awk '/node ID/{print $3}')
PORTS=(7401 7402 7403 7404 7405 7406)
{
  echo "["
  for i in "${!PORTS[@]}"; do
    sep=","; [ "$i" -eq $((${#PORTS[@]} - 1)) ] && sep=""
    echo "  {\"name\": \"node$i\", \"addr\": \"[$IP]:${PORTS[$i]}\"}$sep"
  done
  echo "]"
} > peers.json

PIDS=()
for p in "${PORTS[@]}"; do
  "$BIN" serve -transport "$TRANSPORT" -peers peers.json -port "$p" -data "node$p" -name "n$p" 2>"node$p.log" &
  PIDS+=($!)
done
sleep 1
"$BIN" status -peers peers.json

head -c 10000000 /dev/urandom > demo.bin
"$BIN" put -peers peers.json demo.bin
"$BIN" verify demo.bin.ystub | tail -1

echo "--- stopping two nodes"
kill "${PIDS[1]}" "${PIDS[4]}"
sleep 0.5
"$BIN" get -o restored.bin demo.bin.ystub 2>&1 | grep -v unavailable
cmp demo.bin restored.bin && echo "OK: restored file matches with 2 of 6 nodes down"
