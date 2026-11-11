#!/usr/bin/env bash
# Registers a new machine on an installed gateway and prints the commands to run
# on that machine. Run it on the gateway server.
#
#   fleet-add-worker mac-mini-2
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

if ! token="$(sudo -u fleet "$ROOT/usr/local/bin/fleet-gateway" node add "$NAME" --data "$ROOT/var/lib/fleet" 2>/dev/null)"; then
	cat >&2 <<EOF
add-worker: couldn't create a token for '$NAME'. If that name is already registered,
choose another one, or revoke it first:
  sudo -u fleet /usr/local/bin/fleet-gateway node revoke $NAME --data /var/lib/fleet
A revoked name can't be registered again, so a replacement needs a new name.
EOF
	exit 1
fi

cat <<EOF
Run one of these on the new machine, as the user the worker should run as. Each
contains the machine's token, which isn't shown again.

On a Mac or a Linux machine with systemd, in a terminal:

  curl -fsSL $URL/install/worker.sh | FLEET_NODE_NAME=$NAME FLEET_NODE_TOKEN=$token sh

On Linux, the worker runs as a service under the account that runs this, which
needs sudo. Or, as root, put FLEET_WORKER_USER=ACCOUNT before sh to choose it.

On Windows, in PowerShell opened with "Run as administrator":

  [Net.ServicePointManager]::SecurityProtocol = 'Tls12'; \$env:FLEET_NODE_NAME = '$NAME'; \$env:FLEET_NODE_TOKEN = '$token'; irm $URL/install/worker.ps1 | iex

EOF
