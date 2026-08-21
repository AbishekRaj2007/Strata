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

# Matches the baseline run. Without -r the load is one literal key, and the
# profile then shows map lookups on a single hot entry rather than on a
# keyspace -- the wrong shape to diff a Phase 8 capture against.
readonly KEYSPACE="${STRATA_BENCH_KEYSPACE:-10000}"

die() { echo "error: $*" >&2; exit 1; }

command -v redis-benchmark >/dev/null || die "redis-benchmark not found; install redis-tools"
command -v go >/dev/null || die "go not found"
[[ -x "${SERVER}" ]] || die "${SERVER} not built; run make build"

mkdir -p "${OUT_DIR}"

SERVER_PID=""
BENCH_PID=""
cleanup() {
	# Clear the sentinel before killing the loop: killing the subshell leaves
	# the redis-benchmark it is currently waiting on running, and a stray load
	# generator poisons the next run's measurement.
	rm -f "${LOAD_SENTINEL:-}" 2>/dev/null || true
	if [[ -n "${BENCH_PID}" ]]; then
		pkill -P "${BENCH_PID}" 2>/dev/null || true
		kill "${BENCH_PID}" 2>/dev/null || true
	fi
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

# The load is driven by a restart loop rather than by one large -n, because a
# request count is not a duration: at pipelined speeds 2,000,000 requests
# complete in about three seconds, and an earlier version of this script then
# profiled a perfectly idle server for thirty. Loop until the capture is done
# and let the sentinel file, not the request count, decide when to stop.
LOAD_SENTINEL="$(mktemp)"
generate_load() {
	while [[ -e "${LOAD_SENTINEL}" ]]; do
		redis-benchmark -p "${PORT}" -t set,get -n "${REQUESTS}" -c 50 \
			-d 64 -r "${KEYSPACE}" -P 16 -q >/dev/null 2>&1 || true
	done
}
generate_load &
BENCH_PID=$!

# Let the load reach steady state so the profile is not dominated by startup.
sleep 3

echo "capturing CPU profile (${PROFILE_SECONDS}s)..." >&2
curl -s "http://localhost:${PPROF_PORT}/debug/pprof/profile?seconds=${PROFILE_SECONDS}" \
	-o "${OUT_DIR}/cpu.prof" || die "failed to capture CPU profile"

echo "capturing heap profile..." >&2
curl -s "http://localhost:${PPROF_PORT}/debug/pprof/heap" \
	-o "${OUT_DIR}/heap.prof" || die "failed to capture heap profile"

rm -f "${LOAD_SENTINEL}"
kill "${BENCH_PID}" 2>/dev/null || true
BENCH_PID=""

# A profile of an idle server is not a smaller version of the right answer, it
# is the wrong answer, and it is the failure this script has already shipped
# once. pprof reports "Total samples = 0" in that case; refuse to render
# flamegraphs and a summary from it.
samples=$(go tool pprof -top -nodecount=1 "${SERVER}" "${OUT_DIR}/cpu.prof" 2>/dev/null \
	| sed -n 's/.*Total samples = \([0-9.]*\).*/\1/p')
[[ -z "${samples}" ]] && die "could not read the sample count from ${OUT_DIR}/cpu.prof"
awk -v s="${samples}" 'BEGIN { exit !(s > 0.5) }' \
	|| die "CPU profile has only ${samples}s of samples; the server was idle during the capture"

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
