#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { cp, mkdir, mkdtemp, readFile, readdir, rename, rm, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { backendRoot, buildFrontend, run, withSources } from './release-sources.mjs'

const options = {}
for (let index = 2; index < process.argv.length; index += 2) {
  const name = process.argv[index]
  if (!['--version', '--frontend', '--frontend-ref', '--frontend-repository', '--output', '--binaries', '--platform'].includes(name) || !process.argv[index + 1]) {
    throw new Error('usage: build-native-release.mjs --version VERSION [--output dist] [--frontend-ref master | --frontend PATH] [--frontend-repository URL] [--binaries PATH --platform linux-amd64]')
  }
  options[name.slice(2)] = process.argv[index + 1]
}
if (!/^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$/.test(options.version || '')) throw new Error('a filename-safe version is required')
const output = path.resolve(options.output || path.join(backendRoot, 'dist'))
const digest = data => createHash('sha256').update(data).digest('hex')
const native = `${run('go', ['env', 'GOOS'])}-${run('go', ['env', 'GOARCH'])}`
const platform = options.platform || native
if (!['linux-amd64', 'linux-arm64', 'darwin-arm64', 'darwin-amd64'].includes(platform)) throw new Error('unsupported platform')
if (!options.binaries && native !== platform) throw new Error('CGO release builds must run on the target platform, or supply matching --binaries')
const name = `dst-admin-${options.version}-${platform}`
await mkdir(output, { recursive: true })
const stage = await mkdtemp(path.join(output, '.native-release-'))
try {
  await withSources(options, async sources => {
    const { backend, frontend, backendSource, frontendSource } = sources
    await buildFrontend(sources)
    execFileSync('go', ['test', '-tags', 'webui', './internal/webui'], { cwd: backend, stdio: 'inherit' })
    const release = path.join(stage, name)
    await mkdir(release)
    const buildTime = new Date(process.env.SOURCE_DATE_EPOCH ? Number(process.env.SOURCE_DATE_EPOCH) * 1000 : Date.now()).toISOString()
    const flags = `-s -w -X dont/internal/buildinfo.Version=${options.version} -X dont/internal/buildinfo.Commit=${backendSource.commit} -X dont/internal/buildinfo.BuildTime=${buildTime}`
    for (const [binary, command] of [['dst-admin', 'admin-api'], ['dst-admin-agent', 'agent'], ['dst-map-renderer', 'dst-map-renderer'], ['mod-local-setup', 'mod-local-setup']]) {
      const destination = path.join(release, binary)
      if (options.binaries) {
        const source = path.resolve(options.binaries, binary)
        const info = run('go', ['version', '-m', source])
        const [os, arch] = platform.split('-')
        if (!info.includes(`GOOS=${os}`) || !info.includes(`GOARCH=${arch}`)) throw new Error(`${binary} does not match ${platform}`)
        if (binary === 'dst-admin' && (!info.includes('-tags=webui') || !info.includes(`dont/internal/buildinfo.FrontendCommit=${frontendSource.commit}`))) {
          throw new Error('prebuilt dst-admin must embed the selected frontend commit')
        }
        await cp(source, destination)
      } else {
        const args = ['build', '-trimpath']
        if (binary === 'dst-admin') args.push('-tags', 'webui')
        args.push('-ldflags', flags + (binary === 'dst-admin' ? ` -X dont/internal/buildinfo.FrontendCommit=${frontendSource.commit}` : ''), '-o', destination, `./cmd/${command}`)
        execFileSync('go', args, { cwd: backend, stdio: 'inherit', env: { ...process.env, CGO_ENABLED: binary === 'dst-admin' ? '1' : '0' } })
      }
    }
    await cp(path.join(backend, 'deploy'), path.join(release, 'deploy'), { recursive: true })
    for (const readme of ['README.md', 'README.en.md']) await cp(path.join(backendRoot, readme), path.join(release, readme))
    // Only public tracked docs belong in the package, never ignored research.
    for (const name of run('git', ['ls-files', '-z', '--', 'docs', 'internal/luajit/packages/README.md'], backendRoot).split('\0').filter(Boolean)) {
      const source = path.join(backendRoot, name)
      try { await readFile(source) } catch (error) { if (error.code === 'ENOENT') continue; throw error }
      const target = path.join(release, name)
      await mkdir(path.dirname(target), { recursive: true })
      await cp(source, target)
    }
    await writeFile(path.join(release, 'VERSION'), options.version + '\n')
    const manifest = {
      version: options.version, platform, buildTime, embeddedWebUI: true,
      backend: backendSource, frontend: frontendSource,
      go: run('go', ['version']), node: process.version,
      goSumSHA256: digest(await readFile(path.join(backend, 'go.sum'))),
      frontendLockSHA256: digest(await readFile(path.join(frontend, 'package-lock.json')))
    }
    await writeFile(path.join(release, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n')
    const sums = []
    async function visit(relative = '') {
      const entries = await readdir(path.join(release, relative), { withFileTypes: true })
      entries.sort((left, right) => left.name.localeCompare(right.name, 'en'))
      for (const entry of entries) {
        const name = path.posix.join(relative, entry.name)
        if (entry.isDirectory()) await visit(name)
        else if (entry.isFile()) sums.push(`${digest(await readFile(path.join(release, name)))}  ${name}`)
        else throw new Error(`release contains a non-regular file: ${name}`)
      }
    }
    await visit()
    await writeFile(path.join(release, 'SHA256SUMS'), sums.join('\n') + '\n')
    const archive = path.join(stage, name + '.tar.gz')
    execFileSync('tar', ['-czf', archive, '-C', stage, name], { stdio: 'inherit' })
    await writeFile(path.join(output, name + '.tar.gz'), await readFile(archive), { flag: 'wx' })
    await writeFile(path.join(output, name + '.tar.gz.sha256'), `${digest(await readFile(archive))}  ${name}.tar.gz\n`, { flag: 'wx' })
    // Stable asset names let the installation page use releases/latest/download
    // without querying GitHub's API or hard-coding a release version.
    const agentName = `dst-admin-agent-${platform}.tar.gz`
    const agentStage = path.join(stage, 'agent')
    await mkdir(agentStage)
    await cp(path.join(release, 'dst-admin-agent'), path.join(agentStage, 'dst-admin-agent'))
    await cp(path.join(backend, 'deploy/systemd/agent.conf.example'), path.join(agentStage, 'agent.conf.example'))
    const agentArchive = path.join(stage, agentName)
    execFileSync('tar', ['-czf', agentArchive, '-C', agentStage, 'dst-admin-agent', 'agent.conf.example'], { stdio: 'inherit' })
    const agentData = await readFile(agentArchive)
    await writeFile(path.join(output, agentName), agentData, { flag: 'wx' })
    await writeFile(path.join(output, agentName + '.sha256'), `${digest(agentData)}  ${agentName}\n`, { flag: 'wx' })
    await rename(release, path.join(output, name))
    console.log(path.join(output, name + '.tar.gz'))
  })
} finally {
  await rm(stage, { recursive: true, force: true })
}
