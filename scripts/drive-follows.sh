#!/bin/sh
# drive-follows.sh MOUNTPOINT LABEL SERVICE
#
# Run as root on a box whose node keeps its shards on a USB drive (in
# /etc/fstab by LABEL, with nofail). The node then follows the drive:
#   - without it mounted, the node stays down, rather than fill the
#     machine's own disk under the empty mount point;
#   - when the drive comes back (unplugged, knocked, or dropped off USB),
#     it is mounted again and the node started, by themselves.
#
#   drive-follows.sh /srv/yggstore/stick1 yggdata-1 yggstore-node-localbox-004-usb1
set -eu
[ $# = 3 ] || { echo "usage: $0 MOUNTPOINT LABEL SERVICE" >&2; exit 2; }
mnt=$1 label=$2 svc=${3%.service}
mount_unit=$(systemd-escape -p --suffix=mount "$mnt")

grep -q "^LABEL=$label[[:space:]]" /etc/fstab || { echo "no LABEL=$label line in /etc/fstab" >&2; exit 1; }

mkdir -p "/etc/systemd/system/$svc.service.d" "/etc/systemd/system/$mount_unit.d"
printf '[Unit]\n# The shards live on the USB drive: without it, stay down.\nRequiresMountsFor=%s\n' "$mnt" \
	> "/etc/systemd/system/$svc.service.d/drive.conf"
printf '[Unit]\n# Mounted again (see the udev rule): start the node again too.\nWants=%s.service\n' "$svc" \
	> "/etc/systemd/system/$mount_unit.d/node.conf"
printf '# The drive is back: mount it (which starts its node).\nACTION=="add", SUBSYSTEM=="block", ENV{ID_FS_LABEL}=="%s", ENV{SYSTEMD_WANTS}+="%s"\n' "$label" "$mount_unit" \
	> "/etc/udev/rules.d/90-yggstore-$label.rules"

systemctl daemon-reload
udevadm control --reload
echo "$svc now follows the drive $label at $mnt"
