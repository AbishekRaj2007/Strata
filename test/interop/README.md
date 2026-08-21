# Interoperability checks (T1.3)

T1.3's *Done when* is a claim about real clients: `redis-cli` round-trips every
command in plan.md §7.5, and a Go program using `go-redis` works unmodified.
This directory is that claim, executed.

```sh
make build && test/interop/run.sh
```

Both suites print one line per assertion and a `passed=N failed=N` summary.

## What is checked beyond "it returns something"

The interesting assertions are the ones a hand-rolled client would not make:

- **`redis.Nil` on an absent key.** A client that cannot distinguish "missing"
  from "error" is broken, and this is why the engine interface returns
  `(value, error)` rather than `(value, bool)`.
- **Binary safety.** Values containing NUL, `0xff`, and a bare `\r\n` survive a
  round trip. A length-prefixed protocol implemented with string scanning
  passes every ASCII test and fails this one.
- **The `Scan` iterator.** go-redis drives the cursor itself, so a cursor that
  fails to advance hangs and one that skips loses keys. Neither is visible from
  a single `SCAN 0` call.
- **Reply synchronisation after an error.** Two protocol errors are issued and
  the connection is then reused. This is the specific failure T1.3 already hit
  once: `validateKeys` returned the result of `WriteError`, which is nil on
  success, so a rejected command wrote a second reply and left every later
  reply on that connection off by one.

## Why `goredis/` is a separate module

`go-redis` is not on the permitted dependency list in CLAUDE.md, and a
verification program is not a reason to add it. A nested module is excluded
from the parent module's package patterns, so `go build ./...`, `go test ./...`
and CI never see it, and `go.mod` at the repository root stays limited to the
approved set.

The cost is that this suite needs network access on first run to fetch
`go-redis` into the module cache. That is why it is a separate `make interop`
target rather than part of `make test`.
