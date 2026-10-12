#!/usr/bin/env bash
# Writes Raspberry Pi OS Lite (64-bit) to a USB drive or SD card for a hub
# Pi, ready for scripts/provision-pi.sh: on its first boot the Pi takes the
# NAME from factory/pi.conf (so it is NAME.local on the network), turns on
# SSH, lets this machine's key in as root, and joins the Wi-Fi network if
# WIFI_SSID is set. Nothing else is installed.
#
# Run it as yourself; it asks for sudo only to write the drive.
#
# Usage: scripts/flash-pi.sh                    list the drives it could write
#        scripts/flash-pi.sh DEVICE [CONFIG]    write DEVICE, e.g. /dev/sdd
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEV=${1:-}
CONF=${2:-$ROOT/factory/pi.conf}
[ -f "$CONF" ] || { echo "no config file $CONF" >&2; exit 2; }
# shellcheck disable=SC1090
. "$CONF"

say() { printf '\n== %s\n' "$*"; }
ok() { printf '   %s\n' "$*"; }
die() { printf '\nSTOPPED: %s\n' "$*" >&2; exit 1; }

IMAGE_URL=${IMAGE_URL:-https://downloads.raspberrypi.com/raspios_lite_arm64_latest}
CACHE=${CACHE:-$HOME/.cache/yggstore-factory}
SSH_KEY=${SSH_KEY:-$HOME/.ssh/id_ed25519.pub}

# Drives that may be written: whole disks on USB, or removable (SD readers).
candidates() {
	lsblk -dnpo NAME,TRAN,RM,TYPE | awk '$4 == "disk" && ($2 == "usb" || $3 == "1") {print $1}'
}
show() { lsblk -o NAME,SIZE,TRAN,MODEL,VENDOR,LABEL,MOUNTPOINTS "$1"; }

if [ -z "$DEV" ]; then
	list=$(candidates)
	[ -n "$list" ] || die "no USB drive or SD card is plugged in"
	echo "Drives that could be written:"
	for d in $list; do echo; show "$d"; done
	echo; echo "Then: scripts/flash-pi.sh /dev/sdX"
	exit 0
fi

# --- Checks: the right drive, and what goes on it ------------------------------
[[ "${NAME:-}" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,38}[A-Za-z0-9])?$ ]] ||
	die "NAME in $CONF must be letters, digits and dashes (got '${NAME:-}')"
[ -b "$DEV" ] || die "$DEV is not a drive"
candidates | grep -qx "$DEV" || die "$DEV is not a USB drive or SD card; this only writes those"
for mp in $(lsblk -nro MOUNTPOINTS "$DEV"); do
	case $mp in /|/boot|/boot/*|/home|/usr|/var|/mnt/*|/media/*/storage*) die "$DEV holds $mp, which this machine uses" ;; esac
