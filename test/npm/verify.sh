#!/usr/bin/env bash
# Verifies the npm packages end to end without publishing anything: packs all
# three, installs them together into a scratch directory (so the main
# package's optionalDependencies resolve from the local tarballs instead of
# the registry), then runs strata-server through the installed shim and
# checks it with redis-cli. T9.6's done-when condition.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
npm_dir="$root/npm"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "packing npm packages into $work"
mkdir -p "$work/tarballs"
for pkg in strata-linux-x64 strata-linux-arm64 strata; do
	(cd "$npm_dir/$pkg" && npm pack --silent --pack-destination "$work/tarballs")
done

install_dir="$work/install"
mkdir -p "$install_dir"
(cd "$install_dir" && npm init -y >/dev/null)

# Install the platform tarball matching this host plus the main tarball in
# one command, so the optional dependency resolves locally rather than
# hitting the registry for a package that has never been published.
arch="$(node -e 'console.log(process.arch)')"
case "$arch" in
x64) platform_tarball="strata-linux-x64" ;;
arm64) platform_tarball="strata-linux-arm64" ;;
*)
	echo "verify.sh: unsupported host arch $arch, cannot verify locally" >&2
	exit 1
	;;
esac

platform_tgz="$(ls "$work/tarballs"/abishekraj2007-"$platform_tarball"-*.tgz)"
main_tgz="$(ls "$work/tarballs"/abishekraj2007-strata-*.tgz | grep -v -- -linux-)"

echo "installing $(basename "$platform_tgz") and $(basename "$main_tgz")"
(cd "$install_dir" && npm install --no-save --include=optional "$platform_tgz" "$main_tgz" >/dev/null)

server_bin="$install_dir/node_modules/.bin/strata-server"
if [ ! -x "$server_bin" ]; then
	echo "verify.sh: $server_bin missing or not executable after install" >&2
	exit 1
fi

data_dir="$work/data"
mkdir -p "$data_dir"

echo "starting strata-server via the installed npm shim"
"$server_bin" -addr :16381 -data-dir "$data_dir" &
server_pid=$!
# Wait for the shim to exit after SIGTERM instead of just firing the signal
# and racing on: the shim forwards SIGTERM to the real Go binary (see
# exec-native.js) and re-raises it on itself once the child exits, so
# waiting here is what actually confirms the signal reached the server
# instead of leaving it orphaned, holding the port and this scratch dir open.
trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; rm -rf "$work"' EXIT

for _ in $(seq 1 20); do
	if redis-cli -p 16381 PING >/dev/null 2>&1; then
		break
	fi
	sleep 0.2
done

redis-cli -p 16381 PING | grep -q PONG
redis-cli -p 16381 SET npm-verify-key hello | grep -q OK
[ "$(redis-cli -p 16381 GET npm-verify-key)" = "hello" ]

echo "npm package verification passed"
