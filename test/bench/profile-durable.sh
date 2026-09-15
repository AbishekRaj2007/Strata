#!/usr/bin/env bash
#
# T8.2 profile capture against the real durable engine. Captures CPU profiles
# under write load and under read load separately (mixing them into one
# profile would make "CPU under writes" indistinguishable from "CPU under
# reads"), a heap/allocation profile, and block/mutex contention profiles
# under a mixed load via test/bench/loadgen.
#
# Unlike raw throughput, a CPU profile's *proportions* are comparatively
# robust to a noisy background machine: a busy neighbour process lowers the
# sample count, not which function owns what share of the samples that were
# taken. That is why this capture is trustworthy even on a machine where
# test/bench/full.sh's absolute ops/sec numbers were not.
#
# Usage:
#   test/bench/profile-durable.sh [output-dir]

set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly SERVER="${REPO_ROOT}/bin/strata-server"
readonly PORT="${STRATA_BENCH_PORT:-6382}"
readonly PPROF_PORT="${STRATA_PPROF_PORT:-6061}"
readonly OUT_DIR="${1:-${REPO_ROOT}/docs/profiles}"
readonly DATA_DIR="${STRATA_BENCH_DATA_DIR:-${REPO_ROOT}/.bench-profile-data}"

readonly PROFILE_SECONDS="${STRATA_PROFILE_SECONDS:-15}"
readonly CLIENTS="${STRATA_BENCH_CLIENTS:-50}"
readonly KEYSPACE="${STRATA_BENCH_KEYSPACE:-200000}"

die() { echo "error: $*" >&2; exit 1; }

command -v go >/dev/null || die "go not found"
[[ -x "${SERVER}" ]] || die "${SERVER} not built; run make build"

readonly BIN_DIR="${REPO_ROOT}/bin"
mkdir -p "${BIN_DIR}" "${OUT_DIR}"
go build -o "${BIN_DIR}/loadgen" "${REPO_ROOT}/test/bench/loadgen"

SERVER_PID=""
LOAD_PID=""
cleanup() {
	[[ -n "${LOAD_PID}" ]] && kill "${LOAD_PID}" 2>/dev/null || true
	if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
		kill -TERM "${SERVER_PID}" 2>/dev/null || true
		wait "${SERVER_PID}" 2>/dev/null || true
	fi
	rm -rf "${DATA_DIR}"
}
trap cleanup EXIT

start_server() {
	rm -rf "${DATA_DIR}"
	mkdir -p "${DATA_DIR}"
	"${SERVER}" -addr ":${PORT}" -data-dir "${DATA_DIR}" -sync interval \
		-pprof-addr "localhost:${PPROF_PORT}" -log-level error &
	SERVER_PID=$!
	for _ in $(seq 1 50); do
		redis-cli -p "${PORT}" PING >/dev/null 2>&1 && return 0
		sleep 0.1
	done
	die "server did not become ready"
}

# refuse_idle_profile rejects a profile with (near) zero samples: a profile of
# an idle server is not a smaller correct answer, it is the wrong answer.
refuse_idle_profile() {
	local prof="$1"
	local samples
	samples=$(go tool pprof -top -nodecount=1 "${SERVER}" "${prof}" 2>/dev/null \
		| sed -n 's/.*Total samples = \([0-9.]*\).*/\1/p')
	[[ -z "${samples}" ]] && die "could not read sample count from ${prof}"
	awk -v s="${samples}" 'BEGIN { exit !(s > 0.05) }' \
		|| die "${prof} has only ${samples}s of samples; the server was idle during capture"
}

echo "=== write-load CPU profile ===" >&2
start_server
"${BIN_DIR}/loadgen" mixed -addr "127.0.0.1:${PORT}" -clients "${CLIENTS}" \
	-duration $((PROFILE_SECONDS + 10))s -keyspace "${KEYSPACE}" -read-frac 0.0 \
	>/dev/null 2>&1 &
LOAD_PID=$!
sleep 3
curl -s "http://localhost:${PPROF_PORT}/debug/pprof/profile?seconds=${PROFILE_SECONDS}" \
	-o "${OUT_DIR}/cpu-write.prof" || die "failed to capture write CPU profile"
