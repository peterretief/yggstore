#!/usr/bin/env bash
# Five Docker test nodes (t1..t5) joined to this machine's Yggdrasil.
# The test peer list is testnet/shared/peers.json: this desktop + t1..t5, so
# test files never land on the real nodes.
#
#   scripts/testnet.sh up      build and start, write the peer list, show status
#   scripts/testnet.sh status  yggstore status for the test cluster
#   scripts/testnet.sh down    stop the containers (keys and shards are kept)
#   scripts/testnet.sh clean   stop and delete all test data and keys
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
COMPOSE=(docker compose -f docker/compose.yml)
NODES=(t1 t2 t3 t4 t5)
PEERS=testnet/shared/peers.json

up() {
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o bin/yggstore-linux-amd64 ./cmd/yggstore
  go build -o bin/yggstore ./cmd/yggstore
  if [ ! -x bin/yggdrasil-linux-amd64 ]; then
    for c in yggdrasil yggdrasilctl; do
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOBIN="$ROOT/bin/.ygg" go install -ldflags "-s -w" \
        github.com/yggdrasil-network/yggdrasil-go/cmd/$c@v0.5.12
      mv "bin/.ygg/$c" "bin/$c-linux-amd64"
    done
    rmdir bin/.ygg
  fi
  mkdir -p testnet/shared "${NODES[@]/#/testnet/}"
  docker build -q -t yggstore-testnode -f docker/Dockerfile . >/dev/null
  "${COMPOSE[@]}" up -d --force-recreate

  echo "waiting for the test nodes' Yggdrasil addresses..."
  for n in "${NODES[@]}"; do
    for _ in $(seq 1 60); do [ -s "testnet/$n/id.txt" ] && break; sleep 1; done
    [ -s "testnet/$n/id.txt" ] || { echo "$n did not start; see: docker logs yggtest-$n-1" >&2; exit 1; }
  done

  # The desktop's own entry comes from the real peer list.
  desktop=$(grep -o '{[^}]*"desktop"[^}]*}' peers.json)
  {
    echo "["
    echo "  $desktop,"
    last=${NODES[${#NODES[@]}-1]}
    for n in "${NODES[@]}"; do
      ip=$(awk '/node ID/{print $3}' "testnet/$n/id.txt")
      sep=","; [ "$n" = "$last" ] && sep=""
      echo "  {\"name\": \"$n\", \"addr\": \"[$ip]:7400\"}$sep"
    done
    echo "]"
  } > "$PEERS.tmp" && mv "$PEERS.tmp" "$PEERS"
  echo "wrote $PEERS"
  sleep 5
  status
}

status() { bin/yggstore status -peers "$PEERS"; }

case "${1:-}" in
  up) up ;;
  status) status ;;
  down) "${COMPOSE[@]}" down ;;
  clean)
    "${COMPOSE[@]}" down
    # Files in testnet/ were written by root inside the containers.
    docker run --rm -v "$ROOT/testnet:/t" alpine:3.22 sh -c 'rm -rf /t/*'
    rmdir testnet ;;
  *) sed -n '2,10p' "$0"; exit 1 ;;
esac
