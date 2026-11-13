#!/usr/bin/env bash
# Shows the pairing code of each machine asking to join this gateway, as it asks,
# and says when one has connected. Run it on the gateway server.
#
#   fleet-pair                      watch for 15 minutes
#   fleet-pair --wait 0             list what's waiting now and exit
#   fleet-pair --until-connected    stop once a machine has connected
set -euo pipefail

# Only the install script's tests set this, to install into a scratch directory.
ROOT="${FLEET_INSTALL_ROOT:-}"

if [ ! -x "$ROOT/usr/local/bin/fleet-gateway" ]; then
	echo "fleet-pair: the gateway isn't installed here; run install-gateway.sh first" >&2
	exit 1
fi
# The gateway's data belongs to the fleet account.
if [ "$(id -u)" -eq 0 ]; then
	exec runuser -u fleet -- "$ROOT/usr/local/bin/fleet-gateway" pair --data "$ROOT/var/lib/fleet" "$@"
fi
exec sudo -u fleet "$ROOT/usr/local/bin/fleet-gateway" pair --data "$ROOT/var/lib/fleet" "$@"
