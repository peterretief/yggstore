#!/bin/sh
# Starts Yggdrasil, records this node's address in /data/id.txt, then serves
# shards once /etc/yggstore/peers.json exists (written by scripts/testnet.sh).
# CUSTOMERS=1 lets the node hold paying customers' data. ROLE=gateway also
# runs the S3 gateway on port 9000, keeping its files in /data/gateway.
set -e
mkdir -p /data/shards
if [ ! -s /data/yggdrasil.conf ]; then
  # Fresh key on first start; kept in /data so the address survives restarts.
  yggdrasil -genconf > /data/yggdrasil.conf
fi
yggdrasil -useconffile /data/yggdrasil.conf &

for _ in $(seq 1 30); do
  ip -6 addr show | grep -q 'inet6 20[01]:' && break
  sleep 1
done
yggstore id > /data/id.txt

while [ ! -f /etc/yggstore/peers.json ]; do
  echo "waiting for /etc/yggstore/peers.json"
  sleep 3
done
set -- serve -peers /etc/yggstore/peers.json -data /data/shards \
  -name "${NODE_NAME:-$(hostname)}" -quota 5
[ "${CUSTOMERS:-}" = 1 ] && set -- "$@" -customers
if [ "${ROLE:-}" = gateway ]; then
  yggstore "$@" &
  exec yggstore gateway serve -dir /data/gateway -peers /etc/yggstore/peers.json \
    -pushed /data/shards/peers.pushed.json -listen :9000 -min-machines "${MIN_MACHINES:-3}"
fi
exec yggstore "$@"
