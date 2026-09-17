#!/usr/bin/env node
import { execFileSync } from 'node:child_process'
import path from 'node:path'
import { withSources } from './release-sources.mjs'

const options = {}
for (let index = 2; index < process.argv.length; index++) {
  const name = process.argv[index]
  if (['--push', '--load'].includes(name)) { options[name.slice(2)] = true; continue }
  if (!['--kind', '--tag', '--frontend', '--frontend-ref', '--frontend-repository', '--version', '--platform', '--debian-mirror', '--debian-security-mirror', '--cache-from', '--cache-to'].includes(name) || !process.argv[index + 1]) throw new Error(`unknown or missing build argument: ${name}`)
  options[name.slice(2)] = process.argv[++index]
}
const kind = options.kind || 'all-in-one'
if (!['all-in-one', 'control-plane', 'agent', 'dst-runtime'].includes(kind)) throw new Error('unsupported image kind')
options.web = ['all-in-one', 'control-plane'].includes(kind)
if (options.push && options.load) throw new Error('choose --push or --load')
await withSources(options, async ({ stage, backend, backendSource, frontendSource }) => {
  const version = options.version || 'dev'
  if (!/^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$/.test(version)) throw new Error('invalid version')
  const args = ['buildx', 'build', '--platform', options.platform || 'linux/amd64', '--progress', 'plain',
    '--file', path.join(backend, `deploy/docker/Dockerfile.${kind}`),
    '--tag', options.tag || `dst-admin/${kind}:dev`,
    '--build-arg', `VERSION=${version}`, '--build-arg', `COMMIT=${backendSource.commit}`,
    '--build-arg', `BUILD_TIME=${new Date().toISOString()}`,
    '--label', 'org.opencontainers.image.source=https://github.com/lcy0828/dst-admin-go',
    '--label', `org.opencontainers.image.revision=${backendSource.commit}`,
    '--label', `org.opencontainers.image.version=${version}`]
  if (frontendSource) args.push('--build-arg', `FRONTEND_COMMIT=${frontendSource.commit}`, '--label', `io.dst-admin.frontend.commit=${frontendSource.commit}`)
  for (const [option, arg] of [['debian-mirror', 'DEBIAN_MIRROR'], ['debian-security-mirror', 'DEBIAN_SECURITY_MIRROR']]) {
    const value = options[option] || process.env[`DST_ADMIN_${arg}`]
    if (value) args.push('--build-arg', `${arg}=${value}`)
  }
  for (const key of ['cache-from', 'cache-to']) if (options[key]) args.push(`--${key}`, options[key])
  args.push(options.push ? '--push' : '--load', options.web ? stage : backend)
  execFileSync('docker', args, { stdio: 'inherit' })
})
