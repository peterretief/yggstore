#!/usr/bin/env bash
# Runs a hub with two Syncthings on this machine, one as the hub's and one
# as a device's, and three group nodes on loopback (one of them a web node).
# The device pairs; stores, restores and deletes a file; edits the hub's
# website and sees it published, and a broken edit held back. Then the hub's
# switches are tried. Needs syncthing and curl. Usage: scripts/hub-syncthing.sh
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/bin/yggstore"
(cd "$ROOT" && go build -o bin/yggstore ./cmd/yggstore)
W=$(mktemp -d); cd "$W"
cleanup() { kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null || true; rm -rf "$W"; }
trap cleanup EXIT
export HOME="$W/home"; mkdir -p "$HOME" # nothing here touches the real ~/.yggstore
fail() { echo "FAIL: $*"; echo "--- hub log"; cat hub.log; exit 1; }
wait_for() { # SECONDS COMMAND...: until the command succeeds
	local n=$1; shift
	for _ in $(seq "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
	return 1
}

# A group of three nodes on loopback; n1 serves websites and takes messages.
echo '[{"name":"n1","addr":"[::1]:17401"},{"name":"n2","addr":"[::1]:17402"},{"name":"n3","addr":"[::1]:17403"}]' > peers.json
for i in 1 2 3; do
	extra=(-msg-api "")
	[ "$i" = 1 ] && extra=(-msg-api 127.0.0.1:17411 -msg-token "$W/msg.token" -web 127.0.0.1:18480)
	"$BIN" serve -transport loopback -peers peers.json -port 1740$i -data node$i -name n$i \
		-ygg-admin none -mailbox "" "${extra[@]}" 2>node$i.log &
done
wait_for 20 test -s "$W/msg.token" || fail "node didn't start"

# The website, set up from the hub's machine before any device pairs.
mkdir site-src && echo "<h1>v1</h1>" > site-src/index.html
"$BIN" site publish site-src -name shop.example.org -peers peers.json -api 127.0.0.1:17411 -token "$W/msg.token" -no-route -no-wait >/dev/null
served() { curl -fsS -H "Host: shop.example.org" http://127.0.0.1:18480/ | grep -q "$1"; }
wait_for 30 served v1 || fail "the web node doesn't serve v1"
echo "OK: the website is served (v1)"

# Two Syncthings: the hub's and a device's.
for who in pi client; do
	STNODEFAULTFOLDER=1 syncthing generate --home="$W/st-$who" >/dev/null 2>&1
done
key() { sed -n 's:.*<apikey>\(.*\)</apikey>.*:\1:p' "$W/st-$1/config.xml"; }
STNODEFAULTFOLDER=1 syncthing serve --home="$W/st-pi" --no-browser --gui-address=127.0.0.1:18384 --no-restart >st-pi.log 2>&1 &
STNODEFAULTFOLDER=1 syncthing serve --home="$W/st-client" --no-browser --gui-address=127.0.0.1:18385 --no-restart >st-client.log 2>&1 &
PK=$(key pi); CK=$(key client)
api() { local port=$1 k=$2; shift 2; curl -fsS -H "X-API-Key: $k" "$@" -X "${M:-GET}" "http://127.0.0.1:$port$P"; }
ping_both() { P=/rest/system/ping api 18384 "$PK" && P=/rest/system/ping api 18385 "$CK"; }
wait_for 20 ping_both || fail "Syncthing didn't start"
# Fixed listen ports, no announcements, so the two find each other by address only.
for pair in "18384 $PK 22100" "18385 $CK 22101"; do
	set -- $pair
	M=PATCH P=/rest/config/options api $1 $2 -H 'Content-Type: application/json' \
		-d "{\"listenAddresses\":[\"tcp://127.0.0.1:$3\"],\"globalAnnounceEnabled\":false,\"localAnnounceEnabled\":false,\"relaysEnabled\":false}"
done
myid() { P=/rest/system/status api "$1" "$2" | python3 -c 'import sys,json;print(json.load(sys.stdin)["myID"])'; }
PIID=$(myid 18384 "$PK"); CID=$(myid 18385 "$CK")

# The hub, keeping the website.
"$BIN" hub -dir "$W/hub" site add shop.example.org >/dev/null
"$BIN" hub -listen 127.0.0.1:17490 -peers peers.json -dir "$W/hub" -msg-api 127.0.0.1:17411 -msg-token "$W/msg.token" \
	-syncthing-config "$W/st-pi/config.xml" -syncthing-url http://127.0.0.1:18384 >hub.log 2>&1 &
sleep 2

# The device adds the hub (as a person would, by its ID), then accepts the
# folders it is offered: its files, and the website.
M=PUT P=/rest/config/devices/$PIID api 18385 "$CK" -H 'Content-Type: application/json' \
	-d "{\"deviceID\":\"$PIID\",\"name\":\"pi\",\"addresses\":[\"tcp://127.0.0.1:22100\"]}"
offered() { P=/rest/cluster/pending/folders api 18385 "$CK" | python3 -c 'import sys,json;print(" ".join(sorted(json.load(sys.stdin))))'; }
two_offered() { [ "$(offered | wc -w)" = 2 ]; }
wait_for 60 two_offered || fail "the device wasn't offered its folder and the website: $(offered)"
for fid in $(offered); do
	case $fid in yggstore-site-*) path="$W/client-site" ;; *) path="$W/client-files" ;; esac
	mkdir -p "$path"
	M=PUT P=/rest/config/folders/$fid api 18385 "$CK" -H 'Content-Type: application/json' \
		-d "{\"id\":\"$fid\",\"path\":\"$path\",\"devices\":[{\"deviceID\":\"$PIID\"},{\"deviceID\":\"$CID\"}],\"fsWatcherDelayS\":1,\"rescanIntervalS\":5}"
