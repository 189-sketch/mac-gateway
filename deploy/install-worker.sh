#!/bin/sh
# Installs or upgrades the worker on a Mac. The gateway publishes this script, and
# add-worker.sh prints the command that runs it:
#
#   curl -fsSL https://GATEWAY/install/worker.sh | FLEET_NODE_NAME=NAME FLEET_NODE_TOKEN=TOKEN sh
#
# It installs into ~/fleet and starts the worker as a LaunchAgent for this user.
# Re-running it upgrades the binary and keeps your worker.yaml and token.
#
# Everything is inside main, so a download cut off partway can't run half a script.

GATEWAY_URL="${FLEET_GATEWAY_URL:-@@GATEWAY_URL@@}"

die() {
	echo "install-worker: $*" >&2
	exit 1
}

write_manifest() {
	cat >"$FLEET_HOME/worker.yaml" <<EOF
# This file alone decides what this machine will run. See the README for how to
# add services. Restart after changing it:
#   launchctl kickstart -k gui/$(id -u)/com.fleet.worker
#
# Logs are in worker.log. For more detail, add this line and restart:
#   log_level: debug
gateway_url: $GATEWAY_URL
token_file: token
state_dir: state

node:
  name: $NODE_NAME
  max_jobs: 2
  labels:
    os: macos
  resources:
    cpu_heavy: 1

capabilities:
  - name: tts.say
    adapter: cli
    command: [/usr/bin/say, -o, "{{workdir}}/out.aiff"]
    stdin: "{{input.text}}"
    input_schema:
      type: object
      required: [text]
      additionalProperties: false
      properties:
        text: { type: string, minLength: 1, maxLength: 20000 }
    outputs:
      audio: out.aiff
    requires:
      cpu_heavy: 1
    timeout: 10m
    retry_if_lost: true
    test:
      input: { text: "Fleet test" }
EOF
}

write_plist() {
	cat >"$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.fleet.worker</string>
  <key>ProgramArguments</key>
  <array>
    <string>$FLEET_HOME/fleet-worker</string>
    <string>--config</string>
    <string>$FLEET_HOME/worker.yaml</string>
    <string>--log-file</string>
    <string>$FLEET_HOME/worker.log</string>
  </array>
  <key>WorkingDirectory</key>
  <string>$FLEET_HOME</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>$FLEET_HOME/worker.log.crash</string>
  <key>StandardErrorPath</key>
  <string>$FLEET_HOME/worker.log.crash</string>
</dict>
</plist>
EOF
}

# The worker is built with a Go version that needs this. An older Mac would crash
# when it starts, which would otherwise be reported as a bad manifest.
MIN_MACOS=13

check_macos_version() {
	version="$(sw_vers -productVersion)"
	major="${version%%.*}"
	case "$major" in
	'' | *[!0-9]*) die "couldn't read this Mac's macOS version, got '$version'" ;;
	esac
	[ "$major" -ge "$MIN_MACOS" ] ||
		die "the worker needs macOS $MIN_MACOS or newer, and this Mac has macOS $version. Nothing has been changed"
}

main() {
	set -eu

	[ "$(uname -s)" = "Darwin" ] || die "this installer is for macOS"
	check_macos_version
	[ "$(id -u)" -ne 0 ] || die "run this as the user the worker should run as, not as root"
	case "$GATEWAY_URL" in
	https://*) ;;
	*) die "the gateway address must start with https://, got '$GATEWAY_URL'" ;;
	esac

	case "$(uname -m)" in
	arm64) ARCH=arm64 ;;
	x86_64) ARCH=amd64 ;;
	*) die "unsupported chip: $(uname -m)" ;;
	esac

	FLEET_HOME="$HOME/fleet"
	PLIST="$HOME/Library/LaunchAgents/com.fleet.worker.plist"
	NODE_NAME="${FLEET_NODE_NAME:-}"
	NODE_TOKEN="${FLEET_NODE_TOKEN:-}"
	BINARY="fleet-worker-darwin-$ARCH"

	mkdir -p "$FLEET_HOME" "$HOME/Library/LaunchAgents"
	if [ ! -f "$FLEET_HOME/token" ] && [ -z "$NODE_TOKEN" ]; then
		die "FLEET_NODE_TOKEN isn't set; on the gateway, run: fleet-add-worker NAME"
	fi
	if [ ! -f "$FLEET_HOME/worker.yaml" ]; then
		[ -n "$NODE_NAME" ] || die "FLEET_NODE_NAME isn't set"
		case "$NODE_NAME" in
		*[!A-Za-z0-9._-]* | [!A-Za-z0-9]*) die "'$NODE_NAME' isn't a valid node name" ;;
		esac
	fi

	if id -Gn | tr ' ' '\n' | grep -qx admin; then
		echo "Note: this is an admin account. Jobs run as this user and can read its files,"
		echo "so a dedicated standard account with no personal files is safer."
	fi

	echo "==> Downloading the worker for $ARCH"
	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	curl -fsSL --retry 3 -o "$TMP/$BINARY" "$GATEWAY_URL/install/$BINARY"
	curl -fsSL --retry 3 -o "$TMP/SHA256SUMS" "$GATEWAY_URL/install/SHA256SUMS"
	(cd "$TMP" && grep " $BINARY\$" SHA256SUMS | shasum -a 256 -c - >/dev/null) ||
		die "the download doesn't match its checksum"
	chmod 755 "$TMP/$BINARY"

	if [ -n "$NODE_TOKEN" ]; then
		(umask 077 && printf '%s\n' "$NODE_TOKEN" >"$FLEET_HOME/token")
	fi
	chmod 600 "$FLEET_HOME/token"
	if [ -f "$FLEET_HOME/worker.yaml" ]; then
		echo "==> Keeping your existing worker.yaml"
	else
		write_manifest
	fi

	# Check before replacing anything that's running, so a bad config leaves the
	# current worker alone.
	"$TMP/$BINARY" --config "$FLEET_HOME/worker.yaml" --check ||
		die "$FLEET_HOME/worker.yaml isn't valid"

	echo "==> Starting the worker"
	DOMAIN="gui/$(id -u)"
	launchctl bootout "$DOMAIN/com.fleet.worker" 2>/dev/null || true
	mv -f "$TMP/$BINARY" "$FLEET_HOME/fleet-worker"
	write_plist
	: >"$FLEET_HOME/worker.log"
	launchctl bootstrap "$DOMAIN" "$PLIST" ||
		die "launchd refused to start it. If you're connected over SSH, log in to this user at the Mac's screen (or via Screen Sharing) and run this again"

	connected=""
	i=0
	while [ "$i" -lt 20 ]; do
		if grep -q "connected to gateway" "$FLEET_HOME/worker.log" 2>/dev/null; then
			connected=yes
			break
		fi
		i=$((i + 1))
		sleep 1
	done
	if [ -z "$connected" ]; then
		echo "install-worker: the worker started but hasn't connected. Its log says:" >&2
		tail -n 15 "$FLEET_HOME/worker.log" >&2 || true
		exit 1
	fi

	cat <<EOF

==> Connected to $GATEWAY_URL
Installed in $FLEET_HOME. To watch what it's doing:

  tail -F $FLEET_HOME/worker.log

To keep this Mac available, run these yourself. The first stops it sleeping and
restarts it after a power cut. The second part is in System Settings: turn on
automatic login for this user, so the worker starts again after a reboot.

  sudo pmset -a sleep 0 disablesleep 1 autorestart 1
EOF
}

main "$@"
