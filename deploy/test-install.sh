#!/usr/bin/env bash
# Tests install-gateway.sh, add-worker.sh and install-worker.sh without changing
# this machine. System commands such as sudo, systemctl and launchctl are stubbed,
# and everything installs into a scratch directory. The credentials, gateway and
# worker connection are real.
#
# This can't prove Caddy, systemd, launchd or `say` work. Only a real install can.
#
#   ./deploy/test-install.sh
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
GATEWAY_PID=""
cleanup() {
	[ -n "$GATEWAY_PID" ] && kill "$GATEWAY_PID" 2>/dev/null
	for pidfile in "$WORK"/home/worker.pid "$WORK"/linux-root*/worker.pid "$WORK"/pair-root/worker.pid "$WORK"/skip-root/worker.pid "$WORK"/rel-root/worker.pid; do
		[ -f "$pidfile" ] && kill "$(cat "$pidfile")" 2>/dev/null
	done
	rm -rf "$WORK"
}
trap cleanup EXIT

FAILURES=0
pass() { echo "  ok: $1"; }
fail() {
	echo "  FAILED: $1" >&2
	FAILURES=$((FAILURES + 1))
}
check() { # description, command...
	local what="$1"
	shift
	if "$@" >/dev/null 2>&1; then pass "$what"; else fail "$what"; fi
}
refute() { # description, command...
	local what="$1"
	shift
	if "$@" >/dev/null 2>&1; then fail "$what"; else pass "$what"; fi
}

# --- Stubs for the server ---

STUBS="$WORK/stubs"
mkdir -p "$STUBS"
stub() {
	printf '#!/bin/sh\n%s\n' "$2" >"$STUBS/$1"
	chmod +x "$STUBS/$1"
}
# sudo runs the command as this user, so it can only reach the scratch directory.
stub sudo 'if [ "${1:-}" = "-u" ]; then shift 2; fi
if [ "$#" -eq 0 ] || [ "$1" = "true" ]; then exit 0; fi
exec "$@"'
stub systemctl 'echo "systemctl $*" >>"$STUB_LOG"'
stub useradd 'echo "useradd $*" >>"$STUB_LOG"'
stub chown 'exit 0'
stub caddy '[ "${1:-}" = version ] && echo "v${STUB_CADDY_VERSION:-2.11.9} h1:stub"
exit 0'
stub ss '[ -n "${STUB_PORT_HOLDER:-}" ] && echo "$STUB_PORT_HOLDER"
exit 0'
stub curl 'case "$*" in
*127.0.0.1:8080/healthz*) exit 0 ;;
*api.ipify.org* | *checkip.amazonaws.com* | *ifconfig.me*)
	echo "curl $*" >>"$STUB_LOG"
	[ -z "${STUB_NO_PUBLIC_IP:-}" ] || exit 6
	echo "${STUB_PUBLIC_IP:-203.0.113.10}"
	;;
*/healthz*) [ -z "${STUB_UNREACHABLE:-}" ] ;;
*releases/download/*)
	out=""
	url=""
	while [ $# -gt 0 ]; do
		case "$1" in
		-o) out="$2"; shift 2 ;;
		--retry) shift 2 ;;
		-*) shift ;;
		*) url="$1"; shift ;;
		esac
	done
	[ -f "$STUB_RELEASE/${url##*/}" ] || exit 22
	exec cp "$STUB_RELEASE/${url##*/}" "$out"
	;;
*) echo "unexpected curl: $*" >&2; exit 1 ;;
esac'

export STUB_LOG="$WORK/stub.log"
: >"$STUB_LOG"
SERVER_PATH="$STUBS:$PATH"

run_gateway_install() { # root, args...
	local root="$1"
	shift
	FLEET_INSTALL_ROOT="$root" PATH="$SERVER_PATH" "$REPO/deploy/install-gateway.sh" "$@"
}

# --- Gateway install ---

echo "First install"
ROOT="$WORK/root"
mkdir -p "$ROOT"
OUT="$WORK/first.out"
run_gateway_install "$ROOT" fleet.example.com mac-one >"$OUT" 2>&1
check "exits successfully" test $? -eq 0
check "prints an API key" grep -q 'FLEET_API_KEY=fk_' "$OUT"
check "prints the gateway URL" grep -q 'FLEET_URL=https://fleet.example.com' "$OUT"
check "prints the Mac command" grep -q 'curl -fsSL https://fleet.example.com/install/worker.sh | FLEET_NODE_NAME=mac-one FLEET_NODE_TOKEN=fn_' "$OUT"
check "installs the gateway binary" test -x "$ROOT/usr/local/bin/fleet-gateway"
check "installs the service" test -f "$ROOT/etc/systemd/system/fleet-gateway.service"
check "restarts the service" grep -q 'systemctl restart fleet-gateway' "$STUB_LOG"
check "restarts Caddy" grep -q 'systemctl restart caddy' "$STUB_LOG"
check "creates the service user" grep -q 'useradd --system' "$STUB_LOG"
check "keeps the data private" test "$(stat -c %a "$ROOT/var/lib/fleet")" = 700
check "publishes the installer with the real URL" grep -q 'GATEWAY_URL="${FLEET_GATEWAY_URL:-https://fleet.example.com}"' "$ROOT/srv/fleet/install/worker.sh"
refute "leaves no placeholder in the installer" grep -q '@@' "$ROOT/srv/fleet/install/worker.sh"
check "publishes the arm64 worker" test -f "$ROOT/srv/fleet/install/fleet-worker-darwin-arm64"
check "publishes the Intel worker" test -f "$ROOT/srv/fleet/install/fleet-worker-darwin-amd64"
check "publishes the Linux workers" test -f "$ROOT/srv/fleet/install/fleet-worker-linux-amd64" -a -f "$ROOT/srv/fleet/install/fleet-worker-linux-arm64"
check "the Linux worker really is a Linux program" sh -c "head -c 4 '$ROOT/srv/fleet/install/fleet-worker-linux-arm64' | grep -q ELF"
check "lists the Linux workers in the checksums" grep -q 'fleet-worker-linux-amd64$' "$ROOT/srv/fleet/install/SHA256SUMS"
check "publishes the Windows workers" test -f "$ROOT/srv/fleet/install/fleet-worker-windows-amd64.exe" -a -f "$ROOT/srv/fleet/install/fleet-worker-windows-arm64.exe"
check "lists the Windows workers in the checksums" grep -q 'fleet-worker-windows-arm64.exe$' "$ROOT/srv/fleet/install/SHA256SUMS"
check "the Windows worker really is a Windows program" sh -c "head -c 2 '$ROOT/srv/fleet/install/fleet-worker-windows-amd64.exe' | grep -q MZ"
check "publishes the Windows installer with the real URL" grep -q "gatewayUrl = 'https://fleet.example.com'" "$ROOT/srv/fleet/install/worker.ps1"
refute "leaves no placeholder in the Windows installer" grep -q '@@' "$ROOT/srv/fleet/install/worker.ps1"
refute "keeps the Windows installer plain ASCII, for Windows PowerShell" grep -qP '[^\x00-\x7F]' "$ROOT/srv/fleet/install/worker.ps1"
check "prints the Windows command" grep -q "FLEET_NODE_NAME = 'mac-one'; \$env:FLEET_NODE_TOKEN = 'fn_[0-9a-f]*'; irm https://fleet.example.com/install/worker.ps1 | iex" "$OUT"
check "publishes valid checksums" sh -c "cd '$ROOT/srv/fleet/install' && sha256sum -c SHA256SUMS"
refute "doesn't publish the database" sh -c "find '$ROOT/srv/fleet/install' -name '*.db' | grep ."
check "routes the domain in Caddy" grep -q '^fleet.example.com {' "$ROOT/etc/caddy/fleet.caddy"
check "serves the installer through Caddy" grep -q 'handle_path /install/\*' "$ROOT/etc/caddy/fleet.caddy"
check "imports the Caddy config" grep -qx 'import fleet.caddy' "$ROOT/etc/caddy/Caddyfile"

echo "Existing Caddy sites are kept"
ROOT2="$WORK/root2"
mkdir -p "$ROOT2/etc/caddy"
echo 'other.example.com { respond "hello" }' >"$ROOT2/etc/caddy/Caddyfile"
run_gateway_install "$ROOT2" fleet.example.com >/dev/null 2>&1
check "keeps the other site" grep -q 'other.example.com' "$ROOT2/etc/caddy/Caddyfile"
check "adds the import" grep -qx 'import fleet.caddy' "$ROOT2/etc/caddy/Caddyfile"

echo "Running it again"
OUT="$WORK/second.out"
run_gateway_install "$ROOT" fleet.example.com >"$OUT" 2>&1
check "exits successfully" test $? -eq 0
refute "doesn't print another API key" grep -q 'FLEET_API_KEY=fk_' "$OUT"
check "says the key was kept" grep -q 'already exists' "$OUT"
refute "doesn't register another node" grep -q 'FLEET_NODE_TOKEN' "$OUT"
check "doesn't repeat the Caddy import" test "$(grep -cx 'import fleet.caddy' "$ROOT/etc/caddy/Caddyfile")" = 1

echo "Running it again with a new node name"
OUT="$WORK/third.out"
run_gateway_install "$ROOT" fleet.example.com mac-three >"$OUT" 2>&1
check "registers the new node" grep -q 'FLEET_NODE_NAME=mac-three' "$OUT"

echo "Running it again with no address"
OUT="$WORK/no-address.out"
: >"$STUB_LOG"
run_gateway_install "$ROOT" >"$OUT" 2>&1
check "exits successfully" test $? -eq 0
check "keeps the domain" grep -q 'Keeping the address from the last install, fleet.example.com' "$OUT"
refute "doesn't look up a public IP" grep -q ipify "$STUB_LOG"
check "still serves the domain" grep -q '^fleet.example.com {' "$ROOT/etc/caddy/fleet.caddy"

