#!/usr/bin/env bash
# Connects a "computer" to a hub with only the installer from the box's page
# (join.exe; here a Linux build of the same program), as someone would:
# download, run, nothing typed. Then it stores a file through the folder it
# got. Runs a hub's Syncthing and three group nodes on loopback. Needs
# syncthing and curl. The Windows-only parts (installing Syncthing, starting
# it at sign-in, no console windows) aren't covered.
# Usage: scripts/hub-join.sh [CLIENT_SYNCTHING]   (default: the one on PATH)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d)
BIN="$W/yggstore"
(cd "$ROOT" && go build -o "$BIN" ./cmd/yggstore && go build -o "$W/yggstore-join" ./cmd/yggstore-join)
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
	-syncthing-config "$W/st-pi/config.xml" -syncthing-url http://127.0.0.1:18384 -join-exe "$W/yggstore-join" >hub.log 2>&1 &
wait_for 20 curl -fsS -o /dev/null http://127.0.0.1:17490/connect.sh || fail "the hub isn't up"
curl -fsS http://127.0.0.1:17490/api/state | grep -q '"join_exe":true' || fail "the page doesn't offer the installer"

# The computer: download the installer and run it.
echo "--- the installer ($("$CLIENT_ST" --version | cut -d' ' -f1-2))"
curl -fsS -D headers -o "$W/client/Connect.exe" http://127.0.0.1:17490/join.exe || fail "no installer at /join.exe"
grep -qi 'filename="Connect to .*\.exe"' headers || fail "the download isn't named for the box: $(cat headers)"
chmod +x "$W/client/Connect.exe"
# The box's Syncthing is on loopback here, not at its LAN addresses.
HOME="$W/client" YGGSTORE_BOX_ADDRS=tcp://127.0.0.1:22100 "$W/client/Connect.exe" -no-service -syncthing "$CLIENT_ST" | tee join.out || fail "the installer failed"
echo "---"
F=$(sed -n 's/^Your folder is \(.*\)\. Put files.*/\1/p' join.out)
[ -n "$F" ] || fail "the installer named no folder"
wait_for 60 test -d "$F/files" || fail "the computer's folder has no files/"
echo "OK: connected with the installer; $F has the box's layout"

head -c 300000 /dev/urandom > "$F/files/report.bin"
wait_for 90 test -f "$F/files/report.bin.ystub" || fail "no stub on the computer"
echo "OK: a file put in files/ is stored, and its stub is back on the computer"

# Run again: nothing is added twice.
out=$(HOME="$W/client" "$W/client/Connect.exe" -no-service -syncthing "$CLIENT_ST") || fail "a second run failed: $out"
echo "$out" | grep -q "Added" && fail "a second run added something again: $out"
echo "$out" | grep -q "Your folder is $F" || fail "a second run doesn't name the folder: $out"
echo "OK: running it again changes nothing"
