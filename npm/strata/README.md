# @abishekraj2007/strata

npm distribution for [Strata](https://github.com/AbishekRaj2007/Strata), a
persistent, log-structured key-value store with a Redis-compatible wire
protocol. This package installs the prebuilt `strata-server` and
`strata-cli` native binaries for your platform — there is no Node.js
runtime involved beyond a thin launcher script.

Linux only (`x64` and `arm64`), matching the project's own scope.

## Install

```sh
npm install -g @abishekraj2007/strata
```

## Use

```sh
strata-server -addr :6380 -data-dir ./data &
redis-cli -p 6380 SET foo bar
redis-cli -p 6380 GET foo

strata-cli --help
```

See the [main repository](https://github.com/AbishekRaj2007/Strata) for
architecture, on-disk format, and benchmark documentation.