echo "With no domain, on this server's IP address"
ROOT_IP="$WORK/root-ip"
mkdir -p "$ROOT_IP/etc/caddy"
# The Caddyfile Caddy's package installs.
printf '# The Caddyfile is an easy way to configure your Caddy web server.\n\n:80 {\n\troot * /usr/share/caddy\n\tfile_server\n}\n' >"$ROOT_IP/etc/caddy/Caddyfile"
OUT="$WORK/ip.out"
: >"$STUB_LOG"
run_gateway_install "$ROOT_IP" >"$OUT" 2>&1
RESULT=$?
check "installs" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "finds the public IP address" grep -q 'This server is at 203.0.113.10' "$OUT"
check "asks for an IPv4 address" grep -q 'curl -4 .*api.ipify.org' "$STUB_LOG"
check "prints the IP address as the gateway URL" grep -q 'FLEET_URL=https://203.0.113.10$' "$OUT"
check "prints the worker command for the IP address" grep -qx '  curl -fsSL https://203.0.113.10/install/worker.sh | sh' "$OUT"
refute "makes no token nobody asked for" grep -q 'FLEET_NODE_TOKEN' "$OUT"
check "prints the MCP address" grep -q 'Address:  https://203.0.113.10/mcp' "$OUT"
check "says where pairing codes appear" grep -q 'sudo fleet-pair' "$OUT"
check "installs fleet-pair" test -x "$ROOT_IP/usr/local/bin/fleet-pair"
check "fleet-pair runs" sh -c "FLEET_INSTALL_ROOT='$ROOT_IP' PATH='$SERVER_PATH' '$ROOT_IP/usr/local/bin/fleet-pair' --wait 0 | grep -q 'No machine is asking to pair'"
refute "doesn't wait for a machine when nobody is watching" grep -q 'Waiting for a machine' "$OUT"
check "prints the MCP command for the IP address" grep -q 'claude mcp add --transport http fleet https://203.0.113.10/mcp' "$OUT"
check "publishes the installer with the IP address" grep -q 'GATEWAY_URL="${FLEET_GATEWAY_URL:-https://203.0.113.10}"' "$ROOT_IP/srv/fleet/install/worker.sh"
check "serves the IP address in Caddy" grep -q '^203.0.113.10 {' "$ROOT_IP/etc/caddy/fleet.caddy"
check "asks Let's Encrypt for its short-lived profile" grep -q 'profile shortlived' "$ROOT_IP/etc/caddy/fleet.caddy"
check "tells Caddy which certificate to use without a server name" grep -qx '	default_sni 203.0.113.10 # fleet-gateway' "$ROOT_IP/etc/caddy/Caddyfile"
check "puts the global options first" test "$(grep -v '^#\|^$' "$ROOT_IP/etc/caddy/Caddyfile" | head -1)" = '{ # fleet-gateway'
check "keeps the package's site" grep -q 'root \* /usr/share/caddy' "$ROOT_IP/etc/caddy/Caddyfile"
check "imports the Caddy config" grep -qx 'import fleet.caddy' "$ROOT_IP/etc/caddy/Caddyfile"

OUT="$WORK/ip-again.out"
: >"$STUB_LOG"
run_gateway_install "$ROOT_IP" >"$OUT" 2>&1
check "runs again" test $? -eq 0
check "keeps the IP address" grep -q 'Keeping the address from the last install, 203.0.113.10' "$OUT"
check "adds default_sni once" test "$(grep -c default_sni "$ROOT_IP/etc/caddy/Caddyfile")" = 1
check "adds the global options once" test "$(grep -c '^{ # fleet-gateway$' "$ROOT_IP/etc/caddy/Caddyfile")" = 1

echo "Asking about a domain"
# The answers come from a file standing in for the terminal.
ROOT_ASK="$WORK/root-ask"
mkdir -p "$ROOT_ASK"
echo "" >"$WORK/answer-none"
OUT="$WORK/ask-none.out"
FLEET_INSTALL_TTY="$WORK/answer-none" FLEET_NO_WAIT=1 run_gateway_install "$ROOT_ASK" >"$OUT" 2>&1
check "installs" test $? -eq 0
check "asks whether there's a domain" grep -q 'Domain name for this server, if it has one (press Enter to use its IP address, 203.0.113.10)' "$OUT"
check "uses the IP address when the answer is empty" grep -q '^203.0.113.10 {' "$ROOT_ASK/etc/caddy/fleet.caddy"
ROOT_ASK2="$WORK/root-ask2"
mkdir -p "$ROOT_ASK2"
echo "Fleet.Example.org" >"$WORK/answer-domain"
FLEET_INSTALL_TTY="$WORK/answer-domain" FLEET_NO_WAIT=1 run_gateway_install "$ROOT_ASK2" >"$OUT" 2>&1
check "installs on the domain given, in lower case" grep -q '^fleet.example.org {' "$ROOT_ASK2/etc/caddy/fleet.caddy"
refute "doesn't treat a domain as an IP address" grep -q 'shortlived' "$ROOT_ASK2/etc/caddy/fleet.caddy"
ROOT_ASK3="$WORK/root-ask3"
mkdir -p "$ROOT_ASK3"
printf 'not a domain\nfleet.example.net\n' >"$WORK/answer-retry"
FLEET_INSTALL_TTY="$WORK/answer-retry" FLEET_NO_WAIT=1 run_gateway_install "$ROOT_ASK3" >"$OUT" 2>&1
check "asks again after an answer that isn't a domain" grep -q "doesn't look like a domain name" "$OUT"
check "then uses the domain" grep -q '^fleet.example.net {' "$ROOT_ASK3/etc/caddy/fleet.caddy"
OUT="$WORK/ask-wait.out"
: >"$STUB_LOG"
FLEET_INSTALL_TTY="$WORK/answer-none" FLEET_NO_WAIT=1 run_gateway_install "$ROOT_ASK" fleet.example.com >"$OUT" 2>&1
refute "doesn't ask when an address is given" grep -q 'Domain name for this server' "$OUT"

echo "Moving from the IP address to a domain"
OUT="$WORK/ip-to-domain.out"
run_gateway_install "$ROOT_IP" fleet.example.com >"$OUT" 2>&1
check "runs" test $? -eq 0
refute "removes what it added to the global options" grep -q 'fleet-gateway' "$ROOT_IP/etc/caddy/Caddyfile"
refute "drops the short-lived profile" grep -q 'shortlived' "$ROOT_IP/etc/caddy/fleet.caddy"
check "warns that installed workers use the old address" grep -q 'changed from https://203.0.113.10 to https://fleet.example.com' "$OUT"

echo "An IP address next to existing global options"
ROOT_GLOBAL="$WORK/root-global"
mkdir -p "$ROOT_GLOBAL/etc/caddy"
printf '{\n\temail ops@example.com\n}\n\nother.example.com {\n\trespond "hello"\n}\n' >"$ROOT_GLOBAL/etc/caddy/Caddyfile"
run_gateway_install "$ROOT_GLOBAL" 198.51.100.7 mac-ip >"$WORK/global.out" 2>&1
check "installs with an address given" test $? -eq 0
check "adds default_sni inside the existing global options" test "$(sed -n 2p "$ROOT_GLOBAL/etc/caddy/Caddyfile")" = '	default_sni 198.51.100.7 # fleet-gateway'
check "keeps the existing options" grep -q 'email ops@example.com' "$ROOT_GLOBAL/etc/caddy/Caddyfile"
check "doesn't add a second global block" test "$(grep -c '^{' "$ROOT_GLOBAL/etc/caddy/Caddyfile")" = 1
check "registers the named machine" grep -q 'FLEET_NODE_NAME=mac-ip' "$WORK/global.out"

echo "IP address refusals"
ROOT_REFUSE="$WORK/root-refuse"
mkdir -p "$ROOT_REFUSE/etc/caddy"
printf '{\n\tdefault_sni other.example.com\n}\n' >"$ROOT_REFUSE/etc/caddy/Caddyfile"
cp "$ROOT_REFUSE/etc/caddy/Caddyfile" "$WORK/clash.before"
run_gateway_install "$ROOT_REFUSE" 198.51.100.7 >"$WORK/clash.out" 2>&1
check "refuses when the Caddyfile sets its own default_sni" grep -q 'already sets default_sni' "$WORK/clash.out"
check "leaves that Caddyfile alone" cmp "$WORK/clash.before" "$ROOT_REFUSE/etc/caddy/Caddyfile"
refute "installs nothing then" test -e "$ROOT_REFUSE/usr/local/bin/fleet-gateway"
rm -f "$ROOT_REFUSE/etc/caddy/Caddyfile"
for private in 10.0.0.5 172.20.1.2 192.168.1.10 100.64.3.4 127.0.0.1; do
	run_gateway_install "$ROOT_REFUSE" "$private" >"$WORK/private.out" 2>&1
	check "refuses the private address $private" grep -q 'private address' "$WORK/private.out"
done
run_gateway_install "$ROOT_REFUSE" 2001:db8::1 >"$WORK/v6.out" 2>&1
check "refuses IPv6 for now" grep -q "IPv6 addresses aren't supported yet" "$WORK/v6.out"
run_gateway_install "$ROOT_REFUSE" 300.1.2.3 >/dev/null 2>&1
check "refuses an invalid address" test $? -ne 0
STUB_NO_PUBLIC_IP=1 run_gateway_install "$ROOT_REFUSE" >"$WORK/noip.out" 2>&1
check "stops if it can't find the public IP" grep -q "couldn't find this server's public IP address" "$WORK/noip.out"
STUB_PUBLIC_IP=10.1.2.3 run_gateway_install "$ROOT_REFUSE" >"$WORK/natip.out" 2>&1
check "refuses a private address it found" grep -q 'private address' "$WORK/natip.out"
refute "installs nothing when refusing an address" test -e "$ROOT_REFUSE/usr/local/bin/fleet-gateway"

