#!/usr/bin/env bash
# Publish get.withcare.co.za: the install script (get/install.sh), its page
# and yggstore for Linux and macOS, built from this commit. deploy.sh runs
# it after deploying the desktop; it publishes from this machine's node.
set -euo pipefail
cd "$(dirname "$0")/.."
site=${GET_SITE:-get.withcare.co.za}
version=dev-$(git rev-parse --short HEAD)
[ -z "$(git status --porcelain -- cmd internal go.mod go.sum)" ] || version+=-dirty
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${t%/*} arch=${t#*/}
	name=$os
	[ "$os" = darwin ] && name=macos
	env CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" \
		-o "$out/yggstore-$name-$arch" ./cmd/yggstore
done
cp -r get/install.sh get/index.html get/apply "$out/"
(cd "$out" && sha256sum yggstore-* > SHA256SUMS)
echo "== publishing $site ($version)"
bin/yggstore site publish "$out" -name "$site" -no-wait
