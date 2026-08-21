#!/usr/bin/env bash
#
# T1.3 done-when, executed. Starts strata-server, drives it with the two real
# Redis clients the condition names -- redis-cli and go-redis -- and reports.
#
# The point is not that the commands return something. It is that clients
# written against real Redis, with no accommodation for this server, cannot
# tell the difference.
#
# Usage:
#   test/interop/run.sh

set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly SERVER="${REPO_ROOT}/bin/strata-server"
readonly PORT="${STRATA_INTEROP_PORT:-6390}"

die() { echo "error: $*" >&2; exit 1; }

command -v redis-cli >/dev/null || die "redis-cli not found; install redis-tools or valkey-tools"
command -v go >/dev/null || die "go not found"
[[ -x "${SERVER}" ]] || die "${SERVER} not built; run make build"

SERVER_PID=""
cleanup() {
	if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
		kill -TERM "${SERVER_PID}" 2>/dev/null || true
		wait "${SERVER_PID}" 2>/dev/null || true
	fi
}
trap cleanup EXIT

echo "starting strata-server on port ${PORT}..." >&2
"${SERVER}" -addr ":${PORT}" -log-level error &
SERVER_PID=$!

for _ in $(seq 1 50); do
	redis-cli -p "${PORT}" PING >/dev/null 2>&1 && break
	sleep 0.1
done
redis-cli -p "${PORT}" PING >/dev/null 2>&1 || die "server did not become ready on port ${PORT}"

rc=0

echo
echo "### redis-cli"
"${REPO_ROOT}/test/interop/redis-cli-roundtrip.sh" "${PORT}" || rc=1

echo
echo "### go-redis"
# Nested module: see the README. Running it from here keeps go-redis out of the
# Strata module's dependency graph.
( cd "${REPO_ROOT}/test/interop/goredis" && go run . "localhost:${PORT}" ) || rc=1

echo
if [[ ${rc} -eq 0 ]]; then
	echo "interop: PASS"
else
	echo "interop: FAIL"
fi
exit "${rc}"
