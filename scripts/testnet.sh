#!/usr/bin/env bash
# Five Docker test nodes (t1..t5) and a test S3 gateway (gw, port 9000),
# joined to this machine's Yggdrasil. The test peer list is
# testnet/shared/peers.json: this desktop + t1..t5 + gw, so test files never
# land on the real nodes. t1..t5 take customer data; see docs/gateway.md.
#
#   scripts/testnet.sh up      build and start, write the peer list, show status
#   scripts/testnet.sh status  yggstore status for the test cluster
#   scripts/testnet.sh customer add -name NAME -quota GB   (and the other gateway customer commands)
#   scripts/testnet.sh report  the test gateway's usage report
#   scripts/testnet.sh down    stop the containers (keys and shards are kept)
#   scripts/testnet.sh clean   stop and delete all test data and keys
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
COMPOSE=(docker compose -f docker/compose.yml)
NODES=(t1 t2 t3 t4 t5 gw)
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
  # Keeps the go tool out of the containers' (root-owned, private) data.
  [ -f testnet/go.mod ] || echo "module testnet" > testnet/go.mod
  docker build -q -t yggstore-testnode -f docker/Dockerfile . >/dev/null
  "${COMPOSE[@]}" up -d --force-recreate

  echo "waiting for the test nodes' Yggdrasil addresses..."
  for n in "${NODES[@]}"; do
    for _ in $(seq 1 60); do [ -s "testnet/$n/id.txt" ] && break; sleep 1; done
    [ -s "testnet/$n/id.txt" ] || { echo "$n did not start; see: docker logs yggtest-$n-1" >&2; exit 1; }
  done

  # The desktop's own entry comes from the real peer list.
  desktop=$(grep -o '{[^}]*"name": *"desktop"[^}]*}' peers.json)
  {
    echo "["
    echo "  $desktop,"
    last=${NODES[${#NODES[@]}-1]}
    for n in "${NODES[@]}"; do
      ip=$(awk '/node ID/{print $3}' "testnet/$n/id.txt")
      sep=","; [ "$n" = "$last" ] && sep=""
      extra=""; [ "$n" = gw ] && extra=', "gateway": true'
      echo "  {\"name\": \"$n\", \"addr\": \"[$ip]:7400\"$extra}$sep"
    done
    echo "]"
  } > "$PEERS.tmp" && mv "$PEERS.tmp" "$PEERS"
  echo "wrote $PEERS"
  sleep 5
  status
}

status() { bin/yggstore status -peers "$PEERS"; }

# The address a PC on the LAN reaches the test gateway at.
endpoint() { echo "http://$(hostname -I | awk '{print $1}'):9000"; }

customer() {
  local sub=${1:-list}; shift || true
  docker exec yggtest-gw-1 yggstore gateway customer "$sub" -dir /data/gateway -endpoint "$(endpoint)" "$@"
}

case "${1:-}" in
  up) up ;;
  status) status ;;
  customer) shift; customer "$@" ;;
  report) shift; docker exec yggtest-gw-1 yggstore gateway report -dir /data/gateway "$@" ;;
  down) "${COMPOSE[@]}" down ;;
  clean)
    "${COMPOSE[@]}" down
    # Files in testnet/ were written by root inside the containers.
    docker run --rm -v "$ROOT/testnet:/t" alpine:3.22 sh -c 'rm -rf /t/*'
    rmdir testnet ;;
  *) sed -n '2,10p' "$0"; exit 1 ;;
esac
