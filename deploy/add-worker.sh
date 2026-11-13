#!/usr/bin/env bash
# Registers a new machine on an installed gateway and prints its token, with the
# commands that install a worker using it. Run it on the gateway server.
#
#   fleet-add-worker mac-mini-2
#
# A machine doesn't need this to join: its installer can pair with a code that
# fleet-pair shows. A token is for joining with no questions asked, such as from
# a script, or for pasting into the worker's page.
set -euo pipefail

# Only the install script's tests set this, to install into a scratch directory.
ROOT="${FLEET_INSTALL_ROOT:-}"

NAME="${1:-}"
if [ -z "$NAME" ]; then
	echo "usage: $0 NODE_NAME" >&2
	exit 2
fi
if ! [[ "$NAME" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
	echo "add-worker: '$NAME' isn't a valid node name" >&2
	exit 2
fi
if [ ! -f "$ROOT/srv/fleet/gateway-url" ]; then
	echo "add-worker: the gateway isn't installed here; run install-gateway.sh first" >&2
	exit 1
fi
URL="$(cat "$ROOT/srv/fleet/gateway-url")"

# The gateway's data belongs to the fleet account.
as_fleet() {
	if [ "$(id -u)" -eq 0 ]; then
		runuser -u fleet -- "$@"
	else
		sudo -u fleet "$@"
	fi
}

if ! token="$(as_fleet "$ROOT/usr/local/bin/fleet-gateway" node add "$NAME" --data "$ROOT/var/lib/fleet" 2>/dev/null)"; then
	cat >&2 <<EOF
add-worker: couldn't create a token for '$NAME'. If that name is already registered,
choose another one, or revoke it first:
  sudo -u fleet /usr/local/bin/fleet-gateway node revoke $NAME --data /var/lib/fleet
A revoked name can't be registered again, so a replacement needs a new name.
EOF
	exit 1
fi

cat <<EOF
This is the token for $NAME. It isn't shown again:

  $token

If the worker is already installed on that machine, open its page, and on the
Connection tab choose "With a key from the server" and paste the token.

Otherwise, run one of these on the new machine, as the user the worker should
run as. Each contains the token.

On a Mac or a Linux machine with systemd, in a terminal:

  curl -fsSL $URL/install/worker.sh | FLEET_NODE_NAME=$NAME FLEET_NODE_TOKEN=$token sh

On Linux, the worker runs as a service under the account that runs this, which
needs sudo. As root, it runs as root, or put FLEET_WORKER_USER=ACCOUNT before sh.

On Windows, in PowerShell opened with "Run as administrator":

  [Net.ServicePointManager]::SecurityProtocol = 'Tls12'; \$env:FLEET_NODE_NAME = '$NAME'; \$env:FLEET_NODE_TOKEN = '$token'; irm $URL/install/worker.ps1 | iex

EOF
