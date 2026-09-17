#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { cp, mkdir, mkdtemp, readFile, readdir, rename, rm, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const backend = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const options = {}
for (let index = 2; index < process.argv.length; index += 2) {
  const name = process.argv[index]
  if (!['--version', '--frontend', '--output', '--binaries', '--platform', '--web-root'].includes(name) || !process.argv[index + 1]) {
    throw new Error('usage: build-native-release.mjs --version VERSION --frontend PATH --output PATH [--binaries PATH --platform linux-amd64] [--web-root PATH]')
  }
  options[name.slice(2)] = process.argv[index + 1]
}
if (!/^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$/.test(options.version || '')) throw new Error('a filename-safe version is required')
if (!options.frontend || !options.output) throw new Error('--frontend and --output are required')
const frontend = path.resolve(options.frontend)
const output = path.resolve(options.output)
const run = (command, args, cwd = backend) => execFileSync(command, args, { cwd, encoding: 'utf8' }).trim()
const digest = data => createHash('sha256').update(data).digest('hex')
const revision = root => ({ commit: run('git', ['rev-parse', 'HEAD'], root), dirty: run('git', ['status', '--porcelain'], root) !== '' })
const platform = options.platform || `${run('go', ['env', 'GOOS'])}-${run('go', ['env', 'GOARCH'])}`
if (!['linux-amd64', 'linux-arm64', 'darwin-arm64', 'darwin-amd64'].includes(platform)) throw new Error('unsupported platform')
const name = `dst-admin-${options.version}-${platform}`
await mkdir(output, { recursive: true })
const stage = await mkdtemp(path.join(output, '.native-release-'))
try {
  const release = path.join(stage, name)
  await mkdir(release)
  const backendSource = revision(backend)
  const frontendSource = revision(frontend)
  const buildTime = new Date(process.env.SOURCE_DATE_EPOCH ? Number(process.env.SOURCE_DATE_EPOCH) * 1000 : Date.now()).toISOString()
  for (const [binary, command] of [['dst-admin', 'admin-api'], ['dst-admin-agent', 'agent'], ['dst-map-renderer', 'dst-map-renderer'], ['mod-local-setup', 'mod-local-setup']]) {
    const destination = path.join(release, binary)
    if (options.binaries) {
      const source = path.resolve(options.binaries, binary)
      const info = run('go', ['version', '-m', source])
      const [os, arch] = platform.split('-')
      if (!info.includes(`GOOS=${os}`) || !info.includes(`GOARCH=${arch}`)) throw new Error(`${binary} does not match ${platform}`)
      await cp(source, destination)
    } else {
      const native = `${run('go', ['env', 'GOOS'])}-${run('go', ['env', 'GOARCH'])}`
      if (native !== platform) throw new Error('cross-platform CGO builds must supply --binaries built on that platform')
      execFileSync('go', ['build', '-trimpath', '-ldflags', `-s -w -X dont/internal/buildinfo.Version=${options.version} -X dont/internal/buildinfo.Commit=${backendSource.commit} -X dont/internal/buildinfo.BuildTime=${buildTime}`, '-o', destination, `./cmd/${command}`], { cwd: backend, stdio: 'inherit' })
    }
  }
  if (!options['web-root']) execFileSync('npm', ['run', 'build'], { cwd: frontend, stdio: 'inherit' })
  const web = path.resolve(options['web-root'] || path.join(frontend, 'dist'))
  await readFile(path.join(web, 'index.html'))
  await cp(web, path.join(release, 'public'), { recursive: true })
  await cp(path.join(backend, 'deploy'), path.join(release, 'deploy'), { recursive: true })
  await writeFile(path.join(release, 'VERSION'), options.version + '\n')
  const manifest = {
    version: options.version, platform, buildTime,
    backend: backendSource, frontend: frontendSource,
    go: run('go', ['version']), node: process.version,
    goSumSHA256: digest(await readFile(path.join(backend, 'go.sum'))),
    frontendLockSHA256: digest(await readFile(path.join(frontend, 'package-lock.json')))
  }
  await writeFile(path.join(release, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n')
  const sums = []
  async function visit(relative = '') {
    const entries = await readdir(path.join(release, relative), { withFileTypes: true })
    entries.sort((left, right) => left.name < right.name ? -1 : left.name > right.name ? 1 : 0)
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
  // Exclusive creation prevents accidentally replacing an already delivered version.
  await writeFile(path.join(output, name + '.tar.gz'), await readFile(archive), { flag: 'wx' })
  await writeFile(path.join(output, name + '.tar.gz.sha256'), `${digest(await readFile(archive))}  ${name}.tar.gz\n`, { flag: 'wx' })
  await rename(release, path.join(output, name))
  console.log(path.join(output, name + '.tar.gz'))
} finally {
  await rm(stage, { recursive: true, force: true })
}
