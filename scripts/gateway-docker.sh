#!/usr/bin/env bash
# The S3 gateway in Docker, with HTTPS from Caddy: for a VPS or any machine
# with ports 80 and 443 open to the internet. Needs only Docker and a domain
# name pointing at the machine. Files are in deploy/gateway/.
#
#   scripts/gateway-docker.sh setup          ask for the domain and an invite, then start
#   scripts/gateway-docker.sh up             build and (re)start, e.g. after git pull
#   scripts/gateway-docker.sh status         the gateway's address, the group, HTTPS check
#   scripts/gateway-docker.sh customer add -name NAME -trial 14d   (and the other customer commands)
#   scripts/gateway-docker.sh report         this month's usage
#   scripts/gateway-docker.sh backup         copy the accounts and object keys to deploy/gateway/backups/
#   scripts/gateway-docker.sh logs           follow the logs
#   scripts/gateway-docker.sh down           stop (everything is kept)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
DIR=$ROOT/deploy/gateway
COMPOSE=(docker compose --project-directory "$DIR" -f "$DIR/compose.yml")

ask() { # ask VAR "question" [default]
  local reply
  read -r -p "$2${3:+ [$3]}: " reply
  printf -v "$1" '%s' "${reply:-${3:-}}"
}

setting() { grep -s "^$1=" "$DIR/.env" | tail -1 | cut -d= -f2- ; }

setup() {
  command -v docker >/dev/null || { echo "Install Docker first: https://docs.docker.com/engine/install/" >&2; exit 1; }
  local domain owner peers invite
  echo "The gateway needs a domain name whose DNS points at this machine,"
  echo "and ports 80 and 443 open to the internet (Caddy gets the certificate)."
  ask domain "Domain for customers, e.g. s3.yourgroup.example" "$(setting DOMAIN)"
  [ -n "$domain" ] || { echo "a domain is needed" >&2; exit 1; }
  ask owner "Your name, as the group sees it" "$(setting OWNER)"
  if [ ! -s "$DIR/data/node/peers.json" ]; then
    echo "Make an invite on the admin dashboard and paste it here."
    ask invite "Invite (yggjoin1:...)"
    case "$invite" in yggjoin1:*) ;; *) echo "that isn't an invite" >&2; exit 1 ;; esac
  fi
  echo "Yggdrasil peers come from the invite. To use others, list them here"
  ask peers "Extra Yggdrasil peers (comma separated, Enter for none)" "$(setting YGG_PEERS)"

  cat > "$DIR/.env" <<EOT
DOMAIN=$domain
OWNER=$owner
NODE_NAME=gateway
YGG_PEERS=$peers
MIN_MACHINES=3
EOT
  chmod 600 "$DIR/.env"
  if [ -n "${invite:-}" ]; then
    mkdir -p "$DIR/data"
    (umask 077; printf '%s\n' "$invite" > "$DIR/data/invite")
  fi
  up
}

up() {
  [ -f "$DIR/.env" ] || { echo "run: $0 setup" >&2; exit 1; }
  mkdir -p "$DIR/data" "$DIR/caddy"
  "${COMPOSE[@]}" up -d --build
  echo "waiting for the gateway to join the group..."
  for _ in $(seq 1 150); do
    in_group && break
    sleep 2
  done
  status
}

gw() { "${COMPOSE[@]}" exec -T gateway "$@"; }

in_group() { gw test -s /data/node/peers.json 2>/dev/null; }

address() { gw sh -c "awk '/node ID/{print \$3}' /data/id.txt" 2>/dev/null; }

status() {
  local domain addr
  domain=$(setting DOMAIN)
  addr=$(address || true)
  echo "Yggdrasil address: ${addr:-not up yet}"
  if ! in_group; then
    echo "Not in the group yet. See: $0 logs"
    return
  fi
  gw yggstore status -peers /data/node/peers.json || true
  if ! gw grep -q "\"gateway\": *true" /data/node/peers.json; then
    echo
    echo "On the admin machine, mark this entry in peers.json as the gateway:"
    echo "  {\"name\": \"$(setting NODE_NAME)\", \"addr\": \"[$addr]:7400\", ..., \"gateway\": true}"
    echo "The dashboard sends the changed list to every node within a minute."
  fi
  echo
  # Any HTTP answer will do: without keys the gateway says 403.
  local url="https://$domain${HTTPS_PORT:+:$HTTPS_PORT}/"
  if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$url")" != 000 ]; then
    echo "HTTPS works: customers use https://$domain"
  elif [ "$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 "$url")" != 000 ]; then
    echo "https://$domain answers, but its certificate isn't valid yet. Caddy keeps"
    echo "trying; check that the domain's DNS points here. See: $0 logs caddy"
  else
    echo "https://$domain does not answer. Check that its DNS points here and"
    echo "ports 80 and 443 are open. See: $0 logs caddy"
  fi
}

customer() {
  local sub=${1:-list}; shift || true
  gw yggstore gateway customer "$sub" -dir /data/node/gateway -endpoint "https://$(setting DOMAIN)" "$@"
}

backup() {
  local out
  mkdir -p "$DIR/backups"
  out="$DIR/backups/gateway-$(date +%Y%m%d-%H%M%S).tar.gz"
  (umask 077; gw tar -C /data/node -czf - gateway > "$out")
  echo "wrote $out"
  echo "It holds every customer's keys: copy it somewhere private, off this machine."
}

case "${1:-}" in
  setup) setup ;;
  up) up ;;
  status) status ;;
  customer) shift; customer "$@" ;;
  report) shift; gw yggstore gateway report -dir /data/node/gateway "$@" ;;
  backup) backup ;;
  logs) shift; "${COMPOSE[@]}" logs -f --tail 100 "$@" ;;
  down) "${COMPOSE[@]}" down ;;
  *) sed -n '2,15p' "$0"; exit 1 ;;
esac
