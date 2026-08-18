#!/usr/bin/env bash
#
# T1.4 baseline harness. Runs redis-benchmark against strata-server and emits
# a markdown table for docs/benchmarks.md.
#
# Every number this project publishes traces to a run of this script. Nothing
# is hand-typed, because a hand-typed number cannot be reproduced.
#
# Usage:
#   test/bench/baseline.sh [output.md]
#
# Requires: redis-benchmark (redis-tools), a built strata-server.

set -euo pipefail

readonly REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly SERVER="${REPO_ROOT}/bin/strata-server"
readonly PORT="${STRATA_BENCH_PORT:-6380}"
readonly OUT="${1:-/dev/stdout}"

# Runs per configuration. plan.md requires the median of at least three, never
# best-of-N, because a best-of-N number is not one anybody else can reproduce.
readonly RUNS="${STRATA_BENCH_RUNS:-3}"

readonly REQUESTS="${STRATA_BENCH_REQUESTS:-100000}"
readonly CLIENTS="${STRATA_BENCH_CLIENTS:-50}"
readonly VALUE_SIZE="${STRATA_BENCH_VALUE_SIZE:-64}"

die() { echo "error: $*" >&2; exit 1; }

command -v redis-benchmark >/dev/null || die "redis-benchmark not found; install redis-tools"
[[ -x "${SERVER}" ]] || die "${SERVER} not built; run make build"

# --- machine state ---------------------------------------------------------
# Captured rather than described, because "my laptop" is not a hardware
# specification and the Phase 8 comparison depends on knowing this exactly.

cpu_model()  { grep -m1 '^model name' /proc/cpuinfo | cut -d: -f2- | sed 's/^ *//'; }
cpu_cores()  { nproc; }
ram_total()  { awk '/^MemTotal:/ {printf "%.1f GiB", $2/1024/1024}' /proc/meminfo; }
kernel_ver() { uname -sr; }
go_version() { go version | awk '{print $3}'; }

disk_model() {
	local src
	src=$(findmnt -no SOURCE --target "${REPO_ROOT}" 2>/dev/null || echo "")
	[[ -z "${src}" ]] && { echo "unknown"; return; }
	lsblk -no MODEL "${src}" 2>/dev/null | head -1 | sed 's/^ *//;s/ *$//' || echo "unknown"
}

filesystem() {
	findmnt -no FSTYPE --target "${REPO_ROOT}" 2>/dev/null || echo "unknown"
}

# Conditions that invalidate a comparison if they differ between runs. plan.md
# names an unpinned machine as T1.4's trap.
power_source() {
	local ac
	for ac in /sys/class/power_supply/A{C,DP}*/online; do
		[[ -r "${ac}" ]] || continue
		[[ "$(cat "${ac}")" == "1" ]] && echo "AC" || echo "battery"
		return
	done
	echo "unknown (no battery detected)"
}

cpu_governor() {
	local gov=/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor
	[[ -r "${gov}" ]] && cat "${gov}" || echo "unknown"
}

load_average() { awk '{print $1", "$2", "$3}' /proc/loadavg; }

# --- server lifecycle ------------------------------------------------------

SERVER_PID=""
cleanup() {
	if [[ -n "${SERVER_PID}" ]] && kill -0 "${SERVER_PID}" 2>/dev/null; then
		# SIGTERM exercises the same graceful drain T1.2 tests, so a
		# benchmark run also smoke-tests shutdown.
		kill -TERM "${SERVER_PID}" 2>/dev/null || true
		wait "${SERVER_PID}" 2>/dev/null || true
	fi
}
trap cleanup EXIT

start_server() {
	"${SERVER}" -addr ":${PORT}" -log-level error &
	SERVER_PID=$!

	for _ in $(seq 1 50); do
		if redis-cli -p "${PORT}" PING >/dev/null 2>&1; then
			return 0
		fi
		sleep 0.1
	done
	die "server did not become ready on port ${PORT}"
}

# --- measurement -----------------------------------------------------------