done
size=$(lsblk -dnbo SIZE "$DEV")
[ "$size" -ge $((4 * 1024 ** 3)) ] || die "$DEV is smaller than 4 GB"
[ "$size" -le $((512 * 1024 ** 3)) ] || die "$DEV is bigger than 512 GB; is it really the Pi's drive?"
[ -f "$SSH_KEY" ] || die "no SSH key at $SSH_KEY (set SSH_KEY)"
key=$(cat "$SSH_KEY")
if [ -n "${WIFI_SSID:-}" ]; then
	[[ "$WIFI_SSID" != *[\"\\]* && ${#WIFI_SSID} -le 32 ]] || die "WIFI_SSID in $CONF must be at most 32 characters, without \" or \\"
	[ ${#WIFI_PASSWORD} -ge 8 ] && [ ${#WIFI_PASSWORD} -le 63 ] || die "WIFI_PASSWORD in $CONF must be 8 to 63 characters"
	[[ "${WIFI_COUNTRY:-}" =~ ^[A-Z]{2}$ ]] || die "WIFI_COUNTRY in $CONF must be a two-letter country code, such as ZA"
	# The WPA key itself, so the password isn't on the drive as typed.
	psk=$(WIFI_SSID=$WIFI_SSID WIFI_PASSWORD=$WIFI_PASSWORD python3 -c '
import hashlib, os
print(hashlib.pbkdf2_hmac("sha1", os.environ["WIFI_PASSWORD"].encode(), os.environ["WIFI_SSID"].encode(), 4096, 32).hex())')
fi

say "Writing Raspberry Pi OS for $NAME to $DEV"
show "$DEV"
printf '\nEverything on %s will be erased. Type its name (%s) to go on: ' "$DEV" "$(basename "$DEV")"
read -r answer
[ "$answer" = "$(basename "$DEV")" ] || die "not confirmed; nothing was written"

# --- The image -------------------------------------------------------------------
say "Getting Raspberry Pi OS Lite (64-bit)"
mkdir -p "$CACHE"
url=$(curl -fsSIL -o /dev/null -w '%{url_effective}' "$IMAGE_URL") || die "can't reach downloads.raspberrypi.com"
img="$CACHE/$(basename "$url")"
if [ -f "$img" ]; then
	ok "have $(basename "$img")"
else
	curl -fL --progress-bar -o "$img.part" "$url" || die "download failed"
	mv "$img.part" "$img"
fi
want=$(curl -fsSL "$url.sha256" | awk '{print $1}') || die "can't get the image's checksum"
[ "$(sha256sum "$img" | awk '{print $1}')" = "$want" ] || { rm -f "$img"; die "the image's checksum is wrong; run again to download it again"; }
ok "$(basename "$img"), checksum good"

# --- Write -------------------------------------------------------------------------
say "Writing (a few minutes)"
for p in $(lsblk -nrpo NAME "$DEV" | tail -n +2); do
	sudo umount "$p" 2>/dev/null || true
done
xz -dc "$img" | sudo dd of="$DEV" bs=4M conv=fsync status=progress
sudo partprobe "$DEV" 2>/dev/null || true
sudo udevadm settle

# --- First boot settings, on its boot partition --------------------------------
say "Setting up its first boot"
boot=$(lsblk -nrpo NAME "$DEV" | sed -n 2p)
[ -b "$boot" ] || die "can't find the boot partition on $DEV"
mnt=$(mktemp -d)
sudo mount "$boot" "$mnt"
trap 'sudo umount "$mnt" 2>/dev/null; rmdir "$mnt"' EXIT
[ -f "$mnt/user-data" ] || die "this image has no cloud-init user-data; it is older than this script expects"
# An empty "ssh" file turns SSH on (sshswitch); cloud-init does the rest.
sudo touch "$mnt/ssh"
# Without userconf.txt, the first boot stops at a "create a user" dialog on
# the Pi's screen until someone answers it. This sets the pi user up with a
# random password nobody knows instead (SSH takes keys only).
printf 'pi:%s\n' "$(openssl passwd -6 "$(openssl rand -base64 24)")" | sudo tee "$mnt/userconf.txt" >/dev/null
sudo tee "$mnt/user-data" >/dev/null <<EOF
#cloud-config
# Written by yggstore's scripts/flash-pi.sh for the hub $NAME.
hostname: $NAME
manage_etc_hosts: true
# The default user (pi, no password) and root both take this key; root is
# what scripts/provision-pi.sh logs in as.
disable_root: false
ssh_pwauth: false
ssh_authorized_keys:
  - $key
packages:
  - avahi-daemon
EOF
if [ -n "${WIFI_SSID:-}" ]; then
	sudo tee "$mnt/network-config" >/dev/null <<EOF
# Written by yggstore's scripts/flash-pi.sh: the Wi-Fi network $NAME joins.
network:
  version: 2
  wifis:
    renderer: NetworkManager
    wlan0:
      dhcp4: true
      optional: true
      regulatory-domain: "$WIFI_COUNTRY"
      access-points:
        "$WIFI_SSID":
          password: "$psk"
EOF
	# Wi-Fi stays off until it knows its country, from the first boot on.
	grep -q 'cfg80211.ieee80211_regdom=' "$mnt/cmdline.txt" ||
		sudo sed -i "1s/\$/ cfg80211.ieee80211_regdom=$WIFI_COUNTRY/" "$mnt/cmdline.txt"
fi
sudo umount "$mnt"
sync
ok "name $NAME, SSH on, this machine's key ($(basename "$SSH_KEY")) let in as root"
if [ -n "${WIFI_SSID:-}" ]; then
	ok "joins the Wi-Fi network $WIFI_SSID ($WIFI_COUNTRY)"
	printf '\nDONE: put the drive in the Pi and plug in power; it joins %s.\n' "$WIFI_SSID"
else
	printf '\nDONE: put the drive in the Pi, plug in a network cable and power.\n'
fi
printf 'After a minute or two: ssh root@%s.local, then scripts/provision-pi.sh\n' "$NAME"
