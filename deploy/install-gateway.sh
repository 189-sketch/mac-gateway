#!/usr/bin/env bash
# Installs the gateway on Debian, Ubuntu, or a current CentOS Stream or RHEL-family
# server. Any of these works:
#
#   From a release, with nothing else installed (needs a public repo):
#     curl -fsSL RELEASE_URL/install-gateway.sh | bash -s -- [ADDRESS] [node-name]
#
#   From an unpacked release bundle:
#     ./install-gateway.sh [ADDRESS] [node-name]
#
#   From a checkout of the repo, which builds the binaries and needs Go:
#     ./deploy/install-gateway.sh [ADDRESS] [node-name]
#
# ADDRESS is a domain name such as fleet.example.com, or this server's public IPv4
# address. With none, a first install finds this server's public IP address, and a
# later run keeps the address it used before.
#
# It installs the gateway and Caddy as services, and Caddy gets and renews an HTTPS
# certificate from Let's Encrypt by itself, for an IP address too. It publishes the
# worker installer at https://ADDRESS/install/. On a first install, it prints an API
# key and how to connect an agent through MCP. Then it prints the command to run
# on each machine, and waits, showing the pairing code each machine needs.
# Run it as root, or as a user with sudo rights.
#
# Re-running it upgrades the gateway and keeps existing data and credentials. A
# node name makes a token for that machine, for joining without a pairing code.
#
# Before running it, open ports 80 and 443 in any cloud security group, and for a
# domain, point its DNS at this server. This script can't do either for you. If
# firewalld is running, it opens those two ports there. If SELinux is on, it lets
# Caddy reach the gateway and read the published installer, which SELinux would
# otherwise block.
#
# Everything is inside main, so a download cut off partway can't run half a script.

# build-release.sh fills this in. It stays a placeholder in a checkout.
RELEASE_URL="${FLEET_RELEASE_URL:-@@RELEASE_URL@@}"

# Only the install script's tests set these: to install into a scratch directory,
# and to answer the question without a terminal.
ROOT="${FLEET_INSTALL_ROOT:-}"
TTY="${FLEET_INSTALL_TTY:-/dev/tty}"

# As root there's nothing to ask sudo for, and a minimal system may not have it.
# This stands in for the two ways the script uses it.
if [ "$(id -u)" -eq 0 ]; then
	sudo() {
		if [ "${1:-}" = "-u" ]; then
			local user="$2"
			shift 2
			runuser -u "$user" -- "$@"
		else
			"$@"
		fi
	}
fi

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
			CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$BUNDLE/fleet-worker-linux-$arch" ./cmd/fleet-worker
			CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build -trimpath -o "$BUNDLE/fleet-worker-windows-$arch.exe" ./cmd/fleet-worker
		done
		cp deploy/add-worker.sh deploy/pair.sh deploy/install-worker.sh deploy/install-worker.ps1 deploy/fleet-gateway.service "$BUNDLE/"
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
# Caddy's repository is built for EPEL and has no CentOS Stream build by that name,
# so if dnf can't work out which to use, name the EPEL one for this version.
enable_caddy_copr() {
	sudo dnf copr enable -y -q @caddy/caddy && return 0
	version=$(. "$ROOT/etc/os-release" 2>/dev/null && printf '%s' "${VERSION_ID%%.*}") || return 1
	[ -n "$version" ] || return 1
	sudo dnf copr enable -y -q @caddy/caddy "epel-$version-$(uname -m)"
}

# Installs Caddy, or upgrades it to the newest version its repository has.
install_caddy() {
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
			enable_caddy_copr &&
			sudo dnf install -y -q caddy &&
			sudo dnf upgrade -y -q caddy ||
			die "couldn't install Caddy with dnf, and nothing has been changed. If this system's package repositories have been retired, as CentOS 8 and CentOS Stream 8's have, install Caddy and a caddy systemd service by hand, then run this again"
	fi
}

# Let's Encrypt's certificates for IP addresses need Caddy 2.11.4 or newer, which
# can ask for its short-lived certificate profile and handles IP addresses fully.
MIN_CADDY_FOR_IP="2 11 4"

caddy_version() { # caddy binary; prints "MAJOR MINOR PATCH", or nothing
	"$1" version 2>/dev/null | sed -n 's/^v\([0-9][0-9]*\)\.\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2 \3/p' | head -n 1
}