echo "A Caddy config that doesn't validate is put back"
ROOT_BADCADDY="$WORK/root-badcaddy"
mkdir -p "$ROOT_BADCADDY/etc/caddy"
printf ':80 {\n\trespond "mine"\n}\n' >"$ROOT_BADCADDY/etc/caddy/Caddyfile"
cp "$ROOT_BADCADDY/etc/caddy/Caddyfile" "$WORK/badcaddy.before"
mkdir -p "$WORK/badcaddy-stubs"
printf '#!/bin/sh\n[ "${1:-}" = version ] && echo v2.11.9\n[ "${1:-}" = validate ] && { echo "Error: something is wrong" >&2; exit 1; }\nexit 0\n' >"$WORK/badcaddy-stubs/caddy"
chmod +x "$WORK/badcaddy-stubs/caddy"
: >"$STUB_LOG"
FLEET_INSTALL_ROOT="$ROOT_BADCADDY" PATH="$WORK/badcaddy-stubs:$SERVER_PATH" "$REPO/deploy/install-gateway.sh" 198.51.100.7 >"$WORK/badcaddy.out" 2>&1
check "stops" test $? -ne 0
check "says the old config was put back" grep -q 'the old one was put back' "$WORK/badcaddy.out"
check "shows Caddy's complaint" grep -q 'something is wrong' "$WORK/badcaddy.out"
check "restores the Caddyfile" cmp "$WORK/badcaddy.before" "$ROOT_BADCADDY/etc/caddy/Caddyfile"
refute "removes the new site file" test -e "$ROOT_BADCADDY/etc/caddy/fleet.caddy"
refute "doesn't restart Caddy" grep -q 'restart caddy' "$STUB_LOG"

echo "Server not reachable over HTTPS yet"
ROOT3="$WORK/root3"
mkdir -p "$ROOT3"
OUT="$WORK/unreachable.out"
# seq and sleep are stubbed too, so this doesn't wait out the real retries.
stub sleep 'exit 0'
STUB_UNREACHABLE=1 run_gateway_install "$ROOT3" fleet.example.com >"$OUT" 2>&1
check "exits with an error" test $? -ne 0
check "still prints the one-time API key" grep -q 'FLEET_API_KEY=fk_' "$OUT"
check "still prints the worker command" grep -q 'curl -fsSL https://fleet.example.com/install/worker.sh | sh' "$OUT"
check "explains what to check" grep -q 'security group' "$OUT"
rm "$STUBS/sleep"

echo "Refusals"
ROOT4="$WORK/root4"
mkdir -p "$ROOT4"
run_gateway_install "$ROOT4" "not a domain" >/dev/null 2>&1
check "rejects an invalid domain" test $? -ne 0
run_gateway_install "$ROOT4" fleet.example.com 'bad name;rm' >/dev/null 2>&1
check "rejects an invalid node name" test $? -ne 0
STUB_PORT_HOLDER='LISTEN 0 511 *:80 *:* users:(("nginx",pid=1,fd=6))' run_gateway_install "$ROOT4" fleet.example.com >"$WORK/port.out" 2>&1
check "refuses when another server holds a port" test $? -ne 0
check "names the conflict" grep -q 'nginx' "$WORK/port.out"
refute "changes nothing when refusing" test -e "$ROOT4/usr/local/bin/fleet-gateway"
STUB_PORT_HOLDER='LISTEN 0 511 *:80 *:* users:(("caddy",pid=1,fd=6))' run_gateway_install "$ROOT4" fleet.example.com >/dev/null 2>&1
check "accepts Caddy already holding the port" test $? -eq 0

echo "Installing from a release"
export FLEET_RELEASE_ALLOW_DIRTY=1
(cd "$REPO" && ./deploy/build-release.sh v9.9.9 >/dev/null 2>&1)
check "builds the release" test -f "$REPO/dist/release/SHA256SUMS"
RELEASE="$WORK/release"
mv "$REPO/dist/release" "$RELEASE"
rmdir "$REPO/dist" 2>/dev/null
export STUB_RELEASE="$RELEASE"
check "checksums cover the release" sh -c "cd '$RELEASE' && sha256sum -c SHA256SUMS"
for file in fleet-server-linux-amd64.tar.gz fleet-server-linux-arm64.tar.gz fleet-linux-amd64 fleet-linux-arm64 fleet-darwin-arm64 fleet-darwin-amd64 install-gateway.sh install-cli.sh install-worker.sh install-worker.ps1 fleet-worker-darwin-arm64 fleet-worker-darwin-amd64 fleet-worker-linux-amd64 fleet-worker-linux-arm64 fleet-worker-windows-amd64.exe fleet-worker-windows-arm64.exe; do
	check "includes $file" test -f "$RELEASE/$file"
done
check "fills in the download address" grep -q 'releases/download/v9.9.9' "$RELEASE/install-gateway.sh"
refute "leaves no placeholder" grep -q '@@RELEASE_URL@@' "$RELEASE/install-gateway.sh"
check "the worker installer knows where the release is" grep -q 'RELEASE_URL="${FLEET_RELEASE_URL:-https://github.com/.*/releases/download/v9.9.9}"' "$RELEASE/install-worker.sh"
check "the worker installer has no gateway yet" grep -q 'GATEWAY_URL="${FLEET_GATEWAY_URL:-@@GATEWAY_URL@@}"' "$RELEASE/install-worker.sh"
refute "keeps the Windows installer plain ASCII in the release" grep -qP '[^\x00-\x7F]' "$RELEASE/install-worker.ps1"
check "binaries report the version" sh -c "'$RELEASE/fleet-linux-amd64' version | grep -q v9.9.9"
mkdir -p "$WORK/bundle-check"
tar -xzf "$RELEASE/fleet-server-linux-amd64.tar.gz" -C "$WORK/bundle-check"
BUNDLE_DIR="$WORK/bundle-check/fleet-server-linux-amd64"
check "the bundle includes the gateway" test -x "$BUNDLE_DIR/fleet-gateway"
check "the bundle includes fleet-pair" test -x "$BUNDLE_DIR/pair.sh"
check "the bundled gateway reports the version" sh -c "'$BUNDLE_DIR/fleet-gateway' version | grep -q v9.9.9"
check "the bundle includes the Linux workers" test -x "$BUNDLE_DIR/fleet-worker-linux-amd64" -a -x "$BUNDLE_DIR/fleet-worker-linux-arm64"
check "the bundled Linux worker reports the version" sh -c "'$BUNDLE_DIR/fleet-worker-linux-amd64' --version | grep -q v9.9.9"

# Hide Go, to prove a release install doesn't need it.
NO_GO="$WORK/no-go"
mkdir -p "$NO_GO"
for tool in bash sh env id seq sleep grep sed awk tr mktemp rm cat cp mv mkdir dirname tar gzip sha256sum install touch tee uname cut head chmod stat; do
	ln -s "$(command -v "$tool")" "$NO_GO/$tool"
done
RELEASE_PATH="$STUBS:$NO_GO"

echo "From the piped one-liner"
ROOT5="$WORK/root5"
mkdir -p "$ROOT5"
OUT="$WORK/piped.out"
cat "$RELEASE/install-gateway.sh" | FLEET_INSTALL_ROOT="$ROOT5" PATH="$RELEASE_PATH" "$NO_GO/bash" -s -- fleet.example.com mac-piped >"$OUT" 2>&1
RESULT=$?
check "installs without Go" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "prints the API key" grep -q 'FLEET_API_KEY=fk_' "$OUT"
check "prints the Mac command" grep -q 'FLEET_NODE_NAME=mac-piped' "$OUT"
check "installs the release's gateway" sh -c "'$ROOT5/usr/local/bin/fleet-gateway' version | grep -q v9.9.9"
check "installs fleet-add-worker" test -x "$ROOT5/usr/local/bin/fleet-add-worker"
check "publishes the worker" test -f "$ROOT5/srv/fleet/install/fleet-worker-darwin-arm64"
check "installs fleet-pair" test -x "$ROOT5/usr/local/bin/fleet-pair"
check "publishes a worker installer that names the gateway and the release" sh -c "grep -q 'GATEWAY_URL=.*:-https://fleet.example.com}' '$ROOT5/srv/fleet/install/worker.sh' && grep -q 'RELEASE_URL=.*releases/download/v9.9.9}' '$ROOT5/srv/fleet/install/worker.sh'"

echo "From an unpacked bundle"
ROOT6="$WORK/root6"
mkdir -p "$ROOT6"
OUT="$WORK/bundle.out"
# With no release to download from, this can only pass by using the bundle itself.
STUB_RELEASE="$WORK/nowhere" FLEET_INSTALL_ROOT="$ROOT6" PATH="$RELEASE_PATH" "$NO_GO/bash" "$BUNDLE_DIR/install-gateway.sh" fleet.example.com mac-bundle >"$OUT" 2>&1
RESULT=$?
check "installs" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "prints the Mac command" grep -q 'FLEET_NODE_NAME=mac-bundle' "$OUT"

echo "On CentOS, with SELinux and firewalld"
# A separate set of stubs with no Caddy and no apt, so only dnf can provide it. The
# stubs show which commands run, in what order. Real dnf, SELinux and firewalld
# behaviour can only be proved by a real install.
RHEL="$WORK/rhel"
cp -r "$STUBS" "$RHEL"
rm "$RHEL/caddy"
rhelstub() {
	printf '#!/bin/sh\n%s\n' "$2" >"$RHEL/$1"
	chmod +x "$RHEL/$1"
}
rhelstub dnf 'echo "dnf $*" >>"$STUB_LOG"
[ -z "${STUB_DNF_FAIL:-}" ] || exit 1
case "$*" in
*"copr enable -y -q @caddy/caddy") [ -z "${STUB_COPR_NEEDS_EPEL:-}" ] || exit 1 ;;
*"-q caddy") printf "#!/bin/sh\n[ \"\${1:-}\" = version ] && echo v${STUB_NEW_CADDY:-2.11.7}\nexit 0\n" >"$(dirname "$0")/caddy"; chmod +x "$(dirname "$0")/caddy" ;;
esac'
rhelstub getenforce 'echo "${STUB_SELINUX:-Enforcing}"'
rhelstub setsebool 'echo "setsebool $*" >>"$STUB_LOG"'
rhelstub semanage 'echo "semanage $*" >>"$STUB_LOG"
case "$*" in *" -a "*) [ -z "${STUB_LABEL_EXISTS:-}" ] ;; esac'
rhelstub restorecon 'echo "restorecon $*" >>"$STUB_LOG"'
rhelstub chcon 'echo "chcon $*" >>"$STUB_LOG"'
rhelstub firewall-cmd 'case "$*" in
--state) echo "${STUB_FIREWALLD:-running}" ;;
*) echo "firewall-cmd $*" >>"$STUB_LOG" ;;
esac'
# The real systemctl cat is how the script checks that a caddy service exists.
rhelstub systemctl 'if [ "$1" = cat ] && [ -n "${STUB_NO_CADDY_SERVICE:-}" ]; then exit 1; fi
echo "systemctl $*" >>"$STUB_LOG"'
run_centos_install() { # root; environment assignments come before this call.
	FLEET_INSTALL_ROOT="$1" PATH="$RHEL:$NO_GO" "$NO_GO/bash" "$BUNDLE_DIR/install-gateway.sh" fleet.example.com centos-mac
}
position() { grep -n -- "$1" "$STUB_LOG" | head -1 | cut -d: -f1; }

