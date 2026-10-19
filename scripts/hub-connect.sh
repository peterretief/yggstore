#!/usr/bin/env bash
# Connects a "computer" to a hub with only the box page's one-line command
# (connect.sh), as someone would: no Syncthing settings by hand. Then it
# stores a file through the folder it got. Runs a hub's Syncthing and three
# group nodes on loopback. Needs syncthing and curl.
# Usage: scripts/hub-connect.sh [CLIENT_SYNCTHING]   (default: the one on PATH)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d)
BIN="$W/yggstore"
(cd "$ROOT" && go build -o "$BIN" ./cmd/yggstore)
CLIENT_ST=${1:-$(command -v syncthing)}
cd "$W"
cleanup() {
	HOME="$W/client" "$CLIENT_ST" cli operations shutdown >/dev/null 2>&1 || true
	kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null || true; rm -rf "$W"
}
trap cleanup EXIT
export HOME="$W/home"; mkdir -p "$HOME" "$W/client"
fail() { echo "FAIL: $*"; echo "--- hub log"; cat hub.log; exit 1; }
wait_for() { # SECONDS COMMAND...: until the command succeeds
	local n=$1; shift
	for _ in $(seq "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
	return 1
}

echo '[{"name":"n1","addr":"[::1]:17401"},{"name":"n2","addr":"[::1]:17402"},{"name":"n3","addr":"[::1]:17403"}]' > peers.json
for i in 1 2 3; do
	extra=(-msg-api "")
	[ "$i" = 1 ] && extra=(-msg-api 127.0.0.1:17411 -msg-token "$W/msg.token")
	"$BIN" serve -transport loopback -peers peers.json -port 1740$i -data node$i -name n$i \
		-ygg-admin none -mailbox "" "${extra[@]}" 2>node$i.log &
done
wait_for 20 test -s "$W/msg.token" || fail "node didn't start"

# The box's Syncthing, on a fixed port.
STNODEFAULTFOLDER=1 syncthing generate --home="$W/st-pi" >/dev/null 2>&1
STNODEFAULTFOLDER=1 syncthing serve --home="$W/st-pi" --no-browser --gui-address=127.0.0.1:18384 --no-restart >st-pi.log 2>&1 &
PK=$(sed -n 's:.*<apikey>\(.*\)</apikey>.*:\1:p' "$W/st-pi/config.xml")
wait_for 20 curl -fsS -H "X-API-Key: $PK" http://127.0.0.1:18384/rest/system/ping || fail "Syncthing didn't start"
curl -fsS -H "X-API-Key: $PK" -X PATCH -H 'Content-Type: application/json' http://127.0.0.1:18384/rest/config/options \
	-d '{"listenAddresses":["tcp://127.0.0.1:22100"],"globalAnnounceEnabled":false,"localAnnounceEnabled":false,"relaysEnabled":false}'

"$BIN" hub -listen 127.0.0.1:17490 -peers peers.json -dir "$W/hub" -msg-api 127.0.0.1:17411 -msg-token "$W/msg.token" \
	-syncthing-config "$W/st-pi/config.xml" -syncthing-url http://127.0.0.1:18384 >hub.log 2>&1 &
wait_for 20 curl -fsS -o /dev/null http://127.0.0.1:17490/connect.sh || fail "the hub serves no connect.sh"

# The computer: only the command from the page.
echo "--- connect.sh ($("$CLIENT_ST" --version | cut -d' ' -f1-2))"
HOME="$W/client" SYNCTHING="$CLIENT_ST" YGGSTORE_NO_SERVICE=1 YGGSTORE_BOX_ADDRS=tcp://127.0.0.1:22100 \
	sh -c 'curl -fsSL http://127.0.0.1:17490/connect.sh | sh' || fail "connect.sh failed"
echo "---"
F="$W/client/yggstore"
wait_for 60 test -d "$F/files" || fail "the computer's ~/yggstore has no files/"
echo "OK: connected with one command; ~/yggstore has the box's layout"

head -c 300000 /dev/urandom > "$F/files/report.bin"
wait_for 90 test -f "$F/files/report.bin.ystub" || fail "no stub on the computer"
echo "OK: a file put in files/ is stored, and its stub is back on the computer"

# Run again: nothing is added twice.
out=$(HOME="$W/client" SYNCTHING="$CLIENT_ST" YGGSTORE_NO_SERVICE=1 sh -c 'curl -fsSL http://127.0.0.1:17490/connect.sh | sh') || fail "a second run failed"
echo "$out" | grep -q "Added" && fail "a second run added something again: $out"
echo "$out" | grep -q "Your folder is $F" || fail "a second run doesn't name the folder: $out"
echo "OK: running it again changes nothing"