version_at_least() { # "MAJOR MINOR PATCH" "MAJOR MINOR PATCH"
	local have want
	read -r -a have <<<"$1"
	read -r -a want <<<"$2"
	[ "${#have[@]}" -eq 3 ] || return 1
	local i
	for i in 0 1 2; do
		[ "${have[$i]}" -gt "${want[$i]}" ] && return 0
		[ "${have[$i]}" -lt "${want[$i]}" ] && return 1
	done
	return 0
}

is_ipv4() {
	[[ "$1" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || return 1
	local octet
	for octet in "${BASH_REMATCH[@]:1}"; do
		[ "$((10#$octet))" -le 255 ] || return 1
		# A leading zero means octal to some programs.
		[[ "$octet" == 0 || "$octet" != 0* ]] || return 1
	done
}

# Let's Encrypt only issues for addresses reachable from the internet.
is_public_ipv4() {
	local a b
	IFS=. read -r a b _ _ <<<"$1"
	case "$a" in
	0 | 10 | 127) return 1 ;;
	esac
	[ "$a" -ge 224 ] && return 1
	[ "$a" -eq 169 ] && [ "$b" -eq 254 ] && return 1
	[ "$a" -eq 172 ] && [ "$b" -ge 16 ] && [ "$b" -le 31 ] && return 1
	[ "$a" -eq 192 ] && [ "$b" -eq 168 ] && return 1
	[ "$a" -eq 100 ] && [ "$b" -ge 64 ] && [ "$b" -le 127 ] && return 1
	return 0
}

# Asks services that echo the caller's address, since behind a cloud provider's
# NAT this machine can't see its own public address.
detect_public_ip() {
	local service ip
	for service in https://api.ipify.org https://checkip.amazonaws.com https://ifconfig.me/ip; do
		ip="$(curl -4 -fsS --max-time 5 "$service" 2>/dev/null | tr -d '[:space:]')" || continue
		if is_ipv4 "$ip"; then
			echo "$ip"
			return 0
		fi
	done
	return 1
}

# interactive says whether there's a person to ask. Piped from curl, stdin is the
# script itself, so questions are read from the terminal instead.
interactive() {
	[ -z "${FLEET_NONINTERACTIVE:-}" ] || return 1
	[ -z "${FLEET_INSTALL_TTY:-}" ] || return 0
	[ -t 1 ] && (: <"$TTY") 2>/dev/null
}

is_domain() {
	[[ "$1" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?\.[A-Za-z]{2,}$ ]]
}

# Asks for a domain, and prints it. Printing nothing means use the IP address,
# which is what pressing Enter does. $1 is that address, or empty if unknown.
ask_domain() {
	local answer tries
	for tries in 1 2 3; do
		if [ -n "$1" ]; then
			printf "Domain name for this server, if it has one (press Enter to use its IP address, %s): " "$1" >&2
		else
			printf "This server's public IP address, or its domain name: " >&2
		fi
		read -r answer || answer=""
		answer="$(printf '%s' "$answer" | tr -d '[:space:]' | tr 'A-Z' 'a-z')"
		answer="${answer#https://}"
		answer="${answer%/}"
		if [ -z "$answer" ] || is_domain "$answer" || is_ipv4 "$answer"; then
			printf '%s' "$answer"
			return 0
		fi
		echo "'$answer' doesn't look like a domain name, such as fleet.example.com." >&2
	done
} <"$TTY"

# Lines this script adds to the Caddyfile end with this, so it can find and
# replace them, and leave the rest alone.
CADDY_MARK='# fleet-gateway'

# Sets this server's address as default_sni in the Caddyfile's global options, or
# with an empty address, removes it. A client that connects to an IP address sends
# no server name, and behind a cloud provider's NAT, the address it reached isn't
# one this machine has, so Caddy needs telling which certificate to use.
#
# with_default_sni writes the changed Caddyfile to a new file. It fails with 3 if
# the Caddyfile already sets default_sni itself.
with_default_sni() { # Caddyfile, IP address or empty, output file
	local file="$1" ip="$2" updated="$3"
	local stripped="$updated.stripped"
	if [ -f "$file" ]; then
		sudo cat "$file" | { grep -v -- "$CADDY_MARK\$" || true; } >"$stripped"
	else
		: >"$stripped"
	fi
	if [ -z "$ip" ]; then
		cp "$stripped" "$updated"
	else
		awk -v ip="$ip" -v mark="$CADDY_MARK" '
			function ours() { print "{ " mark; print "\tdefault_sni " ip " " mark; print "} " mark }
			!placed && /^[[:space:]]*(#.*)?$/ { print; next }
			!placed {
				placed = 1
				# A global options block is a lone { at the very top.
				if ($0 ~ /^[[:space:]]*\{[[:space:]]*(#.*)?$/) {
					print
					print "\tdefault_sni " ip " " mark
					depth = 1
					next
				}
				ours()
			}
			depth > 0 {
				if (depth == 1 && $0 ~ /^[[:space:]]*default_sni[[:space:]]/) clash = 1
				line = $0
				sub(/#.*/, "", line)
				depth += gsub(/\{/, "{", line) - gsub(/\}/, "}", line)
			}
			{ print }
			END {
				if (!placed) ours()
				if (clash) exit 3
			}
		' "$stripped" >"$updated" || return
	fi
}

set_default_sni() { # Caddyfile, IP address or empty
	with_default_sni "$1" "$2" "$WORKDIR/Caddyfile.updated"
	sudo tee "$1" <"$WORKDIR/Caddyfile.updated" >/dev/null
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

	local address="${1:-}"
	local node_name_given="${2:-}"
	local node_name="${node_name_given:-worker}"
	[[ "$node_name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || die "'$node_name' isn't a valid node name"
	command -v curl >/dev/null || die "curl isn't installed"
	if [ "$(id -u)" -eq 0 ]; then
		command -v runuser >/dev/null || die "runuser isn't installed; it's part of util-linux"
	else
		command -v sudo >/dev/null || die "sudo isn't installed. Run this as root instead"
	fi

	# The address workers and agents use, and the one in installed workers' settings.
	local previous_url=""
	if [ -f "$ROOT/srv/fleet/gateway-url" ]; then
		previous_url="$(cat "$ROOT/srv/fleet/gateway-url")"
	fi
	if [ -z "$address" ] && [ -n "$previous_url" ]; then
		address="${previous_url#https://}"
		step "Keeping the address from the last install, $address"
	elif [ -z "$address" ]; then
		step "Finding this server's public IP address"
		local found="" domain=""
		found="$(detect_public_ip)" || found=""
		if [ -n "$found" ]; then
			echo "This server is at $found."
		fi
		# A domain is optional. The IP address works by itself, HTTPS included.
		if interactive; then
			domain="$(ask_domain "$found")"
		fi
		address="${domain:-$found}"
		[ -n "$address" ] ||
			die "couldn't find this server's public IP address. Give it, or a domain, as the first argument: install-gateway.sh ADDRESS"
		if [ -z "$domain" ]; then
			echo "Using $address. If that's wrong, run this again with the right address."
		fi
	fi

	local mode
	if is_ipv4 "$address"; then
		is_public_ipv4 "$address" ||
			die "$address is a private address, which Let's Encrypt can't issue a certificate for. Use this server's public IP address, or a domain"
		mode=ip
	elif [[ "$address" == *:* ]]; then
		die "IPv6 addresses aren't supported yet. Use this server's public IPv4 address, or a domain"
	elif is_domain "$address"; then
		mode=domain
	else
		die "'$address' doesn't look like a domain name or an IPv4 address"
	fi

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
	if [ "$mode" = ip ]; then
		local sni_status=0
		with_default_sni "$ROOT/etc/caddy/Caddyfile" "$address" "$WORKDIR/Caddyfile.check" || sni_status=$?
		case "$sni_status" in
		0) ;;
		3) die "/etc/caddy/Caddyfile already sets default_sni in its global options. Remove that line, or set it to $address, then run this again. Nothing has been changed" ;;
		*) die "couldn't read /etc/caddy/Caddyfile to add this server's address to it. Nothing has been changed" ;;
		esac
	fi
	find_bundle
	"$BUNDLE/fleet-gateway" version >/dev/null 2>&1 ||
		die "this bundle's gateway can't run here. This machine is $(uname -m), and the bundle is in $BUNDLE, which must not be mounted noexec"

	if ! command -v caddy >/dev/null; then
		step "Installing Caddy"
		install_caddy
	fi
	# sudo may not search where Caddy is, such as /usr/local/bin on CentOS.
	local caddy
	caddy="$(command -v caddy)" || die "Caddy still isn't on PATH after installing it"
	if [ "$mode" = ip ] && ! version_at_least "$(caddy_version "$caddy")" "$MIN_CADDY_FOR_IP"; then
		step "Upgrading Caddy, because a certificate for an IP address needs Caddy ${MIN_CADDY_FOR_IP// /.} or newer"
		install_caddy
		caddy="$(command -v caddy)" || die "Caddy isn't on PATH after upgrading it"
		version_at_least "$(caddy_version "$caddy")" "$MIN_CADDY_FOR_IP" ||
			die "this server's Caddy is $("$caddy" version 2>/dev/null | head -n 1), and a certificate for an IP address needs ${MIN_CADDY_FOR_IP// /.} or newer. Upgrade Caddy, or use a domain, then run this again. Nothing has been changed"
	fi
	sudo systemctl cat caddy >/dev/null 2>&1 ||
		die "Caddy is installed, but there's no caddy systemd service to run it. Create one, then run this again"

	step "Installing the gateway service"
	if ! id fleet >/dev/null 2>&1; then
		sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin fleet
	fi
	sudo install -d -m 755 "$ROOT/usr/local/bin" "$ROOT/etc/systemd/system"
	sudo install -m 755 "$BUNDLE/fleet-gateway" "$ROOT/usr/local/bin/fleet-gateway"
	sudo install -m 755 "$BUNDLE/add-worker.sh" "$ROOT/usr/local/bin/fleet-add-worker"
	sudo install -m 755 "$BUNDLE/pair.sh" "$ROOT/usr/local/bin/fleet-pair"
	sudo install -m 644 "$BUNDLE/fleet-gateway.service" "$ROOT/etc/systemd/system/fleet-gateway.service"
	sudo install -d -m 700 "$ROOT/var/lib/fleet"
	sudo chown fleet:fleet "$ROOT/var/lib/fleet"
	if selinux_active; then
		# Files keep a label from where they were made, and a wrong one stops a service.
		sudo restorecon "$ROOT/usr/local/bin/fleet-gateway" "$ROOT/usr/local/bin/fleet-add-worker" "$ROOT/usr/local/bin/fleet-pair" "$ROOT/etc/systemd/system/fleet-gateway.service"
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
	local url="https://$address"
	local staging="$WORKDIR/publish"
	mkdir -p "$staging"
	local binaries=(fleet-worker-darwin-arm64 fleet-worker-darwin-amd64 fleet-worker-linux-amd64 fleet-worker-linux-arm64 fleet-worker-windows-amd64.exe fleet-worker-windows-arm64.exe)
	(cd "$BUNDLE" && cp "${binaries[@]}" "$staging/")
	# A bundle from a release already names the release. From a checkout there's
	# none, and the gateway is where workers download from anyway. The placeholder
	# is spelled in two parts so building a release doesn't fill it in here too.
	local no_release='@@RELEASE''_URL@@'
	sed -e "s|@@GATEWAY_URL@@|$url|g" -e "s|$no_release||g" "$BUNDLE/install-worker.sh" >"$staging/worker.sh"
	sed -e "s|@@GATEWAY_URL@@|$url|g" -e "s|$no_release||g" "$BUNDLE/install-worker.ps1" >"$staging/worker.ps1"
	(cd "$staging" && sha256sum "${binaries[@]}" >SHA256SUMS)
	sudo install -d -m 755 "$public"
	(cd "$staging" && sudo install -m 644 "${binaries[@]}" SHA256SUMS worker.sh worker.ps1 "$public/")
	echo "$url" | sudo tee "$ROOT/srv/fleet/gateway-url" >/dev/null

	step "Setting up HTTPS with Caddy"
	allow_caddy_under_selinux
	open_firewalld
	sudo install -d -m 755 "$ROOT/etc/caddy"
	sudo touch "$ROOT/etc/caddy/Caddyfile"
	# Keep the current config, to put back if the new one isn't valid.
	local backup="$WORKDIR/caddy-backup"
	mkdir -p "$backup"
	sudo cat "$ROOT/etc/caddy/Caddyfile" >"$backup/Caddyfile"
	local had_fleet_caddy=""
	if [ -f "$ROOT/etc/caddy/fleet.caddy" ]; then
		sudo cat "$ROOT/etc/caddy/fleet.caddy" >"$backup/fleet.caddy"
		had_fleet_caddy=yes
	fi

	# Let's Encrypt issues certificates for IP addresses only with its short-lived
	# profile. They last about six days, and Caddy renews them every few days.
	local tls_block=""
	if [ "$mode" = ip ]; then
		tls_block="	tls {
		issuer acme {
			profile shortlived
		}
	}
"
		set_default_sni "$ROOT/etc/caddy/Caddyfile" "$address"
	else
		set_default_sni "$ROOT/etc/caddy/Caddyfile" ""
	fi
	# This goes in its own file, so a Caddyfile you already have keeps working.
	sudo tee "$ROOT/etc/caddy/fleet.caddy" >/dev/null <<CADDY
$address {
${tls_block}	handle_path /install/* {
		root * /srv/fleet/install
		file_server
	}
	handle {
		reverse_proxy 127.0.0.1:8080
	}
}
CADDY
	if ! sudo grep -qxF 'import fleet.caddy' "$ROOT/etc/caddy/Caddyfile"; then
		echo 'import fleet.caddy' | sudo tee -a "$ROOT/etc/caddy/Caddyfile" >/dev/null
	fi
	local problem
	if ! problem="$(sudo "$caddy" validate --adapter caddyfile --config "$ROOT/etc/caddy/Caddyfile" 2>&1)"; then
		sudo tee "$ROOT/etc/caddy/Caddyfile" <"$backup/Caddyfile" >/dev/null
		if [ -n "$had_fleet_caddy" ]; then
			sudo tee "$ROOT/etc/caddy/fleet.caddy" <"$backup/fleet.caddy" >/dev/null
		else
			sudo rm -f "$ROOT/etc/caddy/fleet.caddy"
		fi
		echo "$problem" | tail -n 5 >&2
		die "the new Caddy config isn't valid, so the old one was put back and Caddy wasn't restarted"
	fi
	sudo systemctl enable caddy >/dev/null 2>&1
	sudo systemctl restart caddy

	if [ -n "$previous_url" ] && [ "$previous_url" != "$url" ]; then
		cat >&2 <<MOVED

Note: the gateway's address changed from $previous_url to $url. Workers installed
before still use the old one. On each, change gateway_url in worker.yaml and
restart the worker.
MOVED
	fi

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
	step "The gateway is installed at $url"
	local key
	if key="$(sudo -u fleet "$ROOT/usr/local/bin/fleet-gateway" key add agent --capabilities '*' --data "$ROOT/var/lib/fleet" 2>/dev/null)"; then
		cat <<KEY

1. Connect your agent, through MCP

   Address:  $url/mcp
   API key:  $key

   The key isn't shown again. Clients send it as "Authorization: Bearer KEY".
   For Claude Code, run this where you use it:

  claude mcp add --transport http fleet $url/mcp --header "Authorization: Bearer $key"

   Or, as an entry in Claude Code's .mcp.json:

  {"mcpServers": {"fleet": {"type": "http", "url": "$url/mcp", "headers": {"Authorization": "Bearer $key"}}}}

   For the fleet command:

  export FLEET_URL=$url
  export FLEET_API_KEY=$key
KEY
	else
		cat <<KEY

1. Connect your agent, through MCP

   Address:  $url/mcp
   API key:  the one printed when the gateway was first installed. An API key
             named 'agent' already exists, so it was kept. To make another:
             sudo -u fleet fleet-gateway key add NAME --capabilities '*' --data /var/lib/fleet
KEY
	fi
	cat <<WORKER

2. Add a machine

   On a Mac or a Linux machine, in a terminal:

  curl -fsSL $url/install/worker.sh | sh

   On Windows, in PowerShell opened with "Run as administrator":

  [Net.ServicePointManager]::SecurityProtocol = 'Tls12'; irm $url/install/worker.ps1 | iex

   Each asks for a pairing code, which this server shows. To see the codes at
   any time, run: sudo fleet-pair

WORKER

	if [ -n "$node_name_given" ]; then
		"$ROOT/usr/local/bin/fleet-add-worker" "$node_name" || true
	fi

	if [ -z "$reachable" ]; then
		local first_check="DNS for $address points at this server's public IP"
		if [ "$mode" = ip ]; then
			first_check="$address really is this server's public IP address"
		fi
		cat >&2 <<WARNING

WARNING: the gateway is running, but $url isn't reachable yet, so the worker
install command won't work until it is. Check that:
  - $first_check
  - ports 80 and 443 are open in any firewall or cloud security group, because
    Let's Encrypt checks this server through them before issuing a certificate
Then see: sudo journalctl -u caddy -n 50
Caddy keeps retrying the certificate, so you don't need to run this again.
WARNING
		exit 1
	fi

	# Stay to show pairing codes, which is all a new machine needs from here.
	if interactive && [ -z "${FLEET_NO_WAIT:-}" ]; then
		step "Waiting for a machine to pair. Its code will appear here. Press Ctrl-C to stop waiting; nothing is lost"
		"$ROOT/usr/local/bin/fleet-pair" --wait 30m --until-connected || true
	fi
	step "Done"
}

main "$@"
