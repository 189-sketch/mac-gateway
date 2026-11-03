#!/usr/bin/env bash
# Installs the gateway on Debian, Ubuntu, or a current CentOS Stream or RHEL-family
# server. Any of these works:
#
#   From a release, with nothing else installed (needs a public repo):
#     curl -fsSL RELEASE_URL/install-gateway.sh | bash -s -- fleet.example.com [node-name]
#
#   From an unpacked release bundle:
#     ./install-gateway.sh fleet.example.com [node-name]
#
#   From a checkout of the repo, which builds the binaries and needs Go:
#     ./deploy/install-gateway.sh fleet.example.com [node-name]
#
# It installs the gateway and Caddy as services and publishes the worker installer
# at https://DOMAIN/install/. On a first install, it prints an API key and the
# command to run on your first Mac, named mac-mini-1 by default. Run it as a normal
# user with sudo rights.
#
# Re-running it upgrades the gateway and keeps existing data and credentials. It
# only registers another machine if you pass a node name.
#
# Before running it, point DOMAIN's DNS at this server and open ports 80 and 443
# in any cloud security group. This script can't do either for you. If firewalld is
# running, it opens those two ports there. If SELinux is on, it lets Caddy reach
# the gateway and read the published installer, which SELinux would otherwise block.
#
# Everything is inside main, so a download cut off partway can't run half a script.

# build-release.sh fills this in. It stays a placeholder in a checkout.
RELEASE_URL="${FLEET_RELEASE_URL:-@@RELEASE_URL@@}"

# Only the install script's tests set this, to install into a scratch directory.
ROOT="${FLEET_INSTALL_ROOT:-}"

die() {
	echo "install-gateway: $*" >&2
	exit 1
}
step() { echo "==> $*"; }

server_arch() {
	case "$(uname -m)" in
	x86_64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported processor: $(uname -m)" ;;
	esac
}

# Puts everything to install in one directory and sets BUNDLE to it.
find_bundle() {
	local source_path="${BASH_SOURCE[0]:-}"
	local script_dir=""
	if [ -n "$source_path" ] && [ -f "$source_path" ]; then
		script_dir="$(cd "$(dirname "$source_path")" && pwd)"
	fi

	if [ -n "$script_dir" ] && [ -f "$script_dir/fleet-gateway" ]; then
		BUNDLE="$script_dir"
	elif [ -n "$script_dir" ] && [ -d "$script_dir/../cmd/fleet-gateway" ]; then
		build_from_source "$script_dir/.."
	else
		download_bundle
	fi
}

build_from_source() {
	local repo="$1"
	command -v go >/dev/null || die "Go isn't installed or isn't on PATH; it's needed to build from a checkout"
	step "Building binaries (about a minute the first time)"
	BUNDLE="$WORKDIR/bundle"
	mkdir -p "$BUNDLE"
	(
		cd "$repo"
		CGO_ENABLED=0 go build -trimpath -o "$BUNDLE/fleet-gateway" ./cmd/fleet-gateway
		for arch in arm64 amd64; do
			CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -trimpath -o "$BUNDLE/fleet-worker-darwin-$arch" ./cmd/fleet-worker
			CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build -trimpath -o "$BUNDLE/fleet-worker-windows-$arch.exe" ./cmd/fleet-worker
		done
		cp deploy/add-worker.sh deploy/install-worker.sh deploy/install-worker.ps1 deploy/fleet-gateway.service "$BUNDLE/"
	)
}

download_bundle() {
	case "$RELEASE_URL" in
	https://*) ;;
	*) die "run this from a release or a checkout of the repo, so it can find the binaries" ;;
	esac
	command -v tar >/dev/null || die "tar isn't installed"
	local name
	name="fleet-server-linux-$(server_arch)"
	step "Downloading $name"
	curl -fsSL --retry 3 -o "$WORKDIR/$name.tar.gz" "$RELEASE_URL/$name.tar.gz" ||
		die "couldn't download $RELEASE_URL/$name.tar.gz. If the repo is private, download the bundle while logged in, then run ./install-gateway.sh from inside it"
	curl -fsSL --retry 3 -o "$WORKDIR/SHA256SUMS" "$RELEASE_URL/SHA256SUMS" ||
		die "couldn't download the checksums"
	(cd "$WORKDIR" && grep " $name.tar.gz\$" SHA256SUMS | sha256sum -c - >/dev/null 2>&1) ||
		die "the download doesn't match its checksum"
	tar -xzf "$WORKDIR/$name.tar.gz" -C "$WORKDIR"
	BUNDLE="$WORKDIR/$name"
}

