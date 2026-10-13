#!/usr/bin/env bash
# Sets up a Raspberry Pi hub in the factory, from this dev box: names it,
# installs yggstore and Syncthing, joins it to the group, starts its node,
# hub and Syncthing at every boot, and checks it all answers. The settings
# are in factory/pi.conf (or the file given).
#
# The Pi: Raspberry Pi OS Lite (64-bit) written with scripts/flash-pi.sh (or
# Raspberry Pi Imager, with SSH and this machine's key turned on), on this
# network, with internet access. This
# machine: the group's admin dashboard running (it makes the invite).
#
# Each step is skipped if it is done already, so after fixing whatever
# stopped it, run it again.
#
# Usage: scripts/provision-pi.sh [-n] [CONFIG]     -n: show the steps, change nothing
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DRY=""
if [ "${1:-}" = "-n" ]; then DRY=1; shift; fi
CONF=${1:-$ROOT/factory/pi.conf}
[ -f "$CONF" ] || { echo "no config file $CONF" >&2; exit 2; }
# shellcheck disable=SC1090
. "$CONF"

say() { printf '\n== %s\n' "$*"; }
ok() { printf '   %s\n' "$*"; }
die() { printf '\nSTOPPED: %s\n' "$*" >&2; exit 1; }

# --- The settings ---------------------------------------------------------------
[[ "${NAME:-}" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,38}[A-Za-z0-9])?$ ]] ||
	die "NAME in $CONF must be letters, digits and dashes (got '${NAME:-}')"
[ -n "${PI_SSH:-}" ] || die "PI_SSH is not set in $CONF"
[[ "${QUOTA_GB:-}" =~ ^[0-9]+$ ]] || die "QUOTA_GB in $CONF must be a number of GB"
[[ -n "${OWNER:-}" && "$OWNER" != *"'"* ]] || die "OWNER in $CONF must be set, without quotes"
ADMIN_DASHBOARD=${ADMIN_DASHBOARD:-http://127.0.0.1:7480}
PAIR_LIMIT=${PAIR_LIMIT:-5}
SITE_MAX_MB=${SITE_MAX_MB:-500}
customers=""
[ "${CUSTOMERS:-no}" = yes ] && customers="-customers"

say "Setting up $NAME for ${GROUP:-the group} (from $CONF)"
ok "Pi on the bench: $PI_SSH; owner $OWNER; gives ${QUOTA_GB} GB; customers: ${CUSTOMERS:-no}"
[ -n "$DRY" ] && ok "(dry run: nothing is changed)"

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new)
user=${PI_SSH%@*}
[ "$user" = "$PI_SSH" ] && user=$(whoami)
SUDO=""
[ "$user" = root ] || SUDO="sudo"

# on runs a script on the Pi as root.
on() {
	if [ -n "$DRY" ]; then
		printf '   [on the Pi] %s\n' "$(printf '%s' "$1" | head -1)"
		return 0
	fi
	ssh "${SSH_OPTS[@]}" "$TARGET" "$SUDO bash -s" <<<"set -euo pipefail; $1"
}
# ask runs a command on the Pi and prints its answer, even in a dry run.
ask() { ssh "${SSH_OPTS[@]}" "$TARGET" "$SUDO bash -c $(printf '%q' "$1")"; }

# --- 1. Reach the Pi ------------------------------------------------------------
say "1. Reaching the Pi"
TARGET=$PI_SSH
ask true 2>/dev/null || die "can't log in to $PI_SSH with this machine's SSH key. Check it is on, on this network, and that Raspberry Pi Imager put this machine's key on it"
arch=$(ask "uname -m")
# yggstore is built here for the Pi's processor: 64-bit, or 32-bit on a first
# Pi Zero W (ARMv6), which can't run the 64-bit OS.
case $arch in
aarch64) GOENV=(GOARCH=arm64) ;;
armv6l) GOENV=(GOARCH=arm GOARM=6) ;;
armv7l) GOENV=(GOARCH=arm GOARM=7) ;;
*) die "the Pi runs $arch; flash Raspberry Pi OS Lite" ;;
esac
ip=$(ask "hostname -I | awk '{print \$1}'")
[ -n "$ip" ] || die "the Pi has no network address"
# From here on, by address: its .local name changes when it is renamed.
TARGET="$user@$ip"
ok "reached $(ask hostname) at $ip ($arch)"