CENTOS8="$WORK/centos-root8"
mkdir -p "$CENTOS8"
OUT="$WORK/centos.out"
: >"$STUB_LOG"
run_centos_install "$CENTOS8" >"$OUT" 2>&1
RESULT=$?
check "installs" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "prints the API key" grep -q 'FLEET_API_KEY=fk_' "$OUT"
check "prints the Mac command" grep -q 'FLEET_NODE_NAME=centos-mac' "$OUT"
check "installs the copr plugin Caddy documents" grep -q 'dnf install -y -q dnf-plugins-core' "$STUB_LOG"
check "enables Caddy's repository" grep -q 'dnf copr enable -y -q @caddy/caddy' "$STUB_LOG"
check "installs Caddy" grep -q 'dnf install -y -q caddy' "$STUB_LOG"
check "installs Caddy before touching the gateway" test "$(position 'dnf install -y -q caddy')" -lt "$(position 'restart fleet-gateway')"
check "lets Caddy connect to the gateway" grep -q 'setsebool -P httpd_can_network_connect on' "$STUB_LOG"
check "labels the published files as web content" grep -q "semanage fcontext -a -t httpd_sys_content_t /srv/fleet(/.\*)?" "$STUB_LOG"
check "applies that label" grep -q "restorecon -R $CENTOS8/srv/fleet" "$STUB_LOG"
check "applies SELinux settings before starting Caddy" test "$(position setsebool)" -lt "$(position 'restart caddy')"
check "relabels the installed gateway before starting it" test "$(position "restorecon $CENTOS8/usr/local/bin/fleet-gateway")" -lt "$(position 'restart fleet-gateway')"
check "opens firewalld now" grep -qx 'firewall-cmd --quiet --add-service=http --add-service=https' "$STUB_LOG"
check "opens firewalld after a reboot" grep -qx 'firewall-cmd --quiet --permanent --add-service=http --add-service=https' "$STUB_LOG"
refute "doesn't reload firewalld, which would drop other temporary rules" grep -q 'reload' <(grep firewall-cmd "$STUB_LOG")
rm -f "$RHEL/caddy"

: >"$STUB_LOG"
CENTOS9="$WORK/centos-root9"
mkdir -p "$CENTOS9"
STUB_SELINUX=Disabled STUB_FIREWALLD="not running" run_centos_install "$CENTOS9" >/dev/null 2>&1
check "installs with SELinux and firewalld off" test $? -eq 0
refute "leaves SELinux alone when it's off" grep -qE 'setsebool|semanage|restorecon|chcon' "$STUB_LOG"
refute "leaves firewalld alone when it's off" grep -q 'firewall-cmd' "$STUB_LOG"
rm -f "$RHEL/caddy"

: >"$STUB_LOG"
CENTOS10="$WORK/centos-root10"
mkdir -p "$CENTOS10"
mv "$RHEL/semanage" "$WORK/semanage.saved"
run_centos_install "$CENTOS10" >/dev/null 2>&1
check "installs without semanage" test $? -eq 0
check "labels the published files with chcon instead" grep -q "chcon -R -t httpd_sys_content_t $CENTOS10/srv/fleet" "$STUB_LOG"
mv "$WORK/semanage.saved" "$RHEL/semanage"
rm -f "$RHEL/caddy"

: >"$STUB_LOG"
CENTOS11="$WORK/centos-root11"
mkdir -p "$CENTOS11"
STUB_LABEL_EXISTS=1 run_centos_install "$CENTOS11" >/dev/null 2>&1
check "updates a label that already exists" grep -q 'semanage fcontext -m -t httpd_sys_content_t' "$STUB_LOG"
rm -f "$RHEL/caddy"

CENTOS12="$WORK/centos-root12"
mkdir -p "$CENTOS12"
OUT="$WORK/centos-fail.out"
STUB_DNF_FAIL=1 run_centos_install "$CENTOS12" >"$OUT" 2>&1
check "stops if Caddy can't be installed" test $? -ne 0
check "explains retired repositories" grep -q 'retired' "$OUT"
refute "changes nothing if Caddy can't be installed" test -e "$CENTOS12/usr/local/bin/fleet-gateway"

CENTOS9="$WORK/centos9-root"
mkdir -p "$CENTOS9/etc"
printf 'ID="centos"\nVERSION_ID="9"\n' >"$CENTOS9/etc/os-release"
rm -f "$RHEL/caddy"
: >"$STUB_LOG"
OUT="$WORK/centos9.out"
STUB_COPR_NEEDS_EPEL=1 run_centos_install "$CENTOS9" >"$OUT" 2>&1
check "installs on CentOS 9 when dnf can't choose Caddy's repository" test $? -eq 0
check "falls back to the EPEL 9 build of Caddy" grep -q "dnf copr enable -y -q @caddy/caddy epel-9-$(uname -m)" "$STUB_LOG"
rm -f "$RHEL/caddy"

mv "$RHEL/dnf" "$WORK/dnf.saved"
OUT="$WORK/centos7.out"
run_centos_install "$CENTOS12" >"$OUT" 2>&1
check "stops on a system with no dnf or apt" test $? -ne 0
check "mentions CentOS 7" grep -q 'CentOS 7' "$OUT"
refute "changes nothing with no dnf or apt" test -e "$CENTOS12/usr/local/bin/fleet-gateway"
mv "$WORK/dnf.saved" "$RHEL/dnf"

printf '#!/bin/sh\nexit 0\n' >"$RHEL/caddy"
chmod +x "$RHEL/caddy"
OUT="$WORK/no-service.out"
STUB_NO_CADDY_SERVICE=1 run_centos_install "$CENTOS12" >"$OUT" 2>&1
check "stops if Caddy has no service" test $? -ne 0
check "explains the missing service" grep -q 'no caddy systemd service' "$OUT"
refute "changes nothing if Caddy has no service" test -e "$CENTOS12/usr/local/bin/fleet-gateway"
rm -f "$RHEL/caddy"

echo "An IP address on CentOS with an older Caddy"
# Caddy is already installed, but too old for an IP address certificate.
printf '#!/bin/sh\n[ "${1:-}" = version ] && echo v2.10.2\nexit 0\n' >"$RHEL/caddy"
chmod +x "$RHEL/caddy"
CENTOS_IP="$WORK/centos-ip"
mkdir -p "$CENTOS_IP"
: >"$STUB_LOG"
OUT="$WORK/centos-ip.out"
FLEET_INSTALL_ROOT="$CENTOS_IP" PATH="$RHEL:$NO_GO" "$NO_GO/bash" "$BUNDLE_DIR/install-gateway.sh" 198.51.100.7 >"$OUT" 2>&1
RESULT=$?
check "installs" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "says why it upgrades Caddy" grep -q 'needs Caddy 2.11.4 or newer' "$OUT"
check "upgrades Caddy" grep -q 'dnf upgrade -y -q caddy' "$STUB_LOG"
check "upgrades Caddy before touching the gateway" test "$(position 'dnf upgrade -y -q caddy')" -lt "$(position 'restart fleet-gateway')"
check "serves the IP address" grep -q '^198.51.100.7 {' "$CENTOS_IP/etc/caddy/fleet.caddy"

printf '#!/bin/sh\n[ "${1:-}" = version ] && echo v2.10.2\nexit 0\n' >"$RHEL/caddy"
chmod +x "$RHEL/caddy"
CENTOS_OLD="$WORK/centos-old"
mkdir -p "$CENTOS_OLD"
OUT="$WORK/centos-old.out"
STUB_NEW_CADDY=2.11.3 FLEET_INSTALL_ROOT="$CENTOS_OLD" PATH="$RHEL:$NO_GO" "$NO_GO/bash" "$BUNDLE_DIR/install-gateway.sh" 198.51.100.7 >"$OUT" 2>&1
check "stops if Caddy can't be upgraded far enough" test $? -ne 0
check "names the version it found" grep -q 'Caddy is v2.11.3' "$OUT"
refute "changes nothing then" test -e "$CENTOS_OLD/usr/local/bin/fleet-gateway"
rm -f "$RHEL/caddy"

# Some sudo setups can't find Caddy by name, so the script must use its full path.
mkdir -p "$WORK/caddy-elsewhere"
printf '#!/bin/sh\necho "caddy $*" >>"$STUB_LOG"\n' >"$WORK/caddy-elsewhere/caddy"
chmod +x "$WORK/caddy-elsewhere/caddy"
: >"$STUB_LOG"
CENTOS13="$WORK/centos-root13"
mkdir -p "$CENTOS13"
rhelstub sudo 'if [ "${1:-}" = "-u" ]; then shift 2; fi
if [ "$#" -eq 0 ] || [ "$1" = "true" ]; then exit 0; fi
if [ "$1" = caddy ]; then echo "sudo: caddy: command not found" >&2; exit 1; fi
exec "$@"'
FLEET_INSTALL_ROOT="$CENTOS13" PATH="$RHEL:$WORK/caddy-elsewhere:$NO_GO" "$NO_GO/bash" "$BUNDLE_DIR/install-gateway.sh" fleet.example.com centos-mac >/dev/null 2>&1
check "finds Caddy where sudo doesn't look" test $? -eq 0
check "runs Caddy by its full path" grep -q 'caddy validate' "$STUB_LOG"