# Installs Caddy from its official package repository. These run before anything
# else changes, so a system that can't get Caddy is left as it was.
install_caddy() {
	step "Installing Caddy"
	if command -v apt-get >/dev/null; then
		sudo apt-get update -qq
		sudo apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https gnupg
		curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' |
			sudo gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
		curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' |
			sudo tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
		sudo apt-get update -qq
		sudo apt-get install -y -qq caddy
	else
		sudo dnf install -y -q dnf-plugins-core &&
			sudo dnf copr enable -y -q @caddy/caddy &&
			sudo dnf install -y -q caddy ||
			die "couldn't install Caddy with dnf, and nothing has been changed. If this system's package repositories have been retired, as CentOS 8 and CentOS Stream 8's have, install Caddy and a caddy systemd service by hand, then run this again"
	fi
}

selinux_active() {
	command -v getenforce >/dev/null && [ "$(getenforce 2>/dev/null)" != "Disabled" ]
}

# Caddy's package may run it as a confined web server. Then SELinux would refuse
# its connection to the gateway and answer 403 for the published files, even with
# every file permission correct. Both changes do nothing if Caddy isn't confined.
allow_caddy_under_selinux() {
	selinux_active || return 0
	step "Allowing Caddy through SELinux"
	sudo setsebool -P httpd_can_network_connect on ||
		die "couldn't set an SELinux option; see: sudo setsebool -P httpd_can_network_connect on"
	if command -v semanage >/dev/null; then
		sudo semanage fcontext -a -t httpd_sys_content_t '/srv/fleet(/.*)?' 2>/dev/null ||
			sudo semanage fcontext -m -t httpd_sys_content_t '/srv/fleet(/.*)?'
		sudo restorecon -R "$ROOT/srv/fleet"
	else
		# This label is lost if the whole system is relabelled. Running this script
		# again restores it.
		sudo chcon -R -t httpd_sys_content_t "$ROOT/srv/fleet"
	fi
}

# Applies now and after a reboot. A reload would do the second part in one step,
# but it would also drop any temporary rules someone else has added.
open_firewalld() {
	command -v firewall-cmd >/dev/null || return 0
	[ "$(sudo firewall-cmd --state 2>/dev/null)" = "running" ] || return 0
	step "Opening ports 80 and 443 in firewalld"
	sudo firewall-cmd --quiet --add-service=http --add-service=https
	sudo firewall-cmd --quiet --permanent --add-service=http --add-service=https
}

