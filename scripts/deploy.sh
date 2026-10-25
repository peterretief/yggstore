#!/usr/bin/env bash
# Build yggstore from this commit and put it on the group's machines.
#
#   scripts/deploy.sh [--dry-run] [--dirty] NODE... | web | boxes | all
#   scripts/deploy.sh --status               what each machine runs now
#   scripts/deploy.sh --back NODE...         put the binary from before the last deploy back
#
# NODE is a name from the table below; web is the web nodes, boxes the
# rest. Web nodes go one at a time, each checked before the next, so the
# group's sites and sign-in stay up. Each machine keeps the binary it ran
# before as EXE.prev. The working tree must be committed (--dirty builds it
# anyway, as dev-HASH-dirty), so what runs always matches a commit.
#
# Machines need ssh from here, and sudo (or root) there. See docs/deploy.md.
set -euo pipefail
cd "$(dirname "$0")/.."

# NAME          SSH        ARCH   BINARY                          AS    RESTART (services; "user:" = systemctl --user, "rc:" = OpenRC, "docker:" = a container)
NODES='
desktop        local      amd64  bin/yggstore                    -     user:yggstore-node user:yggstore-dashboard
localmail      localmail  amd64  /usr/local/bin/yggstore         sudo  yggstore yggstore-hub
localbox-004   pi4        amd64  /usr/local/bin/yggstore         sudo  yggstore-node yggstore-node-localbox-004-usb1 yggstore-node-localbox-004-usb2 yggstore-hub yggstore-dashboard yggstore-dashboard-localbox-004-usb1 yggstore-dashboard-localbox-004-usb2
localbox-003   pi3        arm64  /usr/local/bin/yggstore         sudo  yggstore-node yggstore-hub yggstore-dashboard
localbox-002   local      amd64  /media/peter/storage1/yggnodes/localbox-002/yggstore/bin/yggstore -  docker:localbox-002-node-1 docker:localbox-002-dashboard-1
'
WEB="desktop localmail localbox-004"

die() { printf '%s\n' "$*" >&2; exit 1; }
field() { awk -v n="$1" -v f="$2" '$1 == n { print $f }' <<<"$NODES"; }
rest() { awk -v n="$1" '$1 == n { for (i = 6; i <= NF; i++) printf "%s ", $i }' <<<"$NODES"; }
names() { awk 'NF { print $1 }' <<<"$NODES"; }

# on NODE CMD: runs CMD on the machine, as root where it needs to be.
on() {
	local n=$1 cmd=$2
	case "$(field "$n" 2)" in
	local) bash -c "$cmd" ;;
	*) ssh -o ConnectTimeout=10 -t "$(field "$n" 2)" "$cmd" ;;
	esac
}
root() { # the prefix for commands as root there
	case "$(field "$1" 5)" in sudo) echo "sudo " ;; *) echo "" ;; esac
}
restart() { # the shell commands that restart NODE's services
	local out=""
	for s in $(rest "$1"); do
		case "$s" in
		user:*) out+="systemctl --user restart ${s#user:}; " ;;
		rc:*) out+="$(root "$1")rc-service ${s#rc:} restart; " ;;
		docker:*) out+="docker restart ${s#docker:} >/dev/null; " ;;
		*) out+="$(root "$1")systemctl restart $s; " ;;
		esac
	done
	printf '%s' "$out"
}
running() { # the shell commands that check NODE's services are up
	local out=""
	for s in $(rest "$1"); do
		case "$s" in
		user:*) out+="systemctl --user is-active --quiet ${s#user:} || { echo '${s#user:} is not running'; exit 1; }; " ;;
		rc:*) out+="$(root "$1")rc-service ${s#rc:} status >/dev/null || { echo '${s#rc:} is not running'; exit 1; }; " ;;
		docker:*) out+="[ \"\$(docker inspect -f '{{.State.Running}}' ${s#docker:} 2>/dev/null)\" = true ] || { echo '${s#docker:} is not running'; exit 1; }; " ;;
		*) out+="systemctl is-active --quiet $s || { echo '$s is not running'; exit 1; }; " ;;
		esac
	done
	printf '%s' "$out"
}

