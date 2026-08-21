#!/usr/bin/env bash
# T1.3 done-when, half one: redis-cli round-trips every command in plan.md 7.5.
set -uo pipefail

PORT="${1:-6390}"
PASS=0; FAIL=0

r() { redis-cli -p "$PORT" "$@" 2>&1 | tr -d '\r'; }

check() {
	local what="$1" want="$2" got="$3"
	if [[ "$got" == "$want" ]]; then
		printf '  ok    %-34s -> %s\n' "$what" "$got"; PASS=$((PASS+1))
	else
		printf '  FAIL  %-34s -> got [%s] want [%s]\n' "$what" "$got" "$want"; FAIL=$((FAIL+1))
	fi
}

echo "== connect =="
check "PING"                  "PONG"    "$(r PING)"
check "PING hello"            "hello"   "$(r PING hello)"
check "ECHO strata"           "strata"  "$(r ECHO strata)"

echo "== set/get =="
r FLUSHDB >/dev/null
check "SET k v"               "OK"      "$(r SET k v)"
check "GET k"                 "v"       "$(r GET k)"
check "GET absent"            ""        "$(r GET nope)"
check "SET k overwrite"       "OK"      "$(r SET k v2)"
check "GET k after overwrite" "v2"      "$(r GET k)"
check "SET empty value"       "OK"      "$(r SET empty '')"
check "GET empty value"       ""        "$(r GET empty)"
check "SET binary-ish value"  "OK"      "$(r SET bin $'a\tb')"

echo "== exists/del =="
r SET a 1 >/dev/null; r SET b 2 >/dev/null; r SET c 3 >/dev/null
check "EXISTS a"              "1"       "$(r EXISTS a)"
check "EXISTS absent"         "0"       "$(r EXISTS zzz)"
check "EXISTS a b c"          "3"       "$(r EXISTS a b c)"
check "EXISTS a a"            "2"       "$(r EXISTS a a)"
check "EXISTS a absent"       "1"       "$(r EXISTS a zzz)"
check "DEL a"                 "1"       "$(r DEL a)"
check "DEL absent"            "0"       "$(r DEL a)"
check "DEL b c"               "2"       "$(r DEL b c)"

echo "== dbsize/flushdb =="
r FLUSHDB >/dev/null
check "DBSIZE after FLUSHDB"  "0"       "$(r DBSIZE)"
r SET x 1 >/dev/null; r SET y 2 >/dev/null
check "DBSIZE after 2 sets"   "2"       "$(r DBSIZE)"
check "FLUSHDB"               "OK"      "$(r FLUSHDB)"
check "DBSIZE after flush"    "0"       "$(r DBSIZE)"

echo "== scan =="
r FLUSHDB >/dev/null
for i in 1 2 3 4 5; do r SET "key:$i" "v$i" >/dev/null; done
r SET other:1 z >/dev/null
scan_all=$(r SCAN 0 COUNT 100 | tail -n +2 | sort | tr '\n' ' ')
check "SCAN 0 COUNT 100 keys"  "key:1 key:2 key:3 key:4 key:5 other:1 " "$scan_all"
scan_match=$(r SCAN 0 MATCH 'key:*' COUNT 100 | tail -n +2 | sort | tr '\n' ' ')
check "SCAN MATCH key:*"       "key:1 key:2 key:3 key:4 key:5 " "$scan_match"
scan_none=$(r SCAN 0 MATCH 'nomatch:*' COUNT 100 | tail -n +2 | grep -v '^$' | tr '\n' ' ')
check "SCAN MATCH no matches"  ""        "$scan_none"

# A full iteration driven by the cursor must visit every key exactly once.
cursor=0; seen=""; iters=0
while :; do
	out=$(r SCAN "$cursor" COUNT 2)
	cursor=$(head -1 <<<"$out")
	keys=$(tail -n +2 <<<"$out")
	[[ -n "$keys" ]] && seen+="$keys"$'\n'
	iters=$((iters+1))
	[[ "$cursor" == "0" || $iters -gt 50 ]] && break
done
cursor_all=$(printf '%s' "$seen" | grep -v '^$' | sort | tr '\n' ' ')
check "SCAN full cursor walk"  "key:1 key:2 key:3 key:4 key:5 other:1 " "$cursor_all"
check "SCAN terminates"        "yes"     "$([[ $iters -le 50 ]] && echo yes || echo no)"

echo "== info/compact/command =="
check "INFO has version"       "yes"     "$(r INFO | grep -q '^strata_version:' && echo yes || echo no)"
check "INFO has keyspace"      "yes"     "$(r INFO | grep -q '^db0:keys=' && echo yes || echo no)"
check "INFO has sync_policy"   "yes"     "$(r INFO | grep -q '^sync_policy:' && echo yes || echo no)"
check "COMPACT"                "OK"      "$(r COMPACT)"
check "COMMAND DOCS empty"     ""        "$(r COMMAND DOCS)"

echo "== arity and unknown-command errors =="
check "GET no args"      "ERR wrong number of arguments for 'get' command"  "$(r GET)"
check "SET one arg"      "ERR wrong number of arguments for 'set' command"  "$(r SET onlykey)"
check "DEL no args"      "ERR wrong number of arguments for 'del' command"  "$(r DEL)"
check "EXISTS no args"   "ERR wrong number of arguments for 'exists' command" "$(r EXISTS)"
check "ECHO no args"     "ERR wrong number of arguments for 'echo' command" "$(r ECHO)"
check "unknown command"  "yes"  "$(r NOSUCHCMD | grep -q '^ERR unknown command' && echo yes || echo no)"

echo "== case insensitivity =="
r FLUSHDB >/dev/null
check "lowercase set"          "OK"      "$(r set ci 1)"
check "MiXeD case get"         "1"       "$(r GeT ci)"
check "uppercase DBSIZE"       "1"       "$(r DBSIZE)"

echo
echo "passed=$PASS failed=$FAIL"
[[ $FAIL -eq 0 ]]
