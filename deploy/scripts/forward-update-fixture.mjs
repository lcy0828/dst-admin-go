// Isolated lifecycle tests must upgrade to a strictly newer program. These
// synthetic-version bundles are test artifacts and never enter Release assets.
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import os from 'node:os'
import path from 'node:path'
import { pathToFileURL } from 'node:url'
import { buildFrontend, withSources } from './release-sources.mjs'

export function nextFixtureVersion(version) {
  const match = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(version)
  if (!match) return null
  const numbers = match.slice(1).map(Number)
  assert(numbers.every(value => value <= 4294967295), 'fixture version exceeds the update protocol')
  for (let i = 2; i >= 0; i--) {
    if (numbers[i] < 4294967295) { numbers[i]++; return 'v' + numbers.join('.') }
    numbers[i] = 0
  }
  throw Error('no newer fixture version is representable')
}

export async function buildForwardFixture(options) {
  const source = path.resolve(options.bundle), output = path.resolve(options.output)
  assert(source !== output, 'fixture must not overwrite a release bundle')
  await mkdir(path.dirname(output), { recursive: true })
  const stage = await mkdtemp(path.join(os.tmpdir(), 'dst-forward-update-'))
  try {
    execFileSync('tar', ['-xzf', source, '-C', stage])
    const manifest = JSON.parse(await readFile(path.join(stage, 'manifest.json'), 'utf8'))
    const version = nextFixtureVersion(manifest.version)
    if (!version) { await cp(source, output); return }
    assert.equal(manifest.protocol, 1)
    assert(['linux-amd64', 'darwin-arm64'].includes(manifest.platform), 'fixture must be built on its native package runner')
    const agent = manifest.kind === 'agent'
    assert(agent || manifest.embeddedWebUI && /^[a-f0-9]{40}$/.test(manifest.frontendCommit))
    const binary = agent ? 'dst-admin-agent' : 'dst-admin'
    await withSources({ web: !agent, frontend: options.frontend, 'frontend-ref': manifest.frontendCommit, 'frontend-dist': options.webui }, async sources => {
      if (!agent) await buildFrontend(sources)
      const flags = `-s -w -X dont/internal/buildinfo.Version=${version} -X dont/internal/buildinfo.Commit=${sources.backendSource.commit}` + (agent ? '' : ` -X dont/internal/buildinfo.FrontendCommit=${manifest.frontendCommit}`)
      execFileSync('go', ['build', '-trimpath', ...(agent ? [] : ['-tags', 'webui']), '-ldflags', flags, '-o', path.join(stage, binary), agent ? './cmd/agent' : './cmd/admin-api'], { cwd: sources.backend, env: { ...process.env, CGO_ENABLED: agent ? '0' : '1' }, stdio: 'inherit' })
    })
    const data = await readFile(path.join(stage, binary))
    assert(manifest.files[binary], 'missing executable in package manifest')
    manifest.files[binary] = { size: data.length, sha256: createHash('sha256').update(data).digest('hex') }
    manifest.version = version
    await writeFile(path.join(stage, 'manifest.json'), JSON.stringify(manifest) + '\n')
    execFileSync('tar', ['-czf', output, '-C', stage, ...Object.keys(manifest.files), 'manifest.json'], { stdio: 'inherit' })
    console.log(`Forward update fixture: ${version} (${manifest.platform}, ${agent ? 'agent' : 'controller'})`)
  } finally { await rm(stage, { recursive: true, force: true }) }
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const options = {}
  for (let i = 2; i < process.argv.length; i += 2) {
    assert(['--bundle', '--output', '--frontend', '--webui'].includes(process.argv[i]) && process.argv[i + 1], 'usage: forward-update-fixture.mjs --bundle FILE --output FILE [--frontend DIR --webui DIR]')
    options[process.argv[i].slice(2)] = process.argv[i + 1]
  }
  assert(options.bundle && options.output)
  await buildForwardFixture(options)
}
