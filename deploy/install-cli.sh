#!/bin/sh
# Installs the fleet command for agents and people, on Linux or macOS.
#
#   curl -fsSL RELEASE_URL/install-cli.sh | sh
#
# It installs to ~/.local/bin, or to FLEET_BIN_DIR if that's set. No root needed.
#
# Everything is inside main, so a download cut off partway can't run half a script.

# build-release.sh fills this in.
RELEASE_URL="${FLEET_RELEASE_URL:-@@RELEASE_URL@@}"

die() {
	echo "install-cli: $*" >&2
	exit 1
}

main() {
	set -eu

	case "$RELEASE_URL" in
	https://*) ;;
	*) die "use the copy of this script attached to a release" ;;
	esac
	case "$(uname -s)" in
	Linux) OS=linux ;;
	Darwin)
		OS=darwin
		# The program is built with a Go version that needs this, and would crash.
		VERSION="$(sw_vers -productVersion)"
		case "${VERSION%%.*}" in
		'' | *[!0-9]*) die "couldn't read this Mac's macOS version, got '$VERSION'" ;;
		esac
		[ "${VERSION%%.*}" -ge 13 ] || die "fleet needs macOS 13 or newer, and this Mac has macOS $VERSION"
		;;
	*) die "unsupported system: $(uname -s)" ;;
	esac
	case "$(uname -m)" in
	x86_64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "unsupported processor: $(uname -m)" ;;
	esac
	NAME="fleet-$OS-$ARCH"
	DIR="${FLEET_BIN_DIR:-$HOME/.local/bin}"

	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	curl -fsSL --retry 3 -o "$TMP/$NAME" "$RELEASE_URL/$NAME" ||
		die "couldn't download $RELEASE_URL/$NAME. If the repo is private, download it while logged in instead"
	curl -fsSL --retry 3 -o "$TMP/SHA256SUMS" "$RELEASE_URL/SHA256SUMS" ||
		die "couldn't download the checksums"
	if command -v sha256sum >/dev/null; then
		VERIFY="sha256sum -c -"
	else
		VERIFY="shasum -a 256 -c -"
	fi
	(cd "$TMP" && grep " $NAME\$" SHA256SUMS | $VERIFY >/dev/null 2>&1) ||
		die "the download doesn't match its checksum"

	mkdir -p "$DIR"
	chmod 755 "$TMP/$NAME"
	mv -f "$TMP/$NAME" "$DIR/fleet"
	echo "Installed $("$DIR/fleet" version) to $DIR/fleet"
	case ":$PATH:" in
	*":$DIR:"*) ;;
	*) echo "Add it to your PATH: export PATH=\"$DIR:\$PATH\"" ;;
	esac
	echo "Then set FLEET_URL and FLEET_API_KEY, and try: fleet capabilities"
}

main "$@"
