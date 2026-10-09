#!/usr/bin/env node
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import path from 'node:path'

const [outputArg, version, platform] = process.argv.slice(2)
assert(outputArg && /^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$/.test(version || '') && ['linux-amd64', 'linux-arm64', 'darwin-amd64', 'darwin-arm64', 'windows-amd64'].includes(platform), 'usage: build-agent-release.mjs OUTPUT VERSION PLATFORM')
const output = path.resolve(outputArg), [GOOS, GOARCH] = platform.split('-'), suffix = GOOS === 'windows' ? '.exe' : ''
const hash = data => createHash('sha256').update(data).digest('hex')
await mkdir(output, { recursive: true })
const stage = await mkdtemp(path.join(output, '.agent-release-'))
try {
  const commit = execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim()
  const flags = `-s -w -X dont/internal/buildinfo.Version=${version} -X dont/internal/buildinfo.Commit=${commit} -X dont/internal/buildinfo.BuildTime=${new Date().toISOString()}`
  const files = {}
  for (const [name, command] of [['dst-admin-agent', 'agent'], ['dst-map-renderer', 'dst-map-renderer'], ['mod-local-setup', 'mod-local-setup']]) {
    const binary = name + suffix
    execFileSync('go', ['build', '-trimpath', '-ldflags', flags, '-o', path.join(stage, binary), `./cmd/${command}`], { stdio: 'inherit', env: { ...process.env, CGO_ENABLED: '0', GOOS, GOARCH } })
    const data = await readFile(path.join(stage, binary)); files[binary] = { size: data.length, sha256: hash(data) }
  }
  await writeFile(path.join(stage, 'manifest.json'), JSON.stringify({ protocol: 1, kind: 'agent', version, platform, files }, null, 2) + '\n')
  async function archive(name, entries) {
    const destination = path.join(output, name)
    execFileSync('tar', ['-czf', destination, '-C', stage, ...entries], { stdio: 'inherit' })
    await writeFile(destination + '.sha256', `${hash(await readFile(destination))}  ${name}\n`)
  }
  await archive(`dst-admin-agent-update-${platform}.tar.gz`, [...Object.keys(files), 'manifest.json'])
  await cp('deploy/systemd/agent.conf.example', path.join(stage, 'agent.conf.example'))
  await archive(`dst-admin-agent-${platform}.tar.gz`, ['dst-admin-agent' + suffix, 'agent.conf.example'])
  console.log(`Agent release built: ${version} ${platform}`)
} finally { await rm(stage, { recursive: true, force: true }) }
