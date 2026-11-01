#!/bin/sh
# Install yggstore and join a group with an invite, on Linux or macOS:
#   curl -fsSL https://get.withcare.co.za/install.sh | sh -s -- 'yggjoin1:...'
# It goes in ~/.local/bin, joins with the invite (using yggstore's own
# Yggdrasil if this machine has none), and starts at every login. No
# admin password is needed.
set -e
base=${YGGSTORE_GET:-https://get.withcare.co.za}
invite=${1:-}

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=macos ;;
*) echo "This installs yggstore on Linux and macOS." >&2; exit 1 ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "There's no yggstore for $(uname -m) here yet." >&2; exit 1 ;;
esac
file=yggstore-$os-$arch
bin=$HOME/.local/bin
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading yggstore for $os ($arch)..."
curl -fsSL "$base/$file" -o "$tmp/yggstore"
curl -fsSL "$base/SHA256SUMS" -o "$tmp/SHA256SUMS"
want=$(awk -v f="$file" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
got=$({ sha256sum "$tmp/yggstore" 2>/dev/null || shasum -a 256 "$tmp/yggstore"; } | awk '{ print $1 }')
if [ -z "$want" ] || [ "$want" != "$got" ]; then
	echo "The download didn't arrive whole, so nothing was installed. Run it again." >&2
	exit 1
fi
mkdir -p "$bin"
mv "$tmp/yggstore" "$bin/yggstore"
chmod 755 "$bin/yggstore"
echo "Installed $("$bin/yggstore" version) in $bin."

if [ -z "$invite" ]; then
	echo "To join a group: $bin/yggstore join -service 'yggjoin1:...'"
	exit 0
fi
"$bin/yggstore" join -service "$invite"

url=http://127.0.0.1:7480
echo
echo "Your dashboard: $url"
if command -v xdg-open >/dev/null 2>&1; then
	xdg-open "$url" >/dev/null 2>&1 || true
elif command -v open >/dev/null 2>&1; then
	open "$url" >/dev/null 2>&1 || true
fi
case ":$PATH:" in
*":$bin:"*) ;;
*) echo "To run yggstore by name, add $bin to your PATH." ;;
esac