refuse_idle_profile "${OUT_DIR}/cpu-write.prof"
wait "${LOAD_PID}" 2>/dev/null || true
LOAD_PID=""
kill -TERM "${SERVER_PID}" 2>/dev/null || true
wait "${SERVER_PID}" 2>/dev/null || true

echo "=== read-load CPU profile ===" >&2
start_server
# Populate first: a read-load profile against an empty database profiles the
# absent-key path, not the intended in-cache hit path.
"${BIN_DIR}/loadgen" mixed -addr "127.0.0.1:${PORT}" -clients "${CLIENTS}" \
	-duration 5s -keyspace "${KEYSPACE}" -read-frac 0.0 >/dev/null 2>&1
"${BIN_DIR}/loadgen" mixed -addr "127.0.0.1:${PORT}" -clients "${CLIENTS}" \
	-duration $((PROFILE_SECONDS + 10))s -keyspace "${KEYSPACE}" -read-frac 1.0 \
	>/dev/null 2>&1 &
LOAD_PID=$!
sleep 3
curl -s "http://localhost:${PPROF_PORT}/debug/pprof/profile?seconds=${PROFILE_SECONDS}" \
	-o "${OUT_DIR}/cpu-read.prof" || die "failed to capture read CPU profile"
refuse_idle_profile "${OUT_DIR}/cpu-read.prof"
wait "${LOAD_PID}" 2>/dev/null || true
LOAD_PID=""

echo "=== heap profile (under continued read+write load) ===" >&2
"${BIN_DIR}/loadgen" mixed -addr "127.0.0.1:${PORT}" -clients "${CLIENTS}" \
	-duration $((PROFILE_SECONDS + 5))s -keyspace "${KEYSPACE}" -read-frac 0.8 \
	>/dev/null 2>&1 &
LOAD_PID=$!
sleep 3
curl -s "http://localhost:${PPROF_PORT}/debug/pprof/heap" \
	-o "${OUT_DIR}/heap.prof" || die "failed to capture heap profile"
wait "${LOAD_PID}" 2>/dev/null || true
LOAD_PID=""
kill -TERM "${SERVER_PID}" 2>/dev/null || true
wait "${SERVER_PID}" 2>/dev/null || true
SERVER_PID=""

echo "=== block and mutex profiles (mixed load, small compaction geometry) ===" >&2
# Reuses test/stress's own contention capture: the same mechanism T7.4's
# audit already validated, pointed at a shorter run for T8.2's report.
(cd "${REPO_ROOT}" && go test ./test/stress -run TestMixedWorkloadStress -count=1 \
	-stress.duration="${PROFILE_SECONDS}s" -stress.clients="${CLIENTS}" \
	-stress.profile="${OUT_DIR}") || die "stress-test contention capture failed"

echo "rendering flamegraphs..." >&2
for name in cpu-write cpu-read heap; do
	go tool pprof -svg "${SERVER}" "${OUT_DIR}/${name}.prof" > "${OUT_DIR}/${name}-flame.svg" 2>/dev/null \
		|| echo "warning: could not render ${name} flamegraph" >&2
done

{
	echo "# T8.2 profiles — durable engine"
	echo
	echo "Captured $(date -Iseconds)."
	echo
	echo '## Top CPU consumers, write load'
	echo
	echo '```'
	go tool pprof -top -nodecount=20 "${SERVER}" "${OUT_DIR}/cpu-write.prof" 2>/dev/null
	echo '```'
	echo
	echo '## Top CPU consumers, read load'
	echo
	echo '```'
	go tool pprof -top -nodecount=20 "${SERVER}" "${OUT_DIR}/cpu-read.prof" 2>/dev/null
	echo '```'
	echo
	echo '## Top allocation sites (alloc_space)'
	echo
	echo '```'
	go tool pprof -top -nodecount=20 -sample_index=alloc_space "${SERVER}" "${OUT_DIR}/heap.prof" 2>/dev/null
	echo '```'
	echo
	echo '## Mutex contention'
	echo
	echo '```'
	go tool pprof -top -nodecount=15 "${OUT_DIR}/mutex.prof" 2>/dev/null
	echo '```'
	echo
	echo '## Block (lock wait) contention'
	echo
	echo '```'
	go tool pprof -top -nodecount=15 "${OUT_DIR}/block.prof" 2>/dev/null
	echo '```'
} > "${OUT_DIR}/t8.2-summary.md"

echo "done; profiles and summary in ${OUT_DIR}" >&2
