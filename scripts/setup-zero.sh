#!/usr/bin/env bash
# Sets up a Pi Zero 2 W hub from start to end, on this dev box: writes its
# microSD card (in a card reader here) with Wi-Fi, waits for the Zero to
# come up on the network, then sets it up with scripts/provision-pi.sh: its
# shards on a USB stick, yggstore, Syncthing, the group and the hub. The
# settings are in factory/zero.conf (or the file given).
#
# The Zero: a Pi Zero 2 W (the first Pi Zero W is 32-bit and too slow), a
# microSD card of 8 GB or more, and a USB stick in its USB port (not PWR)
# through a micro-USB OTG adapter. This machine: the group's admin
# dashboard running, and the same network as the Zero's Wi-Fi.
#
# Usage: scripts/setup-zero.sh                    list the cards it could write
#        scripts/setup-zero.sh DEVICE [CONFIG]    write DEVICE, then set the Zero up
#        scripts/setup-zero.sh -s [CONFIG]        skip writing: the Zero is running
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV=${1:-}
CONF=${2:-$ROOT/factory/zero.conf}
[ -f "$CONF" ] || { echo "no config file $CONF" >&2; exit 2; }
# shellcheck disable=SC1090
. "$CONF"

say() { printf '\n== %s\n' "$*"; }
ok() { printf '   %s\n' "$*"; }
die() { printf '\nSTOPPED: %s\n' "$*" >&2; exit 1; }

[ -n "${WIFI_SSID:-}" ] || die "set WIFI_SSID and WIFI_PASSWORD in $CONF: a Pi Zero has no network socket"
[ "${STORAGE:-}" = usb ] || ok "note: STORAGE in $CONF isn't usb, so the shards go on the SD card"
[ -n "$DEV" ] || exec "$ROOT/scripts/flash-pi.sh"

# --- 1. The card -----------------------------------------------------------------
if [ "$DEV" != -s ]; then
	"$ROOT/scripts/flash-pi.sh" "$DEV" "$CONF"
	say "Starting the Zero"
	ok "1. Put the card in the Zero."
	[ "${STORAGE:-}" = usb ] && ok "2. Plug the USB stick into its USB port (the middle one, not PWR), through the OTG adapter."
	ok "3. Plug power into PWR."
	printf '\n   Press Enter once it has power: '
	read -r _ </dev/tty
	# A new card is a new machine with a new key, under a name used before.
	ssh-keygen -R "$NAME.local" >/dev/null 2>&1 || true
fi

# --- 2. On the network --------------------------------------------------------------
say "Waiting for $NAME to join ${BENCH_WIFI_SSID:-$WIFI_SSID}"
ok "the first start takes a few minutes on a Zero (it resizes the card and sets itself up)"
target=${PI_SSH:-root@$NAME.local}
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=accept-new)
up=""
for _ in $(seq 120); do
	if ssh "${SSH_OPTS[@]}" "$target" true 2>/dev/null; then up=1; break; fi
	sleep 5
done
[ -n "$up" ] || die "$NAME didn't answer at $target after 10 minutes. Check the Wi-Fi name and password in $CONF (a Zero only sees 2.4 GHz networks) and that it has power; plugging a screen into its mini-HDMI shows how far it got"
model=$(ssh "${SSH_OPTS[@]}" "$target" "tr -d '\0' </proc/device-tree/model")
ok "it answers: $model"
# Its first boot is still installing what it was asked to; apt is busy until then.
ssh "${SSH_OPTS[@]}" "$target" "cloud-init status --wait >/dev/null 2>&1 || true"
ok "its first boot is done"
case $model in
*"Zero 2"*) ;;
*Zero*) die "this is a $model: 32-bit and too slow for a hub. It needs a Pi Zero 2 W" ;;
*) ok "note: not a Pi Zero; setting it up anyway" ;;
esac

# --- 3. Set it up -----------------------------------------------------------------
exec "$ROOT/scripts/provision-pi.sh" "$CONF"
