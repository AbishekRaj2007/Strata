#!/usr/bin/env bash
#
# T8.1 full benchmark harness. Runs every workload in plan.md §19 against the
# real durable engine (engine.LSM, not the in-memory stand-in T1.4 measured)
# and emits a markdown report for docs/benchmarks.md.
#
# Every number in this file is measured, never estimated -- run this and
# paste its output, do not hand-edit a row.
#
# Usage:
#   test/bench/full.sh [output.md]
#
# Requires: redis-benchmark (redis-tools), a built strata-server, and
# test/bench/loadgen + test/bench/chart (built by this script).

set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly SERVER="${REPO_ROOT}/bin/strata-server"
readonly PORT="${STRATA_BENCH_PORT:-6381}"
readonly OUT="${1:-/dev/stdout}"
readonly DATA_DIR="${STRATA_BENCH_DATA_DIR:-${REPO_ROOT}/.bench-data}"

readonly RUNS="${STRATA_BENCH_RUNS:-3}"
readonly REQUESTS="${STRATA_BENCH_REQUESTS:-500000}"
readonly CLIENTS="${STRATA_BENCH_CLIENTS:-50}"
readonly VALUE_SIZE="${STRATA_BENCH_VALUE_SIZE:-64}"
readonly KEYSPACE="${STRATA_BENCH_KEYSPACE:-100000}"

# A cache small enough that a real "10x cache" dataset fits in a few hundred
# MB rather than several GB, so the row is affordable to reproduce. Recorded
# explicitly in the report rather than left implicit, because plan.md's
# target table assumes nothing about cache size and a reader comparing
# numbers needs to know this was not the server's 64 MiB default.
readonly SMALL_CACHE_MB="${STRATA_BENCH_CACHE_MB:-16}"
readonly TENX_KEYSPACE="${STRATA_BENCH_TENX_KEYSPACE:-160000}"
readonly TENX_VALUE_SIZE="${STRATA_BENCH_TENX_VALUE_SIZE:-1024}"

die() { echo "error: $*" >&2; exit 1; }

command -v redis-benchmark >/dev/null || die "redis-benchmark not found; install redis-tools"
command -v go >/dev/null || die "go not found"
[[ -x "${SERVER}" ]] || die "${SERVER} not built; run make build"

readonly BIN_DIR="${REPO_ROOT}/bin"
mkdir -p "${BIN_DIR}"
go build -o "${BIN_DIR}/loadgen" "${REPO_ROOT}/test/bench/loadgen"
go build -o "${BIN_DIR}/chart" "${REPO_ROOT}/test/bench/chart"

cpu_model()  { grep -m1 '^model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ *//'; }
cpu_cores()  { nproc; }
ram_total()  { awk '/^MemTotal:/ {printf "%.1f GiB", $2/1024/1024}' /proc/meminfo; }
kernel_ver() { uname -sr; }
go_version() { go version | awk '{print $3}'; }
load_average() { awk '{print $1", "$2", "$3}' /proc/loadavg; }

SERVER_PID=""
cleanup() {
	if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
		kill -TERM "${SERVER_PID}" 2>/dev/null || true
		wait "${SERVER_PID}" 2>/dev/null || true
	fi
	rm -rf "${DATA_DIR}"
}
trap cleanup EXIT

# start_server (re)starts the server against a clean data directory, so one
# workload's data never leaks into the next's measurement.
start_server() {
	local sync="$1" cache_mb="$2"
	if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
		kill -TERM "${SERVER_PID}" 2>/dev/null || true
		wait "${SERVER_PID}" 2>/dev/null || true
	fi
	rm -rf "${DATA_DIR}"
	mkdir -p "${DATA_DIR}"

	"${SERVER}" -addr ":${PORT}" -data-dir "${DATA_DIR}" -sync "${sync}" \
		-cache-mb "${cache_mb}" -log-level error &
	SERVER_PID=$!

	for _ in $(seq 1 50); do
		redis-cli -p "${PORT}" PING >/dev/null 2>&1 && return 0
		sleep 0.1
	done
	die "server did not become ready on port ${PORT}"
}

run_one() {
	local test_name="$1" pipeline="$2"; shift 2
	local args=(-p "${PORT}" -t "${test_name}" -c "${CLIENTS}" --precision 3 "$@")
	[[ "${pipeline}" -gt 1 ]] && args+=(-P "${pipeline}")

	local output
	output=$(redis-benchmark "${args[@]}" 2>/dev/null | tr '\r' '\n') || true
	[[ -z "${output}" ]] && { echo "FAILED no-output"; return; }

	local rps latency_row p50 p95 p99
	rps=$(sed -n 's/.*throughput summary: *\([0-9.]*\) requests per second.*/\1/p' <<<"${output}")
	latency_row=$(grep -A1 -E '^ *avg  *min  *p50' <<<"${output}" | tail -1)
	p50=$(awk '{print $3}' <<<"${latency_row}")
	p95=$(awk '{print $4}' <<<"${latency_row}")
	p99=$(awk '{print $5}' <<<"${latency_row}")
	# p99.9 is not in the Summary row; recover it from the cumulative
	# percentile histogram redis-benchmark prints above it.
	local p999
	p999=$(grep -oE '^[0-9]+\.[0-9]+% <= [0-9.]+ milliseconds' <<<"${output}" \
		| awk -F'[% ]+' '$1+0 >= 99.9 {print $3; exit}')

	[[ -n "${rps}" && -n "${p50}" && -n "${p99}" ]] \
		|| { echo "FAILED unparsable-output"; return; }

	echo "${rps} ${p50} ${p95} ${p99} ${p999:-n/a}"
}

