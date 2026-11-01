#!/bin/sh
# The box in a container (docs/docker-box.md).
#
#   box run                  start everything (the container's command)
#   box join INVITE [NAME] [GB]
#                            join the group with an invite: its Yggdrasil
#                            peers, then the node; NAME defaults to the
#                            container's hostname, GB to 20
#   box status               the status screen, printed once
#
# With INVITE (and BOX_NAME, BOX_GB) set, as in the compose file
# scripts/docker-invite.sh writes, run joins by itself the first time.
#
# Everything is kept in /data: Yggdrasil's key, the node's files, people's
# folders and Syncthing's settings, so the box survives a new image.
set -e
data=/data
home=$data/.yggstore
conf=$data/yggdrasil.conf

ygg_up() { ip -6 addr | grep -qE 'inet6 2[0-9a-f]{2}:'; }

start_yggdrasil() {
	if [ ! -s "$conf" ]; then
		yggdrasil -genconf -json | jq '.IfName = "auto" | .AdminListen = "unix:///data/yggdrasil.sock"' > "$conf"
	fi
	yggdrasil -useconffile "$conf" -logto "$data/yggdrasil.log" &
	for _ in $(seq 30); do ygg_up && return 0; sleep 1; done
	echo "Yggdrasil didn't start: see $data/yggdrasil.log" >&2
	return 1
}

case "${1:-run}" in
run)
	mkdir -p "$data/files"
	start_yggdrasil
	echo "This box's Yggdrasil address: $(ip -6 addr | grep -oE '2[0-9a-f]{2}:[0-9a-f:]+' | head -1)"
	if [ ! -f "$home/peers.json" ] && [ -n "${INVITE:-}" ]; then
		"$0" join "$INVITE" "${BOX_NAME:-$(hostname)}" "${BOX_GB:-20}" ||
			echo "Joining didn't work (above). Ask for a new invite, or join by hand with box join."
	fi
	if [ ! -f "$home/peers.json" ]; then
		echo "Not in a group yet. Join with the invite you were sent:"
		echo "  docker compose exec box box join 'yggjoin1:...'"
		while [ ! -f "$home/peers.json" ]; do sleep 3; done
	fi
	. "$data/box.env"
	syncthing serve --no-browser --no-restart >"$data/syncthing.log" 2>&1 &
	yggstore serve -peers "$home/peers.json" -data "$home/shards" -name "$NAME" -quota "$GB" \
		-sharing-key "$home/sharing.key" -contacts "$home/contacts.json" -invites "$home/invites.json" &
	sleep 3
	yggstore hub &
	yggstore dashboard -peers "$home/peers.json" -outfiles "$data/files" -sharing-key "$home/sharing.key" \
		-contacts "$home/contacts.json" -me "$NAME" -remote -imap= -smtp= &
	echo "$NAME is running. Its screen: docker compose exec -it box yggstore screen"
	# If any part stops, so does the container, and Docker starts it again.
	wait -n 2>/dev/null || wait
	exit 1
	;;
join)
	invite=${2:?usage: box join INVITE [NAME] [GB]}
	name=${3:-$(hostname)}
	gb=${4:-20}
	[ -f "$home/peers.json" ] && { echo "This box is in a group already." >&2; exit 1; }
	# The invite is yggjoin1: and base64url JSON; its ygg_peers link this
	# box's Yggdrasil to the group, as on a Raspberry Pi.
	case "$invite" in yggjoin1:*) ;; *) echo "That isn't an invite: they start with yggjoin1:" >&2; exit 1 ;; esac
	json=$(printf '%s' "${invite#yggjoin1:}" | tr -- '-_' '+/' | awk '{ n = length($0) % 4; if (n) $0 = $0 substr("===", 1, 4 - n); print }' | base64 -d 2>/dev/null)
	peers=$(printf '%s' "$json" | jq -c '.ygg_peers // []' 2>/dev/null) ||
		{ echo "That invite is damaged: copy it again, whole." >&2; exit 1; }
	[ -n "$peers" ] || { echo "That invite is damaged: copy it again, whole." >&2; exit 1; }
	[ "$peers" != "[]" ] || { echo "The invite names no Yggdrasil peers." >&2; exit 1; }
	jq --argjson p "$peers" '.Peers = $p' "$conf" > "$conf.new" && mv "$conf.new" "$conf"
	for p in $(printf '%s' "$peers" | jq -r '.[]'); do
		yggdrasilctl -endpoint "unix://$data/yggdrasil.sock" addPeer uri="$p" >/dev/null 2>&1 || true
	done
	echo "Linking to the group through $(printf '%s' "$peers" | jq length) peer(s)..."
	sleep 10
	printf 'NAME=%s\nGB=%s\n' "$name" "$gb" > "$data/box.env"
	yggstore join -dir "$home" -outfiles "$data/files" -name "$name" -me "$name" -quota "$gb" "$invite" >/tmp/join.log 2>&1 ||
		{ rm -f "$data/box.env"; cat /tmp/join.log >&2; exit 1; }
	grep -E "^You're in|^The group has" /tmp/join.log
	echo "The box starts its node now. Then set it up on its screen:"
	echo "  docker compose exec -it box yggstore screen"
	;;
status)
	exec yggstore screen -once
	;;
*)
	exec "$@"
	;;
esac
