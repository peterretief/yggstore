#!/bin/sh
# Connects this computer to the box @BOX@ with Syncthing, so a folder here
# is kept in step with your folder on the box. It installs Syncthing if it
# isn't here, starts it when you log in, adds the box and puts the folders
# the box offers you in your home folder. Nothing to do in Syncthing itself.
# From the box at @HUB@; run it again at any time, it skips what's done.
set -eu
HUB='@HUB@'
BOX='@BOX@'
BOX_ID='@BOX_ID@'
BOX_ADDRS="${YGGSTORE_BOX_ADDRS:-@ADDRS@}" # the variable is for tests
RELEASES=https://github.com/syncthing/syncthing/releases

say() { printf '%s\n' "$*"; }
die() { printf '%s\n' "$*" >&2; exit 1; }
fetch() { # URL FILE
	if command -v curl >/dev/null 2>&1; then curl -fsSL -o "$2" "$1"; else wget -qO "$2" "$1"; fi
}
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

case "$(uname -s)-$(uname -m)" in
	Linux-x86_64) p=linux-amd64 ;;
	Linux-aarch64|Linux-arm64) p=linux-arm64 ;;
	Linux-armv6l|Linux-armv7l|Linux-armv8l) p=linux-arm ;;
	Darwin-*) p=macos-universal ;;
	*) die "This computer ($(uname -s) $(uname -m)) isn't one Syncthing makes a download for." ;;
esac

# 1. Syncthing: the one already here, or the latest release.
st="${SYNCTHING:-}"
[ -n "$st" ] || [ ! -x "$HOME/.local/bin/syncthing" ] || st="$HOME/.local/bin/syncthing"
[ -n "$st" ] || st="$(command -v syncthing 2>/dev/null || true)"
app=/Applications/Syncthing.app/Contents/Resources/syncthing/syncthing
[ -n "$st" ] || [ ! -x "$app" ] || st="$app"
if [ -z "$st" ]; then
	say "Downloading Syncthing..."
	fetch https://api.github.com/repos/syncthing/syncthing/releases/latest "$tmp/latest.json"
	v="$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
	[ -n "$v" ] || die "Couldn't find Syncthing's latest release."
	case $p in macos-*) ext=zip ;; *) ext=tar.gz ;; esac
	f="syncthing-$p-$v.$ext"
	fetch "$RELEASES/download/$v/$f" "$tmp/$f"
	fetch "$RELEASES/download/$v/sha256sum.txt.asc" "$tmp/sums"
	want="$(awk -v f="$f" '$2 == f { print $1 }' "$tmp/sums")"
	if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$tmp/$f" | cut -d' ' -f1)"; else got="$(shasum -a 256 "$tmp/$f" | cut -d' ' -f1)"; fi
	[ -n "$want" ] && [ "$want" = "$got" ] || die "The Syncthing download doesn't match its checksum; stopping."
	case $ext in zip) (cd "$tmp" && unzip -q "$f") ;; *) tar -xzf "$tmp/$f" -C "$tmp" ;; esac
	mkdir -p "$HOME/.local/bin"
	mv "$tmp/syncthing-$p-$v/syncthing" "$HOME/.local/bin/syncthing"
	st="$HOME/.local/bin/syncthing"
	say "Installed Syncthing $v in $st."
fi

# 2. Running, and started when you log in.
running() { "$st" cli show system >/dev/null 2>&1; }
background() { # starts it now, for this session
	nohup "$st" serve --no-browser >/dev/null 2>&1 &
}
export STNODEFAULTFOLDER=1 # its first start makes no "Default Folder"
if ! running; then
	if [ -n "${YGGSTORE_NO_SERVICE:-}" ]; then # for tests
		background
	elif [ "$(uname -s)" = Darwin ]; then
		if [ "$st" = "$app" ]; then
			open -a Syncthing # the app starts itself at login
		else
			plist="$HOME/Library/LaunchAgents/net.syncthing.syncthing.plist"
			mkdir -p "$HOME/Library/LaunchAgents"
			cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>net.syncthing.syncthing</string>
	<key>ProgramArguments</key><array><string>$st</string><string>serve</string><string>--no-browser</string></array>
	<key>EnvironmentVariables</key><dict><key>STNODEFAULTFOLDER</key><string>1</string></dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ProcessType</key><string>Background</string>
