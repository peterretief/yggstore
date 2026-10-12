#!/usr/bin/env bash
# Takes a node down the planned way, on this machine: seven nodes on
# loopback, the admin's dashboard and a member's (with a node and member
# list of their own), each storing a file. The admin marks a node leaving;
# both dashboards move their files' shards off it while it is up; then it
# and two more nodes stop, and both files still restore.
# Usage: scripts/drain-test.sh
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d)
BIN="$W/yggstore" # its own build: bin/yggstore may be what this machine's services run
(cd "$ROOT" && go build -o "$BIN" ./cmd/yggstore)
cd "$W"
cleanup() { kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null || true; [ -n "${KEEP:-}" ] || rm -rf "$W"; }
trap cleanup EXIT
fail() { echo "FAIL: $*"; for f in admin.log member.log; do echo "--- $f"; tail -20 "$f"; done; exit 1; }
wait_for() { # SECONDS COMMAND...: until the command succeeds
	local n=$1; shift
	for _ in $(seq "$n"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
	return 1
}

# Seven nodes; n0 is the admin's. n1 is the member's, with its own copy of
# the list, which reaches it only by the admin's push.
addr() { echo "[::1]:1760$1"; }
{
	echo "["
	for i in 0 1 2 3 4 5 6; do
		admin=""; [ "$i" = 0 ] && admin=', "admin": true'
		sep=","; [ "$i" = 6 ] && sep=""
		echo "  {\"name\": \"n$i\", \"addr\": \"$(addr $i)\"$admin}$sep"
	done
	echo "]"
} > peers.json
mkdir -p member/shards && cp peers.json member/peers.json
for i in 0 1 2 3 4 5 6; do
	list=peers.json data=node$i
	[ "$i" = 1 ] && list=member/peers.json data=member/shards
	HOME="$W/home-n$i" "$BIN" serve -transport loopback -peers "$list" -port 1760$i -data "$data" -name n$i \
		-ygg-admin none -mailbox "" -msg-api "" 2>node$i.log &
	echo $! > node$i.pid
done
sleep 1

# On loopback every program is ::1, so the member's dashboard would take
# itself for the admin's; it is shown this machine's Yggdrasil address instead.
"$BIN" id -transport ygg >/dev/null 2>&1 || { echo "needs Yggdrasil running on this machine"; exit 2; }
dash() { # NAME PORT PEERS TRANSPORT: a dashboard with its own home and outbox
	mkdir -p "$1-out" "$W/home-$1"
	HOME="$W/home-$1" "$BIN" dashboard -transport "$4" -peers "$3" -outfiles "$W/$1-out" \
		-listen 127.0.0.1:$2 -interval 2s -msg-api "" -imap= -smtp= -site-port 0 -me "$1" >"$1.log" 2>&1 &
}
dash admin 17680 peers.json loopback
dash member 17681 member/peers.json ygg

# Each stores a file.
head -c 3000000 /dev/urandom > a.bin; head -c 3000000 /dev/urandom > m.bin
cp a.bin admin-out/; cp m.bin member-out/
wait_for 60 test -f admin-out/a.bin.ystub || fail "the admin's file wasn't stored"
wait_for 60 test -f member-out/m.bin.ystub || fail "the member's file wasn't stored"
echo "OK: both stored"

L=$(addr 3)
holds() { grep -cF "\"$L\"" "$1" || true; }
[ "$(holds admin-out/a.bin.ystub)" -gt 0 ] && [ "$(holds member-out/m.bin.ystub)" -gt 0 ] ||
	fail "n3 holds no shard of one of the files; nothing to test"
echo "   n3 holds $(holds admin-out/a.bin.ystub) shard(s) of the admin's file, $(holds member-out/m.bin.ystub) of the member's"

# The admin marks n3 leaving.
curl -fsS -X POST -H "X-Yggstore: 1" -H "Content-Type: application/json" \
	-d '{"name": "n3", "leaving": true}' http://127.0.0.1:17680/api/nodes/leaving
grep -q '"leaving":true' peers.json || fail "the admin's list doesn't mark n3"
echo "OK: n3 marked leaving"

off() { [ "$(holds "$1")" = 0 ]; }
wait_for 60 off admin-out/a.bin.ystub || fail "the admin's file still has shards on n3"
echo "OK: the admin's file moved off n3 while it is up"
wait_for 30 grep -q leaving member/shards/peers.pushed.json || fail "the member's node wasn't sent the list"
wait_for 60 grep -q leaving member/peers.json || fail "the member's dashboard didn't take the pushed list"
echo "OK: the member's node and dashboard have the list"
wait_for 60 off member-out/m.bin.ystub || fail "the member's file still has shards on n3"
echo "OK: the member's file moved off n3"
state() { curl -fsS "http://127.0.0.1:$1/api/state" | python3 -c '
import sys, json
p = [p for p in json.load(sys.stdin)["peers"] if p["name"] == "n3"][0]
print("leaving" if p.get("leaving") else "staying", p.get("mine", 0))'; }
[ "$(state 17680)" = "leaving 0" ] || fail "the admin's dashboard shows n3 as: $(state 17680)"

# n3 goes, and two more: both files still come back.
kill "$(cat node3.pid)" "$(cat node5.pid)" "$(cat node6.pid)"
sleep 1
for f in admin-out/a.bin member-out/m.bin; do
	"$BIN" get -o "$W/back.bin" "$f.ystub" >/dev/null 2>&1 || fail "$f didn't restore"
	cmp -s "$W/back.bin" "$(basename "$f")" || fail "$f restored wrong"
done
echo "OK: with n3 and two more nodes down, both files restore"

# Keep it on: n3 takes new shards again.
curl -fsS -X POST -H "X-Yggstore: 1" -H "Content-Type: application/json" \
	-d '{"name": "n3", "leaving": false}' http://127.0.0.1:17680/api/nodes/leaving
grep -q leaving peers.json && fail "n3 is still marked after Keep it on"
echo "OK: Keep it on clears the mark"
echo "PASS"
