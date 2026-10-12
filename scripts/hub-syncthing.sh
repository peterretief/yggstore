#!/usr/bin/env bash
# Runs a hub with two Syncthings on this machine, one as the hub's and one
# as a device's, and three group nodes on loopback. The device pairs, stores
# a file, restores it and deletes its stub; then the hub's switches are tried.
# Needs syncthing and curl. Usage: scripts/hub-syncthing.sh
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN="$ROOT/bin/yggstore"
(cd "$ROOT" && go build -o bin/yggstore ./cmd/yggstore)
W=$(mktemp -d); cd "$W"
cleanup() { kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null || true; rm -rf "$W"; }
trap cleanup EXIT

# A group of three nodes on loopback.
echo '[{"name":"n1","addr":"[::1]:17401"},{"name":"n2","addr":"[::1]:17402"},{"name":"n3","addr":"[::1]:17403"}]' > peers.json
for i in 1 2 3; do "$BIN" serve -transport loopback -peers peers.json -port 1740$i -data node$i -name n$i 2>node$i.log & done

# Two Syncthings: the Pi's and a client's.
for who in pi client; do
  STNODEFAULTFOLDER=1 syncthing generate --home="$W/st-$who" >/dev/null 2>&1
done
key() { sed -n 's:.*<apikey>\(.*\)</apikey>.*:\1:p' "$W/st-$1/config.xml"; }
STNODEFAULTFOLDER=1 syncthing serve --home="$W/st-pi" --no-browser --gui-address=127.0.0.1:18384 --no-restart >st-pi.log 2>&1 &
STNODEFAULTFOLDER=1 syncthing serve --home="$W/st-client" --no-browser --gui-address=127.0.0.1:18385 --no-restart >st-client.log 2>&1 &
PK=$(key pi); CK=$(key client)
api() { local port=$1 k=$2; shift 2; curl -fsS -H "X-API-Key: $k" "$@" -X "${M:-GET}" "http://127.0.0.1:$port$P"; }
for i in $(seq 50); do P=/rest/system/ping api 18384 "$PK" >/dev/null 2>&1 && P=/rest/system/ping api 18385 "$CK" >/dev/null 2>&1 && break; sleep 0.2; done
# Fixed listen ports, no announcements, so the two find each other by address only.
for pair in "18384 $PK 22100" "18385 $CK 22101"; do set -- $pair
  M=PATCH P=/rest/config/options api $1 $2 -d "{\"listenAddresses\":[\"tcp://127.0.0.1:$3\"],\"globalAnnounceEnabled\":false,\"localAnnounceEnabled\":false,\"relaysEnabled\":false}" -H 'Content-Type: application/json'
done
PIID=$(P=/rest/system/status api 18384 "$PK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["myID"])')
CID=$(P=/rest/system/status api 18385 "$CK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["myID"])')

# The hub.
"$BIN" hub -listen 127.0.0.1:17490 -peers peers.json -users "$W/users" -state "$W/hub.json" \
  -sharing-key "$W/sharing.key" -syncthing-config "$W/st-pi/config.xml" -syncthing-url http://127.0.0.1:18384 >hub.log 2>&1 &
sleep 2

# The client adds the Pi (as a person would, by its ID).
M=PUT P=/rest/config/devices/$PIID api 18385 "$CK" -H 'Content-Type: application/json' \
  -d "{\"deviceID\":\"$PIID\",\"name\":\"pi\",\"addresses\":[\"tcp://127.0.0.1:22100\"]}"
echo "client added the pi; waiting for the hub to pair it and share a folder"
for i in $(seq 60); do
  pf=$(P=/rest/cluster/pending/folders api 18385 "$CK")
  fid=$(echo "$pf" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(next(iter(d),""))')
  [ -n "$fid" ] && break; sleep 1
done
[ -n "$fid" ] || { echo "FAIL: no folder offered"; cat hub.log; exit 1; }
echo "offered folder $fid"
mkdir -p "$W/client-files"
M=PUT P=/rest/config/folders/$fid api 18385 "$CK" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$fid\",\"path\":\"$W/client-files\",\"devices\":[{\"deviceID\":\"$PIID\"},{\"deviceID\":\"$CID\"}],\"fsWatcherDelayS\":1,\"rescanIntervalS\":5}"

sleep 3
head -c 3000000 /dev/urandom > "$W/client-files/photo.bin"; cp "$W/client-files/photo.bin" "$W/original.bin"
echo "dropped photo.bin on the client; waiting for its stub to come back"
for i in $(seq 90); do
  [ -f "$W/client-files/photo.bin.ystub" ] && [ ! -f "$W/client-files/photo.bin" ] && break; sleep 1
done
[ -f "$W/client-files/photo.bin.ystub" ] || { echo "FAIL: no stub on the client"; tail -20 hub.log; exit 1; }

# Restore through the folder: the stub into restore/.
cp "$W/client-files/photo.bin.ystub" "$W/client-files/restore/"
for i in $(seq 90); do [ -f "$W/client-files/restored/photo.bin" ] && break; sleep 1; done
cmp "$W/original.bin" "$W/client-files/restored/photo.bin" || { echo "FAIL: restored file differs"; exit 1; }
echo "OK: stored, stub on the device, restored and matches"

# A stub deleted on the client is kept on the hub.
rm "$W/client-files/photo.bin.ystub"; sleep 8
find "$W/users" -path '*.stversions*' -name '*.ystub*' | grep -q . || { echo "FAIL: deleted stub not kept on the hub"; exit 1; }
echo "OK: a stub deleted on the device is kept on the hub"

# Switches.
"$BIN" hub -state "$W/hub.json" users
"$BIN" hub -state "$W/hub.json" off
curl -s 127.0.0.1:17490/api/state | python3 -c 'import sys,json;print("page sync:",json.load(sys.stdin)["sync"])'
"$BIN" hub -state "$W/hub.json" pause; sleep 7
P=/rest/config/folders/$fid api 18384 "$PK" | python3 -c 'import sys,json;print("pi folder paused:",json.load(sys.stdin)["paused"])'
"$BIN" hub -state "$W/hub.json" resume; sleep 7
P=/rest/config/folders/$fid api 18384 "$PK" | python3 -c 'import sys,json;print("pi folder paused:",json.load(sys.stdin)["paused"])'
name=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["users"][0]["name"])' "$W/hub.json")
"$BIN" hub -state "$W/hub.json" remove "$name"; sleep 7
P=/rest/config/folders api 18384 "$PK" | python3 -c 'import sys,json;print("pi folders after remove:",[f["id"] for f in json.load(sys.stdin)])'
P=/rest/config/devices api 18384 "$PK" | python3 -c 'import sys,json;print("pi devices after remove:",len(json.load(sys.stdin)),"(itself only)")'
"$BIN" hub -state "$W/hub.json" users
echo "--- hub log"; cat hub.log
