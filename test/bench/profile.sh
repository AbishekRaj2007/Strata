#!/usr/bin/env bash
#
# T1.4 profile capture. Runs a load generator against strata-server while
# pulling CPU and heap profiles from its pprof endpoint.
#
# The profiles are the point, not the throughput number: they establish where
# time and allocations go before any storage exists, so a Phase 8 profile can
# be diffed against a known starting shape.
#
# Usage:
#   test/bench/profile.sh [output-dir]

set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly SERVER="${REPO_ROOT}/bin/strata-server"
readonly PORT="${STRATA_BENCH_PORT:-6380}"
readonly PPROF_PORT="${STRATA_PPROF_PORT:-6060}"
readonly OUT_DIR="${1:-${REPO_ROOT}/docs/profiles}"

# Long enough to cover a CPU profile with headroom for the load to stabilise.
readonly PROFILE_SECONDS="${STRATA_PROFILE_SECONDS:-30}"
readonly REQUESTS="${STRATA_BENCH_REQUESTS:-2000000}"

die() { echo "error: $*" >&2; exit 1; }

command -v redis-benchmark >/dev/null || die "redis-benchmark not found; install redis-tools"
command -v go >/dev/null || die "go not found"
[[ -x "${SERVER}" ]] || die "${SERVER} not built; run make build"

mkdir -p "${OUT_DIR}"

SERVER_PID=""
BENCH_PID=""
cleanup() {
	[[ -n "${BENCH_PID}" ]] && kill "${BENCH_PID}" 2>/dev/null || true
	if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
		kill -TERM "${SERVER_PID}" 2>/dev/null || true
		wait "${SERVER_PID}" 2>/dev/null || true
	fi
}
trap cleanup EXIT

echo "starting strata-server with pprof on :${PPROF_PORT}..." >&2
"${SERVER}" -addr ":${PORT}" -pprof-addr "localhost:${PPROF_PORT}" -log-level error &
SERVER_PID=$!

for _ in $(seq 1 50); do
	redis-cli -p "${PORT}" PING >/dev/null 2>&1 && break
	sleep 0.1
done
redis-cli -p "${PORT}" PING >/dev/null 2>&1 || die "server did not become ready"

echo "generating load for ${PROFILE_SECONDS}s..." >&2
redis-benchmark -p "${PORT}" -t set,get -n "${REQUESTS}" -c 50 -d 64 -P 16 -q >/dev/null 2>&1 &
BENCH_PID=$!

# Let the load reach steady state so the profile is not dominated by startup.
sleep 3

echo "capturing CPU profile (${PROFILE_SECONDS}s)..." >&2
curl -s "http://localhost:${PPROF_PORT}/debug/pprof/profile?seconds=${PROFILE_SECONDS}" \
	-o "${OUT_DIR}/cpu.prof" || die "failed to capture CPU profile"

echo "capturing heap profile..." >&2
curl -s "http://localhost:${PPROF_PORT}/debug/pprof/heap" \
	-o "${OUT_DIR}/heap.prof" || die "failed to capture heap profile"

kill "${BENCH_PID}" 2>/dev/null || true
BENCH_PID=""

echo "rendering flamegraphs..." >&2
go tool pprof -svg "${SERVER}" "${OUT_DIR}/cpu.prof"  > "${OUT_DIR}/cpu-flame.svg"
go tool pprof -svg "${SERVER}" "${OUT_DIR}/heap.prof" > "${OUT_DIR}/heap-flame.svg"

{
	echo "# Phase 1 baseline profiles"
	echo
	echo "Captured $(date -Iseconds) against the in-memory engine."
	echo
	echo '## Top CPU consumers'
	echo
	echo '```'
	go tool pprof -top -nodecount=20 "${SERVER}" "${OUT_DIR}/cpu.prof" 2>/dev/null
	echo '```'
	echo
	echo '## Top allocation sites'
	echo
	echo '```'
	go tool pprof -top -nodecount=20 -sample_index=alloc_space "${SERVER}" "${OUT_DIR}/heap.prof" 2>/dev/null
	echo '```'
} > "${OUT_DIR}/summary.md"

echo "done; profiles in ${OUT_DIR}" >&2
