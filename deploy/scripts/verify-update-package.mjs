#!/usr/bin/env node
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { lstat, mkdtemp, readFile, rm } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'

const [archiveArg, version, platform, kind = 'management'] = process.argv.slice(2)
assert(archiveArg && version && platform, 'usage: verify-update-package.mjs ARCHIVE VERSION PLATFORM')
const archive = path.resolve(archiveArg)
const archiveName = path.basename(archive), archiveDirectory = path.dirname(archive)
const sha256 = data => createHash('sha256').update(data).digest('hex')
const suffix = platform.startsWith('windows-') ? '.exe' : ''
const binaries = kind === 'agent' ? ['dst-admin-agent', 'dst-map-renderer', 'mod-local-setup'].map(name => name + suffix) : ['dst-admin', 'dst-map-renderer', 'mod-local-setup']
const names = [...binaries, 'manifest.json'].sort()
const entries = execFileSync('tar', ['-tzf', archiveName], { cwd: archiveDirectory, encoding: 'utf8' }).trim().split(/\r?\n/).sort()
assert.deepEqual(entries, names)
const checksum = (await readFile(archive + '.sha256', 'utf8')).trim().split(/\s+/)
assert.equal(checksum[0], sha256(await readFile(archive)))
assert.equal(checksum[1], path.basename(archive))
const directory = await mkdtemp(path.join(os.tmpdir(), 'dst-update-package-'))
try {
  execFileSync('tar', ['-xzf', archiveName, '-C', directory], { cwd: archiveDirectory })
  const manifest = JSON.parse(await readFile(path.join(directory, 'manifest.json'), 'utf8'))
  assert.equal(manifest.protocol, 1)
  assert.equal(manifest.version, version)
  assert.equal(manifest.platform, platform)
  if (kind === 'agent') assert.equal(manifest.kind, 'agent')
  else { assert.equal(manifest.embeddedWebUI, true); assert.match(manifest.frontendCommit, /^[a-f0-9]{40}$/) }
  assert.deepEqual(Object.keys(manifest.files).sort(), names.filter(name => name !== 'manifest.json'))
  for (const [name, digest] of Object.entries(manifest.files)) {
    const file = path.join(directory, name), stat = await lstat(file)
    assert(stat.isFile() && !stat.isSymbolicLink())
    assert.equal(stat.size, digest.size)
    assert.equal(sha256(await readFile(file)), digest.sha256)
  }
  if (kind === 'agent') {
    const nativeOS = process.platform === 'win32' ? 'windows' : process.platform
    const nativeArch = process.arch === 'x64' ? 'amd64' : process.arch
    const binary = path.join(directory, 'dst-admin-agent' + suffix)
    const info = execFileSync('go', ['version', '-m', binary], { encoding: 'utf8' })
    assert(info.includes('dont/cmd/agent') || info.includes('dont/agent/cmd/agent'))
    const [os, arch] = platform.split('-'); assert(info.includes(`GOOS=${os}`) && info.includes(`GOARCH=${arch}`))
    if (`${nativeOS}-${nativeArch}` === platform) {
      const metadata = JSON.parse(execFileSync(binary, ['-build-info'], { encoding: 'utf8', timeout: 10000 }))
      assert.equal(metadata.version, version); assert.equal(metadata.kind, 'agent')
    }
  } else {
    const metadata = JSON.parse(execFileSync(path.join(directory, 'dst-admin'), ['-version'], { encoding: 'utf8', timeout: 10000 }))
    assert.equal(metadata.version, version)
    assert.equal(metadata.frontendCommit, manifest.frontendCommit)
    assert.equal(metadata.embeddedWebUI, true)
  }
  console.log(`Online update bundle verified: ${kind} ${version} ${platform}; executable files and checksums`)
} finally { await rm(directory, { recursive: true, force: true }) }
