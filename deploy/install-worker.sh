#!/bin/sh
# Installs or upgrades the worker on a Mac, or on Linux with systemd.
#
#   From the gateway, which knows its own address:
#     curl -fsSL https://GATEWAY/install/worker.sh | sh
#
#   From a release, which asks for the gateway's address:
#     curl -fsSL RELEASE_URL/install-worker.sh | sh
#
# It installs the worker, then pairs it: it asks the gateway to pair, the server
# shows a code, and you type the code here. Pressing Enter at either question
# skips it, and the worker's page, which opens at the end, can pair it later.
#
# To pair with no questions, give a node token made on the server with
# fleet-add-worker:
#     curl -fsSL https://GATEWAY/install/worker.sh | FLEET_NODE_TOKEN=TOKEN sh
#
# It installs into ~/fleet for the account that runs it, and jobs run as that
# account, so they can use the tools and models installed there.
#
#   - On a Mac, the worker is a LaunchAgent for this user. Run as root, it's a
#     LaunchDaemon that runs as root.
#   - On Linux, it's a systemd service, fleet-worker, that runs as this user from
#     boot without anyone logging in. Installing it needs sudo. As root, it runs
#     as root, or as the account FLEET_WORKER_USER names.
#
# Re-running it upgrades the binary and keeps your worker.yaml and token.
#
# Everything is inside main, so a download cut off partway can't run half a script.

# The gateway fills in the first of these when it publishes this script, and
# build-release.sh the second. Either stays a placeholder otherwise.
GATEWAY_URL="${FLEET_GATEWAY_URL:-@@GATEWAY_URL@@}"
RELEASE_URL="${FLEET_RELEASE_URL:-@@RELEASE_URL@@}"

# Only the install script's tests set these: to install into a scratch directory,
# and to answer the questions without a terminal.
ROOT="${FLEET_INSTALL_ROOT:-}"
TTY="${FLEET_INSTALL_TTY:-/dev/tty}"

die() {
	echo "install-worker: $*" >&2
	exit 1
}

# --- Shared by both systems ---

# interactive says whether there's a person to ask. Piped from curl, stdin is the
# script itself, so questions are read from the terminal instead.
interactive() {
	[ -z "${FLEET_NONINTERACTIVE:-}" ] || return 1
	[ -z "${FLEET_INSTALL_TTY:-}" ] || return 0
	[ -t 1 ] && (: <"$TTY") 2>/dev/null
}

# clean_address turns what a person typed into the gateway's address.
clean_address() {
	address="$(printf '%s' "$1" | tr -d '[:space:]')"
	address="${address%/}"
	case "$address" in
	'' | *://*) ;;
	*) address="https://$address" ;;
	esac
	printf '%s' "$address"
}

valid_address() {
	case "$1" in
	*[!A-Za-z0-9.:/_-]*) return 1 ;;
	https://?*) return 0 ;;
	# Plain http is safe only to this machine itself.
	http://127.0.0.1:* | http://localhost:*) return 0 ;;
	esac
	return 1
}

# ask_gateway asks where the gateway is, until the answer is one that responds.
# An empty answer leaves GATEWAY_URL empty, to pair later.
ask_gateway() {
	tries=0
	while [ "$tries" -lt 5 ]; do
		tries=$((tries + 1))
		printf "Gateway address (the server's IP address or domain; press Enter to skip): "
		answer=""
		read -r answer || answer=""
		answer="$(clean_address "$answer")"
		if [ -z "$answer" ]; then
			return 0
		fi
		if ! valid_address "$answer"; then
			echo "That doesn't look like an address. Give the server's IP address, such as 203.0.113.10, or its domain."
			continue
		fi
		if curl -fsS --max-time 15 -o /dev/null "$answer/healthz" 2>/dev/null; then
			GATEWAY_URL="$answer"
			return 0
		fi
		echo "No gateway answered at $answer. Check the address, and that the gateway's install finished. A new server's certificate can take a minute."
	done
	echo "Carrying on without a gateway. You can pair this machine later from its page."
} <"$TTY"