# run_one echoes "throughput p50 p99" for a single redis-benchmark invocation.
# Latency comes from the same run as throughput so the two always describe the
# same workload.
run_one() {
	local test_name="$1" pipeline="$2"
	local args=(-p "${PORT}" -t "${test_name}" -n "${REQUESTS}" -c "${CLIENTS}"
	            -d "${VALUE_SIZE}" -q --precision 3)
	[[ "${pipeline}" -gt 1 ]] && args+=(-P "${pipeline}")

	# stderr is kept so a failing benchmark says why on the terminal.
	#
	# Failure is signalled by the FAILED sentinel rather than by calling die,
	# because run_one is invoked inside a command substitution: exit there ends
	# only the subshell, and the caller would carry on with empty values. The
	# caller checks for the sentinel.
	local output
	output=$(redis-benchmark "${args[@]}" | grep -i "^${test_name}" || true)
	[[ -z "${output}" ]] && { echo "FAILED no-result-line"; return; }

	# redis-benchmark -q line:
	#   SET: 123456.78 requests per second, p50=0.100 msec, p99=0.500 msec
	local rps p50 p99
	rps=$(sed -n 's/.*[: ]\([0-9.]*\) requests per second.*/\1/p' <<<"${output}")
	p50=$(sed -n 's/.*p50=\([0-9.]*\).*/\1/p' <<<"${output}")
	p99=$(sed -n 's/.*p99=\([0-9.]*\).*/\1/p' <<<"${output}")

	# A parse failure means redis-benchmark changed its output format. Refusing
	# to guess is the point: a defaulted zero would reach docs/benchmarks.md as
	# a published figure that no run actually produced.
	[[ -n "${rps}" && -n "${p50}" && -n "${p99}" ]] \
		|| { echo "FAILED unparsable-output"; return; }

	echo "${rps} ${p50} ${p99}"
}

median() {
	local -a sorted
	mapfile -t sorted < <(printf '%s\n' "$@" | sort -g)
	echo "${sorted[$(( ${#sorted[@]} / 2 ))]}"
}

spread() {
	local -a sorted
	mapfile -t sorted < <(printf '%s\n' "$@" | sort -g)
	printf '%s–%s' "${sorted[0]}" "${sorted[-1]}"
}

# run_workload prints one markdown table row: the median of RUNS runs, with
# the observed range so a reader can see the variance rather than trust a
# single figure.
run_workload() {
	local label="$1" test_name="$2" pipeline="$3"
	local -a rps_all=() p50_all=() p99_all=()

	echo "  ${label} (pipeline=${pipeline})..." >&2

	for run in $(seq 1 "${RUNS}"); do
		redis-cli -p "${PORT}" FLUSHDB >/dev/null 2>&1 || true

		read -r rps p50 p99 <<<"$(run_one "${test_name}" "${pipeline}")"
		[[ "${rps}" == "FAILED" ]] && die "${test_name} (pipeline=${pipeline}) run ${run}: ${p50}; the server may have died mid-run"

		rps_all+=("${rps}"); p50_all+=("${p50}"); p99_all+=("${p99}")
		echo "    run ${run}/${RUNS}: ${rps} ops/sec, p50=${p50}ms, p99=${p99}ms" >&2
	done

	local pipe_label="no"
	[[ "${pipeline}" -gt 1 ]] && pipe_label="yes (P=${pipeline})"

	local cmd="redis-benchmark -p ${PORT} -t ${test_name} -n ${REQUESTS} -c ${CLIENTS} -d ${VALUE_SIZE} -q"
	[[ "${pipeline}" -gt 1 ]] && cmd+=" -P ${pipeline}"

	printf '| %s | %s | %s ops/sec | %s ms | %s ms | `%s` |\n' \
		"${label}" "${pipe_label}" \
		"$(median "${rps_all[@]}")" \
		"$(median "${p50_all[@]}")" \
		"$(median "${p99_all[@]}")" \
		"${cmd}"

	printf '  <!-- range across %d runs: %s ops/sec -->\n' \
		"${RUNS}" "$(spread "${rps_all[@]}")"
}

main() {
	echo "starting strata-server on port ${PORT}..." >&2
	start_server
	echo "running ${RUNS} runs per workload..." >&2

	{
		echo "## Test machine"
		echo
		echo "| Property | Value |"
		echo "|---|---|"
		printf '| CPU | %s (%s cores) |\n' "$(cpu_model)" "$(cpu_cores)"
		printf '| RAM | %s |\n' "$(ram_total)"
		printf '| Disk | %s |\n' "$(disk_model)"
		printf '| Filesystem | %s |\n' "$(filesystem)"
		printf '| Kernel | %s |\n' "$(kernel_ver)"
		printf '| Go version | %s |\n' "$(go_version)"
		echo
		echo "**Conditions at run time**"
		echo
		printf -- '- Power source: %s\n' "$(power_source)"
		printf -- '- CPU governor: %s\n' "$(cpu_governor)"
		printf -- '- Load average before run: %s\n' "$(load_average)"
		printf -- '- Measured: %s\n' "$(date -Iseconds)"
		printf -- '- Engine: in-memory map, no durability (Phase 1)\n'
		echo
		echo "## Baseline — in-memory map (T1.4)"
		echo
		printf 'Median of %d runs, %d requests, %d clients, %d-byte values.\n' \
			"${RUNS}" "${REQUESTS}" "${CLIENTS}" "${VALUE_SIZE}"
		echo
		echo "| Workload | Pipelined | Throughput | p50 | p99 | Command |"
		echo "|---|---|---|---|---|---|"

		run_workload "SET" set 1
		run_workload "SET" set 16
		run_workload "GET" get 1
		run_workload "GET" get 16
	} > "${OUT}"

	echo "done; wrote ${OUT}" >&2
}

main "$@"
