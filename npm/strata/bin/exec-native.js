'use strict'

// Shared by the strata-server and strata-cli shims. The actual binaries live
// in per-platform optionalDependencies packages (@abishekraj2007/strata-linux-x64,
// @abishekraj2007/strata-linux-arm64) so npm's own os/cpu matching -- not a
// postinstall script -- decides which native binary lands on disk. No
// runtime download, nothing executes at install time.

const path = require('path')
const { spawn } = require('child_process')

function platformPackage() {
  if (process.platform !== 'linux') {
    return null
  }
  switch (process.arch) {
    case 'x64':
      return '@abishekraj2007/strata-linux-x64'
    case 'arm64':
      return '@abishekraj2007/strata-linux-arm64'
    default:
      return null
  }
}

function resolveBinary(name) {
  const pkg = platformPackage()
  if (!pkg) {
    console.error(
      `strata: unsupported platform ${process.platform}/${process.arch}. ` +
      'Strata ships linux/x64 and linux/arm64 binaries only -- see plan.md\'s non-goals.'
    )
    process.exit(1)
  }
  let pkgJsonPath
  try {
    pkgJsonPath = require.resolve(`${pkg}/package.json`)
  } catch (err) {
    console.error(
      `strata: optional dependency ${pkg} is not installed. ` +
      'Reinstall with "npm install --include=optional".'
    )
    process.exit(1)
  }
  return path.join(path.dirname(pkgJsonPath), 'bin', name)
}

// run must use an asynchronous spawn, not spawnSync. Strata treats SIGTERM
// as "drain connections, flush, exit clean" -- it's the difference between
// the graceful shutdown path and relying on crash recovery. spawnSync blocks
// the whole event loop inside a single wait() call, which never gives Node's
// JS signal handlers a chance to run, so a SIGTERM delivered to this wrapper
// would kill the wrapper without ever reaching the real server. It has to be
// forwarded explicitly.
function run(name) {
  const bin = resolveBinary(name)
  const child = spawn(bin, process.argv.slice(2), { stdio: 'inherit' })

  const forward = (signal) => () => {
    child.kill(signal)
  }
  const sigint = forward('SIGINT')
  const sigterm = forward('SIGTERM')
  process.on('SIGINT', sigint)
  process.on('SIGTERM', sigterm)

  child.on('error', (err) => {
    console.error(`strata: failed to launch ${bin}: ${err.message}`)
    process.exit(1)
  })

  child.on('exit', (code, signal) => {
    process.off('SIGINT', sigint)
    process.off('SIGTERM', sigterm)
    if (signal) {
      // Re-raise the same signal against ourselves so the parent (a shell,
      // systemd, npm) sees the process die the way it actually died,
      // rather than a synthesized exit code standing in for a signal.
      process.kill(process.pid, signal)
      return
    }
    process.exit(code === null ? 1 : code)
  })
}

module.exports = { run }
