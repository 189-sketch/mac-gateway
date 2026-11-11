#!/bin/sh
# Installs or upgrades the worker on a Mac, or on Linux with systemd. The gateway
# publishes this script, and add-worker.sh prints the command that runs it:
#
#   curl -fsSL https://GATEWAY/install/worker.sh | FLEET_NODE_NAME=NAME FLEET_NODE_TOKEN=TOKEN sh
#
# Both install into ~/fleet for the account that runs it, and jobs run as that
# account, so they can use the tools and models installed there.
#
#   - On a Mac, the worker is a LaunchAgent for this user.
#   - On Linux, it's a systemd service, fleet-worker, that runs as this user from
#     boot without anyone logging in. Installing it needs sudo. As root, set
#     FLEET_WORKER_USER to the account the worker should run as.
#
# Re-running it upgrades the binary and keeps your worker.yaml and token.
#
# Everything is inside main, so a download cut off partway can't run half a script.

GATEWAY_URL="${FLEET_GATEWAY_URL:-@@GATEWAY_URL@@}"

# Only the install script's tests set this, to install into a scratch directory.
ROOT="${FLEET_INSTALL_ROOT:-}"

die() {
	echo "install-worker: $*" >&2
	exit 1
}

# --- Shared by both systems ---

# check_inputs checks the name and token before anything is changed.
check_inputs() {
	if [ ! -f "$FLEET_HOME/token" ] && [ -z "$NODE_TOKEN" ]; then
		die "FLEET_NODE_TOKEN isn't set; on the gateway, run: fleet-add-worker NAME"
	fi
	if [ ! -f "$FLEET_HOME/worker.yaml" ]; then
		[ -n "$NODE_NAME" ] || die "FLEET_NODE_NAME isn't set"
		case "$NODE_NAME" in
		*[!A-Za-z0-9._-]* | [!A-Za-z0-9]*) die "'$NODE_NAME' isn't a valid node name" ;;
		esac
	fi
}

# download downloads the worker into $TMP and checks it. $1 is the command that
# checks a SHA-256 list, which differs between macOS and Linux.
download() {
	echo "==> Downloading the worker for $ARCH"
	as_user curl -fsSL --retry 3 -o "$TMP/$BINARY" "$GATEWAY_URL/install/$BINARY"
	as_user curl -fsSL --retry 3 -o "$TMP/SHA256SUMS" "$GATEWAY_URL/install/SHA256SUMS"
	(cd "$TMP" && grep " $BINARY\$" SHA256SUMS | $1 -c - >/dev/null) ||
		die "the download doesn't match its checksum"
	chmod 755 "$TMP/$BINARY"
}

# save_settings writes the token, and worker.yaml if there isn't one. $1 is the
# manifest to write.
save_settings() {
	if [ -n "$NODE_TOKEN" ]; then
		printf '%s\n' "$NODE_TOKEN" | as_user sh -c 'umask 077 && cat >"$1"' sh "$FLEET_HOME/token"
	fi
	as_user chmod 600 "$FLEET_HOME/token"
	if [ -f "$FLEET_HOME/worker.yaml" ]; then
		echo "==> Keeping your existing worker.yaml"
	else
		printf '%s\n' "$1" | as_user sh -c 'cat >"$1"' sh "$FLEET_HOME/worker.yaml"
	fi

	# Check before replacing anything that's running, so a bad config leaves the
	# current worker alone.
	as_user "$TMP/$BINARY" --config "$FLEET_HOME/worker.yaml" --check ||
		die "$FLEET_HOME/worker.yaml isn't valid"
}

# wait_for_connection waits for the worker to say it has connected. $1 says where
# else to look if it doesn't.
wait_for_connection() {
	i=0
	while [ "$i" -lt 20 ]; do
		if grep -q "connected to gateway" "$FLEET_HOME/worker.log" 2>/dev/null; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	echo "install-worker: the worker started but hasn't connected. Its log says:" >&2
	tail -n 15 "$FLEET_HOME/worker.log" >&2 || true
	[ -z "$1" ] || echo "$1" >&2
	exit 1
}

# The header and settings both systems share. $1 is how to restart the worker,
# $2 is the os label.
manifest_header() {
	cat <<EOF
# This file alone decides what this machine will run. See the README for how to
# add services. Restart after changing it:
#   $1
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
    os: $2
EOF
}

# --- macOS ---