</dict></plist>
EOF
			launchctl unload "$plist" 2>/dev/null || true
			launchctl load -w "$plist"
		fi
	elif command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
		if ! systemctl --user cat syncthing.service >/dev/null 2>&1; then
			mkdir -p "$HOME/.config/systemd/user"
			cat >"$HOME/.config/systemd/user/syncthing.service" <<EOF
[Unit]
Description=Syncthing, keeping folders in step with the box $BOX
After=network.target

[Service]
ExecStart=$st serve --no-browser --no-restart
Environment=STNODEFAULTFOLDER=1
Restart=on-failure

[Install]
WantedBy=default.target
EOF
			systemctl --user daemon-reload
		fi
		systemctl --user enable --now syncthing.service >/dev/null 2>&1
	else
		mkdir -p "$HOME/.config/autostart"
		cat >"$HOME/.config/autostart/syncthing.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Syncthing
Exec=$st serve --no-browser
X-GNOME-Autostart-enabled=true
EOF
		background
	fi
	say "Starting Syncthing..."
	n=0
	until running; do
		n=$((n + 1))
		[ $n -lt 60 ] || die "Syncthing didn't start; try restarting the computer and running this again."
		sleep 1
	done
fi
me="$("$st" cli show system | sed -n 's/.*"myID": *"\([^"]*\)".*/\1/p')"
[ -n "$me" ] || die "Couldn't read this computer's Syncthing ID."

# 3. The box.
if ! "$st" cli config devices list | grep -qx "$BOX_ID"; then
	# Addresses one by one: "devices add --addresses" crashes Syncthing 1.29.
	"$st" cli config devices add --device-id "$BOX_ID" --name "$BOX"
	for a in $BOX_ADDRS; do
		[ "$a" = dynamic ] || "$st" cli config devices "$BOX_ID" addresses add "$a"
	done
	say "Added the box $BOX."
fi

# 4. The folders it offers you: it takes this computer within half a minute
# of it connecting.
say "Waiting for the box to take this computer..."
n=0
while :; do
	if fetch "$HUB/api/connect/folders?device=$me" "$tmp/folders" 2>/dev/null && [ -s "$tmp/folders" ]; then
		break
	fi
	n=$((n + 1))
	if [ $n -ge 90 ]; then
		say "The box hasn't taken this computer yet. It may not be taking new devices"
		say "today: ask whoever looks after it, then run this again."
		say "This computer's Syncthing ID: $me"
		exit 1
	fi
	sleep 2
done
mine=""
tab="$(printf '\t')"
while IFS="$tab" read -r id label rel; do
	[ -n "$id" ] || continue
	if "$st" cli config folders list | grep -qx "$id"; then
		if ! "$st" cli config folders "$id" devices list | grep -qx "$BOX_ID"; then
			# The same ID from somewhere else, such as another box.
			say "Not adding $label: this computer has a folder with the same ID ($id),"
			say "kept in step with another device. Ask whoever looks after the box."
			continue
		fi
		path="$("$st" cli config folders "$id" path get 2>/dev/null || true)"
	else
		path="$HOME/$rel"
		# Not into a folder of yours that's already in use.
		if [ "$rel" = yggstore ] && [ -n "$(ls -A "$path" 2>/dev/null)" ]; then path="$HOME/yggstore-$BOX"; fi
		mkdir -p "$path"
		"$st" cli config folders add --id "$id" --label "$label" --path "$path"
		"$st" cli config folders "$id" devices add --device-id "$BOX_ID"
		say "Added $label: $path"
	fi
	case $id in yggstore-websites|yggstore-team-*) ;; *) mine="$path" ;; esac
done <"$tmp/folders"

say
say "Done. This computer is connected to the box $BOX."
if [ -n "$mine" ]; then
	say "Your folder is $mine. Put files in its files folder to keep them safe;"
	say "each becomes a small .ystub file. Copy a .ystub into restore to get it back."
fi
say "This computer's Syncthing ID (for Work from anywhere): $me"
