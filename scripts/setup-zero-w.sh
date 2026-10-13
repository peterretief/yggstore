#!/usr/bin/env bash
# Sets up a first Pi Zero W (not a Zero 2 W; it says "Zero W V1.1" on the
# back) as a hub, to try it: the same as scripts/setup-zero.sh, with the same
# settings (factory/zero.conf), but with the 32-bit Raspberry Pi OS Lite,
# which is all it can run, and more time for its first start. It is
# single-core with 512 MB, so it works, slowly; a Pi Zero 2 W is better.
#
# Kept for reference. Tried once (2026-10-13) on a Zero W V1.1: it started
# and set itself up on the 32-bit OS, but its Wi-Fi chip never came up
# (brcmfmac: "Downloaded RAM image is corrupted", on kernels 6.12 and 6.18,
# with good power and a slower SDIO bus), so that board was most likely
# faulty. A working Zero W should get further; IMAGE_URL picks another image.
#
# Usage: the same as scripts/setup-zero.sh.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
export IMAGE_URL=${IMAGE_URL:-https://downloads.raspberrypi.com/raspios_lite_armhf_latest}
export WAIT_MINUTES=20
export ZERO_W=1
exec "$ROOT/scripts/setup-zero.sh" "$@"