macos_manifest() {
	manifest_header "launchctl kickstart -k gui/$(id -u)/com.fleet.worker" macos
	cat <<'EOF'
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

install_macos() {
	check_macos_version
	[ "$(id -u)" -ne 0 ] || die "run this as the user the worker should run as, not as root"

	case "$(uname -m)" in
	arm64) ARCH=arm64 ;;
	x86_64) ARCH=amd64 ;;
	*) die "unsupported chip: $(uname -m)" ;;
	esac

	FLEET_HOME="$HOME/fleet"
	PLIST="$HOME/Library/LaunchAgents/com.fleet.worker.plist"
	BINARY="fleet-worker-darwin-$ARCH"

	mkdir -p "$FLEET_HOME" "$HOME/Library/LaunchAgents"
	check_inputs

	if id -Gn | tr ' ' '\n' | grep -qx admin; then
		echo "Note: this is an admin account. Jobs run as this user and can read its files,"
		echo "so a dedicated standard account with no personal files is safer."
	fi

	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	download "shasum -a 256"
	save_settings "$(macos_manifest)"

	echo "==> Starting the worker"
	DOMAIN="gui/$(id -u)"
	launchctl bootout "$DOMAIN/com.fleet.worker" 2>/dev/null || true
	mv -f "$TMP/$BINARY" "$FLEET_HOME/fleet-worker"
	write_plist
	: >"$FLEET_HOME/worker.log"
	launchctl bootstrap "$DOMAIN" "$PLIST" ||
		die "launchd refused to start it. If you're connected over SSH, log in to this user at the Mac's screen (or via Screen Sharing) and run this again"
	wait_for_connection ""

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

# --- Linux ---

linux_manifest() {
	manifest_header "sudo systemctl restart fleet-worker" linux
	cat <<'EOF'

capabilities:
  - name: system.hostname
    adapter: cli
    command: [/bin/uname, -n]
    input_schema:
      type: object
      additionalProperties: false
    timeout: 1m
    retry_if_lost: true
    test: { input: {} }
EOF
}

# The binary is outside the home folder because SELinux, on by default on RHEL
# and CentOS, doesn't let a service run a program stored in one.
LINUX_BINARY_PATH=/usr/local/bin/fleet-worker
LINUX_UNIT=/etc/systemd/system/fleet-worker.service

linux_unit() {
	cat <<EOF
# Written by the worker installer, which rewrites it on every upgrade.
[Unit]
Description=Fleet worker
After=network-online.target
Wants=network-online.target

[Service]
User=$WORKER_USER
WorkingDirectory=$FLEET_HOME
ExecStart=$ROOT$LINUX_BINARY_PATH --config $FLEET_HOME/worker.yaml --log-file $FLEET_HOME/worker.log
ExecReload=/bin/kill -HUP \$MAINPID
Environment=PATH=$USER_HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Restart=always
RestartSec=10
# The worker stops its jobs itself when asked to stop. Anything left after that
# is killed.
KillMode=mixed
TimeoutStopSec=30
# Jobs can't gain privileges, such as through sudo.
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
}

selinux_active() {
	command -v getenforce >/dev/null && [ "$(getenforce 2>/dev/null)" != Disabled ]
}

