#!/usr/bin/env bash
# Builds the files for a GitHub release into dist/release, from a clean checkout.
# It doesn't publish anything.
#
#   ./deploy/build-release.sh v0.2.0
#
# Set FLEET_RELEASE_ALLOW_DIRTY=1 to build uncommitted code, for testing only.
set -euo pipefail

die() {
	echo "build-release: $*" >&2
	exit 1
}

VERSION="${1:-}"
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || die "usage: $0 vMAJOR.MINOR.PATCH"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"
if [ -z "${FLEET_RELEASE_ALLOW_DIRTY:-}" ] && [ -n "$(git status --porcelain)" ]; then
	die "commit your changes first, so a release always matches a commit"
fi

RELEASE_REPO="${FLEET_RELEASE_REPO:-189-sketch/mac-gateway}"
RELEASE_URL="https://github.com/$RELEASE_REPO/releases/download/$VERSION"
OUT="$REPO/dist/release"
rm -rf "$OUT"
mkdir -p "$OUT"

build() { # os, arch, package, output
	CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$4" "./cmd/$3"
}

for arch in amd64 arm64; do
	echo "==> Server bundle for linux/$arch"
	name="fleet-server-linux-$arch"
	bundle="$OUT/$name"
	mkdir -p "$bundle"
	build linux "$arch" fleet-gateway "$bundle/fleet-gateway"
	build darwin arm64 fleet-worker "$bundle/fleet-worker-darwin-arm64"
	build darwin amd64 fleet-worker "$bundle/fleet-worker-darwin-amd64"
	sed "s|@@RELEASE_URL@@|$RELEASE_URL|g" deploy/install-gateway.sh >"$bundle/install-gateway.sh"
	chmod 755 "$bundle/install-gateway.sh"
	cp deploy/add-worker.sh deploy/install-worker.sh deploy/fleet-gateway.service "$bundle/"
	tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name"
	rm -rf "$bundle"
done

echo "==> The fleet command"
for target in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do
	os="${target%/*}"
	arch="${target#*/}"
	build "$os" "$arch" fleet "$OUT/fleet-$os-$arch"
done

for script in install-gateway.sh install-cli.sh; do
	sed "s|@@RELEASE_URL@@|$RELEASE_URL|g" "deploy/$script" >"$OUT/$script"
	chmod 755 "$OUT/$script"
done

(cd "$OUT" && sha256sum -- * >SHA256SUMS)
echo "==> Built $VERSION in $OUT"
ls -l "$OUT"