echo "A bad download is refused"
ROOT7="$WORK/root7"
mkdir -p "$ROOT7"
TAMPERED="$WORK/tampered"
cp -r "$RELEASE" "$TAMPERED"
echo tampered >>"$TAMPERED/fleet-server-linux-amd64.tar.gz"
OUT="$WORK/tampered.out"
cat "$RELEASE/install-gateway.sh" | STUB_RELEASE="$TAMPERED" FLEET_INSTALL_ROOT="$ROOT7" PATH="$RELEASE_PATH" "$NO_GO/bash" -s -- fleet.example.com >"$OUT" 2>&1
check "refuses a tampered bundle" test $? -ne 0
check "says why" grep -q "doesn't match its checksum" "$OUT"
refute "installs nothing" test -e "$ROOT7/usr/local/bin/fleet-gateway"
OUT="$WORK/missing.out"
cat "$RELEASE/install-gateway.sh" | STUB_RELEASE="$WORK/nowhere" FLEET_INSTALL_ROOT="$ROOT7" PATH="$RELEASE_PATH" "$NO_GO/bash" -s -- fleet.example.com >"$OUT" 2>&1
check "fails when the download is unavailable" test $? -ne 0
check "explains the private repo case" grep -q 'private' "$OUT"

echo "A cut-off gateway script does nothing"
SIZE="$(wc -c <"$RELEASE/install-gateway.sh")"
MAIN_AT="$(grep -bn '^main() {' "$RELEASE/install-gateway.sh" | head -1 | cut -d: -f2)"
ROOT8="$WORK/root8"
mkdir -p "$ROOT8"
ran=""
for cut in $(seq "$MAIN_AT" 397 "$((SIZE - 15))"); do
	head -c "$cut" "$RELEASE/install-gateway.sh" | FLEET_INSTALL_ROOT="$ROOT8" PATH="$RELEASE_PATH" "$NO_GO/bash" -s -- fleet.example.com >/dev/null 2>&1
	if [ -e "$ROOT8/usr/local/bin/fleet-gateway" ]; then
		ran="$cut"
		break
	fi
done
if [ -z "$ran" ]; then pass "partial scripts don't run"; else fail "a script cut at byte $ran still ran"; fi

echo "install-cli.sh"
CLI_HOME="$WORK/cli-home"
mkdir -p "$CLI_HOME"
OUT="$WORK/cli.out"
cat "$RELEASE/install-cli.sh" | HOME="$CLI_HOME" PATH="$STUBS:$NO_GO" "$NO_GO/sh" >"$OUT" 2>&1
RESULT=$?
check "installs" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "the installed command runs" sh -c "'$CLI_HOME/.local/bin/fleet' version | grep -q v9.9.9"
check "explains PATH" grep -q 'export PATH' "$OUT"
cat "$RELEASE/install-cli.sh" | STUB_RELEASE="$TAMPERED" HOME="$WORK/cli-home2" PATH="$STUBS:$NO_GO" "$NO_GO/sh" >/dev/null 2>&1
check "accepts an untampered binary from a tampered bundle's release" test $? -eq 0
echo tampered >>"$TAMPERED/fleet-linux-amd64"
cat "$RELEASE/install-cli.sh" | STUB_RELEASE="$TAMPERED" HOME="$WORK/cli-home3" PATH="$STUBS:$NO_GO" "$NO_GO/sh" >/dev/null 2>&1
check "refuses a tampered binary" test $? -ne 0
refute "installs nothing when refusing" test -e "$WORK/cli-home3/.local/bin/fleet"

echo "add-worker.sh"
OUT="$WORK/add.out"
FLEET_INSTALL_ROOT="$ROOT" PATH="$SERVER_PATH" "$REPO/deploy/add-worker.sh" mac-four >"$OUT" 2>&1
check "prints the Mac command" grep -q 'FLEET_NODE_NAME=mac-four' "$OUT"
check "prints the token by itself, for the worker's page" grep -qx '  fn_[0-9a-f]*' "$OUT"
check "says the command is for Linux too" grep -q 'On a Mac or a Linux machine' "$OUT"
FLEET_INSTALL_ROOT="$ROOT" PATH="$SERVER_PATH" "$REPO/deploy/add-worker.sh" mac-four >"$OUT" 2>&1
check "refuses a name already in use" test $? -ne 0
check "explains the refusal" grep -q 'already registered' "$OUT"
FLEET_INSTALL_ROOT="$ROOT" PATH="$SERVER_PATH" "$REPO/deploy/add-worker.sh" 'bad name' >/dev/null 2>&1
check "rejects an invalid name" test $? -ne 0
FLEET_INSTALL_ROOT="$WORK/nowhere" PATH="$SERVER_PATH" "$REPO/deploy/add-worker.sh" mac-five >/dev/null 2>&1
check "refuses when the gateway isn't installed" test $? -ne 0

# --- Worker install, against a real gateway ---

echo "Worker install"
GATEWAY_PORT=18947
"$ROOT/usr/local/bin/fleet-gateway" serve --listen "127.0.0.1:$GATEWAY_PORT" --data "$ROOT/var/lib/fleet" --heartbeat 1s >"$WORK/gateway.log" 2>&1 &
GATEWAY_PID=$!

MAC_HOME="$WORK/home"
MAC_STUBS="$WORK/mac-stubs"
mkdir -p "$MAC_HOME" "$MAC_STUBS"
macstub() {
	printf '#!/bin/sh\n%s\n' "$2" >"$MAC_STUBS/$1"
	chmod +x "$MAC_STUBS/$1"
}
macstub uname 'case "$1" in -s) echo Darwin ;; -m) echo "${STUB_ARCH:-arm64}" ;; esac'
macstub shasum 'shift 2; exec sha256sum "$@"'
macstub sw_vers 'echo "${STUB_MACOS:-14.5}"'
macstub curl "out=''
url=''
while [ \$# -gt 0 ]; do
	case \"\$1\" in
	-o) out=\"\$2\"; shift 2 ;;
	--retry) shift 2 ;;
	-*) shift ;;
	*) url=\"\$1\"; shift ;;
	esac
done
exec cp \"\$PUBLISHED/\${url##*/}\" \"\$out\""
macstub launchctl 'case "$1" in
bootstrap)
	echo "launchctl bootstrap $2" >>"$STUB_LOG"
	"$HOME/fleet/fleet-worker" --config "$HOME/fleet/worker.yaml" >>"$HOME/fleet/worker.log" 2>&1 &
	echo $! >"$HOME/worker.pid"
	;;
bootout)
	echo "launchctl bootout $2" >>"$STUB_LOG"
	if [ -f "$HOME/worker.pid" ]; then kill "$(cat "$HOME/worker.pid")" 2>/dev/null; rm -f "$HOME/worker.pid"; fi
	exit 1
	;;
esac'

# The published binaries are for macOS, so this stands in for one here. It runs
# the real Linux worker on the generated manifest, changing only what can't work
# on this machine: the gateway address, and the `say` program that doesn't exist.
export REAL_WORKER="$WORK/fleet-worker-linux"
export TEST_GATEWAY="http://127.0.0.1:$GATEWAY_PORT"
(cd "$REPO" && go build -o "$REAL_WORKER" ./cmd/fleet-worker && go build -o "$WORK/fleet" ./cmd/fleet)
export PUBLISHED="$WORK/published"
mkdir -p "$PUBLISHED"
cp "$ROOT/srv/fleet/install/worker.sh" "$PUBLISHED/"
cat >"$PUBLISHED/fleet-worker-darwin-arm64" <<'WRAPPER'
#!/bin/sh
case "$1" in
ui) echo "worker $*" >>"$STUB_LOG"; exit 0 ;;
pair) exec "$REAL_WORKER" "$@" ;;
esac
config="$2"
linux="$(dirname "$config")/worker.linux.yaml"
sed -e "s|^gateway_url: \"https://.*|gateway_url: $TEST_GATEWAY|" -e "s|/usr/bin/say|/bin/cat|" -e '$a ui_listen: off' "$config" >"$linux"
exec "$REAL_WORKER" --config "$linux" $3
WRAPPER
cp "$PUBLISHED/fleet-worker-darwin-arm64" "$PUBLISHED/fleet-worker-darwin-amd64"
(cd "$PUBLISHED" && sha256sum fleet-worker-darwin-arm64 fleet-worker-darwin-amd64 >SHA256SUMS)

TOKEN="$(sed -n 's/.*FLEET_NODE_TOKEN=\(fn_[0-9a-f]*\).*/\1/p' "$WORK/add.out" | head -1)"
if [ -z "$TOKEN" ]; then
	FLEET_INSTALL_ROOT="$ROOT" PATH="$SERVER_PATH" "$REPO/deploy/add-worker.sh" mac-worker >"$WORK/add2.out" 2>&1
	TOKEN="$(sed -n 's/.*FLEET_NODE_TOKEN=\(fn_[0-9a-f]*\).*/\1/p' "$WORK/add2.out" | head -1)"
fi
check "got a real node token" test -n "$TOKEN"

run_worker_install() { # Environment assignments come before this call.
	HOME="$MAC_HOME" PATH="$MAC_STUBS:$PATH" sh "$PUBLISHED/worker.sh"
}