# run_custom_cmd is run_one's counterpart for a raw command line rather than
# a -t test name, needed for the absent-key row: -t get always reads
# "key:__rand_int__", the exact keys a prepare pass just wrote, so it cannot
# express "read a key that was never written".
run_custom_cmd() {
	local pipeline="$1"; shift
	local args=(-p "${PORT}" -c "${CLIENTS}" --precision 3 "$@")
	[[ "${pipeline}" -gt 1 ]] && args=(-p "${PORT}" -c "${CLIENTS}" --precision 3 -P "${pipeline}" "$@")

	local output
	output=$(redis-benchmark "${args[@]}" 2>/dev/null | tr '\r' '\n') || true
	[[ -z "${output}" ]] && { echo "FAILED no-output"; return; }

	local rps latency_row p50 p95 p99
	rps=$(sed -n 's/.*throughput summary: *\([0-9.]*\) requests per second.*/\1/p' <<<"${output}")
	latency_row=$(grep -A1 -E '^ *avg  *min  *p50' <<<"${output}" | tail -1)
	p50=$(awk '{print $3}' <<<"${latency_row}")
	p95=$(awk '{print $4}' <<<"${latency_row}")
	p99=$(awk '{print $5}' <<<"${latency_row}")

	[[ -n "${rps}" && -n "${p50}" && -n "${p99}" ]] \
		|| { echo "FAILED unparsable-output"; return; }

	echo "${rps} ${p50} ${p95} ${p99}"
}

run_workload_custom() {
	local label="$1" pipeline="$2"; shift 2
	local -a rps_all=() p50_all=() p95_all=() p99_all=()
	echo "  ${label}..." >&2

	for run in $(seq 1 "${RUNS}"); do
		read -r rps p50 p95 p99 <<<"$(run_custom_cmd "${pipeline}" "$@")"
		[[ "${rps}" == "FAILED" ]] && die "${label} run ${run}: ${p50}"
		rps_all+=("${rps}"); p50_all+=("${p50}"); p95_all+=("${p95}"); p99_all+=("${p99}")
		echo "    run ${run}/${RUNS}: ${rps} ops/sec" >&2
	done

	printf '| %s | %s ops/sec | %s ms | %s ms | %s ms |\n' \
		"${label}" "$(median "${rps_all[@]}")" "$(median "${p50_all[@]}")" \
		"$(median "${p95_all[@]}")" "$(median "${p99_all[@]}")"
}

median() {
	local -a sorted
	mapfile -t sorted < <(printf '%s\n' "$@" | sort -g)
	echo "${sorted[$(( ${#sorted[@]} / 2 ))]}"
}

# run_workload prints one markdown row: median of RUNS runs.
run_workload() {
	local label="$1" test_name="$2" pipeline="$3"; shift 3
	local -a rps_all=() p50_all=() p95_all=() p99_all=()
	echo "  ${label}..." >&2

	for run in $(seq 1 "${RUNS}"); do
		read -r rps p50 p95 p99 p999 <<<"$(run_one "${test_name}" "${pipeline}" "$@")"
		[[ "${rps}" == "FAILED" ]] && die "${label} run ${run}: ${p50}"
		rps_all+=("${rps}"); p50_all+=("${p50}"); p95_all+=("${p95}"); p99_all+=("${p99}")
		echo "    run ${run}/${RUNS}: ${rps} ops/sec, p50=${p50}ms p95=${p95}ms p99=${p99}ms p99.9=${p999}ms" >&2
	done

	printf '| %s | %s ops/sec | %s ms | %s ms | %s ms |\n' \
		"${label}" "$(median "${rps_all[@]}")" "$(median "${p50_all[@]}")" \
		"$(median "${p95_all[@]}")" "$(median "${p99_all[@]}")"
}