# --- 2. Its name --------------------------------------------------------------
say "2. Naming it $NAME"
if [ "$(ask hostname)" = "$NAME" ]; then
	ok "already called $NAME"
else
	on "hostnamectl set-hostname '$NAME'
		sed -i 's/^127\.0\.1\.1.*/127.0.1.1\t$NAME/' /etc/hosts
		grep -q '^127\.0\.1\.1' /etc/hosts || echo '127.0.1.1	$NAME' >> /etc/hosts
		systemctl restart avahi-daemon 2>/dev/null || true"
	ok "now $NAME, on the network as $NAME.local"
fi

# --- 3. Software ----------------------------------------------------------------
say "3. Installing yggstore and Syncthing"
BIN=$(mktemp -d)/yggstore-linux-$arch
trap 'rm -rf "$(dirname "$BIN")"' EXIT
version="factory-$(cd "$ROOT" && git rev-parse --short HEAD 2>/dev/null || echo dev)"
(cd "$ROOT" && env CGO_ENABLED=0 GOOS=linux "${GOENV[@]}" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$BIN" ./cmd/yggstore)
if [ -z "$DRY" ]; then
	scp -q "${SSH_OPTS[@]}" "$BIN" "$TARGET:/tmp/yggstore"
else
	ok "[copy] yggstore $version to the Pi"
fi
on "install -m 755 /tmp/yggstore /usr/local/bin/yggstore && rm -f /tmp/yggstore"
ok "yggstore $version in /usr/local/bin"
if ask "command -v syncthing >/dev/null && command -v avahi-daemon >/dev/null" 2>/dev/null; then
	ok "Syncthing already installed"
else
	on "export DEBIAN_FRONTEND=noninteractive
		apt-get update -q
		apt-get install -y -q --no-install-recommends syncthing avahi-daemon"
	ok "Syncthing installed"
fi
on "systemctl enable --now syncthing@root
	for i in \$(seq 60); do
		[ -f /root/.local/state/syncthing/config.xml ] || [ -f /root/.config/syncthing/config.xml ] && exit 0
		sleep 1
	done
	echo 'Syncthing did not start; see journalctl -u syncthing@root' >&2; exit 1"
ok "Syncthing runs at every boot"

# --- 3b. Its storage -------------------------------------------------------------
SHARDS=/root/.yggstore/shards
if [ "${STORAGE:-sd}" = usb ]; then
	say "3b. Keeping the shards on a USB stick"
	if ask "mountpoint -q $SHARDS" 2>/dev/null; then
		ok "the stick is already mounted at $SHARDS"
	else
		# USB disks on the Pi, not counting the one it boots from.
		root=$(ask "lsblk -no PKNAME \$(findmnt -no SOURCE /)")
		sticks=$(ask "lsblk -dnpo NAME,TRAN,TYPE | awk '\$2 == \"usb\" && \$3 == \"disk\" {print \$1}' | grep -vx /dev/$root || true")
		[ -n "$sticks" ] || die "no USB stick is plugged into the Pi (on a Pi Zero: in the USB port, not PWR, through an OTG adapter)"
		[ "$(printf '%s\n' "$sticks" | wc -l)" = 1 ] || die "more than one USB stick is plugged into the Pi; leave only the one for the shards"
		stick=$sticks
		gb=$(( $(ask "lsblk -dnbo SIZE $stick") / 1000000000 ))
		[ "$gb" -ge "$QUOTA_GB" ] || die "the stick holds ${gb} GB, less than QUOTA_GB ($QUOTA_GB)"
		if ask "test -n \"\$(ls -A $SHARDS 2>/dev/null)\"" 2>/dev/null; then
			die "the node already keeps shards on the SD card ($SHARDS); this doesn't move them"
		fi
		if ask "blkid -L yggdata" >/dev/null 2>&1; then
			ok "$stick is already this Pi's yggstore stick"
		elif [ -n "$DRY" ]; then
			ok "[on the Pi] format $stick ($gb GB) as yggdata, after asking"
		else
			ask "lsblk -o NAME,SIZE,MODEL,VENDOR,LABEL,FSTYPE $stick"
			printf '\nEverything on the stick %s in the Pi will be erased. Type "erase" to go on: ' "$stick"
			read -r answer </dev/tty
			[ "$answer" = erase ] || die "not confirmed; the stick wasn't touched"
			on "wipefs -aq $stick
				printf 'label: gpt\n,,L\n' | sfdisk -q $stick
				udevadm settle
				mkfs.ext4 -q -F -L yggdata \$(lsblk -nrpo NAME $stick | sed -n 2p)"
			ok "formatted $stick as yggdata"
		fi
		# nofail: the Pi still boots without it; the node then waits for it
		# rather than filling the SD card.
		on "mkdir -p $SHARDS
			grep -q '^LABEL=yggdata ' /etc/fstab || echo 'LABEL=yggdata $SHARDS ext4 defaults,noatime,nofail 0 2' >> /etc/fstab
			mkdir -p /etc/systemd/system/yggstore-node.service.d
			printf '[Unit]\nRequiresMountsFor=$SHARDS\n' > /etc/systemd/system/yggstore-node.service.d/storage.conf
			systemctl daemon-reload
			mount $SHARDS && chmod 700 $SHARDS"
		ok "the stick is mounted at $SHARDS, at every boot"
	fi
fi

# make_invite gets an invite for the Pi from the admin dashboard, once.
invite="" ygg_peers=""
make_invite() {
	[ -n "$invite" ] && return 0
	local resp
	resp=$(curl -fsS -m 20 -X POST -H "X-Yggstore: 1" -H "Content-Type: application/json" \
		-d "{\"for\": \"box $NAME\"}" "$ADMIN_DASHBOARD/api/invites") ||
		die "the admin dashboard at $ADMIN_DASHBOARD didn't make an invite. Is it running, and is it the admin's?"
	invite=$(printf '%s' "$resp" | python3 -c 'import sys,json; print(json.load(sys.stdin)["invite"])')
	ygg_peers=$(printf '%s' "$resp" | python3 -c 'import sys,json; print(",".join(json.load(sys.stdin).get("ygg_peers") or []))')
	ok "invite made on the admin dashboard"
}

# --- 3c. Yggdrasil -------------------------------------------------------------
# The Yggdrasil daemon, rather than the node's built-in one: it reaches
# every member over TCP, whatever their firewall lets in and whichever
# yggstore they run (built-in nodes talk to each other over UDP).
say "3c. Yggdrasil"
NODE_UNIT=/etc/systemd/system/yggstore-node.service
if ask "systemctl is-active -q yggdrasil" 2>/dev/null; then
	ok "Yggdrasil runs"
else
	# A node joined before this step was here runs the built-in Yggdrasil:
	# the daemon takes its key, so the node keeps its address, and its peers.
	if ask "grep -q -- '-transport builtin' $NODE_UNIT" 2>/dev/null; then
		ygg_peers=$(ask "grep -o -- '-ygg-peers [^ ]*' $NODE_UNIT | cut -d' ' -f2")
	elif [ -n "$DRY" ]; then
		ygg_peers="(the invite's peers)"
	else
		make_invite
	fi
	[ -n "$ygg_peers" ] || die "the invite names no Yggdrasil peers to connect through"
	on "export DEBIAN_FRONTEND=noninteractive
		command -v yggdrasil >/dev/null || { apt-get update -q; apt-get install -y -q --no-install-recommends yggdrasil; }
		systemctl stop yggdrasil 2>/dev/null || true
		mkdir -p /etc/yggdrasil
		key=''
		[ -f /root/.yggstore/ygg.key ] && key=\$(tr -d ' \n' </root/.yggstore/ygg.key)
		[ -z \"\$key\" ] && [ -f /etc/yggdrasil/yggdrasil.conf ] && key=\$(grep -oE '[0-9a-f]{128}' /etc/yggdrasil/yggdrasil.conf | head -1)
		yggdrasil -genconf -json | KEY=\"\$key\" PEERS='$ygg_peers' python3 -c '
import json, os, sys
c = json.load(sys.stdin)
if os.environ[\"KEY\"]: c[\"PrivateKey\"] = os.environ[\"KEY\"]
c[\"Peers\"] = os.environ[\"PEERS\"].split(\",\")
json.dump(c, sys.stdout, indent=2)' > /etc/yggdrasil/yggdrasil.conf.new
		# The package runs it as the yggdrasil user.
		chown root:yggdrasil /etc/yggdrasil/yggdrasil.conf.new
		chmod 640 /etc/yggdrasil/yggdrasil.conf.new
		mv /etc/yggdrasil/yggdrasil.conf.new /etc/yggdrasil/yggdrasil.conf
		systemctl enable -q yggdrasil
		systemctl reset-failed yggdrasil 2>/dev/null || true
		systemctl restart yggdrasil
		for i in \$(seq 30); do ip -6 addr | grep -qE 'inet6 2[0-9a-f]{2}:' && exit 0; sleep 1; done
		echo 'Yggdrasil did not start; see journalctl -u yggdrasil' >&2; exit 1"
	ok "Yggdrasil runs at every boot, through $(printf '%s' "$ygg_peers" | awk -F, '{print NF}') peer(s) and the local network"
fi
if ask "grep -q -- '-transport builtin' $NODE_UNIT" 2>/dev/null; then
	on "sed -i -E 's/ -transport builtin -ygg-key [^ ]+ -ygg-peers [^ ]+//' $NODE_UNIT
		systemctl daemon-reload
		systemctl restart yggstore-node yggstore-dashboard"
	ok "the node now goes through the Yggdrasil daemon, at the same address"
fi

# --- 4. The group -----------------------------------------------------------------
say "4. Joining the group"
# Anchored, so pgrep doesn't find the shell running this very check.
if ask "test -f /root/.yggstore/peers.json || pgrep -f '^[^ ]*yggstore serve' >/dev/null" 2>/dev/null; then
	ok "already in the group (a node runs on it)"
elif [ -n "$DRY" ]; then
	ok "[dev box] make an invite on $ADMIN_DASHBOARD for $NAME"
	on "yggstore join -service -name $NAME -me $OWNER -quota $QUOTA_GB $customers INVITE"
else
	make_invite
	on "yggstore join -service -name '$NAME' -me '$OWNER' -quota '$QUOTA_GB' $customers '$invite'" ||
		die "joining failed (see above)"
	ok "joined as node $NAME; its node and dashboard run at every boot"
fi

# --- 5. The hub ---------------------------------------------------------------------
say "5. Starting the hub"
if ask "test -f /etc/systemd/system/yggstore-hub.service" 2>/dev/null; then
	on "systemctl restart yggstore-hub"
	ok "hub was set up already; restarted with the new yggstore"
else
	on "yggstore hub -service -pair-limit '$PAIR_LIMIT' -site-max '$SITE_MAX_MB'"
	ok "hub runs at every boot, on port 7490"
fi
# Sites published from the Pi are in the hub's websites/ folder by themselves.
for site in ${SITES:-}; do
	if ask "yggstore site list 2>/dev/null | grep -q '^$site '" 2>/dev/null; then
		ok "keeps the website $site, in ~/yggstore-hub/websites/$site"
	else
		ok "note: $site isn't published from the Pi yet; publish it once there (yggstore site publish FOLDER -name $site)"
	fi
done

# --- 6. Check -------------------------------------------------------------------------
say "6. Checking"
if [ -n "$DRY" ]; then
	ok "[check] http://$ip:7490: node up, the group answers, Syncthing ready"
	printf '\nDry run done: nothing was changed.\n'
	exit 0
fi
state=""
for _ in $(seq 45); do
	state=$(curl -fsS -m 10 "http://$ip:7490/api/state" 2>/dev/null) &&
		printf '%s' "$state" | python3 -c '
import sys, json
s = json.load(sys.stdin); n = s.get("node") or {}
sys.exit(0 if n.get("up") and (s.get("sync") or {}).get("device_id") and s["up"] >= 2 else 1)' && break
	sleep 2
done
printf '%s' "$state" | python3 -c '
import sys, json
try:
    s = json.load(sys.stdin)
except ValueError:
    print("   the hub page did not answer"); sys.exit(1)
n = s.get("node") or {}; y = s.get("sync") or {}
print("   node %s: %s" % (n.get("name", "?"), "up" if n.get("up") else "NOT UP"))
print("   group: %d of %d machines answer" % (s["up"], s["nodes"]))
print("   Syncthing: %s" % (y.get("device_id") or "NOT READY"))
if s.get("problem"): print("   problem: " + s["problem"])
sys.exit(0 if n.get("up") and y.get("device_id") else 1)' || die "the Pi isn't working yet; see above, and journalctl -u yggstore-hub on the Pi"

printf '\nDONE: %s is set up. Its page for users: http://%s.local:7490\n' "$NAME" "$NAME"
if [ -n "${WIFI_SSID:-}" ]; then
	printf 'At its destination it needs only power, in reach of %s.\n' "$WIFI_SSID"
else
	printf 'At its destination it needs only power and a network cable.\n'
fi
