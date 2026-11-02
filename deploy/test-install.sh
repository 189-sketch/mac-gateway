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
	if [ -f "$WORK/home/worker.pid" ]; then
		kill "$(cat "$WORK/home/worker.pid")" 2>/dev/null
	fi
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
stub caddy 'exit 0'
stub ss '[ -n "${STUB_PORT_HOLDER:-}" ] && echo "$STUB_PORT_HOLDER"
exit 0'
stub curl 'case "$*" in
*127.0.0.1:8080/healthz*) exit 0 ;;
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

echo "Server not reachable over HTTPS yet"
ROOT3="$WORK/root3"
mkdir -p "$ROOT3"
OUT="$WORK/unreachable.out"
# seq and sleep are stubbed too, so this doesn't wait out the real retries.
stub sleep 'exit 0'
STUB_UNREACHABLE=1 run_gateway_install "$ROOT3" fleet.example.com >"$OUT" 2>&1
check "exits with an error" test $? -ne 0
check "still prints the one-time API key" grep -q 'FLEET_API_KEY=fk_' "$OUT"
check "still prints the one-time node token" grep -q 'FLEET_NODE_TOKEN=fn_' "$OUT"
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
for file in fleet-server-linux-amd64.tar.gz fleet-server-linux-arm64.tar.gz fleet-linux-amd64 fleet-linux-arm64 fleet-darwin-arm64 fleet-darwin-amd64 install-gateway.sh install-cli.sh; do
	check "includes $file" test -f "$RELEASE/$file"
done
check "fills in the download address" grep -q 'releases/download/v9.9.9' "$RELEASE/install-gateway.sh"
refute "leaves no placeholder" grep -q '@@RELEASE_URL@@' "$RELEASE/install-gateway.sh"
check "binaries report the version" sh -c "'$RELEASE/fleet-linux-amd64' version | grep -q v9.9.9"
mkdir -p "$WORK/bundle-check"
tar -xzf "$RELEASE/fleet-server-linux-amd64.tar.gz" -C "$WORK/bundle-check"
BUNDLE_DIR="$WORK/bundle-check/fleet-server-linux-amd64"
check "the bundle includes the gateway" test -x "$BUNDLE_DIR/fleet-gateway"
check "the bundled gateway reports the version" sh -c "'$BUNDLE_DIR/fleet-gateway' version | grep -q v9.9.9"

# Hide Go, to prove a release install doesn't need it.
NO_GO="$WORK/no-go"
mkdir -p "$NO_GO"
for tool in bash sh env id seq sleep grep sed mktemp rm cat cp mv mkdir dirname tar gzip sha256sum install touch tee uname cut head chmod; do
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
config="$2"
linux="$(dirname "$config")/worker.linux.yaml"
sed -e "s|^gateway_url:.*|gateway_url: $TEST_GATEWAY|" -e "s|/usr/bin/say|/bin/cat|" "$config" >"$linux"
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
FLEET_NODE_NAME=mac-worker FLEET_NODE_TOKEN="" run_worker_install >"$OUT" 2>&1
check "refuses without a token" test $? -ne 0
FLEET_NODE_NAME='bad;name' FLEET_NODE_TOKEN=fn_x run_worker_install >"$OUT" 2>&1
check "refuses an invalid node name" test $? -ne 0
FLEET_NODE_NAME=mac-worker FLEET_NODE_TOKEN=fn_x STUB_ARCH=sparc run_worker_install >"$OUT" 2>&1
check "refuses an unknown chip" test $? -ne 0

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
check "saves the token privately" test "$(stat -c %a "$MAC_HOME/fleet/token")" = 600
check "writes a valid plist" python3 -c "
import plistlib, sys
p = plistlib.load(open('$MAC_HOME/Library/LaunchAgents/com.fleet.worker.plist', 'rb'))
assert p['Label'] == 'com.fleet.worker'
assert p['ProgramArguments'][0] == '$MAC_HOME/fleet/fleet-worker'
assert p['KeepAlive'] is True
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

echo
if [ "$FAILURES" -eq 0 ]; then
	echo "All install tests passed."
	exit 0
fi
echo "$FAILURES install tests failed." >&2
exit 1
