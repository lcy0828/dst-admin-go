#!/usr/bin/env node
// Copies published manifests and layers; never builds or selects a host architecture.
import assert from 'node:assert/strict'
import { execFile, spawnSync } from 'node:child_process'
import path from 'node:path'
import { promisify } from 'node:util'
import { pathToFileURL } from 'node:url'

const roles = [['all-in-one', ''], ['control-plane', 'controller-'], ['agent', 'agent-'], ['dst-runtime', 'runtime-']]
const execute = promisify(execFile)

export function parseOptions(args, env = process.env) {
  const options = { source: 'dockerhub', target: `${env.DST_ADMIN_ALIYUN_REGISTRY || 'registry.cn-hangzhou.aliyuncs.com'}/${env.DST_ADMIN_ALIYUN_NAMESPACE || 'dstadmin'}/dst-admin-go`, latest: false, dryRun: false }
  for (let index = 0; index < args.length; index++) {
    const name = args[index]
    if (name === '--promote-latest') options.latest = true
    else if (name === '--dry-run') options.dryRun = true
    else {
      assert(['--version', '--source', '--target', '--crane'].includes(name) && args[index + 1] && !args[index + 1].startsWith('--'), `unknown or missing argument: ${name}`)
      options[name.slice(2)] = args[++index]
    }
  }
  assert(/^v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$/.test(options.version || ''), '--version vX.Y.Z is required')
  assert(['dockerhub', 'ghcr'].includes(options.source), '--source must be dockerhub or ghcr')
  assert(/^registry\.[a-z0-9-]+\.aliyuncs\.com\/[a-z0-9][a-z0-9_-]*\/dst-admin-go$/.test(options.target), 'target must be an Alibaba Cloud dst-admin-go repository')
  assert(!options.latest || /^v\d+\.\d+\.\d+$/.test(options.version), 'prereleases cannot update latest')
  return options
}

async function parallel(items, run) {
  let next = 0, failure
  await Promise.all(Array.from({ length: Math.min(2, items.length) }, async () => {
    while (!failure && next < items.length) {
      const item = items[next++]
      try { await run(item) } catch (error) { failure ||= error }
    }
  }))
  if (failure) throw failure
}

export async function syncAliyun(options, run, report = console.log) {
  const digest = async ref => {
    const value = (await run(['digest', ref])).trim()
    assert(/^sha256:[a-f0-9]{64}$/.test(value), `invalid manifest digest: ${ref}`)
    return value
  }
  const plans = roles.map(([kind, prefix]) => {
    const repository = options.source === 'dockerhub' ? 'docker.io/lcy0828/dst-admin-go' : `ghcr.io/lcy0828/dst-admin-go/${kind}`
    const sourcePrefix = options.source === 'dockerhub' ? prefix : ''
    return { kind, source: `${repository}:${sourcePrefix}${options.version}`, sourceLatest: `${repository}:${sourcePrefix}latest`, target: `${options.target}:${prefix}${options.version}`, targetLatest: `${options.target}:${prefix}latest` }
  })
  // Resolve every source before any target write. Keep immutable digests for the whole copy.
  await parallel(plans, async plan => {
    plan.digest = await digest(plan.source)
    if (options.latest) assert.equal(await digest(plan.sourceLatest), plan.digest, `${plan.kind}: requested version is not the source latest; refusing to overwrite latest`)
  })
  for (const plan of plans) report(`${options.dryRun ? 'Would copy' : 'Copy'} ${plan.source}@${plan.digest} -> ${plan.target}`)
  if (options.dryRun) return plans
  const copy = async (plan, target) => {
    await run(['copy', '--jobs', '2', `${plan.source.split(':').slice(0, -1).join(':')}@${plan.digest}`, target])
    assert.equal(await digest(target), plan.digest, `copied manifest does not match: ${target}`)
  }
  await parallel(plans, plan => copy(plan, plan.target))
  if (options.latest) {
    // A failed copy never advances latest. Also refuse an old version if a newer release appeared.
    await parallel(plans, async plan => assert.equal(await digest(plan.sourceLatest), plan.digest, `${plan.kind}: source latest changed during synchronization`))
    await parallel(plans, plan => copy(plan, plan.targetLatest))
  }
  report(`Verified ${plans.length} Alibaba Cloud images${options.latest ? ' and latest aliases' : ''}.`)
  return plans
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  const options = parseOptions(process.argv.slice(2))
  const installed = options.crane || process.env.DST_ADMIN_CRANE || 'crane'
  const probe = spawnSync(installed, ['version'], { stdio: 'ignore', timeout: 10000 })
  const command = probe.error?.code === 'ENOENT' && !options.crane && !process.env.DST_ADMIN_CRANE
    ? ['go', ['run', 'github.com/google/go-containerregistry/cmd/crane@v0.20.6']]
    : [installed, []]
  await syncAliyun(options, async args => {
    const { stdout } = await execute(command[0], [...command[1], ...args], { timeout: 10 * 60 * 1000, maxBuffer: 2 * 1024 * 1024 })
    return stdout
  })
}
