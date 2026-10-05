#!/usr/bin/env bash
# Three Docker test nodes (l1..l3) on a remote host, added to the test
# cluster's peer list (testnet/shared/peers.json, see scripts/testnet.sh).
#
#   scripts/testnet-remote.sh up   [HOST]  deploy and start, update the peer list, print node keys
#   scripts/testnet-remote.sh down [HOST]  stop the remote test nodes (keys and shards are kept)
#
# HOST (user@host, or $YGGTEST_HOST) is the machine to run them on. Its Yggdrasil must
# beacon on br-yggtest and list the printed keys in AllowedPublicKeys, and its
# firewall must let br-yggtest in.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
HOST=${2:-${YGGTEST_HOST:?give the remote host, e.g. scripts/testnet-remote.sh up user@host}}
NODES=(l1 l2 l3)
PEERS=testnet/shared/peers.json
RDIR=yggtest

up() {
  [ -f "$PEERS" ] || { echo "run scripts/testnet.sh up first" >&2; exit 1; }
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o bin/yggstore-linux-amd64 ./cmd/yggstore
  ssh "$HOST" "mkdir -p $RDIR/bin $RDIR/docker $RDIR/testnet/shared"
  scp -q bin/yggstore-linux-amd64 bin/yggdrasil-linux-amd64 bin/yggdrasilctl-linux-amd64 "$HOST:$RDIR/bin/"
  scp -q docker/Dockerfile docker/entrypoint.sh "$HOST:$RDIR/docker/"
  scp -q docker/compose.remote.yml "$HOST:$RDIR/docker/compose.yml"
  ssh "$HOST" "cd $RDIR && docker build -q -t yggstore-testnode -f docker/Dockerfile . >/dev/null &&
    docker compose -f docker/compose.yml up -d --force-recreate"

  echo "waiting for the remote test nodes' Yggdrasil addresses..."
  entries=()
  for n in "${NODES[@]}"; do
    id=""
    for _ in $(seq 1 60); do
      id=$(ssh "$HOST" "cat $RDIR/testnet/$n/id.txt 2>/dev/null" || true)
      [ -n "$id" ] && break
      sleep 2
    done
    [ -n "$id" ] || { echo "$n did not start; see: ssh $HOST docker logs yggtest-$n-1" >&2; exit 1; }
    ip=$(awk '/node ID/{print $3}' <<<"$id")
    key=$(ssh "$HOST" "docker exec yggtest-$n-1 yggdrasilctl -json getSelf" | grep -oE '"key": *"[0-9a-f]{64}"' | grep -oE '[0-9a-f]{64}')
    entries+=("$n $ip")
    echo "$n  $ip  key $key"
  done

  # Replace any earlier l* entries, keep the rest of the test peer list.
  python3 - "$PEERS" "${entries[@]}" <<'EOF'
import json, sys
path, entries = sys.argv[1], sys.argv[2:]
names = {e.split()[0] for e in entries}
peers = [p for p in json.load(open(path)) if p["name"] not in names]
peers += [{"name": e.split()[0], "addr": f"[{e.split()[1]}]:7400"} for e in entries]
with open(path + ".tmp", "w") as f:
    json.dump(peers, f, indent=2)
    f.write("\n")
EOF
  mv "$PEERS.tmp" "$PEERS"
  scp -q "$PEERS" "$HOST:$RDIR/testnet/shared/peers.json"
  echo "updated $PEERS and copied it to $HOST"
}

case "${1:-}" in
  up) up ;;
  down) ssh "$HOST" "cd $RDIR && docker compose -f docker/compose.yml down" ;;
  *) sed -n '2,11p' "$0"; exit 1 ;;
esac
