#!/usr/bin/env node
'use strict'

// Writes one version into all three package.json files and pins the main
// package's optionalDependencies to that exact version. esbuild-style
// per-arch packages break silently if the main package's optional
// dependency range ever admits a platform package built from different
// source, so this keeps them locked together rather than trusting a range.

const fs = require('fs')
const path = require('path')

const version = process.argv[2]
if (!version) {
  console.error('usage: stamp-version.js <version>')
  process.exit(1)
}

const root = __dirname
const mainPath = path.join(root, 'strata', 'package.json')
const x64Path = path.join(root, 'strata-linux-x64', 'package.json')
const arm64Path = path.join(root, 'strata-linux-arm64', 'package.json')

function readJSON(p) {
  return JSON.parse(fs.readFileSync(p, 'utf8'))
}

function writeJSON(p, obj) {
  fs.writeFileSync(p, JSON.stringify(obj, null, 2) + '\n')
}

const main = readJSON(mainPath)
const x64 = readJSON(x64Path)
const arm64 = readJSON(arm64Path)

main.version = version
x64.version = version
arm64.version = version
main.optionalDependencies[x64.name] = version
main.optionalDependencies[arm64.name] = version

writeJSON(mainPath, main)
writeJSON(x64Path, x64)
writeJSON(arm64Path, arm64)

console.log(`stamped version ${version} into strata, strata-linux-x64, strata-linux-arm64`)