# choose_linux_user decides which account the worker runs as, and how to run
# commands as it and as root.
choose_linux_user() {
	if [ "$(id -u)" -eq 0 ]; then
		WORKER_USER="${FLEET_WORKER_USER:-}"
		# An upgrade can keep the account the service already uses.
		if [ -z "$WORKER_USER" ] && [ -f "$ROOT$LINUX_UNIT" ]; then
			WORKER_USER="$(sed -n 's/^User=//p' "$ROOT$LINUX_UNIT" | head -n 1)"
		fi
		[ -n "$WORKER_USER" ] ||
			die "jobs would run as root. Run this as the account the worker should use, or as root with FLEET_WORKER_USER=ACCOUNT before sh. To make a new account: useradd --create-home fleet-worker"
		AS_ROOT=""
		RUN_AS_USER=yes
	else
		WORKER_USER="$(id -un)"
		if [ -n "${FLEET_WORKER_USER:-}" ] && [ "$FLEET_WORKER_USER" != "$WORKER_USER" ]; then
			die "to install for '$FLEET_WORKER_USER', run this as root, or as that account"
		fi
		AS_ROOT=sudo
		RUN_AS_USER=""
		USER_HOME="$HOME"
	fi
	# The name goes into the service file, so it must be a plain account name.
	case "$WORKER_USER" in
	*[!A-Za-z0-9._@-]* | [!A-Za-z0-9_]*) die "'$WORKER_USER' isn't an account name the installer can use" ;;
	esac
	uid="$(id -u "$WORKER_USER" 2>/dev/null)" || die "there's no account named '$WORKER_USER'"
	[ "$uid" != 0 ] || die "jobs would run as root. Choose another account with FLEET_WORKER_USER"
	if [ -n "$RUN_AS_USER" ]; then
		USER_HOME="$(getent passwd "$WORKER_USER" | cut -d: -f6)"
		[ -n "$USER_HOME" ] || die "couldn't find the home folder of '$WORKER_USER'"
	fi
	# systemd would split a path with spaces or quotes in it.
	case "$USER_HOME" in
	/*[!A-Za-z0-9._/-]* | '' | [!/]*) die "the worker can't be installed in '$USER_HOME'; its home folder's path must be plain letters, digits, dots, dashes and slashes" ;;
	esac
}

as_user() {
	if [ -n "$RUN_AS_USER" ]; then
		runuser -u "$WORKER_USER" -- "$@"
	else
		"$@"
	fi
}

install_linux() {
	[ -d "$ROOT/run/systemd/system" ] ||
		die "this machine doesn't use systemd, which the installer needs to keep the worker running"
	choose_linux_user

	case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "unsupported processor: $(uname -m)" ;;
	esac

	FLEET_HOME="$USER_HOME/fleet"
	BINARY="fleet-worker-linux-$ARCH"
	check_inputs

	if [ -n "$AS_ROOT" ]; then
		echo "==> Installing the service needs sudo"
		sudo true || die "couldn't use sudo. Run this as root with FLEET_WORKER_USER=$WORKER_USER before sh instead"
	fi
	if id -Gn "$WORKER_USER" | tr ' ' '\n' | grep -qxE 'sudo|wheel|admin'; then
		echo "Note: '$WORKER_USER' can use sudo. Jobs run as this account and can read its files,"
		echo "so a dedicated account with no sudo and no personal files is safer."
	fi

	TMP="$(as_user mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	as_user mkdir -p "$FLEET_HOME"
	download sha256sum
	save_settings "$(linux_manifest)"

	echo "==> Starting the worker"
	$AS_ROOT install -d -m 755 "$ROOT/usr/local/bin" "$ROOT/etc/systemd/system"
	# Replace the binary in one step, so the running worker's file is never
	# half-written.
	$AS_ROOT install -m 755 "$TMP/$BINARY" "$ROOT$LINUX_BINARY_PATH.new"
	$AS_ROOT mv -f "$ROOT$LINUX_BINARY_PATH.new" "$ROOT$LINUX_BINARY_PATH"
	linux_unit >"$TMP/fleet-worker.service"
	$AS_ROOT install -m 644 "$TMP/fleet-worker.service" "$ROOT$LINUX_UNIT"
	if selinux_active; then
		$AS_ROOT restorecon "$ROOT$LINUX_BINARY_PATH" "$ROOT$LINUX_UNIT" ||
			die "couldn't set the SELinux label on $LINUX_BINARY_PATH"
	fi
	# The same command as on a Mac then works here.
	as_user ln -sfn "$ROOT$LINUX_BINARY_PATH" "$FLEET_HOME/fleet-worker"
	as_user sh -c ': >"$1"' sh "$FLEET_HOME/worker.log"
	$AS_ROOT systemctl daemon-reload
	$AS_ROOT systemctl enable --quiet fleet-worker
	$AS_ROOT systemctl restart fleet-worker ||
		die "systemd couldn't start it; see: sudo journalctl -u fleet-worker -n 50"
	wait_for_connection "For more, see: sudo journalctl -u fleet-worker -n 50"

	cat <<EOF

==> Connected to $GATEWAY_URL
Installed in $FLEET_HOME, running as $WORKER_USER. It starts at boot. To watch
what it's doing:

  tail -F $FLEET_HOME/worker.log

To open its configuration page from another computer:

  ssh -L 7878:127.0.0.1:7878 $WORKER_USER@THIS_MACHINE $FLEET_HOME/fleet-worker ui --config $FLEET_HOME/worker.yaml --no-open
EOF
}

main() {
	set -eu

	case "$GATEWAY_URL" in
	https://*) ;;
	*) die "the gateway address must start with https://, got '$GATEWAY_URL'" ;;
	esac
	NODE_NAME="${FLEET_NODE_NAME:-}"
	NODE_TOKEN="${FLEET_NODE_TOKEN:-}"
	RUN_AS_USER=""

	case "$(uname -s)" in
	Darwin) install_macos ;;
	Linux) install_linux ;;
	*) die "this installer is for macOS and Linux. On Windows, use the PowerShell command fleet-add-worker prints" ;;
	esac
}

main "$@"