done
echo "OK: paired; the device has its files folder and the website"

# Files: stored, stub back on the device, restored.
sleep 3
head -c 3000000 /dev/urandom > "$W/client-files/photo.bin"; cp "$W/client-files/photo.bin" "$W/original.bin"
stubbed() { [ -f "$W/client-files/photo.bin.ystub" ] && [ ! -f "$W/client-files/photo.bin" ]; }
wait_for 90 stubbed || fail "no stub on the device"
cp "$W/client-files/photo.bin.ystub" "$W/client-files/restore/"
wait_for 90 test -f "$W/client-files/restored/photo.bin" || fail "not restored"
cmp "$W/original.bin" "$W/client-files/restored/photo.bin" || fail "restored file differs"
echo "OK: stored, stub on the device, restored and matches"
rm "$W/client-files/photo.bin.ystub"
kept() { find "$W/hub/users" -path '*.stversions*' -name '*.ystub*' | grep -q .; }
wait_for 20 kept || fail "deleted stub not kept on the hub"
echo "OK: a stub deleted on the device is kept on the hub"

# Website: the device starts from the live site, edits it, and it is published.
wait_for 30 grep -q v1 "$W/client-site/index.html" || fail "the device's website folder wasn't filled with v1"
echo "OK: the device's website folder holds the live site"
echo "<h1>v2</h1>" > "$W/client-site/index.html"
wait_for 90 served v2 || fail "the edit wasn't published"
echo "OK: an edit on the device is published (v2)"
rm "$W/client-site/index.html"
sleep 30
served v2 || fail "the site broke when index.html was deleted"
"$BIN" hub -dir "$W/hub" sites | grep -q "no index.html" || fail "the held-back change isn't reported"
echo "OK: deleting index.html is held back; v2 stays up"

# Switches.
"$BIN" hub users
"$BIN" hub off
curl -s 127.0.0.1:17490/api/state | python3 -c 'import sys,json;print("page sync:",json.load(sys.stdin)["sync"])'
fid=$(P=/rest/config/folders api 18384 "$PK" | python3 -c 'import sys,json;print([f["id"] for f in json.load(sys.stdin) if not f["id"].startswith("yggstore-site-")][0])')
"$BIN" hub pause; sleep 7
P=/rest/config/folders/$fid api 18384 "$PK" | python3 -c 'import sys,json;print("hub folder paused:",json.load(sys.stdin)["paused"])'
"$BIN" hub resume; sleep 7
P=/rest/config/folders/$fid api 18384 "$PK" | python3 -c 'import sys,json;print("hub folder paused:",json.load(sys.stdin)["paused"])'
name=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["users"][0]["name"])' "$HOME/.yggstore/hub.json")
"$BIN" hub remove "$name"; sleep 7
P=/rest/config/devices api 18384 "$PK" | python3 -c 'import sys,json;print("hub devices after remove:",len(json.load(sys.stdin)),"(itself only)")'
"$BIN" hub users
echo "--- hub log"; cat hub.log