# choose_gateway settles GATEWAY_URL: the one this script was published with, the
# one already in worker.yaml, or the one the person gives.
choose_gateway() {
	# An address has no @ in it, and a placeholder nobody filled in does.
	case "$GATEWAY_URL" in *@*) GATEWAY_URL="" ;; esac
	case "$RELEASE_URL" in https://*) ;; *) RELEASE_URL="" ;; esac
	GATEWAY_URL="$(clean_address "$GATEWAY_URL")"
	if [ -n "$GATEWAY_URL" ]; then
		valid_address "$GATEWAY_URL" || die "the gateway address must start with https://, got '$GATEWAY_URL'"
		return 0
	fi
	if [ -f "$FLEET_HOME/worker.yaml" ]; then
		GATEWAY_URL="$(sed -n 's/^gateway_url:[[:space:]]*//p' "$FLEET_HOME/worker.yaml" | head -n 1 | sed 's/[[:space:]]*#.*//' | tr -d "\"'")"
		return 0
	fi
	if interactive; then
		ask_gateway
	fi
}

has_token() {
	[ -n "$NODE_TOKEN" ] || [ -s "$FLEET_HOME/token" ]
}

# check_inputs checks what was given, and that there's somewhere to download the
# worker from, before anything is changed.
check_inputs() {
	if [ -n "$NODE_NAME" ]; then
		case "$NODE_NAME" in
		*[!A-Za-z0-9._-]* | [!A-Za-z0-9]*) die "'$NODE_NAME' isn't a valid node name" ;;
		esac
	fi
	case "$NODE_TOKEN" in
	*[!A-Za-z0-9_]*) die "FLEET_NODE_TOKEN isn't a node token; on the gateway, run: fleet-add-worker NAME" ;;
	esac
	if [ -n "$GATEWAY_URL" ]; then
		DOWNLOAD_URL="$GATEWAY_URL/install"
	elif [ -n "$RELEASE_URL" ]; then
		DOWNLOAD_URL="$RELEASE_URL"
	else
		die "there's nowhere to download the worker from. Give the gateway's address by putting FLEET_GATEWAY_URL=ADDRESS before sh"
	fi
}

# default_name is what this machine is called until pairing names it.
default_name() {
	name="$(hostname -s 2>/dev/null || uname -n)"
	name="$(printf '%s' "${name%%.*}" | tr -cd 'A-Za-z0-9_-' | sed 's/^[-_]*//')"
	printf '%s' "${name:-worker}"
}

# download downloads the worker into $TMP and checks it. $1 is the command that
# checks a SHA-256 list, which differs between macOS and Linux.
download() {
	echo "==> Downloading the worker for $ARCH"
	as_user curl -fsSL --retry 3 -o "$TMP/$BINARY" "$DOWNLOAD_URL/$BINARY" ||
		die "couldn't download the worker from $DOWNLOAD_URL"
	as_user curl -fsSL --retry 3 -o "$TMP/SHA256SUMS" "$DOWNLOAD_URL/SHA256SUMS" ||
		die "couldn't download the checksums from $DOWNLOAD_URL"
	(cd "$TMP" && grep " $BINARY\$" SHA256SUMS | $1 -c - >/dev/null) ||
		die "the download doesn't match its checksum"
	chmod 755 "$TMP/$BINARY"
}

# save_settings writes the token if one was given, and worker.yaml if there isn't
# one. $1 is the manifest to write.
save_settings() {
	if [ -n "$NODE_TOKEN" ]; then
		printf '%s\n' "$NODE_TOKEN" | as_user sh -c 'umask 077 && cat >"$1"' sh "$FLEET_HOME/token"
	fi
	if [ -f "$FLEET_HOME/token" ]; then
		as_user chmod 600 "$FLEET_HOME/token"
	fi
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

# pair connects this machine to the gateway, unless it already has a token.
# Whatever happens, the install carries on: a machine that isn't paired can be
# paired from its page.
pair() {
	PAIRED=""
	if [ -z "$GATEWAY_URL" ]; then
		return 0
	fi
	if [ -n "$NODE_TOKEN" ] && [ -z "$NODE_NAME" ]; then
		# A token alone is enough: the gateway knows which machine it was made for.
		echo "==> Connecting to $GATEWAY_URL with the token"
		if as_user "$TMP/$BINARY" pair --config "$FLEET_HOME/worker.yaml" --gateway "$GATEWAY_URL" --key "$NODE_TOKEN"; then
			PAIRED=yes
		else
			echo "install-worker: the token didn't work. You can pair this machine from its page instead." >&2
			as_user rm -f "$FLEET_HOME/token"
		fi
		return 0
	fi
	if has_token; then
		PAIRED=yes
		return 0
	fi
	if ! interactive; then
		return 0
	fi
	echo "==> Pairing with $GATEWAY_URL"
	status=0
	as_user "$TMP/$BINARY" pair --config "$FLEET_HOME/worker.yaml" --gateway "$GATEWAY_URL" ${NODE_NAME:+--name "$NODE_NAME"} <"$TTY" || status=$?
	case "$status" in
	0) PAIRED=yes ;;
	3) echo "Skipped. You can pair this machine later from its page." ;;
	*) echo "install-worker: pairing didn't work. The install carries on, and you can pair this machine from its page." >&2 ;;
	esac
}

