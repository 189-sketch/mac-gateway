#!/usr/bin/env bash
# Registers a new machine on an installed gateway and prints the command to run
# on that machine. Run it on the gateway server.
#
#   ./deploy/add-worker.sh mac-mini-2
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
  sudo -u fleet fleet-gateway node revoke $NAME --data /var/lib/fleet
A revoked name can't be registered again, so a replacement needs a new name.
EOF
	exit 1
fi

cat <<EOF
Run this on the Mac, in Terminal, logged in as the user the worker should run as.
It contains the machine's token, which isn't shown again:

  curl -fsSL $URL/install/worker.sh | FLEET_NODE_NAME=$NAME FLEET_NODE_TOKEN=$token sh

EOF