OUT="$WORK/worker.out"
FLEET_NODE_NAME='bad;name' FLEET_NODE_TOKEN=fn_x run_worker_install >"$OUT" 2>&1
check "refuses an invalid node name" test $? -ne 0
FLEET_NODE_TOKEN='fn_x; rm -rf /' run_worker_install >"$OUT" 2>&1
check "refuses a token that isn't one" test $? -ne 0
refute "changes nothing when refusing" test -e "$MAC_HOME/fleet"
FLEET_NODE_NAME=mac-worker FLEET_NODE_TOKEN=fn_x STUB_ARCH=sparc run_worker_install >"$OUT" 2>&1
check "refuses an unknown chip" test $? -ne 0
rm -rf "$MAC_HOME/fleet"
FLEET_NODE_NAME=mac-worker FLEET_NODE_TOKEN=fn_x STUB_MACOS=12.7.6 run_worker_install >"$OUT" 2>&1
check "refuses a macOS too old for the worker" test $? -ne 0
check "names the macOS it needs" grep -q 'needs macOS 13 or newer, and this Mac has macOS 12.7.6' "$OUT"
refute "changes nothing on an old macOS" test -e "$MAC_HOME/fleet"
FLEET_NODE_NAME=mac-worker FLEET_NODE_TOKEN=fn_x STUB_MACOS=unknown run_worker_install >"$OUT" 2>&1
check "refuses an unreadable macOS version" test $? -ne 0

# The name must match the token, so use the name this token was issued for.
NODE="$(sed -n 's/.*FLEET_NODE_NAME=\([^ ]*\).*/\1/p' "$WORK/add.out" | head -1)"
[ -n "$NODE" ] || NODE=mac-worker
rm -rf "$MAC_HOME/fleet"
FLEET_NODE_NAME="$NODE" FLEET_NODE_TOKEN="$TOKEN" run_worker_install >"$OUT" 2>&1
RESULT=$?
check "installs and connects" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "reports the connection" grep -q 'Connected to https://fleet.example.com' "$OUT"
check "prints the power settings to apply" grep -q 'sudo pmset' "$OUT"
check "opens the worker's page" grep -q "worker ui --config $MAC_HOME/fleet/worker.yaml --wait 15s" "$STUB_LOG"
check "points to the MCP test" grep -q 'reach this machine.s tools through MCP' "$OUT"
check "says how to open the page again" grep -q "fleet-worker ui --config $MAC_HOME/fleet/worker.yaml" "$OUT"
check "explains how to watch the log" grep -q 'tail -F .*worker.log' "$OUT"
check "gives the Mac a capability test" grep -q 'test:' "$MAC_HOME/fleet/worker.yaml"
check "saves the token privately" test "$(stat -c %a "$MAC_HOME/fleet/token")" = 600
check "writes a valid plist" python3 -c "
import plistlib, sys
p = plistlib.load(open('$MAC_HOME/Library/LaunchAgents/com.fleet.worker.plist', 'rb'))
assert p['Label'] == 'com.fleet.worker'
assert p['ProgramArguments'][0] == '$MAC_HOME/fleet/fleet-worker'
assert p['KeepAlive'] is True
assert p['ProgramArguments'][3:] == ['--log-file', '$MAC_HOME/fleet/worker.log']
assert p['StandardErrorPath'] == '$MAC_HOME/fleet/worker.log.crash'
"
export FLEET_URL="http://127.0.0.1:$GATEWAY_PORT"
export FLEET_API_KEY="$(sed -n 's/.*FLEET_API_KEY=\(fk_[0-9a-f]*\).*/\1/p' "$WORK/first.out")"
check "the capability is online at the gateway" sh -c "'$WORK/fleet' capabilities | grep -q '\"online_nodes\": 1'"
check "the published manifest offers tts.say" sh -c "'$WORK/fleet' capabilities | grep -q 'tts.say'"

echo "Upgrading the worker"
echo '# my edit' >>"$MAC_HOME/fleet/worker.yaml"
TOKEN_BEFORE="$(cat "$MAC_HOME/fleet/token")"
OUT="$WORK/upgrade.out"
run_worker_install >"$OUT" 2>&1
RESULT=$?
check "upgrades without a name or token" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "keeps the edited manifest" grep -q '# my edit' "$MAC_HOME/fleet/worker.yaml"
check "keeps the token" test "$(cat "$MAC_HOME/fleet/token")" = "$TOKEN_BEFORE"
check "reconnects" sh -c "'$WORK/fleet' capabilities | grep -q '\"online_nodes\": 1'"

echo "A bad manifest leaves the running worker alone"
PID_BEFORE="$(cat "$MAC_HOME/worker.pid")"
cp "$MAC_HOME/fleet/worker.yaml" "$WORK/good.yaml"
echo 'nonsense_field: true' >>"$MAC_HOME/fleet/worker.yaml"
run_worker_install >"$OUT" 2>&1
check "refuses to install" test $? -ne 0
check "doesn't stop the worker" kill -0 "$PID_BEFORE"
cp "$WORK/good.yaml" "$MAC_HOME/fleet/worker.yaml"

echo "A tampered download is refused"
PID_BEFORE="$(cat "$MAC_HOME/worker.pid")"
cp "$PUBLISHED/fleet-worker-darwin-arm64" "$WORK/good-binary"
echo '# tampered' >>"$PUBLISHED/fleet-worker-darwin-arm64"
run_worker_install >"$OUT" 2>&1
check "refuses to install" test $? -ne 0
check "says why" grep -q "doesn't match its checksum" "$OUT"
check "doesn't stop the worker" kill -0 "$PID_BEFORE"
check "doesn't replace the binary" cmp "$PUBLISHED/fleet-worker-darwin-amd64" "$MAC_HOME/fleet/fleet-worker"
cp "$WORK/good-binary" "$PUBLISHED/fleet-worker-darwin-arm64"

echo "Interrupted download"
# Try every cut point that lands inside main, where the work is done. A partial
# script must not run any of it.
SIZE="$(wc -c <"$PUBLISHED/worker.sh")"
MAIN_AT="$(grep -bn '^main() {' "$PUBLISHED/worker.sh" | head -1 | cut -d: -f2)"
ran=""
for cut in $(seq "$MAIN_AT" 97 "$((SIZE - 15))"); do
	head -c "$cut" "$PUBLISHED/worker.sh" >"$WORK/truncated.sh"
	HOME="$WORK/truncated-home" PATH="$MAC_STUBS:$PATH" FLEET_NODE_NAME=x FLEET_NODE_TOKEN=y sh "$WORK/truncated.sh" >/dev/null 2>&1
	if [ -e "$WORK/truncated-home/fleet" ]; then
		ran="$cut"
		break
	fi
done
if [ -z "$ran" ]; then pass "a cut-off script does nothing"; else fail "a script cut at byte $ran still ran"; fi


# --- Linux worker install, against the same gateway ---

echo "Linux worker install"
# systemd, sudo and runuser are stubbed. systemctl restart really starts the
# worker from the ExecStart line of the unit the installer wrote, so the unit's
# command is tested; systemd's handling of the rest of the unit isn't.
LINUX_STUBS="$WORK/linux-stubs"
mkdir -p "$LINUX_STUBS"
linuxstub() {
	printf '#!/bin/sh\n%s\n' "$2" >"$LINUX_STUBS/$1"
	chmod +x "$LINUX_STUBS/$1"
}
linuxstub uname 'case "$1" in -s) echo Linux ;; -m) echo "${STUB_ARCH:-x86_64}" ;; *) exec /bin/uname "$@" ;; esac'
linuxstub sudo 'echo "sudo $*" >>"$STUB_LOG"
[ -z "${STUB_NO_SUDO:-}" ] || exit 1
exec "$@"'
linuxstub runuser 'echo "runuser $*" >>"$STUB_LOG"
shift 3
exec "$@"'
linuxstub getenforce 'echo "${STUB_SELINUX:-Disabled}"'
linuxstub restorecon 'echo "restorecon $*" >>"$STUB_LOG"'
linuxstub systemctl 'echo "systemctl $*" >>"$STUB_LOG"
case "$1" in
restart)
	pidfile="$FLEET_INSTALL_ROOT/worker.pid"
	if [ -f "$pidfile" ]; then kill "$(cat "$pidfile")" 2>/dev/null; sleep 1; fi
	unit="$FLEET_INSTALL_ROOT/etc/systemd/system/fleet-worker.service"
	command="$(sed -n "s/^ExecStart=//p" "$unit")"
	dir="$(sed -n "s/^WorkingDirectory=//p" "$unit")"
	(cd "$dir" && exec $command >/dev/null 2>&1) &
	echo $! >"$pidfile"
	;;
esac'
cp "$MAC_STUBS/curl" "$LINUX_STUBS/curl"

# The real Linux worker, changing only the gateway address, which must be https
# in a manifest but is plain http here.
cat >"$PUBLISHED/fleet-worker-linux-amd64" <<'WRAPPER'
#!/bin/sh
case "$1" in
ui) echo "worker $*" >>"$STUB_LOG"; exit 0 ;;
pair) exec "$REAL_WORKER" "$@" ;;
esac
config="$2"
shift 2
linux="$(dirname "$config")/worker.test.yaml"
sed -e "s|^gateway_url: \"https://.*|gateway_url: $TEST_GATEWAY|" -e '$a ui_listen: off' "$config" >"$linux"
exec "$REAL_WORKER" --config "$linux" "$@"
WRAPPER
cp "$PUBLISHED/fleet-worker-linux-amd64" "$PUBLISHED/fleet-worker-linux-arm64"
cp "$ROOT/srv/fleet/install/worker.sh" "$PUBLISHED/worker.sh"
(cd "$PUBLISHED" && sha256sum fleet-worker-darwin-arm64 fleet-worker-darwin-amd64 fleet-worker-linux-amd64 fleet-worker-linux-arm64 >SHA256SUMS)

online_on() { # capability, machines it should be online on
	"$WORK/fleet" capabilities | python3 -c "
import json, sys
data = json.load(sys.stdin)
caps = data['capabilities'] if isinstance(data, dict) else data
sys.exit(0 if any(c['name'] == '$1' and c['online_nodes'] == $2 for c in caps) else 1)"
}
new_node() { # name; prints its token
	FLEET_INSTALL_ROOT="$ROOT" PATH="$SERVER_PATH" "$REPO/deploy/add-worker.sh" "$1" 2>/dev/null |
		sed -n 's/.*FLEET_NODE_TOKEN=\(fn_[0-9a-f]*\).*/\1/p' | head -1
}
run_linux_install() { # root, home; environment assignments come before this call.
	FLEET_INSTALL_ROOT="$1" HOME="$2" PATH="$LINUX_STUBS:$PATH" sh "$PUBLISHED/worker.sh"
}

