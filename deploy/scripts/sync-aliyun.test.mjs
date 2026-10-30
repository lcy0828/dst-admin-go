import assert from 'node:assert/strict'
import test from 'node:test'
import { parseOptions, syncAliyun } from './sync-aliyun.mjs'

const hash = 'sha256:' + 'a'.repeat(64)
const other = 'sha256:' + 'b'.repeat(64)
const defaults = extra => parseOptions(['--version', 'v1.2.3', ...extra], {})
const silent = () => {}

test('requires an explicit version and rejects unsafe targets and prerelease latest', () => {
  assert.throws(() => parseOptions([], {}), /required/)
  assert.throws(() => defaults(['--target', 'example.com/private/image']), /Alibaba Cloud/)
  assert.throws(() => parseOptions(['--version', 'v1.2.3-rc.1', '--promote-latest'], {}), /prereleases/)
  assert.throws(() => defaults(['--source', 'untrusted']), /source/)
})

test('dry run reads sources without writing registry tags', async () => {
  const calls = []
  const plans = await syncAliyun(defaults(['--dry-run']), async args => { calls.push(args); return hash }, silent)
  assert.equal(plans.length, 4)
  assert(calls.every(args => args[0] === 'digest'))
})

test('copies immutable manifests and verifies all versions before promoting latest', async () => {
  const calls = []
  await syncAliyun(defaults(['--promote-latest']), async args => { calls.push(args); return hash }, silent)
  const copies = calls.filter(args => args[0] === 'copy')
  assert.equal(copies.length, 8)
  assert(copies.every(args => args[3].endsWith('@' + hash)))
  assert(copies.slice(0, 4).every(args => args[4].endsWith('v1.2.3')))
  assert(copies.slice(4).every(args => args[4].endsWith('latest')))
})

test('an old version cannot overwrite latest', async () => {
  const calls = []
  await assert.rejects(syncAliyun(defaults(['--promote-latest']), async args => { calls.push(args); return args[1].endsWith('latest') ? other : hash }, silent), /refusing/)
  assert(calls.every(args => args[0] === 'digest'))
})

test('failed or mismatched copies do not promote latest', async () => {
  for (const mode of ['failed', 'mismatch']) {
    const calls = []
    await assert.rejects(syncAliyun(defaults(['--promote-latest']), async args => {
      calls.push(args)
      if (mode === 'failed' && args[0] === 'copy') throw Error('upload failed')
      return args[0] === 'digest' && args[1].startsWith('registry.') ? other : hash
    }, silent), /failed|match/)
    assert(!calls.some(args => args[0] === 'copy' && args[4].endsWith('latest')))
  }
})

test('a source latest change during upload blocks promotion', async () => {
  let copies = 0
  await assert.rejects(syncAliyun(defaults(['--promote-latest']), async args => {
    if (args[0] === 'copy') { copies++; return '' }
    return copies && args[1].startsWith('docker.') && args[1].endsWith('latest') ? other : hash
  }, silent), /changed/)
  assert.equal(copies, 4)
})