# wait_for_start waits for the worker to say it has connected or, for a machine
# that isn't paired, that it's up. $1 says where else to look if it doesn't.
wait_for_start() {
	waiting_for="connected to gateway"
	[ -n "$PAIRED" ] || waiting_for="not paired with a gateway"
	i=0
	while [ "$i" -lt 20 ]; do
		if grep -q "$waiting_for" "$FLEET_HOME/worker.log" 2>/dev/null; then
			return 0
		fi
		i=$((i + 1))
		sleep 1
	done
	if [ -n "$PAIRED" ]; then
		echo "install-worker: the worker started but hasn't connected. Its log says:" >&2
	else
		echo "install-worker: the worker didn't start. Its log says:" >&2
	fi
	tail -n 15 "$FLEET_HOME/worker.log" >&2 || true
	[ -z "$1" ] || echo "$1" >&2
	exit 1
}

# open_page opens the worker's page, signed in. Where there's no browser, the
# worker says how to reach the page from another computer instead.
open_page() {
	echo
	as_user "$FLEET_HOME/fleet-worker" ui --config "$FLEET_HOME/worker.yaml" --wait 15s ${FLEET_NO_OPEN:+--no-open} ||
		echo "The worker's page didn't open. Its log, $FLEET_HOME/worker.log, says why."
}

# say_result prints how the install ended. $1 says who the worker runs as, if
# that needs saying.
say_result() {
	echo
	if [ -n "$PAIRED" ]; then
		echo "==> Connected to $GATEWAY_URL"
		echo "Installed in $FLEET_HOME$1. The Connection tab of the worker's page has a"
		echo "test that checks agents can reach this machine's tools through MCP."
	else
		echo "==> Installed, but not paired with a gateway yet"
		echo "Installed in $FLEET_HOME$1. To pair it, give the gateway's address on the"
		echo "Connection tab of the worker's page, or run:"
		echo
		echo "  $FLEET_HOME/fleet-worker pair --config $FLEET_HOME/worker.yaml"
	fi
	cat <<EOF

To open the worker's page again:

  $FLEET_HOME/fleet-worker ui --config $FLEET_HOME/worker.yaml

To watch what the worker is doing:

  tail -F $FLEET_HOME/worker.log
EOF
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
#
# Pairing this machine fills in gateway_url and node.name.
gateway_url: "$GATEWAY_URL"
token_file: token
state_dir: state

node:
  name: ${NODE_NAME:-$(default_name)}
  max_jobs: 2
  labels:
    os: $2
EOF
}

# --- macOS ---