dry=false dirty=false mode=deploy targets=()
for a in "$@"; do
	case "$a" in
	--dry-run) dry=true ;;
	--dirty) dirty=true ;;
	--status) mode=status ;;
	--back) mode=back ;;
	all) targets+=($(names)) ;;
	web) targets+=($WEB) ;;
	boxes) for n in $(names); do [[ " $WEB " == *" $n "* ]] || targets+=("$n"); done ;;
	-*) die "Not an option: $a" ;;
	*) [ -n "$(field "$a" 1)" ] || die "No machine $a (see the table at the top of $0)."; targets+=("$a") ;;
	esac
done

if [ $mode = status ]; then
	[ ${#targets[@]} -gt 0 ] || targets=($(names))
	for n in "${targets[@]}"; do
		v=$(on "$n" "$(field "$n" 4) version" 2>/dev/null | tr -d '\r' | sed 's/^yggstore //') || v="(not answering)"
		printf '%-14s %s\n' "$n" "$v"
	done
	exit 0
fi
[ ${#targets[@]} -gt 0 ] || { sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
# Web nodes first, one at a time, then the rest.
ordered=()
for n in $WEB; do [[ " ${targets[*]} " == *" $n "* ]] && ordered+=("$n"); done
for n in "${targets[@]}"; do [[ " $WEB " == *" $n "* ]] || [[ " ${ordered[*]} " == *" $n "* ]] || ordered+=("$n"); done

if [ $mode = back ]; then
	for n in "${ordered[@]}"; do
		exe=$(field "$n" 4) r=$(root "$n")
		echo "== $n: back to the binary from before the last deploy"
		$dry && { echo "  (dry run)"; continue; }
		on "$n" "set -e; [ -f $exe.prev ] || { echo 'no $exe.prev there'; exit 1; }; ${r}cp -p $exe $exe.undone; ${r}cp -p $exe.prev $exe; $(restart "$n") sleep 3; $(running "$n") $exe version"
	done
	exit 0
fi

# Build each architecture once.
hash=$(git rev-parse --short HEAD)
if [ -n "$(git status --porcelain -- cmd internal go.mod go.sum)" ]; then
	$dirty || die "There are changes not committed (git status). Commit them, or add --dirty."
	hash+=-dirty
fi
version=dev-$hash
mkdir -p bin/deploy
declare -A built=()
for n in "${ordered[@]}"; do
	arch=$(field "$n" 3)
	[ -n "${built[$arch]:-}" ] && continue
	out=bin/deploy/yggstore-$version-$arch
	case $arch in
	amd64) env=(GOARCH=amd64) ;;
	arm64) env=(GOARCH=arm64) ;;
	armv7) env=(GOARCH=arm GOARM=7) ;;
	esac
	echo "== building $version for $arch"
	env CGO_ENABLED=0 GOOS=linux "${env[@]}" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out" ./cmd/yggstore
	built[$arch]=$out
done
# The Windows installer the boxes' hubs hand out (Connect to BOX.exe).
join=bin/deploy/yggstore-join-$version.exe
echo "== building the Windows installer"
env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$join" ./cmd/yggstore-join

for n in "${ordered[@]}"; do
	exe=$(field "$n" 4) host=$(field "$n" 2) r=$(root "$n") bin=${built[$(field "$n" 3)]}
	echo "== $n ($host): $version"
	if $dry; then
		echo "  would copy $bin to $exe (keeping the old one as $exe.prev), the Windows installer beside it, and restart: $(rest "$n")"
		continue
	fi
	tmp=/tmp/yggstore-$version
	if [ "$host" = local ]; then cp "$bin" "$tmp"; cp "$join" "$tmp.exe"; else scp -q "$bin" "$host:$tmp"; scp -q "$join" "$host:$tmp.exe"; fi
	# install keeps the binary's owner, and replaces it even while it runs.
	# The installer goes beside the binary, where the hub looks for it.
	on "$n" "set -e; ${r}cp -p $exe $exe.prev; ${r}install -o \$(stat -c %u $exe) -g \$(stat -c %g $exe) -m 644 $tmp.exe \$(dirname $exe)/yggstore-join.exe; ${r}install -o \$(stat -c %u $exe) -g \$(stat -c %g $exe) -m 755 $tmp $exe; rm -f $tmp $tmp.exe; $(restart "$n") sleep 3; $(running "$n") $exe version" ||
		die "$n didn't come back up: scripts/deploy.sh --back $n puts its old binary back. Stopping here."
done
echo "Done. scripts/deploy.sh --status shows what each machine runs."
