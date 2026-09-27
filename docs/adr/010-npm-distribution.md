# ADR-010: npm distribution via per-platform optionalDependencies

**Status:** Accepted

## Context

T9.3 ships Strata as raw cross-compiled binaries (`make dist`) and a Docker
image on GHCR. Neither is a one-line install for someone who just wants to
try the server. This is not a task any phase originally scoped — it was
added afterward as T9.6 — so the decision needs the same discipline as any
other technical choice rather than a quick bolt-on.

Strata compiles to a single static Go binary (`CGO_ENABLED=0`, per ADR-001
and the Dockerfile), so npm here is purely a distribution channel: no
Node.js runtime is involved in running the database. The only question is
how the native binary gets onto the user's machine.

Two established patterns exist for shipping a native binary through npm:

1. **A `postinstall` script that downloads the binary** for the current
   platform from a release URL at install time.
2. **Per-platform packages listed as `optionalDependencies`**, each
   containing a prebuilt binary; npm's own `os`/`cpu` matching installs only
   the one that matches the current machine. This is what `esbuild` and
   `swc` do.

## Decision

Per-platform `optionalDependencies`, no `postinstall` script, matching the
project's real platform scope: `linux/x64` and `linux/arm64` only, packaged
as `@abishekraj2007/strata-linux-x64` and `@abishekraj2007/strata-linux-arm64`.
The main package, `@abishekraj2007/strata`, declares `"os": ["linux"]` and
ships two thin launcher scripts (`bin/strata-server.js`, `bin/strata-cli.js`)
that resolve the matching platform package and `spawn` the real binary,
forwarding `SIGINT`/`SIGTERM` and the child's exit status.

Package names are scoped under `@abishekraj2007` rather than published
unscoped. Checked against the registry before naming anything: `strata-server`
and `strata-cli` unscoped are both already taken by unrelated packages.

## Alternatives considered

**A `postinstall` download script.** Simpler package layout — one package,
no `optionalDependencies` wiring. Rejected because it reintroduces exactly
the supply-chain shape npm's own security advisories warn about: arbitrary
code (a script controlled by the package publisher) runs automatically at
install time with the installing user's permissions, fetching a binary over
the network from wherever the script points. The per-platform-package
approach puts the binary inside the published npm tarball itself, covered by
the registry's own integrity hash, with nothing executing at install time.

**A universal npm package covering macOS and Windows too.** Would match what
most npm CLI tools do. Rejected outright: `plan.md`'s non-goals are explicit
about Linux-only (`fsync` semantics differ enough on Windows to be a
distraction), and CLAUDE.md forbids Windows compatibility layers. Shipping
binaries for platforms the project has deliberately never built, tested, or
crash-tested on would be a correctness claim with nothing behind it.

**Bundling both binaries directly in the main package**, keyed by
`process.platform`/`process.arch` at runtime with no `optionalDependencies`
split. Rejected because every install pulls down every platform's binary
regardless of which one is needed, doubling package size for no benefit at
this scale (2 platforms), and the pattern doesn't extend if a third platform
is ever added.

## Consequences

**Accepted:** three packages to version and publish together instead of one.
`npm/stamp-version.js` keeps all three package.json versions and the main
package's `optionalDependencies` pins in lockstep, because a version skew
between the main package and its platform package is exactly the failure
mode this pattern is prone to.

**Accepted:** an npm install on an unsupported platform (anything non-Linux,
or a Linux architecture that isn't x64/arm64) installs successfully — npm
just skips the unmatched optional dependency — and only fails when the user
actually runs `strata-server`, with an explicit error naming the unsupported
platform. This is `optionalDependencies`' documented behavior, not a gap
specific to this package.

**Gained:** publishing is `npm publish` per package with no build step
server-side and no code exposed to execute during anyone's `npm install` —
consistent with CLAUDE.md's preference for boring, auditable release
mechanics over convenience.

**Critical detail:** the launcher scripts use asynchronous `spawn`, not
`spawnSync`. Strata's graceful-shutdown path depends on `SIGTERM` reaching
the process to drain connections and flush before exit; `spawnSync` blocks
the wrapper's event loop inside a single `wait()` call, which never gives a
JS signal handler the chance to run, so a `SIGTERM` sent to the npm-launched
process would kill the Node wrapper without ever reaching the real binary —
discovered by `test/npm/verify.sh` orphaning the server process on first
cleanup attempt, before this file existed to explain why.