macos_manifest() {
	manifest_header "$MAC_RESTART" macos
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
    <key>HOME</key>
    <string>$MAC_HOME</string>
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

	case "$(uname -m)" in
	arm64) ARCH=arm64 ;;
	x86_64) ARCH=amd64 ;;
	*) die "unsupported chip: $(uname -m)" ;;
	esac

	if [ "$(id -u)" -eq 0 ]; then
		# As root there's no login session to belong to, so the worker is a system
		# service. sudo keeps the caller's HOME, so root's is named outright.
		MAC_HOME="$ROOT/var/root"
		PLIST="$ROOT/Library/LaunchDaemons/com.fleet.worker.plist"
		DOMAIN=system
		MAC_RESTART="sudo launchctl kickstart -k system/com.fleet.worker"
		WHO=", running as root"
	else
		MAC_HOME="$HOME"
		PLIST="$HOME/Library/LaunchAgents/com.fleet.worker.plist"
		DOMAIN="gui/$(id -u)"
		MAC_RESTART="launchctl kickstart -k $DOMAIN/com.fleet.worker"
		WHO=""
	fi
	FLEET_HOME="$MAC_HOME/fleet"
	BINARY="fleet-worker-darwin-$ARCH"

	choose_gateway
	check_inputs
	mkdir -p "$FLEET_HOME" "$(dirname "$PLIST")"

	if [ "$(id -u)" -eq 0 ]; then
		echo "Note: installing as root, so jobs run as root and can read and change"
		echo "anything on this Mac. Offer only tools you'd trust with that."
	elif id -Gn | tr ' ' '\n' | grep -qx admin; then
		echo "Note: this is an admin account. Jobs run as this user and can read its files,"
		echo "so a dedicated standard account with no personal files is safer."
	fi

	TMP="$(mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	download "shasum -a 256"
	save_settings "$(macos_manifest)"
	pair

	echo "==> Starting the worker"
	launchctl bootout "$DOMAIN/com.fleet.worker" 2>/dev/null || true
	mv -f "$TMP/$BINARY" "$FLEET_HOME/fleet-worker"
	write_plist
	: >"$FLEET_HOME/worker.log"
	launchctl bootstrap "$DOMAIN" "$PLIST" ||
		die "launchd refused to start it. If you're connected over SSH, log in to this user at the Mac's screen (or via Screen Sharing) and run this again"
	wait_for_start ""
	open_page
	say_result "$WHO"

	cat <<EOF

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
		# With no account named, the worker runs as root, like this script.
		[ -n "$WORKER_USER" ] || WORKER_USER=root
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
	if [ -n "$RUN_AS_USER" ]; then
		USER_HOME="$(getent passwd "$WORKER_USER" | cut -d: -f6)"
		[ -n "$USER_HOME" ] || die "couldn't find the home folder of '$WORKER_USER'"
		# Root needs no help to act as itself.
		[ "$uid" != 0 ] || RUN_AS_USER=""
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

	if [ -n "$AS_ROOT" ]; then
		echo "==> Installing the service needs sudo"
		sudo true || die "couldn't use sudo. Run this as root instead, with FLEET_WORKER_USER=$WORKER_USER before sh to keep jobs running as this account"
	fi
	choose_gateway
	check_inputs
	if [ "$uid" = 0 ]; then
		echo "Note: the worker will run as root, so jobs can read and change anything on"
		echo "this machine. Offer only tools you'd trust with that. To use another account,"
		echo "put FLEET_WORKER_USER=ACCOUNT before sh."
	elif id -Gn "$WORKER_USER" | tr ' ' '\n' | grep -qxE 'sudo|wheel|admin'; then
		echo "Note: '$WORKER_USER' can use sudo. Jobs run as this account and can read its files,"
		echo "so a dedicated account with no sudo and no personal files is safer."
	fi

	TMP="$(as_user mktemp -d)"
	trap 'rm -rf "$TMP"' EXIT
	as_user mkdir -p "$FLEET_HOME"
	download sha256sum
	save_settings "$(linux_manifest)"
	pair

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
	wait_for_start "For more, see: sudo journalctl -u fleet-worker -n 50"
	open_page
	say_result ", running as $WORKER_USER. It starts at boot"

	cat <<EOF

On a machine with no screen, reach the page from your own computer:

  ssh -L 7878:127.0.0.1:7878 $WORKER_USER@THIS_MACHINE $FLEET_HOME/fleet-worker ui --config $FLEET_HOME/worker.yaml --no-open
EOF
}

main() {
	set -eu

	NODE_NAME="${FLEET_NODE_NAME:-}"
	NODE_TOKEN="${FLEET_NODE_TOKEN:-}"
	RUN_AS_USER=""
	PAIRED=""

	command -v curl >/dev/null || die "curl isn't installed"
	case "$(uname -s)" in
	Darwin) install_macos ;;
	Linux) install_linux ;;
	*) die "this installer is for macOS and Linux. On Windows, use the PowerShell command the gateway's installer prints" ;;
	esac
}

main "$@"