LINUX_ROOT="$WORK/linux-root"
LINUX_HOME="$WORK/linux-home"
mkdir -p "$LINUX_ROOT/run/systemd/system" "$LINUX_HOME"
LINUX_TOKEN="$(new_node linux-one)"
check "got a token for the Linux machine" test -n "$LINUX_TOKEN"
ME="$(id -un)"

OUT="$WORK/linux.out"
: >"$STUB_LOG"
FLEET_NODE_NAME=linux-one FLEET_NODE_TOKEN="$LINUX_TOKEN" run_linux_install "$LINUX_ROOT" "$LINUX_HOME" >"$OUT" 2>&1
RESULT=$?
check "installs and connects" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "reports the connection" grep -q 'Connected to https://fleet.example.com' "$OUT"
check "says which account it runs as" grep -q "running as $ME" "$OUT"
check "explains how to reach the configuration page" grep -q 'ssh -L 7878:127.0.0.1:7878' "$OUT"
check "asks for sudo before changing anything" test "$(grep -n 'sudo true' "$STUB_LOG" | head -1 | cut -d: -f1)" = 1
check "installs the binary outside the home folder" test -x "$LINUX_ROOT/usr/local/bin/fleet-worker"
refute "leaves no half-installed binary" test -e "$LINUX_ROOT/usr/local/bin/fleet-worker.new"
check "links it from ~/fleet, as on a Mac" test "$(readlink "$LINUX_HOME/fleet/fleet-worker")" = "$LINUX_ROOT/usr/local/bin/fleet-worker"
UNIT="$LINUX_ROOT/etc/systemd/system/fleet-worker.service"
check "writes the service" test -f "$UNIT"
check "runs the service as this account" grep -qx "User=$ME" "$UNIT"
check "starts the worker with its config and log" grep -qx "ExecStart=$LINUX_ROOT/usr/local/bin/fleet-worker --config $LINUX_HOME/fleet/worker.yaml --log-file $LINUX_HOME/fleet/worker.log" "$UNIT"
check "restarts it after a crash" grep -qx 'Restart=always' "$UNIT"
check "lets the worker stop its own jobs" grep -qx 'KillMode=mixed' "$UNIT"
check "stops jobs gaining privileges" grep -qx 'NoNewPrivileges=true' "$UNIT"
check "reloads with SIGHUP" grep -qx 'ExecReload=/bin/kill -HUP $MAINPID' "$UNIT"
check "starts at boot" grep -q 'systemctl enable --quiet fleet-worker' "$STUB_LOG"
check "reloads systemd before starting" test "$(position daemon-reload)" -lt "$(position 'restart fleet-worker')"
check "saves the token privately" test "$(stat -c %a "$LINUX_HOME/fleet/token")" = 600
check "labels the machine as Linux" grep -q 'os: linux' "$LINUX_HOME/fleet/worker.yaml"
check "gives it a capability test" grep -q 'test:' "$LINUX_HOME/fleet/worker.yaml"
check "tells you how to restart it" grep -q 'sudo systemctl restart fleet-worker' "$LINUX_HOME/fleet/worker.yaml"
refute "leaves SELinux alone when it's off" grep -q restorecon "$STUB_LOG"
check "the Linux machine is online at the gateway" online_on system.hostname 1
JOB_OUT="$WORK/linux-job.out"
"$WORK/fleet" run system.hostname --input '{}' >"$JOB_OUT" 2>&1
check "runs a job" grep -q "$(uname -n)" "$JOB_OUT"
[ -s "$JOB_OUT" ] && grep -q "$(uname -n)" "$JOB_OUT" || sed 's/^/    /' "$JOB_OUT" >&2

echo "Upgrading the Linux worker, with SELinux on"
echo '# my edit' >>"$LINUX_HOME/fleet/worker.yaml"
TOKEN_BEFORE="$(cat "$LINUX_HOME/fleet/token")"
: >"$STUB_LOG"
STUB_SELINUX=Enforcing run_linux_install "$LINUX_ROOT" "$LINUX_HOME" >"$OUT" 2>&1
RESULT=$?
check "upgrades without a name or token" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "keeps the edited manifest" grep -q '# my edit' "$LINUX_HOME/fleet/worker.yaml"
check "keeps the token" test "$(cat "$LINUX_HOME/fleet/token")" = "$TOKEN_BEFORE"
check "labels the binary for SELinux before starting it" test "$(position "restorecon $LINUX_ROOT/usr/local/bin/fleet-worker")" -lt "$(position 'restart fleet-worker')"
check "reconnects" online_on system.hostname 1

echo "A bad Linux manifest leaves the running worker alone"
cp "$LINUX_HOME/fleet/worker.yaml" "$WORK/linux-good.yaml"
echo 'nonsense_field: true' >>"$LINUX_HOME/fleet/worker.yaml"
: >"$STUB_LOG"
run_linux_install "$LINUX_ROOT" "$LINUX_HOME" >"$OUT" 2>&1
check "refuses to install" test $? -ne 0
refute "doesn't restart the worker" grep -q 'restart fleet-worker' "$STUB_LOG"
cp "$WORK/linux-good.yaml" "$LINUX_HOME/fleet/worker.yaml"

echo "A tampered Linux download is refused"
cp "$LINUX_ROOT/usr/local/bin/fleet-worker" "$WORK/linux-installed"
echo '# tampered' >>"$PUBLISHED/fleet-worker-linux-amd64"
: >"$STUB_LOG"
run_linux_install "$LINUX_ROOT" "$LINUX_HOME" >"$OUT" 2>&1
check "refuses to install" test $? -ne 0
check "says why" grep -q "doesn't match its checksum" "$OUT"
check "doesn't replace the binary" cmp "$WORK/linux-installed" "$LINUX_ROOT/usr/local/bin/fleet-worker"
refute "doesn't restart the worker" grep -q 'restart fleet-worker' "$STUB_LOG"
cp "$PUBLISHED/fleet-worker-linux-arm64" "$PUBLISHED/fleet-worker-linux-amd64"

echo "Linux refusals"
BARE="$WORK/linux-bare"
mkdir -p "$BARE"
FLEET_NODE_NAME=x FLEET_NODE_TOKEN=fn_x run_linux_install "$BARE" "$WORK/linux-home-bare" >"$OUT" 2>&1
check "refuses without systemd" test $? -ne 0
check "says it needs systemd" grep -q 'systemd' "$OUT"
refute "changes nothing without systemd" test -e "$WORK/linux-home-bare/fleet"
mkdir -p "$BARE/run/systemd/system"
STUB_ARCH=riscv64 FLEET_NODE_NAME=x FLEET_NODE_TOKEN=fn_x run_linux_install "$BARE" "$WORK/linux-home-bare" >"$OUT" 2>&1
check "refuses an unknown processor" grep -q 'unsupported processor: riscv64' "$OUT"
FLEET_WORKER_USER=someone-else FLEET_NODE_NAME=x FLEET_NODE_TOKEN=fn_x run_linux_install "$BARE" "$WORK/linux-home-bare" >"$OUT" 2>&1
check "won't install for another account without root" grep -q 'run this as root' "$OUT"
mkdir -p "$WORK/linux home"
FLEET_NODE_NAME=x FLEET_NODE_TOKEN=fn_x run_linux_install "$BARE" "$WORK/linux home" >"$OUT" 2>&1
check "refuses a home folder systemd can't name" grep -q "can't be installed in" "$OUT"
STUB_NO_SUDO=1 FLEET_NODE_NAME=x FLEET_NODE_TOKEN=fn_x run_linux_install "$BARE" "$WORK/linux-home-bare" >"$OUT" 2>&1
check "stops without sudo" test $? -ne 0
check "suggests running as root" grep -q 'Run this as root instead' "$OUT"
refute "changes nothing without sudo" test -e "$WORK/linux-home-bare/fleet"

echo "Linux install as root, for another account"
# id and getent stand in for root and a separate account. The files are this
# test's, so ownership isn't tested, only that every user-side step goes through
# runuser and none through sudo.
ROOT_STUBS="$WORK/root-stubs"
cp -r "$LINUX_STUBS" "$ROOT_STUBS"
printf '#!/bin/sh\ncase "$*" in\n"-u") echo 0 ;;\n"-un") echo root ;;\n"-u root") echo 0 ;;\n"-u fleet-worker") echo 1001 ;;\n"-u "*) exit 1 ;;\n"-Gn "*) echo fleet-worker ;;\n*) exec /usr/bin/id "$@" ;;\nesac\n' >"$ROOT_STUBS/id"
printf '#!/bin/sh\ncase "$2" in\nfleet-worker) echo "fleet-worker:x:1001:1001::$STUB_WORKER_HOME:/bin/bash" ;;\nroot) echo "root:x:0:0:root:$STUB_ROOT_HOME:/bin/bash" ;;\nesac\n' >"$ROOT_STUBS/getent"
chmod +x "$ROOT_STUBS/id" "$ROOT_STUBS/getent"
run_root_install() { # root; environment assignments come before this call.
	FLEET_INSTALL_ROOT="$1" HOME=/nonexistent PATH="$ROOT_STUBS:$PATH" sh "$PUBLISHED/worker.sh"
}
LINUX_ROOT2="$WORK/linux-root2"
export STUB_WORKER_HOME="$WORK/fleet-worker-home"
mkdir -p "$LINUX_ROOT2/run/systemd/system" "$STUB_WORKER_HOME"
LINUX_TOKEN2="$(new_node linux-two)"

