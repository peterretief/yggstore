#!/usr/bin/env bash
# Make a ready-made Docker box for someone: an invite from the admin
# dashboard, in a compose file they run as it is (docs/docker-box.md).
#
#   scripts/docker-invite.sh NAME [GB]
#
# NAME is the box's name in the group; GB what it gives (default 20). The
# file goes in ~/yggstore-invites/NAME.compose.yml, outside git: it holds a
# working invite, so send it only to them. It works once, for 7 days.
set -euo pipefail
name=${1:?usage: scripts/docker-invite.sh NAME [GB]}
gb=${2:-20}
[[ "$name" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,38}[A-Za-z0-9])?$ ]] || { echo "NAME must be letters, digits and dashes" >&2; exit 1; }
dash=${ADMIN_DASHBOARD:-http://127.0.0.1:7480}
invite=$(curl -fsS -m 20 -X POST -H 'X-Yggstore: 1' -H 'Content-Type: application/json' \
	-d "{\"for\": \"docker box $name\"}" "$dash/api/invites" |
	python3 -c 'import sys, json; print(json.load(sys.stdin)["invite"])') ||
	{ echo "The admin dashboard at $dash didn't make an invite. Is it running?" >&2; exit 1; }
out=~/yggstore-invites/$name.compose.yml
mkdir -p ~/yggstore-invites
umask 077
cat > "$out" <<YAML
# A yggstore box for $name, which joins the group by itself.
#   docker compose -f $name.compose.yml up -d
#   docker compose -f $name.compose.yml exec -it box yggstore screen   (then Enter, to set it up)
# The invite below works once, until $(date -d '+7 days' '+%-d %B %Y'). Keep this file to yourself.
name: yggstore-box

services:
  box:
    build:
      context: https://github.com/peterretief/yggstore.git#main
      dockerfile: docker/box/Dockerfile
    image: yggstore-box
    hostname: $name
    restart: unless-stopped
    cap_add: [NET_ADMIN]
    devices: ["/dev/net/tun"]
    sysctls:
      net.ipv6.conf.all.disable_ipv6: 0
    environment:
      BOX_NAME: $name
      BOX_GB: "$gb"
      INVITE: "$invite"
    volumes:
      - box-data:/data

volumes:
  box-data:
YAML
echo "Made $out"
echo "Send it to them. They run:"
echo "  docker compose -f $name.compose.yml up -d"
echo "  docker compose -f $name.compose.yml exec -it box yggstore screen"
