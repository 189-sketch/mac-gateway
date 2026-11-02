#!/usr/bin/env bash
# Installs the gateway on a Debian or Ubuntu server, from a checkout of this repo.
#
#   ./deploy/install-gateway.sh fleet.example.com [node-name]
#
# It builds the binaries, installs the gateway and Caddy as services, and publishes
# the worker installer at https://DOMAIN/install/. On a first install, it prints an
# API key and the command to run on your first Mac, named mac-mini-1 by default.
# Run it as a normal user with sudo rights.
#
# Re-running it upgrades the gateway and keeps existing data and credentials. It
# only registers another machine if you pass a node name.
#
# Before running it, point DOMAIN's DNS at this server and open ports 80 and 443
# in its firewall or cloud security group. This script can't do either for you.
set -euo pipefail

# Only the install script's tests set this, to install into a scratch directory.
ROOT="${FLEET_INSTALL_ROOT:-}"

die() {
	echo "install-gateway: $*" >&2
	exit 1
}
step() { echo "==> $*"; }

DOMAIN="${1:-}"
NODE_NAME_GIVEN="${2:-}"
NODE_NAME="${NODE_NAME_GIVEN:-mac-mini-1}"
[ -n "$DOMAIN" ] || die "usage: $0 DOMAIN [first-node-name]"
[[ "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?\.[A-Za-z]{2,}$ ]] || die "'$DOMAIN' doesn't look like a domain name"
[[ "$NODE_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "'$NODE_NAME' isn't a valid node name"
[ "$(id -u)" -ne 0 ] || die "run this as a normal user with sudo rights, not as root, so the build doesn't run as root"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v go >/dev/null || die "Go isn't installed or isn't on PATH; it's needed to build the binaries"
command -v curl >/dev/null || die "curl isn't installed"
command -v apt-get >/dev/null || command -v caddy >/dev/null ||
	die "this script installs Caddy with apt; on other systems, install Caddy first"

step "Asking for sudo rights"
sudo true

# Stop before changing anything if another web server holds the ports Caddy needs.
for port in 80 443; do
	holder="$(sudo ss -Hltnp "sport = :$port" 2>/dev/null || true)"
	if [ -n "$holder" ] && ! grep -q '"caddy"' <<<"$holder"; then
		die "port $port is already in use by something other than Caddy: $holder"
	fi
done

step "Building binaries (about a minute the first time)"
cd "$REPO"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/fleet-gateway-linux-amd64 ./cmd/fleet-gateway
for arch in arm64 amd64; do
	CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -trimpath -o "dist/fleet-worker-darwin-$arch" ./cmd/fleet-worker
done

step "Installing the gateway service"
if ! id fleet >/dev/null 2>&1; then
	sudo useradd --system --no-create-home --shell /usr/sbin/nologin fleet
fi
sudo install -d -m 755 "$ROOT/usr/local/bin" "$ROOT/etc/systemd/system"
sudo install -m 755 dist/fleet-gateway-linux-amd64 "$ROOT/usr/local/bin/fleet-gateway"
sudo install -m 644 deploy/fleet-gateway.service "$ROOT/etc/systemd/system/fleet-gateway.service"
sudo install -d -m 700 "$ROOT/var/lib/fleet"
sudo chown fleet:fleet "$ROOT/var/lib/fleet"
sudo systemctl daemon-reload
sudo systemctl enable fleet-gateway >/dev/null 2>&1
sudo systemctl restart fleet-gateway

ready=""
for _ in $(seq 1 30); do
	if curl -fsS --max-time 2 http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
		ready=yes
		break
	fi
	sleep 1
done
[ -n "$ready" ] || die "the gateway didn't start; see: sudo journalctl -u fleet-gateway -n 50"

step "Publishing the worker installer"
PUBLIC="$ROOT/srv/fleet/install"
URL="https://$DOMAIN"
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
cp dist/fleet-worker-darwin-arm64 dist/fleet-worker-darwin-amd64 "$workdir/"
sed "s|@@GATEWAY_URL@@|$URL|g" deploy/install-worker.sh >"$workdir/worker.sh"
(cd "$workdir" && sha256sum fleet-worker-darwin-arm64 fleet-worker-darwin-amd64 >SHA256SUMS)
sudo install -d -m 755 "$PUBLIC"
sudo install -m 644 "$workdir"/fleet-worker-darwin-arm64 "$workdir"/fleet-worker-darwin-amd64 "$workdir"/SHA256SUMS "$workdir"/worker.sh "$PUBLIC/"
echo "$URL" | sudo tee "$ROOT/srv/fleet/gateway-url" >/dev/null

step "Setting up HTTPS with Caddy"
if ! command -v caddy >/dev/null; then
	sudo apt-get update -qq
	sudo apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https gnupg
	curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' |
		sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
	curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' |
		sudo tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
	sudo apt-get update -qq
	sudo apt-get install -y -qq caddy
fi
sudo install -d -m 755 "$ROOT/etc/caddy"
# This goes in its own file, so a Caddyfile you already have keeps working.
sudo tee "$ROOT/etc/caddy/fleet.caddy" >/dev/null <<EOF
$DOMAIN {
	handle_path /install/* {
		root * /srv/fleet/install
		file_server
	}
	handle {
		reverse_proxy 127.0.0.1:8080
	}
}
EOF
sudo touch "$ROOT/etc/caddy/Caddyfile"
if ! sudo grep -qxF 'import fleet.caddy' "$ROOT/etc/caddy/Caddyfile"; then
	echo 'import fleet.caddy' | sudo tee -a "$ROOT/etc/caddy/Caddyfile" >/dev/null
fi
sudo caddy validate --adapter caddyfile --config "$ROOT/etc/caddy/Caddyfile" >/dev/null 2>&1 ||
	die "the Caddy config is invalid; see: sudo caddy validate --config /etc/caddy/Caddyfile"
sudo systemctl enable caddy >/dev/null 2>&1
sudo systemctl restart caddy

step "Checking https://$DOMAIN (getting a certificate can take a minute)"
reachable=""
for _ in $(seq 1 30); do
	if curl -fsS --max-time 5 "$URL/healthz" >/dev/null 2>&1; then
		reachable=yes
		break
	fi
	sleep 2
done

# Secrets are shown once only, so these print whether or not HTTPS is working yet.
echo
step "Creating an API key"
if key="$(sudo -u fleet "$ROOT/usr/local/bin/fleet-gateway" key add agent --capabilities '*' --data "$ROOT/var/lib/fleet" 2>/dev/null)"; then
	cat <<EOF
Give these to your agent. The key isn't shown again:

  export FLEET_URL=$URL
  export FLEET_API_KEY=$key

EOF
	first_install=yes
else
	echo "An API key named 'agent' already exists, so it was kept."
	echo
	first_install=""
fi

if [ -n "$first_install" ] || [ -n "$NODE_NAME_GIVEN" ]; then
	"$REPO/deploy/add-worker.sh" "$NODE_NAME" || true
fi

if [ -z "$reachable" ]; then
	cat >&2 <<EOF

WARNING: the gateway is running, but $URL isn't reachable yet, so the Mac
install command won't work until it is. Check that:
  - DNS for $DOMAIN points at this server's public IP
  - ports 80 and 443 are open in the firewall or cloud security group
Then see: sudo journalctl -u caddy -n 50
Caddy keeps retrying the certificate, so you don't need to run this again.
EOF
	exit 1
fi
step "Done"