main() {
	{
		echo "## Full benchmark suite — durable engine (T8.1)"
		echo
		echo "Generated by \`test/bench/full.sh\`. Median of ${RUNS} runs per row unless stated otherwise."
		echo
		printf -- '- CPU: %s (%s cores)\n' "$(cpu_model)" "$(cpu_cores)"
		printf -- '- RAM: %s\n' "$(ram_total)"
		printf -- '- Kernel: %s, Go %s\n' "$(kernel_ver)" "$(go_version)"
		printf -- '- Load average before run: %s\n' "$(load_average)"
		printf -- '- Measured: %s\n' "$(date -Iseconds)"
		printf -- '- Engine: durable engine.LSM (WAL + memtable + SSTables + compaction)\n'
		echo

		echo "### Write and read workloads"
		echo
		echo "| Workload | Throughput | p50 | p95 | p99 |"
		echo "|---|---|---|---|---|"

		echo "sync=interval, pipelined SET (sequential keys)..." >&2
		start_server interval 64
		run_workload "SET, sequential keys, pipelined P=16, sync=interval" set 16 \
			-n "${REQUESTS}" -d "${VALUE_SIZE}" -r "${KEYSPACE}" --sequential

		echo "sync=interval, pipelined SET (random keys)..." >&2
		start_server interval 64
		run_workload "SET, random keys, pipelined P=16, sync=interval" set 16 \
			-n "${REQUESTS}" -d "${VALUE_SIZE}" -r "${KEYSPACE}"

		echo "sync=interval, unpipelined SET..." >&2
		start_server interval 64
		run_workload "SET, unpipelined, sync=interval" set 1 \
			-n "${REQUESTS}" -d "${VALUE_SIZE}" -r "${KEYSPACE}"

		echo "sync=always, unpipelined SET..." >&2
		start_server always 64
		run_workload "SET, unpipelined, sync=always" set 1 \
			-n 20000 -d "${VALUE_SIZE}" -r "${KEYSPACE}"

		echo "GET, dataset in cache..." >&2
		start_server interval 64
		redis-benchmark -p "${PORT}" -t set -n "${KEYSPACE}" -c "${CLIENTS}" \
			-d "${VALUE_SIZE}" -r "${KEYSPACE}" --sequential -q >/dev/null 2>&1
		run_workload "GET, dataset in cache" get 16 \
			-n "${REQUESTS}" -d "${VALUE_SIZE}" -r "${KEYSPACE}"

		echo "GET, dataset 10x cache (${SMALL_CACHE_MB} MiB cache)..." >&2
		start_server interval "${SMALL_CACHE_MB}"
		redis-benchmark -p "${PORT}" -t set -n "${TENX_KEYSPACE}" -c "${CLIENTS}" \
			-d "${TENX_VALUE_SIZE}" -r "${TENX_KEYSPACE}" --sequential -q >/dev/null 2>&1
		run_workload "GET, dataset ~10x cache (${SMALL_CACHE_MB} MiB cache, ${TENX_VALUE_SIZE}B values)" get 16 \
			-n "${REQUESTS}" -d "${TENX_VALUE_SIZE}" -r "${TENX_KEYSPACE}"

		echo "GET, absent key, blooms on..." >&2
		# Populate under "key:" and query under "absent:" -- a disjoint
		# namespace -- so every read is a guaranteed miss against a populated
		# tree, the case the bloom filter exists to make cheap. Querying an
		# empty database would measure the empty-tree fast path instead.
		start_server interval 64
		redis-benchmark -p "${PORT}" -n "${KEYSPACE}" -c "${CLIENTS}" -r "${KEYSPACE}" -q \
			SET "key:__rand_int__" "$(head -c "${VALUE_SIZE}" /dev/zero | tr '\0' 'x')" >/dev/null 2>&1
		run_workload_custom "GET, absent key, blooms on" 16 \
			-n "${REQUESTS}" -r "${KEYSPACE}" GET "absent:__rand_int__"

		echo
		echo "### Mixed 80/20 read/write, full latency percentiles (T8.1)"
		echo
		echo "Measured with \`test/bench/loadgen mixed\`, which valkey-benchmark cannot"
		echo "produce directly: an interleaved weighted workload on persistent connections,"
		echo "timing each operation, with p99.9 alongside p50/p95/p99."
		echo
		start_server interval 64
		local mixed_out
		mixed_out=$("${BIN_DIR}/loadgen" mixed -addr "127.0.0.1:${PORT}" \
			-clients "${CLIENTS}" -duration 20s -keyspace "${KEYSPACE}" -value-size "${VALUE_SIZE}")
		echo '```'
		echo "${mixed_out}"
		echo '```'

		echo
		echo "### Throughput over time under sustained compaction (T8.1)"
		echo
		echo "A small compaction geometry keeps compaction running through the whole window"
		echo "rather than only at the start, so the curve shows real stalls and recovery"
		echo "rather than a quiet steady state."
		echo
		start_server interval 32
		local csv
		csv="$(mktemp /tmp/strata-bench-overtime.XXXXXX.csv)"
		"${BIN_DIR}/loadgen" overtime -addr "127.0.0.1:${PORT}" \
			-clients "${CLIENTS}" -duration 60s -interval 1s -keyspace 500000 -value-size 256 \
			> "${csv}"
		"${BIN_DIR}/chart" -in "${csv}" -out "${REPO_ROOT}/docs/throughput-over-time.svg" \
			-title "SET throughput over 60s under sustained compaction"
		echo "SVG: \`docs/throughput-over-time.svg\` (generated by \`test/bench/chart\` from the CSV below)"
		echo
		echo '```csv'
		cat "${csv}"
		echo '```'
		rm -f "${csv}"

	} > "${OUT}"

	echo "done; wrote ${OUT}" >&2
}

main "$@"
