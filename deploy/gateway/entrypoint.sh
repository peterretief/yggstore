#!/bin/sh
# Starts Yggdrasil, joins the group on first start (from /data/invite), then
# runs a small node and the S3 gateway. Everything is kept in /data:
#   yggdrasil.conf   this machine's Yggdrasil key: its address in the group
#   invite           the invite, until it has been used
#   node/            peers.json, sharing key, shards, and node/gateway/,
#                    the gateway's accounts and object keys (back it up)
set -e
mkdir -p /data/node

# Yggdrasil peers: YGG_PEERS if set, otherwise the ones in the invite.
peers_json() {
  if [ -n "${YGG_PEERS:-}" ]; then
    echo "$YGG_PEERS" | tr ', ' '\n\n' | jq -R . | jq -sc 'map(select(. != ""))'
  elif [ -s /data/invite ]; then
    decode_invite | jq -c '.ygg_peers // []'
  elif [ -s /data/invite.used ]; then
    jq -c '.ygg_peers // []' /data/invite.used
  else
    echo '[]'
  fi
}

# An invite is "yggjoin1:" and base64url JSON.
decode_invite() {
  s=$(sed 's/^yggjoin1://' /data/invite | tr -d ' \n' | tr '_-' '/+')
  while [ $(( ${#s} % 4 )) -ne 0 ]; do s="$s="; done
  echo "$s" | base64 -d
}

if [ ! -s /data/yggdrasil.conf ]; then
  # Fresh key on first start; kept in /data so the address survives rebuilds.
  yggdrasil -genconf -json > /data/yggdrasil.conf
fi
# No multicast (a VPS has no neighbours to find) and no listening port: the
# gateway only dials out.
p=$(peers_json)
jq --argjson p "$p" '.Peers = $p | .MulticastInterfaces = [] | .Listen = []' \
  /data/yggdrasil.conf > /data/yggdrasil.conf.new && mv /data/yggdrasil.conf.new /data/yggdrasil.conf
[ "$p" = "[]" ] && echo "warning: no Yggdrasil peers; set YGG_PEERS or give an invite"
yggdrasil -useconffile /data/yggdrasil.conf &

for _ in $(seq 1 30); do
  ip -6 addr show | grep -q 'inet6 20[01]:' && break
  sleep 1
done
yggstore id > /data/id.txt

if [ ! -s /data/node/peers.json ]; then
  if [ ! -s /data/invite ]; then
    echo "not in a group yet: put an invite in /data/invite (scripts/gateway-docker.sh join)"
    while [ ! -s /data/invite ]; do sleep 5; done
  fi
  # The group's nodes may take a moment to see this new address.
  for i in 1 2 3 4 5 6; do
    if yggstore join -dir /data/node -name "${NODE_NAME:-gateway}" -me "${OWNER:-organiser}" \
        -quota "${NODE_QUOTA:-1}" "$(cat /data/invite)"; then
      decode_invite > /data/invite.used && rm /data/invite
      break
    fi
    [ "$i" = 6 ] && { echo "could not join; check the invite and the Yggdrasil peers"; exit 1; }
    sleep 20
  done
fi

yggstore serve -peers /data/node/peers.json -data /data/node/shards \
  -name "${NODE_NAME:-gateway}" -quota "${NODE_QUOTA:-1}" \
  -sharing-key /data/node/sharing.key -contacts /data/node/contacts.json \
  -invites /data/node/invites.json &

# IPv4 only, so the gateway's plain HTTP port isn't open on Yggdrasil; Caddy
# reaches it over the compose network and adds HTTPS. It won't start until
# the admin marks this machine "gateway": true in the group's list, so keep
# trying while the node runs.
trap 'kill 0; exit 0' TERM INT
while :; do
  yggstore gateway serve -dir /data/node/gateway -peers /data/node/peers.json \
    -pushed /data/node/shards/peers.pushed.json -listen 0.0.0.0:9000 -trust-proxy \
    -min-machines "${MIN_MACHINES:-3}" &
  wait $! || true
  echo "the gateway stopped; trying again in 30 seconds"
  sleep 30 &
  wait $!
done