OUT="$WORK/linux-root.out"
FLEET_WORKER_USER=nobody-here FLEET_NODE_NAME=linux-two FLEET_NODE_TOKEN="$LINUX_TOKEN2" run_root_install "$LINUX_ROOT2" >"$OUT" 2>&1
check "refuses an account that doesn't exist" grep -q "no account named 'nobody-here'" "$OUT"
FLEET_WORKER_USER="$(printf 'fleet-worker\nExecStartPre=/bin/true')" FLEET_NODE_NAME=linux-two FLEET_NODE_TOKEN="$LINUX_TOKEN2" run_root_install "$LINUX_ROOT2" >"$OUT" 2>&1
check "refuses an account name that would add lines to the service" grep -q "isn't an account name" "$OUT"
refute "changes nothing when refusing" test -e "$STUB_WORKER_HOME/fleet"

: >"$STUB_LOG"
FLEET_WORKER_USER=fleet-worker FLEET_NODE_NAME=linux-two FLEET_NODE_TOKEN="$LINUX_TOKEN2" run_root_install "$LINUX_ROOT2" >"$OUT" 2>&1
RESULT=$?
check "installs and connects" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "installs into that account's home" test -f "$STUB_WORKER_HOME/fleet/worker.yaml"
check "both Linux machines are online at the gateway" online_on system.hostname 2
check "runs the service as that account" grep -qx 'User=fleet-worker' "$LINUX_ROOT2/etc/systemd/system/fleet-worker.service"
check "writes the account's files as the account" grep -q 'runuser -u fleet-worker -- sh -c umask 077' "$STUB_LOG"
check "checks the config as the account" grep -q "runuser -u fleet-worker -- .*--check" "$STUB_LOG"
refute "doesn't use sudo as root" grep -q '^sudo' "$STUB_LOG"
: >"$STUB_LOG"
run_root_install "$LINUX_ROOT2" >"$OUT" 2>&1
check "upgrades as root, keeping the service's account" test $? -eq 0
check "keeps the account" grep -qx 'User=fleet-worker' "$LINUX_ROOT2/etc/systemd/system/fleet-worker.service"

echo "Linux install as root, running as root"
# The token alone is given, so the installer asks the gateway whose it is.
LINUX_ROOT3="$WORK/linux-root3"
export STUB_ROOT_HOME="$WORK/root-home"
mkdir -p "$LINUX_ROOT3/run/systemd/system" "$STUB_ROOT_HOME"
LINUX_TOKEN3="$(new_node linux-three)"
OUT="$WORK/linux-as-root.out"
: >"$STUB_LOG"
FLEET_GATEWAY_URL="$TEST_GATEWAY" FLEET_NODE_TOKEN="$LINUX_TOKEN3" run_root_install "$LINUX_ROOT3" >"$OUT" 2>&1
RESULT=$?
check "installs and connects as root" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "installs into root's home" test -f "$STUB_ROOT_HOME/fleet/worker.yaml"
check "runs the service as root" grep -qx 'User=root' "$LINUX_ROOT3/etc/systemd/system/fleet-worker.service"
check "warns that jobs run as root" grep -q 'the worker will run as root' "$OUT"
check "learns the machine's name from its token" grep -qx '  name: linux-three' "$STUB_ROOT_HOME/fleet/worker.yaml"
check "says it connected" grep -q "Connected to $TEST_GATEWAY" "$OUT"
refute "doesn't switch user to act as root" grep -q '^runuser' "$STUB_LOG"
refute "doesn't use sudo as root" grep -q '^sudo' "$STUB_LOG"
check "all three Linux machines are online" online_on system.hostname 3

echo "Pairing with a code"
# The installer asks the gateway to pair and waits for the code, which is read
# from the server exactly as a person would, and typed into a stand-in terminal.
PAIR_ROOT="$WORK/pair-root"
PAIR_HOME="$WORK/pair-home"
mkdir -p "$PAIR_ROOT/run/systemd/system" "$PAIR_HOME"
mkfifo "$WORK/pair-tty"
OUT="$WORK/pair.out"
: >"$STUB_LOG"
# Held open here, so the installer isn't kept waiting for someone to type.
exec 9<>"$WORK/pair-tty"
FLEET_GATEWAY_URL="$TEST_GATEWAY" FLEET_NODE_NAME=paired-box FLEET_INSTALL_TTY="$WORK/pair-tty" run_linux_install "$PAIR_ROOT" "$PAIR_HOME" >"$OUT" 2>&1 &
INSTALL_PID=$!
show_codes() { FLEET_INSTALL_ROOT="$ROOT" PATH="$SERVER_PATH" "$ROOT/usr/local/bin/fleet-pair" --wait 0 2>/dev/null; }
CODE=""
for _ in $(seq 1 40); do
	CODE="$(show_codes | sed -n 's/^ *\([A-Z2-9]\{4\}-[A-Z2-9]\{4\}\)$/\1/p' | head -1)"
	[ -n "$CODE" ] && break
	sleep 0.5
done
check "the server shows a pairing code" test -n "$CODE"
check "the server says which machine is asking" sh -c "FLEET_INSTALL_ROOT='$ROOT' PATH='$SERVER_PATH' '$ROOT/usr/local/bin/fleet-pair' --wait 0 | grep -q 'paired-box (linux/.*) at 127.0.0.1 is asking to pair'"
refute "the code isn't in the gateway's log" grep -q "${CODE:-no-code}\|$(echo "${CODE:-no-code}" | tr -d -)" "$WORK/gateway.log"
# A wrong code first, then the right one in lower case, as someone might type it.
{
	echo "AAAA-AAAA"
	echo "$CODE" | tr 'A-Z' 'a-z'
} >&9
wait "$INSTALL_PID"
RESULT=$?
exec 9>&-
check "installs, pairs and connects" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "says where to find the code" grep -q 'sudo fleet-pair' "$OUT"
check "says the wrong code was wrong" grep -q "That code isn't right" "$OUT"
check "says it paired" grep -q "Paired with $TEST_GATEWAY as paired-box" "$OUT"
check "says it connected" grep -q "Connected to $TEST_GATEWAY" "$OUT"
check "saves the token privately" test "$(stat -c %a "$PAIR_HOME/fleet/token")" = 600
check "the token is a node token" grep -q '^fn_[0-9a-f]\{64\}$' "$PAIR_HOME/fleet/token"
check "keeps the rest of worker.yaml" grep -q 'Pairing this machine fills in gateway_url' "$PAIR_HOME/fleet/worker.yaml"
check "the paired machine is online" online_on system.hostname 4
check "opens the worker's page" grep -q "worker ui --config $PAIR_HOME/fleet/worker.yaml --wait 15s" "$STUB_LOG"
refute "the code can't be used twice" sh -c "FLEET_INSTALL_ROOT='$ROOT' PATH='$SERVER_PATH' '$ROOT/usr/local/bin/fleet-pair' --wait 0 | grep -q '$CODE'"

echo "Skipping the pairing code"
SKIP_ROOT="$WORK/skip-root"
SKIP_HOME="$WORK/skip-home"
mkdir -p "$SKIP_ROOT/run/systemd/system" "$SKIP_HOME"
echo "" >"$WORK/skip-tty"
OUT="$WORK/skip.out"
FLEET_GATEWAY_URL="$TEST_GATEWAY" FLEET_INSTALL_TTY="$WORK/skip-tty" run_linux_install "$SKIP_ROOT" "$SKIP_HOME" >"$OUT" 2>&1
RESULT=$?
check "installs without pairing" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "says it skipped" grep -q 'Skipped. You can pair this machine later from its page' "$OUT"
check "says it isn't paired yet" grep -q 'not paired with a gateway yet' "$OUT"
check "says how to pair from a terminal" grep -q "fleet-worker pair --config $SKIP_HOME/fleet/worker.yaml" "$OUT"
check "the worker is running anyway" kill -0 "$(cat "$SKIP_ROOT/worker.pid")"
refute "saves no token" test -s "$SKIP_HOME/fleet/token"
check "no more machines came online" online_on system.hostname 4

echo "From a release, with no gateway yet"
# Nothing names a gateway, so the installer asks, and the answer here is none.
REL_ROOT="$WORK/rel-root"
REL_HOME="$WORK/rel-home"
mkdir -p "$REL_ROOT/run/systemd/system" "$REL_HOME"
sed 's|@@RELEASE_URL@@|https://releases.example.com/v1|' "$REPO/deploy/install-worker.sh" >"$PUBLISHED/install-worker.sh"
OUT="$WORK/rel.out"
FLEET_INSTALL_TTY="$WORK/skip-tty" FLEET_INSTALL_ROOT="$REL_ROOT" HOME="$REL_HOME" PATH="$LINUX_STUBS:$PATH" sh "$PUBLISHED/install-worker.sh" >"$OUT" 2>&1
RESULT=$?
check "installs with no gateway" test "$RESULT" -eq 0
[ "$RESULT" -eq 0 ] || sed 's/^/    /' "$OUT" >&2
check "asks for the gateway's address" grep -q "Gateway address (the server's IP address or domain; press Enter to skip)" "$OUT"
check "leaves the gateway to be filled in" grep -qx 'gateway_url: ""' "$REL_HOME/fleet/worker.yaml"
check "names the machine after its host" grep -q "^  name: $(hostname -s | tr -cd 'A-Za-z0-9_-')\$" "$REL_HOME/fleet/worker.yaml"
check "says it isn't paired yet" grep -q 'not paired with a gateway yet' "$OUT"
check "the worker is running, waiting to be paired" kill -0 "$(cat "$REL_ROOT/worker.pid")"
OUT="$WORK/rel-nowhere.out"
mkdir -p "$WORK/rel-root2/run/systemd/system"
FLEET_NONINTERACTIVE=1 FLEET_INSTALL_ROOT="$WORK/rel-root2" HOME="$WORK/rel-home2" PATH="$LINUX_STUBS:$PATH" sh "$REPO/deploy/install-worker.sh" >"$OUT" 2>&1
check "stops when there's nowhere to download from" grep -q "nowhere to download the worker from" "$OUT"

echo
if [ "$FAILURES" -eq 0 ]; then
	echo "All install tests passed."
	exit 0
fi
echo "$FAILURES install tests failed." >&2
exit 1