main() {
	set -euo pipefail

	local domain="${1:-}"
	local node_name_given="${2:-}"
	local node_name="${node_name_given:-mac-mini-1}"
	[ -n "$domain" ] || die "usage: install-gateway.sh DOMAIN [node-name]"
	[[ "$domain" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?\.[A-Za-z]{2,}$ ]] || die "'$domain' doesn't look like a domain name"
	[[ "$node_name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "'$node_name' isn't a valid node name"
	[ "$(id -u)" -ne 0 ] || die "run this as a normal user with sudo rights, not as root"

	command -v curl >/dev/null || die "curl isn't installed"
	command -v caddy >/dev/null || command -v apt-get >/dev/null || command -v dnf >/dev/null ||
		die "this script can install Caddy only with apt or dnf. On this system, which may be CentOS 7, install Caddy and a caddy systemd service by hand, then run this again"

	step "Asking for sudo rights"
	sudo true

	# Stop before changing anything if another web server holds the ports Caddy needs.
	local port holder
	for port in 80 443; do
		# Not ss -H: older versions reject it, which would look like a free port.
		holder="$(sudo ss -ltnp "sport = :$port" 2>/dev/null | grep LISTEN || true)"
		if [ -n "$holder" ] && ! grep -q '"caddy"' <<<"$holder"; then
			die "port $port is already in use by something other than Caddy: $holder"
		fi
	done

	WORKDIR="$(mktemp -d)"
	trap 'rm -rf "$WORKDIR"' EXIT
	find_bundle
	"$BUNDLE/fleet-gateway" version >/dev/null 2>&1 ||
		die "this bundle's gateway can't run here. This machine is $(uname -m), and the bundle is in $BUNDLE, which must not be mounted noexec"

	command -v caddy >/dev/null || install_caddy
	# sudo may not search where Caddy is, such as /usr/local/bin on CentOS.
	local caddy
	caddy="$(command -v caddy)" || die "Caddy still isn't on PATH after installing it"
	sudo systemctl cat caddy >/dev/null 2>&1 ||
		die "Caddy is installed, but there's no caddy systemd service to run it. Create one, then run this again"

	step "Installing the gateway service"
	if ! id fleet >/dev/null 2>&1; then
		sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin fleet
	fi
	sudo install -d -m 755 "$ROOT/usr/local/bin" "$ROOT/etc/systemd/system"
	sudo install -m 755 "$BUNDLE/fleet-gateway" "$ROOT/usr/local/bin/fleet-gateway"
	sudo install -m 755 "$BUNDLE/add-worker.sh" "$ROOT/usr/local/bin/fleet-add-worker"
	sudo install -m 644 "$BUNDLE/fleet-gateway.service" "$ROOT/etc/systemd/system/fleet-gateway.service"
	sudo install -d -m 700 "$ROOT/var/lib/fleet"
	sudo chown fleet:fleet "$ROOT/var/lib/fleet"
	if selinux_active; then
		# Files keep a label from where they were made, and a wrong one stops a service.
		sudo restorecon "$ROOT/usr/local/bin/fleet-gateway" "$ROOT/usr/local/bin/fleet-add-worker" "$ROOT/etc/systemd/system/fleet-gateway.service"
	fi
	sudo systemctl daemon-reload
	sudo systemctl enable fleet-gateway >/dev/null 2>&1
	sudo systemctl restart fleet-gateway

	local ready="" _
	for _ in $(seq 1 30); do
		if curl -fsS --max-time 2 http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
			ready=yes
			break
		fi
		sleep 1
	done
	[ -n "$ready" ] || die "the gateway didn't start; see: sudo journalctl -u fleet-gateway -n 50"

	step "Publishing the worker installer"
	local public="$ROOT/srv/fleet/install"
	local url="https://$domain"
	local staging="$WORKDIR/publish"
	mkdir -p "$staging"
	local binaries=(fleet-worker-darwin-arm64 fleet-worker-darwin-amd64 fleet-worker-windows-amd64.exe fleet-worker-windows-arm64.exe)
	(cd "$BUNDLE" && cp "${binaries[@]}" "$staging/")
	sed "s|@@GATEWAY_URL@@|$url|g" "$BUNDLE/install-worker.sh" >"$staging/worker.sh"
	sed "s|@@GATEWAY_URL@@|$url|g" "$BUNDLE/install-worker.ps1" >"$staging/worker.ps1"
	(cd "$staging" && sha256sum "${binaries[@]}" >SHA256SUMS)
	sudo install -d -m 755 "$public"
	(cd "$staging" && sudo install -m 644 "${binaries[@]}" SHA256SUMS worker.sh worker.ps1 "$public/")
	echo "$url" | sudo tee "$ROOT/srv/fleet/gateway-url" >/dev/null

	step "Setting up HTTPS with Caddy"
	allow_caddy_under_selinux
	open_firewalld
	sudo install -d -m 755 "$ROOT/etc/caddy"
	# This goes in its own file, so a Caddyfile you already have keeps working.
	sudo tee "$ROOT/etc/caddy/fleet.caddy" >/dev/null <<CADDY
$domain {
	handle_path /install/* {
		root * /srv/fleet/install
		file_server
	}
	handle {
		reverse_proxy 127.0.0.1:8080
	}
}
CADDY
	sudo touch "$ROOT/etc/caddy/Caddyfile"
	if ! sudo grep -qxF 'import fleet.caddy' "$ROOT/etc/caddy/Caddyfile"; then
		echo 'import fleet.caddy' | sudo tee -a "$ROOT/etc/caddy/Caddyfile" >/dev/null
	fi
	sudo "$caddy" validate --adapter caddyfile --config "$ROOT/etc/caddy/Caddyfile" >/dev/null 2>&1 ||
		die "the Caddy config is invalid; see: sudo $caddy validate --config /etc/caddy/Caddyfile"
	sudo systemctl enable caddy >/dev/null 2>&1
	sudo systemctl restart caddy

	step "Checking $url (getting a certificate can take a minute)"
	local reachable=""
	for _ in $(seq 1 30); do
		if curl -fsS --max-time 5 "$url/healthz" >/dev/null 2>&1; then
			reachable=yes
			break
		fi
		sleep 2
	done

	# Secrets are shown once only, so these print whether or not HTTPS is working yet.
	echo
	step "Creating an API key"
	local key first_install=""
	if key="$(sudo -u fleet "$ROOT/usr/local/bin/fleet-gateway" key add agent --capabilities '*' --data "$ROOT/var/lib/fleet" 2>/dev/null)"; then
		cat <<KEY
Give these to your agent. The key isn't shown again:

  export FLEET_URL=$url
  export FLEET_API_KEY=$key

KEY
		first_install=yes
	else
		echo "An API key named 'agent' already exists, so it was kept."
		echo
	fi

	if [ -n "$first_install" ] || [ -n "$node_name_given" ]; then
		"$ROOT/usr/local/bin/fleet-add-worker" "$node_name" || true
	fi

	if [ -z "$reachable" ]; then
		cat >&2 <<WARNING

WARNING: the gateway is running, but $url isn't reachable yet, so the worker
install command won't work until it is. Check that:
  - DNS for $domain points at this server's public IP
  - ports 80 and 443 are open in any firewall or cloud security group
Then see: sudo journalctl -u caddy -n 50
Caddy keeps retrying the certificate, so you don't need to run this again.
WARNING
		exit 1
	fi
	step "Done"
}

main "$@"
